// Package engrun runs one engagement (a single multi-target mission) and
// presents it to the web console as a console.Source. Unlike the scheduler it
// has no platform/challenges — the console shows findings by verdict
// (confirmed / suspected / failed), the real-business vocabulary.
package engrun

import (
	"context"
	"time"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/console"
	"github.com/well-lzz/chronos/internal/mission"
	"github.com/well-lzz/chronos/internal/swarm"
)

type Runner struct {
	sw       *swarm.Swarm
	bb       *blackboard.Blackboard
	meta     mission.Meta
	cancel   context.CancelFunc
	deadline time.Time
}

func New(sw *swarm.Swarm, bb *blackboard.Blackboard, meta mission.Meta, cancel context.CancelFunc, deadline time.Time) *Runner {
	return &Runner{sw: sw, bb: bb, meta: meta, cancel: cancel, deadline: deadline}
}

func (r *Runner) Snapshot() map[string]any {
	vc := r.bb.VerdictCounts()
	facts := r.bb.Counts()["facts"]
	var findings []map[string]any
	for _, f := range r.bb.FindingsPublic() {
		target := f.Target
		if target == "" {
			target = r.meta.Name
		}
		findings = append(findings, map[string]any{
			"id": f.ID, "title": f.Title, "severity": f.Severity, "verdict": f.Verdict,
			"target": target, "category": f.Category, "evidence": f.Evidence, "mission": r.meta.Name,
		})
	}
	console.SortFindings(findings)
	budget := -1
	if rem := int(time.Until(r.deadline).Seconds()); rem > 0 {
		budget = rem / 60
	}
	return map[string]any{
		"mode": "engagement",
		"summary": map[string]any{
			"confirmed": vc["confirmed"], "suspected": vc["suspected"], "failed": vc["failed"],
			"findings": vc["total"], "facts": facts, "active": 1,
			"tokens": r.sw.TotalTokens(), "budget_left_min": budget,
		},
		"findings":   findings,
		"swarms":     map[string]any{r.meta.Name: r.sw.Status()},
		"engagement": map[string]any{"name": r.meta.Name, "scope": r.meta.ConnectionInfo},
	}
}

func (r *Runner) Activity(challenge, model string, after int) []swarm.ActEvent {
	return r.sw.Activity(model, after)
}

func (r *Runner) Command(action, challenge, model, message string) {
	switch action {
	case "pause":
		r.sw.Pause()
	case "resume":
		r.sw.Resume()
	case "kill":
		r.cancel()
	case "bump":
		r.sw.Bump(model, message)
	}
}
