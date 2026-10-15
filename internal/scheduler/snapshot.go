package scheduler

import "github.com/well-lzz/chronos/internal/swarm"

// Snapshot is a JSON-serializable view of the whole run for the web console.
func (s *Scheduler) Snapshot() map[string]any {
	solved := s.poll.KnownSolved()
	known := s.poll.KnownChallenges()
	stubs := s.poll.KnownStubs()

	s.mu.Lock()
	challenges := make([]map[string]any, 0, len(known))
	for name := range known {
		st := "unsolved"
		switch {
		case solved[name]:
			st = "solved"
		case s.runs[name] != nil:
			st = "active"
		case s.pending[name]:
			st = "preparing"
		case s.deferred[name] != "":
			st = "deferred"
		}
		c := stubs[name]
		challenges = append(challenges, map[string]any{
			"name": name, "value": c.Value, "level": c.Level, "category": c.Category,
			"flag_count": c.FlagCount, "correct_flag_count": c.CorrectFlagCount, "status": st,
		})
	}
	swarms := map[string]any{}
	var totalTokens int
	for name, r := range s.runs {
		swarms[name] = r.sw.Status()
		totalTokens += r.sw.TotalTokens()
	}
	active := len(s.runs)
	preparing := len(s.pending)
	deferredN := len(s.deferred)
	deferredCopy := map[string]string{}
	for k, v := range s.deferred {
		deferredCopy[k] = v
	}
	s.mu.Unlock()

	budget := -1
	if rg := s.remainingGlobal(); rg >= 0 {
		budget = int(rg) / 60
	}
	return map[string]any{
		"summary": map[string]any{
			"solved": len(solved), "unsolved": len(known) - len(solved), "total": len(known),
			"active": active, "preparing": preparing, "deferred": deferredN,
			"total_tokens": totalTokens, "budget_left_min": budget,
		},
		"challenges": challenges,
		"swarms":     swarms,
		"deferred":   deferredCopy,
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
