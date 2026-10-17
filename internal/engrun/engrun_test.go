package engrun

import (
	"testing"

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
