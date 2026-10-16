// Package platform is the competition-platform abstraction and the TSecBench
// adapter. It is part of the "mechanical shell" around the Assessment core:
// the scheduler pulls challenges from a Platform, runs a collaborative swarm on
// each, and submits flags through it. Flags are a platform-confirmable kind of
// finding, not a separate mode.
package platform

import (
	"fmt"

	"github.com/well-lzz/chronos/internal/mission"
)

// New selects a platform adapter by mode. TSecBench is just ONE adapter — the
// agent is not bound to it. To support another evaluation platform or work
// source, implement the Platform interface and register it here; the scheduler,
// swarm and console are platform-agnostic. (Real engagements use no platform at
// all — see the engagement path.)
func New(mode, baseURL, token string) (Platform, error) {
	switch mode {
	case "", "tsec", "tsecbench", "benchmark":
		return NewTSec(baseURL, token), nil
	default:
		return nil, fmt.Errorf("unknown platform mode %q — register an adapter in platform.New", mode)
	}
}

// Challenge is the normalized view of one platform challenge.
type Challenge struct {
	Name             string
	UniqueCode       string
	Category         string
	Description      string
	Value            int
	Level            int
	FlagCount        int
	CorrectFlagCount int
	IsCompleted      bool
	ContainerStatus  string
	ContainerAddr    []string
}

// SubmitResult is the normalized answer-submission result.
type SubmitResult struct {
	Status            string // correct | already_solved | incorrect | unknown
	Message           string
	Display           string
	ChallengeComplete bool
	Progress          string
}

func (r SubmitResult) Confirmed() bool {
	return r.Status == "correct" || r.Status == "already_solved"
}

// Platform is the platform-neutral interface the scheduler/swarm use.
type Platform interface {
	FetchAllChallenges() ([]Challenge, error)
	FetchSolvedNames() (map[string]bool, error)
	FetchPollState() ([]Challenge, map[string]bool, error)
	SubmitFlag(challengeName, flag string) (SubmitResult, error)
	// PullChallenge starts the challenge's container if needed and returns a
	// mission.Meta whose ConnectionInfo carries the container address(es).
	PullChallenge(ch Challenge) (mission.Meta, error)
	ReleaseChallenge(challengeName string) bool
	CloseStaleContainers(keep map[string]bool) []string
	FetchHint(challengeName string) (string, error)
	Close()
}
