package solver

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/scope"
	"github.com/well-lzz/chronos/internal/traffic"
)

func TestKilledProcessDoesNotReplayExternalAction(t *testing.T) {
	if dir := os.Getenv("CHRONOS_CRASH_TEST_DIR"); dir != "" {
		bb, err := blackboard.LoadOrNew("crash", dir)
		if err != nil {
			os.Exit(2)
		}
		tl, err := traffic.LoadOrNew("crash", dir)
		if err != nil {
			os.Exit(2)
		}
		s := New(Options{SessionPath: filepath.Join(dir, "session.jsonl"), BB: bb, TL: tl, Scope: scope.Parse([]string{"127.0.0.1"})})
		if err := s.start(); err != nil {
			os.Exit(2)
		}
		call := openai.ToolCall{ID: "external", Function: openai.FunctionCall{Name: "http_request", Arguments: `{"url":"` + os.Getenv("CHRONOS_CRASH_TEST_URL") + `"}`}}
		s.appendMessage(openai.ChatCompletionMessage{Role: "assistant", ToolCalls: []openai.ToolCall{call}})
		result := s.dispatchCall(context.Background(), call)
		if !strings.Contains(result, "traffic") {
			os.Exit(3)
		}
		// The external effect and traffic are committed, but the tool result has
		// not entered the session. The parent kills this exact crash window.
		fmt.Println("ready-to-kill")
		for {
			time.Sleep(time.Second)
		}
	}
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); fmt.Fprint(w, "observed") }))
	defer server.Close()
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestKilledProcessDoesNotReplayExternalAction$")
	cmd.Env = append(os.Environ(), "CHRONOS_CRASH_TEST_DIR="+dir, "CHRONOS_CRASH_TEST_URL="+server.URL)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if !strings.Contains(line, "ready-to-kill") {
			t.Fatalf("child failed before crash point: %s", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child did not reach crash point")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	bb, err := blackboard.LoadOrNew("crash", dir)
	if err != nil {
		t.Fatal(err)
	}
	tl, err := traffic.LoadOrNew("crash", dir)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{SessionPath: filepath.Join(dir, "session.jsonl"), BB: bb, TL: tl, Scope: scope.Parse([]string{"127.0.0.1"})})
	if err := s.start(); err != nil {
		t.Fatal(err)
	}
	msgs := s.messageSnapshot()
	if !pairingValid(msgs) || !strings.Contains(msgs[len(msgs)-1].Content, "result unknown") {
		t.Fatal("missing unknown tool result")
	}
	// Even an explicit duplicate with the original call identity is refused.
	call := openai.ToolCall{ID: "external", Function: openai.FunctionCall{Name: "http_request", Arguments: `{"url":"` + server.URL + `"}`}}
	if got := s.dispatchCall(context.Background(), call); !strings.Contains(got, "already started") {
		t.Fatal(got)
	}
	if hits.Load() != 1 {
		t.Fatalf("external action replayed: %d", hits.Load())
	}
}

func TestModelCallTimeoutIsProviderFailureNotExplorationStall(t *testing.T) {
	bb, _ := blackboard.LoadOrNew("timeout", t.TempDir())
	s := New(Options{BB: bb, Client: openai.NewClient("test"), ModelCallTimeout: 10 * time.Millisecond, Complete: func(ctx context.Context, _ openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
		<-ctx.Done()
		return openai.ChatCompletionResponse{}, ctx.Err()
	}})
	if got := s.RunUntilDoneOrGaveUp(context.Background()); got.Status != StatusRetryableError {
		t.Fatalf("timeout misclassified: %+v", got)
	}
	if s.Health()["model_started_unix"].(int64) != 0 {
		t.Fatal("model health state not cleared")
	}
}
