// Package taskruntime owns durable run-wide tool-step allocations.
package taskruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/well-lzz/chronos/internal/runevent"
)

type change struct {
	Owner string `json:"owner"`
	Steps int    `json:"steps"`
}
type Budget struct {
	mu           sync.Mutex
	journal      *runevent.Journal
	limit, spent int
	reserved     map[string]int
	sequence     uint64
}

var ErrExhausted = errors.New("tool-step budget exhausted")

func OpenBudget(path, runID string, limit int) (*Budget, error) {
	j, err := runevent.Open(path, runID)
	if err != nil {
		return nil, err
	}
	b := &Budget{journal: j, limit: limit, reserved: map[string]int{}}
	for _, e := range j.Events() {
		var c change
		if err := json.Unmarshal(e.Payload, &c); err != nil {
			return nil, err
		}
		if err := b.apply(e.Kind, c); err != nil {
			return nil, err
		}
		b.sequence = e.Seq
	}
	return b, nil
}

func (b *Budget) apply(kind string, c change) error {
	if (kind == "budget.reserve" || kind == "budget.spend") && c.Steps < 1 {
		return fmt.Errorf("invalid budget step count")
	}
	if (kind == "budget.reserve" || kind == "budget.release") && c.Owner == "" {
		return fmt.Errorf("missing budget owner")
	}
	switch kind {
	case "budget.reserve":
		b.reserved[c.Owner] += c.Steps
	case "budget.spend":
		if c.Owner != "" {
			if b.reserved[c.Owner] < c.Steps {
				return fmt.Errorf("invalid budget reservation")
			}
			b.reserved[c.Owner] -= c.Steps
		}
		b.spent += c.Steps
	case "budget.release":
		delete(b.reserved, c.Owner)
	default:
		return fmt.Errorf("unsupported budget event %s", kind)
	}
	return nil
}

func (b *Budget) change(kind string, c change) error {
	if _, err := b.journal.Append(fmt.Sprintf("budget-%d", b.sequence+1), kind, c); err != nil {
		return err
	}
	b.sequence++
	return b.apply(kind, c)
}

func (b *Budget) available(n int) bool {
	total := b.spent + n
	for _, v := range b.reserved {
		total += v
	}
	return b.limit <= 0 || total <= b.limit
}

func (b *Budget) Reserve(owner string, n int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if owner == "" || n < 1 || !b.available(n) {
		return ErrExhausted
	}
	return b.change("budget.reserve", change{Owner: owner, Steps: n})
}

func (b *Budget) Spend(owner string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if owner != "" && b.reserved[owner] < 1 {
		return ErrExhausted
	}
	if owner == "" && !b.available(1) {
		return ErrExhausted
	}
	return b.change("budget.spend", change{Owner: owner, Steps: 1})
}

func (b *Budget) Exhausted(owner string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if owner != "" {
		return b.reserved[owner] < 1
	}
	return !b.available(1)
}

func (b *Budget) Release(owner string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.reserved[owner]; !ok {
		return nil
	}
	return b.change("budget.release", change{Owner: owner})
}

func (b *Budget) Remaining(owner string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reserved[owner]
}
func (b *Budget) Snapshot() (spent, reserved int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, n := range b.reserved {
		reserved += n
	}
	return b.spent, reserved
}

// ReturnOrphans is called once at run startup, before any executor is launched.
// No worker from the previous process survives that boundary. Charged actions
// remain spent; only unused reservations are returned for explicit new work.
func (b *Budget) ReturnOrphans() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	var owners []string
	for owner := range b.reserved {
		owners = append(owners, owner)
	}
	sort.Strings(owners)
	for _, owner := range owners {
		if err := b.change("budget.release", change{Owner: owner}); err != nil {
			return err
		}
	}
	return nil
}
