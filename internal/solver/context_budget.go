package solver

import (
	"math"

	"github.com/well-lzz/chronos/internal/blackboard"
)

const defaultContextWindowTokens = 16_000

// ContextEvent is emitted for every reinjection and actual compaction. It is
// also journaled so context loss can be diagnosed after a process restart.
type ContextEvent struct {
	Phase           string                  `json:"phase"`
	Reason          string                  `json:"reason,omitempty"`
	WindowTokens    int                     `json:"window_tokens"`
	PromptTokens    int                     `json:"prompt_tokens,omitempty"`
	BudgetChars     int                     `json:"budget_chars"`
	BeforeChars     int                     `json:"before_chars,omitempty"`
	AfterChars      int                     `json:"after_chars,omitempty"`
	DroppedCycles   int                     `json:"dropped_cycles,omitempty"`
	ClippedMessages int                     `json:"clipped_messages,omitempty"`
	Dropped         []string                `json:"dropped,omitempty"`
	Clipped         []string                `json:"clipped,omitempty"`
	Summary         blackboard.SummaryStats `json:"summary"`
}

func (s *Solver) windowTokens() int {
	if s.contextWindow != nil {
		if n := s.contextWindow(s.Model); n >= 4096 {
			return n
		}
	}
	return defaultContextWindowTokens
}

// Message history and blackboard reinjection have independent allocations.
// The unallocated 33% leaves room for tool schemas and completion output.
func (s *Solver) transcriptBudgetChars() int {
	charsPerToken := s.charsPerToken()
	return int(math.Max(4096, float64(s.windowTokens())*0.55*charsPerToken))
}

func (s *Solver) summaryBudgetChars() int {
	return int(math.Max(1024, float64(s.windowTokens())*0.12*s.charsPerToken()))
}

func (s *Solver) deltaBudgetChars() int {
	return int(math.Max(512, float64(s.windowTokens())*0.04*s.charsPerToken()))
}

func (s *Solver) charsPerToken() float64 {
	if s.lastPromptTokens > 0 && s.lastPromptChars > 0 {
		v := float64(s.lastPromptChars) / float64(s.lastPromptTokens)
		if v < 1 {
			return 1
		}
		if v > 6 {
			return 6
		}
		return v
	}
	return 3 // conservative multilingual fallback until provider usage arrives
}

func (s *Solver) estimatedPromptTokens(chars int) int {
	return int(math.Ceil(float64(chars) / s.charsPerToken()))
}

func (s *Solver) contextEvent(event ContextEvent) {
	event.WindowTokens = s.windowTokens()
	event.PromptTokens = s.lastPromptTokens
	s.journal(sessionEvent{Type: "context", Context: &event, Step: s.steps})
	if s.recordContext != nil {
		s.recordContext(event)
	}
}
