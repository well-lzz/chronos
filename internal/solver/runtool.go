package solver

// runtool.go is the external security-tool runner (Layer 1 confinement). It runs
// an allowlisted binary (nmap, nuclei, httpx, ffuf) against exactly ONE in-scope
// target, building argv directly — never through a shell — so there is no shell
// injection. The target is scope-checked, and the extra args must be flags only:
// they may not carry another target or a target-list flag, so they cannot smuggle
// the tool out of scope. A true kernel egress jail (netns + nftables) is Layer 2,
// added at Linux packaging time; this layer is the portable guarantee.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/well-lzz/chronos/internal/scope"
)

type extToolSpec struct {
	positional bool   // target is a positional arg (appended last), e.g. nmap
	targetFlag string // target is passed via this flag, e.g. -u for httpx/nuclei/ffuf
}

// allowedExtTools is the binary allowlist. Only these may be executed.
var allowedExtTools = map[string]extToolSpec{
	"nmap":   {positional: true},
	"httpx":  {targetFlag: "-u"},
	"nuclei": {targetFlag: "-u"},
	"ffuf":   {targetFlag: "-u"},
}

func allowedToolNames() string {
	return "nmap, httpx, nuclei, ffuf"
}

// deniedArgFlags are flags that would read targets from a file or expand the
// target set, bypassing the single-target scope check.
var deniedArgFlags = map[string]bool{
	"-iL": true, "-iR": true, "--input-file": true,
	"-l": true, "-list": true, "--list": true,
	"-L": true, "--target-list": true,
}

const shellMeta = ";|&$`<>\n\r\t"

const toolMaxOutput = 24000

func clipOut(s string) string {
	if len(s) <= toolMaxOutput {
		return s
	}
	return s[:toolMaxOutput] + fmt.Sprintf("\n... [truncated, total %d chars]", len(s))
}

// targetHost extracts the host from a target that may be a URL, host:port, or
// bare host/IP. Returns "" when no host can be determined.
func targetHost(target string) string {
	t := strings.TrimSpace(target)
	if t == "" {
		return ""
	}
	if strings.Contains(t, "://") {
		if u, err := url.Parse(t); err == nil {
			return u.Hostname()
		}
		return ""
	}
	// host:port or bare host/IP
	if h, _, err := net.SplitHostPort(t); err == nil {
		return h
	}
	return t
}

// buildToolArgv validates the request against the allowlist and scope and
// returns the exact argv to execute, or a non-empty error message. Pure and
// testable — no process execution.
func buildToolArgv(tool, target string, args []string, sc scope.Scope) ([]string, string) {
	spec, ok := allowedExtTools[tool]
	if !ok {
		return nil, fmt.Sprintf("tool not allowed: %q (allowed: %s)", tool, allowedToolNames())
	}
	host := targetHost(target)
	if host == "" {
		return nil, "run_tool error: could not determine a host from target " + target
	}
	if sc.IsEmpty() {
		return nil, "BLOCKED (scope lock — NO TARGET KNOWN): the scope is empty; all egress is refused."
	}
	if !sc.Allows(host) {
		return nil, fmt.Sprintf("BLOCKED (scope lock): target host %q is not within the authorized scope (%s).", host, sc.Describe())
	}
	for _, a := range args {
		if strings.ContainsAny(a, shellMeta) {
			return nil, fmt.Sprintf("run_tool error: arg %q contains a disallowed character", a)
		}
		if deniedArgFlags[a] {
			return nil, fmt.Sprintf("run_tool error: flag %q is not allowed (it bypasses the single-target scope check)", a)
		}
		if strings.Contains(a, "://") {
			return nil, fmt.Sprintf("run_tool error: args must be flags only; %q looks like a target URL — pass the one target via 'target'", a)
		}
		if net.ParseIP(a) != nil {
			return nil, fmt.Sprintf("run_tool error: args must be flags only; %q looks like a target IP — pass the one target via 'target'", a)
		}
	}
	argv := []string{tool}
	if spec.positional {
		argv = append(argv, args...)
		argv = append(argv, target)
	} else {
		argv = append(argv, spec.targetFlag, target)
		argv = append(argv, args...)
	}
	return argv, ""
}

func (s *Solver) toolRunTool(ctx context.Context, argsJSON string) string {
	var a struct {
		Tool    string   `json:"tool"`
		Target  string   `json:"target"`
		Args    []string `json:"args"`
		Timeout int      `json:"timeout_seconds"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "run_tool error: bad args"
	}
	a.Tool = strings.TrimSpace(a.Tool)
	argv, errMsg := buildToolArgv(a.Tool, a.Target, a.Args, s.scope)
	if errMsg != "" {
		return errMsg
	}
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Sprintf("run_tool: %q is not installed on this host — install it or use http_request/port_scan/tls_inspect instead.", a.Tool)
	}

	to := time.Duration(a.Timeout) * time.Second
	if to <= 0 {
		to = 180 * time.Second
	}
	if to > 300*time.Second {
		to = 300 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, to)
	defer cancel()

	cmd := exec.CommandContext(runCtx, bin, argv[1:]...)
	out, runErr := cmd.CombinedOutput()
	display := string(out)
	if runCtx.Err() == context.DeadlineExceeded {
		display += fmt.Sprintf("\n[run_tool: timed out after %s — partial output above]", to)
	} else if runErr != nil && len(out) == 0 {
		display = fmt.Sprintf("run_tool: %s exited with error and no output — %v", a.Tool, runErr)
	}
	display = clipOut(strings.TrimSpace(display))
	if display == "" {
		display = "(no output)"
	}

	cmdline := strings.Join(argv, " ")
	header := fmt.Sprintf("$ %s\n", cmdline)
	tid := s.tl.RecordProbe("tool", a.Target, cmdline, header+display, s.ModelSpec)
	return header + display + fmt.Sprintf("\n[traffic %s — cite as evidence]", tid)
}
