// Package mission holds the neutral mission descriptor the solver/swarm,
// prompt builder and report consume. It is the Go counterpart of the Python
// ChallengeMeta, trimmed to what the assessment core needs.
package mission

import "strings"

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

// IsKillchain is the same broad mission classification used for the longer
// scheduler time limit. Only these missions may opt into depth-two delegation.
func (m Meta) IsKillchain() bool {
	blob := strings.ToLower(m.Category + " " + m.Description + " " + m.Name)
	for _, word := range []string{"killchain", "kill chain", "渗透", "横向", "内网", "多阶段", "跳板"} {
		if strings.Contains(blob, word) {
			return true
		}
	}
	return false
}
