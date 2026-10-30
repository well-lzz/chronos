package scope

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type savedOperatorScope struct {
	Version int      `json:"version"`
	Targets []string `json:"targets"`
	RunID   string   `json:"run_id,omitempty"`
}

// NewRunID creates a fresh state namespace for a new operator-authorized task.
func NewRunID() (string, error) {
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

// MissionStateID keeps model sessions, blackboard, and traffic in the same
// per-run namespace while retaining the platform's original challenge name.
func MissionStateID(base, runID string) string {
	if runID == "" {
		return base
	}
	return base + "--scope-" + runID
}

func operatorScopePath(stateDir, identity string) string {
	if stateDir == "" {
		return ""
	}
	hash := sha256.Sum256([]byte(identity))
	return filepath.Join(stateDir, "scopes", hex.EncodeToString(hash[:8])+".json")
}

// LoadOperatorScope returns a saved operator boundary, if one exists. The
// identity must distinguish the engagement or platform mission being edited.
func LoadOperatorScope(stateDir, identity string) (Scope, bool, error) {
	current, _, found, err := LoadOperatorScopeRun(stateDir, identity)
	return current, found, err
}

// LoadOperatorScopeRun also returns the isolated run ID saved with the scope.
// Older scope files have an empty run ID and can be migrated on startup.
func LoadOperatorScopeRun(stateDir, identity string) (Scope, string, bool, error) {
	path := operatorScopePath(stateDir, identity)
	if path == "" {
		return Scope{}, "", false, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Scope{}, "", false, nil
	}
	if err != nil {
		return Scope{}, "", false, err
	}
	var saved savedOperatorScope
	if err := json.Unmarshal(data, &saved); err != nil || saved.Version != 1 {
		return Scope{}, "", false, fmt.Errorf("invalid saved operator scope")
	}
	scope, err := ParseOperatorTargets(saved.Targets)
	if err != nil {
		return Scope{}, "", false, fmt.Errorf("invalid saved operator scope: %w", err)
	}
	return scope, saved.RunID, true, nil
}

// SaveOperatorScope persists the exact replacement boundary independently of
// transient runtime checkpoints, so normal completion does not erase it.
func SaveOperatorScope(stateDir, identity string, current Scope) error {
	return SaveOperatorScopeRun(stateDir, identity, current, "")
}

// SaveOperatorScopeRun stores the current scope and the namespace of its new
// execution. The caller creates a fresh run ID whenever targets change.
func SaveOperatorScopeRun(stateDir, identity string, current Scope, runID string) error {
	path := operatorScopePath(stateDir, identity)
	if path == "" {
		return fmt.Errorf("state directory is required to save scope")
	}
	if current.IsEmpty() {
		return fmt.Errorf("cannot save an empty operator scope")
	}
	data, err := json.MarshalIndent(savedOperatorScope{Version: 1, Targets: current.Entries(), RunID: runID}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".scope-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return nil
}
