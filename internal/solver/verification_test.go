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
	creator := New(Options{ModelSpec: "origin", BB: bb, TL: tl,
		VerificationPeer: func(origin, findingID string) string { return "peer" }})
	result := creator.toolAddFinding(`{"title":"candidate issue","verified":true}`)
	if !strings.Contains(result, "candidate") || !strings.Contains(result, "assigned to peer") {
		t.Fatalf("finding bypassed independent verification: %s", result)
	}
	finding := bb.FindingsSorted()[0]
	goalID, assignee := bb.VerificationAssignment(finding.ID)
	if finding.Status != blackboard.Candidate || goalID == "" || assignee != "peer" {
		t.Fatalf("verification goal missing: finding=%+v goal=%s assigned=%s", finding, goalID, assignee)
	}
	if result := creator.toolVerifyFinding(`{"finding_id":"` + finding.ID + `","verdict":"reproduced"}`); !strings.Contains(result, "assigned to peer") {
		t.Fatalf("discoverer was allowed to self-verify: %s", result)
	}
	peer := New(Options{ModelSpec: "peer", BB: bb, TL: tl})
	if result := peer.toolVerifyFinding(`{"finding_id":"` + finding.ID + `","verdict":"reproduced"}`); !strings.Contains(result, "evidence_ref") {
		t.Fatalf("peer verified without real traffic: %s", result)
	}
	trafficID, err := tl.RecordHTTP("GET", "http://target.test/", nil, "", "HTTP 200\nproof", "peer")
	if err != nil {
		t.Fatal(err)
	}
	if result := peer.toolVerifyFinding(`{"finding_id":"` + finding.ID + `","verdict":"reproduced","evidence_refs":[{"ref":"` + trafficID + `","role":"verification"}]}`); !strings.Contains(result, "verified") {
		t.Fatalf("peer verification failed: %s", result)
	}
	if got := bb.FindingsSorted()[0].Status; got != blackboard.Verified {
		t.Fatalf("finding status=%s", got)
	}
	if goals := bb.OpenSubgoals(); len(goals) != 0 {
		t.Fatalf("verification goal remains open: %+v", goals)
	}
	if result := creator.toolVerifyFinding(`{"finding_id":"` + finding.ID + `","verdict":"refuted","evidence_refs":[{"ref":"` + trafficID + `"}]}`); !strings.Contains(result, "assigned to peer") {
		t.Fatalf("discoverer overturned independent verdict: %s", result)
	}
}
