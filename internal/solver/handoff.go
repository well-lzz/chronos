package solver

// finishSaturationCycle requires repeated compaction with little room freed.
// One unusually large response cannot trigger a handoff by itself.
func (s *Solver) finishSaturationCycle() {
	if s.compactedThisCycle && s.compactAfterChars >= s.transcriptBudgetChars()*3/4 {
		s.saturationCycles++
	} else {
		s.saturationCycles = 0
	}
}

func (s *Solver) Saturated() bool { return s.saturationCycles >= 2 }

// FreshHandoff starts the same neutral executor contract with a clean message
// window. Durable blackboard and traffic remain shared, while the old journal
// stays intact for audit and the new generation gets its own journal.
func (s *Solver) FreshHandoff(sessionPath string) *Solver {
	s.modelMu.RLock()
	client, model := s.client, s.Model
	s.modelMu.RUnlock()
	fresh := New(Options{
		ModelSpec: s.ModelSpec, Model: model, Client: client,
		Meta: s.meta, Scope: s.scopeSnapshot(), BB: s.bb, TL: s.tl,
		AllowExec: s.allowExec, PlaywrightMCP: s.playwrightMCP, InsecureTLS: s.insecureTLS, Deadline: s.deadline,
		Tokens: s.tokens, LLMRecorder: s.llmRecorder, Guard: s.guard, Notify: s.notify, SubmitFn: s.submitFn,
		Record: s.record, RecordEvent: s.recordEvent, RecordContext: s.recordContext, ContextWindow: s.contextWindow,
		Resolve: s.resolve, Complete: s.complete, Paused: s.paused, WaitPaused: s.waitPaused, SessionPath: sessionPath,
		MaxDelegateDepth: s.maxDelegateDepth, MaxDelegateChildren: s.maxDelegateChildren,
		MaxDelegateSteps: s.maxDelegateSteps, MinRemainingToSpawn: s.minRemainingToSpawn,
		GlobalRemaining: s.globalRemaining, SubagentRoot: s.subagentRoot,
		RecordChildEvent: s.recordChildEvent, VerificationPeer: s.verificationPeer,
	})
	fresh.Guide("Take over this mission from a saturated executor. The shared blackboard is the source of truth: inspect open sub-goals and cited traffic, continue unresolved work, and independently verify candidates assigned to you. Avoid repeating already resolved work.")
	return fresh
}
