package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoopbackHost(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1", "localhost"} {
		if !loopbackHost(host) {
			t.Errorf("%s should be accepted", host)
		}
	}
	if loopbackHost("0.0.0.0") || loopbackHost("192.0.2.10") {
		t.Fatal("non-loopback host accepted")
	}
}

func TestProtectRejectsCrossOriginWrite(t *testing.T) {
	handler := protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/command", strings.NewReader(`{}`))
	req.Host = "127.0.0.1"
	req.Header.Set("Origin", "https://evil.example")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d", recorder.Code)
	}
}

func TestServeRejectsNonLoopback(t *testing.T) {
	if err := Serve(context.Background(), nil, nil, "0.0.0.0", 0); err == nil {
		t.Fatal("expected non-loopback bind rejection")
	}
}
