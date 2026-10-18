package llm

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/well-lzz/chronos/internal/config"
	"github.com/well-lzz/chronos/internal/llmconfig"
)

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func response(status int) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}
}

func TestKeyPoolRoundRobin(t *testing.T) {
	var authorizations []string
	pool := &keyPoolDoer{
		base: doerFunc(func(req *http.Request) (*http.Response, error) {
			authorizations = append(authorizations, req.Header.Get("Authorization"))
			return response(http.StatusOK), nil
		}),
		keys: []string{"key-one", "key-two"},
	}
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest(http.MethodPost, "https://example.test/chat", bytes.NewBufferString("{}"))
		resp, err := pool.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	want := []string{"Bearer key-one", "Bearer key-two", "Bearer key-one"}
	for i := range want {
		if authorizations[i] != want[i] {
			t.Fatalf("authorization[%d] = %q, want %q", i, authorizations[i], want[i])
		}
	}
}

func TestKeyPoolFailsOverOnRateLimit(t *testing.T) {
	var authorizations, bodies []string
	pool := &keyPoolDoer{
		base: doerFunc(func(req *http.Request) (*http.Response, error) {
			authorizations = append(authorizations, req.Header.Get("Authorization"))
			body, _ := io.ReadAll(req.Body)
			bodies = append(bodies, string(body))
			if len(authorizations) == 1 {
				return response(http.StatusTooManyRequests), nil
			}
			return response(http.StatusOK), nil
		}),
		keys: []string{"key-one", "key-two"},
	}
	req, _ := http.NewRequest(http.MethodPost, "https://example.test/chat", bytes.NewBufferString(`{"messages":[]}`))
	resp, err := pool.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(authorizations) != 2 || authorizations[0] != "Bearer key-one" || authorizations[1] != "Bearer key-two" {
		t.Fatalf("failover authorizations = %#v", authorizations)
	}
	if len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Fatalf("request body was not replayed safely: %#v", bodies)
	}
}

func TestProfileKeysPoolsDeepSeekEnvKeys(t *testing.T) {
	cfg := config.Settings{
		DeepSeekBaseURL: "https://api.deepseek.com/v1",
		DeepSeekKeys:    []string{"k1", "k2"},
	}
	// DeepSeek profile whose key is already in the env pool: both keys, deduped.
	p := llmconfig.Profile{Provider: "deepseek", Model: "deepseek-flash", BaseURL: "https://api.deepseek.com/v1", APIKey: "k1"}
	got := ProfileKeys(p, cfg)
	if len(got) != 2 || got[0] != "k1" || got[1] != "k2" {
		t.Fatalf("DeepSeek profile keys = %v, want [k1 k2]", got)
	}

	// A profile key not present in the env pool is added ahead of the env keys.
	p.APIKey = "k0"
	if got := ProfileKeys(p, cfg); len(got) != 3 || got[0] != "k0" {
		t.Fatalf("profile+env keys = %v, want [k0 k1 k2]", got)
	}

	// Non-DeepSeek endpoint must not borrow DeepSeek env keys.
	other := llmconfig.Profile{Provider: "openai", Model: "gpt-x", BaseURL: "https://api.openai.com/v1", APIKey: "sk-x"}
	if got := ProfileKeys(other, cfg); len(got) != 1 || got[0] != "sk-x" {
		t.Fatalf("non-DeepSeek profile keys = %v, want [sk-x]", got)
	}
}

func TestProfileKeysPoolsAcrossHostedGatewayRewrite(t *testing.T) {
	cfg := config.Settings{
		TSecBenchHosted: true,
		DeepSeekBaseURL: "https://api.deepseek.com/v1",
		DeepSeekKeys:    []string{"k1", "k2"},
	}
	// Profile saved the public DeepSeek URL; hosted mode rewrites both sides to the
	// gateway host, so they must still match and pool.
	p := llmconfig.Profile{Provider: "deepseek", Model: "deepseek-flash", BaseURL: "https://api.deepseek.com/v1", APIKey: "k1"}
	if got := ProfileKeys(p, cfg); len(got) != 2 {
		t.Fatalf("hosted DeepSeek profile keys = %v, want 2 keys", got)
	}
}

func TestFromProfileBuildsKeyPoolForDeepSeek(t *testing.T) {
	cfg := config.Settings{DeepSeekBaseURL: "https://api.deepseek.com/v1", DeepSeekKeys: []string{"k1", "k2"}}
	p := llmconfig.Profile{Provider: "deepseek", Model: "deepseek-flash", BaseURL: "https://api.deepseek.com/v1", APIKey: "k1"}
	client, model, err := FromProfileWithSettings(p, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if model != "deepseek-flash" {
		t.Fatalf("model = %q, want deepseek-flash", model)
	}
	if client == nil {
		t.Fatal("nil client")
	}
}
