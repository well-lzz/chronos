package swarm

import (
	"reflect"
	"testing"

	"github.com/well-lzz/chronos/internal/config"
	"github.com/well-lzz/chronos/internal/llmconfig"
)

func TestModelSelectionKeepsDistinctExplicitPeers(t *testing.T) {
	store := llmconfig.Load(t.TempDir())
	if err := store.Set(llmconfig.Profile{
		Provider: "custom", Model: "console-model", BaseURL: "http://127.0.0.1:12345/v1", APIKey: "test",
	}); err != nil {
		t.Fatal(err)
	}
	s := Swarm{
		ModelSpecs: []string{"qwen/qwen-plus", "glm/glm-4-plus"},
		Settings: config.Settings{
			QwenKey: "qwen-key", QwenBaseURL: "http://127.0.0.1:12345/v1",
			GLMKey: "glm-key", GLMBaseURL: "http://127.0.0.1:12345/v1",
		},
		LLM: store,
	}
	for spec, want := range map[string]string{"qwen/qwen-plus": "qwen-plus", "glm/glm-4-plus": "glm-4-plus"} {
		_, got, err := s.resolveModel(spec)
		if err != nil || got != want {
			t.Fatalf("resolveModel(%q) = %q, %v; want %q", spec, got, err, want)
		}
	}
	s.ModelSpecs = []string{"custom/console-model"}
	s.UseConsoleProfile = true
	_, got, err := s.resolveModel(s.ModelSpecs[0])
	if err != nil || got != "console-model" {
		t.Fatalf("single console model = %q, %v", got, err)
	}
}

func TestExplicitSingleModelBeatsSavedConsoleProfile(t *testing.T) {
	store := llmconfig.Load(t.TempDir())
	if err := store.Set(llmconfig.Profile{Provider: "custom", Model: "saved", BaseURL: "https://models.example.test/v1", APIKey: "test"}); err != nil {
		t.Fatal(err)
	}
	s := Swarm{ModelSpecs: []string{"qwen/operator-selected"}, LLM: store,
		Settings: config.Settings{QwenBaseURL: "https://models.example.test/v1", QwenKey: "test"}}
	_, model, err := s.resolveModel(s.ModelSpecs[0])
	if err != nil || model != "operator-selected" {
		t.Fatalf("explicit model = %q, %v", model, err)
	}
}

func TestDistinctSpecs(t *testing.T) {
	got := distinctSpecs([]string{" qwen/qwen-plus ", "", "qwen/qwen-plus", "glm/glm-4-plus"})
	want := []string{"qwen/qwen-plus", "glm/glm-4-plus"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("distinctSpecs = %v, want %v", got, want)
	}
}

func TestOperatorProfileOverridesPresetResolution(t *testing.T) {
	s := Swarm{
		ModelSpecs: []string{"operator-choice"},
		ModelProfiles: map[string]llmconfig.Profile{
			"operator-choice": {Provider: "operator-provider", Model: "arbitrary-model-id", BaseURL: "https://models.example.test/v1", APIKey: "test"},
		},
	}
	_, model, err := s.resolveModel("operator-choice")
	if err != nil || model != "arbitrary-model-id" {
		t.Fatalf("operator model = %q, %v", model, err)
	}
}

func TestToolStatsOutliveActivityRing(t *testing.T) {
	s := Swarm{ModelSpecs: []string{"model-a"}}
	s.init()
	for i := 0; i < activityRing+25; i++ {
		s.emit("model-a", i, "tool_call", "http_request", "")
	}
	s.emit("model-a", 0, "tool_call", "add_fact", "")
	s.emit("model-a", 0, "tool_result", "http_request", "")
	if got := len(s.activity["model-a"]); got != activityRing {
		t.Fatalf("activity length = %d, want ring cap %d", got, activityRing)
	}
	agents := s.Status()["agents"].(map[string]any)
	agent := agents["model-a"].(map[string]any)
	if got, want := agent["tool_calls"], activityRing+26; got != want {
		t.Fatalf("tool calls = %v, want %d", got, want)
	}
	tools := agent["tools"].(map[string]int)
	if tools["http_request"] != activityRing+25 || tools["add_fact"] != 1 {
		t.Fatalf("tool counts = %#v", tools)
	}
}
