package llmconfig

import "testing"

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

func TestKnownContextWindows(t *testing.T) {
	for model, want := range map[string]int{"deepseek-flash": 1_048_576, "deepseek-chat": 64_000, "gpt-4o": 128_000, "claude-sonnet-4-5": 200_000, "custom": 0} {
		if got := KnownContextWindowTokens(model); got != want {
			t.Fatalf("%s: got %d want %d", model, got, want)
		}
	}
}
