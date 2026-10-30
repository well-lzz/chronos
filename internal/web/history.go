package web

import (
	"errors"
	"net/http"
	"os"

	"github.com/well-lzz/chronos/internal/console"
	"github.com/well-lzz/chronos/internal/runhistory"
)

func activeRunIDs(snapshot map[string]any) map[string]bool {
	active := map[string]bool{}
	swarms, _ := snapshot["swarms"].(map[string]any)
	for _, raw := range swarms {
		status, _ := raw.(map[string]any)
		if id, _ := status["run_id"].(string); id != "" {
			active[id] = true
		}
	}
	return active
}

func registerHistory(mux *http.ServeMux, src console.Source, store *runhistory.Store) {
	mux.HandleFunc("/api/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		if store == nil {
			writeJSON(w, map[string]any{"runs": []any{}})
			return
		}
		runs, err := store.List(activeRunIDs(src.Snapshot()))
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"runs": runs})
	})
	mux.HandleFunc("/api/runs/detail", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		if store == nil {
			http.NotFound(w, r)
			return
		}
		detail, err := store.Detail(r.URL.Query().Get("id"), activeRunIDs(src.Snapshot()))
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, detail)
	})
	mux.HandleFunc("/api/runs/traffic", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		if store == nil {
			http.NotFound(w, r)
			return
		}
		row, err := store.Traffic(r.URL.Query().Get("id"), r.URL.Query().Get("traffic"))
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, row)
	})
}
