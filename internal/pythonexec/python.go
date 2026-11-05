// Package pythonexec runs offline Python in an operating-system sandbox.
// It never falls back to an unsandboxed host interpreter.
package pythonexec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

type interpreter struct {
	Executable string `json:"executable"`
	Prefix     string `json:"prefix"`
	Stdlib     string `json:"stdlib"`
}

type boundedOutput struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	truncated bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	room := 24000 - b.buffer.Len()
	if room < len(p) {
		b.truncated = true
		p = p[:room]
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}

func Backend() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return exec.LookPath("sandbox-exec")
	case "linux":
		return exec.LookPath("bwrap")
	default:
		return "", fmt.Errorf("Python OS sandbox unsupported on %s", runtime.GOOS)
	}
}

func Run(ctx context.Context, python, code string) (string, error) {
	if len(code) > 64*1024 {
		return "", fmt.Errorf("Python code exceeds 64 KiB")
	}
	backend, err := Backend()
	if err != nil {
		return "", fmt.Errorf("Python sandbox unavailable: %w", err)
	}
	dir, err := os.MkdirTemp("", "chronos-python-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	env := []string{"HOME=" + dir, "TMPDIR=" + dir, "LANG=C", "PATH=/usr/bin:/bin"}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	probe := exec.CommandContext(probeCtx, python, "-I", "-S", "-c", `import json,sys,sysconfig; print(json.dumps(dict(executable=sys.executable,prefix=sys.base_prefix,stdlib=sysconfig.get_path('stdlib'))))`)
	probe.Env = env
	probe.Dir = dir
	data, err := probe.Output()
	cancel()
	if err != nil {
		return "", fmt.Errorf("inspect Python runtime: %w", err)
	}
	var py interpreter
	if err = json.Unmarshal(data, &py); err != nil {
		return "", err
	}
	for _, path := range []*string{&py.Executable, &py.Prefix, &py.Stdlib} {
		*path, err = filepath.EvalSymlinks(*path)
		if err != nil || !filepath.IsAbs(*path) {
			return "", fmt.Errorf("invalid Python runtime path")
		}
	}
	if err = os.WriteFile(filepath.Join(dir, "user.py"), []byte(code), 0600); err != nil {
		return "", err
	}
	bootstrap := `import resource, runpy, sys
for kind, limit in [(resource.RLIMIT_CPU, 10), (resource.RLIMIT_FSIZE, 8388608), (resource.RLIMIT_NOFILE, 64)]:
    hard = resource.getrlimit(kind)[1]
    limit = limit if hard == resource.RLIM_INFINITY else min(limit, hard)
    resource.setrlimit(kind, (limit, limit))
if sys.platform.startswith('linux'):
    resource.setrlimit(resource.RLIMIT_DATA, (268435456, 268435456))
    resource.setrlimit(resource.RLIMIT_AS, (536870912, 536870912))
runpy.run_path('user.py', run_name='__main__')
`
	if err = os.WriteFile(filepath.Join(dir, "bootstrap.py"), []byte(bootstrap), 0600); err != nil {
		return "", err
	}
	cmd, err := sandboxCommand(ctx, backend, py, dir)
	if err != nil {
		return "", err
	}
	cmd.Env = env
	cmd.Dir = dir
	cmd.WaitDelay = 2 * time.Second
	configureProcess(cmd)
	var output boundedOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	err = cmd.Run()
	cleanupProcess(cmd)
	text := output.buffer.String()
	if output.truncated {
		text += "\n... [output truncated at 24000 bytes]"
	}
	if ctx.Err() != nil {
		return text, ctx.Err()
	}
	if err != nil {
		return text, fmt.Errorf("sandboxed Python: %w", err)
	}
	if text == "" {
		text = "(no output)"
	}
	return text, nil
}
