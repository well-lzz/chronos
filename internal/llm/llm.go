// Package llm resolves a model spec ("deepseek/deepseek-chat", "qwen/qwen-plus",
// "glm/glm-4-plus") to a go-openai client pointed at the right OpenAI-compatible
// endpoint, plus the concrete model id. DeepSeek is the default.
package llm

import (
	"context"
	"fmt"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/well-lzz/chronos/internal/config"
	"github.com/well-lzz/chronos/internal/llmconfig"
)

// FromProfile builds a client for any OpenAI-compatible endpoint described by an
// operator-configured profile (base URL + key + model). This is the generic path
// that lets the agent run on any mainstream provider.
func FromProfile(p llmconfig.Profile) (*openai.Client, string, error) {
	base := strings.TrimSpace(p.BaseURL)
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
	oc := openai.DefaultConfig(key)
	oc.BaseURL = base
	return openai.NewClientWithConfig(oc), model, nil
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
	var baseURL, key string
	switch provider {
	case "deepseek", "":
		baseURL, key = cfg.DeepSeekBaseURL, cfg.DeepSeekKey
		if model == "" {
			model = "deepseek-chat"
		}
	case "qwen":
		baseURL, key = cfg.QwenBaseURL, cfg.QwenKey
		if model == "" {
			model = "qwen-plus"
		}
	case "glm":
		baseURL, key = cfg.GLMBaseURL, cfg.GLMKey
		if model == "" {
			model = cfg.GLMModelID
		}
	default:
		return nil, "", fmt.Errorf("unknown provider %q in spec %q", provider, spec)
	}
	if key == "" {
		return nil, "", fmt.Errorf("no API key for provider %q (set the matching *_API_KEY)", provider)
	}
	oc := openai.DefaultConfig(key)
	oc.BaseURL = baseURL
	return openai.NewClientWithConfig(oc), model, nil
}

func split(spec string) (provider, model string) {
	spec = strings.TrimSpace(spec)
	if i := strings.IndexByte(spec, '/'); i >= 0 {
		return spec[:i], spec[i+1:]
	}
	return spec, ""
}
