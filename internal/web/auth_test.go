package web

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/well-lzz/chronos/internal/console"
	"github.com/well-lzz/chronos/internal/llmconfig"
	"github.com/well-lzz/chronos/internal/swarm"
)

type securedSource struct{ commands, scopes atomic.Int32 }

func (s *securedSource) Snapshot() map[string]any                      { return map[string]any{"ok": true} }
func (s *securedSource) Activity(string, string, int) []swarm.ActEvent { return nil }
func (s *securedSource) Command(string, string, string, string)        { s.commands.Add(1) }
func (s *securedSource) ScopeConfig() console.ScopeConfig {
	return console.ScopeConfig{Supported: true, Editable: true}
}
func (s *securedSource) UpdateScope([]string) error { s.scopes.Add(1); return nil }

func TestAllConsoleAPIsRequireAuthenticationBeforeSideEffects(t *testing.T) {
	token := strings.Repeat("t", 43)
	auth, _ := newConsoleAuth("127.0.0.1", 8000, token)
	src := &securedSource{}
	store := llmconfig.Load(t.TempDir())
	handler, err := consoleHandler(src, store, Services{}, auth)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/command", "/api/scope", "/api/approvals/decision", "/api/llm", "/api/llm/test"} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://127.0.0.1:8000"+path, strings.NewReader(`{"action":"resume","targets":["outside.test"]}`))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != 401 {
				t.Fatalf("unauthenticated mutation status: %d", w.Code)
			}
		})
	}
	for _, path := range []string{"/api/state", "/api/stream", "/api/activity", "/api/llm", "/api/runs", "/api/llm/records/trace"} {
		r := httptest.NewRequest("GET", "http://127.0.0.1:8000"+path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("unauthenticated read %s: %d", path, w.Code)
		}
	}
	if src.commands.Load() != 0 || src.scopes.Load() != 0 || store.Active() {
		t.Fatal("unauthenticated request changed state")
	}
}

func TestConsoleRejectsRebindingOriginsAndMissingCSRFEvenWithCookie(t *testing.T) {
	auth, _ := newConsoleAuth("127.0.0.1", 8000, strings.Repeat("t", 43))
	src := &securedSource{}
	handler, err := consoleHandler(src, nil, Services{}, auth)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, host, origin, site, csrf, content string
		bearer                                  bool
		want                                    int
	}{
		{"rebound", "evil.test:8000", "", "", "", "application/json", true, 403},
		{"wrong port", "127.0.0.1:9000", "", "", "", "application/json", true, 403},
		{"wrong scheme", "127.0.0.1:8000", "https://127.0.0.1:8000", "", "", "application/json", true, 403},
		{"cross origin", "127.0.0.1:8000", "http://evil.test:8000", "", "", "application/json", true, 403},
		{"null origin", "127.0.0.1:8000", "null", "", "", "application/json", true, 403},
		{"fetch metadata", "127.0.0.1:8000", "", "cross-site", "", "application/json", true, 403},
		{"same site different port", "127.0.0.1:8000", "", "same-site", "", "application/json", true, 403},
		{"missing csrf", "127.0.0.1:8000", "", "", "", "application/json", false, 403},
		{"wrong csrf", "127.0.0.1:8000", "", "", "wrong", "application/json", false, 403},
		{"form content", "127.0.0.1:8000", "", "", auth.csrf, "text/plain", false, 415},
		{"cookie valid", "127.0.0.1:8000", "http://127.0.0.1:8000", "same-origin", auth.csrf, "application/json", false, 200},
		{"bearer cli", "127.0.0.1:8000", "", "", "", "application/json", true, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://127.0.0.1:8000/api/command", strings.NewReader(`{"action":"pause"}`))
			r.Host = tc.host
			r.AddCookie(&http.Cookie{Name: auth.cookie, Value: auth.session})
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Sec-Fetch-Site", tc.site)
			r.Header.Set("X-Chronos-CSRF", tc.csrf)
			r.Header.Set("Content-Type", tc.content)
			if tc.bearer {
				r.Header.Set("Authorization", "Bearer "+auth.token)
			}
			before := src.commands.Load()
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if tc.want != 200 && src.commands.Load() != before {
				t.Fatal("rejected request reached command handler")
			}
		})
	}
}

func TestBrowserSessionBootstrapAndAuthenticatedScopeWrite(t *testing.T) {
	server := httptest.NewUnstartedServer(nil)
	defer server.Close()
	port, _ := strconv.Atoi(strings.Split(server.Listener.Addr().String(), ":")[1])
	auth, _ := newConsoleAuth("127.0.0.1", port, strings.Repeat("t", 43))
	src := &securedSource{}
	handler, err := consoleHandler(src, nil, Services{}, auth)
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	r, _ := http.NewRequest("POST", server.URL+"/api/session", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+auth.token)
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	var session struct {
		CSRF string `json:"csrf"`
	}
	err = json.NewDecoder(response.Body).Decode(&session)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || session.CSRF == "" {
		t.Fatalf("bootstrap: %d %v", response.StatusCode, err)
	}
	cookies := response.Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("missing browser cookie protections")
	}
	r, _ = http.NewRequest("POST", server.URL+"/api/scope", strings.NewReader(`{"targets":["safe.test"]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", server.URL)
	r.Header.Set("X-Chronos-CSRF", session.CSRF)
	response, err = client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 || src.scopes.Load() != 1 {
		t.Fatalf("authorized scope update rejected: %d", response.StatusCode)
	}
	if strings.Contains(auth.accessURL(), "?") || !strings.Contains(auth.accessURL(), "#token=") {
		t.Fatal("token should be in fragment, not server query")
	}
}

func TestConsoleTokenRotationAndStrictRequestBodies(t *testing.T) {
	for _, token := range []string{"short", strings.Repeat("x", 513), strings.Repeat("x", 32) + "\n", strings.Repeat("x", 32) + "/"} {
		if _, err := newConsoleAuth("127.0.0.1", 8000, token); err == nil {
			t.Fatal("accepted invalid operator token")
		}
	}
	auth, err := newConsoleAuth("127.0.0.1", 8000, "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := newConsoleAuth("127.0.0.1", 8000, auth.token)
	if err != nil {
		t.Fatal(err)
	}
	if auth.session == other.session || auth.csrf == other.csrf {
		t.Fatal("restart reused browser secrets")
	}
	src := &securedSource{}
	handler, err := consoleHandler(src, nil, Services{}, auth)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, body, bearer, cookie string
		want                       int
	}{
		{"/api/session", "{}", "wrong", "", 401},
		{"/api/session", "{}", "", auth.session, 401},
		{"/api/command", `{"action":"pause"} {"action":"resume"}`, auth.token, "", 400},
		{"/api/command", `{"action":"pause","unknown":true}`, auth.token, "", 400},
		{"/api/command", `{"action":"pause"}`, "", other.session, 401},
	} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:8000"+tc.path, strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		if tc.bearer != "" {
			r.Header.Set("Authorization", "Bearer "+tc.bearer)
		}
		if tc.cookie != "" {
			r.AddCookie(&http.Cookie{Name: auth.cookie, Value: tc.cookie})
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s status %d, want %d", tc.path, w.Code, tc.want)
		}
	}
	if src.commands.Load() != 0 {
		t.Fatal("rejected body changed state")
	}
	for _, host := range []string{"127.0.0.1:8000@evil.test", "127.0.0.1:8000/", "127.0.0.1:8000?x", "127.0.0.1:8000#x", "localhost:8000", "127.0.0.1:8000.evil.test"} {
		if auth.validHost(host) {
			t.Fatalf("accepted untrusted authority %q", host)
		}
	}
}
