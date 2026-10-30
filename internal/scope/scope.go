// Package scope is the single, structural authorization boundary.
//
// Every mission runs inside an explicit scope: the hosts and IP ranges the
// agent is authorized to touch. The solver's own egress (http_request /
// tcp_send) is restricted to this set with no bypass — the target lock,
// generalized from a flat host list to host + CIDR. An empty scope allows
// nothing (fail-loud). Hostnames match by exact (lowercased) string; IP/CIDR
// entries match numeric IPs. No DNS resolution happens in the guard.
package scope

import (
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

var schemeRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)

// Scope is an immutable set of authorized hosts plus IP networks.
type Scope struct {
	hosts map[string]struct{}
	nets  []netip.Prefix
	raw   []string
}

// Parse builds a Scope from a connection_info string ("ip:port, ip:port") or a
// list of entries (hosts, IPs, CIDRs). Unparsable tokens are skipped.
func Parse(entries []string) Scope {
	s := Scope{hosts: map[string]struct{}{}}
	seen := map[string]struct{}{}
	for _, tok := range entries {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		body := schemeRe.ReplaceAllString(tok, "")
		// CIDR? left is an IP, right an integer prefix.
		if i := strings.IndexByte(body, '/'); i >= 0 {
			left, right := body[:i], body[i+1:]
			if isAllDigits(right) && looksLikeIP(left) {
				if p, err := netip.ParsePrefix(body); err == nil {
					s.nets = append(s.nets, p.Masked())
					addRaw(&s.raw, seen, tok)
					continue
				}
			}
		}
		if h := normalizeHost(tok); h != "" {
			s.hosts[h] = struct{}{}
			addRaw(&s.raw, seen, tok)
		}
	}
	return s
}

// ParseConnInfo splits a connection_info string on commas/whitespace, then Parse.
func ParseConnInfo(connInfo string) Scope {
	return Parse(regexp.MustCompile(`[,\s]+`).Split(connInfo, -1))
}

func (s Scope) IsEmpty() bool { return len(s.hosts) == 0 && len(s.nets) == 0 }

// Allows reports whether host is authorized. Empty scope allows nothing.
func (s Scope) Allows(host string) bool {
	h := strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	if h == "" {
		return false
	}
	if _, ok := s.hosts[h]; ok {
		return true
	}
	if len(s.nets) > 0 {
		if ip, err := netip.ParseAddr(h); err == nil {
			for _, n := range s.nets {
				if n.Contains(ip) {
					return true
				}
			}
		}
	}
	return false
}

// Describe returns the human-readable target list for block messages / logs.
func (s Scope) Describe() string {
	if len(s.raw) == 0 {
		return "(none)"
	}
	return strings.Join(s.raw, ", ")
}

// Entries returns the operator-provided scope entries in their original order.
// The returned slice is a copy and can be safely exposed to the console.
func (s Scope) Entries() []string {
	return append([]string(nil), s.raw...)
}

// SameTargets compares the actual host and network boundary, ignoring entry
// order and URL presentation differences.
func (s Scope) SameTargets(other Scope) bool {
	if len(s.hosts) != len(other.hosts) || len(s.nets) != len(other.nets) {
		return false
	}
	for host := range s.hosts {
		if _, ok := other.hosts[host]; !ok {
			return false
		}
	}
	a, b := s.CIDRStrings(), other.CIDRStrings()
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// SubsetOf checks the entire child scope, including CIDR ranges, against an
// authorization scope. A child may narrow a network but never widen it.
func (s Scope) SubsetOf(parent Scope) bool {
	for host := range s.hosts {
		if !parent.Allows(host) {
			return false
		}
	}
	for _, child := range s.nets {
		allowed := false
		for _, p := range parent.nets {
			if p.Contains(child.Addr()) && child.Bits() >= p.Bits() {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	return true
}

// Within returns only entries of s still wholly authorized by parent. It is
// used when an operator narrows scope while a delegated child is running.
func (s Scope) Within(parent Scope) Scope {
	var entries []string
	for _, entry := range s.raw {
		candidate := Parse([]string{entry})
		if !candidate.IsEmpty() && candidate.SubsetOf(parent) {
			entries = append(entries, entry)
		}
	}
	return Parse(entries)
}

// CIDRStrings returns the authorized networks as strings.
func (s Scope) CIDRStrings() []string {
	out := make([]string, 0, len(s.nets))
	for _, n := range s.nets {
		out = append(out, n.String())
	}
	return out
}

// Hosts returns the literal authorized hostnames/IPs.
func (s Scope) Hosts() []string {
	out := make([]string, 0, len(s.hosts))
	for h := range s.hosts {
		out = append(out, h)
	}
	return out
}

func normalizeHost(token string) string {
	t := strings.TrimSpace(token)
	if schemeRe.MatchString(t) {
		u, err := url.Parse(t)
		if err != nil {
			return ""
		}
		return strings.ToLower(u.Hostname())
	}
	if i := strings.IndexByte(t, '/'); i >= 0 {
		t = t[:i] // drop any path
	}
	if host, _, err := net.SplitHostPort(t); err == nil {
		t = host
	}
	t = strings.Trim(t, "[]")
	// Strip a trailing :port only for a single-colon (IPv4/hostname) value.
	if strings.Count(t, ":") == 1 {
		t = t[:strings.LastIndexByte(t, ':')]
	}
	return strings.ToLower(t)
}

func looksLikeIP(s string) bool {
	return net.ParseIP(s) != nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func addRaw(raw *[]string, seen map[string]struct{}, tok string) {
	if _, ok := seen[tok]; ok {
		return
	}
	seen[tok] = struct{}{}
	*raw = append(*raw, tok)
}
