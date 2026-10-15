// Package swarm runs several executors in parallel on one mission. They share
// the mission's blackboard and traffic log and COLLABORATE — no racing, no
// sibling cancellation. Each executor searches until the termination hook says
// stop, a hard bump ceiling is hit, or the wall-clock budget ends. When a
// Platform is set, submit_flag goes through it (deduped + cooldown + a circuit
// breaker on blind guessing); a confirmed flag is recorded but does not cancel
// the others (the scheduler's poller kills the swarm on a platform-confirmed
// solve).
package swarm

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/config"
	"github.com/well-lzz/chronos/internal/llm"
	"github.com/well-lzz/chronos/internal/mission"
	"github.com/well-lzz/chronos/internal/platform"
	"github.com/well-lzz/chronos/internal/scope"
	"github.com/well-lzz/chronos/internal/solver"
	"github.com/well-lzz/chronos/internal/termination"
	"github.com/well-lzz/chronos/internal/traffic"
)

// ActEvent is one observable step of an executor (for the web console). Seq is
// a per-swarm monotonic cursor (covers operator messages too, which have no
// solver step), so the chat/stream orders human and agent messages together.
type ActEvent struct {
	Seq  int64  `json:"seq"`
	Step int    `json:"step"`
	Kind string `json:"kind"` // tool_call | tool_result | text | operator
	Tool string `json:"tool,omitempty"`
	Text string `json:"text"`
}

const activityRing = 300

var submissionCooldowns = []time.Duration{0, 30 * time.Second, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute}

const submitLockWrongLimit = 4

type Swarm struct {
	Meta       mission.Meta
	Scope      scope.Scope
	ModelSpecs []string
	Settings   config.Settings
	BB         *blackboard.Blackboard
	TL         *traffic.Log
	Platform   platform.Platform // nil = engagement (no oracle, record-only)

	Tokens int64 // shared, mission-wide

	mu       sync.Mutex
	solvers  map[string]*solver.Solver
	activity map[string][]ActEvent
	status   map[string]string
	paused   atomic.Bool
	evSeq    int64 // monotonic activity cursor (atomic)

	// submit state (shared across executors)
	subMu        sync.Mutex
	submitted    map[string]bool
	submitCount  map[string]int
	lastSubmit   map[string]time.Time
	confirmed    map[string]bool
	wrongTotal   int
	submitLocked bool
}

func (s *Swarm) init() {
	s.solvers = map[string]*solver.Solver{}
	s.activity = map[string][]ActEvent{}
	s.status = map[string]string{}
	s.submitted = map[string]bool{}
	s.submitCount = map[string]int{}
	s.lastSubmit = map[string]time.Time{}
	s.confirmed = map[string]bool{}
}

// Run launches one executor per model spec and waits for all to finish. ctx
// carries the wall-clock budget. Returns the flag if any executor found one.
func (s *Swarm) Run(ctx context.Context) string {
	s.init()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(time.Duration(s.Settings.ChallengeTimeLimitS) * time.Second)
	}
	var wg sync.WaitGroup
	var flagMu sync.Mutex
	flag := ""

	for _, spec := range s.ModelSpecs {
		client, model, err := llm.Resolve(spec, s.Settings)
		if err != nil {
			log.Printf("[%s/%s] cannot start: %v", s.Meta.Name, spec, err)
			s.setStatus(spec, "error")
			continue
		}
		var submitFn solver.SubmitFn
		if s.Platform != nil {
			sp := spec
			submitFn = func(fl string) (string, bool, bool) { return s.trySubmit(fl, sp) }
		}
		sv := solver.New(solver.Options{
			ModelSpec: spec, Model: model, Client: client, Meta: s.Meta, Scope: s.Scope,
			BB: s.BB, TL: s.TL, AllowExec: s.Settings.AllowLocalExec, Deadline: deadline,
			Tokens: &s.Tokens, Notify: func(m string) { log.Printf("[%s/%s] %s", s.Meta.Name, spec, m) },
			SubmitFn: submitFn, Record: s.recorder(spec),
		})
		s.mu.Lock()
		s.solvers[spec] = sv
		s.mu.Unlock()
		wg.Add(1)
		go func(spec string, sv *solver.Solver) {
			defer wg.Done()
			if f := s.runExecutor(ctx, sv, spec); f != "" {
				flagMu.Lock()
				flag = f
				flagMu.Unlock()
			}
		}(spec, sv)
	}
	wg.Wait()
	return flag
}

func (s *Swarm) runExecutor(ctx context.Context, sv *solver.Solver, spec string) string {
	maxBumps := s.Settings.MaxBumps
	if maxBumps <= 0 {
		maxBumps = 20
	}
	retry := time.Duration(s.Settings.SolverRetryDelayS * float64(time.Second))
	started := time.Now()
	bumps, cycles := 0, 0
	s.setStatus(spec, "running")

	for {
		if ctx.Err() != nil {
			s.setStatus(spec, "cancelled")
			return ""
		}
		for s.paused.Load() && ctx.Err() == nil {
			time.Sleep(300 * time.Millisecond)
		}
		markerBefore := s.BB.ProgressMarker()
		res := sv.RunUntilDoneOrGaveUp(ctx)
		cycles++

		switch res.Status {
		case solver.StatusFlagFound:
			log.Printf("[%s/%s] flag found: %s", s.Meta.Name, spec, res.Flag)
			s.setStatus(spec, "flag_found")
			return res.Flag // recorded; does NOT cancel siblings
		case solver.StatusCancelled:
			s.setStatus(spec, "cancelled")
			return ""
		}

		st := termination.CycleState{
			StepCount: res.StepCount, BumpCount: bumps, Cycles: cycles,
			ProgressMarker: s.BB.ProgressMarker(), PrevProgressMark: markerBefore,
			ElapsedSeconds: time.Since(started).Seconds(),
		}
		if termination.ShouldTerminate(s.BB, st) {
			log.Printf("[%s/%s] termination hook requested stop", s.Meta.Name, spec)
			s.setStatus(spec, "finished")
			return ""
		}

		if res.Status == solver.StatusGaveUp || res.Status == solver.StatusError {
			bumps++
			if bumps > maxBumps {
				log.Printf("[%s/%s] max bumps (%d) reached", s.Meta.Name, spec, maxBumps)
				s.setStatus(spec, "finished")
				return ""
			}
			if retry > 0 {
				select {
				case <-ctx.Done():
					s.setStatus(spec, "cancelled")
					return ""
				case <-time.After(retry):
				}
			}
			sv.Bump("Keep covering new ground; read the shared working record and build on it.")
			continue
		}
		s.setStatus(spec, "finished")
		return ""
	}
}

// trySubmit forwards a flag to the platform with dedup, escalating cooldown, and
// a circuit breaker on repeated wrong guesses. Returns (display, confirmed, complete).
func (s *Swarm) trySubmit(flag, spec string) (string, bool, bool) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	norm := strings.TrimSpace(flag)
	if s.confirmed[norm] {
		return "ALREADY SOLVED — flag already confirmed: " + norm, true, true
	}
	if s.submitLocked {
		return fmt.Sprintf("SUBMIT LOCKED — %d incorrect submissions on this task. Stop guessing; only submit with hard evidence.", s.wrongTotal), false, false
	}
	if s.submitted[norm] {
		return "INCORRECT — already tried this exact flag.", false, false
	}
	wrong := s.submitCount[spec]
	idx := wrong
	if idx >= len(submissionCooldowns) {
		idx = len(submissionCooldowns) - 1
	}
	if cd := submissionCooldowns[idx]; cd > 0 {
		if elapsed := time.Since(s.lastSubmit[spec]); elapsed < cd {
			return fmt.Sprintf("COOLDOWN — wait %ds before submitting again (%d incorrect so far).", int((cd - elapsed).Seconds()), wrong), false, false
		}
	}
	s.submitted[norm] = true
	res, err := s.Platform.SubmitFlag(s.Meta.Name, flag)
	if err != nil {
		return "submit_flag error: " + err.Error(), false, false
	}
	switch res.Status {
	case "already_solved":
		s.confirmed[norm] = true
		return res.Display, true, true
	case "correct":
		if res.ChallengeComplete || s.Meta.FlagCount <= 1 {
			s.confirmed[norm] = true
			return res.Display, true, true
		}
		return res.Display + "\nPARTIAL CREDIT — more flags remain; work a different surface.", true, false
	default:
		s.submitCount[spec] = wrong + 1
		s.lastSubmit[spec] = time.Now()
		s.wrongTotal++
		if s.wrongTotal >= submitLockWrongLimit {
			s.submitLocked = true
			log.Printf("[%s] submit circuit breaker tripped after %d wrong", s.Meta.Name, s.wrongTotal)
		}
		return res.Display, false, false
	}
}

// --- live observation (web console) ---

func (s *Swarm) recorder(spec string) func(step int, kind, tool, text string) {
	return func(step int, kind, tool, text string) {
		s.emit(spec, step, kind, tool, text)
	}
}

// emit appends an event to an executor's ring with a monotonic Seq.
func (s *Swarm) emit(spec string, step int, kind, tool, text string) {
	if len(text) > 4000 {
		text = text[:4000]
	}
	seq := atomic.AddInt64(&s.evSeq, 1)
	s.mu.Lock()
	buf := append(s.activity[spec], ActEvent{Seq: seq, Step: step, Kind: kind, Tool: tool, Text: text})
	if len(buf) > activityRing {
		buf = buf[len(buf)-activityRing:]
	}
	s.activity[spec] = buf
	s.mu.Unlock()
}

func (s *Swarm) setStatus(spec, st string) {
	s.mu.Lock()
	s.status[spec] = st
	s.mu.Unlock()
}

// Activity returns this executor's events with Seq > after (the chat/stream cursor).
func (s *Swarm) Activity(spec string, after int) []ActEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ActEvent
	for _, e := range s.activity[spec] {
		if e.Seq > int64(after) {
			out = append(out, e)
		}
	}
	return out
}

// Status returns a per-model status view for the web snapshot.
func (s *Swarm) Status() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	agents := map[string]any{}
	for _, spec := range s.ModelSpecs {
		st := s.status[spec]
		if st == "" {
			st = "pending"
		}
		agents[spec] = map[string]any{"spec": spec, "status": st, "steps": len(s.activity[spec])}
	}
	return map[string]any{"challenge": s.Meta.Name, "paused": s.paused.Load(), "agents": agents}
}

func (s *Swarm) Pause()  { s.paused.Store(true) }
func (s *Swarm) Resume() { s.paused.Store(false) }

// Bump injects operator guidance into one running executor and records it in the
// activity stream so the console chat shows the human's message in order.
func (s *Swarm) Bump(spec, message string) bool {
	s.mu.Lock()
	sv := s.solvers[spec]
	s.mu.Unlock()
	if sv == nil {
		return false
	}
	s.emit(spec, 0, "operator", "", message)
	sv.Bump("[Operator guidance] " + message)
	return true
}

// TotalTokens returns the mission-wide token count.
func (s *Swarm) TotalTokens() int { return int(atomic.LoadInt64(&s.Tokens)) }
