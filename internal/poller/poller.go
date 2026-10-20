// Package poller polls the competition platform on a jittered interval and
// emits events for newly-appeared and newly-solved challenges. The scheduler
// uses the solved events to kill a swarm once the platform confirms a solve —
// that is how a flag-bearing mission terminates (no racing inside the swarm).
package poller

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"sync"
	"time"

	"github.com/well-lzz/chronos/internal/platform"
)

type EventKind string

const (
	NewChallenge      EventKind = "new_challenge"
	ChallengeSolved   EventKind = "challenge_solved"
	PlatformRecovered EventKind = "platform_recovered"
)

type Event struct {
	Kind EventKind
	Name string
}

type Poller struct {
	pf                platform.Platform
	interval          time.Duration
	jitter            time.Duration
	initialRetryDelay time.Duration

	mu          sync.Mutex
	refreshMu   sync.Mutex
	known       map[string]bool
	solved      map[string]bool
	stubs       map[string]platform.Challenge
	unavailable bool // a task-not-runnable response; retry until a valid poll recovers
	events      chan Event
	stop        chan struct{}
	stopOnce    sync.Once
}

func New(pf platform.Platform, interval, jitter time.Duration) *Poller {
	return &Poller{
		pf: pf, interval: interval, jitter: jitter, initialRetryDelay: 2 * time.Second,
		known: map[string]bool{}, solved: map[string]bool{}, stubs: map[string]platform.Challenge{},
		events: make(chan Event, 256), stop: make(chan struct{}),
	}
}

// Start allows a few initial task-unavailable responses before reporting a
// startup error. Once work has started, such responses never stop the poller.
func (p *Poller) Start(ctx context.Context) error {
	const maxInitialAttempts = 3
	for attempt := 1; attempt <= maxInitialAttempts; attempt++ {
		if err := p.seed(); err != nil {
			if !platform.IsTaskNotRunnable(err) || attempt == maxInitialAttempts {
				return err
			}
			p.markTaskUnavailable(err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(p.initialRetryDelay):
			}
			continue
		}
		log.Printf("Poller initialized: %d challenges, %d solved", len(p.known), len(p.solved))
		go p.loop(ctx)
		return nil
	}
	return fmt.Errorf("initial platform poll failed")
}

func (p *Poller) seed() error {
	chs, solved, err := p.pf.FetchPollState()
	if err != nil {
		return fmt.Errorf("initial platform poll: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unavailable = false
	for _, c := range chs {
		p.known[c.Name] = true
		p.stubs[c.Name] = c
	}
	p.solved = solved
	return nil
}

func (p *Poller) Stop() { p.stopOnce.Do(func() { close(p.stop) }) }

// TaskUnavailable reports a task-not-runnable response from the last accepted
// platform state. It is advisory: one error cannot prove the task is permanently
// gone, and must never terminate active work. Successful polling clears it.
func (p *Poller) TaskUnavailable() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.unavailable
}

// Refresh replaces cached challenge addresses after the scheduler closes stale
// containers, so the next pull cannot reuse an address that was just closed.
func (p *Poller) Refresh() { p.pollOnce() }

// Events returns the event channel (new/solved).
func (p *Poller) Events() <-chan Event { return p.events }

func (p *Poller) KnownChallenges() map[string]bool { return copySet(&p.mu, p.known) }
func (p *Poller) KnownSolved() map[string]bool     { return copySet(&p.mu, p.solved) }

func (p *Poller) KnownStubs() map[string]platform.Challenge {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make(map[string]platform.Challenge, len(p.stubs))
	for k, v := range p.stubs {
		cp[k] = v
	}
	return cp
}

func copySet(mu *sync.Mutex, m map[string]bool) map[string]bool {
	mu.Lock()
	defer mu.Unlock()
	cp := make(map[string]bool, len(m))
	for k, v := range m {
		if v {
			cp[k] = true
		}
	}
	return cp
}

func (p *Poller) markTaskUnavailable(err error) {
	p.mu.Lock()
	already := p.unavailable
	p.unavailable = true
	p.mu.Unlock()
	if !already {
		log.Printf("Platform reports task unavailable (%v); pausing new pulls and retrying the poll.", err)
	}
}

func (p *Poller) pollOnce() {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	chs, solved, err := p.pf.FetchPollState()
	if err != nil {
		if platform.IsTaskNotRunnable(err) {
			p.markTaskUnavailable(err)
			return
		}
		log.Printf("Poll error: %v", err)
		return
	}
	current := map[string]bool{}
	for _, c := range chs {
		current[c.Name] = true
	}
	p.mu.Lock()
	// Sanity: a suspiciously small list is likely a transient API error.
	if len(p.known) > 0 && len(current)*2 < len(p.known) {
		p.mu.Unlock()
		log.Printf("Poll returned suspicious data (%d vs %d known) — skipping", len(current), len(p.known))
		return
	}
	if len(p.solved) > 0 && len(solved) == 0 {
		p.mu.Unlock()
		log.Printf("Poll returned 0 solved (had %d) — skipping", len(p.solved))
		return
	}
	var newCh, newSolved []string
	for name := range current {
		if !p.known[name] {
			newCh = append(newCh, name)
		}
	}
	for name := range solved {
		if !p.solved[name] {
			newSolved = append(newSolved, name)
		}
	}
	recovered := p.unavailable
	p.unavailable = false
	p.known = current
	p.stubs = map[string]platform.Challenge{}
	for _, c := range chs {
		p.stubs[c.Name] = c
	}
	p.solved = solved
	p.mu.Unlock()

	if recovered {
		log.Printf("Platform polling recovered; resuming challenge scheduling.")
		p.emit(Event{Kind: PlatformRecovered})
	}
	for _, n := range newCh {
		log.Printf("New challenge detected: %s", n)
		p.emit(Event{NewChallenge, n})
	}
	for _, n := range newSolved {
		log.Printf("Challenge solved: %s", n)
		p.emit(Event{ChallengeSolved, n})
	}
}

func (p *Poller) emit(e Event) {
	select {
	case p.events <- e:
	default: // queue full — drop rather than block the poll loop
	}
}

func (p *Poller) nextDelay() time.Duration {
	j := p.jitter
	if j <= 0 {
		return p.interval
	}
	lo := p.interval - j
	if lo < 0 {
		lo = 0
	}
	span := int64(2*j + 1)
	offset, err := rand.Int(rand.Reader, big.NewInt(span))
	if err != nil {
		return p.interval
	}
	return lo + time.Duration(offset.Int64())
}

func (p *Poller) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stop:
			return
		case <-time.After(p.nextDelay()):
			p.pollOnce()
		}
	}
}
