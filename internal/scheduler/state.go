package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/well-lzz/chronos/internal/durable"
	"os"
	"path/filepath"
	"time"
)

// schedulerState contains only mechanical coordination state. Model windows
// are already journaled separately by Solver under sessions/<mission>/.
type schedulerState struct {
	Version       int                  `json:"version"`
	RunStart      time.Time            `json:"run_start"`
	Attempts      map[string]int       `json:"attempts"`
	Deferred      map[string]string    `json:"deferred"`
	DeferTime     map[string]time.Time `json:"defer_time"`
	ResumePending map[string]time.Time `json:"resume_pending"`
	RunIDs        map[string]string    `json:"run_ids,omitempty"`
	EventSeq      uint64               `json:"event_seq,omitempty"`
	Events        []OrchestrationEvent `json:"events,omitempty"`
}

func (s *Scheduler) statePath() string {
	if s.cfg.StateDir == "" {
		return ""
	}
	identity := sha256.Sum256([]byte(s.cfg.PlatformMode + "\x00" + s.cfg.PlatformBase() + "\x00" + s.cfg.BenchmarkToken))
	return filepath.Join(s.cfg.StateDir, "scheduler", hex.EncodeToString(identity[:8])+".json")
}

func (s *Scheduler) loadState() error {
	path := s.statePath()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var state schedulerState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode scheduler state: %w", err)
	}
	if state.Version != 1 {
		return fmt.Errorf("unsupported scheduler state version %d", state.Version)
	}
	if !state.RunStart.IsZero() {
		s.runStart = state.RunStart
	}
	for name, attempts := range state.Attempts {
		s.attempts[name] = attempts
	}
	for name, reason := range state.Deferred {
		s.deferred[name] = reason
	}
	for name, at := range state.DeferTime {
		s.deferTime[name] = at
	}
	for name, deadline := range state.ResumePending {
		s.resumePending[name] = deadline
	}
	for name, runID := range state.RunIDs {
		s.runIDs[name] = runID
	}
	s.eventSeq = state.EventSeq
	s.events = append(s.events, state.Events...)
	return nil
}

// checkpoint serializes snapshots so an older asynchronous pull completion
// cannot overwrite a newer coordinator state. Rename makes crash tails safe.
func (s *Scheduler) checkpoint() (err error) {
	defer func() {
		if err != nil {
			s.mu.Lock()
			if s.storageErr == nil {
				s.storageErr = err
			}
			cancel := s.storageCancel
			s.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
	}()
	path := s.statePath()
	if path == "" {
		return nil
	}
	s.checkpointMu.Lock()
	defer s.checkpointMu.Unlock()
	s.mu.Lock()
	state := schedulerState{Version: 1, RunStart: s.runStart,
		Attempts: make(map[string]int, len(s.attempts)), Deferred: make(map[string]string, len(s.deferred)),
		DeferTime: make(map[string]time.Time, len(s.deferTime)), ResumePending: make(map[string]time.Time),
		RunIDs: make(map[string]string, len(s.runIDs))}
	for name, n := range s.attempts {
		state.Attempts[name] = n
	}
	for name, reason := range s.deferred {
		state.Deferred[name] = reason
	}
	for name, at := range s.deferTime {
		state.DeferTime[name] = at
	}
	for name, deadline := range s.resumePending {
		state.ResumePending[name] = deadline
	}
	for name, runID := range s.runIDs {
		state.RunIDs[name] = runID
	}
	for name, r := range s.runs {
		state.ResumePending[name] = r.deadline
	}
	for name := range s.pending {
		if _, ok := state.ResumePending[name]; !ok {
			state.ResumePending[name] = time.Time{}
		}
	}
	state.EventSeq = s.eventSeq
	state.Events = append(state.Events, s.events...)
	s.mu.Unlock()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return durable.WriteFile(path, data)
}

func (s *Scheduler) clearState() error {
	path := s.statePath()
	if path == "" {
		return nil
	}
	s.checkpointMu.Lock()
	defer s.checkpointMu.Unlock()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
