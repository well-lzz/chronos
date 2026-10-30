package blackboard

import (
	"fmt"
	"strings"
	"testing"
)

func TestSummaryKeepsMandatoryAcrossAgeAndBudget(t *testing.T) {
	b := newBoard("m", "")
	oldVerified, _ := b.AddFinding(Finding{Title: "old verified", Severity: "low", EvidenceRefs: []EvidenceRef{{Ref: "t1", Role: "proof"}}})
	oldCritical, _ := b.AddFinding(Finding{Title: "old critical candidate", Severity: "critical"})
	oldHigh, _ := b.AddFinding(Finding{Title: "old high inconclusive", Severity: "high", Status: Inconclusive})
	for i := 0; i < 45; i++ {
		_, _ = b.AddFinding(Finding{Title: fmt.Sprintf("low candidate %d", i), Severity: "low"})
	}
	text, stats := b.SummaryWithBudget(500)
	for _, id := range []string{oldVerified, oldCritical, oldHigh} {
		if !strings.Contains(text, "["+id+"]") {
			t.Fatalf("mandatory %s missing: %s", id, text)
		}
	}
	if stats.MandatoryFindings != 3 || stats.QuotaFindings > lowFindingQuota || stats.OmittedFindings == 0 {
		t.Fatalf("unexpected retention stats: %+v", stats)
	}
	if !strings.Contains(text, "other findings") {
		t.Fatal("overflow count missing")
	}
}

func TestMandatoryOverflowIsVisibleAndNeverEvicted(t *testing.T) {
	b := newBoard("m", "")
	var ids []string
	for i := 0; i < 25; i++ {
		id, _ := b.AddFinding(Finding{Title: fmt.Sprintf("critical %d", i), Severity: "critical"})
		ids = append(ids, id)
	}
	text, stats := b.SummaryWithBudget(256)
	if !stats.BudgetOverflow || stats.MandatoryFindings != len(ids) {
		t.Fatalf("overflow not reported: %+v", stats)
	}
	for _, id := range ids {
		if !strings.Contains(text, "["+id+"]") {
			t.Fatalf("lost %s", id)
		}
	}
}

func TestSummaryRanksReferencedFactsAndDeltaSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	b, err := LoadOrNew("m", dir)
	if err != nil {
		t.Fatal(err)
	}
	important, _ := b.AddFact("important first fact", "", "x")
	for i := 0; i < 15; i++ {
		_, _ = b.AddFact(fmt.Sprintf("noise fact %d", i), "", "x")
	}
	_, _ = b.AddSubgoal("verify fact "+important, "x")
	text, _ := b.SummaryWithBudget(550)
	if !strings.Contains(text, "["+important+"]") {
		t.Fatalf("referenced fact evicted: %s", text)
	}
	prior := b.Revision()
	id, _ := b.AddFinding(Finding{Title: "new high", Severity: "high"})
	delta, stats := b.DeltaSince(prior, 800)
	if stats.Revision <= prior || !strings.Contains(delta, "["+id+"]") || strings.Contains(delta, "noise fact") {
		t.Fatalf("incorrect delta: %+v %s", stats, delta)
	}
	reloaded, err := LoadOrNew("m", dir)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Revision() != b.Revision() {
		t.Fatal("revision did not persist")
	}
	_, _ = reloaded.VerifyFinding(id, Verified, "reproduced", "proof", nil, "x")
	if reloaded.Revision() <= stats.Revision {
		t.Fatal("verification did not advance revision")
	}
}
