package scheduler

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/console"
	"github.com/well-lzz/chronos/internal/runhistory"
	"github.com/well-lzz/chronos/internal/scope"
	"github.com/well-lzz/chronos/internal/traffic"
)

func (s *Scheduler) scopeIdentity(challenge string) string {
	return "scheduler\x00" + s.cfg.PlatformMode + "\x00" + s.cfg.PlatformBase() + "\x00" + s.cfg.BenchmarkToken + "\x00" + challenge
}

// ScopeConfigFor returns the live authorization boundary for one mission.
func (s *Scheduler) ScopeConfigFor(challenge string) console.ScopeConfig {
	s.mu.Lock()
	r := s.runs[challenge]
	transitioning := r != nil && r.transitioning
	s.mu.Unlock()
	if r == nil || r.sw == nil {
		return console.ScopeConfig{Supported: true, Editable: false}
	}
	current := r.sw.CurrentScope()
	return console.ScopeConfig{Supported: true, Editable: !transitioning,
		Targets: current.Entries(), Description: current.Describe()}
}

// UpdateScopeFor starts an isolated run for the new target boundary. The old
// swarm is cancelled and its blackboard, traffic, and sessions stay archived.
func (s *Scheduler) UpdateScopeFor(challenge string, targets []string) error {
	if strings.TrimSpace(challenge) == "" {
		return fmt.Errorf("select a mission before changing scope")
	}
	next, err := scope.ParseOperatorTargets(targets)
	if err != nil {
		return err
	}
	s.scopeUpdateMu.Lock()
	defer s.scopeUpdateMu.Unlock()
	s.mu.Lock()
	r := s.runs[challenge]
	if r == nil || r.sw == nil {
		s.mu.Unlock()
		return fmt.Errorf("mission %q is not running", challenge)
	}
	if r.transitioning || r.solved {
		s.mu.Unlock()
		return fmt.Errorf("mission %q is finishing or changing scope", challenge)
	}
	if slices.Equal(r.sw.CurrentScope().Entries(), next.Entries()) {
		s.mu.Unlock()
		return nil
	}
	r.transitioning = true
	s.mu.Unlock()
	completed := false
	defer func() {
		if !completed {
			s.mu.Lock()
			if s.runs[challenge] == r {
				r.transitioning = false
			}
			s.mu.Unlock()
		}
	}()

	runID, err := scope.NewRunID()
	if err != nil {
		return err
	}
	stateID := scope.MissionStateID(r.meta.Name, runID)
	bb, err := blackboard.LoadOrNew(stateID, s.cfg.StateDir)
	if err != nil {
		return fmt.Errorf("prepare isolated blackboard: %w", err)
	}
	tl, err := traffic.LoadOrNew(stateID, s.cfg.StateDir)
	if err != nil {
		return fmt.Errorf("prepare isolated traffic log: %w", err)
	}
	if err := scope.SaveOperatorScopeRun(s.cfg.StateDir, s.scopeIdentity(challenge), next, runID); err != nil {
		return err
	}
	if r.cancel != nil {
		r.cancel()
	}
	if r.done != nil {
		<-r.done
	}
	s.mu.Lock()
	parent := s.runCtx
	s.mu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	if parent.Err() != nil {
		return fmt.Errorf("scheduler stopped before the new scope could start")
	}
	deadline := time.Now().Add(s.perChallengeLimit(r.meta))
	newCtx, cancel := context.WithDeadline(parent, deadline)
	meta := r.meta
	meta.ConnectionInfo = next.Describe()
	sw := s.newSwarm(meta, next, bb, tl)
	if r.sw.Paused() {
		sw.Pause()
	}
	replacement := &run{sw: sw, cancel: cancel, done: make(chan struct{}), meta: meta, deadline: deadline}
	s.mu.Lock()
	if s.runs[challenge] != r || r.solved {
		s.mu.Unlock()
		cancel()
		return fmt.Errorf("mission %q ended during scope change", challenge)
	}
	s.runs[challenge] = replacement
	s.runIDs[challenge] = runID
	s.mu.Unlock()
	completed = true
	history := runhistory.Store{StateDir: s.cfg.StateDir}
	if r.sw.BB != nil {
		if err := history.Finish(r.sw.BB.MissionID, "scope_changed", time.Now()); err != nil {
			log.Printf("finish prior scope history for %s: %v", challenge, err)
		}
	}
	if err := history.Start(stateID, meta.Name, "scheduler", next.Describe(), time.Now()); err != nil {
		log.Printf("start new scope history for %s: %v", challenge, err)
	}
	go func() {
		defer close(replacement.done)
		defer cancel()
		sw.Run(newCtx)
	}()
	s.recordDecision("scope_restart", challenge, "operator", "new isolated run "+stateID+"; scope "+next.Describe())
	if err := s.checkpoint(); err != nil {
		log.Printf("scope restart checkpoint failed for %s: %v", challenge, err)
	}
	return nil
}
