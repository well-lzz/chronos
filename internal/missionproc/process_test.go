package missionproc

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCapsuleHelper(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-1] != "capsule-helper" {
		return
	}
	var input struct {
		Root, Canary, Other string
		Nonce               string
		Hang                bool
	}
	if err := DecodeBootstrap(&input); err != nil {
		os.Exit(11)
	}
	result := map[string]any{"pid": os.Getpid(), "nonce": input.Nonce, "secret": os.Getenv("CHRONOS_HOST_SECRET")}
	_, err := os.ReadFile(input.Canary)
	result["host_read_denied"] = err != nil
	err = os.WriteFile(input.Other, []byte("escaped"), 0600)
	result["host_write_denied"] = err != nil
	err = os.WriteFile(filepath.Join(input.Root, "private.txt"), []byte("owned"), 0600)
	result["private_write"] = err == nil
	listener, err := net.Listen("unix", "probe.sock")
	result["unix_control"] = err == nil
	if err != nil {
		result["unix_error"] = err.Error()
	}
	if listener != nil {
		defer listener.Close()
	}
	data, _ := json.Marshal(result)
	_ = os.WriteFile(filepath.Join(input.Root, "result.json"), data, 0600)
	if input.Hang {
		for {
			time.Sleep(time.Second)
		}
	}
	os.Exit(0)
}

func TestWholeMissionKernelIsolationAndCancellation(t *testing.T) {
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("bwrap"); err != nil {
			if os.Getenv("CHRONOS_REQUIRE_SANDBOX_TESTS") == "1" {
				t.Fatal(err)
			}
			t.Skip("bubblewrap not installed")
		}
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "own")
	canary := filepath.Join(parent, "supervisor-secret")
	other := filepath.Join(parent, "sibling-state")
	if err := os.WriteFile(canary, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHRONOS_HOST_SECRET", "must-not-inherit")
	for _, hang := range []bool{false, true} {
		nonce := strconv.FormatInt(time.Now().UnixNano(), 16)
		p, err := Start(binary, root, []string{"-test.run=^TestCapsuleHelper$", "--", "capsule-helper"}, map[string]any{"Root": root, "Canary": canary, "Other": other, "Hang": hang, "Nonce": nonce})
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		var result map[string]any
		for time.Now().Before(deadline) {
			data, _ := os.ReadFile(filepath.Join(root, "result.json"))
			if json.Unmarshal(data, &result) == nil && result["nonce"] == nonce {
				break
			}
			select {
			case <-p.Done():
				log, _ := os.ReadFile(filepath.Join(root, "worker.log"))
				t.Fatalf("worker failed: %v: %.1000s", p.Err(), log)
			default:
			}
			time.Sleep(10 * time.Millisecond)
		}
		// PID 1 inside Linux's private namespace differs from bwrap's host PID.
		// Writes into the private /tmp view need not error, but must never alter
		// the corresponding host path (checked below).
		if result == nil || result["nonce"] != nonce || result["host_read_denied"] != true || (runtime.GOOS == "darwin" && result["host_write_denied"] != true) || result["private_write"] != true || result["unix_control"] != true || result["secret"] != "" {
			t.Fatalf("sandbox failure: %+v", result)
		}
		if hang {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err = p.Stop(ctx)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
		} else {
			select {
			case <-p.Done():
				if err := p.Err(); err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not finish")
			}
		}
		_ = os.Remove(filepath.Join(root, "result.json"))
		_ = os.Remove(filepath.Join(root, "probe.sock"))
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatal("sibling modified")
	}
}

func TestNoMissionSandboxNeverFallsBack(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Start("/bin/echo", t.TempDir(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "sandbox unavailable") {
		t.Fatalf("unsafe fallback: %v", err)
	}
}
