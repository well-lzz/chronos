package poller

import (
	"context"
	"fmt"
	"testing"

	"github.com/well-lzz/chronos/internal/mission"
	"github.com/well-lzz/chronos/internal/platform"
)

// fakePlatform drives the poller: FetchPollState returns pollErr when set,
// otherwise a single healthy challenge.
type fakePlatform struct{ pollErr error }

func (f *fakePlatform) FetchAllChallenges() ([]platform.Challenge, error) { return nil, f.pollErr }
func (f *fakePlatform) FetchSolvedNames() (map[string]bool, error) {
	return map[string]bool{}, f.pollErr
}
func (f *fakePlatform) FetchPollState() ([]platform.Challenge, map[string]bool, error) {
	if f.pollErr != nil {
		return nil, nil, f.pollErr
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

func TestPollerFlagsTerminalTaskAndStops(t *testing.T) {
	pf := &fakePlatform{pollErr: fmt.Errorf("%w: task already finished", platform.ErrTaskNotRunnable)}
	p := New(pf, 0, 0)

	if p.Terminal() {
		t.Fatal("poller should not start terminal")
	}
	p.pollOnce() // a poll that hits the finished-task error

	if !p.Terminal() {
		t.Fatal("poller must flag Terminal() after a task-not-runnable poll")
	}
	select {
	case <-p.stop:
		// stop channel closed as expected
	default:
		t.Fatal("poller must stop its loop once the task is terminal")
	}
}

func TestPollerIgnoresTransientErrors(t *testing.T) {
	pf := &fakePlatform{pollErr: fmt.Errorf("read platform response: context deadline exceeded")}
	p := New(pf, 0, 0)
	p.pollOnce()
	if p.Terminal() {
		t.Fatal("a transient poll error must not be treated as terminal")
	}
}
