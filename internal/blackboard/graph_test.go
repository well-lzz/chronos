package blackboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func expectVerdict(t *testing.T, b *Blackboard, id, want string) {
	t.Helper()
	got, reason, ok := b.ClaimVerdict(id)
	if !ok || got != want || reason == "" {
		t.Fatalf("%s: verdict=%s reason=%q ok=%v, want %s", id, got, reason, ok, want)
	}
}

func TestEvidenceGraphDefeatAndReinstatement(t *testing.T) {
	b, err := LoadOrNew("argument", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := b.AddFinding(Finding{Title: "access check bypass", Severity: "high", EvidenceRefs: []EvidenceRef{{Ref: "t1", Role: "supporting"}}})
	if err != nil {
		t.Fatal(err)
	}
	expectVerdict(t, b, id, "suspected")
	if changed, err := b.AddEvidenceRefs(id, []EvidenceRef{{Ref: "t2", Role: "proof"}}); err != nil || !changed {
		t.Fatalf("proof: %v %v", changed, err)
	}
	expectVerdict(t, b, id, "confirmed")
	unrelated, _ := b.AddFinding(Finding{Title: "unrelated"})
	unrelatedRevision := b.findings[unrelated].Revision
	challenge, err := b.ChallengeFinding(id, "the response was a cache artifact", []EvidenceRef{{Ref: "t3", Role: "verification"}}, "peer")
	if err != nil {
		t.Fatal(err)
	}
	expectVerdict(t, b, id, "refuted")
	if b.findings[unrelated].Revision != unrelatedRevision {
		t.Fatal("unrelated component was revised")
	}
	counter, err := b.ChallengeFinding(challenge, "fresh request reproduces the bypass", []EvidenceRef{{Ref: "t4", Role: "proof"}}, "third")
	if err != nil {
		t.Fatal(err)
	}
	expectVerdict(t, b, challenge, "refuted")
	expectVerdict(t, b, counter, "confirmed")
	expectVerdict(t, b, id, "confirmed")
	loaded, err := LoadOrNew("argument", filepath.Dir(filepath.Dir(b.Path)))
	if err != nil {
		t.Fatal(err)
	}
	expectVerdict(t, loaded, id, "confirmed")
	if len(loaded.Graph().Edges) < 4 {
		t.Fatal("argument edges did not persist")
	}
	data, err := os.ReadFile(b.Path)
	if err != nil {
		t.Fatal(err)
	}
	var disk struct {
		Findings []map[string]any `json:"findings"`
	}
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	if _, exists := disk.Findings[0]["status"]; exists {
		t.Fatal("computed verdict was persisted as a finding status")
	}
}

func TestEvidenceGraphConflictCreatesAdjudicationGoal(t *testing.T) {
	dir := t.TempDir()
	b, err := LoadOrNew("conflict", dir)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := b.AddFinding(Finding{Title: "claim", EvidenceRefs: []EvidenceRef{{Ref: "t1", Role: "proof"}}})
	challenge, _ := b.ChallengeFinding(id, "contradictory replay", []EvidenceRef{{Ref: "t2", Role: "proof"}}, "peer")
	if err := b.RelateClaims(id, challenge, "refutes", "peer"); err != nil {
		t.Fatal(err)
	}
	expectVerdict(t, b, id, "suspected")
	expectVerdict(t, b, challenge, "suspected")
	var adjudicationID string
	for _, goal := range b.OpenSubgoals() {
		if goal.Kind == "adjudication" {
			adjudicationID = goal.ID
		}
	}
	if adjudicationID == "" {
		t.Fatal("conflict did not create an adjudication sub-goal")
	}
	reloaded, err := LoadOrNew("conflict", dir)
	if err != nil {
		t.Fatal(err)
	}
	expectVerdict(t, reloaded, id, "suspected")
	expectVerdict(t, reloaded, challenge, "suspected")
	adjudications := 0
	for _, goal := range reloaded.OpenSubgoals() {
		if goal.Kind == "adjudication" {
			adjudications++
		}
	}
	if adjudications != 1 {
		t.Fatalf("reload produced %d adjudication goals, want one", adjudications)
	}
	if _, err := b.ChallengeFinding(challenge, "decisive third replay", []EvidenceRef{{Ref: "t3", Role: "proof"}}, "third"); err != nil {
		t.Fatal(err)
	}
	expectVerdict(t, b, id, "confirmed")
	for _, goal := range b.OpenSubgoals() {
		if goal.ID == adjudicationID {
			t.Fatal("resolved conflict remained open")
		}
	}
}

func TestEvidenceGraphSupportDoesNotBorrowProof(t *testing.T) {
	b := newBoard("support", "")
	grounded, _ := b.AddFinding(Finding{Title: "grounded", EvidenceRefs: []EvidenceRef{{Ref: "t1", Role: "proof"}}})
	bare, _ := b.AddFinding(Finding{Title: "bare"})
	if err := b.RelateClaims(grounded, bare, "supports", "agent"); err != nil {
		t.Fatal(err)
	}
	expectVerdict(t, b, bare, "suspected")
	challenge, _ := b.ChallengeFinding(grounded, "unproved objection", nil, "peer")
	expectVerdict(t, b, challenge, "suspected")
	expectVerdict(t, b, grounded, "suspected")
}

func TestInconclusiveVerificationCannotGroundConfirmation(t *testing.T) {
	b := newBoard("inconclusive", "")
	id, _ := b.AddFinding(Finding{Title: "uncertain", EvidenceRefs: []EvidenceRef{{Ref: "t1", Role: "baseline"}}})
	if _, err := b.VerifyFinding(id, Inconclusive, "blocked", "no result", []EvidenceRef{{Ref: "t2", Role: "verification"}}, "peer"); err != nil {
		t.Fatal(err)
	}
	expectVerdict(t, b, id, "suspected")
	for _, edge := range b.Graph().Edges {
		if edge.From == "t2" && edge.Kind == "grounds" && edge.Role != "supporting" {
			t.Fatalf("inconclusive traffic became proof: %+v", edge)
		}
	}
}

func TestEvidenceGraphRejectsUnprovedConfirmationAndMigrates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blackboards", "old.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `{"mission_id":"old","seq":1,"findings":[{"id":"v1","title":"old finding","status":"verified","evidence_refs":[{"ref":"t1","role":"proof"}],"verifications":[{"verdict":"refuted","summary":"replay failed","evidence_refs":[{"ref":"t2","role":"verification"}]}]}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrNew("old", dir)
	if err != nil {
		t.Fatal(err)
	}
	expectVerdict(t, b, "v1", "refuted")
	if !strings.Contains(b.FindingsSorted()[0].VerdictReason, "refuter") {
		t.Fatal("missing verdict rationale")
	}
	if len(b.Graph().Verifications) != 1 {
		t.Fatal("legacy verification was not graphized")
	}
	fact, _ := b.AddFact("reported without traffic", "prose only", "agent")
	expectVerdict(t, b, fact, "suspected")
}

func TestColdDigestRetainsArgumentNodes(t *testing.T) {
	b := newBoard("digest-graph", "")
	proofFact, _ := b.AddFactWithRefs("service exposes debug endpoint", "observed", []EvidenceRef{{Ref: "t1", Role: "proof"}}, "agent")
	finding, _ := b.AddFinding(Finding{Title: "challenged finding", EvidenceRefs: []EvidenceRef{{Ref: "t2", Role: "supporting"}}})
	challenge, _ := b.ChallengeFinding(finding, "counter observation", nil, "peer")
	_ = challenge
	contestedFact, _ := b.AddFact("contested fact", "text only", "agent")
	_, _ = b.ChallengeFinding(contestedFact, "counterexample", nil, "peer")
	plain, _ := b.AddFact("unrelated note", "text only", "agent")
	for range 6 {
		if err := b.AdvanceRound(); err != nil {
			t.Fatal(err)
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.coldMemberLocked(plain); !ok {
		t.Fatal("plain record was not eligible")
	}
	for _, protected := range []string{proofFact, finding, contestedFact} {
		if _, ok := b.coldMemberLocked(protected); ok {
			t.Fatalf("argument node %s was eligible for digest", protected)
		}
	}
}
