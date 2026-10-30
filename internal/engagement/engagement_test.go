package engagement

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "e.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValidAndMeta(t *testing.T) {
	p := write(t, `{"name":"acme","mode":"assessment","goal":"ext test",
		"scope":{"targets":["staging.acme.test","10.10.5.0/24"],"rules_of_engagement":"no DoS"}}`)
	e, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.ScopeTargets) != 2 || e.RulesOfEngagement != "no DoS" {
		t.Fatalf("parsed wrong: %+v", e)
	}
	m := e.ToMeta()
	if m.ConnectionInfo == "" || m.FlagCount != 0 {
		t.Fatal("meta wrong")
	}
	if e.Scope().IsEmpty() {
		t.Fatal("scope should not be empty")
	}
}

func TestEmptyScopeRefused(t *testing.T) {
	for _, body := range []string{
		`{"name":"x","mode":"assessment","goal":"g"}`,
		`{"name":"x","mode":"assessment","scope":{"targets":[]}}`,
		`{"name":"x","mode":"assessment","scope":["   "]}`,
	} {
		if _, err := Load(write(t, body)); err == nil {
			t.Fatalf("expected refusal for %s", body)
		}
	}
}

func TestWebDraftCanStartWithoutInitialScope(t *testing.T) {
	path := write(t, `{"name":"operator-selected","mode":"assessment","goal":"review"}`)
	if _, err := Load(path); err == nil {
		t.Fatal("headless run accepted empty scope")
	}
	draft, err := LoadDraft(path)
	if err != nil || !draft.Scope().IsEmpty() {
		t.Fatalf("paused web draft = %+v, %v", draft, err)
	}
}

func TestScopeAsList(t *testing.T) {
	e, err := Load(write(t, `{"name":"y","mode":"assessment","scope":["h.test","10.0.0.0/24"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !e.Scope().Allows("10.0.0.5") || !e.Scope().Allows("h.test") {
		t.Fatal("list scope wrong")
	}
}
