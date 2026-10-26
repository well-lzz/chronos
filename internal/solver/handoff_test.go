package solver

import (
	"strings"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/scope"
)

func TestSaturationNeedsRepeatedIneffectiveCompaction(t *testing.T) {
	s := New(Options{ContextWindow: func(string) int { return 4096 }})
	s.compactedThisCycle = true
	s.compactAfterChars = s.transcriptBudgetChars() * 9 / 10
	s.finishSaturationCycle()
	if s.Saturated() {
		t.Fatal("one compaction triggered handoff")
	}
	s.finishSaturationCycle()
	if !s.Saturated() {
		t.Fatal("repeated ineffective compaction did not trigger handoff")
	}
	s.compactedThisCycle = false
	s.finishSaturationCycle()
	if s.Saturated() {
		t.Fatal("saturation did not clear after a quiet cycle")
	}
}

func TestFreshHandoffKeepsSharedStateWithoutOldTranscript(t *testing.T) {
	bb, err := blackboard.LoadOrNew("handoff", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{ModelSpec: "model-a", BB: bb, Scope: scope.Parse([]string{"target.test"}),
		Deadline: time.Now().Add(time.Minute)})
	s.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "OLD PRIVATE TRANSCRIPT"}}
	fresh := s.FreshHandoff("")
	if fresh == s || fresh.bb != bb || !fresh.scopeSnapshot().Allows("target.test") || len(fresh.messageSnapshot()) != 0 {
		t.Fatal("handoff did not create an isolated window with shared state")
	}
	if len(fresh.pendingUser) != 1 || !strings.Contains(fresh.pendingUser[0].Content, "shared blackboard") {
		t.Fatal("handoff guidance missing")
	}
}
