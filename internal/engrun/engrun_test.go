package engrun

import (
	"testing"
	"time"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/mission"
	"github.com/well-lzz/chronos/internal/scope"
	"github.com/well-lzz/chronos/internal/swarm"
)

func TestUpdateScopeReplacesRunnerAndSwarmScope(t *testing.T) {
	runner := &Runner{
		sw:   &swarm.Swarm{Scope: scope.Parse([]string{"old.example"})},
		meta: mission.Meta{Name: "test", ConnectionInfo: "old.example"},
	}
	if err := runner.UpdateScope([]string{"https://new.example:8443/path", "10.30.0.0/16"}); err != nil {
		t.Fatal(err)
	}
	config := runner.ScopeConfig()
	if !config.Supported || !config.Editable || config.Description != "https://new.example:8443/path, 10.30.0.0/16" {
		t.Fatalf("ScopeConfig=%+v", config)
	}
	current := runner.sw.CurrentScope()
	if !current.Allows("new.example") || !current.Allows("10.30.2.9") || current.Allows("old.example") {
		t.Fatal("scope replacement was not enforced")
	}
}

func TestUpdateScopeRejectsEmptyInput(t *testing.T) {
	runner := &Runner{sw: &swarm.Swarm{Scope: scope.Parse([]string{"old.example"})}}
	if err := runner.UpdateScope([]string{"", "  "}); err == nil {
		t.Fatal("expected empty scope rejection")
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
	rows, ok := runner.Snapshot()["findings"].([]map[string]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("findings=%T %#v", runner.Snapshot()["findings"], runner.Snapshot()["findings"])
	}
	row := rows[0]
	if row["id"] != id || row["evidence_text"] != "response proves the issue" || row["reproduction"] != "send the request" || row["remediation"] != "validate the input" {
		t.Fatalf("finding detail missing: %#v", row)
	}
	if refs, ok := row["evidence_refs"].([]blackboard.EvidenceRef); !ok || len(refs) != 1 || refs[0].Ref != "t1" {
		t.Fatalf("evidence refs missing: %#v", row["evidence_refs"])
	}
}
