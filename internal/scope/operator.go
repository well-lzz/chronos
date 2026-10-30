package scope

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var operatorHost = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)

// ParseOperatorTargets validates an operator's complete replacement boundary.
// Unlike Parse, it rejects malformed entries instead of silently dropping them.
func ParseOperatorTargets(targets []string) (Scope, error) {
	if len(targets) > 64 {
		return Scope{}, fmt.Errorf("scope contains too many entries (maximum 64)")
	}
	clean := make([]string, 0, len(targets))
	for _, raw := range targets {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if len(entry) > 512 {
			return Scope{}, fmt.Errorf("scope entry is too long")
		}
		if strings.ContainsAny(entry, " \t\r\n") {
			return Scope{}, fmt.Errorf("invalid scope entry %q: use one target per line", entry)
		}
		if strings.Contains(entry, "/") && !strings.Contains(entry, "://") {
			if _, err := netip.ParsePrefix(entry); err != nil {
				return Scope{}, fmt.Errorf("invalid CIDR %q", entry)
			}
		} else if strings.Contains(entry, "://") {
			u, err := url.Parse(entry)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
				return Scope{}, fmt.Errorf("invalid HTTP(S) scope URL %q", entry)
			}
			if err := validateOperatorHost(u.Hostname()); err != nil {
				return Scope{}, fmt.Errorf("invalid scope URL host %q", entry)
			}
			if u.Port() != "" && !validOperatorPort(u.Port()) {
				return Scope{}, fmt.Errorf("invalid scope URL port %q", entry)
			}
		} else {
			host := entry
			if parsed, port, err := net.SplitHostPort(entry); err == nil {
				if !validOperatorPort(port) {
					return Scope{}, fmt.Errorf("invalid scope host or port %q", entry)
				}
				host = parsed
			} else if strings.Count(entry, ":") == 1 {
				parts := strings.SplitN(entry, ":", 2)
				if !validOperatorPort(parts[1]) {
					return Scope{}, fmt.Errorf("invalid scope host or port %q", entry)
				}
				host = parts[0]
			}
			if err := validateOperatorHost(strings.Trim(host, "[]")); err != nil {
				return Scope{}, fmt.Errorf("invalid scope host %q", entry)
			}
		}
		clean = append(clean, entry)
	}
	if len(clean) == 0 {
		return Scope{}, fmt.Errorf("scope must contain at least one host, IP, URL, or CIDR")
	}
	parsed := Parse(clean)
	if parsed.IsEmpty() {
		return Scope{}, fmt.Errorf("scope contains an unusable target")
	}
	return parsed, nil
}

func validOperatorPort(raw string) bool {
	port, err := strconv.Atoi(raw)
	return err == nil && port >= 1 && port <= 65535
}

func validateOperatorHost(host string) error {
	if host == "" {
		return fmt.Errorf("empty host")
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return nil
	}
	if net.ParseIP(host) != nil || !operatorHost.MatchString(host) || strings.Contains(host, "..") {
		return fmt.Errorf("invalid host")
	}
	return nil
}
