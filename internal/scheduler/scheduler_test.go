package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/well-lzz/chronos/internal/config"
	"github.com/well-lzz/chronos/internal/mission"
	"github.com/well-lzz/chronos/internal/platform"
	"github.com/well-lzz/chronos/internal/poller"
)

type fakePlatform struct {
	pollErr   error
	pullErr   error
	staleKeep map[string]bool
}

type unavailableAfterSeedPlatform struct {
	fakePlatform
	seeded chan struct{}
	calls  atomic.Int32
}

func (f *unavailableAfterSeedPlatform) FetchPollState() ([]platform.Challenge, map[string]bool, error) {
	if f.calls.Add(1) == 1 {
		close(f.seeded)
		return []platform.Challenge{{Name: "slow"}}, map[string]bool{}, nil
	}
	return nil, nil, fmt.Errorf("%w: task already finished", platform.ErrTaskNotRunnable)
}

func (f *fakePlatform) FetchAllChallenges() ([]platform.Challenge, error) { return nil, f.pollErr }
func (f *fakePlatform) FetchSolvedNames() (map[string]bool, error) {
	return map[string]bool{}, f.pollErr
}
func (f *fakePlatform) FetchPollState() ([]platform.Challenge, map[string]bool, error) {
	if f.pollErr != nil {
		return nil, nil, f.pollErr
	}
	return []platform.Challenge{{Name: "slow"}}, map[string]bool{}, nil
}
func (f *fakePlatform) SubmitFlag(string, string) (platform.SubmitResult, error) {
	return platform.SubmitResult{}, nil
}
func (f *fakePlatform) PullChallenge(ctx context.Context, _ platform.Challenge) (mission.Meta, error) {
	if f.pullErr != nil {
		return mission.Meta{}, f.pullErr
	}
	<-ctx.Done()
	return mission.Meta{}, ctx.Err()
}
func (f *fakePlatform) ReleaseChallenge(string) bool { return true }
func (f *fakePlatform) CloseStaleContainers(keep map[string]bool) []string {
	f.staleKeep = keep
	return nil
}
func (f *fakePlatform) FetchHint(string) (string, error) { return "", nil }
func (f *fakePlatform) Close()                           {}

func TestGlobalBudgetCancelsPendingPullAndExits(t *testing.T) {
	s := New(&fakePlatform{}, config.Settings{TotalBudgetSeconds: 1, MaxConcurrent: 1}, []string{"test/model"})
	started := time.Now()
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("scheduler exceeded shutdown bound: %v", elapsed)
	}
}

func TestInitialPollFailureIsReturned(t *testing.T) {
	want := errors.New("platform unavailable")
	s := New(&fakePlatform{pollErr: want}, config.Settings{}, nil)
	if err := s.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func TestRunningSchedulerSurvivesTaskUnavailablePoll(t *testing.T) {
	pf := &unavailableAfterSeedPlatform{seeded: make(chan struct{})}
	s := New(pf, config.Settings{MaxConcurrent: 1}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	select {
	case <-pf.seeded:
	case <-time.After(2 * time.Second):
		t.Fatal("initial poll did not run")
	}
	s.poll.Refresh()
	if !s.poll.TaskUnavailable() {
		t.Fatal("terminal-like response did not mark the platform unavailable")
	}
	select {
	case err := <-done:
		t.Fatalf("scheduler exited on one task-unavailable poll: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler did not exit after explicit cancellation")
	}
}

func TestPriorityPrefersReusableContainer(t *testing.T) {
	ready := platform.Challenge{Name: "ready", Value: 10, ContainerStatus: "available", ContainerAddr: []string{"10.0.0.1:80"}}
	highScore := platform.Challenge{Name: "new", Value: 1000, ContainerStatus: "closed"}
	if !priorityLess(ready, highScore) {
		t.Fatal("an already-running container should be reused before starting a new one")
	}
}

func TestPlatformPullFailuresDoNotConsumeSolverAttempts(t *testing.T) {
	pf := &fakePlatform{pullErr: errors.New("capacity reached")}
	s := New(pf, config.Settings{MaxConcurrent: 1}, nil)
	s.poll = poller.New(pf, time.Hour, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.poll.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.poll.Stop()

	for i := 0; i < 2; i++ {
		s.spawnOne(ctx, "slow")
		s.pulls.Wait()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempts["slow"] != 0 {
		t.Fatalf("solver attempts = %d, want 0", s.attempts["slow"])
	}
	if s.deferred["slow"] == "" || !s.retryEligible("slow", time.Now().Add(time.Hour)) {
		t.Fatal("platform failure must remain retryable after cooldown")
	}
}

func TestTaskUnavailableDoesNotSpawnOrCancelActiveWork(t *testing.T) {
	pf := &fakePlatform{}
	s := New(pf, config.Settings{MaxConcurrent: 2}, nil)
	s.poll = poller.New(pf, time.Hour, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.poll.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.poll.Stop()

	activeCtx, activeCancel := context.WithCancel(ctx)
	defer activeCancel()
	s.runs["active"] = &run{cancel: activeCancel}
	pf.pollErr = fmt.Errorf("%w: task already finished", platform.ErrTaskNotRunnable)
	s.poll.Refresh()
	if !s.poll.TaskUnavailable() {
		t.Fatal("expected the platform-unavailable state")
	}
	s.autoSpawnUnsolved(ctx)
	s.spawnOne(ctx, "slow")
	if activeCtx.Err() != nil {
		t.Fatal("platform error cancelled an active challenge")
	}
	if len(s.pending) != 0 {
		t.Fatal("platform error started a new challenge")
	}

	pf.pollErr = nil
	s.poll.Refresh()
	if s.poll.TaskUnavailable() {
		t.Fatal("healthy poll did not restore availability")
	}
}

func TestReconcilePreservesOwnedContainers(t *testing.T) {
	pf := &fakePlatform{}
	s := New(pf, config.Settings{}, nil)
	s.runs["running"] = &run{}
	s.pending["starting"] = true
	s.openContainers["loading"] = true
	s.reconcileContainers()
	for _, name := range []string{"running", "starting", "loading"} {
		if !pf.staleKeep[name] {
			t.Fatalf("reconciliation did not protect %s", name)
		}
	}
}

func TestCapacityFailureQueuesReconcileInsteadOfRunningItInPullWorker(t *testing.T) {
	pf := &fakePlatform{pullErr: fmt.Errorf("platform rejected start: %w", platform.ErrAtCapacity)}
	s := New(pf, config.Settings{MaxConcurrent: 1}, nil)
	s.poll = poller.New(pf, time.Hour, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.poll.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.poll.Stop()

	s.spawnOne(ctx, "slow")
	s.pulls.Wait()
	if pf.staleKeep != nil {
		t.Fatal("pull worker reconciled containers outside the coordinator loop")
	}
	select {
	case <-s.reconcileRequests:
	default:
		t.Fatal("capacity rejection did not queue reconciliation")
	}
}
