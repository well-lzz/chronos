package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestWebhookSendsOnceAndPersistsDedup(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	dir := t.TempDir()
	n, err := Shared(dir, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := n.NotifyOnce(context.Background(), "finding:m1:v1", Event{Kind: "high_severity_finding", Mission: "m1"}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("duplicate webhook calls: %d", calls)
	}
	if info, err := os.Stat(filepath.Join(dir, "notifications.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("notification state permissions: %v %v", info, err)
	}
}
