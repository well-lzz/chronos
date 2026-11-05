package solver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/scope"
)

func TestAsyncChildDoesNotBlockParentAndCompletionSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	bb, _ := blackboard.LoadOrNew("async", dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var parentCalls atomic.Int32
	complete := func(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
		if strings.Contains(req.Messages[0].Content, "slow child") {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return openai.ChatCompletionResponse{}, ctx.Err()
			}
		} else {
			parentCalls.Add(1)
		}
		return openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Role: "assistant", Content: "finished"}}}}, nil
	}
	opts := Options{ModelSpec: "root", Model: "test", Client: openai.NewClient("test"), BB: bb, Scope: scope.Parse([]string{"target.test"}),
		AsyncDelegate: true, MissionContext: ctx, Complete: complete, SubagentRoot: filepath.Join(dir, "children"), SessionPath: filepath.Join(dir, "root.jsonl")}
	s := New(opts)
	s.delegateStepPool = 20
	s.cycleRemaining = 40
	started := s.toolDelegateStart(ctx, `{"task":"slow child","max_steps":3}`)
	var receipt struct {
		Status  string `json:"status"`
		AgentID string `json:"agent_id"`
	}
	_ = json.Unmarshal([]byte(started), &receipt)
	if receipt.Status != "started" {
		t.Fatal(started)
	}
	defer s.CloseJobs()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("child did not start")
	}
	if got := s.RunUntilDoneOrGaveUp(ctx); got.Status != StatusWaitingEvent || parentCalls.Load() != 1 {
		t.Fatalf("parent did not advance independently: %+v", got)
	}
	close(release)
	job := s.jobs.items[receipt.AgentID]
	select {
	case <-job.done:
	case <-time.After(time.Second):
		t.Fatal("child did not finish")
	}
	// Simulate a restart before completion was consumed. No model call is needed
	// to restore the completed result from metadata and board references.
	restored := New(opts)
	if err := restored.RestoreJobs(); err != nil {
		t.Fatal(err)
	}
	if err := restored.PollJobs(); err != nil {
		t.Fatal(err)
	}
	if err := restored.PollJobs(); err != nil {
		t.Fatal(err)
	}
	restored.flushUser()
	count := 0
	for _, m := range restored.messageSnapshot() {
		if strings.Contains(m.Content, "Child report") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("completion delivered %d times", count)
	}
	// A later process rereads the same receipt but session input IDs deduplicate it.
	again := New(opts)
	if err := again.RestoreJobs(); err != nil {
		t.Fatal(err)
	}
	if err := again.PollJobs(); err != nil {
		t.Fatal(err)
	}
	if len(again.pendingUser) != 0 {
		t.Fatal("replayed an already-consumed completion")
	}
	opts.AsyncDelegate = false
	if err := New(opts).RestoreJobs(); err == nil {
		t.Fatal("incompatible async mode silently abandoned child state")
	}
}

func TestInputDeduplicationAndBackpressureSurviveCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	s := New(Options{SessionPath: path})
	if err := s.DeliverInput("operator-1", "operator", "inspect evidence"); err != nil {
		t.Fatal(err)
	}
	s.flushUser()
	s.compactMessagesTo(1024, 1)
	restored := New(Options{SessionPath: path})
	if err := restored.DeliverInput("operator-1", "operator", "inspect evidence"); err != nil {
		t.Fatal(err)
	}
	if len(restored.pendingUser) != 0 {
		t.Fatal("duplicated a consumed input")
	}
	for i := range 128 {
		if err := restored.DeliverInput(time.Unix(int64(i), 0).String(), "operator", "queued"); err != nil {
			t.Fatal(err)
		}
	}
	if err := restored.DeliverInput("overflow", "operator", "retry"); err == nil {
		t.Fatal("missing inbox backpressure")
	}
	if restored.StorageError() != nil {
		t.Fatal("queue saturation was treated as damaged storage")
	}
}

func TestAsyncHandoffRetainsCompletionAndNarrowsOldChild(t *testing.T) {
	dir := t.TempDir()
	bb, _ := blackboard.LoadOrNew("handoff-async", dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	s := New(Options{ModelSpec: "root", Model: "test", Client: openai.NewClient("test"), BB: bb, Scope: scope.Parse([]string{"old.test", "keep.test"}), AsyncDelegate: true, MissionContext: ctx,
		Complete: func(ctx context.Context, _ openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
			close(started)
			<-ctx.Done()
			return openai.ChatCompletionResponse{}, ctx.Err()
		}})
	s.delegateStepPool = 20
	s.cycleRemaining = 40
	var receipt struct {
		AgentID string `json:"agent_id"`
	}
	_ = json.Unmarshal([]byte(s.toolDelegateStart(ctx, `{"task":"wait","max_steps":2}`)), &receipt)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("child not started")
	}
	fresh := s.FreshHandoff("")
	fresh.SetScope(scope.Parse([]string{"keep.test"}))
	child := s.jobs.items[receipt.AgentID].record.solver
	if child.scopeSnapshot().Allows("old.test") {
		t.Fatal("handoff left old child's scope active")
	}
	fresh.toolDelegateCancel(`{"agent_id":"` + receipt.AgentID + `"}`)
	fresh.CloseJobs()
	if err := fresh.PollJobs(); err != nil {
		t.Fatal(err)
	}
	if len(fresh.pendingUser) < 1 {
		t.Fatal("completion not delivered to replacement executor")
	}
}
