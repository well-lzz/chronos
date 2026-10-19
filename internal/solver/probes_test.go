package solver

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/well-lzz/chronos/internal/scope"
)

func TestParseIPv4(t *testing.T) {
	ip := parseIPv4("10.0.0.5")
	if ip == nil || ip[0] != 10 || ip[3] != 5 {
		t.Fatalf("parseIPv4(10.0.0.5) = %v", ip)
	}
	for _, bad := range []string{"10.0.0", "10.0.0.256", "a.b.c.d", "10.0.0.5.6", ""} {
		if parseIPv4(bad) != nil {
			t.Fatalf("parseIPv4(%q) should be nil", bad)
		}
	}
}

func TestSSRFVariantsEncodeHostOnly(t *testing.T) {
	vs := ssrfVariants("http://10.0.0.5:8000/fetch")
	got := map[string]string{}
	for _, v := range vs {
		got[v.Family] = v.URL
	}
	// 10.0.0.5 -> decimal 167772165
	if !strings.Contains(got["decimal-ip"], "167772165") {
		t.Fatalf("decimal-ip = %q", got["decimal-ip"])
	}
	if !strings.Contains(got["hex-dword"], "0x0a000005") {
		t.Fatalf("hex-dword = %q", got["hex-dword"])
	}
	// port and path must be preserved across every variant
	for fam, u := range got {
		if !strings.Contains(u, ":8000") || !strings.HasSuffix(u, "/fetch") {
			t.Fatalf("variant %s dropped port/path: %q", fam, u)
		}
	}
	if _, ok := got["userinfo-confusion"]; !ok {
		t.Fatal("missing userinfo-confusion variant")
	}
}

func TestXSSVariantsCarryDistinctSurvivors(t *testing.T) {
	vs := xssVariants("MARK")
	seen := map[string]bool{}
	for _, v := range vs {
		if !strings.Contains(v.Wire, "MARK") {
			t.Fatalf("variant %s wire lacks marker: %q", v.Family, v.Wire)
		}
		if !strings.Contains(v.Wire, v.Survive) && v.Survive != "" {
			// survive must be a structural subset actually present in the payload
			t.Fatalf("variant %s: survive %q not contained in wire %q", v.Family, v.Survive, v.Wire)
		}
		if seen[v.Family] {
			t.Fatalf("duplicate family %s", v.Family)
		}
		seen[v.Family] = true
	}
	if len(vs) < 6 {
		t.Fatalf("expected a breadth of breakout families, got %d", len(vs))
	}
}

func TestLFIVariantsIncludeKeyFamilies(t *testing.T) {
	vs := lfiVariants("/etc/passwd", "", "")
	fams := map[string]string{}
	for _, v := range vs {
		fams[v.Family] = v.Wire
	}
	if _, ok := fams["php-filter-b64"]; !ok {
		t.Fatal("missing php-filter-b64 family")
	}
	if w := fams["traversal-dblenc-6"]; !strings.Contains(w, "..%252f") {
		t.Fatalf("double-encode family wrong: %q", w)
	}
	if w := fams["traversal-6"]; !strings.HasPrefix(w, "../../../") {
		t.Fatalf("traversal-6 wrong: %q", w)
	}
}

func TestLFISuccessDetectsPasswd(t *testing.T) {
	resp := "HTTP/1.1 200 OK\n\nroot:x:0:0:root:/root:/bin/bash\ndaemon:x:1:1:..."
	ok, ex := lfiSuccess(resp, "/etc/passwd", false)
	if !ok || !strings.Contains(ex, "root:") {
		t.Fatalf("passwd not detected: ok=%v ex=%q", ok, ex)
	}
	if ok, _ := lfiSuccess("HTTP/1.1 200 OK\n\nnothing here", "/etc/passwd", false); ok {
		t.Fatal("false positive on benign body")
	}
}

func TestLFISuccessDecodesPHPFilterSource(t *testing.T) {
	src := "<?php $flag = getenv('FLAG'); echo 'ok'; ?>"
	b64 := base64.StdEncoding.EncodeToString([]byte(src))
	resp := "HTTP/1.1 200 OK\n\n" + b64
	ok, ex := lfiSuccess(resp, "/var/www/app.php", true)
	if !ok || !strings.Contains(ex, "<?php") {
		t.Fatalf("php source not decoded: ok=%v ex=%q", ok, ex)
	}
}

func TestLooksLikeSource(t *testing.T) {
	if !looksLikeSource([]byte("<?php echo 1;")) {
		t.Fatal("php source should be recognized")
	}
	if looksLikeSource([]byte{0x00, 0x01, 0x02, 0xff, 0xfe}) {
		t.Fatal("binary garbage should not look like source")
	}
}

func TestSSRFProbeRefusesOutOfScopeTarget(t *testing.T) {
	s := &Solver{scope: scope.Parse([]string{"10.0.0.0/24"})}
	out := s.toolSSRFProbe(nil, `{"url":"http://10.0.0.9/fetch","param":"u","target":"http://8.8.8.8/"}`)
	if !strings.Contains(out, "refused") || !strings.Contains(out, "scope") {
		t.Fatalf("ssrf_probe must refuse an out-of-scope target, got %q", out)
	}
}
