package solver

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"github.com/well-lzz/chronos/internal/pythonexec"
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
	if blocked := s.requireMutationLease("mission:*"); blocked != "" {
		return blocked
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	output, err := pythonexec.Run(cctx, pythonPath, a.Code)
	if err != nil {
		return output + "\nrun_python error: " + err.Error()
	}
	return output
}
