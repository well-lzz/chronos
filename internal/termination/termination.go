// Package termination is the single, user-defined stop condition for an
// executor. Engagements stop after explicit coverage is resolved and several
// quiet cycles. Platform missions retain their platform-confirmed condition.
package termination

import "github.com/well-lzz/chronos/internal/blackboard"

// CycleState is what the swarm knows at the end of one executor cycle.
type CycleState struct {
	StepCount           int
	BumpCount           int
	Cycles              int
	ProgressMarker      int
	PrevProgressMark    int
	ElapsedSeconds      float64
	QuietCycles         int
	IsEngagement        bool
	RemainingSeconds    float64
	QuietCyclesRequired int
}

func (c CycleState) MadeProgress() bool { return c.ProgressMarker > c.PrevProgressMark }

// ShouldTerminate stops an engagement once its explicit coverage checklist is
// complete, no candidate verification remains, and discovery has stayed quiet.
// No checklist means coverage is unknown, so the agent keeps searching.
func ShouldTerminate(bb *blackboard.Blackboard, state CycleState) bool {
	threshold := state.QuietCyclesRequired
	if threshold < 1 {
		threshold = 3
	}
	if !state.IsEngagement || bb == nil || state.RemainingSeconds <= 0 || state.QuietCycles < threshold {
		return false
	}
	counts := bb.Counts()
	return counts["subgoals_total"] > 0 && counts["subgoals_open"] == 0 && counts["findings_candidate"] == 0 && counts["findings_inconclusive"] == 0
}
