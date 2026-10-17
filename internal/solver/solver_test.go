package solver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	openai "github.com/sashabaranov/go-openai"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/scope"
)

func TestConcurrentBumpAndMessageSnapshot(t *testing.T) {
	s := &Solver{}
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				s.Bump(fmt.Sprintf("%d/%d", worker, i))
				_ = s.messageSnapshot()
			}
		}(worker)
	}
	wg.Wait()
}

func TestSetScopeReplacesAuthorizationBoundary(t *testing.T) {
	s := New(Options{Scope: scope.Parse([]string{"old.example"})})
	s.chatHistory = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleAssistant, Content: "old scope"}}
	if ok, _ := s.hostAllowed("old.example"); !ok {
		t.Fatal("initial scope rejected old.example")
	}
	s.SetScope(scope.Parse([]string{"new.example", "10.20.0.0/16"}))
	if ok, _ := s.hostAllowed("old.example"); ok {
		t.Fatal("replaced scope still allowed old.example")
	}
	if ok, _ := s.hostAllowed("new.example"); !ok {
		t.Fatal("updated scope rejected new.example")
	}
	if ok, _ := s.hostAllowed("10.20.4.5"); !ok {
		t.Fatal("updated scope rejected CIDR member")
	}
	if len(s.chatHistory) != 0 {
		t.Fatal("scope update retained stale chat history")
	}
}

func TestReplyToOperatorUsesDedicatedNoToolsTurn(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat","object":"chat.completion","created":1,"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"收到，当前执行已暂停。"},"finish_reason":"stop"}],"usage":{"total_tokens":7}}`))
	}))
	defer server.Close()

	config := openai.DefaultConfig("test-key")
	config.BaseURL = server.URL + "/v1"
	bb, err := blackboard.LoadOrNew("chat-test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tokens := int64(0)
	s := New(Options{
		Model: "test-model", Client: openai.NewClientWithConfig(config),
		Scope: scope.Parse([]string{"target.example"}), BB: bb, Tokens: &tokens,
	})
	reply, err := s.ReplyToOperator(context.Background(), "请检查目标", true)
	if err != nil {
		t.Fatal(err)
	}
	if reply != "当前后端强制 Scope：\ntarget.example\n\n收到，当前执行已暂停。" {
		t.Fatalf("reply=%q", reply)
	}
	if _, ok := requestBody["tools"]; ok {
		t.Fatal("operator reply unexpectedly exposed tools")
	}
	messages, ok := requestBody["messages"].([]any)
	if !ok || len(messages) < 2 {
		t.Fatalf("messages=%T %#v", requestBody["messages"], requestBody["messages"])
	}
	systemMessage := messages[0].(map[string]any)["content"].(string)
	if !strings.Contains(systemMessage, "target.example") || !strings.Contains(systemMessage, "paused") {
		t.Fatalf("system message omitted scope or state: %s", systemMessage)
	}
}
