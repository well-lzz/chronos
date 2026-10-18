// Package solver is one headless executor: a model + the neutral tool surface,
// driving a search over an authorized scope. It records Facts/Findings on the
// shared blackboard and cites real captured traffic as evidence. No sandbox, no
// role split — reasoning plus http/tcp/python and the blackboard tools.
package solver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/mission"
	"github.com/well-lzz/chronos/internal/prompts"
	"github.com/well-lzz/chronos/internal/scope"
	"github.com/well-lzz/chronos/internal/traffic"
)

// Status values for a cycle result.
const (
	StatusGaveUp      = "gave_up"
	StatusError       = "error"
	StatusConfigError = "config_error" // model misconfigured/unauthorized — recoverable once the operator fixes it
	StatusCancelled   = "cancelled"
	StatusFlagFound   = "flag_found"
)

const maxToolCallsPerCycle = 40

// Result is one RunUntilDoneOrGaveUp outcome.
type Result struct {
	Status    string
	Findings  string
	StepCount int
	Flag      string
	Tokens    int
}

// SubmitFn submits a flag through the platform (dedup/cooldown handled by the
// caller). Returns the display text, whether it was confirmed, and whether that
// confirm completed the whole challenge.
type SubmitFn func(flag string) (display string, confirmed, complete bool)

// Options configures a Solver.
type Options struct {
	ModelSpec   string
	Model       string
	Client      *openai.Client
	Meta        mission.Meta
	Scope       scope.Scope
	BB          *blackboard.Blackboard
	TL          *traffic.Log
	AllowExec   bool
	InsecureTLS bool
	Deadline    time.Time
	Tokens      *int64
	Notify      func(string)
	// SubmitFn, when set, makes submit_flag go to the platform; nil = record-only.
	SubmitFn SubmitFn
	// Record, when set, receives each step for live observation (web console).
	Record func(step int, kind, tool, text string)
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

	meta        mission.Meta
	scope       scope.Scope
	bb          *blackboard.Blackboard
	tl          *traffic.Log
	allowExec   bool
	insecureTLS bool
	deadline    time.Time

	messages     []openai.ChatCompletionMessage
	started      bool
	steps        int
	tokens       *int64 // shared token counter (mission-wide)
	notify       func(string)
	submitFn     SubmitFn
	record       func(step int, kind, tool, text string)
	resolve      func() (*openai.Client, string, error)
	lastFlag     string
	flagComplete bool
	messageMu    sync.Mutex
	scopeMu      sync.RWMutex
	modelMu      sync.RWMutex
	chatMu       sync.Mutex
	chatHistory  []openai.ChatCompletionMessage
}

// New builds a solver from Options.
func New(o Options) *Solver {
	return &Solver{
		ModelSpec: o.ModelSpec, Model: o.Model, client: o.Client, meta: o.Meta, scope: o.Scope,
		bb: o.BB, tl: o.TL, allowExec: o.AllowExec, insecureTLS: o.InsecureTLS, deadline: o.Deadline, tokens: o.Tokens,
		notify: o.Notify, submitFn: o.SubmitFn, record: o.Record, resolve: o.Resolve,
	}
}

func (s *Solver) rec(kind, tool, text string) {
	if s.record != nil {
		s.record(s.steps, kind, tool, text)
	}
}

func (s *Solver) start() {
	s.messageMu.Lock()
	defer s.messageMu.Unlock()
	if s.started {
		return
	}
	remaining := int(time.Until(s.deadline).Seconds())
	sys := prompts.BuildHeadlessPrompt(s.meta, remaining, s.allowExec && pythonAvailable())
	if s.scopeSnapshot().IsEmpty() {
		sys += "\n\n## SCOPE LOCK — NO TARGET KNOWN\nThe scope is empty; all egress is refused. Report via notify and stop."
	}
	s.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: sys}}
	s.started = true
}

// Bump injects sibling insights and prepares to resume.
func (s *Solver) Bump(insights string) {
	s.messageMu.Lock()
	defer s.messageMu.Unlock()
	s.messages = append(s.messages, openai.ChatCompletionMessage{
		Role: openai.ChatMessageRoleUser,
		Content: "Your previous attempt did not finish. Insights from other executors (unverified — " +
			"verify before trusting):\n\n" + insights + "\n\nContinue the search.",
	})
}

// Guide injects an operator instruction into the execution loop without
// changing the structural scope lock. The next model turn can act on it only
// through tools that independently enforce the current scope.
func (s *Solver) Guide(message string) {
	s.appendMessage(openai.ChatCompletionMessage{
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
	s.scope = next
	s.scopeMu.Unlock()
	s.chatMu.Lock()
	s.chatHistory = nil
	s.chatMu.Unlock()
	s.appendMessage(openai.ChatCompletionMessage{
		Role: openai.ChatMessageRoleUser,
		Content: "[Operator scope update]\nThe authorized scope is now exactly: " + next.Describe() +
			". Treat all earlier scope descriptions as obsolete. Structural tool guards enforce this boundary.",
	})
}

func (s *Solver) scopeSnapshot() scope.Scope {
	s.scopeMu.RLock()
	defer s.scopeMu.RUnlock()
	return s.scope
}

// ReplyToOperator runs a separate no-tools conversation turn so every operator
// message receives a direct answer even while the autonomous execution loop is
// paused or busy. The same message is independently queued through Guide for
// execution; this method only communicates status and intent.
func (s *Solver) ReplyToOperator(ctx context.Context, message string, paused bool) (string, error) {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()

	s.modelMu.RLock()
	client, model := s.client, s.Model
	s.modelMu.RUnlock()
	if s.resolve != nil {
		var err error
		client, model, err = s.resolve()
		if err != nil {
			return "", err
		}
	}
	if client == nil {
		return "", errors.New("model is not configured")
	}

	state := "running"
	if paused {
		state = "paused; execution instructions are queued until the operator resumes it"
	}
	currentScope := s.scopeSnapshot().Describe()
	system := "You are the Chronos security-assessment Agent speaking directly with its human operator. " +
		"Reply concisely in the operator's language. This is a conversation response, so do not call tools and do not claim an action has completed unless the supplied status says so. " +
		"The execution state is " + state + ". The current authorized scope is exactly: " + currentScope + ". " +
		"If the request names a target outside that scope, clearly say it is blocked and instruct the operator to update Scope in the console first. " +
		"Never imply that a chat message itself can expand scope. Never describe Scope from memory: when asked, repeat the exact current value from this system message."
	if summary := s.bb.Summary(); summary != "" {
		system += "\n\nCurrent working-record summary:\n" + summary
	}

	messages := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: system}}
	if len(s.chatHistory) > 12 {
		messages = append(messages, s.chatHistory[len(s.chatHistory)-12:]...)
	} else {
		messages = append(messages, s.chatHistory...)
	}
	userMessage := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: strings.TrimSpace(message)}
	messages = append(messages, userMessage)

	resp, err := client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: model, Messages: messages, MaxTokens: 500,
	})
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("empty LLM response")
	}
	atomic.AddInt64(s.tokens, int64(resp.Usage.TotalTokens))
	reply := strings.TrimSpace(resp.Choices[0].Message.Content)
	if reply == "" {
		return "", errors.New("empty operator reply")
	}
	s.chatHistory = append(s.chatHistory, userMessage, openai.ChatCompletionMessage{
		Role: openai.ChatMessageRoleAssistant, Content: reply,
	})
	if len(s.chatHistory) > 24 {
		s.chatHistory = append([]openai.ChatCompletionMessage(nil), s.chatHistory[len(s.chatHistory)-24:]...)
	}
	return "当前后端强制 Scope：\n" + currentScope + "\n\n" + reply, nil
}

func (s *Solver) appendMessage(message openai.ChatCompletionMessage) {
	s.messageMu.Lock()
	s.messages = append(s.messages, message)
	s.messageMu.Unlock()
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
	s.messageMu.Lock()
	defer s.messageMu.Unlock()
	if out, changed := compactWindow(s.messages, contextMaxKeepTurns, contextBudgetChars); changed {
		s.messages = out
	}
}

func (s *Solver) hasHistory() bool {
	s.messageMu.Lock()
	defer s.messageMu.Unlock()
	return len(s.messages) > 1
}

// RunUntilDoneOrGaveUp runs the tool loop until the model stops calling tools
// (one cycle), the per-cycle cap is hit, or the context/deadline ends.
func (s *Solver) RunUntilDoneOrGaveUp(ctx context.Context) Result {
	s.start()

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

	tools := toolDefs(s.allowExec && pythonAvailable(), s.meta.HasFlags())

	// Prompt for this cycle, with the blackboard summary injected.
	prompt := "Solve this task."
	if s.hasHistory() {
		prompt = "Continue solving."
	}
	if sum := s.bb.Summary(); sum != "" {
		prompt = sum + "\n\n" + prompt
	}
	s.appendMessage(openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: prompt})

	calls := 0
	for {
		if err := ctx.Err(); err != nil {
			return Result{Status: StatusCancelled, StepCount: s.steps}
		}
		if time.Now().After(s.deadline) {
			return Result{Status: StatusGaveUp, Findings: "time budget reached", StepCount: s.steps}
		}
		s.compactMessages() // bound the growing transcript before re-sending it
		resp, err := client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
			Model: model, Messages: s.messageSnapshot(), Tools: tools,
		})
		if err != nil {
			if ctx.Err() != nil {
				return Result{Status: StatusCancelled, StepCount: s.steps}
			}
			// Auth / permission failures are a config problem the operator can fix
			// live (wrong or expired key, model not enabled) — surface them as such
			// so the executor waits and recovers instead of burning its retries.
			var apiErr *openai.APIError
			if errors.As(err, &apiErr) && (apiErr.HTTPStatusCode == 401 || apiErr.HTTPStatusCode == 403) {
				return Result{Status: StatusConfigError, Findings: fmt.Sprintf("LLM auth failed (HTTP %d): %s", apiErr.HTTPStatusCode, err.Error()), StepCount: s.steps}
			}
			return Result{Status: StatusError, Findings: "LLM error: " + err.Error(), StepCount: s.steps}
		}
		atomic.AddInt64(s.tokens, int64(resp.Usage.TotalTokens))
		if len(resp.Choices) == 0 {
			return Result{Status: StatusError, Findings: "empty LLM response", StepCount: s.steps}
		}
		msg := resp.Choices[0].Message
		s.appendMessage(msg)

		if len(msg.ToolCalls) == 0 {
			// Final text, no tool calls: this cycle is done.
			s.rec("text", "", strings.TrimSpace(msg.Content))
			return Result{Status: StatusGaveUp, Findings: strings.TrimSpace(msg.Content), StepCount: s.steps}
		}
		for _, tc := range msg.ToolCalls {
			calls++
			s.steps++
			s.flagComplete = false
			s.rec("tool_call", tc.Function.Name, tc.Function.Arguments)
			result := s.dispatch(ctx, tc.Function.Name, tc.Function.Arguments)
			s.rec("tool_result", tc.Function.Name, result)
			s.appendMessage(openai.ChatCompletionMessage{
				Role: openai.ChatMessageRoleTool, ToolCallID: tc.ID, Content: result,
			})
			// A confirmed flag that completed the challenge ends this executor
			// (recorded; does not cancel siblings). The scheduler's poller also
			// kills the swarm on the platform-confirmed solve.
			if tc.Function.Name == "submit_flag" && s.flagComplete && s.lastFlag != "" {
				return Result{Status: StatusFlagFound, Flag: s.lastFlag, StepCount: s.steps, Findings: strings.TrimSpace(msg.Content)}
			}
		}
		if calls >= maxToolCallsPerCycle {
			return Result{Status: StatusGaveUp, Findings: "max tool calls per cycle reached", StepCount: s.steps}
		}
	}
}

// --- scope guard helpers ---

func (s *Solver) hostAllowed(host string) (bool, string) {
	currentScope := s.scopeSnapshot()
	if currentScope.IsEmpty() {
		return false, "BLOCKED (scope lock — NO TARGET KNOWN): the scope is empty; all egress is refused."
	}
	if currentScope.Allows(host) {
		return true, ""
	}
	return false, fmt.Sprintf("BLOCKED (scope lock): %q is not within the authorized scope (%s).", host, currentScope.Describe())
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
