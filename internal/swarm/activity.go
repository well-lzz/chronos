package swarm

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

type persistedActivity struct {
	Model string   `json:"model"`
	Event ActEvent `json:"event"`
}

func (s *Swarm) activityPath() string {
	if s.BB == nil || s.BB.Path == "" {
		return ""
	}
	root := filepath.Dir(filepath.Dir(s.BB.Path))
	name := strings.TrimSuffix(filepath.Base(s.BB.Path), ".json")
	return filepath.Join(root, "activities", name+".jsonl")
}

// appendActivityLocked keeps the live ring bounded and journals its public
// events for read-only task history. The caller holds s.mu so appends are ordered.
func (s *Swarm) appendActivityLocked(model string, event ActEvent) {
	s.countToolActivityLocked(model, event)
	if s.activity == nil {
		s.activity = map[string][]ActEvent{}
	}
	buf := append(s.activity[model], event)
	if len(buf) > activityRing {
		buf = buf[len(buf)-activityRing:]
	}
	s.activity[model] = buf
	path := s.activityPath()
	if path == "" {
		return
	}
	data, err := json.Marshal(persistedActivity{Model: model, Event: event})
	if err != nil {
		log.Printf("serialize activity for %s: %v", s.Meta.Name, err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Printf("create activity directory for %s: %v", s.Meta.Name, err)
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("open activity for %s: %v", s.Meta.Name, err)
		return
	}
	_, writeErr := f.Write(append(data, '\n'))
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		log.Printf("append activity for %s: %v %v", s.Meta.Name, writeErr, closeErr)
	}
}

// countToolActivityLocked accounts for every activity source, including typed
// solver events and child streams. Results never count as another invocation.
// Counts outlive the live ring and are rebuilt from the same journal on restart.
func (s *Swarm) countToolActivityLocked(model string, event ActEvent) {
	if event.Kind != "tool_call" || event.Tool == "" {
		return
	}
	if s.toolCalls == nil {
		s.toolCalls = map[string]int{}
	}
	if s.toolByName == nil {
		s.toolByName = map[string]map[string]int{}
	}
	if s.toolByName[model] == nil {
		s.toolByName[model] = map[string]int{}
	}
	s.toolCalls[model]++
	s.toolByName[model][event.Tool]++
}

// loadActivityLocked restores recent conversation and tool events after a
// process restart. Full history stays on disk; the live stream keeps its cap.
func (s *Swarm) loadActivityLocked() error {
	path := s.activityPath()
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 64*1024), 2*1024*1024)
	maxSeq := atomic.LoadInt64(&s.evSeq)
	for scan.Scan() {
		var record persistedActivity
		if json.Unmarshal(scan.Bytes(), &record) != nil || record.Model == "" {
			continue
		}
		if record.Event.Seq > maxSeq {
			maxSeq = record.Event.Seq
		}
		buf := append(s.activity[record.Model], record.Event)
		if len(buf) > activityRing {
			buf = buf[len(buf)-activityRing:]
		}
		s.activity[record.Model] = buf
		s.countToolActivityLocked(record.Model, record.Event)
	}
	atomic.StoreInt64(&s.evSeq, maxSeq)
	return scan.Err()
}
