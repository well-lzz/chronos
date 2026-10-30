package browsermcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Run with CHRONOS_BROWSER_SMOKE=1 after installing Playwright Chromium.
func TestPlaywrightBrowserSmoke(t *testing.T) {
	if os.Getenv("CHRONOS_BROWSER_SMOKE") != "1" {
		t.Skip("opt-in browser integration test")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<title>Chronos scope test</title><h1>Authorized page</h1>"))
	}))
	defer server.Close()
	var allowed atomic.Bool
	allowed.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client, err := Start(ctx, func(host string) bool { return allowed.Load() && host == "127.0.0.1" })
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Call(ctx, "browser_snapshot", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("automatic browser launch: %v", err)
	}
	result, err := client.Call(ctx, "browser_navigate", json.RawMessage(`{"url":"`+server.URL+`/first"}`))
	if err != nil {
		t.Fatalf("navigation: %v %q", err, result)
	}
	result, err = client.Call(ctx, "browser_snapshot", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(result, "Authorized page") {
		t.Fatalf("snapshot: %v %q", err, result)
	}
	allowed.Store(false)
	result, err = client.Call(ctx, "browser_navigate", json.RawMessage(`{"url":"`+server.URL+`/second"}`))
	if err == nil && strings.Contains(result, "Authorized page") {
		t.Fatalf("browser escaped narrowed scope: %q", result)
	}
}
