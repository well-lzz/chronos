package swarm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/well-lzz/chronos/internal/solver"
)

func TestChatTextContextReachesModelAfterRecoveryWithoutTruncation(t *testing.T) {
	message := "请分析客户材料\n" + strings.Repeat("中文证据与上下文\n", 700) + "END-OF-CUSTOMER-CONTEXT"
	var received atomic.Bool
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request openai.ChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		for _, input := range request.Messages {
			if strings.Contains(input.Content, message) {
				received.Store(true)
			}
		}
		reply(w, final())
	}))
	defer model.Close()
	s := executionSwarm(t, model.URL, "agent")
	s.init()
	client, name, err := s.resolveModel("agent")
	if err != nil {
		t.Fatal(err)
	}
	options := solver.Options{Model: name, Client: client, BB: s.BB, Scope: s.Scope,
		ContextWindow: func(string) int { return 64000 }, SessionPath: filepath.Join(s.Settings.StateDir, "chat.jsonl")}
	s.solvers["agent"] = solver.New(options)
	s.runCtx = context.Background()
	if err := s.ChatChecked("agent", message); err != nil {
		t.Fatal(err)
	}
	// New solver reads the durable inbox; no model request is made by upload
	// itself. Only this explicit run consumes it, through the ordinary prompt.
	restored := solver.New(options)
	if result := restored.RunUntilDoneOrGaveUp(context.Background()); result.Status == solver.StatusStorageError {
		t.Fatal(result)
	}
	if !received.Load() {
		t.Fatal("customer text was truncated or missing from the model request")
	}
	if s.CurrentScope().Allows("outside.example") {
		t.Fatal("text input changed authorized scope")
	}
	if err := s.ChatChecked("agent", strings.Repeat("x", MaxOperatorMessageBytes+1)); err == nil || !strings.Contains(err.Error(), "128 KiB") {
		t.Fatalf("oversized input must return an actionable error: %v", err)
	}
	if s.status["agent"] == solver.StatusStorageError {
		t.Fatal("rejected input marked healthy executor as a storage failure")
	}
}
