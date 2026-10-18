package solver

import (
	"fmt"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

// buildConvo makes a system prompt plus n cycles, each: a user prompt, an
// assistant message with one tool call, and the matching tool result. big pads
// each tool result so the transcript is large.
func buildConvo(n, pad int) []openai.ChatCompletionMessage {
	msgs := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "SYSTEM PROMPT"}}
	for c := 1; c <= n; c++ {
		id := fmt.Sprintf("call_%d", c)
		msgs = append(msgs,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: fmt.Sprintf("cycle %d: working record summary\n\nContinue solving.", c)},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{
				ID: id, Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "http_request", Arguments: `{"url":"http://t/"}`},
			}}},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: id, Content: "RESULT " + strings.Repeat("x", pad)},
		)
	}
	return msgs
}

// assertPairingValid fails if any tool message lacks a preceding assistant
// tool_call with its ID, or any assistant tool_call lacks a following tool
// result — the invariants the OpenAI API enforces.
func assertPairingValid(t *testing.T, msgs []openai.ChatCompletionMessage) {
	t.Helper()
	open := map[string]bool{} // tool_call ids awaiting a result
	seen := map[string]bool{} // tool_call ids ever announced
	for _, m := range msgs {
		switch m.Role {
		case openai.ChatMessageRoleAssistant:
			for _, tc := range m.ToolCalls {
				open[tc.ID] = true
				seen[tc.ID] = true
			}
		case openai.ChatMessageRoleTool:
			if !seen[m.ToolCallID] {
				t.Fatalf("tool result for %q has no preceding assistant tool_call", m.ToolCallID)
			}
			delete(open, m.ToolCallID)
		}
	}
	if len(open) != 0 {
		t.Fatalf("assistant tool_calls left without results: %v", open)
	}
}

func TestCompactWindowDropsOldCyclesKeepsPairing(t *testing.T) {
	msgs := buildConvo(20, 3000) // ~20 cycles, big results → well over budget
	budget := 30_000
	out, changed := compactWindow(msgs, 3, budget)
	if !changed {
		t.Fatalf("expected compaction to trigger")
	}
	if out[0].Role != openai.ChatMessageRoleSystem || out[0].Content != "SYSTEM PROMPT" {
		t.Fatalf("system prompt not preserved as first message")
	}
	if len(out) >= len(msgs) {
		t.Fatalf("compaction did not shrink the transcript (%d -> %d)", len(msgs), len(out))
	}
	if totalChars(out) > budget {
		// one cycle alone may exceed budget; clipOld should have engaged
		t.Fatalf("compacted transcript still over budget: %d > %d", totalChars(out), budget)
	}
	assertPairingValid(t, out)
	// the most recent cycle's user prompt must survive (fresh summary for the model)
	last := out[len(out)-1]
	_ = last
	foundRecent := false
	for _, m := range out {
		if m.Role == openai.ChatMessageRoleUser && strings.Contains(m.Content, "cycle 20") {
			foundRecent = true
		}
	}
	if !foundRecent {
		t.Fatalf("most recent cycle was not retained")
	}
}

func TestCompactWindowNoopWhenSmall(t *testing.T) {
	msgs := buildConvo(3, 50)
	out, changed := compactWindow(msgs, 3, contextBudgetChars)
	if changed {
		t.Fatalf("small transcript should not be compacted")
	}
	if len(out) != len(msgs) {
		t.Fatalf("small transcript was modified")
	}
}

func TestCompactWindowSingleHugeCycleClips(t *testing.T) {
	// One cycle whose tool result alone blows the budget: no cycle to drop, so
	// clipOld must shrink it while keeping pairing valid.
	msgs := buildConvo(1, 200_000)
	out, changed := compactWindow(msgs, 3, 50_000)
	if !changed {
		t.Fatalf("expected clipping to trigger")
	}
	assertPairingValid(t, out)
}
