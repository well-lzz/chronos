// Package llm resolves a model spec ("deepseek/deepseek-chat", "qwen/qwen-plus",
// "glm/glm-4-plus") to a go-openai client pointed at the right OpenAI-compatible
// endpoint, plus the concrete model id. DeepSeek is the default.
package llm

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/well-lzz/chronos/internal/config"
	"github.com/well-lzz/chronos/internal/llmconfig"
	"github.com/well-lzz/chronos/internal/llmrec"
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
		d := &keyPoolDoer{base: &http.Client{}, keys: append([]string(nil), keys...)}
		// A fresh client is built each cycle, so starting every one at keys[0] would
		// skew nearly all first-requests onto one key. Seed the rotation at a random
		// index so load spreads evenly across the pool from the first call.
		d.next.Store(uint64(rand.Intn(len(keys))))
		oc.HTTPClient = d
	}
	return openai.NewClientWithConfig(oc)
}

// ProfileKeys returns the key set a profile-built client rotates over. For a
// DeepSeek endpoint it pools the profile's own key together with the environment
// DeepSeek pool (DEEPSEEK_API_KEY / _2 / _KEYS), de-duplicated, so both keys share
// the load instead of the profile's single key carrying everything. For any other
// provider it is just the profile key.
func ProfileKeys(p llmconfig.Profile, cfg config.Settings) []string {
	base := strings.TrimSpace(p.BaseURL)
	if cfg.TSecBenchHosted {
		base = config.TSecBenchGatewayURL(base)
	}
	return poolKeys(base, strings.TrimSpace(p.APIKey), cfg)
}

func poolKeys(base, profileKey string, cfg config.Settings) []string {
	var keys []string
	seen := map[string]bool{}
	add := func(k string) {
		k = strings.TrimSpace(k)
		if k != "" && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	add(profileKey)
	if isDeepSeekEndpoint(base, cfg) {
		for _, k := range cfg.DeepSeekKeys {
			add(k)
		}
		add(cfg.DeepSeekKey)
	}
	return keys
}

// isDeepSeekEndpoint reports whether base points at the same host as the
// configured DeepSeek endpoint (after the hosted gateway rewrite is applied to
// both), so env DeepSeek keys are only ever pooled onto a DeepSeek endpoint.
func isDeepSeekEndpoint(base string, cfg config.Settings) bool {
	ds := strings.TrimSpace(cfg.DeepSeekBaseURL)
	if ds == "" {
		ds = "https://api.deepseek.com/v1"
	}
	if cfg.TSecBenchHosted {
		ds = config.TSecBenchGatewayURL(ds)
	}
	return sameHost(base, ds)
}

func sameHost(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	if err1 != nil || err2 != nil {
		return false
	}
	ha, hb := ua.Hostname(), ub.Hostname()
	return ha != "" && strings.EqualFold(ha, hb)
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
	return fromProfileWithSettings(p, cfg, true)
}

// FromExactProfileWithSettings uses exactly the operator-specified key. This is
// for models-file entries, where environment key pools must not silently alter
// a model's credentials.
func FromExactProfileWithSettings(p llmconfig.Profile, cfg config.Settings) (*openai.Client, string, error) {
	return fromProfileWithSettings(p, cfg, false)
}

func fromProfileWithSettings(p llmconfig.Profile, cfg config.Settings, pool bool) (*openai.Client, string, error) {
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
	// Pool the profile key with the env DeepSeek pool when the endpoint is DeepSeek,
	// so both keys are exercised (and both issue the profile's model) instead of the
	// single profile key carrying the whole run.
	keys := []string{key}
	if pool {
		keys = poolKeys(base, key, cfg)
	}
	return newClient(base, keys), model, nil
}

// Ping issues a tiny completion to confirm a profile's base URL, key, and model
// actually work. Used by the console's "test" button.
func Ping(ctx context.Context, p llmconfig.Profile) error {
	return PingRecorded(ctx, p, nil)
}

func PingRecorded(ctx context.Context, p llmconfig.Profile, recorder *llmrec.Recorder) error {
	client, model, err := FromProfile(p)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	request := openai.ChatCompletionRequest{
		Model:     model,
		Messages:  []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "ping"}},
		MaxTokens: 1,
	}
	if recorder != nil {
		_, err = recorder.Call(ctx, "console", "model-test", request, client.CreateChatCompletion)
	} else {
		_, err = client.CreateChatCompletion(ctx, request)
	}
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
