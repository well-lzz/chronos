package solver

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	openai "github.com/sashabaranov/go-openai"
)

const contextBudgetChars = 140_000 // fixture for compaction algorithm tests

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

// A user message injected mid tool-loop (operator Guide/SetScope lands between an
// assistant's tool_calls and their results) must NOT be chosen as a cut point —
// cutting there would orphan a tool result. Compaction must keep pairing valid.
func TestCompactWindowIgnoresMidToolUserInjection(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "SYS"}}
	for c := 1; c <= 10; c++ {
		id1 := fmt.Sprintf("a_%d", c)
		id2 := fmt.Sprintf("b_%d", c)
		msgs = append(msgs,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: fmt.Sprintf("cycle %d continue", c)},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{
				{ID: id1, Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "t", Arguments: "{}"}},
				{ID: id2, Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "t", Arguments: "{}"}},
			}},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: id1, Content: "r1 " + strings.Repeat("y", 2000)},
			// operator injection landing BETWEEN the two tool results:
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "[Operator instruction] keep going"},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: id2, Content: "r2 " + strings.Repeat("z", 2000)},
		)
	}
	out, changed := compactWindow(msgs, 3, 20_000)
	if !changed {
		t.Fatalf("expected compaction")
	}
	assertPairingValid(t, out)
	if !pairingValid(out) {
		t.Fatalf("runtime pairingValid disagrees with test helper")
	}
}

func TestCompactWindowSingleHugeCycleClips(t *testing.T) {
	// One cycle whose tool result alone blows the budget: no cycle to drop, so
	// clipOld must shrink it while keeping pairing valid AND actually reaching
	// budget — not just report "compacted".
	const budget = 50_000
	msgs := buildConvo(1, 200_000)
	out, changed := compactWindow(msgs, 3, budget)
	if !changed {
		t.Fatalf("expected clipping to trigger")
	}
	if got := totalChars(out); got > budget {
		t.Fatalf("compaction reported done but stayed over budget: %d > %d", got, budget)
	}
	assertPairingValid(t, out)
}

func TestCompactWindowDenseSingleCycleFitsBudget(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "SYSTEM PROMPT"},
		{Role: openai.ChatMessageRoleUser, Content: "Continue solving."},
	}
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("dense_%d", i)
		msgs = append(msgs,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{
				ID: id, Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "http_request", Arguments: `{"url":"http://t/"}`},
			}}},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: id, Content: strings.Repeat("汉", 8_000)},
		)
	}
	out, changed := compactWindow(msgs, 3, contextBudgetChars)
	if !changed || totalChars(out) > contextBudgetChars {
		t.Fatalf("dense cycle did not fit budget: changed=%v chars=%d", changed, totalChars(out))
	}
	assertPairingValid(t, out)
	for _, m := range out {
		if !utf8.ValidString(m.Content) {
			t.Fatal("compaction split a UTF-8 rune")
		}
	}
}

func TestCompactWindowClipsMultiCallArgumentsWithoutMutatingInput(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "SYSTEM"},
		{Role: openai.ChatMessageRoleUser, Content: "Continue"},
		{Role: openai.ChatMessageRoleAssistant},
	}
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("multi_%d", i)
		msgs[2].ToolCalls = append(msgs[2].ToolCalls, openai.ToolCall{
			ID: id, Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: "run_python", Arguments: `{"code":"` + strings.Repeat("x", 24_000) + `"}`},
		})
		msgs = append(msgs, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: id, Content: "done"})
	}
	originalSize := totalChars(msgs)
	out, changed := compactWindow(msgs, 3, contextBudgetChars)
	if !changed || totalChars(out) > contextBudgetChars {
		t.Fatalf("multi-call arguments exceeded budget: %d", totalChars(out))
	}
	if totalChars(msgs) != originalSize {
		t.Fatal("compaction mutated the input snapshot's tool calls")
	}
	assertPairingValid(t, out)
	for _, m := range out {
		for _, call := range m.ToolCalls {
			if !json.Valid([]byte(call.Function.Arguments)) {
				t.Fatalf("compaction produced invalid tool arguments: %q", call.Function.Arguments)
			}
		}
	}
}

func TestCompactWindowClipsWhenRecentCycleIsHuge(t *testing.T) {
	// A huge tool result in the MOST RECENT cycle (the old "keep last 6 full"
	// logic would have left it untouched). Compaction must still reach budget.
	const budget = 40_000
	msgs := buildConvo(6, 200) // several small cycles...
	// ...then append one more cycle whose tool result is enormous.
	id := "call_huge"
	msgs = append(msgs,
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "cycle 7: continue"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{
			ID: id, Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "http_request", Arguments: `{"url":"http://t/"}`},
		}}},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: id, Content: "HUGE " + strings.Repeat("y", 300_000)},
	)
	out, changed := compactWindow(msgs, 3, budget)
	if !changed {
		t.Fatal("expected compaction")
	}
	if got := totalChars(out); got > budget {
		t.Fatalf("recent huge cycle left over budget: %d > %d", got, budget)
	}
	assertPairingValid(t, out)
}
