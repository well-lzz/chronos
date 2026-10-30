// Package scheduler is the mechanical coordinator (no LLM): it polls the
// platform, fills a bounded concurrency window with the highest-value unsolved
// challenges, runs one collaborative swarm per challenge, reaps finished ones
// and rolls deferred ones back in after a cooldown. A platform-confirmed solve
// (from the poller) kills the swarm — that is how a flag mission terminates.
package scheduler

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/config"
	"github.com/well-lzz/chronos/internal/llmconfig"
	"github.com/well-lzz/chronos/internal/mission"
	"github.com/well-lzz/chronos/internal/platform"
	"github.com/well-lzz/chronos/internal/poller"
	"github.com/well-lzz/chronos/internal/runhistory"
	"github.com/well-lzz/chronos/internal/scope"
	"github.com/well-lzz/chronos/internal/swarm"
	"github.com/well-lzz/chronos/internal/traffic"
)

type run struct {
	sw            *swarm.Swarm
	cancel        context.CancelFunc
	done          chan struct{}
	meta          mission.Meta
	solved        bool
	transitioning bool
	deadline      time.Time
}

// Command is an operator action (from the web console or REPL).
type Command struct {
	Action    string // pause | resume | kill | bump | resume_all
	Challenge string
	Model     string
	Message   string
}

type Scheduler struct {
	pf       platform.Platform
	poll     *poller.Poller
	cfg      config.Settings
	specs    []string
	maxConc  int
	runStart time.Time
	runCtx   context.Context

	LLM               *llmconfig.Store             // operator-editable model profile (optional)
	ModelProfiles     map[string]llmconfig.Profile // optional per-executor operator profiles
	UseConsoleProfile bool

	mu             sync.Mutex
	scopeUpdateMu  sync.Mutex
	checkpointMu   sync.Mutex
	recoveryErr    error
	events         []OrchestrationEvent
	eventSeq       uint64
	runs           map[string]*run
	resumePending  map[string]time.Time
	runIDs         map[string]string
	deferred       map[string]string
	attempts       map[string]int
	deferTime      map[string]time.Time
	pending        map[string]bool
	openContainers map[string]bool
	spawnFailures  map[string]int
	pulls          sync.WaitGroup

	cmds              chan Command
	reconcileRequests chan struct{}
}

func New(pf platform.Platform, cfg config.Settings, specs []string) *Scheduler {
	mc := cfg.MaxConcurrent
	if mc <= 0 {
		mc = 3
	}
	s := &Scheduler{
		pf: pf, cfg: cfg, specs: specs, maxConc: mc, runStart: time.Now(),
		runs: map[string]*run{}, deferred: map[string]string{}, attempts: map[string]int{}, resumePending: map[string]time.Time{}, runIDs: map[string]string{},
		deferTime: map[string]time.Time{}, pending: map[string]bool{}, openContainers: map[string]bool{},
		spawnFailures: map[string]int{}, cmds: make(chan Command, 64),
		reconcileRequests: make(chan struct{}, 1),
	}
	s.recoveryErr = s.loadState()
	return s
}

// Enqueue pushes an operator command (web console / REPL).
func (s *Scheduler) Enqueue(c Command) {
	select {
	case s.cmds <- c:
	default:
	}
}

func (s *Scheduler) Poller() *poller.Poller { return s.poll }

// Run drives the loop until ctx is cancelled or everything is solved.
func (s *Scheduler) Run(ctx context.Context) error {
	if s.recoveryErr != nil {
		return s.recoveryErr
	}
	runCtx := ctx
	cancel := func() {}
	if s.cfg.TotalBudgetSeconds > 0 {
		runCtx, cancel = context.WithDeadline(ctx, s.runStart.Add(time.Duration(s.cfg.TotalBudgetSeconds)*time.Second))
	}
	defer cancel()
	s.mu.Lock()
	s.runCtx = runCtx
	s.mu.Unlock()

	pmin, pmax := 5*time.Second, 15*time.Second
	s.poll = poller.New(s.pf, (pmin+pmax)/2, (pmax-pmin)/2)
	if err := s.poll.Start(runCtx); err != nil {
		s.shutdown()
		return err
	}
	log.Printf("Scheduler starting: %d model(s), %d challenges", len(s.specs), len(s.poll.KnownChallenges()))
	stubs, solved := s.poll.KnownStubs(), s.poll.KnownSolved()
	s.mu.Lock()
	for name := range s.resumePending {
		if _, exists := stubs[name]; !exists || solved[name] {
			delete(s.resumePending, name)
		}
	}
	s.mu.Unlock()

	s.reconcileContainers()
	s.autoSpawnUnsolved(runCtx)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	lastStatus := time.Now()
	var deadlockSince time.Time

	for {
		reconcileNow := false
		select {
		case <-runCtx.Done():
			if ctx.Err() == nil {
				log.Printf("Global budget exhausted; stopping all active work.")
			}
			s.shutdown()
			return nil
		case ev := <-s.poll.Events():
			s.handleEvent(runCtx, ev)
		case c := <-s.cmds:
			s.handleCommand(runCtx, c)
		case <-s.reconcileRequests:
			reconcileNow = true
		case <-ticker.C:
		}

		// Terminal platform state: the benchmark task was finished/expired on the
		// platform. Every start/poll now fails, so stop cleanly instead of churning
		// deferred retries until the global budget runs out.
		if s.poll.Terminal() {
			log.Printf("Benchmark task finished on platform; stopping scheduler cleanly.")
			s.shutdown()
			_ = s.clearState()
			return nil
		}
		if reconcileNow {
			s.reconcileContainers()
			s.autoSpawnUnsolved(runCtx)
		}

		if s.reapAndRefill(runCtx) {
			// slot freed; refill already attempted inside
		}
		if err := s.checkpoint(); err != nil {
			log.Printf("scheduler checkpoint failed: %v", err)
		}

		if time.Since(lastStatus) >= 60*time.Second {
			lastStatus = time.Now()
			s.reconcileContainers()
			s.autoSpawnUnsolved(runCtx)
			deadlockSince = s.handleDeadlock(runCtx, deadlockSince)
		}

		// Clean termination: everything solved and nothing in flight.
		solved := s.poll.KnownSolved()
		known := s.poll.KnownChallenges()
		s.mu.Lock()
		activeOrPending := len(s.runs) + len(s.pending)
		s.mu.Unlock()
		if activeOrPending == 0 && len(known) > 0 && len(solved) >= len(known) {
			log.Printf("All challenges solved; scheduler exiting.")
			s.shutdown()
			_ = s.clearState()
			return nil
		}
	}
}

func (s *Scheduler) handleEvent(ctx context.Context, ev poller.Event) {
	switch ev.Kind {
	case poller.ChallengeSolved:
		s.recordDecision("solved", ev.Name, "platform_confirmed", "platform reported mission solved")
		log.Printf("Solved: %s", ev.Name)
		s.killSwarm(ev.Name, true)
	case poller.NewChallenge:
		s.recordDecision("discovered", ev.Name, "poller", "new mission available")
		s.spawnOne(ctx, ev.Name)
	}
}

func (s *Scheduler) handleCommand(ctx context.Context, c Command) {
	s.recordDecision("operator_"+c.Action, c.Challenge, "operator", c.Model)
	s.mu.Lock()
	r := s.runs[c.Challenge]
	s.mu.Unlock()
	switch c.Action {
	case "pause":
		if r != nil {
			r.sw.Pause()
		}
	case "resume":
		if r != nil {
			r.sw.Resume()
		} else {
			s.mu.Lock()
			delete(s.deferred, c.Challenge)
			delete(s.attempts, c.Challenge)
			delete(s.deferTime, c.Challenge)
			s.mu.Unlock()
			s.spawnOne(ctx, c.Challenge)
		}
	case "resume_all":
		s.mu.Lock()
		for n := range s.deferred {
			delete(s.deferred, n)
			delete(s.attempts, n)
			delete(s.deferTime, n)
		}
		s.mu.Unlock()
		s.autoSpawnUnsolved(ctx)
	case "kill":
		s.killSwarm(c.Challenge, false)
		s.mu.Lock()
		s.deferred[c.Challenge] = "stopped by operator"
		s.mu.Unlock()
		s.recordDecision("defer", c.Challenge, "operator_stop", "mission held until explicit resume")
	case "bump":
		if r != nil {
			r.sw.Bump(c.Model, c.Message)
		}
	case "chat":
		if r != nil {
			r.sw.Chat(c.Model, c.Message)
		}
	}
}

func (s *Scheduler) killSwarm(name string, solved bool) {
	s.mu.Lock()
	r := s.runs[name]
	if r != nil {
		r.solved = r.solved || solved
	}
	s.mu.Unlock()
	if r != nil {
		r.cancel()
		s.recordDecision("kill", name, map[bool]string{true: "platform_solved", false: "operator_or_scheduler"}[solved], "swarm cancellation requested")
	}
}

// reapAndRefill removes finished runs (deferring unsolved ones) and refills.
func (s *Scheduler) reapAndRefill(ctx context.Context) bool {
	var freed bool
	var releases []string
	var deferred []string
	var finished []struct{ id, outcome string }
	s.mu.Lock()
	for name, r := range s.runs {
		if r.transitioning {
			continue
		}
		select {
		case <-r.done:
			outcome := "finished"
			if r.solved {
				outcome = "platform_solved"
			} else if s.deferred[name] != "" {
				outcome = "deferred"
			}
			if !r.solved && s.deferred[name] == "" {
				s.deferred[name] = "solver attempts exhausted"
				outcome = "deferred"
				s.deferTime[name] = time.Now()
				deferred = append(deferred, name)
				log.Printf("Deferred: %s", name)
			} else {
				log.Printf("Swarm finished: %s", name)
			}
			delete(s.runs, name)
			if r.sw != nil && r.sw.BB != nil {
				finished = append(finished, struct{ id, outcome string }{r.sw.BB.MissionID, outcome})
			}
			delete(s.runIDs, name)
			delete(s.openContainers, name)
			releases = append(releases, name)
			freed = true
		default:
		}
	}
	s.mu.Unlock()
	for _, run := range finished {
		if err := (runhistory.Store{StateDir: s.cfg.StateDir}).Finish(run.id, run.outcome, time.Now()); err != nil {
			log.Printf("finish run history %s: %v", run.id, err)
		}
	}
	for _, name := range deferred {
		s.recordDecision("defer", name, "solver_attempts_exhausted", "swarm finished without platform confirmation")
	}
	var closeFailed bool
	for _, name := range releases {
		s.recordDecision("release", name, "swarm_finished", "platform container released")
		if !s.pf.ReleaseChallenge(name) {
			closeFailed = true
		}
	}
	if closeFailed {
		s.reconcileContainers()
	}
	if freed {
		s.autoSpawnUnsolved(ctx)
	}
	return freed
}

func (s *Scheduler) remainingGlobal() float64 {
	if s.cfg.TotalBudgetSeconds <= 0 {
		return -1
	}
	return float64(s.cfg.TotalBudgetSeconds) - time.Since(s.runStart).Seconds()
}

func (s *Scheduler) perChallengeLimit(meta mission.Meta) time.Duration {
	limit := s.cfg.ChallengeTimeLimitS
	if guessKillchain(meta) && s.cfg.KillchainTimeLimitS > limit {
		limit = s.cfg.KillchainTimeLimitS
	}
	if rg := s.remainingGlobal(); rg >= 0 {
		if int(rg) < limit {
			limit = int(rg)
		}
		if limit < 1 {
			limit = 1
		}
	}
	return time.Duration(limit) * time.Second
}

func guessKillchain(m mission.Meta) bool {
	return m.IsKillchain()
}

func priorityLess(a, b platform.Challenge) bool {
	aReady := a.ContainerStatus == "available" && len(a.ContainerAddr) > 0
	bReady := b.ContainerStatus == "available" && len(b.ContainerAddr) > 0
	if aReady != bReady {
		return aReady
	}
	if a.Value != b.Value {
		return a.Value > b.Value // higher score first
	}
	if a.Level != b.Level {
		return a.Level < b.Level // easier first
	}
	return a.Name < b.Name
}

func (s *Scheduler) autoSpawnUnsolved(ctx context.Context) {
	stubs := s.poll.KnownStubs()
	solved := s.poll.KnownSolved()

	var cands []platform.Challenge
	s.mu.Lock()
	slots := s.maxConc - (len(s.runs) + len(s.pending))
	resume := make(map[string]bool, len(s.resumePending))
	for name := range s.resumePending {
		resume[name] = true
	}
	busy := func(n string) bool { _, a := s.runs[n]; _, b := s.pending[n]; return a || b }
	for name, c := range stubs {
		if solved[name] || busy(name) || s.deferred[name] != "" {
			continue
		}
		cands = append(cands, c)
	}
	s.mu.Unlock()
	if slots <= 0 {
		return
	}
	sort.Slice(cands, func(i, j int) bool {
		ai, bj := resume[cands[i].Name], resume[cands[j].Name]
		if ai != bj {
			return ai
		}
		return priorityLess(cands[i], cands[j])
	})

	picked := cands
	if len(picked) > slots {
		picked = picked[:slots]
	}
	remaining := slots - len(picked)

	// Rolling retry: refill remaining slots from eligible deferred challenges.
	if remaining > 0 {
		var retry []platform.Challenge
		now := time.Now()
		s.mu.Lock()
		for name := range s.deferred {
			if busy(name) || !s.retryEligible(name, now) {
				continue
			}
			if c, ok := stubs[name]; ok && !solved[name] {
				retry = append(retry, c)
			}
		}
		sort.Slice(retry, func(i, j int) bool {
			ai, bi := s.attempts[retry[i].Name], s.attempts[retry[j].Name]
			if ai != bi {
				return ai < bi
			}
			return priorityLess(retry[i], retry[j])
		})
		s.mu.Unlock()
		if len(retry) > remaining {
			retry = retry[:remaining]
		}
		for _, c := range retry {
			s.mu.Lock()
			delete(s.deferred, c.Name)
			s.mu.Unlock()
			log.Printf("Auto-retry deferred challenge: %s", c.Name)
			s.recordDecision("retry", c.Name, "cooldown_elapsed", "deferred mission re-entered window")
			picked = append(picked, c)
		}
	}
	for _, c := range picked {
		s.spawnOne(ctx, c.Name)
	}
}

func (s *Scheduler) retryEligible(name string, now time.Time) bool {
	if strings.HasPrefix(s.deferred[name], "stopped by operator") {
		return false
	}
	if max := s.cfg.MaxChallengeAttempts; max > 0 && s.attempts[name] >= max {
		return false
	}
	return now.Sub(s.deferTime[name]) >= time.Duration(s.cfg.DeferRetryCooldownS)*time.Second
}

// Reconcile platform containers against work that still owns a slot. This also
// repairs containers left behind by a failed close or a previous Agent process.
func (s *Scheduler) reconcileContainers() {
	s.mu.Lock()
	keep := make(map[string]bool, len(s.runs)+len(s.pending)+len(s.openContainers))
	for name := range s.runs {
		keep[name] = true
	}
	for name := range s.pending {
		keep[name] = true
	}
	for name := range s.openContainers {
		keep[name] = true
	}
	for name := range s.resumePending {
		keep[name] = true
	}
	s.mu.Unlock()
	if closed := s.pf.CloseStaleContainers(keep); len(closed) > 0 && s.poll != nil {
		s.poll.Refresh()
	}
}

// spawnOne pulls a challenge and starts its swarm (pull runs in a goroutine, so
// container startup does not block the loop).
func (s *Scheduler) spawnOne(ctx context.Context, name string) {
	stubs := s.poll.KnownStubs()
	ch, ok := stubs[name]
	if !ok {
		return
	}
	s.mu.Lock()
	if _, a := s.runs[name]; a || s.pending[name] || s.deferred[name] != "" {
		s.mu.Unlock()
		return
	}
	if len(s.runs)+len(s.pending) >= s.maxConc {
		s.mu.Unlock()
		return
	}
	if rg := s.remainingGlobal(); rg >= 0 && rg < float64(s.cfg.MinRemainingToSpawnS) {
		s.mu.Unlock()
		s.recordDecision("spawn_blocked", name, "global_budget", fmt.Sprintf("%.0fs remaining", rg))
		log.Printf("Skipping spawn of %s: only %.0fs of global budget left", name, rg)
		return
	}
	s.pending[name] = true
	s.mu.Unlock()
	if err := s.checkpoint(); err != nil {
		log.Printf("scheduler checkpoint failed: %v", err)
	}

	s.pulls.Add(1)
	go func() {
		defer s.pulls.Done()
		meta, err := s.pf.PullChallenge(ctx, ch)
		s.mu.Lock()
		delete(s.pending, name)
		if err != nil {
			s.spawnFailures[name]++
			deferred := false
			if s.spawnFailures[name] >= 2 {
				s.deferred[name] = "spawn failed: " + truncate(err.Error(), 100)
				s.deferTime[name] = time.Now()
				deferred = true
			}
			s.mu.Unlock()
			s.recordDecision("spawn_failed", name, "platform_pull", truncate(err.Error(), 100))
			if deferred {
				s.recordDecision("defer", name, "repeated_spawn_failure", "pull failed twice")
			}
			_ = s.checkpoint()
			log.Printf("Auto-spawn failed for %s: %v", name, err)
			// Schedule reconciliation on the coordinator loop. Running it here can
			// race with a new spawn and close a container missing from an old snapshot.
			if platform.IsAtCapacity(err) {
				select {
				case s.reconcileRequests <- struct{}{}:
				default: // a reconciliation is already queued
				}
			}
			return
		}
		delete(s.spawnFailures, name)
		s.openContainers[name] = true
		deadline := time.Now().Add(s.perChallengeLimit(meta))
		if previous, ok := s.resumePending[name]; ok && !previous.IsZero() && previous.Before(deadline) {
			deadline = previous
		}
		if time.Until(deadline) < time.Second {
			deadline = time.Now().Add(time.Second)
		}
		sctx, cancel := context.WithDeadline(ctx, deadline)
		initialScope := scope.ParseConnInfo(meta.ConnectionInfo)
		savedScope, savedRunID, hasSavedScope, scopeErr := scope.LoadOperatorScopeRun(s.cfg.StateDir, s.scopeIdentity(name))
		if hasSavedScope {
			initialScope = savedScope
			meta.ConnectionInfo = initialScope.Describe()
		}
		_, recovering := s.resumePending[name]
		runID := ""
		if recovering {
			runID = s.runIDs[name]
			if runID == "" {
				runID = savedRunID // migration from older checkpoints
			}
		} else if scopeErr == nil {
			runID, scopeErr = scope.NewRunID()
		}
		stateID := scope.MissionStateID(meta.Name, runID)
		bb, bbErr := blackboard.LoadOrNew(stateID, s.cfg.StateDir)
		tl, tlErr := traffic.LoadOrNew(stateID, s.cfg.StateDir)
		if bbErr != nil || tlErr != nil || scopeErr != nil {
			cancel()
			delete(s.openContainers, name)
			s.deferred[name] = "mission state load failed"
			s.deferTime[name] = time.Now()
			s.mu.Unlock()
			s.recordDecision("spawn_failed", name, "state_load", "blackboard, traffic, or scope load failed")
			_ = s.checkpoint()
			s.pf.ReleaseChallenge(name)
			if bbErr != nil {
				log.Printf("State load failed for %s: %v", name, bbErr)
			} else if tlErr != nil {
				log.Printf("Traffic load failed for %s: %v", name, tlErr)
			} else {
				log.Printf("Scope load failed for %s: %v", name, scopeErr)
			}
			return
		}
		// Count an attempt only when a solver actually starts. Platform capacity,
		// container startup and state-load failures must not exhaust this budget.
		if _, recovering := s.resumePending[name]; !recovering {
			s.attempts[name]++
		}
		delete(s.resumePending, name)
		s.runIDs[name] = runID
		sw := s.newSwarm(meta, initialScope, bb, tl)
		r := &run{sw: sw, cancel: cancel, done: make(chan struct{}), meta: meta, deadline: deadline}
		s.runs[name] = r
		s.mu.Unlock()
		if err := (runhistory.Store{StateDir: s.cfg.StateDir}).Start(stateID, meta.Name, "scheduler", initialScope.Describe(), time.Now()); err != nil {
			log.Printf("start run history %s: %v", stateID, err)
		}
		s.recordDecision("spawn", name, "slot_available", fmt.Sprintf("%d executor(s)", len(s.specs)))
		_ = s.checkpoint()
		log.Printf("Swarm spawned for %s (%d models)", name, len(s.specs))
		go func() {
			defer close(r.done)
			defer cancel()
			sw.Run(sctx)
		}()
	}()
}

func (s *Scheduler) newSwarm(meta mission.Meta, current scope.Scope, bb *blackboard.Blackboard, tl *traffic.Log) *swarm.Swarm {
	return &swarm.Swarm{
		Meta: meta, Scope: current, ModelSpecs: s.specs,
		Settings: s.cfg, Platform: s.pf, LLM: s.LLM, ModelProfiles: s.ModelProfiles,
		UseConsoleProfile: s.UseConsoleProfile,
		BB:                bb, TL: tl, GlobalRemaining: s.remainingGlobal,
	}
}

func (s *Scheduler) handleDeadlock(ctx context.Context, since time.Time) time.Time {
	solved := s.poll.KnownSolved()
	known := s.poll.KnownChallenges()
	s.mu.Lock()
	active := len(s.runs) + len(s.pending)
	unsolved := len(known) - len(solved)
	deferredAll := len(s.deferred) >= unsolved && unsolved > 0
	deferredCount := len(s.deferred)
	s.mu.Unlock()
	log.Printf("STATUS: %d solved, %d unsolved, %d active, %d deferred",
		len(solved), unsolved, active, deferredCount)
	if active > 0 || !deferredAll {
		return time.Time{}
	}
	if since.IsZero() {
		since = time.Now()
	}
	if time.Since(since) >= time.Duration(s.cfg.AutoResumeIdleS)*time.Second {
		s.mu.Lock()
		n := len(s.deferred)
		s.deferred = map[string]string{}
		s.mu.Unlock()
		log.Printf("Auto-resuming %d deferred challenges after deadlock", n)
		s.recordDecision("auto_resume", "", "deadlock", fmt.Sprintf("%d deferred missions", n))
		s.autoSpawnUnsolved(ctx)
		return time.Time{}
	}
	return since
}

func (s *Scheduler) shutdown() {
	s.mu.Lock()
	for _, r := range s.runs {
		r.cancel()
	}
	runs := make([]*run, 0, len(s.runs))
	for _, r := range s.runs {
		runs = append(runs, r)
	}
	s.mu.Unlock()
	if s.poll != nil {
		s.poll.Stop()
	}
	pullsDone := make(chan struct{})
	go func() {
		s.pulls.Wait()
		close(pullsDone)
	}()
	select {
	case <-pullsDone:
	case <-time.After(3 * time.Second):
		log.Printf("Timed out waiting for pending platform pulls to stop")
	}
	s.pf.Close()
	runsDone := make(chan struct{})
	go func() {
		for _, r := range runs {
			<-r.done
		}
		close(runsDone)
	}()
	select {
	case <-runsDone:
	case <-time.After(8 * time.Second):
		log.Printf("Timed out waiting for active swarms to stop")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
