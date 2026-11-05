// Package runevent stores control transitions without replaying external work.
package runevent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/well-lzz/chronos/internal/durable"
)

type Event struct {
	Version int             `json:"schema_version"`
	RunID   string          `json:"run_id"`
	Seq     uint64          `json:"seq"`
	ID      string          `json:"event_id"`
	Kind    string          `json:"kind"`
	At      time.Time       `json:"at"`
	Payload json.RawMessage `json:"payload"`
}

type Journal struct {
	mu          sync.Mutex
	path, runID string
	events      []Event
	byID        map[string]int
	err         error
}

func Open(path, runID string) (*Journal, error) {
	j := &Journal{path: path, runID: runID, byID: map[string]int{}}
	if path == "" {
		return j, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return j, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		end := bytes.LastIndexByte(data, '\n') + 1
		f, err := os.OpenFile(path, os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		err = f.Truncate(int64(end))
		if err == nil {
			err = f.Sync()
		}
		f.Close()
		if err != nil {
			return nil, err
		}
		data = data[:end]
	}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("invalid control event: %w", err)
		}
		_, duplicate := j.byID[e.ID]
		if e.Version != 1 || e.RunID != runID || e.Seq != uint64(len(j.events)+1) || e.ID == "" || duplicate || e.Kind == "" {
			return nil, fmt.Errorf("invalid control event identity/version at sequence %d", e.Seq)
		}
		j.byID[e.ID] = len(j.events)
		j.events = append(j.events, e)
	}
	return j, nil
}

// Append is idempotent only for exactly the same kind and payload. A conflicting
// reuse of an event ID fails rather than hiding a different operation.
func (j *Journal) Append(id, kind string, payload any) (Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.appendLocked(id, kind, payload)
}

// Claim commits an execution boundary at most once, even if duplicate calls
// race. The caller must not execute when acquired is false.
func (j *Journal) Claim(id, kind string, payload any) (acquired bool, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err != nil {
		return false, j.err
	}
	if _, ok := j.byID[id]; ok {
		return false, nil
	}
	_, err = j.appendLocked(id, kind, payload)
	return err == nil, err
}

func (j *Journal) appendLocked(id, kind string, payload any) (Event, error) {
	if j.err != nil {
		return Event{}, j.err
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	if i, ok := j.byID[id]; ok {
		e := j.events[i]
		if e.Kind != kind || !bytes.Equal(e.Payload, data) {
			return Event{}, fmt.Errorf("conflicting control event %s", id)
		}
		return e, nil
	}
	if id == "" || kind == "" {
		return Event{}, fmt.Errorf("control event needs id and kind")
	}
	e := Event{Version: 1, RunID: j.runID, Seq: uint64(len(j.events) + 1), ID: id, Kind: kind, At: time.Now().UTC(), Payload: data}
	line, err := json.Marshal(e)
	if err != nil {
		return Event{}, err
	}
	if j.path != "" {
		if err = os.MkdirAll(filepath.Dir(j.path), 0700); err == nil {
			var f *os.File
			f, err = os.OpenFile(j.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
			if err == nil {
				_, err = f.Write(append(line, '\n'))
				if err == nil {
					err = f.Sync()
				}
				closeErr := f.Close()
				if err == nil {
					err = closeErr
				}
				if err == nil {
					err = durable.SyncDir(filepath.Dir(j.path))
				}
			}
		}
		if err != nil {
			j.err = err
			return Event{}, err
		}
	}
	j.byID[id] = len(j.events)
	j.events = append(j.events, e)
	return e, nil
}

func (j *Journal) Events() []Event {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Event, len(j.events))
	copy(out, j.events)
	for i := range out {
		out[i].Payload = append(json.RawMessage(nil), out[i].Payload...)
	}
	return out
}
