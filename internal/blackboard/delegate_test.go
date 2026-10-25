package blackboard

import "testing"

func TestArtifactsSinceAttributesOnlyChildTree(t *testing.T) {
	bb, err := LoadOrNew("delegate-attribution", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := bb.Revision()
	childFact, err := bb.AddFact("child observation", "test evidence", "root/sub:abc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bb.AddFact("peer observation", "test evidence", "peer"); err != nil {
		t.Fatal(err)
	}
	grandchildFact, err := bb.AddFact("grandchild observation", "test evidence", "root/sub:abc/sub:def")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bb.AddFact("similar prefix observation", "test evidence", "root/sub:abcd"); err != nil {
		t.Fatal(err)
	}
	childFinding, err := bb.AddFinding(Finding{Title: "child finding", FoundBy: "root/sub:abc"})
	if err != nil {
		t.Fatal(err)
	}
	facts, findings := bb.ArtifactsSince(before, "root/sub:abc")
	if len(facts) != 2 || facts[0] != childFact || facts[1] != grandchildFact || len(findings) != 1 || findings[0] != childFinding {
		t.Fatalf("unexpected attribution facts=%v findings=%v", facts, findings)
	}
	if facts, findings := bb.ArtifactsSince(bb.Revision(), "root/sub:abc"); len(facts)+len(findings) != 0 {
		t.Fatal("revision cursor returned old artifacts")
	}
}
