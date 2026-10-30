package termination

// StallPolicy turns "no progress" into a graded response: first nudge the
// executor to change approach (a pivot), and only terminate once several
// materially different approaches have each failed to move the working record.
//
// It keys off PROGRESS (the blackboard marker), never wall-clock time. An
// executor that is still recording new facts/findings — even slowly — resets the
// streak and is never pivoted or stopped, so a slow-but-advancing solve keeps its
// full per-challenge deadline. The terminate decision only fires after the whole
// pivot ladder is exhausted while the marker stays flat, which frees the slot for
// other challenges far sooner than burning the full deadline doing nothing.
type StallPolicy struct {
	cfg    StallConfig
	streak int // consecutive no-progress cycles since the last progress or pivot
	pivots int // pivots issued so far (the escalation level)
}

// StallConfig bounds the ladder. Zero values fall back to conservative defaults.
type StallConfig struct {
	// PivotAfter is the number of consecutive no-progress cycles before the FIRST
	// pivot. Higher = more patient before nudging.
	PivotAfter int
	// PivotGrace is the no-progress cycles granted after each pivot to show new
	// progress before escalating again.
	PivotGrace int
	// MaxPivots is how many escalating pivots to try before terminating.
	MaxPivots int
}

func (c StallConfig) withDefaults() StallConfig {
	if c.PivotAfter <= 0 {
		c.PivotAfter = 3
	}
	if c.PivotGrace <= 0 {
		c.PivotGrace = 2
	}
	if c.MaxPivots <= 0 {
		c.MaxPivots = 3
	}
	return c
}

// Decision is what to do after one observed cycle. At most one of Pivot/Terminate
// is set; both false means "keep going unchanged".
type Decision struct {
	Pivot     bool
	Directive string // generic, challenge-agnostic methodology nudge to inject
	Terminate bool
}

// NewStallPolicy builds a policy from cfg (defaults applied for zero fields).
func NewStallPolicy(cfg StallConfig) *StallPolicy {
	return &StallPolicy{cfg: cfg.withDefaults()}
}

// Observe records one cycle's progress and returns the resulting decision.
// madeProgress is CycleState.MadeProgress() for the cycle just finished.
func (p *StallPolicy) Observe(madeProgress bool) Decision {
	if madeProgress {
		// Real progress — the executor is productive. Reset BOTH the stall streak and
		// the accumulated pivot level, so a task that keeps finding new things earns a
		// fresh pivot ladder each time and is never fast-tracked to termination by
		// pivots spent long ago.
		p.streak = 0
		p.pivots = 0
		return Decision{}
	}
	p.streak++

	threshold := p.cfg.PivotAfter
	if p.pivots > 0 {
		threshold = p.cfg.PivotGrace // after the first pivot, escalate on the shorter grace window
	}
	if p.streak < threshold {
		return Decision{}
	}

	if p.pivots >= p.cfg.MaxPivots {
		return Decision{Terminate: true} // every escalation failed; stop and free the slot
	}
	p.pivots++
	p.streak = 0
	return Decision{Pivot: true, Directive: pivotDirective(p.pivots)}
}

// pivotDirective returns the escalation-level nudge. These are deliberately
// generic — the same text for every challenge, pure methodology, no target
// specifics — so they steer strategy without being challenge tuning.
func pivotDirective(level int) string {
	switch level {
	case 1:
		return "No new facts or findings have been recorded for several cycles — the current approach looks stuck. " +
			"Switch to a materially different technique for the SAME weakness (a different encoding, a different way of handling the filter, or a different technique), " +
			"or re-read the working record for a detail you overlooked. Do not repeat the exact approach that just failed."
	case 2:
		return "Still no progress after changing technique. Change the INPUT SURFACE: enumerate endpoints, parameters, or inputs you have not tried yet, " +
			"and consider a different weakness class than the one you have been pursuing on this target."
	default:
		return "Repeated approaches have not advanced the working record. Step back and RE-MAP the target from scratch — " +
			"re-check what the application does, its inputs, and its trust boundaries — then pick the most promising lead you have NOT already tried."
	}
}
