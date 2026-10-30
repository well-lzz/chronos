package solver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/mission"
	"github.com/well-lzz/chronos/internal/scope"
)

func TestDelegateWithoutDeadline(t *testing.T) {
	s := New(Options{})
	if reason := s.delegateGate(); reason != "" {
		t.Fatalf("unlimited delegation was blocked: %s", reason)
	}
	if deadline := s.childDeadline(); !deadline.IsZero() {
		t.Fatalf("unlimited parent created child deadline: %v", deadline)
	}
}

func TestDelegateUsesIsolatedWindowAndReturnsOwnedArtifacts(t *testing.T) {
	var mu sync.Mutex
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		mu.Lock()
		requests = append(requests, body)
		call := len(requests)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"id":"chat1","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"fact-call","type":"function","function":{"name":"add_fact","arguments":"{\"statement\":\"owned finding\",\"evidence\":\"mock\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"total_tokens":12}}`))
		} else {
			_, _ = w.Write([]byte(`{"id":"chat2","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"Subtask conclusion."},"finish_reason":"stop"}],"usage":{"total_tokens":8}}`))
		}
	}))
	defer server.Close()
	root, bb, events := newDelegateTestSolver(t, server.URL)
	if _, err := bb.AddFact("peer observation", "mock", "peer"); err != nil {
		t.Fatal(err)
	}
	result := decodeDelegateResult(t, root.toolDelegate(context.Background(), `{"task":"Investigate one endpoint","max_steps":3,"focus":"10.20.2.0/24"}`))
	if result.Status != "completed" || result.Summary != "Subtask conclusion." || len(result.FactIDs) != 1 || result.FactsAdded != 1 || result.AgentID == "" {
		t.Fatalf("unexpected child result: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(root.subagentRoot, result.AgentID, "session.jsonl")); err != nil {
		t.Fatalf("child transcript missing: %v", err)
	}
	metadata, err := os.ReadFile(filepath.Join(root.subagentRoot, result.AgentID, "metadata.json"))
	if err != nil || !strings.Contains(string(metadata), `"status": "completed"`) {
		t.Fatalf("child metadata missing completion: %s %v", metadata, err)
	}
	mu.Lock()
	first := requests[0]
	mu.Unlock()
	messages := first["messages"].([]any)
	for _, raw := range messages {
		if content, ok := raw.(map[string]any)["content"].(string); ok && strings.Contains(content, "PARENT SECRET TRANSCRIPT") {
			t.Fatal("child inherited parent transcript")
		}
	}
	if !strings.Contains(messages[0].(map[string]any)["content"].(string), "Investigate one endpoint") {
		t.Fatal("child prompt omitted delegated task")
	}
	for _, raw := range first["tools"].([]any) {
		name := raw.(map[string]any)["function"].(map[string]any)["name"]
		if name == "delegate" || name == "delegate_resume" {
			t.Fatal("leaf child received delegation tool")
		}
	}
	if len(*events) < 2 || (*events)[0].Kind != "delegate_spawn" || (*events)[len(*events)-1].Kind != "delegate_return" {
		t.Fatalf("parent delegation lifecycle events missing: %v", *events)
	}
}

func TestDelegateResumeContinuesSameChildTranscript(t *testing.T) {
	var mu sync.Mutex
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		requests = append(requests, body)
		call := len(requests)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"id":"chat1","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"fact-call","type":"function","function":{"name":"add_fact","arguments":"{\"statement\":\"first pass\",\"evidence\":\"mock\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"total_tokens":12}}`))
		} else {
			_, _ = w.Write([]byte(`{"id":"chat2","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"Completed after resuming."},"finish_reason":"stop"}],"usage":{"total_tokens":8}}`))
		}
	}))
	defer server.Close()
	root, _, _ := newDelegateTestSolver(t, server.URL)
	first := decodeDelegateResult(t, root.toolDelegate(context.Background(), `{"task":"Investigate one endpoint","max_steps":1}`))
	if first.Status != "stopped" || first.StoppedReason != "max_steps" || len(first.FactIDs) != 1 {
		t.Fatalf("first child run: %+v", first)
	}
	second := decodeDelegateResult(t, root.toolDelegateResume(context.Background(), `{"agent_id":"`+first.AgentID+`","extra_steps":2,"note":"Check the first pass and finish"}`))
	if second.Status != "completed" || second.AgentID != first.AgentID || len(second.FactIDs) != 1 {
		t.Fatalf("resumed child run: %+v", second)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("expected two model requests, got %d", len(requests))
	}
	var transcript string
	for _, raw := range requests[1]["messages"].([]any) {
		message := raw.(map[string]any)
		if content, ok := message["content"].(string); ok {
			transcript += content
		}
	}
	if !strings.Contains(transcript, "fact f") || !strings.Contains(transcript, "Check the first pass and finish") {
		t.Fatalf("resume did not preserve tool result and note: %s", transcript)
	}
}

func TestDelegateRejectsForeignFocusAndLowGlobalBudget(t *testing.T) {
	root := New(Options{Scope: scope.Parse([]string{"10.20.0.0/16"}), Deadline: time.Now().Add(time.Minute),
		MaxDelegateDepth: 1, MaxDelegateChildren: 2, MaxDelegateSteps: 10, MinRemainingToSpawn: 30 * time.Second,
		GlobalRemaining: func() float64 { return 20 }})
	if got := root.toolDelegate(context.Background(), `{"task":"investigate","max_steps":5}`); !strings.Contains(got, "global budget too low") {
		t.Fatalf("low-budget gate failed: %s", got)
	}
	root.globalRemaining = nil
	if _, err := root.childScope("10.21.0.0/16"); err == nil {
		t.Fatal("foreign focus was accepted")
	}
	if _, err := root.childScope("10.20.1.0/24"); err != nil {
		t.Fatal(err)
	}
}

func TestDelegationBudgetIsChargedToParentAndDepthIsBounded(t *testing.T) {
	root := New(Options{Meta: mission.Meta{Name: "ordinary"}, MaxDelegateDepth: 2,
		MaxDelegateChildren: 2, MaxDelegateSteps: 20})
	if root.maxDelegateDepth != 1 || !root.canDelegate() {
		t.Fatal("ordinary mission should allow one level only")
	}
	root.delegateStepPool, root.cycleRemaining = 20, 40
	grant, reason := root.reserveChildSteps(12)
	if reason != "" || grant != 12 || root.delegatedStepsTotal != 12 {
		t.Fatalf("reservation grant=%d reason=%q total=%d", grant, reason, root.delegatedStepsTotal)
	}
	if available := root.setCycleRemaining(39); available != 27 {
		t.Fatalf("parent remaining tool budget=%d; want 27", available)
	}
	root.releaseChild()

	killchain := New(Options{Meta: mission.Meta{Category: "killchain"}, MaxDelegateDepth: 2})
	if killchain.maxDelegateDepth != 2 {
		t.Fatal("killchain opt-in depth two was lost")
	}
	leaf := New(Options{Depth: 2, MaxDelegateDepth: killchain.maxDelegateDepth})
	if leaf.canDelegate() {
		t.Fatal("depth-two leaf may not delegate")
	}
}

func TestIndependentDelegatesRunConcurrently(t *testing.T) {
	var mu sync.Mutex
	arrived := 0
	gate := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrived++
		if arrived == 2 {
			close(gate)
		}
		mu.Unlock()
		select {
		case <-gate:
		case <-time.After(2 * time.Second):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"}],"usage":{"total_tokens":8}}`))
	}))
	defer server.Close()
	root, _, _ := newDelegateTestSolver(t, server.URL)
	root.record = nil
	calls := []openai.ToolCall{
		{ID: "one", Function: openai.FunctionCall{Name: "delegate", Arguments: `{"task":"Investigate A","max_steps":3}`}},
		{ID: "two", Function: openai.FunctionCall{Name: "delegate", Arguments: `{"task":"Investigate B","max_steps":3}`}},
	}
	results := runToolBatch(context.Background(), calls, 2, root.dispatch)
	for _, raw := range results {
		if result := decodeDelegateResult(t, raw); result.Status != "completed" {
			t.Fatalf("parallel delegation failed: %+v", result)
		}
	}
	mu.Lock()
	got := arrived
	mu.Unlock()
	if got != 2 {
		t.Fatalf("only %d children reached the model", got)
	}
}

func TestParentScopeUpdateCanOnlyNarrowChild(t *testing.T) {
	parent := New(Options{Scope: scope.Parse([]string{"10.20.0.0/16"})})
	child := New(Options{AgentID: "child", Scope: scope.Parse([]string{"10.20.2.0/24"})})
	parent.children["child"] = &delegatedChild{id: "child", solver: child}
	parent.SetScope(scope.Parse([]string{"10.21.0.0/16"}))
	if !child.scopeSnapshot().IsEmpty() {
		t.Fatalf("child retained authority after parent scope was removed: %v", child.scopeSnapshot().Entries())
	}
	parent.SetScope(scope.Parse([]string{"10.20.0.0/16"}))
	if !child.scopeSnapshot().IsEmpty() {
		t.Fatal("parent scope expansion resurrected old child authority")
	}
}

type parentEvent struct{ Kind string }

func newDelegateTestSolver(t *testing.T, url string) (*Solver, *blackboard.Blackboard, *[]parentEvent) {
	t.Helper()
	config := openai.DefaultConfig("test-key")
	config.BaseURL = url + "/v1"
	bb, err := blackboard.LoadOrNew("delegate-solver", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tokens := int64(0)
	events := &[]parentEvent{}
	root := New(Options{ModelSpec: "parent", Model: "test", Client: openai.NewClientWithConfig(config),
		Meta: mission.Meta{Name: "test", Description: "Original mission"}, Scope: scope.Parse([]string{"10.20.0.0/16"}),
		BB: bb, Tokens: &tokens, Deadline: time.Now().Add(time.Minute), MaxDelegateDepth: 1,
		MaxDelegateChildren: 2, MaxDelegateSteps: 10, SubagentRoot: t.TempDir(),
		Record: func(_ int, kind, _, _ string) { *events = append(*events, parentEvent{Kind: kind}) }})
	root.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "PARENT SECRET TRANSCRIPT"}}
	root.delegateStepPool, root.cycleRemaining = 20, 40
	return root, bb, events
}

func decodeDelegateResult(t *testing.T, raw string) delegateResult {
	t.Helper()
	var result delegateResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("invalid delegate JSON: %s (%v)", raw, err)
	}
	return result
}
