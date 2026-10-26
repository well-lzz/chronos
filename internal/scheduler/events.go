package scheduler

import "time"

type OrchestrationEvent struct {
	Seq     uint64  `json:"seq"`
	Time    float64 `json:"time"`
	Kind    string  `json:"kind"`
	Mission string  `json:"mission,omitempty"`
	Reason  string  `json:"reason,omitempty"`
	Detail  string  `json:"detail,omitempty"`
}

func (s *Scheduler) recordDecision(kind, mission, reason, detail string) {
	s.mu.Lock()
	s.eventSeq++
	s.events = append(s.events, OrchestrationEvent{Seq: s.eventSeq, Time: float64(time.Now().UnixNano()) / 1e9,
		Kind: kind, Mission: mission, Reason: reason, Detail: detail})
	if len(s.events) > 200 {
		s.events = append([]OrchestrationEvent(nil), s.events[len(s.events)-200:]...)
	}
	s.mu.Unlock()
}
