package solver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

func TestDamagedSessionStopsBeforeModelAndKeepsEvidence(t *testing.T) {
	for _, content := range []string{"bad middle record\n", "{\"version\":2,\"type\":\"turn\"}\n"} {
		path := filepath.Join(t.TempDir(), "session.jsonl")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		s := New(Options{SessionPath: path, Client: openai.NewClient("test"), Complete: func(context.Context, openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
			t.Fatal("model called after failed recovery")
			return openai.ChatCompletionResponse{}, nil
		}})
		if got := s.RunUntilDoneOrGaveUp(context.Background()); got.Status != StatusStorageError {
			t.Fatalf("result: %+v", got)
		}
		if err := s.Guide("continue"); err == nil {
			t.Fatal("acknowledged an unrecorded instruction")
		}
		if got := s.dispatch(context.Background(), "notify", `{}`); !strings.Contains(got, "not executed") {
			t.Fatal(got)
		}
		data, _ := os.ReadFile(path)
		if string(data) != content || s.sessionPath != path {
			t.Fatal("damaged session was overwritten or persistence disabled")
		}
	}
}

func TestJournalWriteFailureBlocksFollowingTools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	s := New(Options{SessionPath: path})
	if err := s.start(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.Guide("must be durable"); err == nil {
		t.Fatal("accepted instruction despite failed write")
	}
	if len(s.pendingUser) != 0 {
		t.Fatal("uncommitted input was queued")
	}
	if got := s.dispatch(context.Background(), "notify", `{}`); !strings.Contains(got, "storage_error") {
		t.Fatal(got)
	}
}
