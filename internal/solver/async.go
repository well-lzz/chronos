package solver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	openai "github.com/sashabaranov/go-openai"

	"github.com/well-lzz/chronos/internal/scope"
)

const StatusWaitingEvent = "waiting_event"

type asyncKey struct{}
type asyncJob struct {
	record    *delegatedChild
	result    string
	delivered bool
	cancel    context.CancelFunc
	done      chan struct{}
}
type asyncJobs struct {
	mu       sync.Mutex
	items    map[string]*asyncJob
	wake     chan struct{}
	slots    int
	restored bool
}

func (s *Solver) initJobs() *asyncJobs {
	s.delegateMu.Lock()
	defer s.delegateMu.Unlock()
	if s.jobs == nil {
		s.jobs = &asyncJobs{items: map[string]*asyncJob{}, wake: make(chan struct{}, 1)}
	}
	return s.jobs
}

func (s *Solver) toolDelegateStart(ctx context.Context, args string) string {
	if !s.asyncDelegate || s.missionContext == nil {
		return "asynchronous delegation is disabled"
	}
	jobs := s.initJobs()
	jobs.mu.Lock()
	if jobs.slots >= s.maxDelegateChildren {
		jobs.mu.Unlock()
		return "concurrent child limit reached"
	}
	jobs.slots++
	jobs.mu.Unlock()
	result := s.toolDelegate(context.WithValue(ctx, asyncKey{}, true), args)
	var receipt struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal([]byte(result), &receipt)
	if receipt.Status != "started" {
		jobs.mu.Lock()
		jobs.slots--
		jobs.mu.Unlock()
	}
	return result
}

func (s *Solver) launchAsync(record *delegatedChild) string {
	jobs := s.initJobs()
	ctx, cancel := context.WithCancel(s.missionContext)
	job := &asyncJob{record: record, cancel: cancel, done: make(chan struct{})}
	jobs.mu.Lock()
	jobs.items[record.id] = job
	jobs.mu.Unlock()
	go func() {
		defer cancel()
		result := s.runChild(ctx, record)
		jobs.mu.Lock()
		job.result = result
		jobs.slots--
		close(job.done)
		jobs.mu.Unlock()
		select {
		case jobs.wake <- struct{}{}:
		default:
		}
	}()
	return jsonOut(map[string]string{"status": "started", "agent_id": record.id})
}

func (s *Solver) toolDelegateStatus(args string) string {
	var a struct {
		AgentID string `json:"agent_id"`
	}
	if json.Unmarshal([]byte(args), &a) != nil {
		return "invalid child id"
	}
	jobs := s.initJobs()
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	job := jobs.items[a.AgentID]
	if job == nil {
		return "unknown direct child"
	}
	select {
	case <-job.done:
		return job.result
	default:
		return jsonOut(map[string]string{"status": "running", "agent_id": a.AgentID})
	}
}

func (s *Solver) toolDelegateCancel(args string) string {
	var a struct {
		AgentID string `json:"agent_id"`
	}
	if json.Unmarshal([]byte(args), &a) != nil {
		return "invalid child id"
	}
	jobs := s.initJobs()
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	job := jobs.items[a.AgentID]
	if job == nil {
		return "unknown direct child"
	}
	job.cancel()
	return jsonOut(map[string]string{"status": "cancellation_requested", "agent_id": a.AgentID})
}

// PollJobs transfers completed results into the durable inbox. Marking a result
// delivered happens after queue commit; a crash between the two is harmless
// because the input ID is stable across restart and handoff.
func (s *Solver) PollJobs() error {
	if !s.asyncDelegate {
		return nil
	}
	jobs := s.initJobs()
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	for id, job := range jobs.items {
		if job.delivered {
			continue
		}
		select {
		case <-job.done:
		default:
			continue
		}
		if err := s.DeliverInput(fmt.Sprintf("child:%s:%d", id, job.record.attempt), "child", "Child report (untrusted observations; verify against captured evidence):\n"+job.result); err != nil {
			return err
		}
		job.delivered = true
	}
	return nil
}

func (s *Solver) HasJobs() bool {
	if !s.asyncDelegate {
		return false
	}
	jobs := s.initJobs()
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	for _, job := range jobs.items {
		if !job.delivered {
			return true
		}
		select {
		case <-job.done:
			if job.record.status == "needs_reconciliation" {
				return true
			}
		default:
			return true
		}
	}
	return false
}

func (s *Solver) WaitEvent(ctx context.Context) error {
	jobs := s.initJobs()
	s.messageMu.Lock()
	pending := len(s.pendingUser) > 0
	s.messageMu.Unlock()
	if pending {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-jobs.wake:
		return nil
	case <-s.inputWake:
		return nil
	}
}

// CloseJobs is a lifecycle join, not a detached goroutine cancellation. Scope
// transitions and mission shutdown wait until every child has released tools.
func (s *Solver) CloseJobs() {
	if !s.asyncDelegate {
		return
	}
	jobs := s.initJobs()
	jobs.mu.Lock()
	var done []chan struct{}
	for _, job := range jobs.items {
		job.cancel()
		done = append(done, job.done)
	}
	jobs.mu.Unlock()
	for _, ch := range done {
		<-ch
	}
}

// RestoreJobs keeps unfinished actions stopped for reconciliation. Completed
// results are reconstructed from durable metadata and board references, without
// running a tool or the child model. Explicit delegate_resume can then continue
// an interrupted child's preserved window with a fresh bounded allocation.
func (s *Solver) RestoreJobs() error {
	if s.subagentRoot == "" {
		return nil
	}
	jobs := s.initJobs()
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	if jobs.restored {
		return nil
	}
	entries, err := os.ReadDir(s.subagentRoot)
	if os.IsNotExist(err) {
		jobs.restored = true
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(s.subagentRoot, entry.Name())
		data, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		var m childMetadata
		if err := json.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("child metadata %s: %w", entry.Name(), err)
		}
		if !m.Async || m.ParentActor != s.ModelSpec {
			continue
		}
		if !s.asyncDelegate {
			return fmt.Errorf("run contains asynchronous child state; restart with CHRONOS_ASYNC_DELEGATE=true or use a new run")
		}
		if m.Version != 1 || m.RunID != s.bb.MissionID || m.AgentID != entry.Name() {
			return fmt.Errorf("child metadata identity/version mismatch")
		}
		if _, ok := jobs.items[m.AgentID]; ok {
			continue
		}
		childScope := scope.Parse(m.Scope)
		status, reason := m.Status, m.StoppedReason
		if status == "running" || status == "queued" {
			status, reason = "needs_reconciliation", "host restarted; prior external effects may be unknown; inspect evidence before resuming"
		}
		if !childScope.SubsetOf(s.scopeSnapshot()) {
			status, reason = "needs_reconciliation", "authorized scope no longer matches"
		}
		actor := s.ModelSpec + "/sub:" + m.AgentID
		c := &delegatedChild{id: m.AgentID, parentID: m.ParentID, actor: actor, task: m.Task, rev0: m.StartRevision, dir: dir, status: status, summary: m.Summary, stoppedReason: reason, allocatedSteps: m.AllocatedSteps, async: true, parentActor: s.ModelSpec, attempt: m.Attempt, savedScope: m.Scope}
		if s.budget != nil {
			if err := s.budget.Release(m.AgentID); err != nil {
				return err
			}
		}
		facts, findings := s.bb.ArtifactsSince(c.rev0, c.actor)
		result := jsonOut(delegateResult{Status: status, Summary: m.Summary, FactIDs: limitIDs(facts), FindingIDs: limitIDs(findings), FactsAdded: len(facts), AgentID: m.AgentID, StoppedReason: reason})
		ch := make(chan struct{})
		close(ch)
		jobs.items[m.AgentID] = &asyncJob{record: c, result: result, done: ch, cancel: func() {}}
	}
	jobs.restored = true
	return nil
}

func (s *Solver) resumeAsync(ctx context.Context, args string) (string, bool) {
	var a struct {
		AgentID    string `json:"agent_id"`
		ExtraSteps int    `json:"extra_steps"`
		Note       string `json:"note"`
	}
	if json.Unmarshal([]byte(args), &a) != nil {
		return "invalid child resume arguments", true
	}
	jobs := s.initJobs()
	jobs.mu.Lock()
	job := jobs.items[a.AgentID]
	if job == nil {
		jobs.mu.Unlock()
		return "", false
	}
	select {
	case <-job.done:
	default:
		jobs.mu.Unlock()
		return "child is still running", true
	}
	c := job.record
	if c.status == "completed" || jobs.slots >= s.maxDelegateChildren || len(a.Note) > 4000 {
		jobs.mu.Unlock()
		return "child cannot be resumed", true
	}
	if reason := s.delegateGate(); reason != "" {
		jobs.mu.Unlock()
		return reason, true
	}
	current := scope.Parse(c.savedScope)
	if c.solver != nil {
		current = c.solver.scopeSnapshot()
	}
	if current.IsEmpty() || !current.SubsetOf(s.scopeSnapshot()) {
		jobs.mu.Unlock()
		return "child scope no longer authorized", true
	}
	grant, reason := s.reserveChildSteps(a.ExtraSteps)
	if reason != "" {
		jobs.mu.Unlock()
		return reason, true
	}
	if s.budget != nil {
		if err := s.budget.Reserve(c.id, grant); err != nil {
			s.restoreReservation(grant)
			jobs.mu.Unlock()
			return err.Error(), true
		}
	}
	if c.solver == nil {
		meta := s.meta
		meta.Description = c.task
		c.solver = New(Options{ModelCallTimeout: s.modelTimeout, ModelSpec: c.actor, Model: s.Model, Client: s.client, Meta: meta, Scope: current, BB: s.bb, TL: s.tl,
			AllowExec: s.allowExec, PlaywrightMCP: s.playwrightMCP, InsecureTLS: s.insecureTLS, Deadline: s.childDeadline(), Tokens: s.tokens,
			LLMRecorder: s.llmRecorder, Guard: s.guard, SubmitFn: s.submitFn, Resolve: s.resolve, Complete: s.complete, ContextWindow: s.contextWindow,
			Paused: s.paused, WaitPaused: s.waitPaused, Depth: s.depth + 1, MaxDelegateDepth: s.maxDelegateDepth, MaxDelegateChildren: s.maxDelegateChildren,
			MaxDelegateSteps: s.maxDelegateSteps, SubagentRoot: s.subagentRoot, AgentID: c.id, ParentID: s.agentID,
			SessionPath: filepath.Join(c.dir, "session.jsonl"), Budget: s.budget, BudgetOwner: c.id, MissionContext: s.missionContext,
			RecordChildEvent: s.recordChildEvent, VerificationPeer: s.verificationPeer,
			Record: func(step int, kind, tool, text string) {
				s.childEvent(ChildEvent{AgentID: c.id, ParentID: s.agentID, Step: step, Kind: kind, Tool: tool, Text: text})
			}})
	}
	if err := c.solver.start(); err != nil {
		s.failStorage(err)
		s.restoreReservation(grant)
		jobs.mu.Unlock()
		return err.Error(), true
	}
	c.solver.stepLimit = c.solver.steps + c.solver.delegatedStepsTotal + grant
	c.solver.deadline = s.childDeadline()
	c.allocatedSteps += grant
	c.attempt++
	c.status = "running"
	c.running = true
	if err := c.solver.RuntimeGuide("Resume after inspecting prior evidence. Do not replay an action with unknown effects. " + a.Note); err != nil {
		s.failStorage(err)
		s.restoreReservation(grant)
		jobs.mu.Unlock()
		return err.Error(), true
	}
	if err := c.persist(); err != nil {
		s.failStorage(err)
		s.restoreReservation(grant)
		jobs.mu.Unlock()
		return err.Error(), true
	}
	jobs.slots++
	jobs.mu.Unlock()
	return s.launchAsync(c), true
}

func asyncToolDefs() []openai.Tool {
	var out []openai.Tool
	for _, d := range []struct {
		name, desc string
		params     map[string]any
	}{
		{"delegate_start", "Start a bounded independent investigation and return immediately. Completion arrives as an input event; do other work meanwhile.", obj(map[string]any{"task": strProp("self-contained task"), "max_steps": map[string]any{"type": "integer"}, "focus": strProp("optional authorized subset")}, "task", "max_steps")},
		{"delegate_status", "Read a direct child's current status without waiting.", obj(map[string]any{"agent_id": strProp("child id")}, "agent_id")},
		{"delegate_cancel", "Request cancellation of a direct child; its result arrives after cleanup.", obj(map[string]any{"agent_id": strProp("child id")}, "agent_id")},
	} {
		out = append(out, openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{Name: d.name, Description: d.desc, Parameters: d.params}})
	}
	return out
}

func (s *Solver) NeedsReconciliation() bool {
	if !s.asyncDelegate {
		return false
	}
	jobs := s.initJobs()
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	for _, job := range jobs.items {
		select {
		case <-job.done:
			if job.record.status == "needs_reconciliation" {
				return true
			}
		default:
		}
	}
	return false
}
