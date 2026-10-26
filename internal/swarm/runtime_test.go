package swarm

import (
	"testing"
	"time"

	"github.com/well-lzz/chronos/internal/blackboard"
)

func TestExecutorRuntimeCheckpointRestoresCycleCounters(t *testing.T) {
	bb, err := blackboard.LoadOrNew("runtime", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Swarm{BB: bb}
	started := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	state := executorRuntime{Status: "interrupted", StartedAt: started, Cycles: 7, Bumps: 3, ProviderFails: 2, Steps: 31}
	if err := s.saveRuntime("model-a", state); err != nil {
		t.Fatal(err)
	}
	got := s.loadRuntime("model-a")
	if got.Cycles != 7 || got.Bumps != 3 || got.Steps != 31 || !got.StartedAt.Equal(started) {
		t.Fatalf("runtime state not restored: %+v", got)
	}
	state.Status = "finished"
	if err := s.saveRuntime("model-a", state); err != nil {
		t.Fatal(err)
	}
	if got := s.loadRuntime("model-a"); got.Cycles != 0 {
		t.Fatalf("finished attempt unexpectedly resumed: %+v", got)
	}
}
