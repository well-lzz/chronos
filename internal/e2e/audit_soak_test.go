package e2e

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/guard"
	"github.com/well-lzz/chronos/internal/llmrec"
	"github.com/well-lzz/chronos/internal/traffic"
)

// Audit an already completed run without contacting any model or target.
// Native recovery reads private copies so source evidence is never rewritten.
func TestAuditCompletedProcessSoak(t *testing.T) {
	root := os.Getenv("CHRONOS_AUDIT_SOAK_DIR")
	if root == "" {
		t.Skip("opt-in offline audit of completed soak")
	}
	var runner struct {
		Finished, Passed, Interrupted bool
		ExitCode                      int `json:"exit_code"`
	}
	var result struct {
		Finished, Passed bool
		Duration         float64 `json:"duration_seconds"`
		Required         float64 `json:"required_seconds"`
		Rounds           int     `json:"completed_rounds"`
		RequiredRounds   int     `json:"required_rounds"`
		Calls            int     `json:"provider_calls"`
		Tokens           int     `json:"tokens"`
		Requests         int     `json:"fixture_requests"`
		Approved         int     `json:"approved_http_calls"`
		Denied           int     `json:"denied_http_calls"`
		Binary           string  `json:"binary_sha256"`
	}
	readAuditJSON(t, filepath.Join(root, "runner.json"), &runner)
	readAuditJSON(t, filepath.Join(root, "evidence", "soak-result.json"), &result)
	if !runner.Finished || !runner.Passed || runner.Interrupted || runner.ExitCode != 0 || !result.Finished || !result.Passed || result.Duration < 7200 || result.Duration < result.Required || result.Rounds < 12 || result.Rounds < result.RequiredRounds || result.Denied != 0 {
		t.Fatal("run did not satisfy the complete two-hour acceptance conditions")
	}
	binary, err := os.ReadFile(filepath.Join(root, "chronos"))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(binary)) != result.Binary {
		t.Fatal("tested binary checksum mismatch")
	}
	roots, err := filepath.Glob(filepath.Join(root, "evidence", "state", "capsules", "*"))
	if err != nil {
		t.Fatal(err)
	}
	working, calls, tokens, ok, cancelled, approved, requests, findingsTotal := 0, 0, 0, 0, 0, 0, 0, 0
	attempts, cancelledHTTP, defaultGET := 0, 0, 0
	freshFindings, olderVerificationRefs := 0, 0
	targets := map[string]bool{}
	for _, capsule := range roots {
		usagePath := filepath.Join(capsule, "llm_usage.jsonl")
		forAuditLines(t, usagePath, func(line []byte) {
			var row llmrec.Usage
			if json.Unmarshal(line, &row) != nil {
				t.Fatal("invalid usage record")
			}
			calls++
			tokens += row.TotalTokens
			switch row.Status {
			case "ok":
				ok++
			case "cancelled":
				if row.Error != "context cancelled" {
					t.Fatal("unexpected cancellation")
				}
				cancelled++
			default:
				t.Fatal("unexpected provider error in completed soak")
			}
		})
		forAuditLines(t, filepath.Join(capsule, "tool_audit.jsonl"), func(line []byte) {
			var row guard.Audit
			if json.Unmarshal(line, &row) != nil {
				t.Fatal("invalid approval audit")
			}
			if row.Action == "approved" {
				if row.Tool != "http_request" {
					t.Fatal("unexpected approved network tool")
				}
				approved++
			}
		})
		paths, err := filepath.Glob(filepath.Join(capsule, "reports", "*", "runs", "*", "report.json"))
		if err != nil {
			t.Fatal(err)
		}
		if len(paths) != 1 {
			t.Fatalf("capsule has %d archived reports", len(paths))
		}
		var report struct {
			Mission   map[string]any       `json:"mission"`
			Findings  []blackboard.Finding `json:"findings"`
			Semantics string               `json:"verdict_semantics"`
		}
		readAuditJSON(t, paths[0], &report)
		id, _ := report.Mission["name"].(string)
		if id == "" || filepath.Base(id) != id {
			t.Fatal("invalid run identity")
		}
		copyRoot := t.TempDir()
		for _, rel := range []string{filepath.Join("blackboards", id+".json"), filepath.Join("traffic", id+".jsonl")} {
			data, err := os.ReadFile(filepath.Join(capsule, rel))
			if os.IsNotExist(err) && (strings.HasPrefix(rel, "traffic") || len(report.Findings) == 0) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(copyRoot, rel)
			if err = os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(dst, data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		bb, err := blackboard.LoadOrNew(id, copyRoot)
		if err != nil {
			t.Fatal(err)
		}
		tl, err := traffic.LoadOrNew(id, copyRoot)
		if err != nil {
			t.Fatal(err)
		}
		rows := tl.RecentRows(0)
		byID := map[string]map[string]any{}
		for _, row := range rows {
			ref, _ := row["id"].(string)
			target, _ := row["target"].(string)
			u, err := url.Parse(target)
			// The tool schema defines omitted method as GET. Earlier capture
			// records retain that empty value rather than canonicalizing it.
			method, _ := row["method"].(string)
			if method == "" {
				method = "GET"
				defaultGET++
			}
			if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Path != "/fixture" || u.RawQuery != "" || row["kind"] != "http" || method != "GET" {
				t.Fatal("traffic outside exact loopback fixture boundary")
			}
			targets[target] = true
			byID[ref] = row
			attempts++
			if row["status"] == 200 {
				requests++
			} else {
				exchange := tl.Get(ref, "response", 0, 8192)
				body, _ := exchange["response"].(string)
				if row["status"] != 0 || !strings.Contains(body, "context canceled") {
					t.Fatal("unexpected target request failure")
				}
				cancelledHTTP++
			}
		}
		findings := bb.FindingsSorted()
		if len(findings) != len(report.Findings) {
			t.Fatal("report and recovered blackboard disagree")
		}
		if len(findings) == 0 {
			continue
		}
		if report.Semantics == "" {
			t.Fatal("report omitted verdict limitations")
		}
		independent := false
		for _, finding := range findings {
			findingsTotal++
			original := map[string]bool{}
			for _, ref := range finding.EvidenceRefs {
				if byID[ref.Ref] == nil || byID[ref.Ref]["status"] != 200 {
					t.Fatal("finding cites missing traffic")
				}
				// Finding.EvidenceRefs is cumulative: VerifyFinding appends its
				// refs. Only pre-creation proof can be original discovery proof.
				if ref.Role == "proof" && byID[ref.Ref]["ts"].(float64) <= finding.TS {
					original[ref.Ref] = true
				}
			}
			if blackboard.Verdict(finding.Status) != "confirmed" {
				continue
			}
			freshFinding := false
			for _, v := range finding.Verifications {
				if v.By == "" || v.By == finding.FoundBy || v.Verdict != blackboard.Verified {
					continue
				}
				for _, ref := range v.EvidenceRefs {
					if ref.Role != "verification" {
						continue
					}
					row := byID[ref.Ref]
					if row == nil || row["status"] != 200 || row["by"] != v.By {
						t.Fatal("verification did not use fresh traffic from an independent executor")
					}
					// One attempt may include older contextual captures alongside
					// a fresh replay. Record them, but never count them as fresh.
					if original[ref.Ref] || row["ts"].(float64) < finding.TS {
						olderVerificationRefs++
						continue
					}
					exchange := tl.Get(ref.Ref, "response", 0, 8192)
					body, _ := exchange["response"].(string)
					if !strings.Contains(body, "CHRONOS_SYNTHETIC_PUBLIC_MARKER_ONLY") {
						t.Fatal("verification response missing fixture marker")
					}
					independent = true
					freshFinding = true
				}
			}
			if freshFinding {
				freshFindings++
			}
		}
		if !independent {
			t.Fatal("recovered capsule lacks independently grounded verification")
		}
		for _, ext := range []string{"md", "html"} {
			data, err := os.ReadFile(strings.TrimSuffix(paths[0], "json") + ext)
			if err != nil || len(data) == 0 {
				t.Fatal("readable report missing")
			}
		}
		working++
	}
	if working != result.Rounds || calls != result.Calls || tokens != result.Tokens || requests != result.Requests || approved != result.Approved || attempts != approved || len(targets) != 1 {
		t.Fatalf("evidence totals disagree: rounds=%d calls=%d tokens=%d requests=%d approvals=%d targets=%d", working, calls, tokens, requests, approved, len(targets))
	}
	if _, err := os.Stat(filepath.Join(root, "evidence", "private-models.json")); !os.IsNotExist(err) {
		t.Fatal("temporary credential file retained")
	}
	audit := map[string]any{"passed": true, "audited_at": time.Now().UTC(), "recovered_verified_capsules": working, "capsules_including_initial_paused_run": len(roots), "findings_in_recovered_capsules": findingsTotal, "findings_with_fresh_independent_verification": freshFindings, "older_context_refs_not_counted_as_fresh": olderVerificationRefs, "provider_calls": calls, "successful_responses": ok, "expected_cancellations": cancelled, "unexpected_provider_errors": 0, "reported_tokens": tokens, "captured_http_attempts": attempts, "cancelled_http_attempts": cancelledHTTP, "default_get_records": defaultGET, "fixture_requests": requests, "approved_http_calls": approved, "distinct_fixture_urls": len(targets), "temporary_credential_file_removed": true, "native_recovery": "Blackboard.LoadOrNew + traffic.LoadOrNew on private copies; source evidence unchanged", "binary_sha256": result.Binary}
	writeJSONFile(t, filepath.Join(root, "evidence", "postrun-audit.json"), audit)
	t.Logf("audit passed: %d recovered verified capsules, %d successful responses, %d expected cancellations, %d fixture requests", working, ok, cancelled, requests)
}

func readAuditJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, value); err != nil {
		t.Fatal(err)
	}
}
func forAuditLines(t *testing.T, path string, visit func([]byte)) {
	t.Helper()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		if len(scanner.Bytes()) > 0 {
			visit(scanner.Bytes())
		}
	}
	if err = scanner.Err(); err != nil {
		t.Fatal(err)
	}
}
