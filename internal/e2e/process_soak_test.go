package e2e

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/durable"
	"github.com/well-lzz/chronos/internal/guard"
	"github.com/well-lzz/chronos/internal/llmconfig"
	"github.com/well-lzz/chronos/internal/llmrec"
)

// This is a wall-clock soak of the production supervisor, its real sandboxed
// mission children, authenticated API, approvals, scope rotation and reports.
// It uses a periodic workload, not continuous model saturation. No mock LLM.
func TestProductionProcessSoak(t *testing.T) {
	if os.Getenv("CHRONOS_RUN_PROCESS_SOAK") != "1" {
		t.Skip("opt-in real-provider process soak")
	}
	duration := time.Duration(integer(t, "CHRONOS_SOAK_SECONDS", 7200, 10800)) * time.Second
	interval := time.Duration(integer(t, "CHRONOS_SOAK_INTERVAL_SECONDS", 600, 1800)) * time.Second
	binary, err := filepath.Abs(os.Getenv("CHRONOS_SOAK_BINARY"))
	if err != nil {
		t.Fatal(err)
	}
	profile := llmconfig.Load(os.Getenv("CHRONOS_LIVE_PROFILE_DIR")).Get()
	if !profile.Active() {
		t.Fatal("live profile unavailable")
	}
	root := os.Getenv("CHRONOS_LIVE_OUTPUT")
	if root == "" {
		t.Fatal("explicit private output directory required")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) > 0 {
		t.Fatal("soak output must be empty")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	binaryData, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	binaryHash := fmt.Sprintf("%x", sha256.Sum256(binaryData))
	plannedRounds := int((duration + interval - 1) / interval)
	end := started.Add(duration)
	var hits atomic.Int64
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != "GET" || r.URL.Path != "/fixture" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, "CHRONOS_SYNTHETIC_PUBLIC_MARKER_ONLY")
	}))
	defer fixture.Close()
	target := fixture.URL + "/fixture"
	state := filepath.Join(root, "state")
	_ = os.MkdirAll(state, 0700)
	models := []llmconfig.ModelEntry{}
	for _, name := range []string{"discovery", "independent-verifier"} {
		models = append(models, llmconfig.ModelEntry{Name: name, Provider: profile.Provider, Model: profile.Model, BaseURL: profile.BaseURL, APIKey: profile.APIKey, ContextWindowTokens: profile.ContextWindowTokens})
	}
	modelsPath := filepath.Join(root, "private-models.json")
	writeJSONFile(t, modelsPath, map[string]any{"models": models})
	defer os.Remove(modelsPath)
	rules := []guard.Rule{{Tool: "http_request", Action: "ask", Priority: 10, Reason: "fixture URL must be checked by the soak supervisor"}}
	for _, tool := range []string{"artifact_get", "traffic_get", "traffic_search", "add_fact", "add_finding", "verify_finding", "add_subgoal", "resolve_subgoal", "notify"} {
		rules = append(rules, guard.Rule{Tool: tool, Action: "allow", Priority: 5})
	}
	rules = append(rules, guard.Rule{Tool: "*", Action: "deny", Reason: "soak fixture has no authorization for this tool"})
	rulesPath := filepath.Join(root, "rules.json")
	writeJSONFile(t, rulesPath, map[string]any{"rules": rules})
	engPath := filepath.Join(root, "engagement.json")
	writeJSONFile(t, engPath, map[string]any{"name": "production-soak", "time_budget_seconds": 180, "goal": "AUTHORIZED SYNTHETIC TEST. The ONLY network action authorized is GET " + target + ". The body is a deliberately public fixture marker, not a real security vulnerability. Collaborate to record one info finding titled Synthetic public marker with actual proof traffic and numbered reproduction plus Expected:. A different executor must replay and verify it with NEW verification traffic. Inspect the shared blackboard to avoid duplicate findings. Do not investigate other paths or targets. When the finding has independent verification, resolve its verification goal and finish.", "scope": []string{"127.0.0.1"}})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	token := strings.Repeat("s", 32) + strconv.FormatInt(started.UnixNano(), 16)
	log, err := os.OpenFile(filepath.Join(root, "supervisor.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd := exec.Command(binary, "--engagement", engPath, "--models-file", modelsPath, "--web", "--web-port", strconv.Itoa(port))
	cmd.Dir = root
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "CHRONOS_STATE_DIR=" + state, "CHRONOS_CONSOLE_TOKEN=" + token, "CHRONOS_PLAYWRIGHT_MCP=false", "CHRONOS_TOOL_RULES_FILE=" + rulesPath, "MAX_BUMPS=1", "MAX_HANDOFFS=0", "ENGAGEMENT_QUIET_CYCLES=1", "MODEL_CALL_TIMEOUT_SECONDS=45", "MAX_RUN_TOOL_STEPS=64", "LLM_RECORD=false"}
	cmd.Stdout = log
	for _, key := range []string{"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY"} {
		if value := os.Getenv(key); value != "" {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var exitErr error
	go func() { exitErr = cmd.Wait(); close(done) }()
	var stopOnce sync.Once
	stopParent := func() {
		stopOnce.Do(func() {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				_ = cmd.Process.Kill()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("supervisor did not exit after SIGKILL")
				}
			}
		})
	}
	defer stopParent()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 20 * time.Second}
	defer client.CloseIdleConnections()
	api := func(method, path string, body, out any) error {
		var buffer bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buffer).Encode(body)
		}
		req, _ := http.NewRequest(method, base+path, &buffer)
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("supervisor HTTP unavailable")
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return fmt.Errorf("supervisor API HTTP %d", res.StatusCode)
		}
		if out == nil {
			return nil
		}
		return json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(out)
	}
	completed := 0
	var tokens, calls, errorsCount int64
	var maxSupervisorRSS, maxWorkerRSS int64
	workerIDs := map[int]bool{}
	approved, denied := 0, 0
	writeProgress := func(final bool) {
		elapsed := time.Since(started)
		result := map[string]any{"started": started.UTC(), "updated": time.Now().UTC(), "duration_seconds": elapsed.Seconds(), "required_seconds": duration.Seconds(), "workload": "periodic real-model missions, new OS sandbox per scope, persistent supervisor; idle intervals are included", "binary_sha256": binaryHash, "required_rounds": plannedRounds, "completed_rounds": completed, "unique_worker_processes": len(workerIDs), "provider_calls": calls, "provider_errors": errorsCount, "tokens": tokens, "fixture_requests": hits.Load(), "approved_http_calls": approved, "denied_http_calls": denied, "max_supervisor_rss_kib": maxSupervisorRSS, "max_worker_rss_kib": maxWorkerRSS, "finished": final, "passed": final && !t.Failed() && elapsed >= duration && completed >= plannedRounds, "model": profile.Model, "supervisor_pid": cmd.Process.Pid}
		data, _ := json.MarshalIndent(result, "", "  ")
		if err := durable.WriteFile(filepath.Join(root, "soak-result.json"), data); err != nil {
			t.Error(err)
		}
	}
	defer func() { stopParent(); readSoakUsage(state, &tokens, &calls, &errorsCount); writeProgress(true) }()
	ready := time.Now().Add(25 * time.Second)
	var snapshot map[string]any
	for api("GET", "/api/state", nil, &snapshot) != nil {
		select {
		case <-done:
			t.Fatalf("production supervisor exited before readiness: %v; inspect private supervisor.log", exitErr)
		default:
		}
		if time.Now().After(ready) {
			t.Fatal("production supervisor readiness timeout")
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = os.Remove(modelsPath) // Parent has loaded it; no key file is retained.
	for round := 1; time.Now().Before(end) || round == 1; round++ {
		if tokens >= 2_000_000 || calls >= 600 {
			t.Fatal("soak provider budget exhausted before required duration")
		}
		targetScope := "127.0.0.1/32"
		if round%2 == 0 {
			targetScope = "127.0.0.1"
		}
		oldPID := snapshotPID(snapshot)
		if err := api("POST", "/api/scope", map[string]any{"targets": []string{targetScope}}, nil); err != nil {
			t.Fatal(err)
		}
		if err := api("GET", "/api/state", nil, &snapshot); err != nil {
			t.Fatal(err)
		}
		pid := snapshotPID(snapshot)
		capsuleRoot, _ := snapshot["isolation"].(map[string]any)["root"].(string)
		if pid == 0 || pid == oldPID || pid == cmd.Process.Pid {
			t.Fatal("scope did not create a separate mission process")
		}
		workerIDs[pid] = true
		if syscall.Kill(oldPID, 0) == nil {
			t.Fatal("prior scope process survived transition")
		}
		if err := api("POST", "/api/command", map[string]string{"action": "resume"}, nil); err != nil {
			t.Fatal(err)
		}
		roundEnd := time.Now().Add(3 * time.Minute)
		stopping := false
		for {
			var pending struct {
				Pending []guard.Pending `json:"pending"`
			}
			if err := api("GET", "/api/approvals", nil, &pending); err == nil {
				for _, p := range pending.Pending {
					var a struct{ URL, Method string }
					valid := json.Unmarshal([]byte(p.Arguments), &a) == nil && p.Tool == "http_request" && a.URL == target && (a.Method == "" || a.Method == "GET")
					if valid {
						approved++
					} else {
						denied++
					}
					if err := api("POST", "/api/approvals/decision", map[string]any{"id": p.ID, "approve": valid}, nil); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := api("GET", "/api/state", nil, &snapshot); err != nil {
				t.Fatal(err)
			}
			iso, _ := snapshot["isolation"].(map[string]any)
			status, _ := iso["state"].(string)
			if status == "failed" {
				t.Fatal("production mission failed; inspect capsule worker.log and usage records")
			}
			if status == "completed" {
				break
			}
			// The acceptance criterion is fresh independent evidence. Once met,
			// exercise operator cancellation and wait for durable reports/exit;
			// do not spend tokens asking a completed model to manufacture work.
			if !stopping && snapshotIndependent(snapshot) {
				if err := api("POST", "/api/command", map[string]string{"action": "kill"}, nil); err != nil {
					t.Fatal(err)
				}
				stopping = true
			}
			if time.Now().After(roundEnd) {
				t.Fatal("production round exceeded three minutes")
			}
			if rss := residentKiB(cmd.Process.Pid); rss > maxSupervisorRSS {
				maxSupervisorRSS = rss
			}
			if rss := residentKiB(pid); rss > maxWorkerRSS {
				maxWorkerRSS = rss
			}
			if maxSupervisorRSS > 1<<20 || maxWorkerRSS > 1<<20 {
				t.Fatal("process RSS exceeded 1 GiB observation threshold")
			}
			time.Sleep(500 * time.Millisecond)
		}
		if !hasIndependentReport(capsuleRoot) {
			t.Fatal("production reports lack independently replayed evidence")
		}
		completed++
		readSoakUsage(state, &tokens, &calls, &errorsCount)
		writeProgress(false)
		t.Logf("round %d passed; calls=%d tokens=%d elapsed=%.1fs", round, calls, tokens, time.Since(started).Seconds())
		// Continue observing the same supervisor through the idle interval.
		until := started.Add(time.Duration(round) * interval)
		if until.After(end) {
			until = end
		}
		for time.Now().Before(until) {
			select {
			case <-done:
				t.Fatalf("supervisor exited during soak: %v", exitErr)
			default:
			}
			if rss := residentKiB(cmd.Process.Pid); rss > maxSupervisorRSS {
				maxSupervisorRSS = rss
			}
			if err := api("GET", "/api/state", nil, &snapshot); err != nil {
				t.Fatal(err)
			}
			writeProgress(false)
			wait := time.Until(until)
			if wait > 5*time.Second {
				wait = 5 * time.Second
			}
			time.Sleep(wait)
		}
	}
	if denied > 0 {
		t.Fatal("model proposed network actions outside the exact fixture boundary; denied and recorded")
	}
	if completed < plannedRounds {
		t.Errorf("completed %d of %d required workload rounds", completed, plannedRounds)
	}
}

func snapshotIndependent(snapshot map[string]any) bool {
	data, _ := json.Marshal(snapshot["findings"])
	var findings []struct {
		FoundBy       string                    `json:"found_by"`
		Verifications []blackboard.Verification `json:"verifications"`
	}
	if json.Unmarshal(data, &findings) != nil {
		return false
	}
	for _, f := range findings {
		for _, v := range f.Verifications {
			if v.By != "" && v.By != f.FoundBy && v.Verdict == blackboard.Verified {
				return true
			}
		}
	}
	return false
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := durable.WriteFile(path, data); err != nil {
		t.Fatal(err)
	}
}
func snapshotPID(snapshot map[string]any) int {
	iso, _ := snapshot["isolation"].(map[string]any)
	pid, _ := iso["pid"].(float64)
	return int(pid)
}
func residentKiB(pid int) int64 {
	data, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	return n
}
func hasIndependentReport(state string) bool {
	paths, _ := filepath.Glob(filepath.Join(state, "reports", "*", "runs", "*", "report.json"))
	for _, path := range paths {
		data, _ := os.ReadFile(path)
		var r struct {
			Findings []blackboard.Finding `json:"findings"`
		}
		if json.Unmarshal(data, &r) != nil {
			continue
		}
		for _, f := range r.Findings {
			if blackboard.Verdict(f.Status) != "confirmed" {
				continue
			}
			for _, v := range f.Verifications {
				if v.By != "" && v.By != f.FoundBy && v.Verdict == blackboard.Verified {
					return true
				}
			}
		}
	}
	return false
}
func readSoakUsage(state string, tokens, calls, errorsCount *int64) {
	*tokens, *calls, *errorsCount = 0, 0, 0
	paths, _ := filepath.Glob(filepath.Join(state, "capsules", "*", "llm_usage.jsonl"))
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			var row llmrec.Usage
			if json.Unmarshal(scanner.Bytes(), &row) == nil {
				*tokens += int64(row.TotalTokens)
				*calls++
				if row.Status != "ok" {
					*errorsCount++
				}
			}
		}
		f.Close()
	}
}
