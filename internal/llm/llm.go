// Package llm resolves a model spec ("deepseek/deepseek-chat", "qwen/qwen-plus",
// "glm/glm-4-plus") to a go-openai client pointed at the right OpenAI-compatible
// endpoint, plus the concrete model id. DeepSeek is the default.
package llm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/well-lzz/chronos/internal/config"
	"github.com/well-lzz/chronos/internal/llmconfig"
)

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type keyPoolDoer struct {
	base httpDoer
	keys []string
	next atomic.Uint64
}

func (p *keyPoolDoer) Do(req *http.Request) (*http.Response, error) {
	start := int(p.next.Add(1)-1) % len(p.keys)
	var lastErr error
	for attempt := 0; attempt < len(p.keys); attempt++ {
		request := req.Clone(req.Context())
		if attempt > 0 && req.Body != nil {
			if req.GetBody == nil {
				return nil, lastErr
			}
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			request.Body = body
		}
		request.Header.Set("Authorization", "Bearer "+p.keys[(start+attempt)%len(p.keys)])
		resp, err := p.base.Do(request)
		if err != nil {
			lastErr = err
			continue
		}
		if !retryableStatus(resp.StatusCode) || attempt == len(p.keys)-1 {
			return resp, nil
		}
		if resp.Body != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
		}
		lastErr = fmt.Errorf("LLM HTTP %d", resp.StatusCode)
	}
	return nil, lastErr
}

func retryableStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests || status >= 500
}

func newClient(baseURL string, keys []string) *openai.Client {
	oc := openai.DefaultConfig(keys[0])
	oc.BaseURL = baseURL
	if len(keys) > 1 {
		oc.HTTPClient = &keyPoolDoer{base: &http.Client{}, keys: append([]string(nil), keys...)}
	}
	return openai.NewClientWithConfig(oc)
}

// FromProfile builds a client for any OpenAI-compatible endpoint described by an
// operator-configured profile (base URL + key + model). This is the generic path
// that lets the agent run on any mainstream provider.
func FromProfile(p llmconfig.Profile) (*openai.Client, string, error) {
	return FromProfileWithSettings(p, config.Settings{})
}

// FromProfileWithSettings applies runtime-specific endpoint routing before
// building the client. In TSecBench hosted mode this prevents a console-saved
// public endpoint from bypassing the competition gateway rewrite.
func FromProfileWithSettings(p llmconfig.Profile, cfg config.Settings) (*openai.Client, string, error) {
	base := strings.TrimSpace(p.BaseURL)
	if cfg.TSecBenchHosted {
		base = config.TSecBenchGatewayURL(base)
	}
	key := strings.TrimSpace(p.APIKey)
	model := strings.TrimSpace(p.Model)
	if base == "" {
		return nil, "", fmt.Errorf("no base URL configured for the model")
	}
	if key == "" {
		return nil, "", fmt.Errorf("no API key configured for the model")
	}
	if model == "" {
		return nil, "", fmt.Errorf("no model id configured")
	}
	return newClient(base, []string{key}), model, nil
}

// Ping issues a tiny completion to confirm a profile's base URL, key, and model
// actually work. Used by the console's "test" button.
func Ping(ctx context.Context, p llmconfig.Profile) error {
	client, model, err := FromProfile(p)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err = client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model:     model,
		Messages:  []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "ping"}},
		MaxTokens: 1,
	})
	return err
}

// Resolve returns a configured client and the concrete model id for a spec.
func Resolve(spec string, cfg config.Settings) (*openai.Client, string, error) {
	provider, model := split(spec)
	var baseURL string
	var keys []string
	switch provider {
	case "deepseek", "":
		baseURL, keys = cfg.DeepSeekBaseURL, cfg.DeepSeekKeys
		if len(keys) == 0 && cfg.DeepSeekKey != "" {
			keys = []string{cfg.DeepSeekKey}
		}
		if model == "" {
			model = "deepseek-chat"
		}
	case "qwen":
		baseURL, keys = cfg.QwenBaseURL, []string{cfg.QwenKey}
		if model == "" {
			model = "qwen-plus"
		}
	case "glm":
		baseURL, keys = cfg.GLMBaseURL, []string{cfg.GLMKey}
		if model == "" {
			model = cfg.GLMModelID
		}
	default:
		return nil, "", fmt.Errorf("unknown provider %q in spec %q", provider, spec)
	}
	if len(keys) == 0 || strings.TrimSpace(keys[0]) == "" {
		return nil, "", fmt.Errorf("no API key for provider %q (set the matching *_API_KEY)", provider)
	}
	return newClient(baseURL, keys), model, nil
}

func split(spec string) (provider, model string) {
	spec = strings.TrimSpace(spec)
	if i := strings.IndexByte(spec, '/'); i >= 0 {
		return spec[:i], spec[i+1:]
	}
	return spec, ""
}
