package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/well-lzz/chronos/internal/engagement"
	"github.com/well-lzz/chronos/internal/scope"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProductionScopeSwitchStartsNewSandboxAndPreservesHistory(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		if _, err := exec.LookPath("bwrap"); err != nil {
			if os.Getenv("CHRONOS_REQUIRE_SANDBOX_TESTS") == "1" {
				t.Fatal(err)
			}
			t.Skip("OS sandbox backend not installed")
		}
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "chronos")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %.1000s", err, out)
	}
	config := filepath.Join(dir, "engagement.json")
	_ = os.WriteFile(config, []byte(`{"name":"isolation-test","goal":"test only; never contact external targets","scope":[]}`), 0600)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	state := filepath.Join(dir, "state")
	token := strings.Repeat("t", 43)
	log, err := os.Create(filepath.Join(dir, "parent.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd := exec.Command(binary, "--engagement", config, "--web", "--web-port", fmt.Sprint(port))
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "CHRONOS_STATE_DIR=" + state, "CHRONOS_CONSOLE_TOKEN=" + token, "CHRONOS_PLAYWRIGHT_MCP=false"}
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
		}
	}()
	client := &http.Client{Timeout: 20 * time.Second}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	request := func(method, path string, body any, out any) int {
		var input bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&input).Encode(body)
		}
		r, _ := http.NewRequest(method, base+path, &input)
		r.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			r.Header.Set("Content-Type", "application/json")
		}
		response, err := client.Do(r)
		if err != nil {
			return 0
		}
		defer response.Body.Close()
		if out != nil {
			_ = json.NewDecoder(response.Body).Decode(out)
		}
		return response.StatusCode
	}
	var snapshot map[string]any
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if request("GET", "/api/state", nil, &snapshot) == 200 {
			break
		}
		select {
		case err := <-done:
			data, _ := os.ReadFile(filepath.Join(dir, "parent.log"))
			t.Fatalf("startup: %v %.1500s", err, data)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	if snapshot == nil {
		data, _ := os.ReadFile(filepath.Join(dir, "parent.log"))
		t.Fatalf("not ready: %.1500s", data)
	}
	old := int(snapshot["isolation"].(map[string]any)["pid"].(float64))
	if old == cmd.Process.Pid {
		t.Fatal("mission remained in supervisor")
	}
	if status := request("POST", "/api/command", map[string]any{"action": "resume"}, nil); status != http.StatusConflict {
		t.Fatalf("empty scope must refuse resume: HTTP %d", status)
	}
	if status := request("POST", "/api/scope", map[string]any{"targets": []string{"127.0.0.2"}}, nil); status != 200 {
		t.Fatalf("scope switch: HTTP %d", status)
	}
	if request("GET", "/api/state", nil, &snapshot) != 200 {
		t.Fatal("new worker unavailable")
	}
	next := int(snapshot["isolation"].(map[string]any)["pid"].(float64))
	if next == old {
		t.Fatal("scope reused OS process")
	}
	if syscall.Kill(old, 0) == nil {
		t.Fatal("old worker survived scope change")
	}
	var history struct {
		Runs []map[string]any `json:"runs"`
	}
	if request("GET", "/api/runs", nil, &history) != 200 || len(history.Runs) != 2 {
		t.Fatalf("isolated history lost: %+v", history)
	}
	if request("POST", "/api/scope", map[string]any{"targets": []string{}}, nil) == 200 {
		t.Fatal("accepted invalid scope update")
	}
	if request("GET", "/api/state", nil, &snapshot) != 200 || int(snapshot["isolation"].(map[string]any)["pid"].(float64)) != next {
		t.Fatal("invalid scope disturbed active worker")
	}
	// A hard worker crash must leave the supervisor and its authorization
	// endpoint alive. Recovery is an explicit new run, never silent replay.
	if err := syscall.Kill(next, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if request("GET", "/api/state", nil, &snapshot) == 200 && snapshot["isolation"].(map[string]any)["state"] == "failed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if snapshot["isolation"].(map[string]any)["state"] != "failed" {
		t.Fatal("worker crash not surfaced")
	}
	if request("POST", "/api/scope", map[string]any{"targets": []string{"127.0.0.2"}}, nil) != 200 {
		t.Fatal("supervisor cannot start an explicit replacement after worker crash")
	}
	if request("GET", "/api/state", nil, &snapshot) != 200 || int(snapshot["isolation"].(map[string]any)["pid"].(float64)) == next {
		t.Fatal("crashed process reused")
	}
}

func TestIsolatedEmptyScopeCannotResume(t *testing.T) {
	s := &isolatedSource{}
	if err := s.CommandChecked("resume", "", "", ""); err == nil {
		t.Fatal("empty scope resumed")
	}
}

func TestIsolationUpgradePreservesOperatorScope(t *testing.T) {
	dir := t.TempDir()
	eng := &engagement.Engagement{Name: "n", Goal: "g", ScopeTargets: []string{"broader.test"}}
	if err := scope.SaveOperatorScopeRun(dir, "engagement\x00n\x00g", scope.Parse([]string{"authorized.test"}), ""); err != nil {
		t.Fatal(err)
	}
	targets, err := initialCapsuleTargets(dir, eng)
	if err != nil || len(targets) != 1 || targets[0] != "authorized.test" {
		t.Fatalf("operator scope lost on isolation upgrade: %v %v", targets, err)
	}
}
