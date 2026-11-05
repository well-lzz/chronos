package blackboard

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestArtifactRecallIsBoundedAndRunLocal(t *testing.T) {
	b, _ := LoadOrNew("one", t.TempDir())
	id, err := b.AddFact("early omitted record", strings.Repeat("e", 1200), "worker")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddFact("early omitted record", "same observation", "worker"); err != nil {
		t.Fatal(err)
	}
	if m := b.Metrics(); m.UniqueFacts != 1 {
		t.Fatal(m)
	}
	index := b.ArtifactIndex("fact", "omitted", 0)
	if index["total"].(int) != 2 {
		t.Fatal(index)
	}
	page, err := b.ReadArtifact("fact", id, 0, 20, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(page["content"].(string)) != 20 || !page["has_more"].(bool) {
		t.Fatal(page)
	}
	full, err := b.ReadArtifact("fact", id, 0, 8192, false)
	if err != nil {
		t.Fatal(err)
	}
	var f Fact
	if err := json.Unmarshal([]byte(full["content"].(string)), &f); err != nil || f.Statement != "early omitted record" {
		t.Fatalf("record was not retrievable: %+v %v", f, err)
	}
	other, _ := LoadOrNew("two", t.TempDir())
	if _, err := other.ReadArtifact("fact", id, 0, 8192, false); err == nil {
		t.Fatal("cross-run record leaked")
	}
}
