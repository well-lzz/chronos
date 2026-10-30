package runhistory

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/well-lzz/chronos/internal/blackboard"
	"github.com/well-lzz/chronos/internal/traffic"
)

func TestRunHistorySurvivesScopeSwitchAndDiscoversLegacyBoards(t *testing.T) {
	dir := t.TempDir()
	store := Store{StateDir: dir}
	start := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	if err := store.Start("task--scope-000000000000000000000001", "task", "engagement", "old.example", start); err != nil {
		t.Fatal(err)
	}
	old, err := blackboard.LoadOrNew("task--scope-000000000000000000000001", dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.AddFinding(blackboard.Finding{Title: "old finding", EvidenceRefs: []blackboard.EvidenceRef{{Ref: "t1", Role: "proof"}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(old.MissionID, "scope_changed", time.Now()); err != nil {
		t.Fatal(err)
	}
	newID := "task--scope-000000000000000000000002"
	if err := store.Start(newID, "task", "engagement", "new.example", time.Now()); err != nil {
		t.Fatal(err)
	}
	legacy, err := blackboard.LoadOrNew("legacy-task", dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.AddFact("legacy note", "", "agent"); err != nil {
		t.Fatal(err)
	}
	reportDir := filepath.Join(dir, "reports", "legacy-task", "runs", legacy.MissionID)
	if err := os.MkdirAll(reportDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reportDir, "report.json"), []byte(`{"mission":{"name":"legacy-task","mode":"assessment","scope":"legacy.example","started":"2026-10-26 10:00:00","ended":"2026-10-26 11:00:00"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runs, err := store.List(map[string]bool{newID: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("listed %d runs, want three: %+v", len(runs), runs)
	}
	byID := map[string]Run{}
	for _, run := range runs {
		byID[run.ID] = run
	}
	if byID[old.MissionID].Status != "completed" || byID[old.MissionID].Scope != "old.example" || byID[old.MissionID].Summary["confirmed"] != 1 {
		t.Fatalf("old run lost: %+v", byID[old.MissionID])
	}
	if byID[newID].Status != "active" || byID[newID].Scope != "new.example" {
		t.Fatalf("new run wrong: %+v", byID[newID])
	}
	if !byID[legacy.MissionID].Legacy || byID[legacy.MissionID].Summary["facts"] != 1 || byID[legacy.MissionID].Scope != "legacy.example" {
		t.Fatalf("legacy board not discovered: %+v", byID[legacy.MissionID])
	}
	detail, err := store.Detail(old.MissionID, nil)
	if err != nil || len(detail.Findings) != 1 || detail.Findings[0].Title != "old finding" {
		t.Fatalf("old detail missing: %+v %v", detail, err)
	}
	if _, err := store.Detail("../../etc/passwd", nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unknown ID was accepted: %v", err)
	}
}

func TestRunHistoryReadsTraffic(t *testing.T) {
	dir := t.TempDir()
	store := Store{StateDir: dir}
	if err := store.Start("traffic-run", "traffic", "engagement", "target.test", time.Now()); err != nil {
		t.Fatal(err)
	}
	log, err := traffic.LoadOrNew("traffic-run", dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.RecordHTTP("GET", "http://target.test/", nil, "", "HTTP 200\nbody", "agent"); err != nil {
		t.Fatal(err)
	}
	detail, err := store.Detail("traffic-run", nil)
	if err != nil || len(detail.Traffic) != 1 || detail.Traffic[0]["id"] != "t1" {
		t.Fatalf("traffic missing: %+v %v", detail.Traffic, err)
	}
	row, err := store.Traffic("traffic-run", "t1")
	if err != nil || row["response"] != "HTTP 200\nbody" {
		t.Fatalf("traffic content missing: %+v %v", row, err)
	}
}

func TestReportOnlyLegacyRunIsVisible(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reports", "report-only", "report.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data := `{"mission":{"name":"report-only","mode":"assessment","scope":"old.example","started":"2026-10-26 10:00:00","ended":"2026-10-26 11:00:00"},"summary":{"findings":1,"facts":0},"findings":[{"id":"v1","title":"old report finding","status":"candidate","severity":"low"}]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	store := Store{StateDir: dir}
	runs, err := store.List(nil)
	if err != nil || len(runs) != 1 || runs[0].Scope != "old.example" || runs[0].Summary["findings"] != 1 {
		t.Fatalf("report-only run missing: %+v %v", runs, err)
	}
	detail, err := store.Detail("report-only", nil)
	if err != nil || len(detail.Findings) != 1 || detail.Findings[0].Title != "old report finding" {
		t.Fatalf("report-only detail missing: %+v %v", detail, err)
	}
}
