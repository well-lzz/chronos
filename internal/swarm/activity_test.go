package swarm

import (
	"testing"

	"github.com/well-lzz/chronos/internal/blackboard"
)

func TestActivityJournalRestoresConversationAfterRestart(t *testing.T) {
	board, err := blackboard.LoadOrNew("activity-run", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := &Swarm{BB: board, ModelSpecs: []string{"agent"}}
	first.init()
	first.emit("agent", 1, "operator", "", "inspect login")
	first.emit("agent", 2, "tool_call", "http_request", "GET /login")
	second := &Swarm{BB: board, ModelSpecs: []string{"agent"}}
	second.init()
	events := second.Activity("agent", 0)
	if len(events) != 2 || events[0].Text != "inspect login" || events[1].Tool != "http_request" {
		t.Fatalf("activity not restored: %+v", events)
	}
	if got := second.Status()["agents"].(map[string]any)["agent"].(map[string]any)["tool_calls"]; got != 1 {
		t.Fatalf("tool count not restored: %v", got)
	}
}
