package solver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/nettools"
)

// dispatch executes one tool call and returns the tool result text.
func (s *Solver) dispatch(ctx context.Context, name, argsJSON string) string {
	switch name {
	case "http_request":
		return s.toolHTTP(ctx, argsJSON)
	case "tcp_send":
		return s.toolTCP(argsJSON)
	case "run_python":
		return s.toolRunPython(ctx, argsJSON)
	case "add_fact":
		return s.toolAddFact(argsJSON)
	case "add_finding":
		return s.toolAddFinding(argsJSON)
	case "verify_finding":
		return s.toolVerifyFinding(argsJSON)
	case "add_subgoal":
		return s.toolAddSubgoal(argsJSON)
	case "resolve_subgoal":
		return s.toolResolveSubgoal(argsJSON)
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
	default:
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
	display := nettools.DoRawHTTP(ctx, a.URL, a.Method, a.Headers, a.Body, 30*time.Second)
	tid := s.tl.RecordHTTP(a.Method, a.URL, a.Headers, a.Body, display, s.ModelSpec)
	return display + fmt.Sprintf("\n[traffic %s — cite as evidence with add_finding/verify_finding]", tid)
}

func (s *Solver) toolTCP(argsJSON string) string {
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
	payload, err := decodePayload(a.Payload, a.PayloadHex)
	if err != nil {
		return "tcp_send error: " + err.Error()
	}
	display := nettools.DoTCPSend(a.Host, a.Port, payload, 30*time.Second, 3*time.Second)
	tid := s.tl.RecordTCP(a.Host, a.Port, a.Payload, a.PayloadHex, display, s.ModelSpec)
	return display + fmt.Sprintf("\n[traffic %s — cite as evidence]", tid)
}

func (s *Solver) toolAddFact(argsJSON string) string {
	var a struct {
		Statement string `json:"statement"`
		Evidence  string `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "add_fact error: bad args"
	}
	id := s.bb.AddFact(a.Statement, a.Evidence, s.ModelSpec)
	return "fact " + id + " recorded."
}

func (s *Solver) toolAddFinding(argsJSON string) string {
	var a struct {
		Title        string  `json:"title"`
		Severity     string  `json:"severity"`
		Target       string  `json:"target"`
		Category     string  `json:"category"`
		Evidence     string  `json:"evidence"`
		Reproduction string  `json:"reproduction"`
		Verified     bool    `json:"verified"`
		EvidenceRefs []evRef `json:"evidence_refs"`
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
	status := blackboard.Candidate
	if a.Verified {
		status = blackboard.Verified
	}
	refs, warns := s.normalizeRefs(a.EvidenceRefs)
	id := s.bb.AddFinding(blackboard.Finding{
		Title: a.Title, Severity: a.Severity, Target: a.Target, Category: a.Category,
		Status: status, Evidence: a.Evidence, Reproduction: a.Reproduction,
		FoundBy: s.ModelSpec, EvidenceRefs: refs,
	})
	msg := "finding " + id + " recorded"
	if a.Verified {
		msg += " (verified)."
	} else {
		msg += " (candidate — verify it next)."
	}
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
	refs, warns := s.normalizeRefs(a.EvidenceRefs)
	if !s.bb.VerifyFinding(a.FindingID, status, a.Summary, a.Evidence, refs, s.ModelSpec) {
		return "no finding " + a.FindingID + "."
	}
	msg := "finding " + a.FindingID + " -> " + status + "."
	if len(warns) > 0 {
		msg += " dropped refs: " + strings.Join(warns, "; ")
	}
	return msg
}

func (s *Solver) toolAddSubgoal(argsJSON string) string {
	var a struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "add_subgoal error: bad args"
	}
	return "sub-goal " + s.bb.AddSubgoal(a.Description, s.ModelSpec) + " added."
}

func (s *Solver) toolResolveSubgoal(argsJSON string) string {
	var a struct {
		SubgoalID string `json:"subgoal_id"`
		Outcome   string `json:"outcome"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "resolve_subgoal error: bad args"
	}
	if s.bb.ResolveSubgoal(a.SubgoalID, a.Outcome) {
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
	// Assessment core has no platform oracle: record the flag as a verified
	// finding so it enters the report, and surface it to the loop.
	s.lastFlag = a.Flag
	s.bb.AddFinding(blackboard.Finding{
		Title: "flag: " + a.Flag, Target: s.meta.Name, Category: "flag",
		Severity: "info", Status: blackboard.Verified, Evidence: "submitted by model", FoundBy: s.ModelSpec,
	})
	return "flag recorded as a verified finding: " + a.Flag
}
