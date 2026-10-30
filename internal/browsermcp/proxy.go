package browsermcp

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Proxy keeps browser HTTP, HTTPS and WebSocket connections on the mission's
// current authorization boundary, including after the operator narrows scope.
type Proxy struct {
	server *http.Server
	ln     net.Listener
}

func StartProxy(allowed func(string) bool) (*Proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{ln: ln}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, ResponseHeaderTimeout: 30 * time.Second}
	p.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			host, _, err := net.SplitHostPort(r.Host)
			if err != nil || !allowed(host) {
				http.Error(w, "target outside authorized scope", http.StatusForbidden)
				return
			}
			upstream, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(r.Context(), "tcp", r.Host)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			client, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				upstream.Close()
				return
			}
			_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
			go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close() }()
			_, _ = io.Copy(client, upstream)
			_ = client.Close()
			return
		}
		if r.URL == nil || !r.URL.IsAbs() || (r.URL.Scheme != "http" && r.URL.Scheme != "https") || !allowed(r.URL.Hostname()) {
			http.Error(w, "target outside authorized scope", http.StatusForbidden)
			return
		}
		out := r.Clone(r.Context())
		out.RequestURI = ""
		out.Header.Del("Proxy-Authorization")
		resp, err := transport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})}
	go func() { _ = p.server.Serve(ln) }()
	return p, nil
}

func (p *Proxy) URL() string { return "http://" + p.ln.Addr().String() }

func (p *Proxy) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = p.server.Shutdown(ctx)
}

func CheckURL(raw string, allowed func(string) bool) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("browser navigation requires an HTTP(S) URL")
	}
	if !allowed(strings.Trim(u.Hostname(), "[]")) {
		return fmt.Errorf("browser target %s outside authorized scope", u.Hostname())
	}
	return nil
}
