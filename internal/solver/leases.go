package solver

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// canonicalSurface uses host:port for a whole service and the first URL path
// segment for an endpoint family. These keys give the blackboard a structural
// overlap test instead of relying on the model to describe overlap in prose.
func canonicalSurface(raw string) (surface, host string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "mission:*" {
		return raw, "", nil
	}
	if strings.Contains(raw, "://") {
		u, parseErr := url.Parse(raw)
		if parseErr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
			return "", "", fmt.Errorf("surface must be an HTTP(S) URL or host:port")
		}
		host = strings.ToLower(u.Hostname())
		port := u.Port()
		if port == "" {
			if u.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		if n, convErr := strconv.Atoi(port); convErr != nil || n < 1 || n > 65535 {
			return "", "", fmt.Errorf("invalid port")
		}
		surface = net.JoinHostPort(host, port)
		path := strings.Trim(u.EscapedPath(), "/")
		if path != "" {
			surface += "/" + strings.Split(path, "/")[0]
		}
		return surface, host, nil
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil || host == "" {
		return "", "", fmt.Errorf("surface must be an HTTP(S) URL or host:port")
	}
	if n, convErr := strconv.Atoi(port); convErr != nil || n < 1 || n > 65535 {
		return "", "", fmt.Errorf("invalid port")
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	return net.JoinHostPort(host, port), host, nil
}

func (s *Solver) toolClaimSurface(argsJSON string) string {
	var a struct {
		Surface   string `json:"surface"`
		SubgoalID string `json:"subgoal_id"`
		TTL       int    `json:"ttl_seconds"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "claim_surface error: bad args"
	}
	surface, host, err := canonicalSurface(a.Surface)
	if err != nil {
		return "claim_surface error: " + err.Error()
	}
	if surface == "mission:*" {
		if s.scopeSnapshot().IsEmpty() {
			return "claim_surface error: no authorized scope"
		}
	} else if ok, message := s.hostAllowed(host); !ok {
		return "claim_surface error: " + message
	}
	if a.TTL == 0 {
		a.TTL = 120
	}
	lease, err := s.bb.ClaimSurface(surface, s.ModelSpec, a.SubgoalID, time.Duration(a.TTL)*time.Second)
	if err != nil {
		return "claim_surface error: " + err.Error()
	}
	s.rec("claim", surface, fmt.Sprintf("claimed %s until %s", surface, time.Unix(int64(lease.ExpiresAt), 0).UTC().Format(time.RFC3339)))
	return fmt.Sprintf("claimed %s; renew before expiry or release when done", surface)
}

func (s *Solver) toolReleaseSurface(argsJSON string) string {
	var a struct {
		Surface string `json:"surface"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "release_surface error: bad args"
	}
	surface, _, err := canonicalSurface(a.Surface)
	if err != nil {
		return "release_surface error: " + err.Error()
	}
	released, err := s.bb.ReleaseSurface(surface, s.ModelSpec)
	if err != nil {
		return "release_surface error: " + err.Error()
	}
	if !released {
		return "no owned claim for " + surface
	}
	s.rec("release", surface, "released "+surface)
	return "released " + surface
}

func (s *Solver) requireMutationLease(surface string) string {
	if err := s.bb.HoldSurface(surface, s.ModelSpec); err != nil {
		return "【平台管控·非目标防御】BLOCKED (coordination): " + err.Error()
	}
	return ""
}

func (s *Solver) requireHTTPMutationLease(raw, method, body string) string {
	method = strings.ToUpper(strings.TrimSpace(method))
	if (method == "" || method == http.MethodGet || method == http.MethodHead) && body == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	surface, _, err := canonicalSurface(raw)
	if err != nil {
		return "http_request error: " + err.Error()
	}
	return s.requireMutationLease(surface)
}

// Apply the same redirect claim check to HTTP requests and assessment tools.
func (s *Solver) checkHTTPMutationLease(req *http.Request) error {
	if (req.Method == http.MethodGet || req.Method == http.MethodHead) && req.ContentLength <= 0 {
		return nil
	}
	surface, _, err := canonicalSurface(req.URL.String())
	if err != nil {
		return err
	}
	if blocked := s.requireMutationLease(surface); blocked != "" {
		return fmt.Errorf("redirect %s", blocked)
	}
	return nil
}
