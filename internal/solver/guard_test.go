package solver

import (
	"context"
	"strings"
	"testing"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/guard"
	"github.com/well-lzz/chronos/internal/scope"
)

func TestOperatorAllowCannotOverrideScopeLock(t *testing.T) {
	dir := t.TempDir()
	manager := guard.Shared(dir, "")
	if err := manager.SetRules([]guard.Rule{{Tool: "*", Action: "allow", Priority: 100}}); err != nil {
		t.Fatal(err)
	}
	board, err := blackboard.LoadOrNew("guarded", dir)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{ModelSpec: "executor", BB: board, Scope: scope.Parse([]string{"allowed.example"}), Guard: manager})
	result := s.dispatch(context.Background(), "http_request", `{"url":"https://outside.example/"}`)
	if !strings.Contains(result, "平台管控·非目标防御") || !strings.Contains(result, "scope lock") {
		t.Fatalf("scope denial lost platform attribution: %q", result)
	}
	rows := manager.Recent(10)
	if len(rows) != 1 || rows[0].Action != "deny" {
		t.Fatalf("scope denial was not audited: %+v", rows)
	}
}
