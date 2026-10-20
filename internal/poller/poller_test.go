package poller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/well-lzz/chronos/internal/mission"
	"github.com/well-lzz/chronos/internal/platform"
)

// fakePlatform drives the poller: FetchPollState returns pollErr when set,
// otherwise a single healthy challenge.
type fakePlatform struct {
	pollErr         error
	empty           bool
	initialFailures int
}

func (f *fakePlatform) FetchAllChallenges() ([]platform.Challenge, error) { return nil, f.pollErr }
func (f *fakePlatform) FetchSolvedNames() (map[string]bool, error) {
	return map[string]bool{}, f.pollErr
}
func (f *fakePlatform) FetchPollState() ([]platform.Challenge, map[string]bool, error) {
	if f.initialFailures > 0 {
		f.initialFailures--
		return nil, nil, fmt.Errorf("%w: task_not_found", platform.ErrTaskNotRunnable)
	}
	if f.pollErr != nil {
		return nil, nil, f.pollErr
	}
	if f.empty {
		return nil, map[string]bool{}, nil
	}
	return []platform.Challenge{{Name: "web-1"}}, map[string]bool{}, nil
}
func (f *fakePlatform) SubmitFlag(string, string) (platform.SubmitResult, error) {
	return platform.SubmitResult{}, nil
}
func (f *fakePlatform) PullChallenge(context.Context, platform.Challenge) (mission.Meta, error) {
	return mission.Meta{}, nil
}
func (f *fakePlatform) ReleaseChallenge(string) bool                  { return true }
func (f *fakePlatform) CloseStaleContainers(map[string]bool) []string { return nil }
func (f *fakePlatform) FetchHint(string) (string, error)              { return "", nil }
func (f *fakePlatform) Close()                                        {}

func TestTaskUnavailablePausesButKeepsPollingAndRecovers(t *testing.T) {
	pf := &fakePlatform{}
	p := New(pf, 0, 0)
	if err := p.seed(); err != nil {
		t.Fatal(err)
	}

	if p.TaskUnavailable() {
		t.Fatal("poller should start available")
	}
	pf.pollErr = fmt.Errorf("%w: task already finished", platform.ErrTaskNotRunnable)
	p.pollOnce()
	p.pollOnce() // even repeated responses must not stop polling

	if !p.TaskUnavailable() {
		t.Fatal("task-not-runnable response must pause new pulls")
	}
	select {
	case <-p.stop:
		t.Fatal("platform error must not stop the poller")
	default:
	}
	if !p.KnownChallenges()["web-1"] {
		t.Fatal("platform error must preserve the last healthy state")
	}

	pf.pollErr = nil
	pf.empty = true
	p.pollOnce() // a suspicious empty response is not proof of recovery
	if !p.TaskUnavailable() {
		t.Fatal("suspicious empty poll must not resume scheduling")
	}

	pf.empty = false
	p.pollOnce()
	if p.TaskUnavailable() {
		t.Fatal("a healthy poll must restore scheduling")
	}
	select {
	case ev := <-p.Events():
		if ev.Kind != PlatformRecovered {
			t.Fatalf("got %v, want platform recovery event", ev.Kind)
		}
	default:
		t.Fatal("healthy poll did not report recovery")
	}
}

func TestInitialTaskUnavailableIsRetryable(t *testing.T) {
	pf := &fakePlatform{initialFailures: 1}
	p := New(pf, time.Hour, 0)
	p.initialRetryDelay = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("initial task-unavailable response must not abort startup: %v", err)
	}
	defer p.Stop()
	if p.TaskUnavailable() || !p.KnownChallenges()["web-1"] {
		t.Fatal("initial retry did not recover and discover challenges")
	}
}

func TestPersistentInitialTaskUnavailableReturnsError(t *testing.T) {
	pf := &fakePlatform{pollErr: fmt.Errorf("%w: task_not_found", platform.ErrTaskNotRunnable)}
	p := New(pf, time.Hour, 0)
	p.initialRetryDelay = 0
	if err := p.Start(context.Background()); !platform.IsTaskNotRunnable(err) {
		t.Fatalf("persistent initial failure must be reported: %v", err)
	}
}

func TestPollerIgnoresTransientErrors(t *testing.T) {
	pf := &fakePlatform{pollErr: fmt.Errorf("read platform response: context deadline exceeded")}
	p := New(pf, 0, 0)
	p.pollOnce()
	if p.TaskUnavailable() {
		t.Fatal("an ordinary transient poll error must not pause scheduling")
	}
}
