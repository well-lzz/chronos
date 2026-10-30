package blackboard

import (
	"strings"
	"testing"
)

func TestColdDigestPreservesMandatoryFindingsAndRechecksLiveness(t *testing.T) {
	dir := t.TempDir()
	b, err := LoadOrNew("cold-group", dir)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := b.AddFact("root observation", "capture A", "agent")
	childA, _ := b.AddFact("first derived detail", "capture B", "agent")
	childB, _ := b.AddFact("second derived detail", "capture C", "agent")
	if err := b.Link(childA, []string{root}); err != nil {
		t.Fatal(err)
	}
	if err := b.Link(childB, []string{root}); err != nil {
		t.Fatal(err)
	}
	_, _ = b.AddFinding(Finding{Title: "critical issue remains visible", Severity: "critical", Status: Candidate})
	for range 6 {
		if err := b.AdvanceRound(); err != nil {
			t.Fatal(err)
		}
	}
	groups := b.PlanCold(false)
	if len(groups) != 1 || len(groups[0].Members) != 3 {
		t.Fatalf("cold groups: %+v", groups)
	}
	if err := b.TouchMentioned(`{"source":"` + childA + `"}`); err != nil {
		t.Fatal(err)
	}
	if applied, err := b.CommitCold(groups[0], "the branch was examined"); err != nil || applied {
		t.Fatalf("stale digest covered revived fact: applied=%v err=%v", applied, err)
	}
	for range 6 {
		if err := b.AdvanceRound(); err != nil {
			t.Fatal(err)
		}
	}
	groups = b.PlanCold(false)
	if len(groups) != 1 {
		t.Fatalf("replanned groups: %+v", groups)
	}
	if applied, err := b.CommitCold(groups[0], "both derived details were examined; no conclusion"); err != nil || !applied {
		t.Fatalf("digest not committed: applied=%v err=%v", applied, err)
	}
	summary, _ := b.SummaryWithBudget(4000)
	if !strings.Contains(summary, "cold digests") || !strings.Contains(summary, "critical issue remains visible") || strings.Contains(summary, "first derived detail") {
		t.Fatalf("summary did not retain mandatory and replace cold facts:\n%s", summary)
	}
	reloaded, err := LoadOrNew("cold-group", dir)
	if err != nil || len(reloaded.Digests()) != 1 {
		t.Fatalf("digest did not persist: %v", err)
	}
	if err := b.TouchMentioned(`{"fact_id":"` + childA + `"}`); err != nil {
		t.Fatal(err)
	}
	if len(b.Digests()) != 0 {
		t.Fatal("revived member left a stale digest active")
	}
	summary, _ = b.SummaryWithBudget(4000)
	if !strings.Contains(summary, "first derived detail") {
		t.Fatal("revived fact was not reinjected")
	}
}
