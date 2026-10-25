package solver

import (
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

func TestContextBudgetsFollowWindowAndPromptUsage(t *testing.T) {
	window := 8_000
	s := New(Options{Model: "custom", ContextWindow: func(string) int { return window }})
	baseTranscript, baseSummary := s.transcriptBudgetChars(), s.summaryBudgetChars()
	if baseTranscript <= baseSummary {
		t.Fatal("transcript and summary budgets must be independent")
	}
	window = 32_000
	if s.transcriptBudgetChars() <= baseTranscript || s.summaryBudgetChars() <= baseSummary {
		t.Fatal("budgets did not follow live model window")
	}
	s.lastPromptChars, s.lastPromptTokens = 20_000, 10_000 // actual provider feedback: 2 bytes/token
	if s.transcriptBudgetChars() >= 32_000*55*3/100 {
		t.Fatal("usage feedback failed to tighten budget")
	}
	if s.estimatedPromptTokens(20_000) != 10_000 {
		t.Fatal("token estimate not calibrated")
	}
}

func TestCompactionPreservesLatestBlackboardSnapshot(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "scope lock"},
		{Role: openai.ChatMessageRoleUser, Content: "## working record (blackboard)\n- [v1] (verified/high) must remember"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: "x", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "http_request", Arguments: "{}"}}}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "x", Content: strings.Repeat("r", 30_000)},
	}
	out, changed := compactWindow(msgs, 3, 2_000)
	if !changed || !pairingValid(out) || !strings.Contains(out[1].Content, "[v1]") {
		t.Fatalf("mandatory snapshot was clipped or pairing broken: %+v", out)
	}
}

func TestCompactionDoesNotClipMandatoryDelta(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "scope lock"},
		{Role: openai.ChatMessageRoleUser, Content: "## working record (blackboard)\ninitial"},
		{Role: openai.ChatMessageRoleUser, Content: "## blackboard changes (revision 1 → 2)\n- [v2] (verified/critical) must remember"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: "x", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "http_request", Arguments: "{}"}}}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "x", Content: strings.Repeat("r", 30_000)},
	}
	out, changed := compactWindow(msgs, 3, 2_000)
	found := false
	for _, m := range out {
		if strings.Contains(m.Content, "[v2]") {
			found = true
		}
	}
	if !changed || !pairingValid(out) || !found {
		t.Fatalf("delta clipped or pairing broken: %+v", out)
	}
}

func TestRetryRefreshKeepsOperatorInstruction(t *testing.T) {
	s := New(Options{})
	s.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "scope"},
		{Role: openai.ChatMessageRoleUser, Content: "Continue solving."},
		{Role: openai.ChatMessageRoleUser, Content: "[Operator instruction] investigate endpoint A"},
	}
	if !s.refreshCyclePrompt("[Current time budget] 20 seconds remain.\nContinue solving.") {
		t.Fatal("cycle prompt not found")
	}
	if !strings.Contains(s.messages[1].Content, "20 seconds") || !strings.Contains(s.messages[2].Content, "investigate endpoint A") {
		t.Fatalf("operator instruction was replaced: %+v", s.messages)
	}
}
