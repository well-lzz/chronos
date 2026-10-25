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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/config"
	"github.com/well-lzz/chronos/internal/llm"
	"github.com/well-lzz/chronos/internal/llmconfig"
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
	Kind string `json:"kind"` // tool_call | tool_result | text | operator | assistant | status
	Tool string `json:"tool,omitempty"`
	Text string `json:"text"`
}

const activityRing = 300

var submissionCooldowns = []time.Duration{0, 30 * time.Second, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute}

const submitLockWrongLimit = 4

func providerBackoff(failures int) time.Duration {
	if failures < 1 {
		return 0
	}
	if failures > 5 {
		failures = 5
	}
	return time.Duration(1<<uint(failures-1)) * time.Second
}

func (s *Swarm) sessionPath(spec string) string {
	if s.BB == nil || s.BB.Path == "" {
		return ""
	}
	root := filepath.Dir(filepath.Dir(s.BB.Path))
	mission := strings.TrimSuffix(filepath.Base(s.BB.Path), filepath.Ext(s.BB.Path))
	sum := sha256.Sum256([]byte(spec))
	return filepath.Join(root, "sessions", mission, hex.EncodeToString(sum[:8])+".jsonl")
}

type Swarm struct {
	Meta       mission.Meta
	Scope      scope.Scope
	ModelSpecs []string
	Settings   config.Settings
	BB         *blackboard.Blackboard
	TL         *traffic.Log
	Platform   platform.Platform // nil = engagement (no oracle, record-only)
	LLM        *llmconfig.Store  // operator-editable model profile; nil = env/flag only

	Tokens int64 // shared, mission-wide

	mu       sync.Mutex
	solvers  map[string]*solver.Solver
	activity map[string][]ActEvent
	status   map[string]string
	lastErr  map[string]string // last surfaced error per spec (dedup consecutive)
	runCtx   context.Context
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
	s.mu.Lock()
	s.solvers = map[string]*solver.Solver{}
	s.activity = map[string][]ActEvent{}
	s.status = map[string]string{}
	s.lastErr = map[string]string{}
	s.mu.Unlock()
	s.subMu.Lock()
	s.submitted = map[string]bool{}
	s.submitCount = map[string]int{}
	s.lastSubmit = map[string]time.Time{}
	s.confirmed = map[string]bool{}
	s.subMu.Unlock()
}

// Run launches one executor per model spec and waits for all to finish. ctx
// carries the wall-clock budget. Returns the flag if any executor found one.
func (s *Swarm) Run(ctx context.Context) string {
	s.init()
	s.mu.Lock()
	s.runCtx = ctx
	s.mu.Unlock()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(time.Duration(s.Settings.ChallengeTimeLimitS) * time.Second)
	}
	var wg sync.WaitGroup
	var flagMu sync.Mutex
	flag := ""

	for _, spec := range s.ModelSpecs {
		sp := spec
		// Resolve the model fresh each cycle: the operator-configured profile wins
		// when set (any OpenAI-compatible provider), otherwise fall back to the
		// env/flag provider. The executor starts even when nothing is configured
		// yet — the first cycle reports a config error and recovers once the
		// operator fills in the model settings in the console.
		resolve := func() (*openai.Client, string, error) {
			if s.LLM != nil && s.LLM.Active() {
				return llm.FromProfileWithSettings(s.LLM.Get(), s.Settings)
			}
			return llm.Resolve(sp, s.Settings)
		}
		var submitFn solver.SubmitFn
		if s.Platform != nil {
			submitFn = func(fl string) (string, bool, bool) { return s.trySubmit(fl, sp) }
		}
		sv := solver.New(solver.Options{
			ModelSpec: spec, Meta: s.Meta, Scope: s.Scope,
			BB: s.BB, TL: s.TL, AllowExec: s.Settings.AllowLocalExec, InsecureTLS: s.Settings.TargetInsecureTLS, Deadline: deadline,
			Tokens: &s.Tokens, Notify: func(m string) { log.Printf("[%s/%s] %s", s.Meta.Name, sp, m) },
			SubmitFn: submitFn, Record: s.recorder(spec), Resolve: resolve, SessionPath: s.sessionPath(spec),
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
	providerFailures := 0
	s.setStatus(spec, "running")

	for {
		if ctx.Err() != nil {
			s.setStatus(spec, "cancelled")
			return ""
		}
		for s.paused.Load() && ctx.Err() == nil {
			time.Sleep(300 * time.Millisecond)
		}
		s.setStatus(spec, "running")
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
		case solver.StatusConfigError:
			// The model isn't configured/authorized yet. Surface it, mark the
			// executor as waiting, and keep polling — the moment the operator saves
			// a working model in the console, the next cycle picks it up. Does not
			// count against the retry ceiling.
			s.emitErrorOnce(spec, res.StepCount, res.Findings+"  → 打开控制台右上角「模型设置」填写后自动恢复")
			s.setStatus(spec, "needs-config")
			select {
			case <-ctx.Done():
				s.setStatus(spec, "cancelled")
				return ""
			case <-time.After(3 * time.Second):
			}
			continue
		case solver.StatusRetryableError:
			// A provider outage is not evidence that the exploration strategy
			// failed. Keep the conversation intact and retry within the mission
			// deadline, with bounded exponential backoff.
			providerFailures++
			s.emitErrorOnce(spec, res.StepCount, res.Findings)
			s.setStatus(spec, "waiting-provider")
			select {
			case <-ctx.Done():
				s.setStatus(spec, "cancelled")
				return ""
			case <-time.After(providerBackoff(providerFailures)):
			}
			continue
		}
		providerFailures = 0

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

		if res.Status == solver.StatusError && res.Findings != "" {
			// Surface the failure so the console shows WHY nothing is coming back
			// (bad key, unreachable endpoint, …) instead of "waiting for activity".
			s.emitErrorOnce(spec, res.StepCount, res.Findings)
		} else {
			s.mu.Lock()
			delete(s.lastErr, spec)
			s.mu.Unlock()
		}

		if res.Status == solver.StatusGaveUp || res.Status == solver.StatusError {
			if s.BB.ProgressMarker() > markerBefore {
				// MAX_BUMPS is a stall ceiling, not a lifetime limit. A long
				// mission that keeps producing durable findings can use its full
				// wall-clock budget.
				bumps = 0
			} else {
				bumps++
			}
			if bumps > maxBumps {
				log.Printf("[%s/%s] max bumps (%d) reached", s.Meta.Name, spec, maxBumps)
				s.emit(spec, res.StepCount, "text", "", "Executor stopped: retry ceiling reached after repeated failures.")
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

// emitErrorOnce surfaces an executor error to the console, collapsing a run of
// identical errors (e.g. a repeating 401) into a single visible line.
func (s *Swarm) emitErrorOnce(spec string, step int, text string) {
	s.mu.Lock()
	same := s.lastErr[spec] == text
	s.lastErr[spec] = text
	s.mu.Unlock()
	if same {
		return
	}
	s.emit(spec, step, "error", "", text)
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

// UpdateScope atomically replaces the structural egress boundary for the swarm
// and every live solver. Operator chat cannot call this method; only the
// authenticated console scope endpoint can.
func (s *Swarm) UpdateScope(next scope.Scope) {
	s.mu.Lock()
	s.Scope = next
	s.Meta.ConnectionInfo = next.Describe()
	solvers := make(map[string]*solver.Solver, len(s.solvers))
	for spec, sv := range s.solvers {
		solvers[spec] = sv
	}
	s.mu.Unlock()
	for spec, sv := range solvers {
		sv.SetScope(next)
		s.emit(spec, 0, "status", "", "Scope updated by operator: "+next.Describe())
	}
}

// CurrentScope returns the immutable current boundary snapshot.
func (s *Swarm) CurrentScope() scope.Scope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Scope
}

// Chat records a human turn, queues it for autonomous execution, and launches
// a separate no-tools model turn that guarantees a direct conversational reply.
func (s *Swarm) Chat(spec, message string) bool {
	message = strings.TrimSpace(message)
	if message == "" {
		return false
	}
	if len(message) > 4000 {
		message = message[:4000]
	}
	s.mu.Lock()
	sv := s.solvers[spec]
	ctx := s.runCtx
	s.mu.Unlock()
	if sv == nil {
		return false
	}
	s.emit(spec, 0, "operator", "", message)
	s.emit(spec, 0, "status", "", "已收到指令，Agent 正在回复；所有执行动作仍受当前 Scope 强制约束。")
	sv.Guide(message)
	if ctx == nil {
		ctx = context.Background()
	}
	go func(paused bool) {
		replyCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		reply, err := sv.ReplyToOperator(replyCtx, message, paused)
		if err != nil {
			s.emitErrorOnce(spec, 0, "Agent reply failed: "+err.Error())
			return
		}
		s.emit(spec, 0, "assistant", "", reply)
	}(s.paused.Load())
	return true
}

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
