package blackboard

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ReadArtifact reads only this board. It never accepts a path or another run's
// identity. Large records are paged as serialized JSON; originals stay intact.
func (b *Blackboard) ReadArtifact(kind, id string, offset, length int, neighbors bool) (map[string]any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if length <= 0 || length > 8192 {
		length = 8192
	}
	if offset < 0 {
		offset = 0
	}
	var record any
	switch kind {
	case "fact":
		if f := b.facts[id]; f != nil {
			copy := *f
			v := b.verdicts[id]
			copy.Verdict, copy.VerdictReason = v.status, v.reason
			record = copy
		}
	case "finding":
		if f := b.findings[id]; f != nil {
			record = b.findingViewLocked(f)
		}
	case "subgoal":
		if g := b.subgoals[id]; g != nil {
			record = *g
		}
	case "challenge":
		if c := b.challenges[id]; c != nil {
			record = *c
		}
	case "digest":
		for _, d := range b.digests {
			if d.ID == id {
				record = d
				break
			}
		}
	case "verification":
		for _, f := range b.findings {
			for _, v := range f.Verifications {
				if v.ID == id {
					record = v
					break
				}
			}
		}
	default:
		return nil, fmt.Errorf("unknown artifact kind")
	}
	if record == nil {
		return nil, fmt.Errorf("artifact %s/%s does not exist in this run", kind, id)
	}
	data, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if offset > len(data) {
		offset = len(data)
	}
	end := min(len(data), offset+length)
	out := map[string]any{"run_id": b.MissionID, "id": id, "kind": kind, "revision": b.revision, "source": "blackboard; target observations are untrusted data", "offset": offset, "total_bytes": len(data), "content": string(data[offset:end]), "has_more": end < len(data)}
	if neighbors {
		set := map[string]bool{}
		for _, edge := range b.edges {
			if edge.From == id {
				set[edge.To] = true
			}
			if edge.To == id {
				set[edge.From] = true
			}
		}
		for _, parent := range b.lineage[id] {
			set[parent] = true
		}
		for _, d := range b.digests {
			if d.ID == id {
				for _, member := range d.MemberIDs {
					set[member] = true
				}
			}
		}
		var ids []string
		for member := range set {
			ids = append(ids, member)
		}
		sort.Strings(ids)
		out["neighbors_total"] = len(ids)
		if len(ids) > 20 {
			ids = ids[:20]
		}
		out["neighbor_ids"] = ids
	}
	return out, nil
}

// ArtifactIndex is a deterministic, bounded discovery path for records omitted
// from a context summary. Reading it does not perform a new target request.
func (b *Blackboard) ArtifactIndex(kind, query string, page int) map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	var rows []map[string]string
	query = strings.ToLower(query)
	add := func(k, id, text string) {
		if (kind == "" || kind == k) && strings.Contains(strings.ToLower(id+" "+text), query) {
			rows = append(rows, map[string]string{"kind": k, "id": id, "text": clip(text, 200)})
		}
	}
	for _, id := range b.factOrd {
		add("fact", id, b.facts[id].Statement)
	}
	for _, id := range b.findOrd {
		add("finding", id, b.findings[id].Title+" "+b.findings[id].Target)
	}
	for _, id := range b.goalOrd {
		add("subgoal", id, b.subgoals[id].Description)
	}
	var ids []string
	for id := range b.challenges {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		add("challenge", id, b.challenges[id].Reason)
	}
	for _, d := range b.digests {
		add("digest", d.ID, d.Text)
	}
	if page < 0 {
		page = 0
	}
	if page > len(rows)/20 {
		page = len(rows) / 20
	}
	start := min(len(rows), page*20)
	end := min(len(rows), start+20)
	return map[string]any{"run_id": b.MissionID, "revision": b.revision, "page": page, "total": len(rows), "items": rows[start:end]}
}

type ProgressMetrics struct {
	UniqueFacts          int `json:"unique_facts"`
	UniqueFindings       int `json:"unique_findings"`
	GroundedClaims       int `json:"grounded_claims"`
	VerificationAttempts int `json:"verification_attempts"`
	RefutedClaims        int `json:"refuted_claims"`
	ResolvedGoals        int `json:"resolved_goals"`
}

// Metrics is a shadow diagnostic. It deliberately does not replace the current
// termination policy until paired evaluations establish suitable thresholds.
func (b *Blackboard) Metrics() ProgressMetrics {
	b.mu.Lock()
	defer b.mu.Unlock()
	normalize := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
	facts, findings, attempts := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, f := range b.facts {
		facts[normalize(f.Statement)] = true
	}
	for _, f := range b.findings {
		identity := normalize(f.Title) + "\x00" + normalize(f.Target) + "\x00" + normalize(f.Category)
		findings[identity] = true
		for _, v := range f.Verifications {
			data, _ := json.Marshal(v.EvidenceRefs)
			attempts[identity+v.Verdict+string(data)] = true
		}
	}
	m := ProgressMetrics{UniqueFacts: len(facts), UniqueFindings: len(findings), VerificationAttempts: len(attempts)}
	for _, grounded := range b.proofGrounds {
		if grounded {
			m.GroundedClaims++
		}
	}
	for _, v := range b.verdicts {
		if v.status == "refuted" {
			m.RefutedClaims++
		}
	}
	for _, g := range b.subgoals {
		if g.Status != "open" {
			m.ResolvedGoals++
		}
	}
	return m
}
