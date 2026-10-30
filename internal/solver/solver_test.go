package solver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	s.start()
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
	if got := s.messageSnapshot()[0].Content; strings.Contains(got, "old.example") || !strings.Contains(got, "new.example") {
		t.Fatalf("system prompt retained stale scope: %s", got)
	}
}

func TestOperatorInstructionAndProgressUseExecutorSession(t *testing.T) {
	requests := make([]map[string]any, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		requests = append(requests, request)
		w.Header().Set("Content-Type", "application/json")
		if len(requests) == 1 {
			_, _ = w.Write([]byte(`{"id":"step-1","object":"chat.completion","created":1,"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"先核对已有证据。","tool_calls":[{"id":"call-1","type":"function","function":{"name":"add_fact","arguments":"{\"statement\":\"evidence reviewed\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"total_tokens":7}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"step-2","object":"chat.completion","created":1,"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"已核对证据，记录了一条事实。","finish_reason":"stop"}}],"usage":{"total_tokens":7}}`))
	}))
	defer server.Close()
	config := openai.DefaultConfig("test-key")
	config.BaseURL = server.URL + "/v1"
	bb, err := blackboard.LoadOrNew("chat-session-test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tokens := int64(0)
	var events []LoopEvent
	sessionPath := filepath.Join(t.TempDir(), "agent.jsonl")
	s := New(Options{Model: "test-model", Client: openai.NewClientWithConfig(config),
		Scope: scope.Parse([]string{"target.example"}), BB: bb, Tokens: &tokens,
		SessionPath: sessionPath, RecordEvent: func(event LoopEvent) { events = append(events, event) }})
	s.Guide("请检查已有证据并告诉我结论")
	result := s.RunUntilDoneOrGaveUp(context.Background())
	if result.Findings != "已核对证据，记录了一条事实。" || result.Tokens != 14 || len(requests) != 2 {
		t.Fatalf("result=%+v requests=%d", result, len(requests))
	}
	first := requests[0]["messages"].([]any)
	last := first[len(first)-1].(map[string]any)["content"].(string)
	if !strings.Contains(last, "请检查已有证据") {
		t.Fatalf("operator message missing from executor turn: %s", last)
	}
	second := requests[1]["messages"].([]any)
	toolSeen := false
	for _, message := range second {
		if message.(map[string]any)["role"] == "tool" {
			toolSeen = true
		}
	}
	if !toolSeen {
		t.Fatal("tool result missing from second model request")
	}
	want := []string{"input_applied", "turn_start", "progress", "tool_call", "tool_result", "turn_end", "turn_start", "assistant", "turn_end", "result"}
	if len(events) != len(want) {
		t.Fatalf("event count=%d want=%d events=%+v", len(events), len(want), events)
	}
	for i, kind := range want {
		if events[i].Kind != kind {
			t.Fatalf("event %d=%s want=%s", i, events[i].Kind, kind)
		}
	}
	if events[0].Turn != 1 || events[3].Turn != 1 || events[3].MessageID != "step-1" || events[3].ToolCallID != "call-1" ||
		events[4].ToolCallID != "call-1" || events[4].MessageType != "user" ||
		events[7].Turn != 2 || !events[7].Final || events[9].Tokens != 14 {
		t.Fatalf("message identity or result usage lost: %+v", events)
	}
	journal, err := loadSession(sessionPath)
	if err != nil || journal.turn != 2 {
		t.Fatalf("turn cursor not journaled: state=%+v err=%v", journal, err)
	}
}

func TestEmptyModelMessageEndsAsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"empty","object":"chat.completion","created":1,"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"total_tokens":3}}`))
	}))
	defer server.Close()
	config := openai.DefaultConfig("test-key")
	config.BaseURL = server.URL + "/v1"
	bb, err := blackboard.LoadOrNew("empty-message-test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tokens := int64(0)
	var events []LoopEvent
	s := New(Options{Model: "test-model", Client: openai.NewClientWithConfig(config), BB: bb, Tokens: &tokens,
		RecordEvent: func(event LoopEvent) { events = append(events, event) }})
	result := s.RunUntilDoneOrGaveUp(context.Background())
	if result.Status != StatusError || result.StopReason != "empty_final" || len(events) != 3 ||
		events[1].Kind != "turn_end" || events[1].Outcome != "empty_final" || events[2].Kind != "result" {
		t.Fatalf("empty completion was treated as a valid reply: result=%+v events=%+v", result, events)
	}
}

func TestCompletedFlagClosesRemainingToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"multi","object":"chat.completion","created":1,"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"flag-call","type":"function","function":{"name":"submit_flag","arguments":"{\"flag\":\"FLAG{ok}\"}"}},{"id":"later-call","type":"function","function":{"name":"add_fact","arguments":"{\"statement\":\"must not run\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"total_tokens":5}}`))
	}))
	defer server.Close()
	config := openai.DefaultConfig("test-key")
	config.BaseURL = server.URL + "/v1"
	bb, err := blackboard.LoadOrNew("flag-pairing-test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tokens := int64(0)
	s := New(Options{Model: "test-model", Client: openai.NewClientWithConfig(config), BB: bb, Tokens: &tokens,
		SubmitFn: func(string) (string, bool, bool) { return "correct", true, true }})
	result := s.RunUntilDoneOrGaveUp(context.Background())
	if result.Status != StatusFlagFound || !pairingValid(s.messageSnapshot()) || bb.Counts()["facts"] != 0 {
		t.Fatalf("confirmed flag left an unmatched or executed follow-up call: result=%+v messages=%+v", result, s.messageSnapshot())
	}
}
