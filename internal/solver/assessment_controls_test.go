package solver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/guard"
	"github.com/well-lzz/chronos/internal/scope"
	"github.com/well-lzz/chronos/internal/traffic"
)

func TestAssessmentToolsDenyScopeBeforeOperatorApproval(t *testing.T) {
	for _, tool := range []string{"race", "xss_check", "ssrf_check", "lfi_check"} {
		t.Run(tool, func(t *testing.T) {
			dir := t.TempDir()
			manager := guard.Shared(dir, "")
			if err := manager.SetRules([]guard.Rule{{Tool: "*", Action: "ask"}}); err != nil {
				t.Fatal(err)
			}
			board, err := blackboard.LoadOrNew("scope-before-approval", dir)
			if err != nil {
				t.Fatal(err)
			}
			s := New(Options{ModelSpec: "executor", BB: board, Guard: manager,
				Scope: scope.Parse([]string{"allowed.example"})})
			args := `{"url":"https://outside.example/","param":"input","target":"https://allowed.example/"}`
			if tool == "ssrf_check" {
				args = `{"url":"https://allowed.example/fetch","param":"input","target":"https://outside.example/"}`
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			result := s.dispatch(ctx, tool, args)
			if !strings.Contains(result, "scope lock") {
				t.Fatalf("scope denial was lost: %s", result)
			}
			rows := manager.Recent(10)
			if len(rows) != 1 || rows[0].Action != "deny" || len(manager.Pending()) != 0 {
				t.Fatalf("out-of-scope tool reached approval: %+v", rows)
			}
		})
	}
}

func TestAssessmentMutationsRequireClaims(t *testing.T) {
	for _, tool := range []string{"race", "xss_check", "ssrf_check", "lfi_check"} {
		t.Run(tool, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = w.Write([]byte("constant response"))
			}))
			defer server.Close()
			s := assessmentSolver(t, server.URL)
			args, err := json.Marshal(map[string]any{"url": server.URL + "/api/check", "method": "POST",
				"param": "input", "target": server.URL + "/internal", "count": 2})
			if err != nil {
				t.Fatal(err)
			}
			if result := s.dispatch(context.Background(), tool, string(args)); !strings.Contains(result, "claim_surface required") || calls.Load() != 0 {
				t.Fatalf("unclaimed mutation reached target: %s calls=%d", result, calls.Load())
			}
			if result := s.toolClaimSurface(`{"surface":"` + server.URL + `/api"}`); !strings.Contains(result, "claimed") {
				t.Fatal(result)
			}
			if result := s.dispatch(context.Background(), tool, string(args)); strings.Contains(result, "BLOCKED") || calls.Load() == 0 {
				t.Fatalf("claimed mutation did not run: %s calls=%d", result, calls.Load())
			}
		})
	}
}

func TestAssessmentRedirectsRequireDestinationClaims(t *testing.T) {
	for _, tool := range []string{"race", "xss_check"} {
		t.Run(tool, func(t *testing.T) {
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/start" {
					http.Redirect(w, r, "/admin/update", http.StatusTemporaryRedirect)
					return
				}
				writes.Add(1)
				_, _ = w.Write([]byte("constant response"))
			}))
			defer server.Close()
			s := assessmentSolver(t, server.URL)
			args := `{"url":"` + server.URL + `/api/start","method":"POST","param":"input","count":2}`
			if result := s.toolClaimSurface(`{"surface":"` + server.URL + `/api"}`); !strings.Contains(result, "claimed") {
				t.Fatal(result)
			}
			s.dispatch(context.Background(), tool, args)
			if writes.Load() != 0 {
				t.Fatal("redirected mutation escaped its claimed endpoint family")
			}
			if result := s.toolClaimSurface(`{"surface":"` + server.URL + `/admin"}`); !strings.Contains(result, "claimed") {
				t.Fatal(result)
			}
			s.dispatch(context.Background(), tool, args)
			if writes.Load() == 0 {
				t.Fatal("claimed redirect destination did not receive requests")
			}
		})
	}
}

func assessmentSolver(t *testing.T, target string) *Solver {
	t.Helper()
	dir := t.TempDir()
	board, err := blackboard.LoadOrNew("assessment-controls", dir)
	if err != nil {
		t.Fatal(err)
	}
	log, err := traffic.LoadOrNew("assessment-controls", dir)
	if err != nil {
		t.Fatal(err)
	}
	return New(Options{ModelSpec: "executor", BB: board, TL: log, Scope: scope.Parse([]string{target})})
}
