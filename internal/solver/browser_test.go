package solver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/well-lzz/chronos/internal/scope"
)

func TestBrowserScopeUpdateStopsSession(t *testing.T) {
	if os.Getenv("CHRONOS_BROWSER_SMOKE") != "1" {
		t.Skip("opt-in browser integration test")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<h1>Mission target</h1>"))
	}))
	defer server.Close()
	s := New(Options{Scope: scope.Parse([]string{"127.0.0.1"}), PlaywrightMCP: true})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	defer s.CloseBrowser()
	if err := s.ensureBrowser(ctx); err != nil {
		t.Fatal(err)
	}
	result := s.toolBrowser(ctx, "browser_navigate", `{"url":"`+server.URL+`"}`)
	if !strings.Contains(result, "Mission target") {
		t.Fatalf("browser did not return page snapshot: %s", result)
	}
	s.SetScope(scope.Parse([]string{"other.test"}))
	if s.browser != nil {
		t.Fatal("scope update retained old browser tunnel")
	}
	if got := s.preToolScope("browser_navigate", `{"url":"`+server.URL+`"}`); !strings.Contains(got, "BLOCKED") {
		t.Fatalf("scope update allowed old target: %s", got)
	}
}
