package solver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/well-lzz/chronos/internal/scope"
)

func TestSessionRestoresMessagesAndClosesInterruptedCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions", "agent.jsonl")
	first := New(Options{Scope: scope.Parse([]string{"old.example"}), SessionPath: path, Deadline: time.Now().Add(time.Minute)})
	first.start()
	first.appendMessage(openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "investigate"})
	first.appendMessage(openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{
		{ID: "a", Function: openai.FunctionCall{Name: "http_request", Arguments: `{"url":"http://old.example"}`}},
		{ID: "b", Function: openai.FunctionCall{Name: "http_request", Arguments: `{"url":"http://old.example"}`}},
	}})
	first.appendMessage(openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "a", Content: "traffic t1"})
	first.Guide("check evidence")

	restored := New(Options{Scope: scope.Parse([]string{"new.example"}), SessionPath: path, Deadline: time.Now().Add(time.Minute)})
	restored.start()
	msgs := restored.messageSnapshot()
	if !pairingValid(msgs) || !strings.Contains(msgs[0].Content, "new.example") || strings.Contains(msgs[0].Content, "old.example") {
		t.Fatalf("invalid recovered context or stale scope: %#v", msgs)
	}
	last := msgs[len(msgs)-1]
	if last.Role != openai.ChatMessageRoleTool || last.ToolCallID != "b" || !strings.Contains(last.Content, "interrupted") {
		t.Fatalf("unfinished tool call was not closed: %#v", last)
	}
	restored.flushUser()
	if tail := restored.messageSnapshot(); !strings.Contains(tail[len(tail)-1].Content, "check evidence") {
		t.Fatal("queued operator instruction was lost")
	}
	state, err := loadSession(path)
	if err != nil || !pairingValid(state.messages) {
		t.Fatalf("recovered journal invalid: %v", err)
	}
}

func TestSessionTruncatesPartialTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	msg := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "keep"}
	if err := appendSessionEvent(path, sessionEvent{Type: "message", Message: &msg}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"message","message":`)
	_ = f.Close()
	state, err := loadSession(path)
	if err != nil || len(state.messages) != 1 || state.messages[0].Content != "keep" {
		t.Fatalf("partial tail recovery failed: %#v %v", state, err)
	}
}
