package engagement

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/well-lzz/chronos/internal/scope"
)

type Runtime struct {
	Version  int       `json:"version"`
	Started  time.Time `json:"started"`
	Deadline time.Time `json:"deadline"`
	Scope    []string  `json:"scope"`
	RunID    string    `json:"run_id,omitempty"`
}

func runtimePath(stateDir, name string) string {
	if stateDir == "" {
		return ""
	}
	hash := sha256.Sum256([]byte(name))
	return filepath.Join(stateDir, "engagement", hex.EncodeToString(hash[:8])+".json")
}

func LoadRuntime(stateDir, name string) (Runtime, error) {
	path := runtimePath(stateDir, name)
	if path == "" {
		return Runtime{}, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Runtime{}, nil
	}
	if err != nil {
		return Runtime{}, err
	}
	var state Runtime
	if err := json.Unmarshal(data, &state); err != nil {
		return Runtime{}, err
	}
	if state.Version != 1 || state.Started.IsZero() {
		return Runtime{}, fmt.Errorf("invalid engagement runtime state")
	}
	return state, nil
}

func SaveRuntime(stateDir, name string, started, deadline time.Time, current scope.Scope, runID ...string) error {
	path := runtimePath(stateDir, name)
	if path == "" {
		return nil
	}
	id := ""
	if len(runID) > 0 {
		id = runID[0]
	}
	data, err := json.MarshalIndent(Runtime{Version: 1, Started: started, Deadline: deadline, Scope: current.Entries(), RunID: id}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func ClearRuntime(stateDir, name string) error {
	path := runtimePath(stateDir, name)
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
