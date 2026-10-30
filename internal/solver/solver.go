// Package solver is one headless executor: a model + the neutral tool surface,
// driving a search over an authorized scope. It records Facts/Findings on the
// shared blackboard and cites real captured traffic as evidence. No sandbox, no
// role split — reasoning plus http/tcp/python and the blackboard tools.
package solver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/browsermcp"
	"github.com/well-lzz/chronos/internal/guard"
	"github.com/well-lzz/chronos/internal/llmrec"
	"github.com/well-lzz/chronos/internal/mission"
	"github.com/well-lzz/chronos/internal/prompts"
	"github.com/well-lzz/chronos/internal/scope"
	"github.com/well-lzz/chronos/internal/traffic"
)

// Status values for a cycle result.
const (
	StatusGaveUp         = "gave_up"
	StatusError          = "error"
	StatusRetryableError = "retryable_error" // provider/network outage; does not consume exploration bumps
	StatusConfigError    = "config_error"    // model misconfigured/unauthorized — recoverable once the operator fixes it
	StatusCancelled      = "cancelled"
	StatusFlagFound      = "flag_found"
	StatusStepBudget     = "step_budget"
)

const maxToolCallsPerCycle = 40

// Result is one RunUntilDoneOrGaveUp outcome.
type Result struct {
	Status     string
	Findings   string
	StepCount  int
	Flag       string
	Tokens     int
	StopReason string
}

// SubmitFn submits a flag through the platform (dedup/cooldown handled by the
// caller). Returns the display text, whether it was confirmed, and whether that
// confirm completed the whole challenge.
type SubmitFn func(flag string) (display string, confirmed, complete bool)

// Options configures a Solver.
type Options struct {
	ModelSpec     string
	Model         string
	Client        *openai.Client
	Meta          mission.Meta
	Scope         scope.Scope
	BB            *blackboard.Blackboard
	TL            *traffic.Log
	AllowExec     bool
	PlaywrightMCP bool
	InsecureTLS   bool
	Deadline      time.Time
	Tokens        *int64
	LLMRecorder   *llmrec.Recorder
	Complete      func(context.Context, openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error)
	Paused        func() bool
	WaitPaused    func(context.Context) error
	Guard         *guard.Manager
	Notify        func(string)
	// SubmitFn, when set, makes submit_flag go to the platform; nil = record-only.
	SubmitFn SubmitFn
	// Record, when set, receives each step for live observation (web console).
	Record        func(step int, kind, tool, text string)
	RecordEvent   func(LoopEvent)
	RecordContext func(ContextEvent)
	// ContextWindow resolves the active model's configured token window each cycle.
	ContextWindow       func(model string) int
	Depth               int
	MaxDelegateDepth    int
	MaxDelegateChildren int
	MaxDelegateSteps    int
	MinRemainingToSpawn time.Duration
	GlobalRemaining     func() float64
	SubagentRoot        string
	AgentID             string
	ParentID            string
	StepLimit           int
	RecordChildEvent    func(ChildEvent)
	VerificationPeer    func(origin, findingID string) string
	// SessionPath enables append-only transcript recovery for this executor.
	SessionPath string
	// Resolve, when set, supplies the client+model at the start of each cycle so
	// an operator who fixes the LLM config mid-run is picked up without a restart.
	// Takes precedence over Client.
	Resolve func() (*openai.Client, string, error)
}

// Solver is one executor bound to a model and a shared mission state.
type Solver struct {
	ModelSpec string
	Model     string
	client    *openai.Client

	meta          mission.Meta
	scope         scope.Scope
	bb            *blackboard.Blackboard
	tl            *traffic.Log
	allowExec     bool
	playwrightMCP bool
	browser       *browsermcp.Client
	browserMu     sync.Mutex
	browserURL    string
	browserWarned bool
	insecureTLS   bool
	deadline      time.Time

	messages            []openai.ChatCompletionMessage
	pendingUser         []openai.ChatCompletionMessage // operator/peer input, inserted only before a model request
	resumeCycle         bool                           // retry a failed model request without duplicating the cycle prompt
	started             bool
	steps               int
	turns               int
	tokens              *int64 // shared token counter (mission-wide)
	llmRecorder         *llmrec.Recorder
	complete            func(context.Context, openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error)
	paused              func() bool
	waitPaused          func(context.Context) error
	guard               *guard.Manager
	notify              func(string)
	submitFn            SubmitFn
	record              func(step int, kind, tool, text string)
	recordEvent         func(LoopEvent)
	recordContext       func(ContextEvent)
	contextWindow       func(model string) int
	lastPromptTokens    int
	lastPromptChars     int
	compactedThisCycle  bool
	compactAfterChars   int
	saturationCycles    int
	depth               int
	maxDelegateDepth    int
	maxDelegateChildren int
	maxDelegateSteps    int
	minRemainingToSpawn time.Duration
	globalRemaining     func() float64
	subagentRoot        string
	agentID             string
	parentID            string
	stepLimit           int
	recordChildEvent    func(ChildEvent)
	verificationPeer    func(origin, findingID string) string
	delegateMu          sync.Mutex
	children            map[string]*delegatedChild
	activeChildren      int
	delegateStepPool    int
	cycleRemaining      int
	cycleDelegatedSteps int
	delegatedStepsTotal int
	resolve             func() (*openai.Client, string, error)
	lastFlag            string
	flagComplete        bool
	messageMu           sync.Mutex
	scopeMu             sync.RWMutex
	modelMu             sync.RWMutex
	sessionPath         string
}

// New builds a solver from Options.
func New(o Options) *Solver {
	if o.MaxDelegateDepth < 1 {
		o.MaxDelegateDepth = 1
	}
	if o.MaxDelegateDepth > 2 {
		o.MaxDelegateDepth = 2
	}
	if o.Depth == 0 && o.MaxDelegateDepth == 2 && !o.Meta.IsKillchain() {
		o.MaxDelegateDepth = 1
	}
	if o.MaxDelegateChildren < 1 {
		o.MaxDelegateChildren = 3
	}
	if o.MaxDelegateChildren > 8 {
		o.MaxDelegateChildren = 8
	}
	if o.MaxDelegateSteps < 1 {
		o.MaxDelegateSteps = 20
	}
	if o.MaxDelegateSteps > 40 {
		o.MaxDelegateSteps = 40
	}
	return &Solver{
		ModelSpec: o.ModelSpec, Model: o.Model, client: o.Client, meta: o.Meta, scope: o.Scope,
		bb: o.BB, tl: o.TL, allowExec: o.AllowExec, playwrightMCP: o.PlaywrightMCP, insecureTLS: o.InsecureTLS, deadline: o.Deadline, tokens: o.Tokens,
		llmRecorder: o.LLMRecorder, complete: o.Complete, paused: o.Paused, waitPaused: o.WaitPaused, guard: o.Guard,
		notify: o.Notify, submitFn: o.SubmitFn, record: o.Record, recordEvent: o.RecordEvent, recordContext: o.RecordContext,
		contextWindow: o.ContextWindow, resolve: o.Resolve, sessionPath: o.SessionPath,
		depth: o.Depth, maxDelegateDepth: o.MaxDelegateDepth, maxDelegateChildren: o.MaxDelegateChildren,
		maxDelegateSteps: o.MaxDelegateSteps, minRemainingToSpawn: o.MinRemainingToSpawn,
		globalRemaining: o.GlobalRemaining, subagentRoot: o.SubagentRoot, agentID: o.AgentID,
		parentID: o.ParentID, stepLimit: o.StepLimit, recordChildEvent: o.RecordChildEvent,
		verificationPeer: o.VerificationPeer,
		children:         map[string]*delegatedChild{},
	}
}

func (s *Solver) rec(kind, tool, text string) {
	s.recAt(s.steps, kind, tool, text)
}

func (s *Solver) recAt(step int, kind, tool, text string) {
	if s.record != nil {
		s.record(step, kind, tool, text)
	}
}

func (s *Solver) loopEvent(event LoopEvent) {
	if s.recordEvent != nil {
		s.recordEvent(event)
		return
	}
	s.recAt(event.Step, event.Kind, event.Tool, event.Text)
}

func (s *Solver) start() {
	s.messageMu.Lock()
	defer s.messageMu.Unlock()
	if s.started {
		return
	}
	if s.sessionPath != "" {
		state, err := loadSession(s.sessionPath)
		if err != nil {
			log.Printf("session recovery failed (%s): %v", s.sessionPath, err)
			s.sessionPath = ""
		} else {
			s.messages, s.pendingUser, s.steps, s.turns = state.messages, state.pending, state.step, state.turn
		}
	}
	if len(s.messages) == 0 || s.messages[0].Role != openai.ChatMessageRoleSystem {
		s.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: s.systemPrompt()}}
		s.journal(sessionEvent{Type: "message", Message: &s.messages[0], Step: s.steps})
	} else {
		// Authorization and time budget are always rebuilt from live state.
		s.messages[0].Content = s.systemPrompt()
	}
	for _, id := range interruptedCalls(s.messages) {
		message := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: id,
			Content: "interrupted before completion; result unknown. Check the traffic log before retrying any action."}
		s.messages = append(s.messages, message)
		s.journal(sessionEvent{Type: "message", Message: &message, Step: s.steps})
	}
	for i := len(s.messages) - 1; i > 0; i-- {
		m := s.messages[i]
		if m.Role != openai.ChatMessageRoleUser {
			break
		}
		if strings.Contains(m.Content, "Solve this task.") || strings.Contains(m.Content, "Continue solving.") {
			s.resumeCycle = true
			break
		}
	}
	s.started = true
}

func (s *Solver) journal(event sessionEvent) {
	if err := appendSessionEvent(s.sessionPath, event); err != nil {
		log.Printf("session journal write failed (%s): %v", s.sessionPath, err)
	}
}

func (s *Solver) systemPrompt() string {
	meta := s.meta
	current := s.scopeSnapshot()
	meta.ConnectionInfo = strings.Join(current.Entries(), ", ")
	sys := prompts.BuildHeadlessPrompt(meta, 0, s.allowExec && pythonAvailable())
	s.browserMu.Lock()
	browserReady := s.browser != nil
	s.browserMu.Unlock()
	if browserReady {
		sys += "\n\n## Browser exploration\nPlaywright MCP browser tools are available. Start with browser_navigate on an in-scope HTTP(S) URL, then inspect the accessibility snapshot and interact as needed. Claim the target surface before click, typing, form entry or key actions. Browser observations are exploratory; reproduce important evidence with http_request to obtain a traffic ID for findings."
	}
	if s.agentID != "" {
		sys += "\n\n## Delegated investigation\nInvestigate only the delegated task. Record evidence and findings on the shared blackboard, then return a concise conclusion. Your tool activity is reported in your own child stream."
	}
	if current.IsEmpty() {
		sys += "\n\n## SCOPE LOCK — NO TARGET KNOWN\nThe scope is empty; all egress is refused. Report via notify and stop."
	}
	return sys
}

// Bump injects sibling insights and prepares to resume.
func (s *Solver) Bump(insights string) {
	s.queueUser(openai.ChatCompletionMessage{
		Role: openai.ChatMessageRoleUser,
		Content: "Your previous attempt did not finish. Insights from other executors (unverified — " +
			"verify before trusting):\n\n" + insights + "\n\nContinue the search.",
	})
}

// Guide injects an operator instruction into the execution loop without
// changing the structural scope lock. The next model turn can act on it only
// through tools that independently enforce the current scope.
func (s *Solver) Guide(message string) {
	s.queueUser(openai.ChatCompletionMessage{
		Role: openai.ChatMessageRoleUser,
		Content: "[Operator instruction]\n" + strings.TrimSpace(message) +
			"\n\nFollow this instruction only within the currently authorized scope. " +
			"If it targets anything outside scope, do not access it; ask the operator to update Scope in the console.",
	})
}

// SetScope atomically replaces the egress boundary used by every tool call.
// A prompt update is also queued for the model, but enforcement does not rely
// on the model honoring that text.
func (s *Solver) SetScope(next scope.Scope) {
	s.scopeMu.Lock()
	if s.agentID != "" {
		// Delegated agents may only lose authority, even if scope updates race.
		next = s.scope.Within(next)
	}
	changed := !s.scope.SameTargets(next)
	s.scope = next
	s.scopeMu.Unlock()
	// Existing HTTPS tunnels may outlive a single request. Restart the isolated
	// browser so a changed scope cannot reuse an authorized old connection.
	if changed {
		s.CloseBrowser()
	}
	s.messageMu.Lock()
	if s.started && len(s.messages) > 0 {
		s.messages[0].Content = s.systemPrompt()
	}
	s.messageMu.Unlock()
	s.queueUser(openai.ChatCompletionMessage{
		Role: openai.ChatMessageRoleUser,
		Content: "[Operator scope update]\nThe authorized scope is now exactly: " + next.Describe() +
			". Treat all earlier scope descriptions as obsolete. Structural tool guards enforce this boundary.",
	})
	s.narrowChildren(next)
}

func (s *Solver) scopeSnapshot() scope.Scope {
	s.scopeMu.RLock()
	defer s.scopeMu.RUnlock()
	return s.scope
}

func (s *Solver) Deadline() time.Time { return s.deadline }

func (s *Solver) appendMessage(message openai.ChatCompletionMessage) {
	s.messageMu.Lock()
	s.messages = append(s.messages, message)
	s.journal(sessionEvent{Type: "message", Message: &message, Step: s.steps})
	s.messageMu.Unlock()
}

func (s *Solver) queueUser(message openai.ChatCompletionMessage) {
	s.messageMu.Lock()
	s.pendingUser = append(s.pendingUser, message)
	s.journal(sessionEvent{Type: "queue", Message: &message})
	s.messageMu.Unlock()
}

// flushUser runs only at a model-request boundary, after all tool results from
// the previous assistant response have been appended. Operator input can arrive
// at any time, but must not split a tool-call/result group.
func (s *Solver) flushUser() {
	s.messageMu.Lock()
	if len(s.pendingUser) == 0 {
		s.messageMu.Unlock()
		return
	}
	count := len(s.pendingUser)
	s.messages = append(s.messages, s.pendingUser...)
	s.pendingUser = nil
	s.journal(sessionEvent{Type: "flush", Step: s.steps})
	s.messageMu.Unlock()
	s.loopEvent(LoopEvent{Kind: "input_applied", MessageType: "user", Subtype: "input_applied",
		Turn: s.turns + 1, Step: s.steps, Text: fmt.Sprintf("%d queued message(s) added to the next model request", count)})
}

func (s *Solver) messageSnapshot() []openai.ChatCompletionMessage {
	s.messageMu.Lock()
	defer s.messageMu.Unlock()
	return append([]openai.ChatCompletionMessage(nil), s.messages...)
}

// compactMessages bounds the executor's growing transcript in place, dropping
// older cycles while preserving the system prompt and tool-call pairing. The
// durable facts/findings live on the blackboard and are re-injected each cycle,
// so older raw exchanges are redundant.
func (s *Solver) compactMessages() {
	s.compactMessagesTo(s.transcriptBudgetChars(), contextMaxKeepTurns)
}

func (s *Solver) compactMessagesTo(budget, keepTurns int) bool {
	s.messageMu.Lock()
	before := s.messages
	beforeChars := totalChars(before)
	if out, changed := compactWindow(s.messages, keepTurns, budget); changed {
		s.messages = out
		s.compactedThisCycle = true
		s.compactAfterChars = totalChars(out)
		s.journal(sessionEvent{Type: "compact", Messages: out, Step: s.steps})
		s.messageMu.Unlock()
		clipped := 0
		var clippedIDs, droppedIDs []string
		cut := len(before) - len(out)
		for i := 1; i <= cut && i < len(before); i++ {
			droppedIDs = append(droppedIDs, messageLabel(i, before[i]))
		}
		for i := len(out) - 1; i > 0 && i < len(before); i-- {
			j := cut + i
			if j > 0 && j < len(before) && msgChars(out[i]) < msgChars(before[j]) {
				clipped++
				clippedIDs = append(clippedIDs, messageLabel(j, before[j]))
			}
		}
		s.contextEvent(ContextEvent{Phase: "compact", BudgetChars: budget, BeforeChars: beforeChars,
			AfterChars: totalChars(out), DroppedCycles: max(0, countCyclePrompts(before)-countCyclePrompts(out)),
			ClippedMessages: clipped, Dropped: droppedIDs, Clipped: clippedIDs})
		return true
	}
	s.messageMu.Unlock()
	return false
}

func countCyclePrompts(messages []openai.ChatCompletionMessage) int {
	n := 0
	for _, m := range messages {
		if m.Role == openai.ChatMessageRoleUser &&
			(strings.Contains(m.Content, "Solve this task.") || strings.Contains(m.Content, "Continue solving.")) {
			n++
		}
	}
	return n
}

func messageLabel(index int, message openai.ChatCompletionMessage) string {
	if message.Role == openai.ChatMessageRoleTool {
		return fmt.Sprintf("%d:tool:%s", index, message.ToolCallID)
	}
	if len(message.ToolCalls) > 0 {
		return fmt.Sprintf("%d:assistant:%d calls", index, len(message.ToolCalls))
	}
	return fmt.Sprintf("%d:%s", index, message.Role)
}

func (s *Solver) hasFullSummary() bool {
	for _, m := range s.messageSnapshot() {
		if m.Role == openai.ChatMessageRoleUser && strings.Contains(m.Content, "## working record (blackboard)") {
			return true
		}
	}
	return false
}

func (s *Solver) remainingTimeLine() string {
	if s.deadline.IsZero() {
		return "[Current time budget] No wall-clock deadline is set."
	}
	remaining := time.Until(s.deadline)
	if remaining < 0 {
		remaining = 0
	}
	return fmt.Sprintf("[Current time budget] %d seconds remain.", int(remaining.Seconds()))
}

func (s *Solver) refreshCyclePrompt(prompt string) bool {
	s.messageMu.Lock()
	defer s.messageMu.Unlock()
	for i := len(s.messages) - 1; i > 0; i-- {
		m := s.messages[i]
		if m.Role == openai.ChatMessageRoleAssistant {
			break
		}
		if m.Role != openai.ChatMessageRoleUser ||
			(!strings.Contains(m.Content, "Solve this task.") && !strings.Contains(m.Content, "Continue solving.")) {
			continue
		}
		s.messages[i].Content = prompt
		s.journal(sessionEvent{Type: "compact", Messages: s.messages, Step: s.steps})
		return true
	}
	return false
}

func (s *Solver) hasHistory() bool {
	s.messageMu.Lock()
	defer s.messageMu.Unlock()
	return len(s.messages) > 1
}

func (s *Solver) waitIfPaused(ctx context.Context) error {
	if s.waitPaused != nil {
		return s.waitPaused(ctx)
	}
	if s.paused == nil {
		return ctx.Err()
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for s.paused() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return ctx.Err()
}

// RunUntilDoneOrGaveUp runs the tool loop until the model stops calling tools
// (one cycle), the per-cycle cap is hit, or the context/deadline ends.
func (s *Solver) RunUntilDoneOrGaveUp(ctx context.Context) (out Result) {
	s.compactedThisCycle = false
	s.compactAfterChars = 0
	defer s.finishSaturationCycle()
	cycleTokens := 0
	cycleStartTurn := 0
	defer func() {
		out.Tokens = cycleTokens
		lastTurn := 0
		if s.turns > cycleStartTurn {
			lastTurn = s.turns
		}
		s.loopEvent(LoopEvent{Kind: "result", MessageType: "result", Subtype: "cycle", Turn: lastTurn,
			Step: out.StepCount, Text: out.Findings, Outcome: out.Status + ":" + out.StopReason, Tokens: cycleTokens})
	}()
	if s.waitIfPaused(ctx) != nil {
		return Result{Status: StatusCancelled, StopReason: "cancelled", StepCount: s.steps}
	}
	s.start()
	cycleStartTurn = s.turns

	// Resolve the live model config for this cycle. If the operator hasn't
	// configured a working model yet (or the key is wrong), report it as a
	// recoverable config error instead of a hard failure.
	s.modelMu.RLock()
	client, model := s.client, s.Model
	s.modelMu.RUnlock()
	if s.resolve != nil {
		var err error
		client, model, err = s.resolve()
		if err != nil {
			return Result{Status: StatusConfigError, Findings: "model not configured: " + err.Error(), StepCount: s.steps}
		}
		s.modelMu.Lock()
		s.client, s.Model = client, model
		s.modelMu.Unlock()
	}
	if client == nil {
		return Result{Status: StatusConfigError, Findings: "model not configured: open 模型设置 in the console", StepCount: s.steps}
	}
	if s.waitIfPaused(ctx) != nil {
		return Result{Status: StatusCancelled, StopReason: "cancelled", StepCount: s.steps}
	}
	if !s.resumeCycle && s.bb != nil {
		_ = s.bb.AdvanceRound()
	}

	tools := toolDefs(s.allowExec && pythonAvailable(), s.meta.HasFlags(), s.canDelegate())
	if s.playwrightMCP && !s.scopeSnapshot().IsEmpty() {
		if !browsermcp.Available() {
			if !s.browserWarned {
				s.browserWarned = true
				s.rec("tool_error", "playwright_mcp", "Node.js >=18 and Playwright MCP are required for browser exploration")
				log.Printf("[%s/%s] Playwright MCP runtime unavailable", s.meta.Name, s.ModelSpec)
			}
		} else if err := s.ensureBrowser(ctx); err != nil {
			log.Printf("[%s/%s] Playwright MCP unavailable: %v", s.meta.Name, s.ModelSpec, err)
			s.rec("tool_error", "playwright_mcp", err.Error())
		} else {
			s.browserWarned = false
			tools = append(tools, s.browserTools()...)
			s.messageMu.Lock()
			if len(s.messages) > 0 {
				s.messages[0].Content = s.systemPrompt()
			}
			s.messageMu.Unlock()
		}
	}

	// Prompt for this cycle, with the blackboard summary injected.
	prompt := "Solve this task."
	if s.hasHistory() {
		prompt = "Continue solving."
	}
	seenRevision := s.bb.Revision()
	if sum, stats := s.bb.SummaryWithBudget(s.summaryBudgetChars()); sum != "" {
		prompt = sum + "\n\n" + prompt
		s.contextEvent(ContextEvent{Phase: "reinject", Reason: "cycle", BudgetChars: s.summaryBudgetChars(), Summary: stats})
	}
	prompt = s.remainingTimeLine() + "\n" + prompt
	if !s.resumeCycle {
		s.appendMessage(openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: prompt})
	} else {
		// The request was journaled before a provider failure. Refresh the
		// same prompt in place, including current time and blackboard state.
		if !s.refreshCyclePrompt(prompt) {
			s.appendMessage(openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: prompt})
		}
	}
	s.resumeCycle = false

	calls := 0
	s.delegateMu.Lock()
	s.delegateStepPool = maxToolCallsPerCycle / 2
	s.cycleDelegatedSteps = 0
	s.cycleRemaining = maxToolCallsPerCycle
	if s.stepLimit > 0 {
		s.cycleRemaining = min(s.cycleRemaining, max(0, s.stepLimit-s.steps-s.delegatedStepsTotal))
		s.delegateStepPool = min(s.delegateStepPool, s.cycleRemaining/2)
	}
	s.delegateMu.Unlock()
	contextRecoveries := 0
	for {
		if s.waitIfPaused(ctx) != nil {
			return Result{Status: StatusCancelled, StopReason: "cancelled", StepCount: s.steps}
		}
		if s.stepLimit > 0 && s.steps+s.delegatedStepsTotal >= s.stepLimit {
			return Result{Status: StatusStepBudget, StopReason: "max_steps", StepCount: s.steps}
		}
		if err := ctx.Err(); err != nil {
			return Result{Status: StatusCancelled, StopReason: "cancelled", StepCount: s.steps}
		}
		if !s.deadline.IsZero() && time.Now().After(s.deadline) {
			return Result{Status: StatusGaveUp, Findings: "time budget reached", StopReason: "deadline", StepCount: s.steps}
		}
		s.flushUser()
		s.compactMessages() // bound the growing transcript before re-sending it
		if s.bb.Revision() > 0 && !s.hasFullSummary() {
			sum, stats := s.bb.SummaryWithBudget(s.summaryBudgetChars())
			if sum != "" {
				s.appendMessage(openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: sum})
				s.contextEvent(ContextEvent{Phase: "reinject", Reason: "summary_evicted", BudgetChars: s.summaryBudgetChars(), Summary: stats})
				seenRevision = stats.Revision
			}
		} else if revision := s.bb.Revision(); revision > seenRevision {
			delta, stats := s.bb.DeltaSince(seenRevision, s.deltaBudgetChars())
			if delta != "" {
				s.appendMessage(openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: delta})
				s.contextEvent(ContextEvent{Phase: "reinject", Reason: "blackboard_delta", BudgetChars: s.deltaBudgetChars(), Summary: stats})
			}
			seenRevision = stats.Revision
		}
		requestMessages := s.messageSnapshot()
		if s.waitIfPaused(ctx) != nil {
			return Result{Status: StatusCancelled, StopReason: "cancelled", StepCount: s.steps}
		}
		s.turns++
		turn := s.turns
		s.journal(sessionEvent{Type: "turn", Turn: turn, Step: s.steps})
		s.loopEvent(LoopEvent{Kind: "turn_start", MessageType: "system", Subtype: "turn_start", Turn: turn, Step: s.steps})
		request := openai.ChatCompletionRequest{
			Model: model, Messages: requestMessages, Tools: tools,
		}
		complete := client.CreateChatCompletion
		if s.complete != nil {
			complete = s.complete
		} else if s.llmRecorder != nil {
			missionID := s.meta.Name
			if s.bb != nil {
				missionID = s.bb.MissionID
			}
			wrapped := complete
			complete = func(callCtx context.Context, callRequest openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
				return s.llmRecorder.Call(callCtx, missionID, s.ModelSpec, callRequest, wrapped)
			}
		}
		if s.waitIfPaused(ctx) != nil {
			return Result{Status: StatusCancelled, StopReason: "cancelled", StepCount: s.steps}
		}
		resp, err := complete(ctx, request)
		if err != nil {
			s.loopEvent(LoopEvent{Kind: "turn_end", MessageType: "system", Subtype: "turn_end", Turn: turn, Step: s.steps, Outcome: "model_error"})
			if ctx.Err() != nil {
				return Result{Status: StatusCancelled, StepCount: s.steps}
			}
			if isContextLimitError(err) && contextRecoveries == 0 && s.compactMessagesTo(s.transcriptBudgetChars()/2, 1) {
				contextRecoveries++
				continue
			}
			// Auth / permission failures are a config problem the operator can fix
			// live (wrong or expired key, model not enabled) — surface them as such
			// so the executor waits and recovers instead of burning its retries.
			var apiErr *openai.APIError
			if errors.As(err, &apiErr) && (apiErr.HTTPStatusCode == 401 || apiErr.HTTPStatusCode == 403) {
				s.resumeCycle = true
				return Result{Status: StatusConfigError, Findings: fmt.Sprintf("LLM auth failed (HTTP %d): %s", apiErr.HTTPStatusCode, err.Error()), StepCount: s.steps}
			}
			// A failed request has not advanced the conversation. Reuse this exact
			// prompt on the next cycle and let the swarm back off separately from
			// its exploration retry ceiling.
			s.resumeCycle = true
			if retryableModelError(err) {
				return Result{Status: StatusRetryableError, Findings: "LLM temporarily unavailable: " + err.Error(), StepCount: s.steps}
			}
			return Result{Status: StatusError, Findings: "LLM error: " + err.Error(), StepCount: s.steps}
		}
		usedTokens := resp.Usage.TotalTokens
		if usedTokens == 0 {
			usedTokens = resp.Usage.PromptTokens + resp.Usage.CompletionTokens
		}
		if s.tokens != nil {
			atomic.AddInt64(s.tokens, int64(usedTokens))
		}
		cycleTokens += usedTokens
		if resp.Usage.PromptTokens > 0 {
			s.lastPromptChars = totalChars(requestMessages)
			s.lastPromptTokens = resp.Usage.PromptTokens
		}
		if len(resp.Choices) == 0 {
			s.loopEvent(LoopEvent{Kind: "turn_end", MessageType: "system", Subtype: "turn_end", Turn: turn, Step: s.steps, Outcome: "empty_response"})
			return Result{Status: StatusError, Findings: "empty LLM response", StepCount: s.steps}
		}
		msg := resp.Choices[0].Message
		if s.waitIfPaused(ctx) != nil {
			return Result{Status: StatusCancelled, StopReason: "cancelled", StepCount: s.steps}
		}
		s.appendMessage(msg)
		if len(msg.ToolCalls) > 0 {
			// Public assistant text can explain the current decision while tools
			// run. It is distinct from private model reasoning and tool output.
			if content := strings.TrimSpace(msg.Content); content != "" {
				s.loopEvent(LoopEvent{Kind: "progress", MessageType: "assistant", Subtype: "text", Turn: turn,
					Step: s.steps, MessageID: resp.ID, Text: content})
			} else {
				var names []string
				for _, call := range msg.ToolCalls {
					names = append(names, call.Function.Name)
				}
				s.loopEvent(LoopEvent{Kind: "progress", MessageType: "system", Subtype: "tool_pending", Turn: turn,
					Step: s.steps, Text: "正在执行 " + strings.Join(names, "、") + "；完成后会根据结果继续判断。"})
			}
		}

		if len(msg.ToolCalls) == 0 {
			// Final text, no tool calls: this cycle is done.
			if strings.TrimSpace(msg.Content) == "" {
				s.loopEvent(LoopEvent{Kind: "turn_end", MessageType: "system", Subtype: "turn_end", Turn: turn,
					Step: s.steps, Outcome: "empty_final"})
				return Result{Status: StatusError, Findings: "model returned no text or tool calls", StopReason: "empty_final", StepCount: s.steps}
			}
			s.loopEvent(LoopEvent{Kind: "assistant", MessageType: "assistant", Subtype: "final_text", Turn: turn,
				Step: s.steps, MessageID: resp.ID, Text: strings.TrimSpace(msg.Content), Final: true})
			s.loopEvent(LoopEvent{Kind: "turn_end", MessageType: "system", Subtype: "turn_end", Turn: turn,
				Step: s.steps, Outcome: "final"})
			return Result{Status: StatusGaveUp, Findings: strings.TrimSpace(msg.Content), StopReason: "done", StepCount: s.steps}
		}
		if parallelReadBatch(msg.ToolCalls) || parallelDelegateBatch(msg.ToolCalls) {
			if s.waitIfPaused(ctx) != nil {
				s.skipToolCalls(msg.ToolCalls, turn, resp.ID, "cancelled; call was not executed")
				return Result{Status: StatusCancelled, StopReason: "cancelled", StepCount: s.steps}
			}
			remaining := s.setCycleRemaining(maxToolCallsPerCycle - calls)
			steps := make([]int, len(msg.ToolCalls))
			for i, tc := range msg.ToolCalls {
				calls++
				s.steps++
				steps[i] = s.steps
				s.loopEvent(LoopEvent{Kind: "tool_call", MessageType: "assistant", Subtype: "tool_use", Turn: turn,
					Step: steps[i], MessageID: resp.ID, ToolCallID: tc.ID, Tool: tc.Function.Name, Text: tc.Function.Arguments})
			}
			results := runToolBatch(ctx, msg.ToolCalls, remaining, s.dispatch)
			for i, tc := range msg.ToolCalls {
				s.loopEvent(LoopEvent{Kind: "tool_result", MessageType: "user", Subtype: "tool_result", Turn: turn,
					Step: steps[i], MessageID: resp.ID, ToolCallID: tc.ID, Tool: tc.Function.Name, Text: results[i]})
				s.appendMessage(openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: tc.ID, Content: results[i]})
			}
		} else {
			for i, tc := range msg.ToolCalls {
				if s.waitIfPaused(ctx) != nil {
					s.skipToolCalls(msg.ToolCalls[i:], turn, resp.ID, "cancelled; call was not executed")
					return Result{Status: StatusCancelled, StopReason: "cancelled", StepCount: s.steps}
				}
				remaining := s.setCycleRemaining(maxToolCallsPerCycle - calls)
				calls++
				s.steps++
				s.flagComplete = false
				s.loopEvent(LoopEvent{Kind: "tool_call", MessageType: "assistant", Subtype: "tool_use", Turn: turn,
					Step: s.steps, MessageID: resp.ID, ToolCallID: tc.ID, Tool: tc.Function.Name, Text: tc.Function.Arguments})
				result := "tool budget exhausted; call was not executed"
				if remaining > 0 && calls <= maxToolCallsPerCycle && (s.stepLimit == 0 || s.steps+s.delegatedStepsTotal <= s.stepLimit) {
					result = s.dispatch(ctx, tc.Function.Name, tc.Function.Arguments)
				}
				s.loopEvent(LoopEvent{Kind: "tool_result", MessageType: "user", Subtype: "tool_result", Turn: turn,
					Step: s.steps, MessageID: resp.ID, ToolCallID: tc.ID, Tool: tc.Function.Name, Text: result})
				s.appendMessage(openai.ChatCompletionMessage{
					Role: openai.ChatMessageRoleTool, ToolCallID: tc.ID, Content: result,
				})
				if ctx.Err() != nil {
					s.skipToolCalls(msg.ToolCalls[i+1:], turn, resp.ID, "cancelled; call was not executed")
					return Result{Status: StatusCancelled, StopReason: "cancelled", StepCount: s.steps}
				}
				// A confirmed flag that completed the challenge ends this executor
				// (recorded; does not cancel siblings). The scheduler's poller also
				// kills the swarm on the platform-confirmed solve.
				if tc.Function.Name == "submit_flag" && s.flagComplete && s.lastFlag != "" {
					// The assistant may have announced more calls in this same
					// message. Close each with a skipped result so the journal and
					// next session never contain unmatched tool-call IDs.
					for _, skipped := range msg.ToolCalls[i+1:] {
						calls++
						s.steps++
						s.loopEvent(LoopEvent{Kind: "tool_call", MessageType: "assistant", Subtype: "tool_use", Turn: turn,
							Step: s.steps, MessageID: resp.ID, ToolCallID: skipped.ID, Tool: skipped.Function.Name, Text: skipped.Function.Arguments})
						const reason = "skipped: confirmed flag completed the mission"
						s.loopEvent(LoopEvent{Kind: "tool_result", MessageType: "user", Subtype: "tool_result", Turn: turn,
							Step: s.steps, MessageID: resp.ID, ToolCallID: skipped.ID, Tool: skipped.Function.Name, Text: reason})
						s.appendMessage(openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: skipped.ID, Content: reason})
					}
					s.loopEvent(LoopEvent{Kind: "turn_end", MessageType: "system", Subtype: "turn_end", Turn: turn,
						Step: s.steps, Outcome: "flag_found"})
					return Result{Status: StatusFlagFound, Flag: s.lastFlag, StepCount: s.steps, Findings: strings.TrimSpace(msg.Content)}
				}
			}
		}
		s.loopEvent(LoopEvent{Kind: "turn_end", MessageType: "system", Subtype: "turn_end", Turn: turn,
			Step: s.steps, Outcome: "tool_use"})
		if s.stepLimit > 0 && s.steps+s.delegatedStepsTotal >= s.stepLimit {
			return Result{Status: StatusStepBudget, StopReason: "max_steps", StepCount: s.steps}
		}
		if calls+s.cycleDelegatedSteps >= maxToolCallsPerCycle {
			return Result{Status: StatusGaveUp, Findings: "max tool calls per cycle reached", StopReason: "max_cycle_steps", StepCount: s.steps}
		}
	}
}

// skipToolCalls closes an assistant's announced calls before cancellation, so
// the append-only session never contains unmatched tool-call IDs.
func (s *Solver) skipToolCalls(calls []openai.ToolCall, turn int, messageID, reason string) {
	for _, tc := range calls {
		s.steps++
		s.loopEvent(LoopEvent{Kind: "tool_call", MessageType: "assistant", Subtype: "tool_use", Turn: turn,
			Step: s.steps, MessageID: messageID, ToolCallID: tc.ID, Tool: tc.Function.Name, Text: tc.Function.Arguments})
		s.loopEvent(LoopEvent{Kind: "tool_result", MessageType: "user", Subtype: "tool_result", Turn: turn,
			Step: s.steps, MessageID: messageID, ToolCallID: tc.ID, Tool: tc.Function.Name, Text: reason})
		s.appendMessage(openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: tc.ID, Content: reason})
	}
}

// --- scope guard helpers ---

func (s *Solver) hostAllowed(host string) (bool, string) {
	currentScope := s.scopeSnapshot()
	if currentScope.IsEmpty() {
		return false, "【平台管控·非目标防御】BLOCKED (scope lock — NO TARGET KNOWN): the scope is empty; all egress is refused."
	}
	if currentScope.Allows(host) {
		return true, ""
	}
	return false, fmt.Sprintf("【平台管控·非目标防御】BLOCKED (scope lock): %q is not within the authorized scope (%s).", host, currentScope.Describe())
}

type evRef struct {
	Ref  string `json:"ref"`
	ID   string `json:"id"`
	Role string `json:"role"`
	Note string `json:"note"`
}

func (s *Solver) normalizeRefs(raw []evRef) ([]blackboard.EvidenceRef, []string) {
	var out []blackboard.EvidenceRef
	var warns []string
	for _, r := range raw {
		id := r.Ref
		if id == "" {
			id = r.ID
		}
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !s.tl.Exists(id) {
			warns = append(warns, id+" (no such traffic — dropped)")
			continue
		}
		role := strings.ToLower(strings.TrimSpace(r.Role))
		if !traffic.Roles[role] {
			role = traffic.DefaultRole
		}
		note := r.Note
		if len(note) > 300 {
			note = note[:300]
		}
		out = append(out, blackboard.EvidenceRef{Ref: id, Role: role, Note: note})
	}
	return out, warns
}
