package swarm

import (
	"encoding/json"
	"fmt"
	"github.com/well-lzz/chronos/internal/durable"
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

func (s *Swarm) loadRuntime(spec string) (executorRuntime, error) {
	path := s.runtimePath(spec)
	if path == "" {
		return executorRuntime{}, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return executorRuntime{}, nil
	}
	if err != nil {
		return executorRuntime{}, err
	}
	var state executorRuntime
	if err := json.Unmarshal(data, &state); err != nil {
		return executorRuntime{}, fmt.Errorf("decode executor checkpoint: %w", err)
	}
	if state.Version != 1 {
		return executorRuntime{}, fmt.Errorf("unsupported executor checkpoint version %d", state.Version)
	}
	if state.Status == "finished" || state.Status == "cancelled" || state.Status == "flag_found" {
		return executorRuntime{}, nil
	}
	return state, nil
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
	return durable.WriteFile(path, data)
}
