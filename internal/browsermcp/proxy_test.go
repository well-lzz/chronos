package browsermcp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestProxyEnforcesCurrentScope(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "authorized") }))
	defer target.Close()
	allowed := true
	proxy, err := StartProxy(func(host string) bool { return allowed && host == "127.0.0.1" })
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	get := func(raw string) (int, string) {
		resp, err := client.Get(raw)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if status, body := get(target.URL); status != 200 || body != "authorized" {
		t.Fatalf("allowed: %d %q", status, body)
	}
	allowed = false
	if status, body := get(target.URL); status != 403 || !strings.Contains(body, "outside authorized scope") {
		t.Fatalf("narrowed: %d %q", status, body)
	}
	if status, _ := get("http://outside.invalid/"); status != 403 {
		t.Fatalf("out of scope: %d", status)
	}
}

func TestCheckURL(t *testing.T) {
	allow := func(host string) bool { return host == "target.test" }
	for _, raw := range []string{"file:///etc/passwd", "https://outside.test", "https://user@target.test/"} {
		if CheckURL(raw, allow) == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	if err := CheckURL("https://target.test/path", allow); err != nil {
		t.Fatal(err)
	}
}
