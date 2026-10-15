package scheduler

import (
	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/console"
	"github.com/well-lzz/chronos/internal/swarm"
)

// Snapshot is a findings-centric view of the run for the console. A real
// assessment is measured by findings (confirmed / suspected / failed), not by
// "solved/unsolved/deferred" challenges — so the console presents verdicts and
// the live executors, aggregated across every active mission.
func (s *Scheduler) Snapshot() map[string]any {
	s.mu.Lock()
	runs := make([]*run, 0, len(s.runs))
	for _, r := range s.runs {
		runs = append(runs, r)
	}
	active := len(s.runs)
	preparing := len(s.pending)
	s.mu.Unlock()

	confirmed, suspected, failed, facts, tokens := 0, 0, 0, 0, 0
	goalsOpen, goalsTotal := 0, 0
	var findings []map[string]any
	var subgoals []map[string]any
	var trafficRows []map[string]any
	swarms := map[string]any{}
	for _, r := range runs {
		if r.sw == nil {
			continue
		}
		swarms[r.meta.Name] = r.sw.Status()
		tokens += r.sw.TotalTokens()
		if bb := r.sw.BB; bb != nil {
			vc := bb.VerdictCounts()
			confirmed += vc["confirmed"]
			suspected += vc["suspected"]
			failed += vc["failed"]
			cc := bb.Counts()
			facts += cc["facts"]
			goalsOpen += cc["subgoals_open"]
			goalsTotal += cc["subgoals_total"]
			for _, f := range bb.FindingsPublic() {
				findings = append(findings, findingRow(r.meta.Name, f))
			}
			for _, g := range bb.SubgoalsPublic() {
				subgoals = append(subgoals, map[string]any{
					"id": r.meta.Name + "/" + g.ID, "description": g.Description,
					"status": g.Status, "outcome": g.Outcome, "done": g.Done, "mission": r.meta.Name,
				})
			}
		}
		if r.sw.TL != nil {
			for _, row := range r.sw.TL.RecentRows(40) {
				row["mission"] = r.meta.Name
				trafficRows = append(trafficRows, row)
			}
		}
	}
	console.SortFindings(findings)
	trafficRows = console.TailTraffic(trafficRows, 60)

	budget := -1
	if rg := s.remainingGlobal(); rg >= 0 {
		budget = int(rg) / 60
	}
	return map[string]any{
		"mode": "scheduler",
		"summary": map[string]any{
			"confirmed": confirmed, "suspected": suspected, "failed": failed,
			"findings": confirmed + suspected + failed, "facts": facts,
			"active": active, "preparing": preparing, "tokens": tokens, "budget_left_min": budget,
			"goals_open": goalsOpen, "goals_total": goalsTotal, "goals_done": goalsTotal - goalsOpen,
		},
		"findings": findings,
		"subgoals": subgoals,
		"traffic":  trafficRows,
		"swarms":   swarms,
	}
}

func findingRow(mission string, f blackboard.FindingRow) map[string]any {
	target := f.Target
	if target == "" {
		target = mission
	}
	return map[string]any{
		"id": mission + "/" + f.ID, "title": f.Title, "severity": f.Severity,
		"verdict": f.Verdict, "target": target, "category": f.Category,
		"evidence": f.Evidence, "mission": mission,
	}
}

// Activity returns an executor's recent steps with Step > after.
func (s *Scheduler) Activity(challenge, model string, after int) []swarm.ActEvent {
	s.mu.Lock()
	r := s.runs[challenge]
	s.mu.Unlock()
	if r == nil {
		return nil
	}
	return r.sw.Activity(model, after)
}

// Command adapts the console's action tuple to the operator queue.
func (s *Scheduler) Command(action, challenge, model, message string) {
	s.Enqueue(Command{Action: action, Challenge: challenge, Model: model, Message: message})
}
