// Package solver is one headless executor: a model + the neutral tool surface,
// driving a search over an authorized scope. It records Facts/Findings on the
// shared blackboard and cites real captured traffic as evidence. No sandbox, no
// role split — reasoning plus http/tcp/python and the blackboard tools.
package solver

import (
	"context"
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
	StatusGaveUp    = "gave_up"
	StatusError     = "error"
	StatusCancelled = "cancelled"
	StatusFlagFound = "flag_found"
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

	messages []openai.ChatCompletionMessage
	started  bool
	steps    int
	tokens   *int64 // shared token counter (mission-wide)
	notify   func(string)
	lastFlag string
}

// New builds a solver. tokens is a shared counter; notify logs upward.
func New(modelSpec, model string, client *openai.Client, meta mission.Meta, sc scope.Scope,
	bb *blackboard.Blackboard, tl *traffic.Log, allowExec bool, deadline time.Time,
	tokens *int64, notify func(string)) *Solver {
	return &Solver{
		ModelSpec: modelSpec, Model: model, client: client, meta: meta, scope: sc,
		bb: bb, tl: tl, allowExec: allowExec, deadline: deadline, tokens: tokens, notify: notify,
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
			return Result{Status: StatusGaveUp, Findings: strings.TrimSpace(msg.Content), StepCount: s.steps}
		}
		for _, tc := range msg.ToolCalls {
			calls++
			s.steps++
			result := s.dispatch(ctx, tc.Function.Name, tc.Function.Arguments)
			s.messages = append(s.messages, openai.ChatCompletionMessage{
				Role: openai.ChatMessageRoleTool, ToolCallID: tc.ID, Content: result,
			})
			if tc.Function.Name == "submit_flag" && s.meta.HasFlags() {
				if flag := s.lastFlag; flag != "" {
					return Result{Status: StatusFlagFound, Flag: flag, StepCount: s.steps, Findings: strings.TrimSpace(msg.Content)}
				}
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
