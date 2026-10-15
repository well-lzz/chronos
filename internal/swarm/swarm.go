// Package swarm runs several executors in parallel on one mission. They share
// the mission's blackboard and traffic log and COLLABORATE — no racing, no
// sibling cancellation. Each executor searches until the termination hook says
// stop, a hard bump ceiling is hit, or the wall-clock budget ends. A flag (if
// any) is recorded but does not cancel the others.
package swarm

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/config"
	"github.com/well-lzz/chronos/internal/llm"
	"github.com/well-lzz/chronos/internal/mission"
	"github.com/well-lzz/chronos/internal/scope"
	"github.com/well-lzz/chronos/internal/solver"
	"github.com/well-lzz/chronos/internal/termination"
	"github.com/well-lzz/chronos/internal/traffic"
)

type Swarm struct {
	Meta       mission.Meta
	Scope      scope.Scope
	ModelSpecs []string
	Settings   config.Settings
	BB         *blackboard.Blackboard
	TL         *traffic.Log

	Tokens int64 // shared, mission-wide
}

// Run launches one executor per model spec and waits for all to finish. ctx
// carries the wall-clock budget. Returns the flag if any executor found one.
func (s *Swarm) Run(ctx context.Context) string {
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
			continue
		}
		wg.Add(1)
		go func(spec, model string, client *openai.Client) {
			defer wg.Done()
			notify := func(m string) { log.Printf("[%s/%s] %s", s.Meta.Name, spec, m) }
			sv := solver.New(spec, model, client, s.Meta, s.Scope, s.BB, s.TL,
				s.Settings.AllowLocalExec, deadline, &s.Tokens, notify)
			if f := s.runExecutor(ctx, sv, spec); f != "" {
				flagMu.Lock()
				flag = f
				flagMu.Unlock()
			}
		}(spec, model, client)
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

	for {
		if ctx.Err() != nil {
			return ""
		}
		markerBefore := s.BB.ProgressMarker()
		res := sv.RunUntilDoneOrGaveUp(ctx)
		cycles++

		switch res.Status {
		case solver.StatusFlagFound:
			log.Printf("[%s/%s] flag found: %s", s.Meta.Name, spec, res.Flag)
			return res.Flag // recorded; does NOT cancel siblings
		case solver.StatusCancelled:
			return ""
		}

		// User-defined termination.
		st := termination.CycleState{
			StepCount: res.StepCount, BumpCount: bumps, Cycles: cycles,
			ProgressMarker: s.BB.ProgressMarker(), PrevProgressMark: markerBefore,
			ElapsedSeconds: time.Since(started).Seconds(),
		}
		if termination.ShouldTerminate(s.BB, st) {
			log.Printf("[%s/%s] termination hook requested stop", s.Meta.Name, spec)
			return ""
		}

		if res.Status == solver.StatusGaveUp || res.Status == solver.StatusError {
			bumps++
			if bumps > maxBumps {
				log.Printf("[%s/%s] max bumps (%d) reached", s.Meta.Name, spec, maxBumps)
				return ""
			}
			if retry > 0 {
				select {
				case <-ctx.Done():
					return ""
				case <-time.After(retry):
				}
			}
			sv.Bump("Keep covering new ground; read the shared working record and build on it.")
			continue
		}
		return ""
	}
}

// TotalTokens returns the mission-wide token count.
func (s *Swarm) TotalTokens() int { return int(atomic.LoadInt64(&s.Tokens)) }
