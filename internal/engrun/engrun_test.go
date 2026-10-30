package engrun

import (
	"context"
	"testing"
	"time"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/mission"
	"github.com/well-lzz/chronos/internal/scope"
	"github.com/well-lzz/chronos/internal/swarm"
)

func TestUpdateScopeReplacesRunnerAndSwarmScope(t *testing.T) {
	dir := t.TempDir()
	oldBoard, err := blackboard.LoadOrNew("test", dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oldBoard.AddFact("old target is reachable", "", "test"); err != nil {
		t.Fatal(err)
	}
	oldSwarm := &swarm.Swarm{Scope: scope.Parse([]string{"old.example"}), BB: oldBoard}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := New(oldSwarm, oldBoard, mission.Meta{Name: "test", ConnectionInfo: "old.example"}, cancel, time.Time{})
	runner.runSwarm = func(ctx context.Context, _ *swarm.Swarm) { <-ctx.Done() }
	var stoppedID, startedID string
	runner.SetOnRunStopped(func(_ *swarm.Swarm, board *blackboard.Blackboard, _ mission.Meta, _ time.Time) {
		stoppedID = board.MissionID
	})
	runner.SetOnRunStarted(func(_ *swarm.Swarm, board *blackboard.Blackboard, _ mission.Meta, _ time.Time) {
		startedID = board.MissionID
	})
	runner.SetRunFactory(func(next scope.Scope) (IsolatedRun, error) {
		board, err := blackboard.LoadOrNew("test--scope-new", dir)
		if err != nil {
			return IsolatedRun{}, err
		}
		return IsolatedRun{Swarm: &swarm.Swarm{Scope: next, BB: board}, Blackboard: board,
			Meta: mission.Meta{Name: "test", ConnectionInfo: next.Describe()}, Started: time.Now()}, nil
	})
	done := runner.Start(ctx)
	if err := runner.UpdateScope([]string{"https://new.example:8443/path", "10.30.0.0/16"}); err != nil {
		t.Fatal(err)
	}
	config := runner.ScopeConfig()
	if !config.Supported || !config.Editable || config.Description != "https://new.example:8443/path, 10.30.0.0/16" {
		t.Fatalf("ScopeConfig=%+v", config)
	}
	currentSwarm, currentBoard, _, _ := runner.Current()
	current := currentSwarm.CurrentScope()
	if !current.Allows("new.example") || !current.Allows("10.30.2.9") || current.Allows("old.example") {
		t.Fatal("scope replacement was not enforced")
	}
	if currentSwarm == oldSwarm || currentBoard == oldBoard || currentBoard.MissionID == oldBoard.MissionID {
		t.Fatal("scope change retained the old executor or blackboard")
	}
	if currentBoard.Counts()["facts"] != 0 || oldBoard.Counts()["facts"] != 1 {
		t.Fatal("old target facts leaked into the new task")
	}
	if stoppedID != oldBoard.MissionID || startedID != currentBoard.MissionID {
		t.Fatalf("run lifecycle callbacks missing: stopped=%q started=%q", stoppedID, startedID)
	}
	cancel()
	<-done
}

func TestUpdateScopeRejectsEmptyInput(t *testing.T) {
	runner := &Runner{sw: &swarm.Swarm{Scope: scope.Parse([]string{"old.example"})}}
	if err := runner.UpdateScope([]string{"", "  "}); err == nil {
		t.Fatal("expected empty scope rejection")
	}
}

func TestOperatorScopePersistsAcrossRuntimeCleanup(t *testing.T) {
	dir := t.TempDir()
	id := "engagement\x00operator-selected"
	oldBoard, err := blackboard.LoadOrNew("empty-draft", dir)
	if err != nil {
		t.Fatal(err)
	}
	sw := &swarm.Swarm{BB: oldBoard}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := New(sw, oldBoard, mission.Meta{Name: "operator-selected"}, cancel, time.Time{})
	runner.runSwarm = func(ctx context.Context, _ *swarm.Swarm) { <-ctx.Done() }
	runner.SetRunFactory(func(next scope.Scope) (IsolatedRun, error) {
		board, err := blackboard.LoadOrNew("operator-selected--scope-new", dir)
		if err != nil {
			return IsolatedRun{}, err
		}
		if err := scope.SaveOperatorScopeRun(dir, id, next, "new"); err != nil {
			return IsolatedRun{}, err
		}
		return IsolatedRun{Swarm: &swarm.Swarm{Scope: next, BB: board}, Blackboard: board,
			Meta: mission.Meta{Name: "operator-selected", ConnectionInfo: next.Describe()}, Started: time.Now()}, nil
	})
	done := runner.Start(ctx)
	if err := runner.CommandChecked("resume", "", "", ""); err == nil {
		t.Fatal("empty scope should block resume")
	}
	if err := runner.UpdateScope([]string{"operator.example"}); err != nil {
		t.Fatal(err)
	}
	if err := runner.CommandChecked("resume", "", "", ""); err != nil {
		t.Fatalf("saved scope still blocked resume: %v", err)
	}
	loaded, found, err := scope.LoadOperatorScope(dir, id)
	if err != nil || !found || !loaded.Allows("operator.example") {
		t.Fatalf("operator scope was not durable: found=%v err=%v", found, err)
	}
	cancel()
	<-done
}

func TestSnapshotWithoutDeadline(t *testing.T) {
	board, err := blackboard.LoadOrNew("unlimited-test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{sw: &swarm.Swarm{}, bb: board, meta: mission.Meta{Name: "unlimited-test"}, deadline: time.Time{}}
	summary := runner.Snapshot()["summary"].(map[string]any)
	if got := summary["budget_left_min"]; got != -1 {
		t.Fatalf("budget_left_min = %v, want -1", got)
	}
}

func TestSnapshotIncludesFindingDetails(t *testing.T) {
	board, err := blackboard.LoadOrNew("detail-test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := board.AddFinding(blackboard.Finding{
		Title: "Detailed finding", Target: "https://target.example/path",
		Category: "web/test", Severity: "high", Status: blackboard.Verified,
		Evidence: "response proves the issue", Reproduction: "send the request",
		Remediation: "validate the input", FoundBy: "test-agent",
		EvidenceRefs: []blackboard.EvidenceRef{{Ref: "t1", Role: "proof"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{
		sw: &swarm.Swarm{Scope: scope.Parse([]string{"target.example"})}, bb: board,
		meta:     mission.Meta{Name: "detail-test", ConnectionInfo: "target.example"},
		deadline: time.Now().Add(time.Hour),
	}
	snapshot := runner.Snapshot()
	rows, ok := snapshot["findings"].([]map[string]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("findings=%T %#v", snapshot["findings"], snapshot["findings"])
	}
	row := rows[0]
	if row["id"] != id || row["evidence_text"] != "response proves the issue" || row["reproduction"] != "send the request" || row["remediation"] != "validate the input" {
		t.Fatalf("finding detail missing: %#v", row)
	}
	if refs, ok := row["evidence_refs"].([]blackboard.EvidenceRef); !ok || len(refs) != 1 || refs[0].Ref != "t1" {
		t.Fatalf("evidence refs missing: %#v", row["evidence_refs"])
	}
	graphs, ok := snapshot["evidence_graphs"].(map[string]blackboard.GraphSnapshot)
	if !ok || len(graphs["detail-test"].Claims) != 1 || len(graphs["detail-test"].Evidence) != 1 || len(graphs["detail-test"].Edges) != 1 {
		t.Fatalf("evidence graph missing: %#v", snapshot["evidence_graphs"])
	}
	edge := graphs["detail-test"].Edges[0]
	if edge.From != "t1" || edge.To != id || edge.Kind != "grounds" || edge.Role != "proof" {
		t.Fatalf("unexpected evidence edge: %#v", edge)
	}
}
