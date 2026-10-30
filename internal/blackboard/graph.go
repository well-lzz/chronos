package blackboard

import (
	"fmt"
	"sort"
	"strings"
)

// GraphEdge is an append-only argument, not a stored verdict. Traffic IDs are
// evidence nodes; f/v/c IDs are claim nodes; z IDs are verification attempts.
type GraphEdge struct {
	From     string  `json:"from"`
	To       string  `json:"to"`
	Kind     string  `json:"kind"` // grounds | supports | refutes | verifies | derived_from
	Role     string  `json:"role,omitempty"`
	Note     string  `json:"note,omitempty"`
	By       string  `json:"by,omitempty"`
	TS       float64 `json:"ts"`
	Revision uint64  `json:"revision,omitempty"`
}

// Challenge is a falsifiable counterclaim. A proof-grounded challenge can
// defeat its target; an ungrounded one leaves a visible unresolved conflict.
type Challenge struct {
	ID       string  `json:"id"`
	Reason   string  `json:"reason"`
	By       string  `json:"by,omitempty"`
	TS       float64 `json:"ts"`
	Revision uint64  `json:"revision,omitempty"`
}

type ClaimView struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Text    string `json:"text"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

type EvidenceNode struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // traffic | oracle
}

type VerificationNode struct {
	ID        string `json:"id"`
	FindingID string `json:"finding_id"`
	Outcome   string `json:"outcome"`
	Summary   string `json:"summary,omitempty"`
	By        string `json:"by,omitempty"`
}

type GraphSnapshot struct {
	Claims        []ClaimView        `json:"claims"`
	Evidence      []EvidenceNode     `json:"evidence"`
	Verifications []VerificationNode `json:"verifications"`
	Edges         []GraphEdge        `json:"edges"`
}

type claimVerdict struct{ status, reason string }

func (b *Blackboard) claimExistsLocked(id string) bool {
	return b.facts[id] != nil || b.findings[id] != nil || b.challenges[id] != nil
}

func (b *Blackboard) addEdgeLocked(from, to, kind, role, note, by string) bool {
	edge := GraphEdge{From: from, To: to, Kind: kind, Role: role, Note: clip(note, 500), By: by, TS: now()}
	key := edgeKey(edge)
	if b.edgeKeys[key] {
		return false
	}
	edge.Revision = b.nextRevision()
	b.edges = append(b.edges, edge)
	b.indexEdgeLocked(edge)
	return true
}

func edgeKey(e GraphEdge) string { return e.Kind + "\x00" + e.From + "\x00" + e.To + "\x00" + e.Role }

func (b *Blackboard) indexEdgeLocked(edge GraphEdge) {
	b.edgeKeys[edgeKey(edge)] = true
	switch edge.Kind {
	case "grounds":
		if edge.Role == "proof" || edge.Role == "verification" {
			b.proofGrounds[edge.To] = true
		}
	case "refutes":
		b.neighbors[edge.From] = append(b.neighbors[edge.From], edge.To)
		b.neighbors[edge.To] = append(b.neighbors[edge.To], edge.From)
		b.attackers[edge.To] = append(b.attackers[edge.To], edge.From)
	}
}

func (b *Blackboard) rebuildGraphIndexesLocked() {
	b.attackers, b.neighbors = map[string][]string{}, map[string][]string{}
	b.proofGrounds, b.edgeKeys = map[string]bool{}, map[string]bool{}
	for _, edge := range b.edges {
		b.indexEdgeLocked(edge)
	}
}

func (b *Blackboard) groundLocked(id string, refs []EvidenceRef, by string) bool {
	changed := false
	for _, ref := range refs {
		if ref.Ref != "" && b.addEdgeLocked(ref.Ref, id, "grounds", ref.Role, ref.Note, by) {
			changed = true
		}
	}
	return changed
}

// recomputeLocked evaluates only the attack-connected component touched by a
// changed ground or refutation. The synchronous fixed point leaves cycles
// undecided, including two mutually grounded refuters.
func (b *Blackboard) recomputeLocked(seeds ...string) {
	if b.verdicts == nil {
		b.verdicts = map[string]claimVerdict{}
	}
	neighbors, attackers, proof := b.neighbors, b.attackers, b.proofGrounds
	component := map[string]bool{}
	queue := append([]string(nil), seeds...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if component[id] || !b.claimExistsLocked(id) {
			continue
		}
		component[id] = true
		queue = append(queue, neighbors[id]...)
	}
	if len(component) == 0 {
		return
	}
	states := map[string]string{}
	for id := range component {
		states[id] = "suspected"
	}
	for step := 0; step < len(component); step++ {
		changed := false
		next := map[string]string{}
		for id := range component {
			if states[id] != "suspected" {
				next[id] = states[id]
				continue
			}
			anyWinner, allDefeated := false, true
			for _, attacker := range attackers[id] {
				if states[attacker] == "confirmed" && proof[attacker] {
					anyWinner = true
				}
				if states[attacker] != "refuted" {
					allDefeated = false
				}
			}
			switch {
			case anyWinner:
				next[id] = "refuted"
			case proof[id] && allDefeated:
				next[id] = "confirmed"
			default:
				next[id] = "suspected"
			}
			if next[id] != states[id] {
				changed = true
			}
		}
		states = next
		if !changed {
			break
		}
	}
	var stateIDs []string
	for id := range states {
		stateIDs = append(stateIDs, id)
	}
	sort.Strings(stateIDs)
	for _, id := range stateIDs {
		status := states[id]
		var live []string
		for _, attacker := range attackers[id] {
			if states[attacker] != "refuted" {
				live = append(live, attacker)
			}
		}
		sort.Strings(live)
		reason := "awaiting proof traffic"
		switch {
		case status == "refuted":
			reason = "defeated by proof-grounded refuter " + strings.Join(live, ", ")
		case len(live) > 0:
			reason = "unresolved challenge from " + strings.Join(live, ", ")
		case status == "confirmed" && b.findings[id] != nil && b.findings[id].Oracle:
			reason = "confirmed by platform oracle"
		case status == "confirmed":
			reason = "proof or verification traffic grounds this claim"
		case proof[id]:
			reason = "proof exists, but the argument is unresolved"
		}
		previous, existed := b.verdicts[id]
		b.verdicts[id] = claimVerdict{status, reason}
		if f := b.findings[id]; f != nil {
			if status == "confirmed" {
				b.promoted[id] = true
			}
			f.Status = legacyStatus(b.verdicts[id], f)
			f.VerdictReason = reason
			if existed && (previous.status != status || previous.reason != reason) {
				f.Revision = b.nextRevision()
				b.activity[id] = b.round
			}
		}
		if f := b.facts[id]; f != nil {
			f.Verdict, f.VerdictReason = status, reason
			if existed && (previous.status != status || previous.reason != reason) {
				f.Revision = b.nextRevision()
				b.activity[id] = b.round
			}
		}
	}
	b.syncConflictGoalsLocked(component, states, proof)
}

func (b *Blackboard) syncConflictGoalsLocked(component map[string]bool, states map[string]string, proof map[string]bool) {
	pairs := map[string][]string{}
	for target := range component {
		for _, attacker := range b.attackers[target] {
			if !component[attacker] || !b.edgeKeys[edgeKey(GraphEdge{From: target, To: attacker, Kind: "refutes"})] {
				continue
			}
			ids := []string{attacker, target}
			sort.Strings(ids)
			pairs[strings.Join(ids, "|")] = ids
		}
	}
	var keys []string
	for key := range pairs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		ids := pairs[key]
		unresolved := proof[ids[0]] && proof[ids[1]] && states[ids[0]] == "suspected" && states[ids[1]] == "suspected"
		var goal *SubGoal
		for _, id := range b.goalOrd {
			candidate := b.subgoals[id]
			if candidate.Kind == "adjudication" && strings.Join(candidate.ConflictIDs, "|") == key {
				goal = candidate
				break
			}
		}
		if goal == nil && unresolved {
			id := b.nextID("g")
			b.subgoals[id] = &SubGoal{ID: id, Description: fmt.Sprintf("Resolve contradictory proof-grounded claims %s and %s with decisive evidence", ids[0], ids[1]), Status: "open", Kind: "adjudication", ConflictIDs: ids, TS: now(), Revision: b.nextRevision()}
			b.goalOrd = append(b.goalOrd, id)
		} else if goal != nil && unresolved && goal.Status != "open" {
			goal.Status, goal.Outcome, goal.Revision = "open", "", b.nextRevision()
		} else if goal != nil && !unresolved && goal.Status == "open" {
			goal.Status, goal.Outcome, goal.Revision = "resolved", "graph conflict resolved", b.nextRevision()
		}
	}
}

func (b *Blackboard) verdictLocked(id string) claimVerdict {
	if v, ok := b.verdicts[id]; ok {
		return v
	}
	b.recomputeLocked(id)
	return b.verdicts[id]
}

func legacyStatus(v claimVerdict, finding *Finding) string {
	switch v.status {
	case "confirmed":
		return Verified
	case "refuted":
		return Refuted
	default:
		if len(finding.Verifications) > 0 && finding.Verifications[len(finding.Verifications)-1].Verdict == Inconclusive {
			return Inconclusive
		}
		return Candidate
	}
}

func (b *Blackboard) findingViewLocked(f *Finding) Finding {
	out := *f
	v := b.verdictLocked(f.ID)
	out.Status, out.VerdictReason = legacyStatus(v, f), v.reason
	if v.status == "confirmed" {
		ts := f.TS
		if len(f.Verifications) > 0 {
			ts = f.Verifications[len(f.Verifications)-1].TS
		}
		out.VerifiedTS = &ts
	} else {
		out.VerifiedTS = nil
	}
	return out
}

func (b *Blackboard) ClaimVerdict(id string) (string, string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.claimExistsLocked(id) {
		return "", "", false
	}
	v := b.verdictLocked(id)
	return v.status, v.reason, true
}

func (b *Blackboard) ChallengeFinding(targetID, reason string, refs []EvidenceRef, by string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.claimExistsLocked(targetID) {
		return "", fmt.Errorf("unknown claim %s", targetID)
	}
	if strings.TrimSpace(reason) == "" {
		return "", fmt.Errorf("challenge reason is required")
	}
	id := b.nextID("c")
	b.challenges[id] = &Challenge{ID: id, Reason: clip(reason, 2000), By: by, TS: now(), Revision: b.nextRevision()}
	b.groundLocked(id, refs, by)
	b.addEdgeLocked(id, targetID, "refutes", "", "", by)
	b.recomputeLocked(id)
	b.invalidateDigestsLocked(targetID)
	return id, b.persist()
}

func (b *Blackboard) RelateClaims(from, to, kind, by string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.claimExistsLocked(from) || !b.claimExistsLocked(to) || from == to {
		return fmt.Errorf("two distinct existing claims are required")
	}
	if kind != "supports" && kind != "refutes" {
		return fmt.Errorf("kind must be supports or refutes")
	}
	if !b.addEdgeLocked(from, to, kind, "", "", by) {
		return nil
	}
	b.markActiveLocked(to)
	b.markActiveLocked(from)
	if kind == "refutes" {
		b.recomputeLocked(from)
		b.invalidateDigestsLocked(to)
	}
	return b.persist()
}

func (b *Blackboard) Graph() GraphSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out GraphSnapshot
	for _, id := range b.factOrd {
		f := b.facts[id]
		v := b.verdictLocked(id)
		out.Claims = append(out.Claims, ClaimView{id, "fact", f.Statement, v.status, v.reason})
	}
	for _, id := range b.findOrd {
		f := b.findings[id]
		v := b.verdictLocked(id)
		out.Claims = append(out.Claims, ClaimView{id, "finding", f.Title, v.status, v.reason})
		for _, attempt := range f.Verifications {
			out.Verifications = append(out.Verifications, VerificationNode{attempt.ID, id, attempt.Verdict, attempt.Summary, attempt.By})
		}
	}
	var challengeIDs []string
	for id := range b.challenges {
		challengeIDs = append(challengeIDs, id)
	}
	sort.Strings(challengeIDs)
	for _, id := range challengeIDs {
		c := b.challenges[id]
		v := b.verdictLocked(id)
		out.Claims = append(out.Claims, ClaimView{id, "challenge", c.Reason, v.status, v.reason})
	}
	evidence := map[string]string{}
	for _, edge := range b.edges {
		if edge.Kind == "grounds" {
			kind := "traffic"
			if strings.HasPrefix(edge.From, "oracle:") {
				kind = "oracle"
			}
			evidence[edge.From] = kind
		}
	}
	var evidenceIDs []string
	for id := range evidence {
		evidenceIDs = append(evidenceIDs, id)
	}
	sort.Strings(evidenceIDs)
	for _, id := range evidenceIDs {
		out.Evidence = append(out.Evidence, EvidenceNode{id, evidence[id]})
	}
	out.Edges = append(out.Edges, b.edges...)
	return out
}

func (b *Blackboard) graphCriticalLocked(id string) bool {
	return b.proofGrounds[id] || len(b.neighbors[id]) > 0
}

// migrateLegacyGraphLocked reuses historical traffic references and replay
// records. A legacy status without supporting evidence does not become proof.
func (b *Blackboard) migrateLegacyGraphLocked() {
	for _, id := range b.factOrd {
		fact := b.facts[id]
		b.groundLocked(fact.ID, fact.EvidenceRefs, fact.FoundBy)
	}
	for _, id := range b.findOrd {
		finding := b.findings[id]
		unresolvedRefs := map[string]bool{}
		for _, verification := range finding.Verifications {
			if verification.Verdict == Refuted || verification.Verdict == Inconclusive {
				for _, ref := range verification.EvidenceRefs {
					unresolvedRefs[ref.Ref+"|"+ref.Role] = true
				}
			}
		}
		var original []EvidenceRef
		for _, ref := range finding.EvidenceRefs {
			if !unresolvedRefs[ref.Ref+"|"+ref.Role] {
				original = append(original, ref)
			}
		}
		b.groundLocked(finding.ID, original, finding.FoundBy)
		for i := range finding.Verifications {
			v := &finding.Verifications[i]
			if v.ID == "" {
				v.ID = b.nextID("z")
			}
			b.addEdgeLocked(v.ID, finding.ID, "verifies", v.Verdict, v.Summary, v.By)
			if v.Verdict == Refuted {
				id := b.nextID("c")
				b.challenges[id] = &Challenge{ID: id, Reason: clip(v.Summary+": "+v.Evidence, 2000), By: v.By, TS: v.TS, Revision: b.nextRevision()}
				b.groundLocked(id, v.EvidenceRefs, v.By)
				b.addEdgeLocked(id, finding.ID, "refutes", "", "", v.By)
			} else if v.Verdict == Inconclusive {
				for _, ref := range v.EvidenceRefs {
					b.addEdgeLocked(ref.Ref, finding.ID, "grounds", "supporting", ref.Note, v.By)
				}
			}
		}
		if finding.Category == "flag" && finding.Status == Verified {
			finding.Oracle = true
			b.addEdgeLocked("oracle:"+finding.ID, finding.ID, "grounds", "proof", "legacy platform-confirmed flag", finding.FoundBy)
		}
	}
	var children []string
	for child := range b.lineage {
		children = append(children, child)
	}
	sort.Strings(children)
	for _, child := range children {
		parents := b.lineage[child]
		if !b.claimExistsLocked(child) {
			continue
		}
		for _, parent := range parents {
			if b.claimExistsLocked(parent) {
				b.addEdgeLocked(parent, child, "derived_from", "", "", "")
			}
		}
	}
}
