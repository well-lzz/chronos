package solver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/well-lzz/chronos/internal/blackboard"
)

func TestRetryableModelErrorResumesWithoutDuplicatePrompt(t *testing.T) {
	var requests []map[string]any
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		mu.Lock()
		requests = append(requests, body)
		n := len(requests)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"chat","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"total_tokens":5}}`))
	}))
	defer server.Close()
	cfg := openai.DefaultConfig("test")
	cfg.BaseURL = server.URL + "/v1"
	bb, err := blackboard.LoadOrNew("retry-test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tokens := int64(0)
	s := New(Options{Model: "test", Client: openai.NewClientWithConfig(cfg), BB: bb, Deadline: time.Now().Add(time.Minute), Tokens: &tokens})
	if got := s.RunUntilDoneOrGaveUp(context.Background()).Status; got != StatusRetryableError {
		t.Fatalf("first status = %s, want retryable", got)
	}
	if got := s.RunUntilDoneOrGaveUp(context.Background()).Status; got != StatusGaveUp {
		t.Fatalf("second status = %s", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	for _, req := range requests {
		messages := req["messages"].([]any)
		if len(messages) != 2 {
			t.Fatalf("retry duplicated prompt: %d messages", len(messages))
		}
	}
}

func TestLongCycleInjectsBlackboardDelta(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		requests = append(requests, body)
		w.Header().Set("Content-Type", "application/json")
		if len(requests) == 1 {
			_, _ = w.Write([]byte(`{"id":"chat","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"add_fact","arguments":"{\"statement\":\"new inner-cycle fact\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"total_tokens":110}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"chat","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"total_tokens":125}}`))
	}))
	defer server.Close()
	cfg := openai.DefaultConfig("test")
	cfg.BaseURL = server.URL + "/v1"
	bb, _ := blackboard.LoadOrNew("delta-test", t.TempDir())
	_, _ = bb.AddFact("initial fact", "", "x")
	tokens := int64(0)
	s := New(Options{Model: "test", Client: openai.NewClientWithConfig(cfg), BB: bb, Deadline: time.Now().Add(time.Minute), Tokens: &tokens})
	if result := s.RunUntilDoneOrGaveUp(context.Background()); result.Status != StatusGaveUp {
		t.Fatalf("status: %+v", result)
	}
	if len(requests) != 2 {
		t.Fatalf("requests: %d", len(requests))
	}
	second, _ := json.Marshal(requests[1]["messages"])
	if !strings.Contains(string(second), "blackboard changes") || !strings.Contains(string(second), "new inner-cycle fact") {
		t.Fatalf("missing delta: %s", second)
	}
}

func TestContextOverflowCompactsAndRetriesOnce(t *testing.T) {
	var sizes []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []openai.ChatCompletionMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		sizes = append(sizes, totalChars(body.Messages))
		w.Header().Set("Content-Type", "application/json")
		if len(sizes) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"maximum context length exceeded","type":"invalid_request_error"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"chat","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"total_tokens":5}}`))
	}))
	defer server.Close()
	cfg := openai.DefaultConfig("test")
	cfg.BaseURL = server.URL + "/v1"
	bb, err := blackboard.LoadOrNew("overflow-test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tokens := int64(0)
	s := New(Options{Model: "test", Client: openai.NewClientWithConfig(cfg), BB: bb, Deadline: time.Now().Add(time.Minute), Tokens: &tokens})
	s.Guide(strings.Repeat("x", 95_000))
	if got := s.RunUntilDoneOrGaveUp(context.Background()).Status; got != StatusGaveUp {
		t.Fatalf("status = %s", got)
	}
	if len(sizes) != 2 || sizes[1] >= sizes[0] || sizes[1] > 60_000 {
		t.Fatalf("context recovery sizes = %v", sizes)
	}
}

func TestQueuedOperatorInputDoesNotSplitToolResults(t *testing.T) {
	s := &Solver{}
	s.appendMessage(openai.ChatCompletionMessage{Role: openai.ChatMessageRoleSystem, Content: "system"})
	s.appendMessage(openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{
		{ID: "a", Function: openai.FunctionCall{Name: "http_request", Arguments: "{}"}},
		{ID: "b", Function: openai.FunctionCall{Name: "http_request", Arguments: "{}"}},
	}})
	s.Guide("continue")
	s.appendMessage(openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "a", Content: "a"})
	s.appendMessage(openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "b", Content: "b"})
	s.flushUser()
	msgs := s.messageSnapshot()
	if !pairingValid(msgs) || msgs[len(msgs)-1].Role != openai.ChatMessageRoleUser {
		t.Fatalf("operator instruction split tool results: %#v", msgs)
	}
}

func TestParallelReadBatchBoundedAndOrdered(t *testing.T) {
	calls := make([]openai.ToolCall, 7)
	for i := range calls {
		calls[i] = openai.ToolCall{ID: fmt.Sprint(i), Function: openai.FunctionCall{
			Name: "http_request", Arguments: fmt.Sprintf(`{"url":"http://example/%d"}`, i),
		}}
	}
	var active, peak int32
	dispatch := func(_ context.Context, _, args string) string {
		n := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&peak)
			if n <= old || atomic.CompareAndSwapInt32(&peak, old, n) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return args
	}
	results := runToolBatch(context.Background(), calls, 5, dispatch)
	if peak < 2 || peak > maxParallelReads {
		t.Fatalf("parallelism = %d", peak)
	}
	for i := 0; i < 5; i++ {
		if results[i] != calls[i].Function.Arguments {
			t.Fatalf("result %d changed order", i)
		}
	}
	for _, result := range results[5:] {
		if !strings.Contains(result, "not executed") {
			t.Fatalf("over-budget call executed: %q", result)
		}
	}
	calls[0].Function.Arguments = `{"url":"http://example","method":"POST"}`
	if parallelReadBatch(calls) {
		t.Fatal("POST batch must stay sequential")
	}
}
