package blackboard

import (
	"strings"
	"testing"
	"time"
)

func TestSurfaceLeaseConflictReleaseAndSummary(t *testing.T) {
	stateDir := t.TempDir()
	b, err := LoadOrNew("lease-test", stateDir)
	if err != nil {
		t.Fatal(err)
	}
	goal, err := b.AddSubgoal("inspect service", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	before := b.Revision()
	if _, err := b.ClaimSurface("target.test:80/api", "model-a", goal, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ClaimSurface("target.test:80", "model-b", "", time.Minute); err == nil {
		t.Fatal("overlapping service claim was accepted")
	}
	if _, err := b.ClaimSurface("target.test:80/other", "model-b", "", time.Minute); err != nil {
		t.Fatalf("independent endpoint family was blocked: %v", err)
	}
	if !strings.Contains(b.Summary(), "target.test:80/api owned by model-a") {
		t.Fatal("active claim was absent from reinjected summary")
	}
	if delta, _ := b.DeltaSince(before, 1200); !strings.Contains(delta, "claimed by model-a") {
		t.Fatalf("claim missing from delta: %s", delta)
	}
	if err := b.HoldSurface("target.test:80/api/foo", "model-a"); err != nil {
		t.Fatal(err)
	}
	if err := b.HoldSurface("target.test:80/api/foo", "model-b"); err == nil {
		t.Fatal("peer used another owner's claim")
	}
	if _, err := b.ResolveSubgoal(goal, "done"); err != nil {
		t.Fatal(err)
	}
	if err := b.HoldSurface("target.test:80/api/foo", "model-a"); err == nil {
		t.Fatal("resolving sub-goal did not release its lease")
	}
	if _, err := b.ClaimSurface("target.test:80", "model-a", "", time.Minute); err == nil {
		t.Fatal("peer claim disappeared after unrelated release")
	}
	if err := b.ReleaseOwner("model-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ClaimSurface("target.test:80", "model-a", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadOrNew("lease-test", stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.ActiveLeases()) != 1 {
		t.Fatal("active lease was not persisted")
	}
}

func TestExpiredLeaseReleasesAndAdvancesRevision(t *testing.T) {
	b := newBoard("expiry", "")
	_, _ = b.ClaimSurface("target.test:80", "model-a", "", time.Minute)
	before := b.Revision()
	b.mu.Lock()
	lease := b.leases["target.test:80"]
	lease.ExpiresAt = now() - 1
	b.leases[lease.Surface] = lease
	b.mu.Unlock()
	if b.Revision() <= before || len(b.ActiveLeases()) != 0 {
		t.Fatal("expiry did not change board state")
	}
	if _, err := b.ClaimSurface("target.test:80", "model-b", "", time.Minute); err != nil {
		t.Fatal(err)
	}
}
