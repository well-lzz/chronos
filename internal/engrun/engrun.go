// Package engrun runs one engagement (a single multi-target mission) and
// presents it to the web console as a console.Source. Unlike the scheduler it
// has no platform/challenges — the console shows findings by verdict
// (confirmed / suspected / failed), the real-business vocabulary.
package engrun

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/console"
	"github.com/well-lzz/chronos/internal/mission"
	"github.com/well-lzz/chronos/internal/scope"
	"github.com/well-lzz/chronos/internal/swarm"
)

type Runner struct {
	sw            *swarm.Swarm
	bb            *blackboard.Blackboard
	meta          mission.Meta
	cancel        context.CancelFunc
	deadline      time.Time
	started       time.Time
	mu            sync.RWMutex
	scopeUpdateMu sync.Mutex
	prepareRun    func(scope.Scope) (IsolatedRun, error)
	runCancel     context.CancelFunc
	running       bool
	pending       *runTransition
	runSwarm      func(context.Context, *swarm.Swarm)
	onRunStopped  func(*swarm.Swarm, *blackboard.Blackboard, mission.Meta, time.Time)
	onRunStarted  func(*swarm.Swarm, *blackboard.Blackboard, mission.Meta, time.Time)
}

type IsolatedRun struct {
	Swarm      *swarm.Swarm
	Blackboard *blackboard.Blackboard
	Meta       mission.Meta
	Deadline   time.Time
	Started    time.Time
}

type runTransition struct {
	next  IsolatedRun
	ready chan struct{}
}

func New(sw *swarm.Swarm, bb *blackboard.Blackboard, meta mission.Meta, cancel context.CancelFunc, deadline time.Time) *Runner {
	return &Runner{sw: sw, bb: bb, meta: meta, cancel: cancel, deadline: deadline, started: time.Now(),
		runSwarm: func(ctx context.Context, current *swarm.Swarm) { current.Run(ctx) }}
}

func (r *Runner) SetStarted(started time.Time) { r.mu.Lock(); r.started = started; r.mu.Unlock() }

func (r *Runner) SetOnRunStopped(fn func(*swarm.Swarm, *blackboard.Blackboard, mission.Meta, time.Time)) {
	r.mu.Lock()
	r.onRunStopped = fn
	r.mu.Unlock()
}

func (r *Runner) SetOnRunStarted(fn func(*swarm.Swarm, *blackboard.Blackboard, mission.Meta, time.Time)) {
	r.mu.Lock()
	r.onRunStarted = fn
	r.mu.Unlock()
}

func (r *Runner) Current() (*swarm.Swarm, *blackboard.Blackboard, mission.Meta, time.Time) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sw, r.bb, r.meta, r.started
}

func (r *Runner) Deadline() time.Time { r.mu.RLock(); defer r.mu.RUnlock(); return r.deadline }

func (r *Runner) SetRunFactory(factory func(scope.Scope) (IsolatedRun, error)) {
	r.mu.Lock()
	r.prepareRun = factory
	r.mu.Unlock()
}

// Start runs each scope as a separate executor session. A scope transition
// cancels and joins the old swarm before publishing and starting the new one.
func (r *Runner) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	r.mu.Lock()
	r.running = true
	currentCtx, currentCancel := runContext(ctx, r.deadline)
	r.runCancel = currentCancel
	r.mu.Unlock()
	go func() {
		defer close(done)
		for {
			r.mu.RLock()
			current := r.sw
			r.mu.RUnlock()
			r.runSwarm(currentCtx, current)
			currentCancel()
			r.mu.RLock()
			archive, oldBB, oldMeta, oldStarted := r.onRunStopped, r.bb, r.meta, r.started
			r.mu.RUnlock()
			r.mu.Lock()
			request := r.pending
			if request == nil || ctx.Err() != nil {
				r.running = false
				r.runCancel = nil
				r.mu.Unlock()
				if request != nil {
					close(request.ready)
				}
				return
			}
			r.mu.Unlock()
			if archive != nil {
				archive(current, oldBB, oldMeta, oldStarted)
			}
			r.mu.Lock()
			if ctx.Err() != nil {
				r.running = false
				r.runCancel = nil
				r.mu.Unlock()
				close(request.ready)
				return
			}
			r.sw, r.bb, r.meta, r.deadline, r.started = request.next.Swarm, request.next.Blackboard, request.next.Meta, request.next.Deadline, request.next.Started
			started := r.onRunStarted
			r.pending = nil
			currentCtx, currentCancel = runContext(ctx, r.deadline)
			r.runCancel = currentCancel
			r.mu.Unlock()
			if started != nil {
				started(request.next.Swarm, request.next.Blackboard, request.next.Meta, request.next.Started)
			}
			close(request.ready)
		}
	}()
	return done
}

func runContext(parent context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	if !deadline.IsZero() {
		return context.WithDeadline(parent, deadline)
	}
	return context.WithCancel(parent)
}

func (r *Runner) Snapshot() map[string]any {
	r.mu.RLock()
	meta, sw, bb, deadline := r.meta, r.sw, r.bb, r.deadline
	r.mu.RUnlock()
	vc := bb.VerdictCounts()
	cc := bb.Counts()
	facts := cc["facts"]
	var findings []map[string]any
	for _, f := range bb.FindingsPublic() {
		target := f.Target
		if target == "" {
			target = meta.Name
		}
		findings = append(findings, map[string]any{
			"id": f.ID, "title": f.Title, "severity": f.Severity, "verdict": f.Verdict,
			"status": f.Status, "verdict_reason": f.VerdictReason, "target": target, "category": f.Category,
			"evidence": f.Evidence, "evidence_text": f.EvidenceText,
			"reproduction": f.Reproduction, "remediation": f.Remediation,
			"found_by": f.FoundBy, "ts": f.TS, "verified_ts": f.VerifiedTS,
			"evidence_refs": f.EvidenceRefs, "verifications": f.Verifications,
			"mission": meta.Name,
		})
	}
	console.SortFindings(findings)

	var subgoals []map[string]any
	for _, g := range bb.SubgoalsPublic() {
		subgoals = append(subgoals, map[string]any{
			"id": g.ID, "description": g.Description, "status": g.Status,
			"outcome": g.Outcome, "done": g.Done, "mission": meta.Name,
			"kind": g.Kind, "finding_id": g.FindingID, "assigned_to": g.AssignedTo,
		})
	}
	var trafficRows []map[string]any
	if sw.TL != nil {
		trafficRows = sw.TL.RecentRows(60)
		for _, row := range trafficRows {
			row["mission"] = meta.Name
		}
	}

	budget := -1
	if rem := int(time.Until(deadline).Seconds()); rem > 0 {
		budget = rem / 60
	}
	return map[string]any{
		"mode": "engagement",
		"summary": map[string]any{
			"confirmed": vc["confirmed"], "suspected": vc["suspected"], "failed": vc["failed"],
			"findings": vc["total"], "facts": facts, "active": 1,
			"tokens": sw.TotalTokens(), "budget_left_min": budget,
			"goals_open": cc["subgoals_open"], "goals_total": cc["subgoals_total"],
			"goals_done": cc["subgoals_total"] - cc["subgoals_open"],
		},
		"findings":        findings,
		"subgoals":        subgoals,
		"traffic":         trafficRows,
		"swarms":          map[string]any{meta.Name: sw.Status()},
		"claims":          bb.ActiveLeases(),
		"evidence_graphs": map[string]blackboard.GraphSnapshot{meta.Name: bb.Graph()},
		"engagement": map[string]any{
			"name": meta.Name, "scope": meta.ConnectionInfo,
			"targets": sw.CurrentScope().Entries(), "scope_editable": true,
		},
	}
}

func (r *Runner) Activity(challenge, model string, after int) []swarm.ActEvent {
	r.mu.RLock()
	sw := r.sw
	r.mu.RUnlock()
	return sw.Activity(model, after)
}

func (r *Runner) Command(action, challenge, model, message string) {
	r.mu.RLock()
	sw := r.sw
	r.mu.RUnlock()
	switch action {
	case "pause":
		sw.Pause()
	case "resume":
		if !sw.CurrentScope().IsEmpty() {
			sw.Resume()
		}
	case "kill":
		r.cancel()
	case "bump":
		sw.Bump(model, message)
	case "chat":
		_ = r.Chat(challenge, model, message)
	}
}

func (r *Runner) CommandChecked(action, challenge, model, message string) error {
	r.mu.RLock()
	sw := r.sw
	r.mu.RUnlock()
	if action == "resume" && sw.CurrentScope().IsEmpty() {
		return fmt.Errorf("set and save an authorized scope before resuming")
	}
	r.Command(action, challenge, model, message)
	return nil
}

func (r *Runner) Chat(_ string, model, message string) error {
	r.mu.RLock()
	sw := r.sw
	r.mu.RUnlock()
	if !sw.Chat(model, message) {
		return fmt.Errorf("selected executor is unavailable; choose a running executor")
	}
	return nil
}

func (r *Runner) ScopeConfig() console.ScopeConfig {
	r.mu.RLock()
	sw := r.sw
	r.mu.RUnlock()
	current := sw.CurrentScope()
	return console.ScopeConfig{
		Supported: true, Editable: true, Targets: current.Entries(), Description: current.Describe(),
	}
}

func (r *Runner) UpdateScope(targets []string) error {
	next, err := scope.ParseOperatorTargets(targets)
	if err != nil {
		return err
	}
	r.scopeUpdateMu.Lock()
	defer r.scopeUpdateMu.Unlock()
	r.mu.RLock()
	current, factory, running := r.sw.CurrentScope(), r.prepareRun, r.running
	r.mu.RUnlock()
	if slices.Equal(current.Entries(), next.Entries()) {
		return nil
	}
	if !running || factory == nil {
		return fmt.Errorf("scope change requires an active isolated run")
	}
	prepared, err := factory(next)
	if err != nil {
		return err
	}
	if prepared.Swarm == nil || prepared.Blackboard == nil {
		return fmt.Errorf("scope change did not prepare an isolated run")
	}
	r.mu.RLock()
	wasPaused := r.sw.Paused()
	r.mu.RUnlock()
	if wasPaused {
		prepared.Swarm.Pause()
	}
	request := &runTransition{next: prepared, ready: make(chan struct{})}
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return fmt.Errorf("engagement ended before scope change")
	}
	r.pending = request
	cancel := r.runCancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	<-request.ready
	r.mu.RLock()
	applied := r.sw == prepared.Swarm
	r.mu.RUnlock()
	if !applied {
		return fmt.Errorf("engagement stopped before new scope started")
	}
	return nil
}
