package solver

import (
	openai "github.com/sashabaranov/go-openai"
)

// Context-window management.
//
// One executor keeps a SINGLE growing conversation across all of its retry
// cycles. Without bounding, every tool call and result accumulates and the whole
// history is re-sent on each API call — the dominant token cost on long/hard
// tasks (observed: ~88k tokens per call on a stuck challenge). The durable
// knowledge already lives on the shared blackboard and is re-injected as a
// summary at the top of every cycle, so the raw transcript of older cycles is
// redundant and can be dropped.
//
// Compaction must never break the tool-call/tool-result pairing the OpenAI API
// requires (an assistant message with tool_calls must be followed by the matching
// tool messages). It therefore only ever cuts at a USER-message boundary — the
// start of a cycle / bump / guide / scope-update — so no assistant tool_call is
// ever separated from its results, and the retained tail is always structurally
// valid on its own.

const (
	// contextBudgetChars triggers compaction (≈ 4 chars per token). Set well under
	// the model's context window so re-sent history stays cheap.
	contextBudgetChars = 140_000
	// contextMaxKeepTurns is the most recent cycle boundaries kept when compacting.
	contextMaxKeepTurns = 3
	// perMsgClip bounds a single retained tool/assistant message's content when
	// even one cycle alone exceeds the budget.
	perMsgClip = 6_000
)

func msgChars(m openai.ChatCompletionMessage) int {
	n := len(m.Content)
	for _, tc := range m.ToolCalls {
		n += len(tc.Function.Name) + len(tc.Function.Arguments)
	}
	return n
}

func totalChars(msgs []openai.ChatCompletionMessage) int {
	n := 0
	for _, m := range msgs {
		n += msgChars(m)
	}
	return n
}

// compactWindow bounds the conversation to roughly budget characters while
// preserving the system message, tool-call pairing, and the most recent cycles.
// Returns the (possibly) new slice and whether it changed.
func compactWindow(msgs []openai.ChatCompletionMessage, maxKeepTurns, budget int) ([]openai.ChatCompletionMessage, bool) {
	if budget <= 0 || maxKeepTurns < 1 {
		return msgs, false
	}
	if totalChars(msgs) <= budget {
		return msgs, false
	}
	if len(msgs) == 0 || msgs[0].Role != openai.ChatMessageRoleSystem {
		return msgs, false
	}

	// Cycle boundaries = user messages after the system message.
	var userIdx []int
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role == openai.ChatMessageRoleUser {
			userIdx = append(userIdx, i)
		}
	}
	if len(userIdx) < 2 {
		// Only the current cycle exists — nothing to drop. Clip bulky old messages.
		return clipOld(msgs, budget), true
	}

	for keep := maxKeepTurns; keep >= 1; keep-- {
		if keep > len(userIdx) {
			continue
		}
		cut := userIdx[len(userIdx)-keep]
		out := make([]openai.ChatCompletionMessage, 0, len(msgs)-cut+1)
		out = append(out, msgs[0])     // system prompt
		out = append(out, msgs[cut:]...) // last `keep` cycles (starts on a user message)
		if totalChars(out) <= budget || keep == 1 {
			if keep == 1 && totalChars(out) > budget {
				out = clipOld(out, budget)
			}
			return out, true
		}
	}
	return msgs, false
}

// clipOld shrinks the Content of bulky tool/assistant messages — keeping role and
// ToolCallID/ToolCalls so pairing stays intact — oldest first, leaving the last
// few messages at full fidelity, until under budget or nothing is left to clip.
func clipOld(msgs []openai.ChatCompletionMessage, budget int) []openai.ChatCompletionMessage {
	out := append([]openai.ChatCompletionMessage(nil), msgs...)
	keepFullFrom := len(out) - 6
	if keepFullFrom < 1 {
		keepFullFrom = 1
	}
	for i := 1; i < keepFullFrom && totalChars(out) > budget; i++ {
		m := out[i]
		if (m.Role == openai.ChatMessageRoleTool || m.Role == openai.ChatMessageRoleAssistant) && len(m.Content) > perMsgClip {
			m.Content = m.Content[:perMsgClip/2] + "\n…[elided to conserve context]…\n" + m.Content[len(m.Content)-perMsgClip/2:]
			out[i] = m
		}
	}
	return out
}
