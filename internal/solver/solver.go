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
	ModelSpec string
	Model     string
	Client    *openai.Client
	Meta      mission.Meta
	Scope     scope.Scope
	BB        *blackboard.Blackboard
	TL        *traffic.Log
	AllowExec bool
	Deadline  time.Time
	Tokens    *int64
	Notify    func(string)
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

	meta      mission.Meta
	scope     scope.Scope
	bb        *blackboard.Blackboard
	tl        *traffic.Log
	allowExec bool
	deadline  time.Time

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
}

// New builds a solver from Options.
func New(o Options) *Solver {
	return &Solver{
		ModelSpec: o.ModelSpec, Model: o.Model, client: o.Client, meta: o.Meta, scope: o.Scope,
		bb: o.BB, tl: o.TL, allowExec: o.AllowExec, deadline: o.Deadline, tokens: o.Tokens,
		notify: o.Notify, submitFn: o.SubmitFn, record: o.Record, resolve: o.Resolve,
	}
}

func (s *Solver) rec(kind, tool, text string) {
	if s.record != nil {
		s.record(s.steps, kind, tool, text)
	}
}

func (s *Solver) start() {
	if s.started {
		return
	}
	remaining := int(time.Until(s.deadline).Seconds())
	sys := prompts.BuildHeadlessPrompt(s.meta, remaining, s.allowExec && pythonAvailable())
	if s.scope.IsEmpty() {
		sys += "\n\n## SCOPE LOCK — NO TARGET KNOWN\nThe scope is empty; all egress is refused. Report via notify and stop."
	}
	s.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: sys}}
	s.started = true
}

// Bump injects sibling insights and prepares to resume.
func (s *Solver) Bump(insights string) {
	s.messages = append(s.messages, openai.ChatCompletionMessage{
		Role: openai.ChatMessageRoleUser,
		Content: "Your previous attempt did not finish. Insights from other executors (unverified — " +
			"verify before trusting):\n\n" + insights + "\n\nContinue the search.",
	})
}

// RunUntilDoneOrGaveUp runs the tool loop until the model stops calling tools
// (one cycle), the per-cycle cap is hit, or the context/deadline ends.
func (s *Solver) RunUntilDoneOrGaveUp(ctx context.Context) Result {
	s.start()

	// Resolve the live model config for this cycle. If the operator hasn't
	// configured a working model yet (or the key is wrong), report it as a
	// recoverable config error instead of a hard failure.
	if s.resolve != nil {
		client, model, err := s.resolve()
		if err != nil {
			return Result{Status: StatusConfigError, Findings: "model not configured: " + err.Error(), StepCount: s.steps}
		}
		s.client, s.Model = client, model
	}
	if s.client == nil {
		return Result{Status: StatusConfigError, Findings: "model not configured: open 模型设置 in the console", StepCount: s.steps}
	}

	tools := toolDefs(s.allowExec && pythonAvailable(), s.meta.HasFlags())

	// Prompt for this cycle, with the blackboard summary injected.
	prompt := "Solve this task."
	if len(s.messages) > 1 {
		prompt = "Continue solving."
	}
	if sum := s.bb.Summary(); sum != "" {
		prompt = sum + "\n\n" + prompt
	}
	s.messages = append(s.messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: prompt})

	calls := 0
	for {
		if err := ctx.Err(); err != nil {
			return Result{Status: StatusCancelled, StepCount: s.steps}
		}
		if time.Now().After(s.deadline) {
			return Result{Status: StatusGaveUp, Findings: "time budget reached", StepCount: s.steps}
		}
		resp, err := s.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
			Model: s.Model, Messages: s.messages, Tools: tools,
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
		s.messages = append(s.messages, msg)

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
			s.messages = append(s.messages, openai.ChatCompletionMessage{
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
	if s.scope.IsEmpty() {
		return false, "BLOCKED (scope lock — NO TARGET KNOWN): the scope is empty; all egress is refused."
	}
	if s.scope.Allows(host) {
		return true, ""
	}
	return false, fmt.Sprintf("BLOCKED (scope lock): %q is not within the authorized scope (%s).", host, s.scope.Describe())
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
