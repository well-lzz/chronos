package solver

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var pythonPath string
var pythonOnce sync.Once

func pythonAvailable() bool {
	pythonOnce.Do(func() {
		if p, err := exec.LookPath("python3"); err == nil {
			pythonPath = p
		}
	})
	return pythonPath != ""
}

func decodePayload(payload, payloadHex string) ([]byte, error) {
	if payloadHex != "" {
		return hex.DecodeString(strings.Join(strings.Fields(payloadHex), ""))
	}
	return []byte(payload), nil
}

// guardPreamble installs a scope lock on the snippet's sockets. All handles and
// the allow-set are CLOSURE-LOCAL (installed inside a function), so model code
// runs in a clean namespace with no guard internals reachable — unlike a flat
// preamble whose _orig_connect/_guard leak as module globals. This is a
// drift-prevention guard, not an adversarial sandbox: determined code can still
// reach raw handles via ctypes; real isolation needs an OS boundary.
func guardPreamble(hosts, cidrs []string) string {
	hj, _ := json.Marshal(hosts)
	cj, _ := json.Marshal(cidrs)
	return fmt.Sprintf(`
def _chronos_target_lock():
    import socket as _s, ipaddress as _ip
    _hosts = frozenset(%s)
    _nets = tuple(_ip.ip_network(c) for c in %s)
    def _ok(host):
        if host in _hosts:
            return True
        if _nets:
            try:
                a = _ip.ip_address(host)
            except ValueError:
                return False
            return any(a in n for n in _nets)
        return False
    def _guard(address):
        host = str(address[0] if isinstance(address, tuple) else address).strip('[]').lower()
        if host and not _ok(host):
            raise PermissionError("scope lock: '%%s' is out of the authorized scope" %% host)
    _oc = _s.socket.connect
    def _connect(self, address):
        _guard(address); return _oc(self, address)
    _s.socket.connect = _connect
    _oce = _s.create_connection
    def _create_connection(address, *a, **k):
        _guard(address); return _oce(address, *a, **k)
    _s.create_connection = _create_connection
_chronos_target_lock()
del _chronos_target_lock
`, string(hj), string(cj))
}

func (s *Solver) toolRunPython(ctx context.Context, argsJSON string) string {
	if !s.allowExec {
		return "run_python is disabled for this run."
	}
	if !pythonAvailable() {
		return "run_python unavailable: python3 is not installed on this host."
	}
	var a struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil || strings.TrimSpace(a.Code) == "" {
		return "run_python error: empty or invalid code"
	}
	currentScope := s.scopeSnapshot()
	code := guardPreamble(currentScope.Hosts(), currentScope.CIDRStrings()) + "\n" + a.Code

	f, err := os.CreateTemp("", "chronos_*.py")
	if err != nil {
		return "run_python error: " + err.Error()
	}
	defer os.Remove(f.Name())
	_, _ = f.WriteString(code)
	_ = f.Close()

	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, pythonPath, "-I", f.Name())
	out, err := cmd.CombinedOutput()
	res := string(out)
	if cctx.Err() == context.DeadlineExceeded {
		return "run_python: timed out after 60s. Break the work into smaller steps."
	}
	if err != nil && res == "" {
		return "run_python error: " + err.Error()
	}
	if len(res) > 24000 {
		res = res[:24000] + "\n... [truncated]"
	}
	if res == "" {
		res = "(no output)"
	}
	return res
}
