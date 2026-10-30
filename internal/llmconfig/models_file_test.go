package llmconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadModelsFileAllowsOperatorDefinedProviders(t *testing.T) {
	t.Setenv("CHRONOS_TEST_MODEL_KEY", "from-env")
	path := filepath.Join(t.TempDir(), "models.json")
	data := `{"models":[
		{"name":"research","provider":"my-provider","model":"reasoner-v9","base_url":"https://models.example.test/v1","api_key_env":"CHRONOS_TEST_MODEL_KEY","context_window_tokens":96000,
		 "fallbacks":[{"name":"backup","model":"reasoner-v8","base_url":"https://backup.example.test/v1","api_key":"backup-key","priority":2}]},
		{"name":"review","provider":"another-provider","model":"review-v2","base_url":"https://another.example.test/v1","api_key":"direct-key"}
	]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	names, profiles, err := LoadModelsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"research", "review"}) {
		t.Fatalf("names = %v", names)
	}
	if profiles["research"].APIKey != "from-env" || profiles["research"].Model != "reasoner-v9" || profiles["research"].ContextWindowTokens != 96000 {
		t.Fatalf("custom model not loaded: %+v", profiles["research"])
	}
	if profiles["review"].APIKey != "direct-key" {
		t.Fatal("second operator model not loaded")
	}
	if backups := profiles["research"].Fallbacks; len(backups) != 1 || backups[0].Model != "reasoner-v8" || backups[0].Priority != 2 {
		t.Fatalf("fallback chain not loaded: %+v", backups)
	}
}

func TestLoadModelsFileRejectsDuplicateIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	data := `{"models":[
		{"name":"same","model":"a","base_url":"https://a.example/v1","api_key":"a"},
		{"name":"same","model":"b","base_url":"https://b.example/v1","api_key":"b"}
	]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := LoadModelsFile(path)
	if err == nil || !strings.Contains(err.Error(), "duplicate model name") {
		t.Fatalf("duplicate name error = %v", err)
	}
}
