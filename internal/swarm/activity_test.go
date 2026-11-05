package swarm

import (
	"context"
	"testing"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/solver"
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

func TestTypedToolActivityStatsSurviveRingAndRestart(t *testing.T) {
	dir := t.TempDir()
	board, err := blackboard.LoadOrNew("typed-activity-run", dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &Swarm{BB: board, ModelSpecs: []string{"agent"}}
	s.init()
	record := s.loopRecorder("agent")
	for i := 0; i < activityRing+1; i++ {
		record(solver.LoopEvent{Kind: "tool_call", Tool: "http_request", ToolCallID: "call", Step: i + 1})
		record(solver.LoopEvent{Kind: "tool_result", Tool: "http_request", ToolCallID: "call", Step: i + 1})
	}
	// Legacy, typed, and child events share one accounting path. Results and
	// orchestration messages must not count as additional tool calls.
	s.emit("agent", 0, "tool_call", "add_fact", "")
	s.childRecorder("agent")(solver.ChildEvent{AgentID: "child", Kind: "tool_call", Tool: "traffic_get"})
	s.childRecorder("agent")(solver.ChildEvent{AgentID: "child", Kind: "tool_result", Tool: "traffic_get"})
	s.emit("agent", 0, "delegate_spawn", "child", "")
	check := func(s *Swarm) {
		t.Helper()
		agent := s.Status()["agents"].(map[string]any)["agent"].(map[string]any)
		tools := agent["tools"].(map[string]int)
		if agent["tool_calls"] != activityRing+2 || tools["http_request"] != activityRing+1 || tools["add_fact"] != 1 || len(tools) != 2 {
			t.Fatalf("tool stats do not match actual events: %+v", agent)
		}
		if s.toolCalls["child"] != 1 || s.toolByName["child"]["traffic_get"] != 1 {
			t.Fatalf("child tool calls lost or attributed to parent: %+v", s.toolCalls)
		}
		if len(s.Activity("agent", 0)) != activityRing {
			t.Fatal("live activity ring must stay bounded")
		}
	}
	check(s)
	restored := &Swarm{BB: board, ModelSpecs: []string{"agent"}}
	restored.init()
	check(restored)
	// Exercise the real executor startup too: a stale cycle checkpoint must
	// not zero out the newer activity journal loaded by init. A cancelled
	// context keeps this recovery test entirely offline.
	restored.Settings.StateDir = dir
	if err := restored.saveRuntime("agent", executorRuntime{Status: "interrupted", ToolCalls: 1, Tools: map[string]int{"http_request": 1}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	restored.Run(ctx)
	check(restored)
}
