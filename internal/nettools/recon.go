package nettools

// recon.go adds the non-HTTP reconnaissance primitives: DNS resolution, a TCP
// connect port scan with light banner grabbing, and TLS inspection. Like the
// rest of nettools these are pure IO plus output bounding — scope enforcement
// lives in the solver, which checks the engagement scope before calling them.

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- DNS ------------------------------------------------------------------

// DNSLookup resolves a host (forward records) or an IP (reverse) and returns a
// readable transcript. Uses the system resolver with a bounded deadline.
func DNSLookup(ctx context.Context, host string, timeout time.Duration) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return "dns_lookup error: empty host"
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r := net.DefaultResolver
	var b strings.Builder
	fmt.Fprintf(&b, "DNS %s\n", host)

	if ip := net.ParseIP(host); ip != nil {
		names, err := r.LookupAddr(ctx, host)
		if err != nil {
			fmt.Fprintf(&b, "PTR: (none) — %v\n", err)
		} else {
			fmt.Fprintf(&b, "PTR: %s\n", strings.Join(names, ", "))
		}
		return truncate(b.String())
	}

	if addrs, err := r.LookupIPAddr(ctx, host); err == nil {
		var v4, v6 []string
		for _, a := range addrs {
			if a.IP.To4() != nil {
				v4 = append(v4, a.IP.String())
			} else {
				v6 = append(v6, a.IP.String())
			}
		}
		if len(v4) > 0 {
			fmt.Fprintf(&b, "A: %s\n", strings.Join(v4, ", "))
		}
		if len(v6) > 0 {
			fmt.Fprintf(&b, "AAAA: %s\n", strings.Join(v6, ", "))
		}
	} else {
		fmt.Fprintf(&b, "A/AAAA: (none) — %v\n", err)
	}
	if cname, err := r.LookupCNAME(ctx, host); err == nil && !strings.EqualFold(strings.TrimSuffix(cname, "."), host) {
		fmt.Fprintf(&b, "CNAME: %s\n", cname)
	}
	if mxs, err := r.LookupMX(ctx, host); err == nil && len(mxs) > 0 {
		var m []string
		for _, mx := range mxs {
			m = append(m, fmt.Sprintf("%s(%d)", strings.TrimSuffix(mx.Host, "."), mx.Pref))
		}
		fmt.Fprintf(&b, "MX: %s\n", strings.Join(m, ", "))
	}
	if ns, err := r.LookupNS(ctx, host); err == nil && len(ns) > 0 {
		var n []string
		for _, s := range ns {
			n = append(n, strings.TrimSuffix(s.Host, "."))
		}
		fmt.Fprintf(&b, "NS: %s\n", strings.Join(n, ", "))
	}
	if txt, err := r.LookupTXT(ctx, host); err == nil && len(txt) > 0 {
		fmt.Fprintf(&b, "TXT: %s\n", strings.Join(txt, " | "))
	}
	return truncate(b.String())
}

// --- Port scan ------------------------------------------------------------

const (
	maxScanPorts  = 4096
	scanWorkers   = 128
	defaultScanTO = 1200 * time.Millisecond
)

// topPorts is the default set when the caller gives no port list.
var topPorts = []int{
	21, 22, 23, 25, 53, 80, 110, 111, 135, 139, 143, 389, 443, 445, 465, 587,
	993, 995, 1433, 1521, 1723, 2049, 2375, 2376, 3000, 3306, 3389, 4444, 5000,
	5432, 5601, 5672, 5900, 5985, 6379, 7001, 8000, 8008, 8080, 8081, 8088, 8443,
	8888, 9000, 9092, 9200, 9300, 11211, 15672, 27017, 50070,
}

// ParsePorts turns "80,443,8000-8010" into a sorted, de-duplicated, capped list.
// Empty input yields the default top-ports set.
func ParsePorts(spec string) ([]int, string) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		out := append([]int(nil), topPorts...)
		return out, ""
	}
	seen := map[int]bool{}
	add := func(p int) {
		if p >= 1 && p <= 65535 {
			seen[p] = true
		}
	}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			a, e1 := strconv.Atoi(strings.TrimSpace(lo))
			b, e2 := strconv.Atoi(strings.TrimSpace(hi))
			if e1 != nil || e2 != nil {
				return nil, "bad port range: " + part
			}
			if a > b {
				a, b = b, a
			}
			for p := a; p <= b && len(seen) <= maxScanPorts; p++ {
				add(p)
			}
		} else {
			p, err := strconv.Atoi(part)
			if err != nil {
				return nil, "bad port: " + part
			}
			add(p)
		}
	}
	out := make([]int, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Ints(out)
	if len(out) > maxScanPorts {
		out = out[:maxScanPorts]
	}
	return out, ""
}

type scanResult struct {
	port   int
	banner string
}

// PortScan runs a bounded TCP connect scan and grabs a short banner from each
// open port. Returns a readable transcript of open ports.
func PortScan(ctx context.Context, host string, ports []int, timeout time.Duration) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return "port_scan error: empty host"
	}
	if len(ports) == 0 {
		ports = append([]int(nil), topPorts...)
	}
	if timeout <= 0 {
		timeout = defaultScanTO
	}
	sem := make(chan struct{}, scanWorkers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var open []scanResult

	for _, p := range ports {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(port int) {
			defer wg.Done()
			defer func() { <-sem }()
			addr := net.JoinHostPort(host, strconv.Itoa(port))
			d := net.Dialer{Timeout: timeout}
			conn, err := d.DialContext(ctx, "tcp", addr)
			if err != nil {
				return
			}
			defer conn.Close()
			banner := grabBanner(conn)
			mu.Lock()
			open = append(open, scanResult{port: port, banner: banner})
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	sort.Slice(open, func(i, j int) bool { return open[i].port < open[j].port })
	var b strings.Builder
	fmt.Fprintf(&b, "port_scan %s — %d ports probed, %d open\n", host, len(ports), len(open))
	if len(open) == 0 {
		b.WriteString("(no open ports in the probed set)\n")
	}
	for _, r := range open {
		if r.banner != "" {
			fmt.Fprintf(&b, "%d/tcp open  %s\n", r.port, r.banner)
		} else {
			fmt.Fprintf(&b, "%d/tcp open\n", r.port)
		}
	}
	return truncate(b.String())
}

// grabBanner reads a short, printable banner from an open connection without
// sending anything (many services greet on connect). Best-effort, bounded.
func grabBanner(conn net.Conn) string {
	_ = conn.SetReadDeadline(time.Now().Add(600 * time.Millisecond))
	buf := make([]byte, 160)
	n, _ := conn.Read(buf)
	if n <= 0 {
		return ""
	}
	s := strings.TrimSpace(string(buf[:n]))
	// collapse to one printable line
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 32 || r > 126 {
			return '.'
		}
		return r
	}, s)
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// --- TLS inspection -------------------------------------------------------

var tlsVersionName = map[uint16]string{
	tls.VersionTLS10: "TLS 1.0", tls.VersionTLS11: "TLS 1.1",
	tls.VersionTLS12: "TLS 1.2", tls.VersionTLS13: "TLS 1.3",
}

// TLSInspect performs a TLS handshake (accepting any certificate so it can
// report on bad ones) and summarizes version, cipher, and the leaf certificate,
// flagging weak or expired configurations.
func TLSInspect(ctx context.Context, host string, port int, timeout time.Duration) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return "tls_inspect error: empty host"
	}
	if port == 0 {
		port = 443
	}
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	d := tls.Dialer{
		NetDialer: &net.Dialer{Timeout: timeout},
		Config:    &tls.Config{InsecureSkipVerify: true, ServerName: host},
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Sprintf("tls_inspect error: handshake with %s failed — %v", addr, err)
	}
	defer conn.Close()
	st := conn.(*tls.Conn).ConnectionState()

	var b strings.Builder
	ver := tlsVersionName[st.Version]
	if ver == "" {
		ver = fmt.Sprintf("0x%04x", st.Version)
	}
	fmt.Fprintf(&b, "tls_inspect %s\nnegotiated: %s, cipher %s\n", addr, ver, tls.CipherSuiteName(st.CipherSuite))

	var weak []string
	if st.Version < tls.VersionTLS12 {
		weak = append(weak, "obsolete protocol "+ver)
	}
	if len(st.PeerCertificates) > 0 {
		c := st.PeerCertificates[0]
		fmt.Fprintf(&b, "subject: %s\nissuer: %s\n", c.Subject.String(), c.Issuer.String())
		if len(c.DNSNames) > 0 {
			fmt.Fprintf(&b, "SAN: %s\n", strings.Join(c.DNSNames, ", "))
		}
		fmt.Fprintf(&b, "valid: %s → %s\nsig: %s\n",
			c.NotBefore.UTC().Format("2006-01-02"), c.NotAfter.UTC().Format("2006-01-02"), c.SignatureAlgorithm)
		now := time.Now()
		if now.Before(c.NotBefore) {
			weak = append(weak, "certificate not yet valid")
		}
		if now.After(c.NotAfter) {
			weak = append(weak, "certificate expired")
		}
		if c.Subject.String() == c.Issuer.String() {
			weak = append(weak, "self-signed certificate")
		}
		switch c.SignatureAlgorithm {
		case 0: // UnknownSignatureAlgorithm
		default:
			if sa := c.SignatureAlgorithm.String(); strings.Contains(sa, "MD5") || strings.Contains(sa, "SHA1") {
				weak = append(weak, "weak signature "+sa)
			}
		}
		// hostname match (informational)
		if err := c.VerifyHostname(host); err != nil {
			weak = append(weak, "hostname mismatch ("+host+")")
		}
	} else {
		b.WriteString("(no peer certificate presented)\n")
	}
	if len(weak) > 0 {
		fmt.Fprintf(&b, "WEAK: %s\n", strings.Join(weak, "; "))
	}
	return truncate(b.String())
}
