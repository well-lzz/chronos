// Package notify delivers optional, operator-configured webhook events.
package notify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/well-lzz/chronos/internal/durable"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
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

type pendingEvent struct {
	EndpointID string    `json:"endpoint_id"`
	Event      Event     `json:"event"`
	Attempts   int       `json:"attempts"`
	Next       time.Time `json:"next_attempt"`
}

type Notifier struct {
	pending map[string]pendingEvent
	mu      sync.Mutex
	url     string
	path    string
	sent    map[string]bool
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
	n := &Notifier{url: endpoint, path: filepath.Join(stateDir, "notifications.json"), sent: map[string]bool{}, pending: map[string]pendingEvent{}}
	for path, target := range map[string]any{n.path: &n.sent, n.path + ".pending": &n.pending} {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, target); err != nil {
			return nil, fmt.Errorf("read notification state: %w", err)
		}
	}
	if n.sent == nil || n.pending == nil {
		return nil, fmt.Errorf("invalid notification state")
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
	if _, ok := n.pending[key]; !ok {
		if len(n.pending) >= 256 {
			return fmt.Errorf("notification outbox is full")
		}
		event.Time = time.Now().UTC()
		n.pending[key] = pendingEvent{Event: event, EndpointID: n.endpointID()}
		if err := n.savePending(); err != nil {
			delete(n.pending, key)
			return err
		}
	}
	return n.send(ctx, key)
}

func (n *Notifier) savePending() error {
	data, err := json.Marshal(n.pending)
	if err != nil {
		return err
	}
	return durable.WriteFile(n.path+".pending", data)
}

func (n *Notifier) send(ctx context.Context, key string) error {
	item := n.pending[key]
	if item.EndpointID != n.endpointID() {
		return nil
	}
	if n.sent[key] {
		delete(n.pending, key)
		return n.savePending()
	}
	if time.Now().Before(item.Next) {
		return nil
	}
	data, err := json.Marshal(item.Event)
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
	request.Header.Set("Idempotency-Key", key)
	response, sendErr := http.DefaultClient.Do(request)
	if response != nil {
		response.Body.Close()
		if sendErr == nil && (response.StatusCode < 200 || response.StatusCode >= 300) {
			sendErr = fmt.Errorf("webhook HTTP %d", response.StatusCode)
		}
	}
	if sendErr != nil {
		item.Attempts++
		delay := 5 * time.Second * time.Duration(1<<min(item.Attempts-1, 6))
		if delay > 300*time.Second {
			delay = 300 * time.Second
		}
		item.Next = time.Now().Add(delay)
		n.pending[key] = item
		if err := n.savePending(); err != nil {
			return err
		}
		return sendErr
	}
	n.sent[key] = true
	state, err := json.MarshalIndent(n.sent, "", "  ")
	if err == nil {
		err = durable.WriteFile(n.path, state)
	}
	if err != nil {
		delete(n.sent, key)
		return err
	}
	delete(n.pending, key)
	return n.savePending()
}

// Flush retries already-committed notices, including those from earlier runs.
// The endpoint remains operator-configured; source content never selects it.
func (n *Notifier) Flush(ctx context.Context) error {
	if n == nil || n.url == "" {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	var keys []string
	for key := range n.pending {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := n.send(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

func (n *Notifier) Run(ctx context.Context) {
	if n == nil || n.url == "" {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		_ = n.Flush(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (n *Notifier) endpointID() string {
	hash := sha256.Sum256([]byte(n.url))
	return hex.EncodeToString(hash[:])
}
