package solver

import (
	"strings"
	"testing"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/traffic"
)

func TestCandidateRoutesToIndependentPeerAndRequiresTraffic(t *testing.T) {
	dir := t.TempDir()
	bb, err := blackboard.LoadOrNew("verify", dir)
	if err != nil {
		t.Fatal(err)
	}
	tl, err := traffic.LoadOrNew("verify", dir)
	if err != nil {
		t.Fatal(err)
	}
	originalTraffic, err := tl.RecordHTTP("GET", "http://target.test/", nil, "", "HTTP 200\noriginal proof", "origin")
	if err != nil {
		t.Fatal(err)
	}
	creator := New(Options{ModelSpec: "origin", BB: bb, TL: tl,
		VerificationPeer: func(origin, findingID string) string { return "peer" }})
	result := creator.toolAddFinding(`{"title":"candidate issue","target":"http://target.test/","category":"web/test","evidence":"observed the response","reproduction":"1. GET http://target.test/\nExpected: HTTP 200 with the observed marker","evidence_refs":[{"ref":"` + originalTraffic + `","role":"proof"}],"verified":true}`)
	if !strings.Contains(result, "confirmed") || !strings.Contains(result, "assigned to peer") {
		t.Fatalf("finding bypassed independent verification: %s", result)
	}
	finding := bb.FindingsSorted()[0]
	goalID, assignee := bb.VerificationAssignment(finding.ID)
	if finding.Status != blackboard.Verified || goalID == "" || assignee != "peer" {
		t.Fatalf("verification goal missing: finding=%+v goal=%s assigned=%s", finding, goalID, assignee)
	}
	if result := creator.toolVerifyFinding(`{"finding_id":"` + finding.ID + `","verdict":"reproduced"}`); !strings.Contains(result, "assigned to peer") {
		t.Fatalf("discoverer was allowed to self-verify: %s", result)
	}
	peer := New(Options{ModelSpec: "peer", BB: bb, TL: tl})
	if result := peer.toolVerifyFinding(`{"finding_id":"` + finding.ID + `","verdict":"reproduced"}`); !strings.Contains(result, "evidence_ref") {
		t.Fatalf("peer verified without real traffic: %s", result)
	}
	if result := peer.toolVerifyFinding(`{"finding_id":"` + finding.ID + `","verdict":"reproduced","summary":"replayed","evidence":"actual replay observations","evidence_refs":[{"ref":"` + originalTraffic + `","role":"proof"}]}`); !strings.Contains(result, "role=verification") {
		t.Fatalf("peer reused original proof as independent verification: %s", result)
	}
	verificationTraffic, err := tl.RecordHTTP("GET", "http://target.test/", nil, "", "HTTP 200\nverification replay", "peer")
	if err != nil {
		t.Fatal(err)
	}
	if result := peer.toolVerifyFinding(`{"finding_id":"` + finding.ID + `","verdict":"reproduced","summary":"replayed","evidence_refs":[{"ref":"` + verificationTraffic + `","role":"verification"}]}`); !strings.Contains(result, "actual replay observations") {
		t.Fatalf("peer verified without replay observations: %s", result)
	}
	if result := peer.toolVerifyFinding(`{"finding_id":"` + finding.ID + `","verdict":"reproduced","summary":"replayed","evidence":"actual replay observations","evidence_refs":[{"ref":"` + verificationTraffic + `","role":"verification"}]}`); !strings.Contains(result, "confirmed") {
		t.Fatalf("peer verification failed: %s", result)
	}
	if got := bb.FindingsSorted()[0].Status; got != blackboard.Verified {
		t.Fatalf("finding status=%s", got)
	}
	if goals := bb.OpenSubgoals(); len(goals) != 0 {
		t.Fatalf("verification goal remains open: %+v", goals)
	}
	if result := creator.toolVerifyFinding(`{"finding_id":"` + finding.ID + `","verdict":"refuted","evidence_refs":[{"ref":"` + verificationTraffic + `"}]}`); !strings.Contains(result, "assigned to peer") {
		t.Fatalf("discoverer overturned independent verdict: %s", result)
	}
}

func TestFindingRequiresExecutableRunbookAndPreventsExactDuplicates(t *testing.T) {
	dir := t.TempDir()
	bb, err := blackboard.LoadOrNew("runbook", dir)
	if err != nil {
		t.Fatal(err)
	}
	tl, err := traffic.LoadOrNew("runbook", dir)
	if err != nil {
		t.Fatal(err)
	}
	trafficID, err := tl.RecordHTTP("GET", "http://target.test/search", nil, "", "HTTP 200\nproof", "origin")
	if err != nil {
		t.Fatal(err)
	}
	solver := New(Options{ModelSpec: "origin", BB: bb, TL: tl})
	valid := `{"title":"candidate issue","target":"http://target.test/search","category":"web/test","evidence":"observed","reproduction":"1. GET /search\nExpected: HTTP 200 with proof","evidence_refs":[{"ref":"` + trafficID + `","role":"proof"}]}`
	cases := map[string]string{
		"missing evidence":  strings.Replace(valid, `"evidence":"observed"`, `"evidence":""`, 1),
		"missing steps":     strings.Replace(valid, "1. GET", "GET", 1),
		"missing expected":  strings.Replace(valid, "Expected:", "Expect", 1),
		"missing traffic":   strings.Replace(valid, `"evidence_refs":[{"ref":"`+trafficID+`","role":"proof"}]`, `"evidence_refs":[]`, 1),
		"missing reproduce": strings.Replace(valid, `"reproduction":"1. GET /search\nExpected: HTTP 200 with proof"`, `"reproduction":""`, 1),
	}
	for name, args := range cases {
		if result := solver.toolAddFinding(args); !strings.Contains(result, "blocked") {
			t.Fatalf("%s was accepted: %s", name, result)
		}
	}
	if len(bb.FindingsSorted()) != 0 {
		t.Fatal("invalid findings reached the blackboard")
	}
	if result := solver.toolAddFinding(valid); !strings.Contains(result, "confirmed") {
		t.Fatalf("valid runbook was rejected: %s", result)
	}
	if result := solver.toolAddFinding(valid); !strings.Contains(result, "same title, target, and category") {
		t.Fatalf("exact duplicate was accepted: %s", result)
	}
	if len(bb.FindingsSorted()) != 1 {
		t.Fatal("duplicate finding reached the blackboard")
	}
}

func TestChallengeToolUsesCapturedTrafficAndUpdatesGraphVerdict(t *testing.T) {
	dir := t.TempDir()
	bb, err := blackboard.LoadOrNew("challenge-tool", dir)
	if err != nil {
		t.Fatal(err)
	}
	tl, err := traffic.LoadOrNew("challenge-tool", dir)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := tl.RecordHTTP("GET", "http://target.test/", nil, "", "HTTP 200\nclaimed proof", "origin")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := tl.RecordHTTP("GET", "http://target.test/", nil, "", "HTTP 403\ncontradictory replay", "peer")
	if err != nil {
		t.Fatal(err)
	}
	id, err := bb.AddFinding(blackboard.Finding{Title: "access granted", EvidenceRefs: []blackboard.EvidenceRef{{Ref: proof, Role: "proof"}}})
	if err != nil {
		t.Fatal(err)
	}
	peer := New(Options{ModelSpec: "peer", BB: bb, TL: tl})
	result := peer.toolChallengeFinding(`{"target_id":"` + id + `","reason":"replay was denied","evidence_refs":[{"ref":"missing","role":"proof"}]}`)
	if !strings.Contains(result, "dropped refs") {
		t.Fatalf("missing traffic was accepted: %s", result)
	}
	if verdict, _, _ := bb.ClaimVerdict(id); verdict != "suspected" {
		t.Fatalf("unresolved challenge verdict = %s", verdict)
	}
	result = peer.toolChallengeFinding(`{"target_id":"` + id + `","reason":"replay was denied","evidence_refs":[{"ref":"` + replay + `","role":"verification"}]}`)
	if !strings.Contains(result, "is refuted") {
		t.Fatalf("captured replay did not defeat claim: %s", result)
	}
}
