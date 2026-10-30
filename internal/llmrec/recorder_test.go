package llmrec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

func TestRecordsFailedAndSuccessfulCallsSeparately(t *testing.T) {
	dir := t.TempDir()
	r := Shared(dir, true)
	request := openai.ChatCompletionRequest{Model: "model-a", Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hello"}}}
	_, err := r.Call(context.Background(), "mission-1", "worker-1", request, func(context.Context, openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
		return openai.ChatCompletionResponse{}, errors.New("network failed")
	})
	if err == nil {
		t.Fatal("expected provider failure")
	}
	_, err = r.Call(context.Background(), "mission-1", "worker-1", request, func(context.Context, openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
		return openai.ChatCompletionResponse{Usage: openai.Usage{PromptTokens: 7, CompletionTokens: 3}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := r.Recent(10)
	if len(rows) != 2 || rows[0].Status != "error" || rows[1].Status != "ok" || rows[1].TotalTokens != 10 {
		t.Fatalf("usage rows: %+v", rows)
	}
	trace, ok := r.TraceByID("mission-1", rows[1].ID)
	if !ok || trace.Request.Messages[0].Content != "hello" || trace.Response == nil {
		t.Fatal("trace was not saved")
	}
	if info, err := os.Stat(filepath.Join(dir, "llm_usage.jsonl")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("usage file mode: %v %v", info, err)
	}
}
