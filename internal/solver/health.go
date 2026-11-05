package solver

import (
	"context"
	"sync/atomic"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

type loopHealth struct {
	lastBoundary atomic.Int64
	modelSince   atomic.Int64
	activeTools  atomic.Int32
}

func (s *Solver) modelCall(ctx context.Context, req openai.ChatCompletionRequest, complete func(context.Context, openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error)) (openai.ChatCompletionResponse, error) {
	callCtx, cancel := context.WithTimeout(ctx, s.modelTimeout)
	defer cancel()
	s.health.modelSince.Store(time.Now().Unix())
	defer s.health.modelSince.Store(0)
	defer func() { s.health.lastBoundary.Store(time.Now().Unix()) }()
	return complete(callCtx, req)
}

// Health is an observation, never authority to change scope or verdicts.
// Waiting for an event or operator is not classified as a stalled model call.
func (s *Solver) Health() map[string]any {
	s.messageMu.Lock()
	queued := len(s.pendingUser)
	s.messageMu.Unlock()
	out := map[string]any{"last_boundary_unix": s.health.lastBoundary.Load(), "model_started_unix": s.health.modelSince.Load(), "active_tools": s.health.activeTools.Load(), "queued_inputs": queued}
	if s.asyncDelegate {
		jobs := s.initJobs()
		jobs.mu.Lock()
		out["running_children"] = jobs.slots
		undelivered := 0
		for _, job := range jobs.items {
			if !job.delivered {
				undelivered++
			}
		}
		out["undelivered_children"] = undelivered
		jobs.mu.Unlock()
	}
	return out
}
