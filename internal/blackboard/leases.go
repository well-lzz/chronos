package blackboard

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Lease reserves one target surface for a single executor's state-changing work.
// A host:port key covers all endpoint families below it; mission:* covers all.
type Lease struct {
	Surface   string  `json:"surface"`
	Owner     string  `json:"owner"`
	SubgoalID string  `json:"subgoal_id,omitempty"`
	ExpiresAt float64 `json:"expires_at"`
	Revision  uint64  `json:"revision"`
}

type LeaseEvent struct {
	Surface  string `json:"surface"`
	Owner    string `json:"owner"`
	Action   string `json:"action"` // claimed | released | expired
	Revision uint64 `json:"revision"`
}

func surfacesOverlap(a, b string) bool {
	return a == "mission:*" || b == "mission:*" || a == b ||
		strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func (b *Blackboard) leaseEventLocked(lease Lease, action string) {
	b.leaseLog = append(b.leaseLog, LeaseEvent{Surface: lease.Surface, Owner: lease.Owner, Action: action, Revision: b.nextRevision()})
	if len(b.leaseLog) > 200 {
		b.leaseLog = append([]LeaseEvent(nil), b.leaseLog[len(b.leaseLog)-200:]...)
	}
}

func (b *Blackboard) expireLeasesLocked() bool {
	changed := false
	for key, lease := range b.leases {
		if lease.ExpiresAt <= now() {
			delete(b.leases, key)
			b.leaseEventLocked(lease, "expired")
			changed = true
		}
	}
	return changed
}

// ClaimSurface acquires or renews a bounded lease. Conflicting surfaces are
// rejected atomically; callers cannot silently steal a peer's work.
func (b *Blackboard) ClaimSurface(surface, owner, subgoalID string, ttl time.Duration) (Lease, error) {
	if surface == "" || owner == "" || len(surface) > 512 || ttl < 30*time.Second || ttl > 5*time.Minute {
		return Lease{}, fmt.Errorf("invalid surface, owner, or TTL (30–300s)")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	changed := b.expireLeasesLocked()
	if subgoalID != "" {
		goal := b.subgoals[subgoalID]
		if goal == nil || goal.Status != "open" {
			return Lease{}, fmt.Errorf("sub-goal %s is not open", subgoalID)
		}
	}
	for _, active := range b.leases {
		if active.Owner != owner && surfacesOverlap(surface, active.Surface) {
			if changed {
				_ = b.persist()
			}
			return Lease{}, fmt.Errorf("surface is claimed by %s until %s", active.Owner, time.Unix(int64(active.ExpiresAt), 0).UTC().Format(time.RFC3339))
		}
	}
	lease, exists := b.leases[surface]
	if exists && lease.Owner != owner {
		return Lease{}, fmt.Errorf("surface is claimed by %s", lease.Owner)
	}
	if !exists {
		lease = Lease{Surface: surface, Owner: owner, SubgoalID: subgoalID, Revision: b.nextRevision()}
		b.leaseLog = append(b.leaseLog, LeaseEvent{Surface: surface, Owner: owner, Action: "claimed", Revision: lease.Revision})
		if len(b.leaseLog) > 200 {
			b.leaseLog = append([]LeaseEvent(nil), b.leaseLog[len(b.leaseLog)-200:]...)
		}
	} else if subgoalID != "" {
		lease.SubgoalID = subgoalID
	}
	lease.ExpiresAt = float64(time.Now().Add(ttl).UnixNano()) / 1e9
	b.leases[surface] = lease
	return lease, b.persist()
}

// HoldSurface confirms that owner covers the target and renews the lease for
// the maximum duration of a tool call, avoiding expiry during that action.
func (b *Blackboard) HoldSurface(surface, owner string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	changed := b.expireLeasesLocked()
	for key, lease := range b.leases {
		if lease.Owner == owner && (key == "mission:*" || key == surface || strings.HasPrefix(surface, key+"/")) {
			lease.ExpiresAt = float64(time.Now().Add(2*time.Minute).UnixNano()) / 1e9
			b.leases[key] = lease
			return b.persist()
		}
	}
	if changed {
		_ = b.persist()
	}
	return fmt.Errorf("claim_surface required for %s before a state-changing action", surface)
}

func (b *Blackboard) ReleaseSurface(surface, owner string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	lease, ok := b.leases[surface]
	if !ok || lease.Owner != owner {
		return false, nil
	}
	delete(b.leases, surface)
	b.leaseEventLocked(lease, "released")
	return true, b.persist()
}

func (b *Blackboard) ReleaseOwner(owner string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	changed := false
	for key, lease := range b.leases {
		if lease.Owner == owner {
			delete(b.leases, key)
			b.leaseEventLocked(lease, "released")
			changed = true
		}
	}
	if changed {
		return b.persist()
	}
	return nil
}

func (b *Blackboard) releaseSubgoalLocked(id string) {
	for key, lease := range b.leases {
		if lease.SubgoalID == id {
			delete(b.leases, key)
			b.leaseEventLocked(lease, "released")
		}
	}
}

func (b *Blackboard) ActiveLeases() []Lease {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.expireLeasesLocked() {
		_ = b.persist()
	}
	out := make([]Lease, 0, len(b.leases))
	for _, lease := range b.leases {
		out = append(out, lease)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Surface < out[j].Surface })
	return out
}

func (b *Blackboard) LeaseEventsSince(revision uint64) []LeaseEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]LeaseEvent, 0)
	for _, event := range b.leaseLog {
		if event.Revision > revision {
			out = append(out, event)
		}
	}
	return out
}
