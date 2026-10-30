// Package notify delivers optional, operator-configured webhook events.
package notify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Event struct {
	Kind      string    `json:"kind"`
	Mission   string    `json:"mission"`
	FindingID string    `json:"finding_id,omitempty"`
	Title     string    `json:"title,omitempty"`
	Severity  string    `json:"severity,omitempty"`
	Time      time.Time `json:"time"`
}

type Notifier struct {
	mu   sync.Mutex
	url  string
	path string
	sent map[string]bool
}

var shared sync.Map

func Shared(stateDir, endpoint string) (*Notifier, error) {
	if endpoint != "" {
		u, err := url.Parse(endpoint)
		if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, fmt.Errorf("CHRONOS_WEBHOOK_URL must be an absolute HTTP(S) URL")
		}
	}
	key := filepath.Clean(stateDir) + "\x00" + endpoint
	if current, ok := shared.Load(key); ok {
		return current.(*Notifier), nil
	}
	n := &Notifier{url: endpoint, path: filepath.Join(stateDir, "notifications.json"), sent: map[string]bool{}}
	if data, err := os.ReadFile(n.path); err == nil {
		_ = json.Unmarshal(data, &n.sent)
	}
	actual, _ := shared.LoadOrStore(key, n)
	return actual.(*Notifier), nil
}

func (n *Notifier) NotifyOnce(ctx context.Context, key string, event Event) error {
	if n == nil || n.url == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(n.url + "\x00" + key))
	key = hex.EncodeToString(sum[:])
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.sent[key] {
		return nil
	}
	event.Time = time.Now().UTC()
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(callCtx, http.MethodPost, n.url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	_ = response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("webhook HTTP %d", response.StatusCode)
	}
	n.sent[key] = true
	rollback := func(err error) error { delete(n.sent, key); return err }
	if err := os.MkdirAll(filepath.Dir(n.path), 0o700); err != nil {
		return rollback(err)
	}
	state, err := json.MarshalIndent(n.sent, "", "  ")
	if err != nil {
		return rollback(err)
	}
	tmp := n.path + ".tmp"
	if err := os.WriteFile(tmp, state, 0o600); err != nil {
		return rollback(err)
	}
	if err := os.Rename(tmp, n.path); err != nil {
		_ = os.Remove(tmp)
		return rollback(err)
	}
	return nil
}
