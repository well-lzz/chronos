package blackboard

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const coldAfterRounds = 6

// Digest is a recoverable summary of low-value cold records. MemberIDs point
// to the unchanged full records in the durable blackboard.
type Digest struct {
	ID         string   `json:"id"`
	MemberIDs  []string `json:"member_ids"`
	Text       string   `json:"text"`
	Signature  string   `json:"signature"`
	Generation int      `json:"generation"`
	Revision   uint64   `json:"revision"`
}

type ColdMember struct {
	ID       string
	Text     string
	Revision uint64
}

type ColdGroup struct {
	Members   []ColdMember
	Signature string
}

// AdvanceRound tracks actual executor cycles, separately from blackboard
// revisions. It does not trigger a delta injection by itself.
func (b *Blackboard) AdvanceRound() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.round++
	return b.persist()
}

// Link records explicit fact/intent lineage without changing the original
// evidence. Unknown references are ignored; full records remain untouched.
func (b *Blackboard) Link(child string, parents []string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.existsLocked(child) {
		return fmt.Errorf("unknown blackboard id %q", child)
	}
	for _, parent := range parents {
		if parent == child || !b.existsLocked(parent) {
			return fmt.Errorf("invalid lineage source %q for %s", parent, child)
		}
	}
	seen := map[string]bool{}
	for _, parent := range b.lineage[child] {
		seen[parent] = true
	}
	changed := false
	for _, parent := range parents {
		if seen[parent] {
			continue
		}
		b.lineage[child] = append(b.lineage[child], parent)
		if b.claimExistsLocked(parent) && b.claimExistsLocked(child) {
			b.addEdgeLocked(parent, child, "derived_from", "", "", "")
		}
		seen[parent] = true
		changed = true
		b.markActiveLocked(parent)
	}
	if !changed {
		return nil
	}
	b.markActiveLocked(child)
	return b.persist()
}

var mentionedID = regexp.MustCompile(`\b[fgv][0-9]+\b`)

// TouchMentioned revives records explicitly referenced by a tool call, so a
// background digest can never hide work that the executor is using again.
func (b *Blackboard) TouchMentioned(text string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	seen := map[string]bool{}
	changed := false
	for _, id := range mentionedID.FindAllString(text, -1) {
		if seen[id] || !b.existsLocked(id) {
			continue
		}
		seen[id] = true
		if b.activity[id] != b.round || b.digestCoversLocked(id) {
			b.markActiveLocked(id)
			changed = true
		}
	}
	if changed {
		return b.persist()
	}
	return nil
}

func (b *Blackboard) existsLocked(id string) bool {
	return b.facts[id] != nil || b.findings[id] != nil || b.subgoals[id] != nil
}

func (b *Blackboard) markActiveLocked(id string) {
	b.activity[id] = b.round
	b.invalidateDigestsLocked(id)
	revision := b.nextRevision()
	if item := b.facts[id]; item != nil {
		item.Revision = revision
	}
	if item := b.findings[id]; item != nil {
		item.Revision = revision
	}
	if item := b.subgoals[id]; item != nil {
		item.Revision = revision
	}
}

func (b *Blackboard) digestCoversLocked(id string) bool {
	for _, digest := range b.digests {
		for _, member := range digest.MemberIDs {
			if member == id {
				return true
			}
		}
	}
	return false
}

func (b *Blackboard) invalidateDigestsLocked(id string) {
	kept := b.digests[:0]
	for _, digest := range b.digests {
		covered := false
		for _, member := range digest.MemberIDs {
			if member == id {
				covered = true
				break
			}
		}
		if !covered {
			kept = append(kept, digest)
		}
	}
	b.digests = kept
}

func (b *Blackboard) coldMemberLocked(id string) (ColdMember, bool) {
	if b.graphCriticalLocked(id) {
		return ColdMember{}, false
	}
	if b.round < coldAfterRounds || b.activity[id] > b.round || b.round-b.activity[id] < coldAfterRounds {
		return ColdMember{}, false
	}
	for _, goal := range b.subgoals {
		if goal.Status == "open" && (goal.FindingID == id || mentioned(id, goal.Description)) {
			return ColdMember{}, false
		}
	}
	for _, finding := range b.findings {
		if b.verdictLocked(finding.ID).status == "confirmed" || finding.Severity == "critical" || finding.Severity == "high" {
			if mentioned(id, finding.Evidence+" "+finding.Reproduction) {
				return ColdMember{}, false
			}
		}
	}
	if item := b.facts[id]; item != nil {
		return ColdMember{ID: id, Text: "fact: " + clip(item.Statement+"; evidence: "+item.Evidence, 1500), Revision: item.Revision}, true
	}
	if item := b.findings[id]; item != nil {
		if b.verdictLocked(id).status == "confirmed" || item.Severity == "high" || item.Severity == "critical" {
			return ColdMember{}, false
		}
		return ColdMember{ID: id, Text: "finding (" + item.Status + "/" + item.Severity + "): " + clip(item.Title+"; "+item.Evidence, 1500), Revision: item.Revision}, true
	}
	return ColdMember{}, false
}

func signature(members []ColdMember) string {
	var b strings.Builder
	for _, member := range members {
		fmt.Fprintf(&b, "%s:%d;", member.ID, member.Revision)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// PlanCold groups cold nodes by lineage or a common parent. Minor passes touch
// only uncovered nodes; major passes revisit every cold group and reuse
// unchanged signatures without another LLM call.
func (b *Blackboard) PlanCold(major bool) []ColdGroup {
	b.mu.Lock()
	defer b.mu.Unlock()
	nodes := map[string]ColdMember{}
	for _, id := range append(append([]string(nil), b.factOrd...), b.findOrd...) {
		if !major && b.digestCoversLocked(id) {
			continue
		}
		if node, ok := b.coldMemberLocked(id); ok {
			nodes[id] = node
		}
	}
	parent := map[string]string{}
	for id := range nodes {
		parent[id] = id
	}
	var root func(string) string
	root = func(id string) string {
		if parent[id] != id {
			parent[id] = root(parent[id])
		}
		return parent[id]
	}
	union := func(a, c string) {
		ra, rc := root(a), root(c)
		if ra != rc {
			parent[rc] = ra
		}
	}
	byParent := map[string]string{}
	for id := range nodes {
		for _, source := range b.lineage[id] {
			if _, ok := nodes[source]; ok {
				union(id, source)
			}
			if sibling, ok := byParent[source]; ok {
				union(id, sibling)
			} else {
				byParent[source] = id
			}
		}
	}
	grouped := map[string][]ColdMember{}
	for id, node := range nodes {
		grouped[root(id)] = append(grouped[root(id)], node)
	}
	var groups []ColdGroup
	for _, members := range grouped {
		if len(members) < 2 {
			continue
		}
		sort.Slice(members, func(i, j int) bool { return members[i].ID < members[j].ID })
		sig := signature(members)
		if major {
			unchanged := false
			for _, digest := range b.digests {
				if digest.Signature == sig {
					unchanged = true
					break
				}
			}
			if unchanged {
				continue
			}
		}
		groups = append(groups, ColdGroup{Members: members, Signature: sig})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Members[0].ID < groups[j].Members[0].ID })
	if len(groups) > 2 {
		groups = groups[:2]
	}
	return groups
}

// CommitCold performs a liveness recheck after the model returns. If even one
// member changed or became hot, the whole generated digest is discarded.
func (b *Blackboard) CommitCold(group ColdGroup, text string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(group.Members) < 2 || strings.TrimSpace(text) == "" {
		return false, nil
	}
	current := make([]ColdMember, 0, len(group.Members))
	for _, member := range group.Members {
		node, ok := b.coldMemberLocked(member.ID)
		if !ok || node.Revision != member.Revision {
			return false, nil
		}
		current = append(current, node)
	}
	if signature(current) != group.Signature {
		return false, nil
	}
	ids := make([]string, 0, len(group.Members))
	for _, member := range group.Members {
		ids = append(ids, member.ID)
	}
	generation := 1
	for _, digest := range b.digests {
		for _, id := range digest.MemberIDs {
			for _, member := range ids {
				if id == member && digest.Generation >= generation {
					generation = digest.Generation + 1
				}
			}
		}
	}
	for _, id := range ids {
		b.invalidateDigestsLocked(id)
	}
	digest := Digest{ID: fmt.Sprintf("d%d", b.nextIDNumber()), MemberIDs: ids, Text: clip(strings.TrimSpace(text), 2000),
		Signature: group.Signature, Generation: generation, Revision: b.nextRevision()}
	b.digests = append(b.digests, digest)
	return true, b.persist()
}

func (b *Blackboard) nextIDNumber() int { b.seq++; return b.seq }

func (b *Blackboard) Digests() []Digest {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Digest, len(b.digests))
	for i, digest := range b.digests {
		out[i] = digest
		out[i].MemberIDs = append([]string(nil), digest.MemberIDs...)
	}
	return out
}
