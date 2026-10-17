// Package llmconfig is the operator-editable LLM profile: which model backs the
// agent, persisted so it survives restarts and is configured from the console
// instead of environment variables. Any OpenAI-compatible endpoint works (a
// base URL + key + model), so mainstream providers are all reachable through the
// one generic path.
package llmconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Profile is the active model configuration.
type Profile struct {
	Provider string `json:"provider"` // display label / preset id (e.g. "deepseek", "openai", "custom")
	Model    string `json:"model"`    // concrete model id, e.g. "deepseek-chat"
	BaseURL  string `json:"base_url"` // OpenAI-compatible base, e.g. "https://api.deepseek.com/v1"
	APIKey   string `json:"api_key"`  // provider key (stored locally, 0600)
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

func (s *Store) Get() Profile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.p
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
	if strings.TrimSpace(p.APIKey) == "" {
		p.APIKey = s.p.APIKey // preserve existing secret
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
		"has_key": p.APIKey != "", "key_hint": hint, "active": p.Active(),
	}
}
