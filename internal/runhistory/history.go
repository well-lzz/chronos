// Package runhistory indexes isolated task runs for the read-only console history.
// A manifest records the scope and lifecycle; existing blackboards without a
// manifest are discovered so work from earlier versions remains visible.
package runhistory

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/swarm"
	"github.com/well-lzz/chronos/internal/traffic"
)

type Store struct{ StateDir string }

type Run struct {
	ID      string         `json:"id"`
	Mission string         `json:"mission"`
	Mode    string         `json:"mode,omitempty"`
	Scope   string         `json:"scope,omitempty"`
	Started time.Time      `json:"started,omitempty"`
	Ended   time.Time      `json:"ended,omitempty"`
	Updated time.Time      `json:"updated,omitempty"`
	Status  string         `json:"status"`
	Outcome string         `json:"outcome,omitempty"`
	Summary map[string]int `json:"summary"`
	Legacy  bool           `json:"legacy,omitempty"`
}

type ActivityRecord struct {
	Model string         `json:"model"`
	Event swarm.ActEvent `json:"event"`
}

type Detail struct {
	Run        Run                      `json:"run"`
	Findings   []blackboard.FindingRow  `json:"findings"`
	Facts      []blackboard.Fact        `json:"facts"`
	Subgoals   []blackboard.SubGoal     `json:"subgoals"`
	Traffic    []map[string]any         `json:"traffic"`
	Graph      blackboard.GraphSnapshot `json:"graph"`
	Activity   []ActivityRecord         `json:"activity"`
	EventCount int                      `json:"event_count"`
}

type savedReport struct {
	Mission  map[string]any           `json:"mission"`
	Summary  map[string]int           `json:"summary"`
	Findings []blackboard.Finding     `json:"findings"`
	Facts    []blackboard.Fact        `json:"facts"`
	Subgoals []blackboard.SubGoal     `json:"subgoals"`
	Graph    blackboard.GraphSnapshot `json:"evidence_graph"`
}

func (s Store) reportFiles() ([]string, error) {
	perRun, err := filepath.Glob(filepath.Join(s.StateDir, "reports", "*", "runs", "*", "report.json"))
	if err != nil {
		return nil, err
	}
	latest, err := filepath.Glob(filepath.Join(s.StateDir, "reports", "*", "report.json"))
	if err != nil {
		return nil, err
	}
	return append(perRun, latest...), nil
}

func loadReport(path string) (savedReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return savedReport{}, err
	}
	var report savedReport
	if err := json.Unmarshal(data, &report); err != nil {
		return savedReport{}, err
	}
	return report, nil
}

func (s Store) manifestPath(id string) string {
	hash := sha256.Sum256([]byte(id))
	return filepath.Join(s.StateDir, "run-history", hex.EncodeToString(hash[:12])+".json")
}

func (s Store) Start(id, mission, mode, scope string, started time.Time) error {
	if s.StateDir == "" || id == "" {
		return nil
	}
	path := s.manifestPath(id)
	if data, err := os.ReadFile(path); err == nil {
		var existing Run
		if json.Unmarshal(data, &existing) == nil && existing.ID == id {
			return nil // process restart resumes the same run
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return writeRun(path, Run{ID: id, Mission: mission, Mode: mode, Scope: scope, Started: started})
}

func (s Store) Finish(id, outcome string, ended time.Time) error {
	if s.StateDir == "" || id == "" {
		return nil
	}
	path := s.manifestPath(id)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var run Run
	if err := json.Unmarshal(data, &run); err != nil || run.ID != id {
		return fmt.Errorf("invalid run manifest for %s", id)
	}
	if run.Ended.IsZero() {
		run.Ended, run.Outcome = ended, outcome
		return writeRun(path, run)
	}
	return nil
}

func writeRun(path string, run Run) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".run-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s Store) List(active map[string]bool) ([]Run, error) {
	if s.StateDir == "" {
		return []Run{}, nil
	}
	indexed := map[string]Run{}
	manifests, err := filepath.Glob(filepath.Join(s.StateDir, "run-history", "*.json"))
	if err != nil {
		return nil, err
	}
	for _, path := range manifests {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var run Run
		if err := json.Unmarshal(data, &run); err != nil || run.ID == "" {
			return nil, fmt.Errorf("invalid run manifest %s", path)
		}
		indexed[run.ID] = run
	}
	boards, err := filepath.Glob(filepath.Join(s.StateDir, "blackboards", "*.json"))
	if err != nil {
		return nil, err
	}
	boardTimes := map[string]time.Time{}
	for _, path := range boards {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var header struct {
			MissionID string `json:"mission_id"`
		}
		if err := json.Unmarshal(data, &header); err != nil || header.MissionID == "" {
			return nil, fmt.Errorf("invalid blackboard %s", path)
		}
		if _, exists := indexed[header.MissionID]; !exists {
			indexed[header.MissionID] = Run{ID: header.MissionID, Mission: baseMission(header.MissionID), Legacy: true}
		}
		if info, err := os.Stat(path); err == nil {
			boardTimes[header.MissionID] = info.ModTime()
		}
	}
	reports, err := s.reportFiles()
	if err != nil {
		return nil, err
	}
	reportSummaries := map[string]map[string]int{}
	for _, path := range reports {
		report, err := loadReport(path)
		if err != nil {
			continue
		}
		id, _ := report.Mission["name"].(string)
		if id == "" {
			continue
		}
		counts := make(map[string]int, len(report.Summary)+5)
		for key, value := range report.Summary {
			counts[key] = value
		}
		counts["total"] = len(report.Findings)
		counts["facts"] = len(report.Facts)
		for _, finding := range report.Findings {
			counts[blackboard.Verdict(finding.Status)]++
		}
		reportSummaries[id] = counts
		run, exists := indexed[id]
		if !exists {
			run = Run{ID: id, Mission: baseMission(id), Legacy: true}
		}
		if run.Legacy {
			if scope, _ := report.Mission["scope"].(string); scope != "" {
				run.Scope = scope
			}
			if mode, _ := report.Mission["mode"].(string); mode != "" {
				run.Mode = mode
			}
			if started, _ := report.Mission["started"].(string); started != "" {
				if value, err := time.ParseInLocation("2006-01-02 15:04:05", started, time.Local); err == nil {
					run.Started = value
				}
			}
			if ended, _ := report.Mission["ended"].(string); ended != "" {
				if value, err := time.ParseInLocation("2006-01-02 15:04:05", ended, time.Local); err == nil {
					run.Ended = value
				}
			}
		}
		indexed[id] = run
	}
	out := make([]Run, 0, len(indexed))
	for id, run := range indexed {
		bb, err := blackboard.LoadOrNew(id, s.StateDir)
		if err != nil {
			return nil, err
		}
		counts := bb.VerdictCounts()
		counts["facts"] = bb.Counts()["facts"]
		if _, hasBoard := boardTimes[id]; !hasBoard && reportSummaries[id] != nil {
			counts = reportSummaries[id]
		}
		run.Summary = counts
		if modified, ok := boardTimes[id]; ok {
			run.Updated = modified
			if run.Started.IsZero() {
				run.Started = modified // earliest time available for legacy runs
			}
		}
		if run.Mission == "" {
			run.Mission = baseMission(id)
		}
		if active[id] {
			run.Status = "active"
		} else if !run.Ended.IsZero() {
			run.Status = "completed"
		} else if run.Legacy {
			run.Status = "archived"
		} else {
			run.Status = "interrupted"
		}
		out = append(out, run)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Started.Equal(out[j].Started) {
			return out[i].ID < out[j].ID
		}
		return out[i].Started.After(out[j].Started)
	})
	return out, nil
}

func baseMission(id string) string {
	const marker = "--scope-"
	i := strings.LastIndex(id, marker)
	if i < 0 || len(id[i+len(marker):]) != 24 {
		return id
	}
	if _, err := hex.DecodeString(id[i+len(marker):]); err != nil {
		return id
	}
	return id[:i]
}

func (s Store) Detail(id string, active map[string]bool) (Detail, error) {
	runs, err := s.List(active)
	if err != nil {
		return Detail{}, err
	}
	var run *Run
	for i := range runs {
		if runs[i].ID == id {
			run = &runs[i]
			break
		}
	}
	if run == nil {
		return Detail{}, os.ErrNotExist
	}
	bb, err := blackboard.LoadOrNew(id, s.StateDir)
	if err != nil {
		return Detail{}, err
	}
	tl, err := traffic.LoadOrNew(id, s.StateDir)
	if err != nil {
		return Detail{}, err
	}
	activity, count, err := s.readActivity(bb.Path)
	if err != nil {
		return Detail{}, err
	}
	result := Detail{Run: *run, Findings: bb.FindingsPublic(), Facts: bb.FactsOrdered(),
		Subgoals: bb.SubgoalsOrdered(), Traffic: tl.RecentRows(200), Graph: bb.Graph(),
		Activity: activity, EventCount: count}
	if _, err := os.Stat(bb.Path); os.IsNotExist(err) {
		if report, ok := s.findReport(id); ok {
			result.Findings = make([]blackboard.FindingRow, 0, len(report.Findings))
			for _, finding := range report.Findings {
				result.Findings = append(result.Findings, blackboard.FindingRow{
					ID: finding.ID, Title: finding.Title, Severity: finding.Severity,
					Status: finding.Status, Verdict: blackboard.Verdict(finding.Status), VerdictReason: finding.VerdictReason,
					Target: finding.Target, Category: finding.Category, Evidence: len(finding.EvidenceRefs),
					EvidenceText: finding.Evidence, Reproduction: finding.Reproduction, Remediation: finding.Remediation,
					FoundBy: finding.FoundBy, TS: finding.TS, VerifiedTS: finding.VerifiedTS,
					EvidenceRefs: finding.EvidenceRefs, Verifications: finding.Verifications,
				})
			}
			result.Facts, result.Subgoals, result.Graph = report.Facts, report.Subgoals, report.Graph
		}
	}
	return result, nil
}

func (s Store) findReport(id string) (savedReport, bool) {
	paths, err := s.reportFiles()
	if err != nil {
		return savedReport{}, false
	}
	for _, path := range paths {
		report, err := loadReport(path)
		if err == nil && report.Mission["name"] == id {
			return report, true
		}
	}
	return savedReport{}, false
}

func (s Store) Traffic(runID, trafficID string) (map[string]any, error) {
	runs, err := s.List(nil)
	if err != nil {
		return nil, err
	}
	found := false
	for _, run := range runs {
		if run.ID == runID {
			found = true
			break
		}
	}
	if !found {
		return nil, os.ErrNotExist
	}
	log, err := traffic.LoadOrNew(runID, s.StateDir)
	if err != nil {
		return nil, err
	}
	row := log.Get(trafficID, "both", 0, 8192)
	if row["error"] != nil {
		return nil, os.ErrNotExist
	}
	return row, nil
}

func (s Store) readActivity(boardPath string) ([]ActivityRecord, int, error) {
	name := strings.TrimSuffix(filepath.Base(boardPath), ".json")
	path := filepath.Join(s.StateDir, "activities", name+".jsonl")
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return []ActivityRecord{}, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 64*1024), 2*1024*1024)
	var out []ActivityRecord
	count := 0
	for scan.Scan() {
		var record ActivityRecord
		if json.Unmarshal(scan.Bytes(), &record) != nil {
			continue // tolerate an interrupted final append
		}
		count++
		out = append(out, record)
		if len(out) > 1000 {
			out = out[len(out)-1000:]
		}
	}
	if err := scan.Err(); err != nil {
		return nil, 0, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Event.Seq < out[j].Event.Seq })
	return out, count, nil
}
