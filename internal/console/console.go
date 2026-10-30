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

// CheckedCommander reports immediate rejections of an operator command.
type CheckedCommander interface {
	CommandChecked(action, challenge, model, message string) error
}

// ChatSource accepts a message only while its selected executor can process it.
// The web handler uses this acknowledgement instead of claiming every queued
// command was delivered to a live session.
type ChatSource interface {
	Chat(challenge, model, message string) error
}

// ScopeConfig is the operator-visible authorization boundary for one mission.
type ScopeConfig struct {
	Supported   bool     `json:"supported"`
	Editable    bool     `json:"editable"`
	Targets     []string `json:"targets"`
	Description string   `json:"description"`
}

// ScopeController controls a single engagement's boundary.
type ScopeController interface {
	ScopeConfig() ScopeConfig
	UpdateScope(targets []string) error
}

// MissionScopeController controls one selected mission within a scheduler.
type MissionScopeController interface {
	ScopeConfigFor(challenge string) ScopeConfig
	UpdateScopeFor(challenge string, targets []string) error
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

// TailTraffic sorts captured exchanges newest-last by timestamp and keeps the
// most recent n for the console's live traffic table.
func TailTraffic(rows []map[string]any, n int) []map[string]any {
	sort.SliceStable(rows, func(i, j int) bool {
		return ts(rows[i]) < ts(rows[j])
	})
	if n > 0 && len(rows) > n {
		rows = rows[len(rows)-n:]
	}
	return rows
}

func ts(m map[string]any) float64 {
	if v, ok := m["ts"].(float64); ok {
		return v
	}
	return 0
}
