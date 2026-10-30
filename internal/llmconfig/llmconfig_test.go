package llmconfig

import (
	"reflect"
	"testing"
)

func TestContextWindowPersistsWithProfile(t *testing.T) {
	dir := t.TempDir()
	s := Load(dir)
	if err := s.Set(Profile{Provider: "custom", Model: "custom-model", BaseURL: "https://example.test/v1", APIKey: "secret", ContextWindowTokens: 128_000}); err != nil {
		t.Fatal(err)
	}
	reloaded := Load(dir)
	if got := reloaded.Get().ContextWindowTokens; got != 128_000 {
		t.Fatalf("context window = %d", got)
	}
	if got := reloaded.Public()["context_window_tokens"]; got != 128_000 {
		t.Fatalf("public window = %v", got)
	}
}

func TestFallbackDoesNotReplaceSavedProfile(t *testing.T) {
	s := Load("")
	fallback := Profile{Provider: "deepseek", Model: "deepseek-chat", BaseURL: "https://api.deepseek.com/v1", APIKey: "runtime-secret"}
	s.UseFallback(fallback)
	if !s.Active() || s.Public()["model"] != "deepseek-chat" {
		t.Fatalf("runtime fallback was not applied: %+v", s.Public())
	}
	s.UseFallback(Profile{Provider: "custom", Model: "other", BaseURL: "https://example.test/v1", APIKey: "other-secret"})
	if got := s.Get(); !reflect.DeepEqual(got, fallback) {
		t.Fatalf("fallback replaced active profile: %+v", got)
	}
}

func TestKnownContextWindows(t *testing.T) {
	for model, want := range map[string]int{"deepseek-flash": 1_048_576, "deepseek-chat": 64_000, "gpt-4o": 128_000, "claude-sonnet-4-5": 200_000, "custom": 0} {
		if got := KnownContextWindowTokens(model); got != want {
			t.Fatalf("%s: got %d want %d", model, got, want)
		}
	}
}

func TestFallbackKeyStaysPrivateAndSurvivesPrimaryEdit(t *testing.T) {
	s := Load(t.TempDir())
	primary := Profile{Provider: "custom", Model: "first", BaseURL: "https://first.example/v1", APIKey: "primary-secret",
		Fallbacks: []FallbackProfile{{Model: "second", BaseURL: "https://second.example/v1", APIKey: "backup-secret"}}}
	if err := s.Set(primary); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(Profile{Provider: "custom", Model: "first-v2", BaseURL: primary.BaseURL,
		Fallbacks: []FallbackProfile{{Model: "second", BaseURL: "https://second.example/v1"}}}); err != nil {
		t.Fatal(err)
	}
	got := s.Get()
	if got.APIKey != "primary-secret" || len(got.Fallbacks) != 1 || got.Fallbacks[0].APIKey != "backup-secret" {
		t.Fatalf("saved credentials lost: %+v", got)
	}
	public := s.Public()["fallbacks"].([]map[string]any)
	if public[0]["has_key"] != true {
		t.Fatal("backup key status missing")
	}
	if _, exposed := public[0]["api_key"]; exposed {
		t.Fatal("backup credential exposed in public profile")
	}
}
