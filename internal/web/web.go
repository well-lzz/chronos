// Package web is the embedded console: live run visualization + operator
// control + per-executor activity watch. Additive observability — the scheduler
// and swarms are unchanged. State is read from the scheduler snapshot; operator
// actions go through the same command queue. Live push uses SSE (stdlib only,
// no websocket dependency).
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/well-lzz/chronos/internal/console"
)

// The console front-end is a Next.js app, statically exported to dist/ (built
// with `npm run build` under web/, then copied here) and embedded so the single
// binary still serves the whole UI. `all:` is required to include _next/.
//
//go:embed all:dist
var distFS embed.FS

// Serve starts the console HTTP server over any console.Source (the scheduler
// or a single engagement). The caller runs it in a goroutine.
func Serve(src console.Source, host string, port int) error {
	mux := http.NewServeMux()

	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return err
	}
	mux.Handle("/", http.FileServerFS(sub))

	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, src.Snapshot())
	})

	mux.HandleFunc("/api/activity", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.Atoi(r.URL.Query().Get("after"))
		ev := src.Activity(r.URL.Query().Get("challenge"), r.URL.Query().Get("model"), after)
		writeJSON(w, map[string]any{"events": ev})
	})

	mux.HandleFunc("/api/stream", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			data, _ := json.Marshal(src.Snapshot())
			fmt.Fprintf(w, "data: %s\n\n", data)
			fl.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
			}
		}
	})

	mux.HandleFunc("/api/command", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var c struct {
			Action    string `json:"action"`
			Challenge string `json:"challenge"`
			Model     string `json:"model"`
			Message   string `json:"message"`
		}
		if json.NewDecoder(r.Body).Decode(&c) != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		src.Command(c.Action, c.Challenge, c.Model, c.Message)
		writeJSON(w, map[string]any{"ok": true})
	})

	addr := fmt.Sprintf("%s:%d", host, port)
	log.Printf("Web console on http://%s", addr)
	return http.ListenAndServe(addr, mux)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
