package solver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/scope"
	"github.com/well-lzz/chronos/internal/traffic"
)

func TestMutatingHTTPRequiresOwnedSurface(t *testing.T) {
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			writes++
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	stateDir := t.TempDir()
	bb, _ := blackboard.LoadOrNew("mutation", stateDir)
	tl, _ := traffic.LoadOrNew("mutation", stateDir)
	s := New(Options{ModelSpec: "model-a", BB: bb, TL: tl, Scope: scope.Parse([]string{u.Hostname()})})
	target := server.URL + "/api/item"
	post := `{"url":"` + target + `","method":"POST","body":"update"}`
	if result := s.toolHTTP(context.Background(), post); !strings.Contains(result, "claim_surface required") || writes != 0 {
		t.Fatalf("unclaimed mutation executed: %q writes=%d", result, writes)
	}
	if result := s.toolClaimSurface(`{"surface":"` + server.URL + `/api"}`); !strings.Contains(result, "claimed") {
		t.Fatalf("claim failed: %s", result)
	}
	if result := s.toolHTTP(context.Background(), post); strings.Contains(result, "BLOCKED") || writes != 1 {
		t.Fatalf("owned mutation failed: %q writes=%d", result, writes)
	}
	peer := New(Options{ModelSpec: "model-b", BB: bb, TL: tl, Scope: scope.Parse([]string{u.Hostname()})})
	if result := peer.toolHTTP(context.Background(), post); !strings.Contains(result, "claim_surface required") || writes != 1 {
		t.Fatalf("peer mutation was not blocked: %q writes=%d", result, writes)
	}
}

func TestMutatingHTTPRedirectRequiresDestinationLease(t *testing.T) {
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/start" {
			http.Redirect(w, r, "/admin/update", http.StatusTemporaryRedirect)
			return
		}
		if r.URL.Path == "/admin/update" && r.Method == http.MethodPost {
			writes++
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	stateDir := t.TempDir()
	bb, _ := blackboard.LoadOrNew("redirect", stateDir)
	tl, _ := traffic.LoadOrNew("redirect", stateDir)
	s := New(Options{ModelSpec: "model-a", BB: bb, TL: tl, Scope: scope.Parse([]string{u.Hostname()})})
	if result := s.toolClaimSurface(`{"surface":"` + server.URL + `/api"}`); !strings.Contains(result, "claimed") {
		t.Fatalf("claim failed: %s", result)
	}
	post := `{"url":"` + server.URL + `/api/start","method":"POST","body":"update"}`
	if result := s.toolHTTP(context.Background(), post); !strings.Contains(result, "claim_surface required") || writes != 0 {
		t.Fatalf("redirected mutation escaped lease: %q writes=%d", result, writes)
	}
	if result := s.toolClaimSurface(`{"surface":"` + server.URL + `/admin"}`); !strings.Contains(result, "claimed") {
		t.Fatalf("destination claim failed: %s", result)
	}
	if result := s.toolHTTP(context.Background(), post); strings.Contains(result, "BLOCKED") || writes != 1 {
		t.Fatalf("redirected mutation with lease failed: %q writes=%d", result, writes)
	}
}
