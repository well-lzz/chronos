package solver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	openai "github.com/sashabaranov/go-openai"
)

// sessionEvent is an append-only audit record. Compact events replace only the
// active model window; earlier message events remain available in the journal.
type sessionEvent struct {
	Type     string                         `json:"type"`
	Message  *openai.ChatCompletionMessage  `json:"message,omitempty"`
	Messages []openai.ChatCompletionMessage `json:"messages,omitempty"`
	Step     int                            `json:"step"`
	Context  *ContextEvent                  `json:"context,omitempty"`
}

type sessionState struct {
	messages []openai.ChatCompletionMessage
	pending  []openai.ChatCompletionMessage
	step     int
}

func appendSessionEvent(path string, event sessionEvent) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(event); err != nil {
		return err
	}
	return f.Sync()
}

func loadSession(path string) (sessionState, error) {
	var state sessionState
	if path == "" {
		return state, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	// A crash may interrupt the last write. Discard only the unfinished tail so
	// future appended events still form valid JSONL.
	if len(data) > 0 && data[len(data)-1] != '\n' {
		end := bytes.LastIndexByte(data, '\n') + 1
		if err := os.Truncate(path, int64(end)); err != nil {
			return state, err
		}
		data = data[:end]
	}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var event sessionEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return sessionState{}, fmt.Errorf("invalid session journal: %w", err)
		}
		switch event.Type {
		case "message":
			if event.Message != nil {
				state.messages = append(state.messages, *event.Message)
			}
		case "queue":
			if event.Message != nil {
				state.pending = append(state.pending, *event.Message)
			}
		case "flush":
			state.messages = append(state.messages, state.pending...)
			state.pending = nil
		case "compact":
			state.messages = event.Messages
		case "context":
			// Audit-only event; no effect on active model state.
		default:
			return sessionState{}, fmt.Errorf("unknown session event %q", event.Type)
		}
		if event.Step > state.step {
			state.step = event.Step
		}
	}
	return state, nil
}

// interruptedCalls finds calls whose result was not journaled before a crash.
// They must be closed with a synthetic result, never executed a second time.
func interruptedCalls(messages []openai.ChatCompletionMessage) []string {
	var ordered []string
	open := map[string]bool{}
	for _, message := range messages {
		if message.Role == openai.ChatMessageRoleAssistant {
			for _, call := range message.ToolCalls {
				ordered = append(ordered, call.ID)
				open[call.ID] = true
			}
		}
		if message.Role == openai.ChatMessageRoleTool {
			delete(open, message.ToolCallID)
		}
	}
	var missing []string
	for _, id := range ordered {
		if open[id] {
			missing = append(missing, id)
		}
	}
	return missing
}
