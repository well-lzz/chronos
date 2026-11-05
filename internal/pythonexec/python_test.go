package pythonexec

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func sandboxPython(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		if os.Getenv("CHRONOS_REQUIRE_SANDBOX_TESTS") == "1" {
			t.Fatal("required Python runtime unavailable")
		}
		t.Skip("Python not installed")
	}
	if _, err := Backend(); err != nil {
		if os.Getenv("CHRONOS_REQUIRE_SANDBOX_TESTS") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	output, err := Run(context.Background(), python, "print(6 * 7)")
	if err != nil {
		if runtime.GOOS == "linux" && (strings.Contains(output, "Operation not permitted") || strings.Contains(output, "No permissions to create")) {
			if os.Getenv("CHRONOS_REQUIRE_SANDBOX_TESTS") == "1" {
				t.Fatalf("required kernel sandbox cannot start: %s", output)
			}
			t.Skip("host disallows bubblewrap namespaces: " + output)
		}
		t.Fatalf("sandbox startup: %s %v", output, err)
	}
	if strings.TrimSpace(output) != "42" {
		t.Fatal(output)
	}
	return python
}

func TestKernelSandboxDeniesHostCredentialsFilesAndNetwork(t *testing.T) {
	python := sandboxPython(t)
	dir := t.TempDir()
	canary := filepath.Join(dir, "credential.txt")
	writePath := filepath.Join(dir, "outside-write.txt")
	if err := os.WriteFile(canary, []byte("private-canary"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHRONOS_SANDBOX_SECRET", "private-environment-canary")
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); w.WriteHeader(204) }))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	_, port, _ := net.SplitHostPort(u.Host)
	portNumber, _ := strconv.Atoi(port)
	readJSON, _ := json.Marshal(canary)
	writeJSON, _ := json.Marshal(writePath)
	code := fmt.Sprintf(`import os, socket, json, ctypes
results = {'secret': os.getenv('CHRONOS_SANDBOX_SECRET')}
try:
    open(%s).read()
    results['read'] = 'ESCAPED'
except OSError:
    results['read'] = 'denied'
try:
    open(%s, 'w').write('ESCAPED')
    results['write'] = 'ESCAPED'
except OSError:
    results['write'] = 'denied'
try:
    conn = socket.create_connection(('127.0.0.1', %d), timeout=0.5)
    conn.sendall(b'GET / HTTP/1.0\r\n\r\n')
    results['network'] = 'ESCAPED'
    conn.close()
except OSError:
    results['network'] = 'denied'
libc = ctypes.CDLL(None)
fd = libc.open(%s.encode(), 0)
results['raw_file'] = 'denied' if fd == -1 else 'ESCAPED'
if fd >= 0: os.close(fd)
open('inside.txt', 'w').write('allowed')
results['inside'] = open('inside.txt').read()
print(json.dumps(results))
`, readJSON, writeJSON, portNumber, readJSON)
	output, err := Run(context.Background(), python, code)
	if err != nil {
		t.Fatalf("sandbox test: %s %v", output, err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(output, err)
	}
	if result["secret"] != nil || result["read"] != "denied" || result["write"] != "denied" || result["network"] != "denied" || result["raw_file"] != "denied" || result["inside"] != "allowed" {
		t.Fatalf("sandbox escape: %s", output)
	}
	if hits.Load() != 0 {
		t.Fatal("host network reached")
	}
	if _, err := os.Stat(writePath); !os.IsNotExist(err) {
		t.Fatal("host file was created")
	}
}

func TestSandboxBoundsOutputAndCancelsExecution(t *testing.T) {
	python := sandboxPython(t)
	output, err := Run(context.Background(), python, `print('x' * 100000)`)
	if err != nil || len(output) > 24100 || !strings.Contains(output, "output truncated") {
		t.Fatalf("unbounded output: %d %v", len(output), err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = Run(ctx, python, `while True: pass`)
	if err == nil || ctx.Err() == nil || time.Since(started) > 3*time.Second {
		t.Fatalf("execution did not cancel: %v", err)
	}
}

func TestMissingSandboxNeverFallsBackToHost(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := Run(context.Background(), "/usr/bin/python3", `print('must not run')`); err == nil {
		t.Fatal("unsandboxed fallback")
	}
}
