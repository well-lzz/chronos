package blackboard

import "fmt"

// AssignVerification directs one pending verification to a peer executor. The
// goal remains open until reproduced or refuted; inconclusive attempts leave
// it available for another pass.
func (b *Blackboard) AssignVerification(findingID, assignee string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.findings[findingID] == nil {
		return "", fmt.Errorf("unknown finding %s", findingID)
	}
	for _, id := range b.goalOrd {
		goal := b.subgoals[id]
		if goal.Kind == "verification" && goal.FindingID == findingID && goal.Status == "open" {
			if goal.AssignedTo != assignee {
				goal.AssignedTo = assignee
				goal.Revision = b.nextRevision()
				if err := b.persist(); err != nil {
					return "", err
				}
			}
			return id, nil
		}
	}
	return "", fmt.Errorf("finding %s has no open verification goal", findingID)
}

func (b *Blackboard) VerificationAssignment(findingID string) (goalID, assignee string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, id := range b.goalOrd {
		goal := b.subgoals[id]
		if goal.Kind == "verification" && goal.FindingID == findingID {
			return id, goal.AssignedTo
		}
	}
	return "", ""
}

func (b *Blackboard) CheckVerificationActor(findingID, actor string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, id := range b.goalOrd {
		goal := b.subgoals[id]
		if goal.Kind != "verification" || goal.FindingID != findingID {
			continue
		}
		if goal.AssignedTo != "" && goal.AssignedTo != actor {
			return fmt.Errorf("%s is assigned to %s; let that peer independently assess it", id, goal.AssignedTo)
		}
		if goal.AssignedTo == "" && b.findings[findingID] != nil && b.findings[findingID].FoundBy == actor {
			return fmt.Errorf("%s is awaiting an independent verifier", id)
		}
		return nil
	}
	return nil
}

// ReassignVerificationOwner clears work directed at an executor that exited.
// The next living executor may then pick it up; no candidate is silently lost.
func (b *Blackboard) ReassignVerificationOwner(owner string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	changed := false
	for _, goal := range b.subgoals {
		if goal.Kind == "verification" && goal.Status == "open" && goal.AssignedTo == owner {
			goal.AssignedTo = ""
			goal.Revision = b.nextRevision()
			changed = true
		}
	}
	if changed {
		return b.persist()
	}
	return nil
}
