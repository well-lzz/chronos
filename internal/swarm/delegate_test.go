package swarm

import (
	"testing"

	"github.com/well-lzz/chronos/internal/solver"
)

func TestChildActivityIsIsolatedAndLinked(t *testing.T) {
	s := &Swarm{activity: map[string][]ActEvent{}}
	s.emit("root-model", 2, "delegate_spawn", "child-1", "delegated task")
	s.childRecorder("root-model")(solver.ChildEvent{AgentID: "child-1", Step: 1, Kind: "notify", Text: "child progress"})
	s.childRecorder("root-model")(solver.ChildEvent{AgentID: "child-1", Step: 2, Kind: "delegate_spawn", Tool: "grandchild-1", Text: "nested task"})
	parent := s.Activity("root-model", 0)
	child := s.Activity("child-1", 0)
	if len(parent) != 1 || parent[0].AgentID != "child-1" || len(child) != 2 || child[0].Kind != "notify" || child[0].ParentModel != "root-model" || child[1].AgentID != "grandchild-1" || child[1].ParentID != "child-1" {
		t.Fatalf("delegation trace mismatch: parent=%v child=%v", parent, child)
	}
	if len(s.Activity("child-1", int(child[1].Seq))) != 0 {
		t.Fatal("child activity cursor returned old event")
	}
}
