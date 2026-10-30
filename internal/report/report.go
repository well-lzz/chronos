// Package report renders a mission's blackboard into local artifacts:
// report.json (structured), report.md (readable) and report.html
// (self-contained). Findings come pre-sorted severity-first from the
// blackboard; evidence traffic and verification history are included.
package report

import (
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/well-lzz/chronos/internal/blackboard"
)

// RunMeta is the run-level metadata shown in the report header.
type RunMeta struct {
	Mode            string   `json:"mode,omitempty"`
	Goal            string   `json:"goal,omitempty"`
	Scope           string   `json:"scope,omitempty"`
	Models          []string `json:"models,omitempty"`
	Started         string   `json:"started,omitempty"`
	Ended           string   `json:"ended,omitempty"`
	DurationSeconds int      `json:"duration_seconds,omitempty"`
	CostUSD         float64  `json:"cost_usd,omitempty"`
}

type report struct {
	Mission       map[string]any           `json:"mission"`
	GeneratedAt   string                   `json:"generated_at"`
	Summary       map[string]int           `json:"summary"`
	Findings      []blackboard.Finding     `json:"findings"`
	Facts         []blackboard.Fact        `json:"facts"`
	Subgoals      []blackboard.SubGoal     `json:"subgoals"`
	EvidenceGraph blackboard.GraphSnapshot `json:"evidence_graph"`
}

func build(bb *blackboard.Blackboard, rm RunMeta) report {
	mi := map[string]any{"name": bb.MissionID}
	if rm.Mode != "" {
		mi["mode"] = rm.Mode
	}
	if rm.Goal != "" {
		mi["goal"] = rm.Goal
	}
	if rm.Scope != "" {
		mi["scope"] = rm.Scope
	}
	if len(rm.Models) > 0 {
		mi["models"] = rm.Models
	}
	if rm.Started != "" {
		mi["started"] = rm.Started
	}
	if rm.Ended != "" {
		mi["ended"] = rm.Ended
	}
	if rm.DurationSeconds > 0 {
		mi["duration_seconds"] = rm.DurationSeconds
	}
	if rm.CostUSD > 0 {
		mi["cost_usd"] = rm.CostUSD
	}
	return report{
		Mission:       mi,
		GeneratedAt:   time.Now().Format("2006-01-02 15:04:05 -0700"),
		Summary:       bb.Counts(),
		Findings:      bb.FindingsSorted(),
		Facts:         bb.FactsOrdered(),
		Subgoals:      bb.SubgoalsOrdered(),
		EvidenceGraph: bb.Graph(),
	}
}

// RenderReports writes report.{json,md,html} into outDir, returns the paths.
func RenderReports(bb *blackboard.Blackboard, outDir string, rm RunMeta) (map[string]string, error) {
	r := build(bb, rm)
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return nil, err
	}
	paths := map[string]string{
		"json": filepath.Join(outDir, "report.json"),
		"md":   filepath.Join(outDir, "report.md"),
		"html": filepath.Join(outDir, "report.html"),
	}
	jsonData, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(paths["json"], jsonData, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(paths["md"], []byte(renderMarkdown(r)), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(paths["html"], []byte(renderHTML(r)), 0o600); err != nil {
		return nil, err
	}
	return paths, nil
}

func metaStr(m map[string]any, k string) string {
	v, ok := m[k]
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case []string:
		return strings.Join(t, ", ")
	case int:
		return fmt.Sprintf("%d", t)
	case float64:
		return fmt.Sprintf("%g", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

var metaOrder = []struct{ key, label string }{
	{"mode", "Mode"}, {"goal", "Goal"}, {"scope", "Scope"}, {"models", "Models"},
	{"started", "Started"}, {"ended", "Ended"}, {"duration_seconds", "Duration (s)"}, {"cost_usd", "Cost (USD)"},
}

func renderMarkdown(r report) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s + "\n") }
	w("# Mission report — " + metaStr(r.Mission, "name"))
	w("")
	for _, mo := range metaOrder {
		if v := metaStr(r.Mission, mo.key); v != "" {
			w(fmt.Sprintf("- **%s**: %s", mo.label, v))
		}
	}
	s := r.Summary
	w("")
	w("## Summary")
	w(fmt.Sprintf("- Findings: **%d** (verified %d, candidate %d, inconclusive %d, refuted %d)",
		s["findings"], s["findings_verified"], s["findings_candidate"], s["findings_inconclusive"], s["findings_refuted"]))
	w(fmt.Sprintf("- Facts established: %d", s["facts"]))
	w(fmt.Sprintf("- Sub-goals: %d (%d open)", s["subgoals_total"], s["subgoals_open"]))
	w("")
	w("## Findings")
	if len(r.Findings) == 0 {
		w("_No findings recorded._")
	}
	for _, f := range r.Findings {
		w(fmt.Sprintf("### [%s] %s  ·  _%s_", strings.ToUpper(f.Severity), f.Title, f.Status))
		w("- **Computed verdict**: " + blackboard.Verdict(f.Status) + " — " + f.VerdictReason)
		if f.Target != "" {
			w("- **Target**: " + f.Target)
		}
		if f.Category != "" {
			w("- **Category**: " + f.Category)
		}
		if f.Evidence != "" {
			w("- **Evidence**:")
			w("")
			w("  ```")
			for _, ln := range strings.Split(f.Evidence, "\n") {
				w("  " + ln)
			}
			w("  ```")
		}
		if f.Reproduction != "" {
			w("- **Reproduction**:")
			w("")
			for _, ln := range strings.Split(f.Reproduction, "\n") {
				w("  " + ln)
			}
		}
		if f.Remediation != "" {
			w("- **Remediation**: " + f.Remediation)
		}
		if len(f.EvidenceRefs) > 0 {
			var parts []string
			for _, rf := range f.EvidenceRefs {
				p := fmt.Sprintf("`%s` (%s", rf.Ref, rf.Role)
				if rf.Note != "" {
					p += ": " + rf.Note
				}
				parts = append(parts, p+")")
			}
			w("- **Evidence traffic**: " + strings.Join(parts, ", "))
		}
		if len(f.Verifications) > 0 {
			w("- **Verification history**:")
			for _, v := range f.Verifications {
				var vr []string
				for _, rf := range v.EvidenceRefs {
					vr = append(vr, rf.Ref)
				}
				line := "  - _" + v.Verdict + "_"
				if v.Summary != "" {
					line += " — " + v.Summary
				}
				if len(vr) > 0 {
					line += " (traffic: " + strings.Join(vr, ", ") + ")"
				}
				w(line)
				if v.Evidence != "" {
					w("")
					for _, ln := range strings.Split(v.Evidence, "\n") {
						w("    " + ln)
					}
				}
			}
		}
		w("")
	}
	if len(r.Facts) > 0 {
		w("## Established facts")
		for _, x := range r.Facts {
			w("- [" + x.ID + "] " + x.Statement + " — " + x.Verdict + ": " + x.VerdictReason)
		}
		w("")
	}
	w("## Evidence argument graph")
	w(fmt.Sprintf("%d claims, %d evidence nodes, %d verification attempts, %d edges.", len(r.EvidenceGraph.Claims), len(r.EvidenceGraph.Evidence), len(r.EvidenceGraph.Verifications), len(r.EvidenceGraph.Edges)))
	w("")
	w("### Claims")
	for _, claim := range r.EvidenceGraph.Claims {
		w(fmt.Sprintf("- `%s` (%s): %s — %s: %s", claim.ID, claim.Kind, claim.Text, claim.Verdict, claim.Reason))
	}
	w("")
	w("### Verification attempts")
	for _, attempt := range r.EvidenceGraph.Verifications {
		w(fmt.Sprintf("- `%s` → `%s`: %s — %s", attempt.ID, attempt.FindingID, attempt.Outcome, attempt.Summary))
	}
	w("")
	w("### Edges")
	for _, edge := range r.EvidenceGraph.Edges {
		line := fmt.Sprintf("- `%s` —%s→ `%s`", edge.From, edge.Kind, edge.To)
		if edge.Role != "" {
			line += " (" + edge.Role + ")"
		}
		w(line)
	}
	w("")
	if len(r.Subgoals) > 0 {
		w("## Sub-goals (coverage checklist)")
		for _, g := range r.Subgoals {
			mark := " "
			if g.Status == "resolved" {
				mark = "x"
			}
			tail := ""
			if g.Outcome != "" {
				tail = " — " + g.Outcome
			}
			w(fmt.Sprintf("- [%s] %s%s", mark, g.Description, tail))
		}
		w("")
	}
	return b.String()
}

var sevColor = map[string]string{
	"critical": "#b00020", "high": "#d9480f", "medium": "#b8860b", "low": "#2b6cb0", "info": "#555",
}

func h(s string) string { return html.EscapeString(s) }

func renderHTML(r report) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s + "\n") }
	name := metaStr(r.Mission, "name")
	w("<!doctype html><html lang='en'><head><meta charset='utf-8'>")
	w("<meta name='viewport' content='width=device-width, initial-scale=1'>")
	w("<title>Mission report — " + h(name) + "</title>")
	w("<style>")
	w(":root{color-scheme:light dark}")
	w("body{font:16px/1.6 system-ui,-apple-system,Segoe UI,Roboto,sans-serif;max-width:60rem;margin:2rem auto;padding:0 1rem}")
	w("h1{margin-bottom:.2rem}h3{margin:1.4rem 0 .2rem}")
	w(".meta li{margin:.1rem 0}.sev{font-weight:700;padding:.05rem .4rem;border-radius:.3rem;color:#fff}")
	w(".status{color:#666;font-style:italic;margin-left:.4rem}")
	w("pre{background:#00000011;padding:.6rem;border-radius:.4rem;overflow:auto;white-space:pre-wrap}")
	w(".card{border:1px solid #00000022;border-radius:.5rem;padding:.8rem 1rem;margin:.8rem 0}")
	w("</style></head><body>")
	w("<h1>Mission report — " + h(name) + "</h1>")
	w("<p>Generated " + h(r.GeneratedAt) + "</p>")
	w("<ul class='meta'>")
	for _, mo := range metaOrder {
		if v := metaStr(r.Mission, mo.key); v != "" {
			w("<li><b>" + h(mo.key) + "</b>: " + h(v) + "</li>")
		}
	}
	s := r.Summary
	w("</ul>")
	w("<h2>Summary</h2><ul>")
	w(fmt.Sprintf("<li>Findings: <b>%d</b> (verified %d, candidate %d, inconclusive %d, refuted %d)</li>",
		s["findings"], s["findings_verified"], s["findings_candidate"], s["findings_inconclusive"], s["findings_refuted"]))
	w(fmt.Sprintf("<li>Facts: %d</li>", s["facts"]))
	w(fmt.Sprintf("<li>Sub-goals: %d (%d open)</li>", s["subgoals_total"], s["subgoals_open"]))
	w("</ul>")
	w("<h2>Findings</h2>")
	if len(r.Findings) == 0 {
		w("<p><em>No findings recorded.</em></p>")
	}
	for _, f := range r.Findings {
		color := sevColor[f.Severity]
		if color == "" {
			color = "#555"
		}
		w("<div class='card'>")
		w(fmt.Sprintf("<h3><span class='sev' style='background:%s'>%s</span> %s<span class='status'>%s</span></h3>",
			color, h(strings.ToUpper(f.Severity)), h(f.Title), h(f.Status)))
		w("<p><b>Computed verdict:</b> " + h(blackboard.Verdict(f.Status)) + " — " + h(f.VerdictReason) + "</p>")
		if f.Target != "" {
			w("<p><b>Target:</b> " + h(f.Target) + "</p>")
		}
		if f.Category != "" {
			w("<p><b>Category:</b> " + h(f.Category) + "</p>")
		}
		if f.Evidence != "" {
			w("<p><b>Evidence:</b></p><pre>" + h(f.Evidence) + "</pre>")
		}
		if f.Reproduction != "" {
			w("<p><b>Reproduction:</b></p><pre>" + h(f.Reproduction) + "</pre>")
		}
		if f.Remediation != "" {
			w("<p><b>Remediation:</b> " + h(f.Remediation) + "</p>")
		}
		if len(f.EvidenceRefs) > 0 {
			var parts []string
			for _, rf := range f.EvidenceRefs {
				p := "<code>" + h(rf.Ref) + "</code> (" + h(rf.Role)
				if rf.Note != "" {
					p += ": " + h(rf.Note)
				}
				parts = append(parts, p+")")
			}
			w("<p><b>Evidence traffic:</b> " + strings.Join(parts, ", ") + "</p>")
		}
		if len(f.Verifications) > 0 {
			w("<p><b>Verification history:</b></p><ul>")
			for _, v := range f.Verifications {
				var vr []string
				for _, rf := range v.EvidenceRefs {
					vr = append(vr, rf.Ref)
				}
				head := "<em>" + h(v.Verdict) + "</em>"
				if v.Summary != "" {
					head += " — " + h(v.Summary)
				}
				if len(vr) > 0 {
					head += " (traffic: " + h(strings.Join(vr, ", ")) + ")"
				}
				body := ""
				if v.Evidence != "" {
					body = "<pre>" + h(v.Evidence) + "</pre>"
				}
				w("<li>" + head + body + "</li>")
			}
			w("</ul>")
		}
		w("</div>")
	}
	if len(r.Facts) > 0 {
		w("<h2>Established facts</h2><ul>")
		for _, x := range r.Facts {
			w("<li>" + h(x.Statement) + " — " + h(x.Verdict) + ": " + h(x.VerdictReason) + "</li>")
		}
		w("</ul>")
	}
	w("<h2>Evidence argument graph</h2><h3>Claims</h3><ul>")
	for _, claim := range r.EvidenceGraph.Claims {
		w("<li><code>" + h(claim.ID) + "</code> (" + h(claim.Kind) + "): " + h(claim.Text) + " — " + h(claim.Verdict) + ": " + h(claim.Reason) + "</li>")
	}
	w("</ul><h3>Verification attempts</h3><ul>")
	for _, attempt := range r.EvidenceGraph.Verifications {
		w("<li><code>" + h(attempt.ID) + "</code> → <code>" + h(attempt.FindingID) + "</code>: " + h(attempt.Outcome) + " — " + h(attempt.Summary) + "</li>")
	}
	w("</ul><h3>Edges</h3><ul>")
	for _, edge := range r.EvidenceGraph.Edges {
		line := "<code>" + h(edge.From) + "</code> —" + h(edge.Kind) + "→ <code>" + h(edge.To) + "</code>"
		if edge.Role != "" {
			line += " (" + h(edge.Role) + ")"
		}
		w("<li>" + line + "</li>")
	}
	w("</ul>")
	if len(r.Subgoals) > 0 {
		w("<h2>Sub-goals (coverage checklist)</h2><ul>")
		for _, g := range r.Subgoals {
			mark := "☐"
			if g.Status == "resolved" {
				mark = "☑"
			}
			tail := ""
			if g.Outcome != "" {
				tail = " — " + h(g.Outcome)
			}
			w("<li>" + mark + " " + h(g.Description) + tail + "</li>")
		}
		w("</ul>")
	}
	w("</body></html>")
	return b.String()
}
