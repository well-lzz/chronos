// Package llmconfig is the operator-editable LLM profile: which model backs the
// agent, persisted so it survives restarts and is configured from the console
// instead of environment variables. Any OpenAI-compatible endpoint works (a
// base URL + key + model), so mainstream providers are all reachable through the
// one generic path.
package llmconfig

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Profile is the active model configuration.
type Profile struct {
	Provider            string            `json:"provider"`                        // display label / preset id (e.g. "deepseek", "openai", "custom")
	Model               string            `json:"model"`                           // concrete model id, e.g. "deepseek-chat"
	BaseURL             string            `json:"base_url"`                        // OpenAI-compatible base, e.g. "https://api.deepseek.com/v1"
	APIKey              string            `json:"api_key"`                         // provider key (stored locally, 0600)
	ContextWindowTokens int               `json:"context_window_tokens,omitempty"` // operator-confirmed model window; 0 = conservative fallback
	Fallbacks           []FallbackProfile `json:"fallbacks,omitempty"`
}

// FallbackProfile is an explicitly selected backup for one executor. It is
// tried only after the primary fails before returning a completion.
type FallbackProfile struct {
	Name                string `json:"name,omitempty"`
	Provider            string `json:"provider,omitempty"`
	Model               string `json:"model"`
	BaseURL             string `json:"base_url"`
	APIKey              string `json:"api_key"`
	ContextWindowTokens int    `json:"context_window_tokens,omitempty"`
	Priority            int    `json:"priority,omitempty"`
}

// Active reports whether the profile has enough to build a client.
func (p Profile) Active() bool {
	return strings.TrimSpace(p.BaseURL) != "" && strings.TrimSpace(p.APIKey) != "" && strings.TrimSpace(p.Model) != ""
}

// Label is a short "provider/model" tag for the console.
func (p Profile) Label() string {
	prov := p.Provider
	if prov == "" {
		prov = "llm"
	}
	m := p.Model
	if m == "" {
		m = "model"
	}
	return prov + "/" + m
}

// KnownContextWindowTokens is a small model catalog for common exact IDs.
// Unknown/custom IDs use the operator's per-profile value or the conservative
// solver fallback. Values should be reviewed as providers revise aliases.
func KnownContextWindowTokens(model string) int {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "deepseek-flash", "deepseek-v4-pro":
		return 1_048_576 // https://api-docs.deepseek.com/api/list-models/
	case "deepseek-chat", "deepseek-reasoner":
		return 64_000 // conservative floor for the legacy aliases
	case "gpt-4o":
		return 128_000 // https://developers.openai.com/api/docs/models/gpt-4o
	case "claude-sonnet-4-5":
		return 200_000 // https://docs.anthropic.com/en/api/claude-on-amazon-bedrock
	case "moonshot-v1-8k":
		return 8_192
	default:
		return 0
	}
}

// Store holds the profile and persists it to <stateDir>/llm.json.
type Store struct {
	mu   sync.RWMutex
	path string
	p    Profile
}

// Load reads the saved profile (if any) from stateDir.
func Load(stateDir string) *Store {
	s := &Store{path: filepath.Join(stateDir, "llm.json")}
	if data, err := os.ReadFile(s.path); err == nil {
		var p Profile
		if json.Unmarshal(data, &p) == nil {
			s.p = p
		}
	}
	return s
}

// UseFallback applies an environment-backed runtime profile without persisting
// its secret. An explicitly saved console profile always takes precedence.
func (s *Store) UseFallback(p Profile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.p.Active() {
		s.p = p
	}
}

func (s *Store) Get() Profile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.p
	p.Fallbacks = append([]FallbackProfile(nil), p.Fallbacks...)
	return p
}

func (s *Store) Active() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.p.Active()
}

// Set replaces the profile and persists it with restrictive permissions. An
// empty APIKey keeps the previously stored key (so editing other fields in the
// console does not wipe the secret).
func (s *Store) Set(p Profile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.Provider = strings.TrimSpace(p.Provider)
	p.Model = strings.TrimSpace(p.Model)
	p.BaseURL = strings.TrimSpace(p.BaseURL)
	if p.ContextWindowTokens < 4096 || p.ContextWindowTokens > 2_000_000 {
		p.ContextWindowTokens = 0
	}
	if strings.TrimSpace(p.APIKey) == "" {
		p.APIKey = s.p.APIKey // preserve existing secret
	}
	if p.Fallbacks == nil && s.p.Fallbacks != nil {
		p.Fallbacks = append([]FallbackProfile(nil), s.p.Fallbacks...)
	}
	for i := range p.Fallbacks {
		backup := &p.Fallbacks[i]
		backup.Name = strings.TrimSpace(backup.Name)
		backup.Provider = strings.TrimSpace(backup.Provider)
		backup.Model = strings.TrimSpace(backup.Model)
		backup.BaseURL = strings.TrimSpace(backup.BaseURL)
		if backup.APIKey == "" && i < len(s.p.Fallbacks) && backup.BaseURL == s.p.Fallbacks[i].BaseURL {
			backup.APIKey = s.p.Fallbacks[i].APIKey
		}
		u, err := url.Parse(backup.BaseURL)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || backup.Model == "" || backup.APIKey == "" {
			return fmt.Errorf("fallback %d needs a valid base URL, model and API key", i+1)
		}
		if backup.ContextWindowTokens != 0 && (backup.ContextWindowTokens < 4096 || backup.ContextWindowTokens > 2_000_000) {
			return fmt.Errorf("fallback %d has an invalid context window", i+1)
		}
		if backup.Priority == 0 {
			backup.Priority = i + 1
		}
	}
	if s.path == "" {
		s.p = p
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	s.p = p
	return nil
}

// Public is the console-safe view: the key itself is never returned, only
// whether one is set and a short masked hint.
func (s *Store) Public() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.p
	backups := make([]map[string]any, 0, len(p.Fallbacks))
	for _, backup := range p.Fallbacks {
		backups = append(backups, map[string]any{"name": backup.Name, "provider": backup.Provider,
			"model": backup.Model, "base_url": backup.BaseURL, "context_window_tokens": backup.ContextWindowTokens,
			"priority": backup.Priority, "has_key": backup.APIKey != ""})
	}
	hint := ""
	if n := len(p.APIKey); n > 0 {
		if n <= 4 {
			hint = strings.Repeat("•", n)
		} else {
			hint = "••••" + p.APIKey[n-4:]
		}
	}
	return map[string]any{
		"provider": p.Provider, "model": p.Model, "base_url": p.BaseURL,
		"fallbacks":             backups,
		"context_window_tokens": p.ContextWindowTokens,
		"effective_context_window_tokens": func() int {
			if p.ContextWindowTokens > 0 {
				return p.ContextWindowTokens
			}
			if n := KnownContextWindowTokens(p.Model); n > 0 {
				return n
			}
			return 16_000
		}(),
		"has_key": p.APIKey != "", "key_hint": hint, "active": p.Active(),
	}
}
