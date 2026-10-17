// Package web is the embedded console: live run visualization + operator
// control + per-executor activity watch. Additive observability — the scheduler
// and swarms are unchanged. State is read from the scheduler snapshot; operator
// actions go through the same command queue. Live push uses SSE (stdlib only,
// no websocket dependency).
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/well-lzz/chronos/internal/console"
	"github.com/well-lzz/chronos/internal/llm"
	"github.com/well-lzz/chronos/internal/llmconfig"
)

// The console front-end is a Next.js app, statically exported to dist/ (built
// with `npm run build` under web/, then copied here) and embedded so the single
// binary still serves the whole UI. `all:` is required to include _next/.
//
//go:embed all:dist
var distFS embed.FS

// Serve starts the console HTTP server over any console.Source (the scheduler
// or a single engagement). store, when non-nil, backs the model-settings panel.
// The caller runs it in a goroutine.
func Serve(ctx context.Context, src console.Source, store *llmconfig.Store, host string, port int) error {
	if !loopbackHost(host) {
		return fmt.Errorf("web console refuses non-loopback host %q; use 127.0.0.1, ::1, or localhost", host)
	}
	operatorToken, err := newOperatorToken()
	if err != nil {
		return fmt.Errorf("generate operator token: %w", err)
	}
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
		if !authorizeOperator(w, r, operatorToken) {
			return
		}
		var c struct {
			Action    string `json:"action"`
			Challenge string `json:"challenge"`
			Model     string `json:"model"`
			Message   string `json:"message"`
		}
		if !decodeJSON(w, r, &c) {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		src.Command(c.Action, c.Challenge, c.Model, c.Message)
		writeJSON(w, map[string]any{"ok": true})
	})

	mux.HandleFunc("/api/scope", func(w http.ResponseWriter, r *http.Request) {
		controller, ok := src.(console.ScopeController)
		if !ok {
			writeJSON(w, console.ScopeConfig{Supported: false})
			return
		}
		if r.Method == http.MethodGet {
			writeJSON(w, controller.ScopeConfig())
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
			return
		}
		if !authorizeOperator(w, r, operatorToken) {
			return
		}
		var request struct {
			Targets []string `json:"targets"`
		}
		if !decodeJSON(w, r, &request) {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if err := controller.UpdateScope(request.Targets); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		response := controller.ScopeConfig()
		writeJSON(w, map[string]any{
			"ok": true, "supported": response.Supported, "editable": response.Editable,
			"targets": response.Targets, "description": response.Description,
		})
	})

	// Model settings: GET current (key never returned), POST to save, POST /test
	// to verify a profile against the live endpoint.
	mux.HandleFunc("/api/llm", func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			writeJSON(w, map[string]any{"supported": false})
			return
		}
		if r.Method == http.MethodGet {
			pub := store.Public()
			pub["supported"] = true
			writeJSON(w, pub)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
			return
		}
		if !authorizeOperator(w, r, operatorToken) {
			return
		}
		var p llmconfig.Profile
		if !decodeJSON(w, r, &p) {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if err := store.Set(p); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		pub := store.Public()
		pub["ok"] = true
		writeJSON(w, pub)
	})

	mux.HandleFunc("/api/llm/test", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if !authorizeOperator(w, r, operatorToken) {
			return
		}
		if store == nil {
			writeJSON(w, map[string]any{"ok": false, "error": "settings unavailable"})
			return
		}
		var p llmconfig.Profile
		if !decodeJSON(w, r, &p) {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		// An omitted key means "test the saved one".
		if p.APIKey == "" {
			saved := store.Get()
			p.APIKey = saved.APIKey
			if p.BaseURL == "" {
				p.BaseURL = saved.BaseURL
			}
			if p.Model == "" {
				p.Model = saved.Model
			}
		}
		if err := llm.Ping(r.Context(), p); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	log.Printf("Web console on http://%s", addr)
	log.Printf("Authenticated operator console: http://%s/#operator=%s", addr, operatorToken)
	server := &http.Server{
		Addr: addr, Handler: protect(mux), ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func newOperatorToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func authorizeOperator(w http.ResponseWriter, r *http.Request, expected string) bool {
	provided := strings.TrimSpace(r.Header.Get("X-Chronos-Operator"))
	if len(provided) != len(expected) || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		writeJSONStatus(w, http.StatusUnauthorized, map[string]any{
			"ok": false, "error": "operator authorization required; reopen the authenticated console URL printed by Chronos",
		})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(dst) == nil
}

func loopbackHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || !strings.EqualFold(u.Host, r.Host) {
					http.Error(w, "cross-origin request blocked", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
