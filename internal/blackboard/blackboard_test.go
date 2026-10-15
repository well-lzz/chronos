package blackboard

import (
	"testing"
)

func TestEvidenceAndImmutableVerification(t *testing.T) {
	dir := t.TempDir()
	b := LoadOrNew("m1", dir)
	id := b.AddFinding(Finding{
		Title: "Reflected XSS", Severity: "medium", Target: "h.test",
		Category: "web/injection", Evidence: "original note",
		EvidenceRefs: []EvidenceRef{{Ref: "t1", Role: "proof", Note: "reflected"}},
	})
	if b.findings[id].Status != Candidate || b.findings[id].Evidence != "original note" {
		t.Fatal("add_finding defaults wrong")
	}
	// inconclusive keeps original evidence, appends a record
	b.VerifyFinding(id, Inconclusive, "WAF blocked", "tried A/B, blocked", nil, "deepseek")
	if b.findings[id].Status != Inconclusive || b.findings[id].Evidence != "original note" {
		t.Fatal("verification overwrote original evidence")
	}
	// reproduced -> verified with verification traffic
	b.VerifyFinding(id, Verified, "fired headless", "confirmed",
		[]EvidenceRef{{Ref: "t1", Role: "verification"}}, "deepseek")
	f := b.findings[id]
	if f.Status != Verified || len(f.Verifications) != 2 {
		t.Fatalf("status=%s verifications=%d", f.Status, len(f.Verifications))
	}
	if len(f.EvidenceRefs) != 2 { // t1/proof + t1/verification
		t.Fatalf("evidence refs=%d want 2", len(f.EvidenceRefs))
	}
}

func TestProgressMarkerAndPersist(t *testing.T) {
	dir := t.TempDir()
	b := LoadOrNew("m2", dir)
	b.AddFact("port 80 open", "", "x")
	id := b.AddFinding(Finding{Title: "y", Severity: "low"})
	if b.ProgressMarker() != 2 {
		t.Fatalf("marker=%d want 2", b.ProgressMarker())
	}
	b.VerifyFinding(id, Verified, "", "", nil, "x")
	if b.ProgressMarker() != 3 {
		t.Fatalf("marker=%d want 3 after verify", b.ProgressMarker())
	}
	g := b.AddSubgoal("probe login", "x")
	b.ResolveSubgoal(g, "done")
	if b.ProgressMarker() != 3 {
		t.Fatal("subgoal churn should not move marker")
	}
	// reload round-trip
	b2 := LoadOrNew("m2", dir)
	if b2.Counts()["findings_verified"] != 1 || b2.Counts()["facts"] != 1 {
		t.Fatalf("reload counts wrong: %v", b2.Counts())
	}
	if b2.seq != b.seq {
		t.Fatal("seq not preserved on reload")
	}
}
