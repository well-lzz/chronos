package swarm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/config"
	"github.com/well-lzz/chronos/internal/llmconfig"
	"github.com/well-lzz/chronos/internal/mission"
	"github.com/well-lzz/chronos/internal/scope"
	"github.com/well-lzz/chronos/internal/solver"
	"github.com/well-lzz/chronos/internal/traffic"
)

func executionSwarm(t *testing.T, endpoint string, specs ...string) *Swarm {
	t.Helper()
	dir := t.TempDir()
	bb, err := blackboard.LoadOrNew("execution", dir)
	if err != nil {
		t.Fatal(err)
	}
	tl, err := traffic.LoadOrNew("execution", dir)
	if err != nil {
		t.Fatal(err)
	}
	profiles := map[string]llmconfig.Profile{}
	for _, spec := range specs {
		profiles[spec] = llmconfig.Profile{Model: spec, BaseURL: endpoint, APIKey: "test"}
	}
	return &Swarm{Meta: mission.Meta{Name: "execution", Description: "Authorized local fixture"}, Scope: scope.Parse([]string{"127.0.0.1"}), BB: bb, TL: tl, ModelSpecs: specs, ModelProfiles: profiles,
		Settings: config.Settings{StateDir: dir, MaxBumps: 1, ModelCallTimeoutS: 15, MaxRunToolSteps: 64}}
}

func reply(w http.ResponseWriter, message openai.ChatCompletionMessage) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{ID: "fixture", Choices: []openai.ChatCompletionChoice{{Message: message}}, Usage: openai.Usage{TotalTokens: 10, PromptTokens: 5, CompletionTokens: 5}})
}
func call(name string, args any) openai.ChatCompletionMessage {
	data, _ := json.Marshal(args)
	return openai.ChatCompletionMessage{Role: "assistant", ToolCalls: []openai.ToolCall{{ID: name, Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: name, Arguments: string(data)}}}}
}
func final() openai.ChatCompletionMessage {
	return openai.ChatCompletionMessage{Role: "assistant", Content: "Fixture work recorded."}
}
func lastTraffic(req openai.ChatCompletionRequest) string {
	re := regexp.MustCompile(`\[traffic (t[0-9]+)`)
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if match := re.FindStringSubmatch(req.Messages[i].Content); len(match) > 1 {
			return match[1]
		}
	}
	return ""
}

func TestSwarmMainLoopRecordsRealTrafficAndIndependentVerification(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { targetHits.Add(1); fmt.Fprint(w, "fixture-marker") }))
	defer target.Close()
	var s *Swarm
	ready := make(chan struct{})
	var readyOnce sync.Once
	var mu sync.Mutex
	calls := map[string]int{}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openai.ChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		turn := calls[req.Model]
		calls[req.Model]++
		mu.Unlock()
		if req.Model == "origin" {
			switch turn {
			case 0:
				reply(w, call("http_request", map[string]any{"url": target.URL}))
				return
			case 1:
				reply(w, call("add_finding", map[string]any{"title": "Synthetic public marker", "severity": "info", "target": target.URL, "category": "fixture", "evidence": "HTTP 200 fixture-marker", "reproduction": "1. GET " + target.URL + "\nExpected: fixture-marker", "evidence_refs": []map[string]string{{"ref": lastTraffic(req), "role": "proof"}}}))
				return
			default:
				readyOnce.Do(func() { close(ready) })
			}
		} else {
			select {
			case <-ready:
			case <-r.Context().Done():
				return
			}
			switch turn {
			case 0:
				reply(w, call("http_request", map[string]any{"url": target.URL}))
				return
			case 1:
				findings := s.BB.FindingsSorted()
				if len(findings) == 0 {
					t.Error("finding not shared")
					reply(w, final())
					return
				}
				reply(w, call("verify_finding", map[string]any{"finding_id": findings[0].ID, "verdict": "reproduced", "summary": "independent replay", "evidence": "HTTP 200 fixture-marker", "evidence_refs": []map[string]string{{"ref": lastTraffic(req), "role": "verification"}}}))
				return
			}
		}
		reply(w, final())
	}))
	defer model.Close()
	s = executionSwarm(t, model.URL, "origin", "peer")
	// Race/coverage builds plus durable fsync can be slow on shared CI hosts.
	// Keep the behavioral assertions, but do not confuse host load with a hang.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.Run(ctx)
	if ctx.Err() != nil {
		t.Fatal("execution did not terminate")
	}
	if err := s.Failure(); err != nil {
		t.Fatal(err)
	}
	findings := s.BB.FindingsSorted()
	if len(findings) != 1 || len(findings[0].Verifications) != 1 || findings[0].Verifications[0].By != "peer" {
		t.Fatalf("independent verification missing: %+v", findings)
	}
	if targetHits.Load() != 2 || s.TotalTokens() == 0 {
		t.Fatalf("traffic/tokens missing: %d/%d", targetHits.Load(), s.TotalTokens())
	}
	if len(s.BB.OpenSubgoals()) != 0 {
		t.Fatal("verification did not resolve its goal")
	}
	for _, spec := range []string{"origin", "peer"} {
		if s.status[spec] != "finished" {
			t.Fatalf("%s status %s", spec, s.status[spec])
		}
		agent := s.Status()["agents"].(map[string]any)[spec].(map[string]any)
		tools := agent["tools"].(map[string]int)
		if agent["tool_calls"] != 2 || tools["http_request"] != 1 {
			t.Fatalf("%s executed tools but console stats disagree: %+v", spec, agent)
		}
		other := "add_finding"
		if spec == "peer" {
			other = "verify_finding"
		}
		if tools[other] != 1 || len(tools) != 2 {
			t.Fatalf("%s per-tool stats disagree: %+v", spec, tools)
		}
	}
}

func TestSwarmProviderRetryAndFatalFailure(t *testing.T) {
	for _, status := range []int{429, 400} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if status == 400 || calls.Add(1) == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					fmt.Fprint(w, `{"error":{"message":"fixture error","type":"fixture"}}`)
					return
				}
				reply(w, final())
			}))
			defer model.Close()
			s := executionSwarm(t, model.URL, "agent")
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			s.Run(ctx)
			if ctx.Err() != nil || s.status["agent"] != "finished" {
				t.Fatalf("status %s: %v", s.status["agent"], ctx.Err())
			}
			if status == 429 && s.TotalTokens() == 0 {
				t.Fatal("provider did not recover")
			}
			foundError := false
			for _, event := range s.Activity("agent", 0) {
				if event.Kind == "error" {
					foundError = true
				}
			}
			if !foundError {
				t.Fatal("provider failure not surfaced")
			}
		})
	}
}

func TestSwarmAuthFailureWaitsForConfigAndCancels(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"message":"denied","type":"fixture"}}`)
	}))
	defer model.Close()
	s := executionSwarm(t, model.URL, "agent")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		status := s.status["agent"]
		s.mu.Unlock()
		if status == "needs-config" {
			cancel()
			<-done
			return
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	t.Fatal("auth failure did not enter needs-config")
}

func TestSwarmScopeChangeBlocksStaleModelToolCall(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); fmt.Fprint(w, "fixture") }))
	defer target.Close()
	boundary, release := make(chan struct{}), make(chan struct{})
	var turns atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch turns.Add(1) {
		case 1:
			reply(w, call("http_request", map[string]any{"url": target.URL}))
		case 2:
			close(boundary)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			reply(w, call("http_request", map[string]any{"url": target.URL}))
		default:
			reply(w, final())
		}
	}))
	defer model.Close()
	s := executionSwarm(t, model.URL, "agent")
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	select {
	case <-boundary:
	case <-ctx.Done():
		t.Fatal("model boundary not reached")
	}
	s.UpdateScope(scope.Parse([]string{"new.test"}))
	close(release)
	<-done
	if hits.Load() != 1 || s.CurrentScope().Allows("127.0.0.1") {
		t.Fatalf("stale tool bypassed scope: hits=%d", hits.Load())
	}
	blocked := false
	for _, e := range s.Activity("agent", 0) {
		if strings.Contains(e.Text, "scope lock") {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("scope denial not visible")
	}
}

func TestSwarmBrokenCheckpointNeverContactsModel(t *testing.T) {
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); reply(w, final()) }))
	defer model.Close()
	s := executionSwarm(t, model.URL, "agent")
	dir := filepath.Dir(s.sessionPath("agent"))
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	s.Run(context.Background())
	if calls.Load() != 0 || s.Failure() == nil {
		t.Fatal("broken checkpoint allowed model execution")
	}
}

func TestVerificationRoutingSkipsFailedPeers(t *testing.T) {
	s := &Swarm{ModelSpecs: []string{"origin", "broken", "healthy"}, status: map[string]string{"origin": "running", "broken": solver.StatusStorageError, "healthy": "running"}}
	if got := s.routeVerification("origin", "v1"); got != "healthy" {
		t.Fatalf("selected unavailable verifier: %s", got)
	}
	s.status["healthy"] = "finished"
	if got := s.routeVerification("origin", "v1"); got != "origin" {
		t.Fatalf("single executor fallback: %s", got)
	}
}
