package swarm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type executorRuntime struct {
	Version           int            `json:"version"`
	Status            string         `json:"status"`
	StartedAt         time.Time      `json:"started_at"`
	Cycles            int            `json:"cycles"`
	Bumps             int            `json:"bumps"`
	QuietCycles       int            `json:"quiet_cycles"`
	ProviderFails     int            `json:"provider_failures"`
	Steps             int            `json:"steps"`
	ToolCalls         int            `json:"tool_calls"`
	Tools             map[string]int `json:"tools,omitempty"`
	BBRevision        uint64         `json:"blackboard_revision"`
	HandoffGeneration int            `json:"handoff_generation"`
}

func (s *Swarm) runtimePath(spec string) string {
	path := s.sessionPath(spec)
	if path == "" {
		return ""
	}
	return strings.TrimSuffix(path, ".jsonl") + ".state.json"
}

func (s *Swarm) loadRuntime(spec string) executorRuntime {
	path := s.runtimePath(spec)
	if path == "" {
		return executorRuntime{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return executorRuntime{}
	}
	var state executorRuntime
	if json.Unmarshal(data, &state) != nil || state.Version != 1 || state.Status == "finished" || state.Status == "cancelled" || state.Status == "flag_found" {
		return executorRuntime{}
	}
	return state
}

func (s *Swarm) saveRuntime(spec string, state executorRuntime) error {
	path := s.runtimePath(spec)
	if path == "" {
		return nil
	}
	state.Version = 1
	state.BBRevision = s.BB.Revision()
	data, err := json.MarshalIndent(state, "", "  ")
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
