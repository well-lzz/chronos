package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestPendingWebhookRecoversWithSameIdempotencyKey(t *testing.T) {
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		if len(keys) == 1 {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	n, err := Shared(dir, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.NotifyOnce(context.Background(), "done", Event{Kind: "mission_complete", Mission: "run"}); err == nil {
		t.Fatal("expected temporary failure")
	}
	shared.Delete(filepath.Clean(dir) + "\x00" + server.URL)
	restored, err := Shared(dir, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.pending) != 1 {
		t.Fatal("pending notification lost")
	}
	for key, item := range restored.pending {
		item.Next = time.Time{}
		restored.pending[key] = item
	}
	if err := restored.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] || len(restored.pending) != 0 {
		t.Fatalf("invalid retries: %v", keys)
	}
	if err := restored.NotifyOnce(context.Background(), "done", Event{}); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatal("sent notification repeated")
	}
}
