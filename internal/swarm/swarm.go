// Package swarm runs several executors in parallel on one mission. They share
// the mission's blackboard and traffic log and COLLABORATE — no racing, no
// sibling cancellation. Each executor searches until the termination hook says
// stop, a hard bump ceiling is hit, or the wall-clock budget ends. When a
// Platform is set, submit_flag goes through it (deduped + cooldown + a circuit
// breaker on blind guessing); a confirmed flag is recorded but does not cancel
// the others (the scheduler's poller kills the swarm on a platform-confirmed
// solve).
package swarm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/config"
	"github.com/well-lzz/chronos/internal/guard"
	"github.com/well-lzz/chronos/internal/llm"
	"github.com/well-lzz/chronos/internal/llmconfig"
	"github.com/well-lzz/chronos/internal/llmrec"
	"github.com/well-lzz/chronos/internal/mission"
	"github.com/well-lzz/chronos/internal/notify"
	"github.com/well-lzz/chronos/internal/platform"
	"github.com/well-lzz/chronos/internal/scope"
	"github.com/well-lzz/chronos/internal/solver"
	"github.com/well-lzz/chronos/internal/termination"
	"github.com/well-lzz/chronos/internal/traffic"
)

// ActEvent is one observable step of an executor (for the web console). Seq is
// a per-swarm monotonic cursor (covers operator messages too, which have no
// solver step), so the chat/stream orders human and agent messages together.
type ActEvent struct {
	Seq         int64                `json:"seq"`
	TS          float64              `json:"ts"`
	Step        int                  `json:"step"`
	Turn        int                  `json:"turn,omitempty"`
	Kind        string               `json:"kind"`
	MessageType string               `json:"message_type,omitempty"`
	Subtype     string               `json:"subtype,omitempty"`
	MessageID   string               `json:"message_id,omitempty"`
	ToolCallID  string               `json:"tool_call_id,omitempty"`
	Outcome     string               `json:"outcome,omitempty"`
	Final       bool                 `json:"final,omitempty"`
	Tokens      int                  `json:"tokens,omitempty"`
	Tool        string               `json:"tool,omitempty"`
	Text        string               `json:"text"`
	Context     *solver.ContextEvent `json:"context,omitempty"`
	AgentID     string               `json:"agent_id,omitempty"`
	ParentID    string               `json:"parent_id,omitempty"`
	ParentModel string               `json:"parent_model,omitempty"`
}

const activityRing = 300

var submissionCooldowns = []time.Duration{0, 30 * time.Second, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute}

const submitLockWrongLimit = 4

func providerBackoff(failures int) time.Duration {
	if failures < 1 {
		return 0
	}
	if failures > 5 {
		failures = 5
	}
	return time.Duration(1<<uint(failures-1)) * time.Second
}

func (s *Swarm) sessionPath(spec string) string {
	if s.BB == nil || s.BB.Path == "" {
		return ""
	}
	root := filepath.Dir(filepath.Dir(s.BB.Path))
	mission := strings.TrimSuffix(filepath.Base(s.BB.Path), filepath.Ext(s.BB.Path))
	sum := sha256.Sum256([]byte(spec))
	return filepath.Join(root, "sessions", mission, hex.EncodeToString(sum[:8])+".jsonl")
}

func (s *Swarm) handoffSessionPath(spec string, generation int) string {
	path := s.sessionPath(spec)
	if generation <= 0 || path == "" {
		return path
	}
	return strings.TrimSuffix(path, ".jsonl") + fmt.Sprintf(".handoff-%d.jsonl", generation)
}

func (s *Swarm) subagentRoot() string {
	if s.BB == nil || s.BB.Path == "" {
		return ""
	}
	root := filepath.Dir(filepath.Dir(s.BB.Path))
	mission := strings.TrimSuffix(filepath.Base(s.BB.Path), filepath.Ext(s.BB.Path))
	return filepath.Join(root, "subagents", mission)
}

type Swarm struct {
	Meta              mission.Meta
	Scope             scope.Scope
	ModelSpecs        []string
	Settings          config.Settings
	BB                *blackboard.Blackboard
	TL                *traffic.Log
	Platform          platform.Platform            // nil = engagement (no oracle, record-only)
	LLM               *llmconfig.Store             // operator-editable model profile; nil = env/flag only
	ModelProfiles     map[string]llmconfig.Profile // optional per-executor operator profiles
	UseConsoleProfile bool                         // implicit selection follows live console edits
	GlobalRemaining   func() float64               // negative = no global budget

	Tokens int64 // shared, mission-wide

	mu         sync.Mutex
	solvers    map[string]*solver.Solver
	activity   map[string][]ActEvent
	status     map[string]string
	lastErr    map[string]string // last surfaced error per spec (dedup consecutive)
	toolCalls  map[string]int
	toolByName map[string]map[string]int
	runCtx     context.Context
	paused     atomic.Bool
	pauseMu    sync.Mutex
	resumeCh   chan struct{}
	evSeq      int64 // monotonic activity cursor (atomic)

	// submit state (shared across executors)
	subMu        sync.Mutex
	submitted    map[string]bool
	submitCount  map[string]int
	lastSubmit   map[string]time.Time
	confirmed    map[string]bool
	wrongTotal   int
	submitLocked bool
}

func (s *Swarm) init() {
	// Keep activity cursors increasing across a process restart while remaining
	// exactly representable as JavaScript numbers in /api/activity's `after`.
	atomic.StoreInt64(&s.evSeq, time.Now().UnixMilli()*1000)
	s.mu.Lock()
	s.solvers = map[string]*solver.Solver{}
	s.activity = map[string][]ActEvent{}
	s.status = map[string]string{}
	if s.Paused() {
		for _, spec := range s.ModelSpecs {
			s.status[spec] = "paused"
		}
	}
	s.lastErr = map[string]string{}
	s.toolCalls = map[string]int{}
	s.toolByName = map[string]map[string]int{}
	if err := s.loadActivityLocked(); err != nil {
		log.Printf("load activity for %s: %v", s.Meta.Name, err)
	}
	s.mu.Unlock()
	s.subMu.Lock()
	s.submitted = map[string]bool{}
	s.submitCount = map[string]int{}
	s.lastSubmit = map[string]time.Time{}
	s.confirmed = map[string]bool{}
	s.subMu.Unlock()
}

// Run launches one cooperative executor per distinct model spec and waits for
// all of them. There is no winner selection or sibling cancellation here.
func (s *Swarm) Run(ctx context.Context) {
	s.ModelSpecs = distinctSpecs(s.ModelSpecs)
	s.init()
	s.mu.Lock()
	s.runCtx = ctx
	s.mu.Unlock()
	digestCtx, stopDigest := context.WithCancel(ctx)
	digestDone := make(chan struct{})
	go func() { defer close(digestDone); s.digestLoop(digestCtx) }()
	deadline, _ := ctx.Deadline()
	var wg sync.WaitGroup

	for _, spec := range s.ModelSpecs {
		sp := spec
		// The selection source is fixed by the operator at startup. Only an
		// implicit console-driven slot follows live console model updates.
		resolve := func() (*openai.Client, string, error) { return s.resolveModel(sp) }
		var complete func(context.Context, openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error)
		if profile, ok := s.ModelProfiles[sp]; ok && len(profile.Fallbacks) > 0 {
			complete = func(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
				return llm.SharedRegistry(s.Settings.StateDir).CompleteExact(ctx, s.Settings, profile, req,
					llmrec.Shared(s.Settings.StateDir, s.Settings.LLMRecord), s.BB.MissionID, sp)
			}
		} else if s.UseConsoleProfile && s.LLM != nil && len(s.LLM.Get().Fallbacks) > 0 {
			complete = func(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
				return llm.SharedRegistry(s.Settings.StateDir).Complete(ctx, s.Settings, s.LLM.Get(), req,
					llmrec.Shared(s.Settings.StateDir, s.Settings.LLMRecord), s.BB.MissionID, sp)
			}
		}
		contextWindow := func(model string) int {
			if p, ok := s.ModelProfiles[sp]; ok && p.Model == model && p.ContextWindowTokens > 0 {
				return p.ContextWindowTokens
			}
			if s.UseConsoleProfile && s.LLM != nil && s.LLM.Active() {
				p := s.LLM.Get()
				if p.Model == model && p.ContextWindowTokens > 0 {
					return p.ContextWindowTokens
				}
			}
			if s.Settings.ContextWindowTokens > 0 {
				return s.Settings.ContextWindowTokens
			}
			return llmconfig.KnownContextWindowTokens(model)
		}
		var submitFn solver.SubmitFn
		if s.Platform != nil {
			submitFn = func(fl string) (string, bool, bool) { return s.trySubmit(fl, sp) }
		}
		previous := s.loadRuntime(spec)
		generation := previous.HandoffGeneration
		sv := solver.New(solver.Options{
			ModelSpec: spec, Meta: s.Meta, Scope: s.Scope,
			BB: s.BB, TL: s.TL, AllowExec: s.Settings.AllowLocalExec, PlaywrightMCP: s.Settings.PlaywrightMCP, InsecureTLS: s.Settings.TargetInsecureTLS, Deadline: deadline,
			Tokens: &s.Tokens, LLMRecorder: llmrec.Shared(s.Settings.StateDir, s.Settings.LLMRecord), Guard: guard.Shared(s.Settings.StateDir, s.Settings.ToolRulesFile), Notify: func(m string) { log.Printf("[%s/%s] %s", s.Meta.Name, sp, m) },
			SubmitFn: submitFn, Record: s.recorder(spec), RecordEvent: s.loopRecorder(spec), RecordContext: s.contextRecorder(spec),
			ContextWindow: contextWindow, Resolve: resolve, Complete: complete, Paused: s.paused.Load,
			WaitPaused: func(ctx context.Context) error { return s.waitIfPaused(ctx, sp) }, SessionPath: s.handoffSessionPath(spec, generation),
			MaxDelegateDepth: s.Settings.MaxDelegateDepth, MaxDelegateChildren: s.Settings.MaxDelegateChildren,
			MaxDelegateSteps:    s.Settings.MaxDelegateSteps,
			MinRemainingToSpawn: time.Duration(s.Settings.MinRemainingToSpawnS) * time.Second,
			GlobalRemaining:     s.GlobalRemaining, SubagentRoot: s.subagentRoot(),
			RecordChildEvent: s.childRecorder(spec),
			VerificationPeer: s.routeVerification,
		})
		s.mu.Lock()
		s.solvers[spec] = sv
		s.mu.Unlock()
		s.loopRecorder(spec)(solver.LoopEvent{Kind: "session_init", MessageType: "system", Subtype: "init",
			Text: "Executor session ready: " + spec})
		if previous.Version == 1 {
			sv.Guide("This executor resumed after a process restart. Revalidate target state before relying on prior actions or traffic; continue from the shared blackboard and preserved transcript.")
			s.emit(spec, previous.Steps, "resume", "process_restart", "Restored executor session and cycle counters")
		}
		wg.Add(1)
		go func(spec string, sv *solver.Solver) {
			defer wg.Done()
			s.runExecutor(ctx, sv, spec)
		}(spec, sv)
	}
	wg.Wait()
	stopDigest()
	<-digestDone
	if ctx.Err() == nil && s.Settings.WebhookURL != "" && s.BB != nil {
		n, _ := notify.Shared(s.Settings.StateDir, s.Settings.WebhookURL)
		if err := n.NotifyOnce(ctx, "complete:"+s.BB.MissionID, notify.Event{Kind: "mission_complete", Mission: s.BB.MissionID}); err != nil {
			log.Printf("completion webhook failed for %s: %v", s.BB.MissionID, err)
		}
	}
}

func (s *Swarm) digestLoop(ctx context.Context) {
	if s.BB == nil || len(s.ModelSpecs) == 0 {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	passes := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if s.waitIfPaused(ctx, "") != nil {
			return
		}
		passes++
		for _, group := range s.BB.PlanCold(passes%20 == 0) {
			if ctx.Err() != nil {
				return
			}
			if s.waitIfPaused(ctx, "") != nil {
				return
			}
			client, model, err := s.resolveModel(s.ModelSpecs[0])
			if err != nil {
				break
			}
			var members strings.Builder
			for _, member := range group.Members {
				fmt.Fprintf(&members, "[%s] %s\n", member.ID, member.Text)
			}
			request := openai.ChatCompletionRequest{Model: model, Messages: []openai.ChatCompletionMessage{
				{Role: openai.ChatMessageRoleSystem, Content: "Summarize these completed or inactive blackboard records faithfully in at most 180 words. Preserve concrete outcomes and uncertainties. Do not infer new findings. The original records remain available by ID."},
				{Role: openai.ChatMessageRoleUser, Content: members.String()},
			}}
			if s.waitIfPaused(ctx, "") != nil {
				return
			}
			callCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
			response, err := llmrec.Shared(s.Settings.StateDir, s.Settings.LLMRecord).Call(callCtx, s.BB.MissionID,
				"cold-digest", request, client.CreateChatCompletion)
			cancel()
			if s.waitIfPaused(ctx, "") != nil {
				return
			}
			if err != nil || len(response.Choices) == 0 {
				continue
			}
			used := response.Usage.TotalTokens
			if used == 0 {
				used = response.Usage.PromptTokens + response.Usage.CompletionTokens
			}
			atomic.AddInt64(&s.Tokens, int64(used))
			if applied, err := s.BB.CommitCold(group, response.Choices[0].Message.Content); err == nil && applied {
				s.emit(s.ModelSpecs[0], 0, "context", "cold_digest", fmt.Sprintf("Cold digest saved for %d records", len(group.Members)))
			}
		}
	}
}

func distinctSpecs(specs []string) []string {
	seen := make(map[string]bool, len(specs))
	out := make([]string, 0, len(specs))
	for _, spec := range specs {
		spec = strings.TrimSpace(spec)
		if spec != "" && !seen[spec] {
			seen[spec] = true
			out = append(out, spec)
		}
	}
	return out
}

func (s *Swarm) resolveModel(spec string) (*openai.Client, string, error) {
	if p, ok := s.ModelProfiles[spec]; ok {
		return llm.FromExactProfileWithSettings(p, s.Settings)
	}
	if s.UseConsoleProfile && s.LLM != nil && s.LLM.Active() {
		return llm.FromProfileWithSettings(s.LLM.Get(), s.Settings)
	}
	return llm.Resolve(spec, s.Settings)
}

func (s *Swarm) runExecutor(ctx context.Context, sv *solver.Solver, spec string) {
	defer func() {
		sv.CloseBrowser()
		if err := s.BB.ReleaseOwner(spec); err != nil {
			log.Printf("[%s/%s] release claims: %v", s.Meta.Name, spec, err)
		}
		if err := s.BB.ReassignVerificationOwner(spec); err != nil {
			log.Printf("[%s/%s] release verification work: %v", s.Meta.Name, spec, err)
		}
		for _, goal := range s.BB.OpenSubgoals() {
			if goal.Kind == "verification" && goal.AssignedTo == "" {
				if next := s.routeVerification(spec, goal.FindingID); next != "" && next != spec {
					_, _ = s.BB.AssignVerification(goal.FindingID, next)
				}
			}
		}
	}()
	maxBumps := s.Settings.MaxBumps
	if maxBumps <= 0 {
		maxBumps = 20
	}
	retry := time.Duration(s.Settings.SolverRetryDelayS * float64(time.Second))
	previous := s.loadRuntime(spec)
	s.mu.Lock()
	s.toolCalls[spec] = previous.ToolCalls
	s.toolByName[spec] = map[string]int{}
	for name, count := range previous.Tools {
		s.toolByName[spec][name] = count
	}
	s.mu.Unlock()
	started := previous.StartedAt
	if started.IsZero() {
		started = time.Now()
	}
	bumps, cycles := previous.Bumps, previous.Cycles
	quietCycles := previous.QuietCycles
	providerFailures := previous.ProviderFails
	lastSteps := previous.Steps
	handoffGeneration := previous.HandoffGeneration
	checkpointRuntime := func(status string) {
		s.mu.Lock()
		toolCalls, tools := s.toolStatsLocked(spec)
		s.mu.Unlock()
		if err := s.saveRuntime(spec, executorRuntime{Status: status, StartedAt: started, Cycles: cycles,
			Bumps: bumps, QuietCycles: quietCycles, ProviderFails: providerFailures, Steps: lastSteps,
			ToolCalls: toolCalls, Tools: tools, HandoffGeneration: handoffGeneration}); err != nil {
			log.Printf("[%s/%s] executor checkpoint failed: %v", s.Meta.Name, spec, err)
		}
	}
	finalStatus := "interrupted"
	defer func() { checkpointRuntime(finalStatus) }()
	if s.Paused() {
		s.setStatus(spec, "paused")
	} else {
		s.setStatus(spec, "running")
	}
	// Preserve the platform's progress-based stall ladder alongside runtime recovery.
	benchmark := s.Platform != nil
	stall := termination.NewStallPolicy(termination.StallConfig{})

	for {
		if ctx.Err() != nil {
			s.setStatus(spec, "cancelled")
			return
		}
		if err := s.waitIfPaused(ctx, spec); err != nil {
			s.setStatus(spec, "cancelled")
			return
		}
		markerBefore := s.BB.ProgressMarker()
		res := sv.RunUntilDoneOrGaveUp(ctx)
		if s.waitIfPaused(ctx, spec) != nil {
			s.setStatus(spec, "cancelled")
			return
		}
		if s.Settings.WebhookURL != "" {
			n, _ := notify.Shared(s.Settings.StateDir, s.Settings.WebhookURL)
			for _, finding := range s.BB.FindingsSorted() {
				if finding.Severity != "critical" && finding.Severity != "high" {
					continue
				}
				if err := n.NotifyOnce(ctx, "finding:"+s.BB.MissionID+":"+finding.ID, notify.Event{Kind: "high_severity_finding",
					Mission: s.BB.MissionID, FindingID: finding.ID, Title: finding.Title, Severity: finding.Severity}); err != nil && ctx.Err() == nil {
					log.Printf("finding webhook failed for %s: %v", finding.ID, err)
				}
			}
		}
		cycles++
		lastSteps = res.StepCount
		checkpointRuntime("running")
		if s.Platform == nil && res.Status == solver.StatusGaveUp && sv.Saturated() && handoffGeneration < min(2, s.Settings.MaxHandoffs) && len(s.BB.OpenSubgoals()) > 0 && (sv.Deadline().IsZero() || time.Until(sv.Deadline()) > 2*time.Minute) {
			if err := s.BB.ReleaseOwner(spec); err != nil {
				log.Printf("[%s/%s] handoff claim release failed: %v", s.Meta.Name, spec, err)
			}
			handoffGeneration++
			s.emit(spec, res.StepCount, "handoff", "context_saturation", fmt.Sprintf("Clean executor generation %d takes over open sub-goals using shared blackboard", handoffGeneration))
			old := sv
			sv = sv.FreshHandoff(s.handoffSessionPath(spec, handoffGeneration))
			old.CloseBrowser()
			s.mu.Lock()
			s.solvers[spec] = sv
			s.mu.Unlock()
			checkpointRuntime("running")
			continue
		}

		switch res.Status {
		case solver.StatusFlagFound:
			finalStatus = "flag_found"
			log.Printf("[%s/%s] flag found: %s", s.Meta.Name, spec, res.Flag)
			s.setStatus(spec, "flag_found")
			return // recorded; the platform poller owns mission completion
		case solver.StatusCancelled:
			s.setStatus(spec, "cancelled")
			return
		case solver.StatusConfigError:
			// The model isn't configured/authorized yet. Surface it, mark the
			// executor as waiting, and keep polling — the moment the operator saves
			// a working model in the console, the next cycle picks it up. Does not
			// count against the retry ceiling.
			s.emitErrorOnce(spec, res.StepCount, res.Findings+"  → 打开控制台右上角「模型设置」填写后自动恢复")
			s.setStatus(spec, "needs-config")
			select {
			case <-ctx.Done():
				s.setStatus(spec, "cancelled")
				return
			case <-time.After(3 * time.Second):
			}
			continue
		case solver.StatusRetryableError:
			// A provider outage is not evidence that the exploration strategy
			// failed. Keep the conversation intact and retry within the mission
			// deadline, with bounded exponential backoff.
			providerFailures++
			checkpointRuntime("waiting-provider")
			s.emitErrorOnce(spec, res.StepCount, res.Findings)
			s.setStatus(spec, "waiting-provider")
			select {
			case <-ctx.Done():
				s.setStatus(spec, "cancelled")
				return
			case <-time.After(providerBackoff(providerFailures)):
			}
			continue
		}
		providerFailures = 0
		if s.BB.ProgressMarker() > markerBefore {
			quietCycles = 0
		} else {
			quietCycles++
		}
		remaining := -1.0
		if !sv.Deadline().IsZero() {
			remaining = time.Until(sv.Deadline()).Seconds()
		}

		st := termination.CycleState{
			StepCount: res.StepCount, BumpCount: bumps, Cycles: cycles,
			ProgressMarker: s.BB.ProgressMarker(), PrevProgressMark: markerBefore,
			ElapsedSeconds: time.Since(started).Seconds(),
			QuietCycles:    quietCycles, IsEngagement: s.Platform == nil, RemainingSeconds: remaining,
			QuietCyclesRequired: s.Settings.EngagementQuietCycles,
		}
		if termination.ShouldTerminate(s.BB, st) {
			finalStatus = "finished"
			log.Printf("[%s/%s] termination hook requested stop", s.Meta.Name, spec)
			threshold := s.Settings.EngagementQuietCycles
			if threshold < 1 {
				threshold = 3
			}
			s.emit(spec, res.StepCount, "terminate", "coverage", fmt.Sprintf("All explicit goals and verifications resolved; discovery stayed quiet for %d cycles.", threshold))
			s.setStatus(spec, "finished")
			return
		}

		switch d := stall.Observe(st.MadeProgress()); {
		case d.Terminate && benchmark:
			finalStatus = "finished"
			log.Printf("[%s/%s] stalled across every pivot with no new progress — terminating to free the slot", s.Meta.Name, spec)
			s.setStatus(spec, "finished")
			return
		case d.Pivot:
			log.Printf("[%s/%s] no progress — pivoting to a different approach", s.Meta.Name, spec)
			sv.Pivot(d.Directive)
		}

		if res.Status == solver.StatusError && res.Findings != "" {
			// Surface the failure so the console shows WHY nothing is coming back
			// (bad key, unreachable endpoint, …) instead of "waiting for activity".
			s.emitErrorOnce(spec, res.StepCount, res.Findings)
		} else {
			s.mu.Lock()
			delete(s.lastErr, spec)
			s.mu.Unlock()
		}

		if res.Status == solver.StatusGaveUp || res.Status == solver.StatusError {
			if s.BB.ProgressMarker() > markerBefore {
				// MAX_BUMPS is a stall ceiling, not a lifetime limit. A long
				// mission that keeps producing durable findings can use its full
				// wall-clock budget.
				bumps = 0
			} else {
				bumps++
			}
			if bumps > maxBumps {
				finalStatus = "finished"
				log.Printf("[%s/%s] max bumps (%d) reached", s.Meta.Name, spec, maxBumps)
				s.emit(spec, res.StepCount, "text", "", "Executor stopped: retry ceiling reached after repeated failures.")
				s.setStatus(spec, "finished")
				return
			}
			checkpointRuntime("running")
			if retry > 0 {
				select {
				case <-ctx.Done():
					s.setStatus(spec, "cancelled")
					return
				case <-time.After(retry):
				}
			}
			sv.Bump("Keep covering new ground; read the shared working record and build on it.")
			s.emit(spec, res.StepCount, "pivot", "stalled_cycle", "No durable progress; executor prompted to cover new ground")
			continue
		}
		s.setStatus(spec, "finished")
		finalStatus = "finished"
		return
	}
}

// routeVerification chooses another neutral executor when one is available.
// It changes only who checks the evidence, never the executor's role or tools.
func (s *Swarm) routeVerification(origin, findingID string) string {
	s.mu.Lock()
	chosen := ""
	for _, spec := range s.ModelSpecs {
		if spec == origin || strings.HasPrefix(origin, spec+"/sub:") {
			continue
		}
		status := s.status[spec]
		if status != "finished" && status != "cancelled" && status != "flag_found" {
			chosen = spec
			break
		}
	}
	if chosen == "" {
		for _, spec := range s.ModelSpecs {
			if s.status[spec] != "finished" && s.status[spec] != "cancelled" {
				chosen = spec
				break
			}
		}
	}
	peer := s.solvers[chosen]
	s.mu.Unlock()
	if peer != nil && chosen != origin {
		peer.Guide("Independently assess candidate finding " + findingID + ". Read its evidence and original traffic, perform your own check, then call verify_finding with fresh traffic evidence_refs. The verification goal is assigned to you.")
		s.emit(chosen, 0, "verify_task", findingID, "Independent verification assigned: "+findingID)
	}
	return chosen
}

// trySubmit forwards a flag to the platform with dedup, escalating cooldown, and
// a circuit breaker on repeated wrong guesses. Returns (display, confirmed, complete).
func (s *Swarm) trySubmit(flag, spec string) (string, bool, bool) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	norm := strings.TrimSpace(flag)
	if s.confirmed[norm] {
		return "ALREADY SOLVED — flag already confirmed: " + norm, true, true
	}
	if s.submitLocked {
		return fmt.Sprintf("SUBMIT LOCKED — %d incorrect submissions on this task. Stop guessing; only submit with hard evidence.", s.wrongTotal), false, false
	}
	if s.submitted[norm] {
		return "INCORRECT — already tried this exact flag.", false, false
	}
	wrong := s.submitCount[spec]
	idx := wrong
	if idx >= len(submissionCooldowns) {
		idx = len(submissionCooldowns) - 1
	}
	if cd := submissionCooldowns[idx]; cd > 0 {
		if elapsed := time.Since(s.lastSubmit[spec]); elapsed < cd {
			return fmt.Sprintf("COOLDOWN — wait %ds before submitting again (%d incorrect so far).", int((cd - elapsed).Seconds()), wrong), false, false
		}
	}
	s.submitted[norm] = true
	res, err := s.Platform.SubmitFlag(s.Meta.Name, flag)
	if err != nil {
		return "submit_flag error: " + err.Error(), false, false
	}
	switch res.Status {
	case "already_solved":
		s.confirmed[norm] = true
		return res.Display, true, true
	case "correct":
		if res.ChallengeComplete || s.Meta.FlagCount <= 1 {
			s.confirmed[norm] = true
			return res.Display, true, true
		}
		return res.Display + "\nPARTIAL CREDIT — more flags remain; work a different surface.", true, false
	default:
		s.submitCount[spec] = wrong + 1
		s.lastSubmit[spec] = time.Now()
		s.wrongTotal++
		if s.wrongTotal >= submitLockWrongLimit {
			s.submitLocked = true
			log.Printf("[%s] submit circuit breaker tripped after %d wrong", s.Meta.Name, s.wrongTotal)
		}
		return res.Display, false, false
	}
}

// --- live observation (web console) ---

func (s *Swarm) recorder(spec string) func(step int, kind, tool, text string) {
	return func(step int, kind, tool, text string) {
		s.emit(spec, step, kind, tool, text)
	}
}

func (s *Swarm) loopRecorder(spec string) func(solver.LoopEvent) {
	return func(event solver.LoopEvent) {
		value := event.Text
		if len(value) > 4000 {
			value = value[:4000]
		}
		seq := atomic.AddInt64(&s.evSeq, 1)
		s.mu.Lock()
		s.appendActivityLocked(spec, ActEvent{Seq: seq, TS: float64(time.Now().UnixNano()) / 1e9,
			Step: event.Step, Turn: event.Turn, Kind: event.Kind, MessageType: event.MessageType,
			Subtype: event.Subtype, MessageID: event.MessageID, ToolCallID: event.ToolCallID,
			Outcome: event.Outcome, Final: event.Final, Tokens: event.Tokens,
			Tool: event.Tool, Text: value})
		s.mu.Unlock()
	}
}

func (s *Swarm) contextRecorder(spec string) func(solver.ContextEvent) {
	return func(event solver.ContextEvent) {
		text := fmt.Sprintf("%s: %d → %d chars, dropped %d cycles, clipped %d messages; kept %d mandatory + %d quota findings, %d omitted (revision %d)",
			event.Phase, event.BeforeChars, event.AfterChars, event.DroppedCycles, event.ClippedMessages,
			event.Summary.MandatoryFindings, event.Summary.QuotaFindings, event.Summary.OmittedFindings, event.Summary.Revision)
		if event.Phase == "reinject" {
			text = fmt.Sprintf("blackboard %s: kept %d mandatory + %d quota findings; %d findings, %d facts, %d goals summarized (revision %d, %d/%d chars)",
				event.Reason, event.Summary.MandatoryFindings, event.Summary.QuotaFindings,
				event.Summary.OmittedFindings, event.Summary.OmittedFacts, event.Summary.OmittedGoals,
				event.Summary.Revision, event.Summary.ActualChars, event.BudgetChars)
		} else {
			if len(event.Dropped) > 0 {
				text += "; dropped: " + briefContextIDs(event.Dropped)
			}
			if len(event.Clipped) > 0 {
				text += "; clipped: " + briefContextIDs(event.Clipped)
			}
		}
		s.emitContext(spec, event, text)
	}
}

func briefContextIDs(ids []string) string {
	const maxShown = 12
	if len(ids) <= maxShown {
		return strings.Join(ids, ", ")
	}
	return fmt.Sprintf("%s (+%d more)", strings.Join(ids[:maxShown], ", "), len(ids)-maxShown)
}

func (s *Swarm) emitContext(spec string, event solver.ContextEvent, text string) {
	seq := atomic.AddInt64(&s.evSeq, 1)
	s.mu.Lock()
	subtype := event.Phase
	if event.Phase == "compact" {
		subtype = "compact_boundary"
	}
	s.appendActivityLocked(spec, ActEvent{Seq: seq, TS: float64(time.Now().UnixNano()) / 1e9,
		Step: 0, Kind: "context", MessageType: "system", Subtype: subtype, Text: text, Context: &event})
	s.mu.Unlock()
}

func (s *Swarm) childRecorder(rootSpec string) func(solver.ChildEvent) {
	return func(event solver.ChildEvent) {
		seq := atomic.AddInt64(&s.evSeq, 1)
		value := event.Text
		if len(value) > 4000 {
			value = value[:4000]
		}
		linkedAgent, linkedParent := event.AgentID, event.ParentID
		if strings.HasPrefix(event.Kind, "delegate_") {
			linkedAgent, linkedParent = event.Tool, event.AgentID
		}
		s.mu.Lock()
		s.appendActivityLocked(event.AgentID, ActEvent{Seq: seq, TS: float64(time.Now().UnixNano()) / 1e9,
			Step: event.Step, Kind: event.Kind,
			Tool: event.Tool, Text: value, Context: event.Context, AgentID: linkedAgent,
			ParentID: linkedParent, ParentModel: rootSpec})
		s.mu.Unlock()
	}
}

// emit appends an event to an executor's ring with a monotonic Seq.
func (s *Swarm) emit(spec string, step int, kind, tool, text string) {
	if len(text) > 4000 {
		text = text[:4000]
	}
	seq := atomic.AddInt64(&s.evSeq, 1)
	s.mu.Lock()
	if kind == "tool_call" && tool != "" {
		if s.toolCalls == nil {
			s.toolCalls = map[string]int{}
		}
		if s.toolByName[spec] == nil {
			s.toolByName[spec] = map[string]int{}
		}
		s.toolCalls[spec]++
		s.toolByName[spec][tool]++
	}
	messageType := "system"
	if kind == "operator" {
		messageType = "user"
	}
	event := ActEvent{Seq: seq, TS: float64(time.Now().UnixNano()) / 1e9,
		Step: step, Kind: kind, MessageType: messageType, Tool: tool, Text: text}
	if strings.HasPrefix(kind, "delegate_") {
		event.AgentID = tool
	}
	s.appendActivityLocked(spec, event)
	s.mu.Unlock()
}

// emitErrorOnce surfaces an executor error to the console, collapsing a run of
// identical errors (e.g. a repeating 401) into a single visible line.
func (s *Swarm) emitErrorOnce(spec string, step int, text string) {
	s.mu.Lock()
	same := s.lastErr[spec] == text
	s.lastErr[spec] = text
	s.mu.Unlock()
	if same {
		return
	}
	s.emit(spec, step, "error", "", text)
}

func (s *Swarm) setStatus(spec, st string) {
	s.mu.Lock()
	s.status[spec] = st
	s.mu.Unlock()
}

// Activity returns this executor's events with Seq > after (the chat/stream cursor).
func (s *Swarm) Activity(spec string, after int) []ActEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ActEvent
	for _, e := range s.activity[spec] {
		if e.Seq > int64(after) {
			out = append(out, e)
		}
	}
	return out
}

func (s *Swarm) toolStatsLocked(spec string) (int, map[string]int) {
	tools := map[string]int{}
	for name, count := range s.toolByName[spec] {
		tools[name] = count
	}
	return s.toolCalls[spec], tools
}

// Status returns a per-model status view for the web snapshot.
func (s *Swarm) Status() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	agents := map[string]any{}
	for _, spec := range s.ModelSpecs {
		st := s.status[spec]
		if st == "" {
			st = "pending"
		}
		toolCalls, tools := s.toolStatsLocked(spec)
		agents[spec] = map[string]any{"spec": spec, "status": st, "steps": len(s.activity[spec]), "tool_calls": toolCalls, "tools": tools}
	}
	runID := ""
	if s.BB != nil {
		runID = s.BB.MissionID
	}
	return map[string]any{"challenge": s.Meta.Name, "run_id": runID, "paused": s.paused.Load(),
		"scope": s.Scope.Describe(), "targets": s.Scope.Entries(), "agents": agents}
}

func (s *Swarm) Paused() bool { return s.paused.Load() }

// UpdateScope atomically replaces the structural egress boundary for the swarm
// and every live solver. Operator chat cannot call this method; only the
// authenticated console scope endpoint can.
func (s *Swarm) UpdateScope(next scope.Scope) {
	s.mu.Lock()
	s.Scope = next
	s.Meta.ConnectionInfo = next.Describe()
	solvers := make(map[string]*solver.Solver, len(s.solvers))
	for spec, sv := range s.solvers {
		solvers[spec] = sv
	}
	s.mu.Unlock()
	for spec, sv := range solvers {
		sv.SetScope(next)
		s.emit(spec, 0, "status", "", "Scope updated by operator: "+next.Describe())
	}
}

// CurrentScope returns the immutable current boundary snapshot.
func (s *Swarm) CurrentScope() scope.Scope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Scope
}

// Chat records a human turn in the executor's own session. The next model turn
// reads it alongside the same tool history and working record it uses to act.
func (s *Swarm) Chat(spec, message string) bool {
	message = strings.TrimSpace(message)
	if message == "" {
		return false
	}
	if len(message) > 4000 {
		message = message[:4000]
	}
	s.mu.Lock()
	sv := s.solvers[spec]
	status := s.status[spec]
	ctx := s.runCtx
	s.mu.Unlock()
	if sv == nil || ctx == nil || ctx.Err() != nil || status == "finished" || status == "cancelled" || status == "flag_found" {
		return false
	}
	s.emit(spec, 0, "operator", "", message)
	sv.Guide(message)
	if s.paused.Load() {
		s.emit(spec, 0, "status", "", "消息已进入执行器会话；恢复执行后，Agent 会在下一轮处理。")
	} else {
		s.emit(spec, 0, "status", "", "消息已进入执行器会话；Agent 会在下一轮回应并继续执行。")
	}
	return true
}

// Bump injects operator guidance into one running executor and records it in the
// activity stream so the console chat shows the human's message in order.
func (s *Swarm) Bump(spec, message string) bool {
	s.mu.Lock()
	sv := s.solvers[spec]
	s.mu.Unlock()
	if sv == nil {
		return false
	}
	s.emit(spec, 0, "operator", "", message)
	sv.Bump("[Operator guidance] " + message)
	return true
}

// TotalTokens returns the mission-wide token count.
func (s *Swarm) TotalTokens() int { return int(atomic.LoadInt64(&s.Tokens)) }
