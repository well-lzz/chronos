package termination

import (
	"testing"

	"github.com/well-lzz/chronos/internal/blackboard"
)

func TestEngagementStopsOnlyAfterCoverageAndQuietCycles(t *testing.T) {
	bb, err := blackboard.LoadOrNew("termination", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := CycleState{IsEngagement: true, QuietCycles: 3, RemainingSeconds: 60}
	if ShouldTerminate(bb, state) {
		t.Fatal("unknown coverage must not be treated as complete")
	}
	goal, _ := bb.AddSubgoal("inspect target", "agent")
	if ShouldTerminate(bb, state) {
		t.Fatal("open goal was ignored")
	}
	_, _ = bb.ResolveSubgoal(goal, "complete")
	state.QuietCycles = 2
	if ShouldTerminate(bb, state) {
		t.Fatal("stopped before quiet threshold")
	}
	state.QuietCycles = 3
	if !ShouldTerminate(bb, state) {
		t.Fatal("resolved coverage and quiet cycles did not stop")
	}
	if ShouldTerminate(bb, CycleState{QuietCycles: 3, RemainingSeconds: 60}) {
		t.Fatal("platform mission used engagement stop rule")
	}
	_, _ = bb.AddFinding(blackboard.Finding{Title: "needs checking"})
	if ShouldTerminate(bb, state) {
		t.Fatal("unverified candidate was ignored")
	}
}
