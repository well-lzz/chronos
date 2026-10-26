package solver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/well-lzz/chronos/internal/scope"
)

// ChildEvent is routed to an isolated, expandable child activity stream.
type ChildEvent struct {
	AgentID  string
	ParentID string
	Step     int
	Kind     string
	Tool     string
	Text     string
	Context  *ContextEvent
}

type delegatedChild struct {
	id             string
	parentID       string
	actor          string
	task           string
	rev0           uint64
	solver         *Solver
	dir            string
	status         string
	stoppedReason  string
	summary        string
	running        bool
	allocatedSteps int
}

type delegateResult struct {
	Status        string   `json:"status"`
	Summary       string   `json:"summary"`
	FindingIDs    []string `json:"finding_ids"`
	FactIDs       []string `json:"fact_ids"`
	FactsAdded    int      `json:"facts_added"`
	AgentID       string   `json:"agent_id"`
	StoppedReason string   `json:"stopped_reason,omitempty"`
}

type childMetadata struct {
	AgentID        string    `json:"agent_id"`
	ParentID       string    `json:"parent_id,omitempty"`
	Depth          int       `json:"depth"`
	Task           string    `json:"task"`
	Scope          []string  `json:"scope"`
	StartRevision  uint64    `json:"start_revision"`
	AllocatedSteps int       `json:"allocated_steps"`
	RemainingSteps int       `json:"remaining_steps"`
	Status         string    `json:"status"`
	StoppedReason  string    `json:"stopped_reason,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (s *Solver) canDelegate() bool { return s.depth < s.maxDelegateDepth }

func (s *Solver) delegateGate() string {
	if !s.canDelegate() {
		return "delegation disabled at this depth; continue directly"
	}
	if s.globalRemaining != nil && s.minRemainingToSpawn > 0 {
		remaining := s.globalRemaining()
		if remaining >= 0 && remaining < s.minRemainingToSpawn.Seconds() {
			return "global budget too low; delegation not started; continue directly"
		}
	}
	if time.Until(s.deadline) < 2*time.Second {
		return "mission deadline too near; delegation not started; continue directly"
	}
	return ""
}

func (s *Solver) childScope(focus string) (scope.Scope, error) {
	parent := s.scopeSnapshot()
	if strings.TrimSpace(focus) == "" {
		return parent, nil
	}
	child := scope.ParseConnInfo(focus)
	if child.IsEmpty() || !child.SubsetOf(parent) {
		return scope.Scope{}, fmt.Errorf("focus must be a nonempty subset of authorized scope (%s)", parent.Describe())
	}
	return child, nil
}

func (s *Solver) reserveChildSteps(requested int) (int, string) {
	if requested < 1 {
		return 0, "max_steps must be positive"
	}
	s.delegateMu.Lock()
	defer s.delegateMu.Unlock()
	if s.activeChildren >= s.maxDelegateChildren {
		return 0, "concurrent child limit reached"
	}
	grant := min(requested, s.maxDelegateSteps, s.delegateStepPool, s.cycleRemaining/2)
	if grant < 1 {
		return 0, "parent cycle has no remaining delegation step budget"
	}
	s.delegateStepPool -= grant
	s.cycleRemaining -= grant
	s.cycleDelegatedSteps += grant
	s.delegatedStepsTotal += grant
	s.activeChildren++
	return grant, ""
}

func (s *Solver) setCycleRemaining(remaining int) int {
	s.delegateMu.Lock()
	s.cycleRemaining = max(0, remaining-s.cycleDelegatedSteps)
	if s.stepLimit > 0 {
		s.cycleRemaining = min(s.cycleRemaining, max(0, s.stepLimit-s.steps-s.delegatedStepsTotal))
	}
	available := s.cycleRemaining
	s.delegateMu.Unlock()
	return available
}

func (s *Solver) releaseChild() {
	s.delegateMu.Lock()
	s.activeChildren--
	s.delegateMu.Unlock()
}

func (s *Solver) restoreReservation(grant int) {
	s.delegateMu.Lock()
	s.delegateStepPool += grant
	s.cycleRemaining += grant
	s.cycleDelegatedSteps -= grant
	s.delegatedStepsTotal -= grant
	s.activeChildren--
	s.delegateMu.Unlock()
}

func newAgentID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func (s *Solver) childDirectory(id string) string {
	if s.subagentRoot == "" {
		return ""
	}
	return filepath.Join(s.subagentRoot, id)
}

func (c *delegatedChild) persist() error {
	if c.dir == "" {
		return nil
	}
	m := childMetadata{AgentID: c.id, ParentID: c.parentID, Depth: c.solver.depth, Task: c.task,
		Scope: c.solver.scopeSnapshot().Entries(), StartRevision: c.rev0, AllocatedSteps: c.allocatedSteps,
		RemainingSteps: max(0, c.solver.stepLimit-c.solver.steps-c.solver.delegatedStepsTotal), Status: c.status,
		StoppedReason: c.stoppedReason, UpdatedAt: time.Now()}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(c.dir, "metadata.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(c.dir, "metadata.json")); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *Solver) toolDelegate(ctx context.Context, argsJSON string) string {
	if reason := s.delegateGate(); reason != "" {
		return jsonOut(map[string]any{"status": "stopped", "stopped_reason": reason})
	}
	var args struct {
		Task     string `json:"task"`
		MaxSteps int    `json:"max_steps"`
		Focus    string `json:"focus"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "delegate error: bad args"
	}
	args.Task = strings.TrimSpace(args.Task)
	if args.Task == "" || len(args.Task) > 8000 {
		return "delegate error: task must contain 1–8000 bytes"
	}
	childScope, err := s.childScope(args.Focus)
	if err != nil {
		return "delegate error: " + err.Error()
	}
	grant, reason := s.reserveChildSteps(args.MaxSteps)
	if reason != "" {
		return jsonOut(map[string]any{"status": "stopped", "stopped_reason": reason})
	}
	id, err := newAgentID()
	if err != nil {
		s.restoreReservation(grant)
		return "delegate error: " + err.Error()
	}
	actor := s.ModelSpec + "/sub:" + id
	dir := s.childDirectory(id)
	childMeta := s.meta
	childMeta.Description = args.Task
	child := New(Options{
		ModelSpec: actor, Model: s.Model, Client: s.client, Meta: childMeta, Scope: childScope,
		BB: s.bb, TL: s.tl, AllowExec: s.allowExec, InsecureTLS: s.insecureTLS,
		Deadline: s.childDeadline(), Tokens: s.tokens, SubmitFn: s.submitFn,
		Resolve: s.resolve, ContextWindow: s.contextWindow,
		Depth: s.depth + 1, MaxDelegateDepth: s.maxDelegateDepth, MaxDelegateChildren: s.maxDelegateChildren,
		MaxDelegateSteps: s.maxDelegateSteps, MinRemainingToSpawn: s.minRemainingToSpawn,
		GlobalRemaining: s.globalRemaining, SubagentRoot: s.subagentRoot,
		AgentID: id, ParentID: s.agentID, StepLimit: grant,
		SessionPath: func() string {
			if dir == "" {
				return ""
			}
			return filepath.Join(dir, "session.jsonl")
		}(),
		Record: func(step int, kind, tool, text string) {
			s.childEvent(ChildEvent{AgentID: id, ParentID: s.agentID, Step: step, Kind: kind, Tool: tool, Text: text})
		},
		RecordContext: func(event ContextEvent) {
			s.childEvent(ChildEvent{AgentID: id, ParentID: s.agentID, Kind: "context", Text: fmt.Sprintf("%s: %d/%d chars", event.Phase, event.Summary.ActualChars, event.BudgetChars), Context: &event})
		},
		RecordChildEvent: s.recordChildEvent,
		VerificationPeer: s.verificationPeer,
		Notify: func(message string) {
			log.Printf("[%s/sub:%s] %s", s.meta.Name, id, message)
			s.childEvent(ChildEvent{AgentID: id, ParentID: s.agentID, Kind: "notify", Text: message})
		},
	})
	record := &delegatedChild{id: id, parentID: s.agentID, actor: actor, task: args.Task, rev0: s.bb.Revision(), solver: child, dir: dir, status: "running", running: true, allocatedSteps: grant}
	if err := record.persist(); err != nil {
		s.restoreReservation(grant)
		return "delegate persistence error: " + err.Error()
	}
	s.delegateMu.Lock()
	s.children[id] = record
	s.delegateMu.Unlock()
	// The operator may have narrowed the parent scope while the child was being
	// constructed. Registration precedes this check so later updates also see it.
	if current := s.scopeSnapshot(); !child.scopeSnapshot().SubsetOf(current) {
		child.SetScope(child.scopeSnapshot().Within(current))
	}
	s.rec("delegate_spawn", id, fmt.Sprintf("⇲ delegated: %s (max %d steps, depth %d)", clipRunes(args.Task, 140), grant, child.depth))
	return s.runChild(ctx, record)
}

func (s *Solver) toolDelegateResume(ctx context.Context, argsJSON string) string {
	if reason := s.delegateGate(); reason != "" {
		return jsonOut(map[string]any{"status": "stopped", "stopped_reason": reason})
	}
	var args struct {
		AgentID    string `json:"agent_id"`
		ExtraSteps int    `json:"extra_steps"`
		Note       string `json:"note"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "delegate_resume error: bad args"
	}
	if len(args.Note) > 4000 {
		return "delegate_resume error: note too long"
	}
	s.delegateMu.Lock()
	record := s.children[args.AgentID]
	if record == nil || record.parentID != s.agentID || record.solver == nil || record.status == "completed" {
		s.delegateMu.Unlock()
		return "delegate_resume error: no unfinished direct child with that agent_id"
	}
	if record.running {
		s.delegateMu.Unlock()
		return "delegate_resume error: child is already running"
	}
	s.delegateMu.Unlock()
	grant, reason := s.reserveChildSteps(args.ExtraSteps)
	if reason != "" {
		return jsonOut(map[string]any{"status": "stopped", "stopped_reason": reason})
	}
	s.delegateMu.Lock()
	if record.running {
		s.delegateMu.Unlock()
		s.restoreReservation(grant)
		return "delegate_resume error: child is already running"
	}
	record.running = true
	record.status = "running"
	previousLimit := record.solver.stepLimit
	record.solver.stepLimit += grant
	record.solver.deadline = s.childDeadline()
	record.allocatedSteps += grant
	s.delegateMu.Unlock()
	if err := record.persist(); err != nil {
		s.delegateMu.Lock()
		record.running = false
		record.solver.stepLimit = previousLimit
		record.allocatedSteps -= grant
		s.delegateMu.Unlock()
		s.restoreReservation(grant)
		return "delegate_resume persistence error: " + err.Error()
	}
	if note := strings.TrimSpace(args.Note); note != "" {
		record.solver.Guide(note)
	}
	s.rec("delegate_resume", record.id, "↻ resumed "+record.id)
	return s.runChild(ctx, record)
}

func (s *Solver) childDeadline() time.Time {
	remaining := time.Until(s.deadline)
	slice := remaining / 2
	if slice > 2*time.Minute {
		slice = 2 * time.Minute
	}
	if slice < time.Second {
		slice = time.Second
	}
	return time.Now().Add(slice)
}

func (s *Solver) childEvent(event ChildEvent) {
	if s.recordChildEvent != nil {
		s.recordChildEvent(event)
	}
}

func (s *Solver) runChild(ctx context.Context, record *delegatedChild) string {
	defer s.releaseChild()
	defer func() { _ = s.bb.ReleaseOwner(record.actor) }()
	childCtx, cancel := context.WithDeadline(ctx, record.solver.deadline)
	defer cancel()
	res := record.solver.RunUntilDoneOrGaveUp(childCtx)
	status, reason := "inconclusive", res.StopReason
	if reason == "done" || res.Status == StatusFlagFound {
		status, reason = "completed", "done"
	}
	if res.Status == StatusStepBudget {
		status, reason = "stopped", "max_steps"
	}
	if res.Status == StatusCancelled || reason == "deadline" {
		status = "stopped"
		if childCtx.Err() == context.DeadlineExceeded || time.Now().After(record.solver.deadline) {
			reason = "deadline"
		} else {
			reason = "cancelled"
		}
	}
	if res.Status == StatusGaveUp && reason == "max_cycle_steps" {
		status = "stopped"
	}
	summary := strings.TrimSpace(res.Findings)
	if summary == "" {
		summary = fmt.Sprintf("Sub-investigation %s after %d steps; see shared blackboard for artifacts.", status, res.StepCount)
	}
	summary = clipRunes(summary, 240)
	facts, findings := s.bb.ArtifactsSince(record.rev0, record.actor)
	result := delegateResult{Status: status, Summary: summary, FindingIDs: limitIDs(findings), FactIDs: limitIDs(facts),
		FactsAdded: len(facts), AgentID: record.id, StoppedReason: reason}
	s.delegateMu.Lock()
	record.status, record.stoppedReason, record.summary = status, reason, summary
	s.delegateMu.Unlock()
	if err := record.persist(); err != nil {
		log.Printf("subagent metadata write failed (%s): %v", record.id, err)
	}
	s.delegateMu.Lock()
	record.running = false
	s.delegateMu.Unlock()
	s.rec("delegate_return", record.id, fmt.Sprintf("⇱ returned: %v +%d facts (%s; %s)", result.FindingIDs, result.FactsAdded, status, reason))
	return jsonOut(result)
}

func limitIDs(ids []string) []string {
	if len(ids) > 20 {
		return ids[:20]
	}
	if ids == nil {
		return []string{}
	}
	return ids
}

func (s *Solver) narrowChildren(next scope.Scope) {
	s.delegateMu.Lock()
	children := make([]*delegatedChild, 0, len(s.children))
	for _, c := range s.children {
		if c.solver != nil {
			children = append(children, c)
		}
	}
	s.delegateMu.Unlock()
	for _, c := range children {
		c.solver.SetScope(c.solver.scopeSnapshot().Within(next))
	}
}
