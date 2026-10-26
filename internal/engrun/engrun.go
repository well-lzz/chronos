// Package engrun runs one engagement (a single multi-target mission) and
// presents it to the web console as a console.Source. Unlike the scheduler it
// has no platform/challenges — the console shows findings by verdict
// (confirmed / suspected / failed), the real-business vocabulary.
package engrun

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/console"
	"github.com/well-lzz/chronos/internal/mission"
	"github.com/well-lzz/chronos/internal/scope"
	"github.com/well-lzz/chronos/internal/swarm"
)

type Runner struct {
	sw           *swarm.Swarm
	bb           *blackboard.Blackboard
	meta         mission.Meta
	cancel       context.CancelFunc
	deadline     time.Time
	mu           sync.RWMutex
	persistScope func(scope.Scope) error
}

func New(sw *swarm.Swarm, bb *blackboard.Blackboard, meta mission.Meta, cancel context.CancelFunc, deadline time.Time, persistScope ...func(scope.Scope) error) *Runner {
	r := &Runner{sw: sw, bb: bb, meta: meta, cancel: cancel, deadline: deadline}
	if len(persistScope) > 0 {
		r.persistScope = persistScope[0]
	}
	return r
}

func (r *Runner) Snapshot() map[string]any {
	r.mu.RLock()
	meta := r.meta
	r.mu.RUnlock()
	vc := r.bb.VerdictCounts()
	cc := r.bb.Counts()
	facts := cc["facts"]
	var findings []map[string]any
	for _, f := range r.bb.FindingsPublic() {
		target := f.Target
		if target == "" {
			target = meta.Name
		}
		findings = append(findings, map[string]any{
			"id": f.ID, "title": f.Title, "severity": f.Severity, "verdict": f.Verdict,
			"status": f.Status, "target": target, "category": f.Category,
			"evidence": f.Evidence, "evidence_text": f.EvidenceText,
			"reproduction": f.Reproduction, "remediation": f.Remediation,
			"found_by": f.FoundBy, "ts": f.TS, "verified_ts": f.VerifiedTS,
			"evidence_refs": f.EvidenceRefs, "verifications": f.Verifications,
			"mission": meta.Name,
		})
	}
	console.SortFindings(findings)

	var subgoals []map[string]any
	for _, g := range r.bb.SubgoalsPublic() {
		subgoals = append(subgoals, map[string]any{
			"id": g.ID, "description": g.Description, "status": g.Status,
			"outcome": g.Outcome, "done": g.Done, "mission": meta.Name,
			"kind": g.Kind, "finding_id": g.FindingID, "assigned_to": g.AssignedTo,
		})
	}
	var trafficRows []map[string]any
	if r.sw.TL != nil {
		trafficRows = r.sw.TL.RecentRows(60)
		for _, row := range trafficRows {
			row["mission"] = meta.Name
		}
	}

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
			"goals_open": cc["subgoals_open"], "goals_total": cc["subgoals_total"],
			"goals_done": cc["subgoals_total"] - cc["subgoals_open"],
		},
		"findings": findings,
		"subgoals": subgoals,
		"traffic":  trafficRows,
		"swarms":   map[string]any{meta.Name: r.sw.Status()},
		"claims":   r.bb.ActiveLeases(),
		"engagement": map[string]any{
			"name": meta.Name, "scope": meta.ConnectionInfo,
			"targets": r.sw.CurrentScope().Entries(), "scope_editable": true,
		},
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
	case "chat":
		r.sw.Chat(model, message)
	}
}

func (r *Runner) ScopeConfig() console.ScopeConfig {
	current := r.sw.CurrentScope()
	return console.ScopeConfig{
		Supported: true, Editable: true, Targets: current.Entries(), Description: current.Describe(),
	}
}

func (r *Runner) UpdateScope(targets []string) error {
	if len(targets) == 0 {
		return fmt.Errorf("scope must contain at least one host, IP, URL, or CIDR")
	}
	if len(targets) > 64 {
		return fmt.Errorf("scope contains too many entries (maximum 64)")
	}
	clean := make([]string, 0, len(targets))
	for _, target := range targets {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		if len(target) > 512 {
			return fmt.Errorf("scope entry is too long")
		}
		clean = append(clean, target)
	}
	next := scope.Parse(clean)
	if next.IsEmpty() {
		return fmt.Errorf("scope parsed to no usable targets")
	}
	if r.persistScope != nil {
		if err := r.persistScope(next); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.meta.ConnectionInfo = next.Describe()
	r.mu.Unlock()
	r.sw.UpdateScope(next)
	return nil
}
