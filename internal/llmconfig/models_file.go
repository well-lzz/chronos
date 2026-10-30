package llmconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// ModelEntry names one operator-configured executor model. The name is a stable
// executor identity; provider is only a display label and never an allowlist.
type ModelEntry struct {
	Name                string       `json:"name"`
	Provider            string       `json:"provider"`
	Model               string       `json:"model"`
	BaseURL             string       `json:"base_url"`
	APIKey              string       `json:"api_key,omitempty"`
	APIKeyEnv           string       `json:"api_key_env,omitempty"`
	ContextWindowTokens int          `json:"context_window_tokens,omitempty"`
	Fallbacks           []ModelEntry `json:"fallbacks,omitempty"`
	Priority            int          `json:"priority,omitempty"`
}

var modelNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// LoadModelsFile loads arbitrary OpenAI-compatible models in operator order.
// API keys can be supplied directly or through an environment variable.
func LoadModelsFile(path string) ([]string, map[string]Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var file struct {
		Models []ModelEntry `json:"models"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return nil, nil, fmt.Errorf("parse models file: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, nil, fmt.Errorf("models file must contain one JSON object")
	}
	if len(file.Models) == 0 {
		return nil, nil, fmt.Errorf("models file must contain at least one model")
	}
	names := make([]string, 0, len(file.Models))
	profiles := make(map[string]Profile, len(file.Models))
	for i, item := range file.Models {
		name := strings.TrimSpace(item.Name)
		if !modelNamePattern.MatchString(name) {
			return nil, nil, fmt.Errorf("model %d: name must use letters, digits, dots, underscores, or hyphens", i+1)
		}
		if _, exists := profiles[name]; exists {
			return nil, nil, fmt.Errorf("duplicate model name %q", name)
		}
		if item.APIKey != "" && item.APIKeyEnv != "" {
			return nil, nil, fmt.Errorf("model %q: choose api_key or api_key_env", name)
		}
		key := strings.TrimSpace(item.APIKey)
		if item.APIKeyEnv != "" {
			key = strings.TrimSpace(os.Getenv(strings.TrimSpace(item.APIKeyEnv)))
		}
		base := strings.TrimSpace(item.BaseURL)
		u, parseErr := url.Parse(base)
		if parseErr != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, nil, fmt.Errorf("model %q: base_url must be an absolute HTTP(S) URL", name)
		}
		if strings.TrimSpace(item.Model) == "" || key == "" {
			return nil, nil, fmt.Errorf("model %q: model and API key are required", name)
		}
		if item.ContextWindowTokens != 0 && (item.ContextWindowTokens < 4096 || item.ContextWindowTokens > 2_000_000) {
			return nil, nil, fmt.Errorf("model %q: context_window_tokens must be between 4096 and 2000000", name)
		}
		profiles[name] = Profile{
			Provider: strings.TrimSpace(item.Provider), Model: strings.TrimSpace(item.Model),
			BaseURL: base, APIKey: key, ContextWindowTokens: item.ContextWindowTokens,
		}
		for j, backup := range item.Fallbacks {
			if len(backup.Fallbacks) > 0 {
				return nil, nil, fmt.Errorf("model %q fallback %d: nested fallbacks are not supported", name, j+1)
			}
			if backup.APIKey != "" && backup.APIKeyEnv != "" {
				return nil, nil, fmt.Errorf("model %q fallback %d: choose api_key or api_key_env", name, j+1)
			}
			backupKey := strings.TrimSpace(backup.APIKey)
			if backup.APIKeyEnv != "" {
				backupKey = strings.TrimSpace(os.Getenv(strings.TrimSpace(backup.APIKeyEnv)))
			}
			backupBase := strings.TrimSpace(backup.BaseURL)
			backupURL, backupErr := url.Parse(backupBase)
			if backupErr != nil || backupURL.Hostname() == "" || (backupURL.Scheme != "http" && backupURL.Scheme != "https") || strings.TrimSpace(backup.Model) == "" || backupKey == "" {
				return nil, nil, fmt.Errorf("model %q fallback %d: valid base_url, model and API key are required", name, j+1)
			}
			if backup.ContextWindowTokens != 0 && (backup.ContextWindowTokens < 4096 || backup.ContextWindowTokens > 2_000_000) {
				return nil, nil, fmt.Errorf("model %q fallback %d: invalid context_window_tokens", name, j+1)
			}
			p := profiles[name]
			priority := backup.Priority
			if priority == 0 {
				priority = j + 1
			}
			p.Fallbacks = append(p.Fallbacks, FallbackProfile{Name: backup.Name, Provider: backup.Provider,
				Model: strings.TrimSpace(backup.Model), BaseURL: backupBase, APIKey: backupKey,
				ContextWindowTokens: backup.ContextWindowTokens, Priority: priority})
			profiles[name] = p
		}
		names = append(names, name)
	}
	return names, profiles, nil
}
