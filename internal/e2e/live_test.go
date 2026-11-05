// Package e2e contains opt-in, billed real-provider tests against a fixture
// owned by this process. Ordinary go test never calls an external model.
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/config"
	"github.com/well-lzz/chronos/internal/durable"
	"github.com/well-lzz/chronos/internal/llm"
	"github.com/well-lzz/chronos/internal/llmconfig"
	"github.com/well-lzz/chronos/internal/mission"
	"github.com/well-lzz/chronos/internal/report"
	"github.com/well-lzz/chronos/internal/scope"
	"github.com/well-lzz/chronos/internal/solver"
	"github.com/well-lzz/chronos/internal/traffic"
)

func integer(t *testing.T, key string, fallback, max int) int {
	t.Helper()
	if os.Getenv(key) == "" {
		return fallback
	}
	n, err := strconv.Atoi(os.Getenv(key))
	if err != nil || n < 1 || n > max {
		t.Fatalf("%s must be between 1 and %d", key, max)
	}
	return n
}

func TestLiveModelEvidenceLifecycle(t *testing.T) {
	if os.Getenv("CHRONOS_RUN_LIVE_E2E") != "1" {
		t.Skip("opt-in: CHRONOS_RUN_LIVE_E2E=1")
	}
	profile := llmconfig.Load(os.Getenv("CHRONOS_LIVE_PROFILE_DIR")).Get()
	client, model, err := llm.FromExactProfileWithSettings(profile, config.Settings{})
	if err != nil {
		t.Fatal("live model profile unavailable (credentials are never logged)")
	}
	rounds := integer(t, "CHRONOS_LIVE_ROUNDS", 1, 100)
	minutes := integer(t, "CHRONOS_LIVE_MINUTES", 5, 120)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(minutes)*time.Minute)
	defer cancel()
	dir := os.Getenv("CHRONOS_LIVE_OUTPUT")
	if dir == "" {
		dir = t.TempDir()
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("live output directory must be empty; preserve prior evidence in its original directory")
	}
	started := time.Now()
	var calls, tokens, hits atomic.Int64
	passed := 0
	// Deliberately excludes request bodies, profile URLs, credentials and raw
	// provider errors. Keep the evidence directory private as normal run state.
	defer func() {
		result := map[string]any{"model": model, "started": started.UTC(), "duration_seconds": time.Since(started).Seconds(), "requested_rounds": rounds, "passed_rounds": passed, "provider_calls": calls.Load(), "tokens": tokens.Load(), "fixture_requests": hits.Load(), "passed": !t.Failed(), "target": "process-owned loopback fixture only", "long_duration_proven": false}
		data, _ := json.MarshalIndent(result, "", "  ")
		if err := durable.WriteFile(filepath.Join(dir, "live-result.json"), data); err != nil {
			t.Error(err)
		}
		t.Logf("live result: model=%s rounds=%d/%d calls=%d tokens=%d duration=%.1fs", model, passed, rounds, calls.Load(), tokens.Load(), time.Since(started).Seconds())
	}()
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != "GET" || r.URL.Path != "/fixture" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "CHRONOS_SYNTHETIC_PUBLIC_MARKER_ONLY")
	}))
	defer fixture.Close()
	target := fixture.URL + "/fixture"
	for round := 1; round <= rounds; round++ {
		state := filepath.Join(dir, fmt.Sprintf("round-%03d", round))
		bb, err := blackboard.LoadOrNew("live", state)
		if err != nil {
			t.Fatal(err)
		}
		tl, err := traffic.LoadOrNew("live", state)
		if err != nil {
			t.Fatal(err)
		}
		complete := func(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
			if calls.Load() >= int64(rounds*12) || tokens.Load() >= int64(rounds*50000) {
				return openai.ChatCompletionResponse{}, fmt.Errorf("live budget exhausted")
			}
			allowed := map[string]bool{"http_request": true, "add_fact": true, "add_finding": true, "verify_finding": true, "artifact_get": true, "traffic_get": true, "traffic_search": true, "resolve_subgoal": true}
			tools := make([]openai.Tool, 0, len(allowed))
			for _, tool := range req.Tools {
				if tool.Function != nil && allowed[tool.Function.Name] {
					tools = append(tools, tool)
				}
			}
			req.Tools, req.MaxTokens = tools, 1536
			calls.Add(1)
			resp, err := client.CreateChatCompletion(ctx, req)
			if err != nil {
				var apiErr *openai.APIError
				if errors.As(err, &apiErr) {
					message := strings.ReplaceAll(apiErr.Message, profile.APIKey, "[redacted]")
					message = strings.ReplaceAll(message, profile.BaseURL, "[provider]")
					t.Logf("provider rejected HTTP %d: %.500s", apiErr.HTTPStatusCode, message)
				}
				return openai.ChatCompletionResponse{}, fmt.Errorf("live provider request failed (%T)", err)
			}
			tokens.Add(int64(resp.Usage.TotalTokens))
			for _, choice := range resp.Choices {
				for _, call := range choice.Message.ToolCalls {
					if !allowed[call.Function.Name] {
						t.Logf("fixture rejected unexpected tool %q", call.Function.Name)
						return openai.ChatCompletionResponse{}, fmt.Errorf("live tool boundary denied")
					}
					if call.Function.Name == "http_request" {
						var args struct {
							URL    string `json:"url"`
							Method string `json:"method"`
						}
						if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || args.URL != target || (args.Method != "" && args.Method != "GET") {
							t.Log("fixture rejected an HTTP request outside its exact GET endpoint")
							return openai.ChatCompletionResponse{}, fmt.Errorf("live fixture boundary denied")
						}
					}
				}
			}
			return resp, nil
		}
		meta := mission.Meta{Name: "live", Description: "AUTHORIZED SYNTHETIC PIPELINE TEST. Only GET " + target + " is authorized. No other addresses, paths, methods or tools. This is a deliberately public test marker, NOT a real vulnerability. First call http_request. Record exactly one info severity finding about the public marker with its actual traffic ref as proof, category fixture, reproduction steps, then finish. Never invent traffic IDs."}
		options := solver.Options{ModelSpec: "discovery", Model: model, Client: client, Meta: meta, Scope: scope.Parse([]string{"127.0.0.1"}), BB: bb, TL: tl, Complete: complete, StepLimit: 10, ModelCallTimeout: 45 * time.Second, SessionPath: filepath.Join(state, "discovery.jsonl"), VerificationPeer: func(string, string) string { return "independent-verifier" }}
		origin := solver.New(options)
		result := origin.RunUntilDoneOrGaveUp(ctx)
		findings := bb.FindingsSorted()
		if result.Status != solver.StatusGaveUp || len(findings) != 1 {
			t.Fatalf("round %d discovery incomplete: status=%s findings=%d detail=%.500s", round, result.Status, len(findings), result.Findings)
		}
		options.ModelSpec = "independent-verifier"
		options.SessionPath = filepath.Join(state, "verifier.jsonl")
		options.Meta.Description = "AUTHORIZED SYNTHETIC VERIFICATION TEST. Only GET " + target + " is authorized. Independently replay http_request NOW, then verify_finding for " + findings[0].ID + " as reproduced with the NEW traffic ref, role verification, and an accurate summary. Do not reuse the discoverer's evidence and do not create more findings. The expected body is CHRONOS_SYNTHETIC_PUBLIC_MARKER_ONLY. Finish after verification."
		verifier := solver.New(options)
		result = verifier.RunUntilDoneOrGaveUp(ctx)
		findings = bb.FindingsSorted()
		if result.Status != solver.StatusGaveUp || len(findings) != 1 || len(findings[0].Verifications) == 0 || findings[0].Verifications[0].By != "independent-verifier" || blackboard.Verdict(findings[0].Status) != "confirmed" {
			t.Fatalf("round %d independent verification incomplete: status=%s", round, result.Status)
		}
		if _, err := report.RenderReports(bb, filepath.Join(state, "reports"), report.RunMeta{Mode: "synthetic-live-e2e", Models: []string{model}}); err != nil {
			t.Fatal(err)
		}
		// Reload persisted evidence, rather than only checking the live object.
		restored, err := blackboard.LoadOrNew("live", state)
		if err != nil || len(restored.FindingsSorted()) != 1 || blackboard.Verdict(restored.FindingsSorted()[0].Status) != "confirmed" {
			t.Fatalf("round %d persisted graph did not recover", round)
		}
		passed++
	}
}
