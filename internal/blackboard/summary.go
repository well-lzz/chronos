package blackboard

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// SummaryStats makes every reinjection decision inspectable by the executor.
type SummaryStats struct {
	Revision          uint64 `json:"revision"`
	BudgetChars       int    `json:"budget_chars"`
	ActualChars       int    `json:"actual_chars"`
	MandatoryFindings int    `json:"mandatory_findings"`
	QuotaFindings     int    `json:"quota_findings"`
	OmittedFindings   int    `json:"omitted_findings"`
	OmittedFacts      int    `json:"omitted_facts"`
	OmittedGoals      int    `json:"omitted_goals"`
	BudgetOverflow    bool   `json:"budget_overflow"`
	ActiveClaims      int    `json:"active_claims"`
}

const (
	mediumFindingQuota = 12
	lowFindingQuota    = 8
)

// SummaryWithBudget retains every verified or high-severity finding, then
// allocates the remaining space to unresolved work. The character limit is a
// target: mandatory findings can exceed it, which is reported explicitly.
func (b *Blackboard) SummaryWithBudget(budget int) (string, SummaryStats) {
	return b.summarySince(0, budget, false)
}

// DeltaSince contains only records changed after revision. The caller can
// inject it inside a long tool cycle without repeating the entire blackboard.
func (b *Blackboard) DeltaSince(revision uint64, budget int) (string, SummaryStats) {
	return b.summarySince(revision, budget, true)
}

func (b *Blackboard) summarySince(since uint64, budget int, delta bool) (string, SummaryStats) {
	if budget < 256 {
		budget = 256
	}
	stats := SummaryStats{Revision: b.Revision(), BudgetChars: budget}
	allFindings := b.FindingsSorted() // shared severity-first ordering used by reports
	facts := b.FactsOrdered()
	allGoals := b.SubgoalsOrdered()
	leases := b.ActiveLeases()
	leaseEvents := b.LeaseEventsSince(since)
	findings, goals := allFindings, allGoals
	if delta {
		findings = filterFindings(findings, since)
		facts = filterFacts(facts, since)
		goals = filterGoals(goals, since)
	}
	if len(findings)+len(facts)+len(goals) == 0 && ((!delta && len(leases) == 0) || (delta && len(leaseEvents) == 0)) {
		return "", stats
	}

	header := "## working record (blackboard)\n"
	if delta {
		header = fmt.Sprintf("## blackboard changes (revision %d → %d)\n", since, stats.Revision)
	}
	var out strings.Builder
	out.WriteString(header)
	appendLine := func(line string) bool {
		if out.Len()+len(line) > budget {
			return false
		}
		out.WriteString(line)
		return true
	}
	if !delta && len(leases) > 0 {
		out.WriteString("active claims (avoid overlapping state-changing work):\n")
		for _, lease := range leases {
			out.WriteString(fmt.Sprintf("- %s owned by %s until %s\n", lease.Surface, lease.Owner, time.Unix(int64(lease.ExpiresAt), 0).UTC().Format(time.RFC3339)))
			stats.ActiveClaims++
		}
	}
	if delta && len(leaseEvents) > 0 {
		out.WriteString("claim changes:\n")
		for _, event := range leaseEvents {
			out.WriteString(fmt.Sprintf("- %s %s by %s\n", event.Surface, event.Action, event.Owner))
		}
	}
	mandatory := make([]Finding, 0)
	optional := make([]Finding, 0)
	for _, f := range findings {
		if f.Status == Verified || f.Severity == "critical" || f.Severity == "high" {
			mandatory = append(mandatory, f)
		} else if f.Status == Candidate || f.Status == Inconclusive {
			optional = append(optional, f)
		} else {
			stats.OmittedFindings++
		}
	}
	if len(findings) > 0 {
		out.WriteString("findings:\n")
	}
	for _, f := range mandatory {
		line := findingLine(f, 160)
		if !appendLine(line) {
			// Never silently evict a mandatory item. Keep a compact ID/status
			// entry, even when the target budget is already exhausted.
			line = findingLine(f, 0)
			out.WriteString(line)
			stats.BudgetOverflow = true
		}
		stats.MandatoryFindings++
	}
	openText := openGoalText(allGoals)
	if len(goals) > 0 {
		out.WriteString("sub-goals:\n")
		for _, g := range goals {
			if !delta && g.Status != "open" {
				continue
			}
			line := fmt.Sprintf("- [%s] %s: %s", g.ID, g.Status, clip(g.Description, 180))
			if g.AssignedTo != "" {
				line += " (assigned to " + g.AssignedTo + ")"
			}
			line += "\n"
			if !appendLine(line) {
				stats.OmittedGoals++
			}
		}
		if stats.OmittedGoals > 0 {
			out.WriteString(fmt.Sprintf("+%d more sub-goals; see report\n", stats.OmittedGoals))
		}
	}
	if len(facts) > 0 {
		out.WriteString("confirmed facts:\n")
		// Structured fact references do not exist yet; explicit fact ID
		// mentions in findings/goals are the reliable importance signal.
		references := openText
		for _, f := range allFindings {
			references += " " + f.Evidence + " " + f.Reproduction
		}
		sort.SliceStable(facts, func(i, j int) bool {
			ai, aj := mentioned(facts[i].ID, references), mentioned(facts[j].ID, references)
			if ai != aj {
				return ai
			}
			return facts[i].Revision > facts[j].Revision
		})
		for _, f := range facts {
			line := fmt.Sprintf("- [%s] %s\n", f.ID, clip(f.Statement, 220))
			if !appendLine(line) {
				stats.OmittedFacts++
			}
		}
		if stats.OmittedFacts > 0 {
			out.WriteString(fmt.Sprintf("+%d more facts; see report\n", stats.OmittedFacts))
		}
	}
	// A candidate related to an open goal wins a quota tie; within that,
	// recently updated findings win. Severity still orders the primary buckets.
	sort.SliceStable(optional, func(i, j int) bool {
		a, z := severityRank(optional[i].Severity), severityRank(optional[j].Severity)
		if a != z {
			return a < z
		}
		ai, aj := related(optional[i].Title, openText), related(optional[j].Title, openText)
		if ai != aj {
			return ai
		}
		return optional[i].Revision > optional[j].Revision
	})
	if len(optional) > 0 || stats.OmittedFindings > 0 {
		out.WriteString("other findings:\n")
	}
	medium, low := 0, 0
	for _, f := range optional {
		isMedium := f.Severity == "medium"
		if (isMedium && medium >= mediumFindingQuota) || (!isMedium && low >= lowFindingQuota) {
			stats.OmittedFindings++
			continue
		}
		if !appendLine(findingLine(f, 160)) {
			stats.OmittedFindings++
			continue
		}
		if isMedium {
			medium++
		} else {
			low++
		}
		stats.QuotaFindings++
	}
	if stats.OmittedFindings > 0 {
		// Count takes precedence over optional detail if the budget is tight.
		out.WriteString(fmt.Sprintf("+%d other findings (candidate/inconclusive/refuted); see report\n", stats.OmittedFindings))
	}
	result := strings.TrimRight(out.String(), "\n")
	stats.ActualChars = len(result)
	stats.BudgetOverflow = stats.BudgetOverflow || stats.ActualChars > budget
	return result, stats
}

func findingLine(f Finding, titleLimit int) string {
	line := fmt.Sprintf("- [%s] (%s/%s)", f.ID, f.Status, f.Severity)
	if titleLimit > 0 {
		line += " " + clip(f.Title, titleLimit)
		if f.Target != "" {
			line += " @ " + clip(f.Target, 80)
		}
		if len(f.EvidenceRefs) > 0 {
			line += fmt.Sprintf(" evidence:%d", len(f.EvidenceRefs))
		}
	}
	return line + "\n"
}

func severityRank(s string) int {
	switch s {
	case "critical":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	case "low":
		return 3
	default:
		return 4
	}
}

func openGoalText(goals []SubGoal) string {
	var b strings.Builder
	for _, g := range goals {
		if g.Status == "open" {
			b.WriteString(g.Description)
			b.WriteByte(' ')
		}
	}
	return b.String()
}

func related(title, goals string) bool {
	for _, word := range strings.Fields(strings.ToLower(title)) {
		if len(word) >= 5 && strings.Contains(strings.ToLower(goals), word) {
			return true
		}
	}
	return false
}

func mentioned(id, text string) bool {
	for _, field := range strings.FieldsFunc(text, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	}) {
		if field == id {
			return true
		}
	}
	return false
}

func filterFindings(in []Finding, since uint64) []Finding {
	out := in[:0]
	for _, x := range in {
		if x.Revision > since {
			out = append(out, x)
		}
	}
	return out
}
func filterFacts(in []Fact, since uint64) []Fact {
	out := in[:0]
	for _, x := range in {
		if x.Revision > since {
			out = append(out, x)
		}
	}
	return out
}
func filterGoals(in []SubGoal, since uint64) []SubGoal {
	out := in[:0]
	for _, x := range in {
		if x.Revision > since {
			out = append(out, x)
		}
	}
	return out
}
