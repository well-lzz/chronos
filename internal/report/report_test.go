package report

import (
	"os"
	"strings"
	"testing"

	"github.com/well-lzz/chronos/internal/blackboard"
)

func TestRenderEvidenceAndVerification(t *testing.T) {
	dir := t.TempDir()
	bb, err := blackboard.LoadOrNew("acme", dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := bb.AddFinding(blackboard.Finding{
		Title: "Reflected XSS in /search", Severity: "medium", Target: "staging.acme.test",
		Category: "web/injection", Evidence: "payload reflected unescaped",
		EvidenceRefs: []blackboard.EvidenceRef{{Ref: "t1", Role: "proof", Note: "reflected"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bb.VerifyFinding(id, blackboard.Inconclusive, "WAF blocked", "tried A/B, blocked", nil, "deepseek"); err != nil {
		t.Fatal(err)
	}
	if _, err := bb.VerifyFinding(id, blackboard.Verified, "fired in headless", "confirmed",
		[]blackboard.EvidenceRef{{Ref: "t1", Role: "verification"}}, "deepseek"); err != nil {
		t.Fatal(err)
	}
	if _, err := bb.AddFact("nginx 1.25.3 on staging.acme.test", "", "deepseek"); err != nil {
		t.Fatal(err)
	}
	g, err := bb.AddSubgoal("enumerate /api/*", "deepseek")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bb.ResolveSubgoal(g, "12 endpoints, 2 unauth"); err != nil {
		t.Fatal(err)
	}

	paths, err := RenderReports(bb, dir+"/r", RunMeta{Mode: "assessment", Scope: "staging.acme.test"})
	if err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(paths["md"])
	html, _ := os.ReadFile(paths["html"])
	for _, want := range []string{"Evidence traffic", "t1", "Verification history", "inconclusive", "Reflected XSS"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("md missing %q", want)
		}
	}
	if !strings.Contains(string(html), "Evidence traffic") || !strings.Contains(string(html), "Verification history") {
		t.Error("html missing evidence/verification sections")
	}
}
