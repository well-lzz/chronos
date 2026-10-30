package solver

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/well-lzz/chronos/internal/blackboard"
)

func TestPauseAfterModelResponseDefersToolsAndNextTurn(t *testing.T) {
	board, err := blackboard.LoadOrNew("pause-solver", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var paused atomic.Bool
	var calls atomic.Int32
	resume := make(chan struct{})
	blocked := make(chan struct{})
	var once sync.Once
	wait := func(ctx context.Context) error {
		if !paused.Load() {
			return ctx.Err()
		}
		once.Do(func() { close(blocked) })
		select {
		case <-resume:
			return ctx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	complete := func(_ context.Context, _ openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
		if calls.Add(1) == 1 {
			paused.Store(true) // operator presses pause while the model request completes
			return openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{
				Role: openai.ChatMessageRoleAssistant,
				ToolCalls: []openai.ToolCall{{ID: "call-1", Type: openai.ToolTypeFunction,
					Function: openai.FunctionCall{Name: "add_fact", Arguments: `{"statement":"observed fact"}`}}},
			}}}}, nil
		}
		return openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{
			Role: openai.ChatMessageRoleAssistant, Content: "done",
		}}}}, nil
	}
	s := New(Options{Model: "test", Client: openai.NewClient("test"), Complete: complete,
		BB: board, WaitPaused: wait, Paused: paused.Load})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	finished := make(chan Result, 1)
	go func() { finished <- s.RunUntilDoneOrGaveUp(ctx) }()
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatal("solver did not stop at the pause gate")
	}
	if board.Counts()["facts"] != 0 || calls.Load() != 1 {
		t.Fatalf("work advanced while paused: facts=%d calls=%d", board.Counts()["facts"], calls.Load())
	}
	paused.Store(false)
	close(resume)
	select {
	case result := <-finished:
		if result.Status != StatusGaveUp || board.Counts()["facts"] != 1 || calls.Load() != 2 {
			t.Fatalf("resume did not continue the same turn: %+v facts=%d calls=%d", result, board.Counts()["facts"], calls.Load())
		}
	case <-ctx.Done():
		t.Fatal("solver did not resume")
	}
}

func TestPauseBetweenSequentialToolsStopsTheRestOfTheBatch(t *testing.T) {
	board, err := blackboard.LoadOrNew("pause-batch", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var paused atomic.Bool
	blocked, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	completeCalls := 0
	s := New(Options{Model: "test", Client: openai.NewClient("test"), BB: board,
		Complete: func(context.Context, openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
			completeCalls++
			if completeCalls == 1 {
				return openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{
					Role: openai.ChatMessageRoleAssistant,
					ToolCalls: []openai.ToolCall{
						{ID: "one", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "add_fact", Arguments: `{"statement":"first"}`}},
						{ID: "two", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "add_fact", Arguments: `{"statement":"second"}`}},
					},
				}}}}, nil
			}
			return openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "done"}}}}, nil
		},
		RecordEvent: func(event LoopEvent) {
			if event.Kind == "tool_result" && event.ToolCallID == "one" {
				paused.Store(true)
			}
		},
		WaitPaused: func(ctx context.Context) error {
			if !paused.Load() {
				return ctx.Err()
			}
			once.Do(func() { close(blocked) })
			select {
			case <-resume:
				return ctx.Err()
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	finished := make(chan Result, 1)
	go func() { finished <- s.RunUntilDoneOrGaveUp(ctx) }()
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatal("second tool was not gated")
	}
	if got := board.Counts()["facts"]; got != 1 {
		t.Fatalf("%d facts recorded during pause, want only the first", got)
	}
	paused.Store(false)
	close(resume)
	select {
	case result := <-finished:
		if result.Status != StatusGaveUp || board.Counts()["facts"] != 2 {
			t.Fatalf("batch did not resume: %+v facts=%d", result, board.Counts()["facts"])
		}
	case <-ctx.Done():
		t.Fatal("batch did not resume")
	}
}
