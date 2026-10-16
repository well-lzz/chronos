// Package termination is the single, user-defined stop condition for an
// executor. The search has no built-in "done" signal: an executor keeps
// searching and recording Findings until this predicate says stop, or a hard
// safety bound (max bumps / wall-clock budget) is hit. Define your policy in
// ShouldTerminate — coverage, diminishing returns, a finding-count threshold,
// whatever fits. The default never terminates early.
package termination

import "github.com/well-lzz/chronos/internal/blackboard"

// CycleState is what the swarm knows at the end of one executor cycle.
type CycleState struct {
	StepCount        int
	BumpCount        int
	Cycles           int
	ProgressMarker   int
	PrevProgressMark int
	ElapsedSeconds   float64
}

func (c CycleState) MadeProgress() bool { return c.ProgressMarker > c.PrevProgressMark }

// ShouldTerminate returns true to stop this executor. DEFINE YOUR POLICY HERE.
//
// Default: never stop early — let the safety bounds end the run. Example:
//
//	// diminishing returns: quiet cycle with nothing left to chase
//	// if !state.MadeProgress() && bb != nil && len(bb.OpenSubgoals()) == 0 { ... }
//
//	// enough verified findings:
//	// if bb != nil && bb.Counts()["findings_verified"] >= 10 { return true }
func ShouldTerminate(bb *blackboard.Blackboard, state CycleState) bool {
	_ = bb
	_ = state
	return false
}
