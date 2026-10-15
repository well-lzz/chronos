// Package nettools is the sandbox-free network surface: direct HTTP and raw TCP
// to live targets. Scope enforcement lives in the solver (which checks the
// engagement scope before calling these); these functions are pure IO plus
// output truncation.
package nettools

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const maxOutput = 24000

func truncate(s string) string {
	if len(s) <= maxOutput {
		return s
	}
	return s[:maxOutput] + fmt.Sprintf("\n... [truncated, total %d chars]", len(s))
}

// DoRawHTTP sends a full-control HTTP request and returns a verbatim transcript.
func DoRawHTTP(ctx context.Context, url, method string, headers map[string]string, body string, timeout time.Duration) string {
	url = strings.TrimSpace(url)
	if url == "" {
		return "http_request error: empty url"
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "http://" + strings.TrimLeft(url, "/")
	}
	if method == "" {
		method = "GET"
	}
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), url, rdr)
	if err != nil {
		return "http_request error: " + err.Error()
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // targets use self-signed certs
			Proxy:           nil,                                   // never route target traffic through any proxy
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "http_request error: " + err.Error()
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxOutput+1))
	var hb strings.Builder
	for k, vs := range resp.Header {
		for _, v := range vs {
			fmt.Fprintf(&hb, "%s: %s\n", k, v)
		}
	}
	hdr := strings.TrimRight(hb.String(), "\n")
	if hdr == "" {
		hdr = "(none)"
	}
	bodyStr := string(data)
	if bodyStr == "" {
		bodyStr = "(empty)"
	}
	out := fmt.Sprintf("HTTP %d %s\nURL: %s\n--- response headers ---\n%s\n--- response body ---\n%s",
		resp.StatusCode, http.StatusText(resp.StatusCode), resp.Request.URL.String(), hdr, bodyStr)
	return truncate(out)
}

// DoTCPSend sends one payload over raw TCP and returns the reply.
func DoTCPSend(host string, port int, payload []byte, readTimeout, silence time.Duration) string {
	if strings.TrimSpace(host) == "" {
		return "tcp_send error: empty host"
	}
	addr := fmt.Sprintf("%s:%d", host, port)
	conn, err := net.DialTimeout("tcp", addr, readTimeout)
	if err != nil {
		return fmt.Sprintf("tcp_send error: cannot connect to %s — %v", addr, err)
	}
	defer conn.Close()
	if len(payload) > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(readTimeout))
		if _, err := conn.Write(payload); err != nil {
			return "tcp_send error: write failed — " + err.Error()
		}
	}
	var out []byte
	buf := make([]byte, 8192)
	for len(out) < maxOutput {
		_ = conn.SetReadDeadline(time.Now().Add(silence))
		n, err := conn.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if err != nil {
			break // timeout (silence window) or EOF — normal end of reply
		}
	}
	text := string(out)
	if text == "" {
		text = "(no data received — service may expect different input or closed early)"
	}
	prefix := fmt.Sprintf("TCP %s — sent %d bytes, received %d bytes\n--- response ---\n", addr, len(payload), len(out))
	return truncate(prefix + text)
}
