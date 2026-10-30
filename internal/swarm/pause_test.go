package swarm

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPauseBlocksUntilResumeAndCancellation(t *testing.T) {
	s := &Swarm{ModelSpecs: []string{"agent"}}
	s.init()
	s.Pause()
	if !s.Paused() {
		t.Fatal("pause did not set the gate")
	}
	finished := make(chan error, 1)
	go func() { finished <- s.waitIfPaused(context.Background(), "agent") }()
	deadline := time.After(time.Second)
	for {
		if s.Status()["agents"].(map[string]any)["agent"].(map[string]any)["status"] == "paused" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("executor did not acknowledge pause")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-finished:
		t.Fatal("pause gate returned before resume")
	default:
	}
	s.Resume()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("resume did not wake paused executor")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.Pause()
	go func() { finished <- s.waitIfPaused(ctx, "agent") }()
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stopped gate returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stop did not interrupt pause gate")
	}
}
