package blackboard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEvidenceAndImmutableVerification(t *testing.T) {
	dir := t.TempDir()
	b, err := LoadOrNew("m1", dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := b.AddFinding(Finding{
		Title: "Reflected XSS", Severity: "medium", Target: "h.test",
		Category: "web/injection", Evidence: "original note",
		EvidenceRefs: []EvidenceRef{{Ref: "t1", Role: "proof", Note: "reflected"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.findings[id].Status != Candidate || b.findings[id].Evidence != "original note" {
		t.Fatal("add_finding defaults wrong")
	}
	// inconclusive keeps original evidence, appends a record
	if _, err := b.VerifyFinding(id, Inconclusive, "WAF blocked", "tried A/B, blocked", nil, "deepseek"); err != nil {
		t.Fatal(err)
	}
	if b.findings[id].Status != Inconclusive || b.findings[id].Evidence != "original note" {
		t.Fatal("verification overwrote original evidence")
	}
	// reproduced -> verified with verification traffic
	if _, err := b.VerifyFinding(id, Verified, "fired headless", "confirmed",
		[]EvidenceRef{{Ref: "t1", Role: "verification"}}, "deepseek"); err != nil {
		t.Fatal(err)
	}
	f := b.findings[id]
	if f.Status != Verified || len(f.Verifications) != 2 {
		t.Fatalf("status=%s verifications=%d", f.Status, len(f.Verifications))
	}
	if len(f.EvidenceRefs) != 2 { // t1/proof + t1/verification
		t.Fatalf("evidence refs=%d want 2", len(f.EvidenceRefs))
	}
}

func TestPersistenceFailureIsReturned(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := newBoard("m", filepath.Join(parent, "board.json"))
	if _, err := b.AddFact("must persist", "", "test"); err == nil {
		t.Fatal("expected persistence error")
	}
}

func TestCorruptBlackboardIsRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blackboards", "m.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrNew("m", dir); err == nil {
		t.Fatal("expected corrupt state error")
	}
}

func TestProgressMarkerAndPersist(t *testing.T) {
	dir := t.TempDir()
	b, err := LoadOrNew("m2", dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddFact("port 80 open", "", "x"); err != nil {
		t.Fatal(err)
	}
	id, err := b.AddFinding(Finding{Title: "y", Severity: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if b.ProgressMarker() != 2 {
		t.Fatalf("marker=%d want 2", b.ProgressMarker())
	}
	if _, err := b.VerifyFinding(id, Verified, "", "", nil, "x"); err != nil {
		t.Fatal(err)
	}
	if b.ProgressMarker() != 3 {
		t.Fatalf("marker=%d want 3 after verify", b.ProgressMarker())
	}
	g, err := b.AddSubgoal("probe login", "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.ResolveSubgoal(g, "done"); err != nil {
		t.Fatal(err)
	}
	if b.ProgressMarker() != 3 {
		t.Fatal("subgoal churn should not move marker")
	}
	// reload round-trip
	b2, err := LoadOrNew("m2", dir)
	if err != nil {
		t.Fatal(err)
	}
	if b2.Counts()["findings_verified"] != 1 || b2.Counts()["facts"] != 1 {
		t.Fatalf("reload counts wrong: %v", b2.Counts())
	}
	if b2.seq != b.seq {
		t.Fatal("seq not preserved on reload")
	}
}
