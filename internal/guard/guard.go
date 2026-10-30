// Package guard applies operator rules before a model tool call runs. Scope
// enforcement remains a separate, higher-priority built-in boundary.
package guard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const ringSize = 2000

type Rule struct {
	Tool             string `json:"tool"`
	ArgumentContains string `json:"argument_contains,omitempty"`
	Action           string `json:"action"` // allow, deny, ask
	Priority         int    `json:"priority,omitempty"`
	Reason           string `json:"reason,omitempty"`
}

type Pending struct {
	ID        string    `json:"id"`
	Mission   string    `json:"mission"`
	Executor  string    `json:"executor"`
	Tool      string    `json:"tool"`
	Arguments string    `json:"arguments"`
	Reason    string    `json:"reason"`
	Created   time.Time `json:"created"`
	Deadline  time.Time `json:"deadline"`
}

type Audit struct {
	Time     time.Time `json:"time"`
	Mission  string    `json:"mission"`
	Executor string    `json:"executor"`
	Tool     string    `json:"tool"`
	Action   string    `json:"action"`
	Reason   string    `json:"reason,omitempty"`
	Pending  string    `json:"pending,omitempty"`
}

type pendingRequest struct {
	Pending
	decision chan bool
}

type Manager struct {
	mu       sync.Mutex
	stateDir string
	rules    []Rule
	pending  map[string]*pendingRequest
	audit    []Audit
	loadErr  error
}

var shared sync.Map

func Shared(stateDir, rulesFile string) *Manager {
	key := filepath.Clean(stateDir) + "\x00" + rulesFile
	if current, ok := shared.Load(key); ok {
		return current.(*Manager)
	}
	m := &Manager{stateDir: stateDir, pending: map[string]*pendingRequest{}}
	if rulesFile != "" {
		data, err := os.ReadFile(rulesFile)
		if err == nil {
			var file struct {
				Rules []Rule `json:"rules"`
			}
			err = json.Unmarshal(data, &file)
			if err == nil {
				err = m.SetRules(file.Rules)
			}
		}
		m.loadErr = err
	}
	actual, _ := shared.LoadOrStore(key, m)
	return actual.(*Manager)
}

func (m *Manager) LoadError() error { m.mu.Lock(); defer m.mu.Unlock(); return m.loadErr }

func (m *Manager) SetRules(rules []Rule) error {
	for _, rule := range rules {
		if strings.TrimSpace(rule.Tool) == "" || (rule.Action != "allow" && rule.Action != "deny" && rule.Action != "ask") {
			return fmt.Errorf("invalid tool rule: tool and action (allow/deny/ask) are required")
		}
	}
	copyRules := append([]Rule(nil), rules...)
	sort.SliceStable(copyRules, func(i, j int) bool { return copyRules[i].Priority > copyRules[j].Priority })
	m.mu.Lock()
	m.rules = copyRules
	m.mu.Unlock()
	return nil
}

func (m *Manager) Rules() []Rule {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Rule(nil), m.rules...)
}

// Check blocks for an ask decision, timeout, or caller cancellation. The
// returned text is suitable for the model's tool result and distinguishes
// platform policy from target behavior.
func (m *Manager) Check(ctx context.Context, mission, executor, tool, arguments string) (bool, string) {
	m.mu.Lock()
	var matched *Rule
	for i := range m.rules {
		rule := &m.rules[i]
		if (rule.Tool == "*" || rule.Tool == tool) && (rule.ArgumentContains == "" || strings.Contains(arguments, rule.ArgumentContains)) {
			copyRule := *rule
			matched = &copyRule
			break
		}
	}
	m.mu.Unlock()
	if matched == nil || matched.Action == "allow" {
		m.record(Audit{Time: time.Now().UTC(), Mission: mission, Executor: executor, Tool: tool, Action: "allow"})
		return true, ""
	}
	reason := strings.TrimSpace(matched.Reason)
	if reason == "" {
		reason = "operator tool policy"
	}
	if matched.Action == "deny" {
		m.record(Audit{Time: time.Now().UTC(), Mission: mission, Executor: executor, Tool: tool, Action: "deny", Reason: reason})
		return false, "【平台管控·非目标防御】此操作被禁止：" + reason
	}
	var idBytes [12]byte
	_, _ = rand.Read(idBytes[:])
	id := hex.EncodeToString(idBytes[:])
	request := &pendingRequest{Pending: Pending{ID: id, Mission: mission, Executor: executor, Tool: tool,
		Arguments: arguments, Reason: reason, Created: time.Now().UTC(), Deadline: time.Now().Add(2 * time.Minute)}, decision: make(chan bool, 1)}
	m.mu.Lock()
	m.pending[id] = request
	m.mu.Unlock()
	m.record(Audit{Time: time.Now().UTC(), Mission: mission, Executor: executor, Tool: tool, Action: "ask", Reason: reason, Pending: id})
	timer := time.NewTimer(time.Until(request.Deadline))
	defer timer.Stop()
	allowed := false
	decision := "timeout"
	select {
	case allowed = <-request.decision:
		if allowed {
			decision = "approved"
		} else {
			decision = "rejected"
		}
	case <-timer.C:
	case <-ctx.Done():
		decision = "cancelled"
	}
	m.mu.Lock()
	delete(m.pending, id)
	m.mu.Unlock()
	m.record(Audit{Time: time.Now().UTC(), Mission: mission, Executor: executor, Tool: tool, Action: decision, Reason: reason, Pending: id})
	if allowed && ctx.Err() == nil {
		return true, ""
	}
	return false, "【平台管控·非目标防御】此操作未获批准（" + decision + "）：" + reason
}

func (m *Manager) Decide(id string, approve bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	request := m.pending[id]
	if request == nil {
		return fmt.Errorf("approval %q is no longer pending", id)
	}
	select {
	case request.decision <- approve:
		delete(m.pending, id)
		return nil
	default:
		return fmt.Errorf("approval %q has already been decided", id)
	}
}

func (m *Manager) Pending() []Pending {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Pending, 0, len(m.pending))
	for _, request := range m.pending {
		out = append(out, request.Pending)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

func (m *Manager) Recent(limit int) []Audit {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > ringSize {
		limit = 100
	}
	if limit > len(m.audit) {
		limit = len(m.audit)
	}
	return append([]Audit(nil), m.audit[len(m.audit)-limit:]...)
}

func (m *Manager) BuiltinDeny(mission, executor, tool, reason string) {
	m.record(Audit{Time: time.Now().UTC(), Mission: mission, Executor: executor, Tool: tool, Action: "deny", Reason: reason})
}

func (m *Manager) Post(mission, executor, tool, result string) {
	action := "ok"
	if strings.HasPrefix(result, "【平台管控·非目标防御】") {
		action = "blocked"
	} else if strings.Contains(strings.ToLower(result), " error:") || strings.HasPrefix(strings.ToLower(result), "error:") {
		action = "error"
	}
	m.record(Audit{Time: time.Now().UTC(), Mission: mission, Executor: executor, Tool: tool, Action: action})
}

func (m *Manager) record(row Audit) {
	m.mu.Lock()
	m.audit = append(m.audit, row)
	if len(m.audit) > ringSize {
		m.audit = append([]Audit(nil), m.audit[len(m.audit)-ringSize:]...)
	}
	if m.stateDir != "" {
		data, err := json.Marshal(row)
		if err == nil && os.MkdirAll(m.stateDir, 0o700) == nil {
			if file, err := os.OpenFile(filepath.Join(m.stateDir, "tool_audit.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				_, _ = file.Write(append(data, '\n'))
				_ = file.Close()
			}
		}
	}
	m.mu.Unlock()
}
