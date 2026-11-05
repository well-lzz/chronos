package taskruntime

import (
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentBudgetSurvivesRestartAndReturnsUnusedCredits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	b, err := OpenBudget(path, "run", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Reserve("child", 6); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = b.Spend("") }()
	}
	wg.Wait()
	spent, reserved := b.Snapshot()
	if spent != 4 || reserved != 6 {
		t.Fatalf("overspent: %d %d", spent, reserved)
	}
	b, err = OpenBudget(path, "run", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Spend("child"); err != nil {
		t.Fatal(err)
	}
	if err := b.Release("child"); err != nil {
		t.Fatal(err)
	}
	if b.Remaining("child") != 0 {
		t.Fatal("reservation not returned")
	}
	if err := b.Spend(""); err != nil {
		t.Fatal(err)
	}
	spent, reserved = b.Snapshot()
	if spent != 6 || reserved != 0 {
		t.Fatalf("bad recovered accounting: %d %d", spent, reserved)
	}
}

func TestStartupReturnsOnlyUnusedReservations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.jsonl")
	b, err := OpenBudget(path, "run", 5)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Reserve("child", 4); err != nil {
		t.Fatal(err)
	}
	if err := b.Spend("child"); err != nil {
		t.Fatal(err)
	}
	b, err = OpenBudget(path, "run", 5)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReturnOrphans(); err != nil {
		t.Fatal(err)
	}
	spent, reserved := b.Snapshot()
	if spent != 1 || reserved != 0 {
		t.Fatalf("bad orphan reconciliation: %d/%d", spent, reserved)
	}
}
