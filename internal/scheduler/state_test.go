package scheduler

import (
	"testing"
	"time"

	"github.com/well-lzz/chronos/internal/config"
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
	first.recordDecision("defer", "mission-b", "cooldown", "waiting")
	if err := first.checkpoint(); err != nil {
		t.Fatal(err)
	}
	restored := New(nil, cfg, []string{"model-a"})
	if restored.recoveryErr != nil || restored.attempts["mission-a"] != 2 || restored.deferred["mission-b"] != "cooldown" ||
		!restored.resumePending["mission-a"].Equal(deadline) || !restored.runStart.Equal(first.runStart) ||
		len(restored.events) != 1 || restored.events[0].Kind != "defer" {
		t.Fatalf("scheduler state not restored: error=%v attempts=%v deferred=%v resume=%v", restored.recoveryErr, restored.attempts, restored.deferred, restored.resumePending)
	}
	if err := restored.clearState(); err != nil {
		t.Fatal(err)
	}
	if next := New(nil, cfg, []string{"model-a"}); len(next.resumePending)+len(next.deferred) != 0 {
		t.Fatal("completed scheduler state was not cleared")
	}
}
