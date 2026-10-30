package scheduler

import (
	"testing"
	"time"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/config"
	"github.com/well-lzz/chronos/internal/mission"
	"github.com/well-lzz/chronos/internal/swarm"
)

func TestSchedulerCheckpointRestoresDeferredAndActiveWindow(t *testing.T) {
	cfg := config.Settings{StateDir: t.TempDir(), MaxConcurrent: 2}
	first := New(nil, cfg, []string{"model-a"})
	deadline := time.Now().Add(9 * time.Minute).UTC().Truncate(time.Second)
	first.runStart = time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	first.attempts["mission-a"] = 2
	first.deferred["mission-b"] = "cooldown"
	first.deferTime["mission-b"] = time.Now().UTC().Truncate(time.Second)
	first.runs["mission-a"] = &run{deadline: deadline}
	first.runIDs["mission-a"] = "isolated-run-a"
	first.recordDecision("defer", "mission-b", "cooldown", "waiting")
	if err := first.checkpoint(); err != nil {
		t.Fatal(err)
	}
	restored := New(nil, cfg, []string{"model-a"})
	if restored.recoveryErr != nil || restored.attempts["mission-a"] != 2 || restored.deferred["mission-b"] != "cooldown" ||
		!restored.resumePending["mission-a"].Equal(deadline) || !restored.runStart.Equal(first.runStart) ||
		len(restored.events) != 1 || restored.events[0].Kind != "defer" || restored.runIDs["mission-a"] != "isolated-run-a" {
		t.Fatalf("scheduler state not restored: error=%v attempts=%v deferred=%v resume=%v", restored.recoveryErr, restored.attempts, restored.deferred, restored.resumePending)
	}
	if err := restored.clearState(); err != nil {
		t.Fatal(err)
	}
	if next := New(nil, cfg, []string{"model-a"}); len(next.resumePending)+len(next.deferred) != 0 {
		t.Fatal("completed scheduler state was not cleared")
	}
}

func TestSnapshotIncludesEvidenceGraphPerMission(t *testing.T) {
	board, err := blackboard.LoadOrNew("mission-a", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := board.AddFinding(blackboard.Finding{
		Title: "Graph finding", Severity: "high",
		EvidenceRefs: []blackboard.EvidenceRef{{Ref: "t1", Role: "proof"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduler := New(nil, config.Settings{StateDir: t.TempDir()}, []string{"model-a"})
	scheduler.runs["mission-a"] = &run{
		sw: &swarm.Swarm{BB: board}, meta: mission.Meta{Name: "mission-a"},
	}
	graphs, ok := scheduler.Snapshot()["evidence_graphs"].(map[string]blackboard.GraphSnapshot)
	if !ok || len(graphs) != 1 {
		t.Fatalf("evidence graphs missing: %#v", scheduler.Snapshot()["evidence_graphs"])
	}
	graph := graphs["mission-a"]
	if len(graph.Claims) != 1 || graph.Claims[0].ID != id || len(graph.Evidence) != 1 || len(graph.Edges) != 1 {
		t.Fatalf("unexpected mission graph: %#v", graph)
	}
}
