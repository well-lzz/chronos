package nettools

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDoRawHTTPBlocksOutOfScopeRedirect(t *testing.T) {
	destinationURL, closeDestination := serveIPv4(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("should not be reached"))
	}))
	defer closeDestination()

	originURL, closeOrigin := serveIPv4(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(destinationURL, "127.0.0.1", "localhost", 1), http.StatusFound)
	}))
	defer closeOrigin()

	result := DoRawHTTP(context.Background(), originURL, http.MethodGet, nil, "", time.Second,
		func(host string) bool { return host == "127.0.0.1" }, false)
	if !strings.Contains(result, "redirect blocked by scope lock") {
		t.Fatalf("redirect was not blocked: %s", result)
	}
}

func serveIPv4(t *testing.T, handler http.Handler) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listener unavailable: %v", err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	return "http://" + listener.Addr().String(), func() {
		_ = server.Close()
	}
}

func TestDoTCPSendSupportsIPv6(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer listener.Close()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("ok"))
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	result := DoTCPSend("::1", port, nil, time.Second, time.Second)
	if !strings.Contains(result, "ok") {
		t.Fatalf("IPv6 TCP failed: %s", result)
	}
}
