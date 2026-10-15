// Package llm resolves a model spec ("deepseek/deepseek-chat", "qwen/qwen-plus",
// "glm/glm-4-plus") to a go-openai client pointed at the right OpenAI-compatible
// endpoint, plus the concrete model id. DeepSeek is the default.
package llm

import (
	"fmt"
	"strings"

	openai "github.com/sashabaranov/go-openai"

	"github.com/well-lzz/chronos/internal/config"
)

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
