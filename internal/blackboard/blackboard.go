// Package blackboard is the explicit shared search state for one mission:
// Facts (confirmed world state), Findings (search products — e.g. vulns, with a
// candidate→verified|refuted|inconclusive lifecycle and real-traffic evidence),
// and Sub-Goals (the model's live coverage checklist). It is shared across
// executors on the same mission and persisted to JSON on every mutation so a
// long run survives a restart.
package blackboard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Finding lifecycle.
const (
	Candidate    = "candidate"
	Verified     = "verified"
	Refuted      = "refuted"
	Inconclusive = "inconclusive"
)

var statuses = map[string]bool{Candidate: true, Verified: true, Refuted: true, Inconclusive: true}
var severities = map[string]bool{"info": true, "low": true, "medium": true, "high": true, "critical": true}

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

var slugRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func slug(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(s, "-"), "-")
	if s == "" {
		return "mission"
	}
	return s
}

// EvidenceRef cites a real captured exchange (traffic id) with a role.
type EvidenceRef struct {
	Ref  string `json:"ref"`
	Role string `json:"role"`
	Note string `json:"note,omitempty"`
}

// Verification is one append-only verification attempt.
type Verification struct {
	Verdict      string        `json:"verdict"`
	Summary      string        `json:"summary,omitempty"`
	Evidence     string        `json:"evidence,omitempty"`
	EvidenceRefs []EvidenceRef `json:"evidence_refs,omitempty"`
	By           string        `json:"by,omitempty"`
	TS           float64       `json:"ts"`
}

type Fact struct {
	ID        string  `json:"id"`
	Statement string  `json:"statement"`
	Evidence  string  `json:"evidence,omitempty"`
	FoundBy   string  `json:"found_by,omitempty"`
	TS        float64 `json:"ts"`
	Revision  uint64  `json:"revision,omitempty"`
}

type Finding struct {
	ID            string         `json:"id"`
	Title         string         `json:"title"`
	Target        string         `json:"target,omitempty"`
	Category      string         `json:"category,omitempty"`
	Severity      string         `json:"severity"`
	Status        string         `json:"status"`
	Evidence      string         `json:"evidence,omitempty"` // original — immutable after add
	Reproduction  string         `json:"reproduction,omitempty"`
	Remediation   string         `json:"remediation,omitempty"`
	FoundBy       string         `json:"found_by,omitempty"`
	TS            float64        `json:"ts"`
	VerifiedTS    *float64       `json:"verified_ts,omitempty"`
	EvidenceRefs  []EvidenceRef  `json:"evidence_refs,omitempty"`
	Verifications []Verification `json:"verifications,omitempty"`
	Revision      uint64         `json:"revision,omitempty"`
}

type SubGoal struct {
	ID          string  `json:"id"`
	Description string  `json:"description"`
	Status      string  `json:"status"` // open | resolved
	Outcome     string  `json:"outcome,omitempty"`
	CreatedBy   string  `json:"created_by,omitempty"`
	TS          float64 `json:"ts"`
	Revision    uint64  `json:"revision,omitempty"`
	Kind        string  `json:"kind,omitempty"` // verification for independent finding checks
	FindingID   string  `json:"finding_id,omitempty"`
	AssignedTo  string  `json:"assigned_to,omitempty"`
}

// Blackboard is the shared Fact/Finding/Sub-Goal store for one mission.
type Blackboard struct {
	MissionID string
	Path      string

	mu       sync.Mutex
	seq      int
	revision uint64
	facts    map[string]*Fact
	findings map[string]*Finding
	subgoals map[string]*SubGoal
	factOrd  []string
	findOrd  []string
	goalOrd  []string
	leases   map[string]Lease
	leaseLog []LeaseEvent
}

func newBoard(missionID, path string) *Blackboard {
	return &Blackboard{
		MissionID: missionID, Path: path,
		facts: map[string]*Fact{}, findings: map[string]*Finding{}, subgoals: map[string]*SubGoal{}, leases: map[string]Lease{},
	}
}

func (b *Blackboard) nextID(prefix string) string {
	b.seq++
	return fmt.Sprintf("%s%d", prefix, b.seq)
}

func (b *Blackboard) nextRevision() uint64 { b.revision++; return b.revision }

func (b *Blackboard) Revision() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.expireLeasesLocked() {
		_ = b.persist()
	}
	return b.revision
}

// ArtifactsSince reports new facts/findings authored by a delegated agent or
// any of its descendants. Revision alone would accidentally attribute writes
// from concurrently running peer executors to the child.
func (b *Blackboard) ArtifactsSince(revision uint64, actorPrefix string) (facts, findings []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, id := range b.factOrd {
		f := b.facts[id]
		if f.Revision > revision && actorInTree(f.FoundBy, actorPrefix) {
			facts = append(facts, id)
		}
	}
	for _, id := range b.findOrd {
		f := b.findings[id]
		if f.Revision > revision && actorInTree(f.FoundBy, actorPrefix) {
			findings = append(findings, id)
		}
	}
	return facts, findings
}

func actorInTree(actor, prefix string) bool {
	return prefix != "" && (actor == prefix || strings.HasPrefix(actor, prefix+"/sub:"))
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	end := 0
	for i := range s {
		if i > n {
			break
		}
		end = i
	}
	return s[:end]
}

// ----------------------------- mutations ------------------------------------

func (b *Blackboard) AddFact(statement, evidence, foundBy string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID("f")
	b.facts[id] = &Fact{ID: id, Statement: clip(statement, 2000), Evidence: clip(evidence, 2000), FoundBy: foundBy, TS: now(), Revision: b.nextRevision()}
	b.factOrd = append(b.factOrd, id)
	return id, b.persist()
}

func (b *Blackboard) AddFinding(f Finding) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !severities[f.Severity] {
		f.Severity = "info"
	}
	if !statuses[f.Status] {
		f.Status = Candidate
	}
	id := b.nextID("v")
	f.ID = id
	f.Title = clip(f.Title, 300)
	f.Target = clip(f.Target, 300)
	f.Category = clip(f.Category, 80)
	f.Evidence = clip(f.Evidence, 4000)
	f.Reproduction = clip(f.Reproduction, 4000)
	f.Remediation = clip(f.Remediation, 2000)
	f.TS = now()
	f.Revision = b.nextRevision()
	if f.Status == Verified {
		t := now()
		f.VerifiedTS = &t
	}
	b.findings[id] = &f
	b.findOrd = append(b.findOrd, id)
	if f.Status == Candidate {
		goalID := b.nextID("g")
		b.subgoals[goalID] = &SubGoal{ID: goalID, Description: "Independently reproduce or refute finding " + id + ": " + clip(f.Title, 200),
			Status: "open", CreatedBy: f.FoundBy, TS: now(), Revision: b.nextRevision(), Kind: "verification", FindingID: id}
		b.goalOrd = append(b.goalOrd, goalID)
	}
	return id, b.persist()
}

func dedupRefs(existing []EvidenceRef, add []EvidenceRef) []EvidenceRef {
	seen := map[string]bool{}
	for _, r := range existing {
		seen[r.Ref+"|"+r.Role] = true
	}
	for _, r := range add {
		if r.Ref == "" {
			continue
		}
		k := r.Ref + "|" + r.Role
		if !seen[k] {
			existing = append(existing, r)
			seen[k] = true
		}
	}
	return existing
}

func (b *Blackboard) AddEvidenceRefs(findingID string, refs []EvidenceRef) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f := b.findings[findingID]
	if f == nil || len(refs) == 0 {
		return false, nil
	}
	before := len(f.EvidenceRefs)
	f.EvidenceRefs = dedupRefs(f.EvidenceRefs, refs)
	if len(f.EvidenceRefs) == before {
		return false, nil
	}
	f.Revision = b.nextRevision()
	return true, b.persist()
}

// VerifyFinding appends an immutable verification record and updates status to
// the latest verdict; the finding's original Evidence is never overwritten.
func (b *Blackboard) VerifyFinding(findingID, verdict, summary, evidence string, refs []EvidenceRef, by string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f := b.findings[findingID]
	if f == nil || !statuses[verdict] {
		return false, nil
	}
	f.Verifications = append(f.Verifications, Verification{
		Verdict: verdict, Summary: clip(summary, 1000), Evidence: clip(evidence, 4000),
		EvidenceRefs: refs, By: by, TS: now(),
	})
	f.Status = verdict
	if verdict == Verified {
		t := now()
		f.VerifiedTS = &t
	}
	f.EvidenceRefs = dedupRefs(f.EvidenceRefs, refs)
	f.Revision = b.nextRevision()
	if verdict == Verified || verdict == Refuted {
		for _, goal := range b.subgoals {
			if goal.Kind == "verification" && goal.FindingID == findingID && goal.Status == "open" {
				goal.Status = "resolved"
				goal.Outcome = verdict + " by " + by
				goal.Revision = b.nextRevision()
			}
		}
	}
	return true, b.persist()
}

func (b *Blackboard) AddSubgoal(desc, createdBy string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID("g")
	b.subgoals[id] = &SubGoal{ID: id, Description: clip(desc, 500), Status: "open", CreatedBy: createdBy, TS: now(), Revision: b.nextRevision()}
	b.goalOrd = append(b.goalOrd, id)
	return id, b.persist()
}

func (b *Blackboard) ResolveSubgoal(id, outcome string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	g := b.subgoals[id]
	if g == nil {
		return false, nil
	}
	if g.Kind == "verification" {
		return false, fmt.Errorf("verification sub-goal %s resolves only through verify_finding", id)
	}
	g.Status = "resolved"
	g.Outcome = clip(outcome, 1000)
	g.Revision = b.nextRevision()
	b.releaseSubgoalLocked(id)
	return true, b.persist()
}

// ------------------------------ reads ---------------------------------------

func (b *Blackboard) OpenSubgoals() []*SubGoal {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*SubGoal
	for _, id := range b.goalOrd {
		if g := b.subgoals[id]; g != nil && g.Status == "open" {
			copy := *g
			out = append(out, &copy)
		}
	}
	return out
}

// ProgressMarker is a monotone discovery counter for the diminishing-returns
// signal: facts + findings + verified promotions. Sub-goal churn and
// refutations deliberately do not move it.
func (b *Blackboard) ProgressMarker() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := len(b.facts) + len(b.findings)
	for _, f := range b.findings {
		if f.Status == Verified {
			m++
		}
	}
	return m
}

func (b *Blackboard) Counts() map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := map[string]int{"facts": len(b.facts), "findings": len(b.findings),
		"subgoals_total": len(b.subgoals)}
	for _, f := range b.findings {
		switch f.Status {
		case Verified:
			c["findings_verified"]++
		case Candidate:
			c["findings_candidate"]++
		case Inconclusive:
			c["findings_inconclusive"]++
		case Refuted:
			c["findings_refuted"]++
		}
	}
	for _, g := range b.subgoals {
		if g.Status == "open" {
			c["subgoals_open"]++
		}
	}
	return c
}

// Summary is the budgeted working record injected into an executor's context.
func (b *Blackboard) Summary() string {
	text, _ := b.SummaryWithBudget(12_000)
	return text
}

// FindingsSorted returns findings severity-first (critical→info), then
// verified→candidate→inconclusive→refuted — for reports.
func (b *Blackboard) FindingsSorted() []Finding {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Finding, 0, len(b.findings))
	for _, id := range b.findOrd {
		out = append(out, *b.findings[id])
	}
	sevRank := map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3, "info": 4}
	stRank := map[string]int{Verified: 0, Candidate: 1, Inconclusive: 2, Refuted: 3}
	sort.SliceStable(out, func(i, j int) bool {
		if sevRank[out[i].Severity] != sevRank[out[j].Severity] {
			return sevRank[out[i].Severity] < sevRank[out[j].Severity]
		}
		return stRank[out[i].Status] < stRank[out[j].Status]
	})
	return out
}

// Verdict maps the internal finding status to the real-business presentation
// vocabulary: confirmed (确认) / suspected (疑似) / failed (失败). A real
// assessment has no "solved/unsolved/deferred" — only these finding verdicts.
func Verdict(status string) string {
	switch status {
	case Verified:
		return "confirmed"
	case Refuted:
		return "failed"
	default: // candidate, inconclusive
		return "suspected"
	}
}

// FindingRow is the presentation view of one finding for a console snapshot.
type FindingRow struct {
	ID            string         `json:"id"`
	Title         string         `json:"title"`
	Severity      string         `json:"severity"`
	Status        string         `json:"status"`
	Verdict       string         `json:"verdict"`
	Target        string         `json:"target,omitempty"`
	Category      string         `json:"category,omitempty"`
	Evidence      int            `json:"evidence"`
	EvidenceText  string         `json:"evidence_text,omitempty"`
	Reproduction  string         `json:"reproduction,omitempty"`
	Remediation   string         `json:"remediation,omitempty"`
	FoundBy       string         `json:"found_by,omitempty"`
	TS            float64        `json:"ts"`
	VerifiedTS    *float64       `json:"verified_ts,omitempty"`
	EvidenceRefs  []EvidenceRef  `json:"evidence_refs,omitempty"`
	Verifications []Verification `json:"verifications,omitempty"`
}

// FindingsPublic returns findings as presentation rows (verdict-labeled, sorted).
func (b *Blackboard) FindingsPublic() []FindingRow {
	fs := b.FindingsSorted()
	out := make([]FindingRow, 0, len(fs))
	for _, f := range fs {
		out = append(out, FindingRow{
			ID: f.ID, Title: f.Title, Severity: f.Severity, Status: f.Status,
			Verdict: Verdict(f.Status), Target: f.Target, Category: f.Category,
			Evidence: len(f.EvidenceRefs), EvidenceText: f.Evidence,
			Reproduction: f.Reproduction, Remediation: f.Remediation, FoundBy: f.FoundBy,
			TS: f.TS, VerifiedTS: f.VerifiedTS, EvidenceRefs: f.EvidenceRefs,
			Verifications: f.Verifications,
		})
	}
	return out
}

// VerdictCounts buckets findings into confirmed / suspected / failed (+ total).
func (b *Blackboard) VerdictCounts() map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := map[string]int{"confirmed": 0, "suspected": 0, "failed": 0, "total": len(b.findings)}
	for _, f := range b.findings {
		c[Verdict(f.Status)]++
	}
	return c
}

func (b *Blackboard) FactsOrdered() []Fact {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Fact, 0, len(b.factOrd))
	for _, id := range b.factOrd {
		out = append(out, *b.facts[id])
	}
	return out
}

func (b *Blackboard) SubgoalsOrdered() []SubGoal {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]SubGoal, 0, len(b.goalOrd))
	for _, id := range b.goalOrd {
		out = append(out, *b.subgoals[id])
	}
	return out
}

// SubgoalRow is the console view of one sub-goal (the live coverage checklist).
type SubgoalRow struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Status      string `json:"status"` // open | resolved
	Outcome     string `json:"outcome,omitempty"`
	Done        bool   `json:"done"`
	Kind        string `json:"kind,omitempty"`
	FindingID   string `json:"finding_id,omitempty"`
	AssignedTo  string `json:"assigned_to,omitempty"`
}

// SubgoalsPublic returns the coverage checklist as light rows for a snapshot:
// resolved goals first (so progress reads top-down), then open ones, each in
// creation order.
func (b *Blackboard) SubgoalsPublic() []SubgoalRow {
	b.mu.Lock()
	defer b.mu.Unlock()
	var done, open []SubgoalRow
	for _, id := range b.goalOrd {
		g := b.subgoals[id]
		if g == nil {
			continue
		}
		row := SubgoalRow{ID: g.ID, Description: g.Description, Status: g.Status, Outcome: g.Outcome, Done: g.Status == "resolved",
			Kind: g.Kind, FindingID: g.FindingID, AssignedTo: g.AssignedTo}
		if row.Done {
			done = append(done, row)
		} else {
			open = append(open, row)
		}
	}
	return append(done, open...)
}

// ---------------------------- persistence -----------------------------------

type diskModel struct {
	MissionID string       `json:"mission_id"`
	Seq       int          `json:"seq"`
	Revision  uint64       `json:"revision,omitempty"`
	Facts     []Fact       `json:"facts"`
	Findings  []Finding    `json:"findings"`
	Subgoals  []SubGoal    `json:"subgoals"`
	Leases    []Lease      `json:"leases,omitempty"`
	LeaseLog  []LeaseEvent `json:"lease_log,omitempty"`
}

// persist assumes the caller holds the lock.
func (b *Blackboard) persist() error {
	if b.Path == "" {
		return nil
	}
	m := diskModel{MissionID: b.MissionID, Seq: b.seq, Revision: b.revision}
	for _, id := range b.factOrd {
		m.Facts = append(m.Facts, *b.facts[id])
	}
	for _, id := range b.findOrd {
		m.Findings = append(m.Findings, *b.findings[id])
	}
	for _, id := range b.goalOrd {
		m.Subgoals = append(m.Subgoals, *b.subgoals[id])
	}
	for _, lease := range b.leases {
		m.Leases = append(m.Leases, lease)
	}
	m.LeaseLog = append(m.LeaseLog, b.leaseLog...)
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.Path), 0o700); err != nil {
		return err
	}
	tmp := b.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, b.Path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// LoadOrNew loads a mission's blackboard from state_dir, or starts a fresh one.
func LoadOrNew(missionID, stateDir string) (*Blackboard, error) {
	path := filepath.Join(stateDir, "blackboards", slug(missionID)+".json")
	b := newBoard(missionID, path)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return b, nil
		}
		return nil, err
	}
	var m diskModel
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("load blackboard %s: %w", path, err)
	}
	b.seq = m.Seq
	b.revision = m.Revision
	for i := range m.Facts {
		fc := m.Facts[i]
		b.facts[fc.ID] = &fc
		b.factOrd = append(b.factOrd, fc.ID)
	}
	for i := range m.Findings {
		fd := m.Findings[i]
		b.findings[fd.ID] = &fd
		b.findOrd = append(b.findOrd, fd.ID)
	}
	for i := range m.Subgoals {
		g := m.Subgoals[i]
		b.subgoals[g.ID] = &g
		b.goalOrd = append(b.goalOrd, g.ID)
	}
	for _, lease := range m.Leases {
		b.leases[lease.Surface] = lease
	}
	b.leaseLog = m.LeaseLog
	if b.expireLeasesLocked() {
		if err := b.persist(); err != nil {
			return nil, err
		}
	}
	// Legacy boards had no revision stamps. Treat their contents as one initial
	// revision so a newly resumed executor can still receive a full snapshot.
	if b.revision == 0 && b.seq > 0 {
		b.revision = 1
		for _, f := range b.facts {
			f.Revision = 1
		}
		for _, f := range b.findings {
			f.Revision = 1
		}
		for _, g := range b.subgoals {
			g.Revision = 1
		}
	}
	return b, nil
}
