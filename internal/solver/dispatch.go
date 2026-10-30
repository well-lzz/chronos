package solver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/browsermcp"
	"github.com/well-lzz/chronos/internal/nettools"
)

// dispatch executes one tool call and returns the tool result text.
func (s *Solver) dispatch(ctx context.Context, name, argsJSON string) string {
	if s.waitIfPaused(ctx) != nil {
		return "cancelled; call was not executed"
	}
	missionID := s.meta.Name
	if s.bb != nil {
		missionID = s.bb.MissionID
	}
	if blocked := s.preToolScope(name, argsJSON); blocked != "" {
		if s.guard != nil {
			s.guard.BuiltinDeny(missionID, s.ModelSpec, name, blocked)
		}
		return blocked
	}
	if s.guard != nil {
		allowed, message := s.guard.Check(ctx, missionID, s.ModelSpec, name, argsJSON)
		if !allowed {
			return message
		}
	}
	if s.bb != nil {
		_ = s.bb.TouchMentioned(argsJSON)
	}
	result := s.dispatchCore(ctx, name, argsJSON)
	if s.guard != nil {
		s.guard.Post(missionID, s.ModelSpec, name, result)
	}
	return result
}

// preToolScope runs before operator rules; no allow or approval can widen the
// authorized network boundary. Runtime redirects and Python sockets also keep
// their existing scope guards.
func (s *Solver) preToolScope(name, argsJSON string) string {
	var host string
	switch name {
	case "browser_navigate":
		var a struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(argsJSON), &a) != nil {
			return ""
		}
		if err := browsermcp.CheckURL(a.URL, func(host string) bool { return s.scopeSnapshot().Allows(host) }); err != nil {
			return "【平台管控·非目标防御】BLOCKED (scope lock): " + err.Error()
		}
		return ""
	case "http_request":
		var a struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(argsJSON), &a) != nil {
			return ""
		}
		raw := a.URL
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		if parsed, err := url.Parse(raw); err == nil {
			host = parsed.Hostname()
		}
	case "tcp_send":
		var a struct {
			Host string `json:"host"`
		}
		if json.Unmarshal([]byte(argsJSON), &a) != nil {
			return ""
		}
		host = a.Host
	default:
		return ""
	}
	if ok, message := s.hostAllowed(host); !ok {
		return message
	}
	return ""
}

func (s *Solver) dispatchCore(ctx context.Context, name, argsJSON string) string {
	switch name {
	case "http_request":
		return s.toolHTTP(ctx, argsJSON)
	case "tcp_send":
		return s.toolTCP(ctx, argsJSON)
	case "run_python":
		return s.toolRunPython(ctx, argsJSON)
	case "add_fact":
		return s.toolAddFact(argsJSON)
	case "add_finding":
		return s.toolAddFinding(argsJSON)
	case "verify_finding":
		return s.toolVerifyFinding(argsJSON)
	case "challenge_finding":
		return s.toolChallengeFinding(argsJSON)
	case "relate_findings":
		return s.toolRelateFindings(argsJSON)
	case "add_subgoal":
		return s.toolAddSubgoal(argsJSON)
	case "resolve_subgoal":
		return s.toolResolveSubgoal(argsJSON)
	case "claim_surface":
		return s.toolClaimSurface(argsJSON)
	case "release_surface":
		return s.toolReleaseSurface(argsJSON)
	case "traffic_search":
		return s.toolTrafficSearch(argsJSON)
	case "traffic_get":
		return s.toolTrafficGet(argsJSON)
	case "notify":
		var a struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal([]byte(argsJSON), &a)
		if s.notify != nil && strings.TrimSpace(a.Message) != "" {
			s.notify(a.Message)
		}
		return "noted."
	case "submit_flag":
		return s.toolSubmitFlag(argsJSON)
	case "delegate":
		return s.toolDelegate(ctx, argsJSON)
	case "delegate_resume":
		return s.toolDelegateResume(ctx, argsJSON)
	default:
		if isBrowserTool(name) {
			return s.toolBrowser(ctx, name, argsJSON)
		}
		return "unknown tool: " + name
	}
}

func jsonOut(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "error: " + err.Error()
	}
	return string(b)
}

func (s *Solver) toolHTTP(ctx context.Context, argsJSON string) string {
	var a struct {
		URL     string            `json:"url"`
		Method  string            `json:"method"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "http_request error: bad args: " + err.Error()
	}
	raw := a.URL
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	host := ""
	if u, err := url.Parse(raw); err == nil {
		host = u.Hostname()
	}
	if ok, msg := s.hostAllowed(host); !ok {
		return msg
	}
	method := strings.ToUpper(strings.TrimSpace(a.Method))
	if (method != "" && method != "GET" && method != "HEAD") || a.Body != "" {
		surface, _, err := canonicalSurface(raw)
		if err != nil {
			return "http_request error: " + err.Error()
		}
		if blocked := s.requireMutationLease(surface); blocked != "" {
			return blocked
		}
	}
	display := nettools.DoRawHTTP(ctx, a.URL, a.Method, a.Headers, a.Body, 30*time.Second, func(host string) bool {
		return s.scopeSnapshot().Allows(host)
	}, s.insecureTLS, func(req *http.Request) error {
		if (req.Method == http.MethodGet || req.Method == http.MethodHead) && req.ContentLength <= 0 {
			return nil
		}
		surface, _, err := canonicalSurface(req.URL.String())
		if err != nil {
			return err
		}
		if blocked := s.requireMutationLease(surface); blocked != "" {
			return fmt.Errorf("redirect %s", blocked)
		}
		return nil
	})
	tid, err := s.tl.RecordHTTP(a.Method, a.URL, a.Headers, a.Body, display, s.ModelSpec)
	if err != nil {
		return display + "\ntraffic persistence error: " + err.Error()
	}
	return display + fmt.Sprintf("\n[traffic %s — cite as evidence with add_finding/verify_finding]", tid)
}

func (s *Solver) toolTCP(ctx context.Context, argsJSON string) string {
	var a struct {
		Host       string `json:"host"`
		Port       int    `json:"port"`
		Payload    string `json:"payload"`
		PayloadHex string `json:"payload_hex"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "tcp_send error: bad args: " + err.Error()
	}
	if ok, msg := s.hostAllowed(a.Host); !ok {
		return msg
	}
	if blocked := s.requireMutationLease(net.JoinHostPort(strings.ToLower(strings.Trim(a.Host, "[]")), strconv.Itoa(a.Port))); blocked != "" {
		return blocked
	}
	payload, err := decodePayload(a.Payload, a.PayloadHex)
	if err != nil {
		return "tcp_send error: " + err.Error()
	}
	display := nettools.DoTCPSendContext(ctx, a.Host, a.Port, payload, 30*time.Second, 3*time.Second)
	tid, err := s.tl.RecordTCP(a.Host, a.Port, a.Payload, a.PayloadHex, display, s.ModelSpec)
	if err != nil {
		return display + "\ntraffic persistence error: " + err.Error()
	}
	return display + fmt.Sprintf("\n[traffic %s — cite as evidence]", tid)
}

func (s *Solver) toolAddFact(argsJSON string) string {
	var a struct {
		Statement    string   `json:"statement"`
		Evidence     string   `json:"evidence"`
		EvidenceRefs []evRef  `json:"evidence_refs"`
		FromIDs      []string `json:"from_ids"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "add_fact error: bad args"
	}
	refs, warns := s.normalizeRefs(a.EvidenceRefs)
	id, err := s.bb.AddFactWithRefs(a.Statement, a.Evidence, refs, s.ModelSpec)
	if err != nil {
		return "add_fact persistence error: " + err.Error()
	}
	if len(a.FromIDs) > 0 {
		if err := s.bb.Link(id, a.FromIDs); err != nil {
			return "fact " + id + " recorded; lineage error: " + err.Error()
		}
	}
	message := "fact " + id + " recorded; verdict computed from graph."
	if len(warns) > 0 {
		message += " dropped refs: " + strings.Join(warns, "; ")
	}
	return message
}

func findingIdentity(title, target, category string) string {
	normalize := func(value string) string {
		return strings.ToLower(strings.Join(strings.Fields(value), " "))
	}
	return normalize(title) + "\x00" + normalize(target) + "\x00" + normalize(category)
}

func duplicateFindingID(bb *blackboard.Blackboard, title, target, category string) string {
	identity := findingIdentity(title, target, category)
	for _, finding := range bb.FindingsSorted() {
		if findingIdentity(finding.Title, finding.Target, finding.Category) == identity {
			return finding.ID
		}
	}
	return ""
}

func (s *Solver) toolAddFinding(argsJSON string) string {
	var a struct {
		Title        string   `json:"title"`
		Severity     string   `json:"severity"`
		Target       string   `json:"target"`
		Category     string   `json:"category"`
		Evidence     string   `json:"evidence"`
		Reproduction string   `json:"reproduction"`
		EvidenceRefs []evRef  `json:"evidence_refs"`
		FromIDs      []string `json:"from_ids"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "add_finding error: bad args"
	}
	if a.Severity == "" {
		a.Severity = "info"
	}
	if a.Target == "" {
		a.Target = s.meta.Name
	}
	refs, warns := s.normalizeRefs(a.EvidenceRefs)
	if strings.TrimSpace(a.Title) == "" {
		return "add_finding blocked: title is required"
	}
	if strings.TrimSpace(a.Evidence) == "" {
		return "add_finding blocked: evidence must contain actual observations"
	}
	reproduction := strings.TrimSpace(a.Reproduction)
	hasNumberedStep := strings.Contains(reproduction, "1.") || strings.Contains(reproduction, "1)") || strings.Contains(reproduction, "1、")
	if reproduction == "" || !hasNumberedStep || !strings.Contains(strings.ToLower(reproduction), "expected:") {
		return "add_finding blocked: reproduction must contain numbered steps and an Expected: success criterion"
	}
	if len(refs) == 0 {
		return "add_finding blocked: at least one valid captured traffic evidence_ref is required"
	}
	if existing := duplicateFindingID(s.bb, a.Title, a.Target, a.Category); existing != "" {
		return "add_finding blocked: finding " + existing + " already has the same title, target, and category; append a verification to that record instead"
	}
	id, err := s.bb.AddFinding(blackboard.Finding{
		Title: a.Title, Severity: a.Severity, Target: a.Target, Category: a.Category,
		Status: blackboard.Candidate, Evidence: a.Evidence, Reproduction: a.Reproduction,
		FoundBy: s.ModelSpec, EvidenceRefs: refs,
	})
	if err != nil {
		return "add_finding persistence error: " + err.Error()
	}
	if len(a.FromIDs) > 0 {
		if err := s.bb.Link(id, a.FromIDs); err != nil {
			return "finding " + id + " recorded; lineage error: " + err.Error()
		}
	}
	assignee := s.ModelSpec
	if s.verificationPeer != nil {
		if peer := s.verificationPeer(s.ModelSpec, id); peer != "" {
			assignee = peer
		}
	}
	goalID, err := s.bb.AssignVerification(id, assignee)
	if err != nil {
		return "finding " + id + " recorded, but verification assignment failed: " + err.Error()
	}
	s.rec("verify_assign", id, fmt.Sprintf("%s assigned to %s for independent verification", goalID, assignee))
	verdict, reason, _ := s.bb.ClaimVerdict(id)
	msg := fmt.Sprintf("finding %s recorded as %s (%s); verification goal %s assigned to %s.", id, verdict, reason, goalID, assignee)
	if len(refs) > 0 {
		var ids []string
		for _, r := range refs {
			ids = append(ids, r.Ref)
		}
		msg += " evidence: " + strings.Join(ids, ", ") + "."
	}
	if len(warns) > 0 {
		msg += " dropped refs: " + strings.Join(warns, "; ")
	}
	return msg
}

func (s *Solver) toolVerifyFinding(argsJSON string) string {
	var a struct {
		FindingID    string  `json:"finding_id"`
		Verdict      string  `json:"verdict"`
		Summary      string  `json:"summary"`
		Evidence     string  `json:"evidence"`
		EvidenceRefs []evRef `json:"evidence_refs"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "verify_finding error: bad args"
	}
	verdictMap := map[string]string{
		"reproduced": blackboard.Verified, "verified": blackboard.Verified,
		"refuted": blackboard.Refuted, "inconclusive": blackboard.Inconclusive,
	}
	status, ok := verdictMap[strings.ToLower(strings.TrimSpace(a.Verdict))]
	if !ok {
		return "verdict must be one of: reproduced, refuted, inconclusive."
	}
	if err := s.bb.CheckVerificationActor(a.FindingID, s.ModelSpec); err != nil {
		return "verify_finding blocked: " + err.Error()
	}
	refs, warns := s.normalizeRefs(a.EvidenceRefs)
	if (status == blackboard.Verified || status == blackboard.Refuted) && len(refs) == 0 {
		return "verify_finding blocked: reproduced/refuted verdict requires a real captured traffic evidence_ref"
	}
	if strings.TrimSpace(a.Summary) == "" || strings.TrimSpace(a.Evidence) == "" {
		return "verify_finding blocked: summary and actual replay observations or blocker evidence are required"
	}
	if status == blackboard.Verified || status == blackboard.Refuted {
		hasVerificationTraffic := false
		for _, ref := range refs {
			if ref.Role == "verification" {
				hasVerificationTraffic = true
				break
			}
		}
		if !hasVerificationTraffic {
			return "verify_finding blocked: reproduced/refuted verdict requires new replay traffic with role=verification"
		}
	}
	persisted, err := s.bb.VerifyFinding(a.FindingID, status, a.Summary, a.Evidence, refs, s.ModelSpec)
	if err != nil {
		return "verify_finding persistence error: " + err.Error()
	}
	if !persisted {
		return "no finding " + a.FindingID + "."
	}
	computed, reason, _ := s.bb.ClaimVerdict(a.FindingID)
	msg := "finding " + a.FindingID + " verification recorded; graph verdict: " + computed + " (" + reason + ")."
	if len(warns) > 0 {
		msg += " dropped refs: " + strings.Join(warns, "; ")
	}
	return msg
}

func (s *Solver) toolChallengeFinding(argsJSON string) string {
	var a struct {
		TargetID     string  `json:"target_id"`
		Reason       string  `json:"reason"`
		EvidenceRefs []evRef `json:"evidence_refs"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "challenge_finding error: bad args"
	}
	refs, warns := s.normalizeRefs(a.EvidenceRefs)
	id, err := s.bb.ChallengeFinding(a.TargetID, a.Reason, refs, s.ModelSpec)
	if err != nil {
		return "challenge_finding error: " + err.Error()
	}
	verdict, reason, _ := s.bb.ClaimVerdict(a.TargetID)
	msg := fmt.Sprintf("challenge %s recorded; %s is %s (%s).", id, a.TargetID, verdict, reason)
	if len(warns) > 0 {
		msg += " dropped refs: " + strings.Join(warns, "; ")
	}
	return msg
}

func (s *Solver) toolRelateFindings(argsJSON string) string {
	var a struct {
		A    string `json:"a"`
		B    string `json:"b"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "relate_findings error: bad args"
	}
	if err := s.bb.RelateClaims(a.A, a.B, a.Kind, s.ModelSpec); err != nil {
		return "relate_findings error: " + err.Error()
	}
	verdict, reason, _ := s.bb.ClaimVerdict(a.B)
	return fmt.Sprintf("%s %s -> %s recorded; target verdict %s (%s).", a.Kind, a.A, a.B, verdict, reason)
}

func (s *Solver) toolAddSubgoal(argsJSON string) string {
	var a struct {
		Description string   `json:"description"`
		FromIDs     []string `json:"from_ids"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "add_subgoal error: bad args"
	}
	id, err := s.bb.AddSubgoal(a.Description, s.ModelSpec)
	if err != nil {
		return "add_subgoal persistence error: " + err.Error()
	}
	if len(a.FromIDs) > 0 {
		if err := s.bb.Link(id, a.FromIDs); err != nil {
			return "sub-goal " + id + " added; lineage error: " + err.Error()
		}
	}
	return "sub-goal " + id + " added."
}

func (s *Solver) toolResolveSubgoal(argsJSON string) string {
	var a struct {
		SubgoalID string `json:"subgoal_id"`
		Outcome   string `json:"outcome"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "resolve_subgoal error: bad args"
	}
	ok, err := s.bb.ResolveSubgoal(a.SubgoalID, a.Outcome)
	if err != nil {
		return "resolve_subgoal persistence error: " + err.Error()
	}
	if ok {
		return "sub-goal " + a.SubgoalID + " resolved."
	}
	return "no sub-goal " + a.SubgoalID + "."
}

func (s *Solver) toolTrafficSearch(argsJSON string) string {
	var a struct {
		Host         string `json:"host"`
		URLContains  string `json:"url_contains"`
		BodyContains string `json:"body_contains"`
		Page         int    `json:"page"`
	}
	_ = json.Unmarshal([]byte(argsJSON), &a)
	return jsonOut(s.tl.Search(a.Host, a.URLContains, a.BodyContains, a.Page))
}

func (s *Solver) toolTrafficGet(argsJSON string) string {
	var a struct {
		TrafficID string `json:"traffic_id"`
		Side      string `json:"side"`
		Offset    int    `json:"offset"`
		Length    int    `json:"length"`
	}
	_ = json.Unmarshal([]byte(argsJSON), &a)
	return jsonOut(s.tl.Get(a.TrafficID, a.Side, a.Offset, a.Length))
}

func (s *Solver) toolSubmitFlag(argsJSON string) string {
	var a struct {
		Flag string `json:"flag"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "submit_flag error: bad args"
	}
	a.Flag = strings.TrimSpace(a.Flag)
	if a.Flag == "" {
		return "submit_flag error: empty flag"
	}
	// With a platform SubmitFn (scheduler/TSec mode), the flag is submitted for
	// real; only a confirmed flag is recorded as a verified finding. Without one
	// (engagement mode, no oracle), record it directly.
	if s.submitFn != nil {
		display, confirmed, complete := s.submitFn(a.Flag)
		if confirmed {
			s.lastFlag = a.Flag
			s.flagComplete = complete
			if _, err := s.bb.AddFinding(blackboard.Finding{
				Title: "flag: " + a.Flag, Target: s.meta.Name, Category: "flag",
				Severity: "info", Oracle: true, Evidence: display, FoundBy: s.ModelSpec,
			}); err != nil {
				return display + "\nflag persistence error: " + err.Error()
			}
		}
		return display
	}
	s.lastFlag = a.Flag
	s.flagComplete = true
	if _, err := s.bb.AddFinding(blackboard.Finding{
		Title: "flag: " + a.Flag, Target: s.meta.Name, Category: "flag",
		Severity: "info", Evidence: "submitted by model (no oracle)", FoundBy: s.ModelSpec,
	}); err != nil {
		return "flag persistence error: " + err.Error()
	}
	return "flag recorded as an unverified claim (no platform oracle): " + a.Flag
}
