package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/well-lzz/chronos/internal/runhistory"
	"github.com/well-lzz/chronos/internal/swarm"
	"github.com/well-lzz/chronos/internal/traffic"
)

type historySource struct{ id string }

func (s historySource) Snapshot() map[string]any {
	return map[string]any{"swarms": map[string]any{"task": map[string]any{"run_id": s.id}}}
}
func (historySource) Activity(string, string, int) []swarm.ActEvent { return nil }
func (historySource) Command(string, string, string, string)        {}

func TestHistoryAPIListsAndReadsRuns(t *testing.T) {
	store := &runhistory.Store{StateDir: t.TempDir()}
	if err := store.Start("run-one", "task", "engagement", "target.test", time.Now()); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerHistory(mux, historySource{id: "run-one"}, store)
	list := httptest.NewRecorder()
	mux.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/runs", nil))
	var result struct {
		Runs []runhistory.Run `json:"runs"`
	}
	if list.Code != http.StatusOK || json.Unmarshal(list.Body.Bytes(), &result) != nil || len(result.Runs) != 1 || result.Runs[0].Status != "active" {
		t.Fatalf("history list: %d %s", list.Code, list.Body.String())
	}
	detail := httptest.NewRecorder()
	mux.ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/api/runs/detail?id=run-one", nil))
	if detail.Code != http.StatusOK || !json.Valid(detail.Body.Bytes()) {
		t.Fatalf("history detail: %d %s", detail.Code, detail.Body.String())
	}
	missing := httptest.NewRecorder()
	mux.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/api/runs/detail?id=unknown", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing history status: %d", missing.Code)
	}
	log, err := traffic.LoadOrNew("run-one", store.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.RecordHTTP("GET", "http://target.test/", nil, "", "HTTP 200\nbody", "agent"); err != nil {
		t.Fatal(err)
	}
	trafficResponse := httptest.NewRecorder()
	mux.ServeHTTP(trafficResponse, httptest.NewRequest(http.MethodGet, "/api/runs/traffic?id=run-one&traffic=t1", nil))
	if trafficResponse.Code != http.StatusOK || !json.Valid(trafficResponse.Body.Bytes()) {
		t.Fatalf("history traffic: %d %s", trafficResponse.Code, trafficResponse.Body.String())
	}
}
