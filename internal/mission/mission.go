// Package mission holds the neutral mission descriptor the solver/swarm,
// prompt builder and report consume. It is the Go counterpart of the Python
// ChallengeMeta, trimmed to what the assessment core needs.
package mission

// Meta describes one mission: a name, a goal/description, and the authorized
// scope (ConnectionInfo carries hosts/CIDRs). FlagCount>=1 means the target may
// carry a flag (platform range); 0 means a diffuse-goal engagement.
type Meta struct {
	Name           string
	Category       string
	Description    string
	ConnectionInfo string
	FlagCount      int
}

func (m Meta) HasFlags() bool { return m.FlagCount >= 1 }
