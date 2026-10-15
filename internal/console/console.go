// Package console defines the contract between the web server and whatever is
// running the mission(s) — the scheduler (many missions) or a single engagement.
// Both present the same findings-centric view: confirmed / suspected / failed,
// not CTF "solved/unsolved/deferred".
package console

import (
	"sort"

	"github.com/well-lzz/chronos/internal/swarm"
)

// Source is what the web console reads and commands.
type Source interface {
	Snapshot() map[string]any
	Activity(challenge, model string, after int) []swarm.ActEvent
	Command(action, challenge, model, message string)
}

var verdictRank = map[string]int{"confirmed": 0, "suspected": 1, "failed": 2}
var sevRank = map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3, "info": 4}

func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

// SortFindings orders rows by verdict (confirmed first) then severity.
func SortFindings(rows []map[string]any) {
	sort.SliceStable(rows, func(i, j int) bool {
		vi, vj := verdictRank[str(rows[i], "verdict")], verdictRank[str(rows[j], "verdict")]
		if vi != vj {
			return vi < vj
		}
		return sevRank[str(rows[i], "severity")] < sevRank[str(rows[j], "severity")]
	})
}
