package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/well-lzz/chronos/internal/config"
	"github.com/well-lzz/chronos/internal/llmconfig"
	"github.com/well-lzz/chronos/internal/llmrec"
)

func TestFailoverAccountsForEachAttemptAndPersistsBreaker(t *testing.T) {
	primaryCalls, backupCalls := 0, 0
	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"expired key","type":"authentication_error"}}`))
	}))
	defer primaryServer.Close()
	backupServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"reply","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`))
	}))
	defer backupServer.Close()
	dir := t.TempDir()
	primary := llmconfig.Profile{Model: "first", BaseURL: primaryServer.URL, APIKey: "first-secret",
		Fallbacks: []llmconfig.FallbackProfile{{Model: "second", BaseURL: backupServer.URL, APIKey: "second-secret", Priority: 1}}}
	r := &Registry{path: filepath.Join(dir, "llm_breakers.json"), states: map[string]breakerState{}}
	recorder := llmrec.Shared(dir, false)
	response, err := r.Complete(context.Background(), config.Settings{StateDir: dir}, primary,
		openai.ChatCompletionRequest{Model: "first", Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hi"}}}, recorder, "mission", "executor")
	if err != nil || response.Choices[0].Message.Content != "ok" || primaryCalls != 1 || backupCalls != 1 {
		t.Fatalf("failover: response=%+v err=%v calls=%d/%d", response, err, primaryCalls, backupCalls)
	}
	rows := recorder.Recent(10)
	if len(rows) != 2 || rows[0].HTTPStatus != 401 || rows[1].Model != "second" || rows[1].TotalTokens != 6 {
		t.Fatalf("per-attempt usage: %+v", rows)
	}
	if !r.cooling(primary, time.Now()) {
		t.Fatal("hard failure did not open the breaker")
	}
	if data, err := os.ReadFile(r.path); err != nil || len(data) == 0 {
		t.Fatalf("breaker was not persisted: %v", err)
	}
	stateBytes, err := os.ReadFile(r.path)
	if err != nil {
		t.Fatal(err)
	}
	recovered := &Registry{states: map[string]breakerState{}}
	if err := json.Unmarshal(stateBytes, &recovered.states); err != nil {
		t.Fatal(err)
	}
	if !recovered.cooling(primary, time.Now()) {
		t.Fatal("restarted registry forgot cooldown")
	}
}

func TestFailoverNeverRetriesCancelledOrInvalidRequest(t *testing.T) {
	if shouldFailover(context.Canceled) || shouldFailover(context.DeadlineExceeded) || shouldFailover(&openai.APIError{HTTPStatusCode: 400}) {
		t.Fatal("cancelled or invalid request would spend a backup")
	}
	if !shouldFailover(&openai.APIError{HTTPStatusCode: 429}) {
		t.Fatal("rate limit should fail over")
	}
}

func TestSoftFailuresNeedThreeConsecutiveTrips(t *testing.T) {
	r := &Registry{states: map[string]breakerState{}}
	p := llmconfig.Profile{BaseURL: "https://example.test/v1", Model: "model", APIKey: "key"}
	err := &openai.APIError{HTTPStatusCode: 429}
	for i := 0; i < 2; i++ {
		r.failure(p, err, time.Now())
	}
	if r.cooling(p, time.Now()) {
		t.Fatal("breaker opened before third soft failure")
	}
	r.failure(p, err, time.Now())
	if !r.cooling(p, time.Now()) {
		t.Fatal("breaker failed to open after third soft failure")
	}
}
