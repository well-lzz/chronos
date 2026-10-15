// Package scheduler is the mechanical coordinator (no LLM): it polls the
// platform, fills a bounded concurrency window with the highest-value unsolved
// challenges, runs one collaborative swarm per challenge, reaps finished ones
// and rolls deferred ones back in after a cooldown. A platform-confirmed solve
// (from the poller) kills the swarm — that is how a flag mission terminates.
package scheduler

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/config"
	"github.com/well-lzz/chronos/internal/mission"
	"github.com/well-lzz/chronos/internal/platform"
	"github.com/well-lzz/chronos/internal/poller"
	"github.com/well-lzz/chronos/internal/scope"
	"github.com/well-lzz/chronos/internal/swarm"
	"github.com/well-lzz/chronos/internal/traffic"
)

type run struct {
	sw     *swarm.Swarm
	cancel context.CancelFunc
	done   chan struct{}
	meta   mission.Meta
	solved bool
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

	mu             sync.Mutex
	runs           map[string]*run
	deferred       map[string]string
	attempts       map[string]int
	deferTime      map[string]time.Time
	pending        map[string]bool
	openContainers map[string]bool
	spawnFailures  map[string]int

	cmds chan Command
}

func New(pf platform.Platform, cfg config.Settings, specs []string) *Scheduler {
	mc := cfg.MaxConcurrent
	if mc <= 0 {
		mc = 3
	}
	return &Scheduler{
		pf: pf, cfg: cfg, specs: specs, maxConc: mc, runStart: time.Now(),
		runs: map[string]*run{}, deferred: map[string]string{}, attempts: map[string]int{},
		deferTime: map[string]time.Time{}, pending: map[string]bool{}, openContainers: map[string]bool{},
		spawnFailures: map[string]int{}, cmds: make(chan Command, 64),
	}
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
func (s *Scheduler) Run(ctx context.Context) {
	pmin, pmax := 5*time.Second, 15*time.Second
	s.poll = poller.New(s.pf, (pmin+pmax)/2, (pmax-pmin)/2)
	s.poll.Start(ctx)
	log.Printf("Scheduler starting: %d model(s), %d challenges", len(s.specs), len(s.poll.KnownChallenges()))

	s.autoSpawnUnsolved(ctx)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	lastStatus := time.Now()
	var deadlockSince time.Time

	for {
		select {
		case <-ctx.Done():
			s.shutdown()
			return
		case ev := <-s.poll.Events():
			s.handleEvent(ctx, ev)
		case c := <-s.cmds:
			s.handleCommand(ctx, c)
		case <-ticker.C:
		}

		if s.reapAndRefill(ctx) {
			// slot freed; refill already attempted inside
		}

		if time.Since(lastStatus) >= 60*time.Second {
			lastStatus = time.Now()
			s.autoSpawnUnsolved(ctx)
			deadlockSince = s.handleDeadlock(ctx, deadlockSince)
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
			return
		}
	}
}

func (s *Scheduler) handleEvent(ctx context.Context, ev poller.Event) {
	switch ev.Kind {
	case poller.ChallengeSolved:
		log.Printf("Solved: %s", ev.Name)
		s.killSwarm(ev.Name, true)
	case poller.NewChallenge:
		s.spawnOne(ctx, ev.Name)
	}
}

func (s *Scheduler) handleCommand(ctx context.Context, c Command) {
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
	case "bump":
		if r != nil {
			r.sw.Bump(c.Model, c.Message)
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
	}
}

// reapAndRefill removes finished runs (deferring unsolved ones) and refills.
func (s *Scheduler) reapAndRefill(ctx context.Context) bool {
	var freed bool
	s.mu.Lock()
	for name, r := range s.runs {
		select {
		case <-r.done:
			if !r.solved && s.deferred[name] == "" {
				s.deferred[name] = "solver attempts exhausted"
				s.deferTime[name] = time.Now()
				log.Printf("Deferred: %s", name)
			} else {
				log.Printf("Swarm finished: %s", name)
			}
			delete(s.runs, name)
			delete(s.openContainers, name)
			s.pf.ReleaseChallenge(name)
			freed = true
		default:
		}
	}
	s.mu.Unlock()
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
		if limit < 60 {
			limit = 60
		}
	}
	return time.Duration(limit) * time.Second
}

func guessKillchain(m mission.Meta) bool {
	blob := strings.ToLower(m.Category + " " + m.Description + " " + m.Name)
	for _, k := range []string{"killchain", "渗透", "横向", "内网", "多阶段", "跳板", "kill chain"} {
		if strings.Contains(blob, k) {
			return true
		}
	}
	return false
}

func priorityLess(a, b platform.Challenge) bool {
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
	sort.Slice(cands, func(i, j int) bool { return priorityLess(cands[i], cands[j]) })

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
		s.mu.Unlock()
		sort.Slice(retry, func(i, j int) bool {
			ai, bi := s.attempts[retry[i].Name], s.attempts[retry[j].Name]
			if ai != bi {
				return ai < bi
			}
			return priorityLess(retry[i], retry[j])
		})
		if len(retry) > remaining {
			retry = retry[:remaining]
		}
		for _, c := range retry {
			s.mu.Lock()
			delete(s.deferred, c.Name)
			s.mu.Unlock()
			log.Printf("Auto-retry deferred challenge: %s", c.Name)
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
		log.Printf("Skipping spawn of %s: only %.0fs of global budget left", name, rg)
		return
	}
	s.pending[name] = true
	s.attempts[name]++
	s.mu.Unlock()

	go func() {
		meta, err := s.pf.PullChallenge(ch)
		s.mu.Lock()
		delete(s.pending, name)
		if err != nil {
			s.spawnFailures[name]++
			if s.spawnFailures[name] >= 2 {
				s.deferred[name] = "spawn failed: " + truncate(err.Error(), 100)
				s.deferTime[name] = time.Now()
			}
			s.mu.Unlock()
			log.Printf("Auto-spawn failed for %s: %v", name, err)
			return
		}
		delete(s.spawnFailures, name)
		s.openContainers[name] = true
		sctx, cancel := context.WithTimeout(ctx, s.perChallengeLimit(meta))
		sw := &swarm.Swarm{
			Meta: meta, Scope: scope.ParseConnInfo(meta.ConnectionInfo), ModelSpecs: s.specs,
			Settings: s.cfg, Platform: s.pf,
			BB: blackboard.LoadOrNew(meta.Name, s.cfg.StateDir),
			TL: traffic.LoadOrNew(meta.Name, s.cfg.StateDir),
		}
		r := &run{sw: sw, cancel: cancel, done: make(chan struct{}), meta: meta}
		s.runs[name] = r
		s.mu.Unlock()
		log.Printf("Swarm spawned for %s (%d models)", name, len(s.specs))
		go func() {
			defer close(r.done)
			defer cancel()
			sw.Run(sctx)
		}()
	}()
}

func (s *Scheduler) handleDeadlock(ctx context.Context, since time.Time) time.Time {
	solved := s.poll.KnownSolved()
	known := s.poll.KnownChallenges()
	s.mu.Lock()
	active := len(s.runs) + len(s.pending)
	unsolved := len(known) - len(solved)
	deferredAll := len(s.deferred) >= unsolved && unsolved > 0
	s.mu.Unlock()
	log.Printf("STATUS: %d solved, %d unsolved, %d active, %d deferred",
		len(solved), unsolved, active, len(s.deferred))
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
	for _, r := range runs {
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
		}
	}
	if s.poll != nil {
		s.poll.Stop()
	}
	s.pf.Close()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
