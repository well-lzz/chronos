package scheduler

import (
	"testing"

	"github.com/well-lzz/chronos/internal/config"
	"github.com/well-lzz/chronos/internal/mission"
	"github.com/well-lzz/chronos/internal/runhistory"
	"github.com/well-lzz/chronos/internal/scope"
	"github.com/well-lzz/chronos/internal/swarm"
)

func TestSchedulerOperatorCanReplaceMissionScope(t *testing.T) {
	dir := t.TempDir()
	oldDone := make(chan struct{})
	close(oldDone)
	oldSwarm := &swarm.Swarm{Scope: scope.Parse([]string{"old.example"})}
	s := &Scheduler{
		cfg:    config.Settings{StateDir: dir, PlatformMode: "tsec", BenchmarkURL: "https://platform.example"},
		runs:   map[string]*run{"challenge-1": {sw: oldSwarm, done: oldDone, cancel: func() {}, meta: mission.Meta{Name: "challenge-1"}}},
		runIDs: map[string]string{},
	}
	if err := s.UpdateScopeFor("challenge-1", []string{"new.example", "10.30.0.0/16"}); err != nil {
		t.Fatal(err)
	}
	current := s.ScopeConfigFor("challenge-1")
	if !current.Editable || len(current.Targets) != 2 || s.runs["challenge-1"].sw.CurrentScope().Allows("old.example") {
		t.Fatalf("mission scope was not replaced: %+v", current)
	}
	loaded, found, err := scope.LoadOperatorScope(dir, s.scopeIdentity("challenge-1"))
	if err != nil || !found || !loaded.Allows("new.example") || !loaded.Allows("10.30.1.7") {
		t.Fatalf("scope not persisted: found=%v err=%v", found, err)
	}
	if err := s.UpdateScopeFor("missing", []string{"other.example"}); err == nil {
		t.Fatal("inactive mission scope should not be editable")
	}
	newRun := s.runs["challenge-1"]
	newRun.cancel()
	<-newRun.done
	if newRun.sw == oldSwarm || newRun.sw.BB == nil || newRun.sw.BB.MissionID == "challenge-1" {
		t.Fatal("scope change reused the old swarm or mission state")
	}
	runs, err := (runhistory.Store{StateDir: dir}).List(nil)
	if err != nil || len(runs) != 1 || runs[0].ID != newRun.sw.BB.MissionID || runs[0].Scope != "new.example, 10.30.0.0/16" {
		t.Fatalf("new isolated run missing from history: %+v %v", runs, err)
	}
}
