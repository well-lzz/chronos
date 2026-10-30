package guard

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRulePriorityAndOperatorApproval(t *testing.T) {
	m := &Manager{stateDir: t.TempDir(), pending: map[string]*pendingRequest{}}
	if err := m.SetRules([]Rule{{Tool: "*", Action: "deny", Priority: 1, Reason: "default blocked"},
		{Tool: "http_request", Action: "ask", Priority: 10, Reason: "confirm request"}}); err != nil {
		t.Fatal(err)
	}
	allowedCh := make(chan bool, 1)
	go func() {
		allowed, _ := m.Check(context.Background(), "mission", "executor", "http_request", `{"url":"https://target.test"}`)
		allowedCh <- allowed
	}()
	var pending []Pending
	deadline := time.After(time.Second)
	for len(pending) == 0 {
		select {
		case <-deadline:
			t.Fatal("approval did not become pending")
		default:
			time.Sleep(time.Millisecond)
		}
		pending = m.Pending()
	}
	if pending[0].Tool != "http_request" {
		t.Fatalf("pending: %+v", pending)
	}
	if err := m.Decide(pending[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if !<-allowedCh {
		t.Fatal("approved call was blocked")
	}
	allowed, message := m.Check(context.Background(), "mission", "executor", "tcp_send", "{}")
	if allowed || !strings.Contains(message, "非目标防御") {
		t.Fatalf("deny result: %v %q", allowed, message)
	}
	if len(m.Recent(10)) < 3 {
		t.Fatal("audit did not capture ask, approval and deny")
	}
}
