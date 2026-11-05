package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// consoleAuth is process-local. Restarting the server invalidates old browser
// sessions; neither the operator token nor session secret is embedded in assets.
type consoleAuth struct {
	host    string
	port    int
	token   string
	session string
	csrf    string
	cookie  string
}

func randomSecret() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}

func newConsoleAuth(host string, port int, token string) (*consoleAuth, error) {
	if token == "" {
		var err error
		token, err = randomSecret()
		if err != nil {
			return nil, err
		}
	}
	if len(token) < 32 || len(token) > 512 {
		return nil, fmt.Errorf("console token must contain 32–512 non-whitespace bytes")
	}
	for _, c := range token {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return nil, fmt.Errorf("console token must use URL-safe letters, digits, _ or -")
		}
	}
	session, err := randomSecret()
	if err != nil {
		return nil, err
	}
	csrf, err := randomSecret()
	if err != nil {
		return nil, err
	}
	return &consoleAuth{host: strings.ToLower(strings.Trim(host, "[]")), port: port, token: token, session: session, csrf: csrf, cookie: "chronos_session_" + strconv.Itoa(port)}, nil
}

func secretEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (a *consoleAuth) validHost(authority string) bool {
	u, err := url.Parse("http://" + authority)
	if err != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Host != authority {
		return false
	}
	port := 80
	if u.Port() != "" {
		var err error
		port, err = strconv.Atoi(u.Port())
		if err != nil {
			return false
		}
	}
	return port == a.port && strings.EqualFold(u.Hostname(), a.host) && loopbackHost(u.Hostname())
}

func (a *consoleAuth) bearer(r *http.Request) bool {
	scheme, value, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	return ok && strings.EqualFold(scheme, "Bearer") && secretEqual(value, a.token)
}

func (a *consoleAuth) authenticated(r *http.Request) bool {
	if a.bearer(r) {
		return true
	}
	cookie, err := r.Cookie(a.cookie)
	return err == nil && secretEqual(cookie.Value, a.session)
}

func (a *consoleAuth) sessionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "GET or POST only", 405)
		return
	}
	if r.Method == http.MethodPost {
		// Bootstrap is the only write exempt from the cookie CSRF token. It
		// always requires the operator's bearer token, never just a cookie.
		if !a.bearer(r) {
			http.Error(w, "operator token required", 401)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: a.cookie, Value: a.session, Path: "/api/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
	}
	writeJSON(w, map[string]any{"ok": true, "csrf": a.csrf})
}

func (a *consoleAuth) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'; base-uri 'self'; object-src 'none'")
		if !a.validHost(r.Host) {
			http.Error(w, "untrusted Host", 403)
			return
		}
		api := strings.HasPrefix(r.URL.Path, "/api/")
		unsafe := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
		if api || unsafe {
			if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
				http.Error(w, "cross-origin request blocked", 403)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				scheme := "http"
				if r.TLS != nil {
					scheme = "https"
				}
				if err != nil || u.Scheme != scheme || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || !a.validHost(u.Host) {
					http.Error(w, "cross-origin request blocked", 403)
					return
				}
			}
		}
		if api {
			if !a.authenticated(r) {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "console authentication required", 401)
				return
			}
			if unsafe {
				mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
				if err != nil || mediaType != "application/json" {
					http.Error(w, "application/json required", 415)
					return
				}
				if r.URL.Path != "/api/session" && !a.bearer(r) && !secretEqual(r.Header.Get("X-Chronos-CSRF"), a.csrf) {
					http.Error(w, "CSRF token required", 403)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *consoleAuth) accessURL() string {
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(a.host, strconv.Itoa(a.port)), Path: "/", Fragment: "token=" + a.token}
	return u.String()
}
