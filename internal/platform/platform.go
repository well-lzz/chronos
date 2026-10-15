// Package platform is the competition-platform abstraction and the TSecBench
// adapter. It is part of the "mechanical shell" around the Assessment core:
// the scheduler pulls challenges from a Platform, runs a collaborative swarm on
// each, and submits flags through it. Flags are a platform-confirmable kind of
// finding, not a separate mode.
package platform

import "github.com/well-lzz/chronos/internal/mission"

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
