package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	openai "github.com/sashabaranov/go-openai"

	"github.com/well-lzz/chronos/internal/config"
)

func TestDeepSeekSecondEnvironmentKeyReachesRealClientRequests(t *testing.T) {
	var mu sync.Mutex
	var authorizations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		if !json.Valid(body) {
			t.Errorf("go-openai sent invalid JSON: %q", body)
		}
		mu.Lock()
		authorizations = append(authorizations, req.Header.Get("Authorization"))
		call := len(authorizations)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"test","object":"chat.completion","created":1,"model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer server.Close()

	t.Setenv("TSECBENCH_HOSTED", "false")
	t.Setenv("DEEPSEEK_BASE_URL", server.URL)
	t.Setenv("DEEPSEEK_API_KEY", "first-platform-value")
	t.Setenv("DEEPSEEK_API_KEY_2", "second-platform-value")
	t.Setenv("DEEPSEEK_API_KEYS", "")

	client, model, err := Resolve("deepseek/deepseek-chat", config.Load())
	if err != nil {
		t.Fatal(err)
	}
	request := openai.ChatCompletionRequest{
		Model: model,
		Messages: []openai.ChatCompletionMessage{{
			Role: openai.ChatMessageRoleUser, Content: "ping",
		}},
	}
	for i := 0; i < 2; i++ {
		response, err := client.CreateChatCompletion(context.Background(), request)
		if err != nil {
			t.Fatalf("CreateChatCompletion(%d): %v", i, err)
		}
		if len(response.Choices) != 1 || response.Choices[0].Message.Content != "ok" {
			t.Fatalf("unexpected response: %+v", response)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"Bearer first-platform-value",
		"Bearer second-platform-value",
		"Bearer second-platform-value",
	}
	if len(authorizations) != len(want) {
		t.Fatalf("authorization calls = %#v, want %#v", authorizations, want)
	}
	for i := range want {
		if authorizations[i] != want[i] {
			t.Fatalf("authorization[%d] = %q, want %q", i, authorizations[i], want[i])
		}
	}
}
