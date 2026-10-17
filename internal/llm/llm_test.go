package llm

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func response(status int) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}
}

func TestKeyPoolRoundRobin(t *testing.T) {
	var authorizations []string
	pool := &keyPoolDoer{
		base: doerFunc(func(req *http.Request) (*http.Response, error) {
			authorizations = append(authorizations, req.Header.Get("Authorization"))
			return response(http.StatusOK), nil
		}),
		keys: []string{"key-one", "key-two"},
	}
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest(http.MethodPost, "https://example.test/chat", bytes.NewBufferString("{}"))
		resp, err := pool.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	want := []string{"Bearer key-one", "Bearer key-two", "Bearer key-one"}
	for i := range want {
		if authorizations[i] != want[i] {
			t.Fatalf("authorization[%d] = %q, want %q", i, authorizations[i], want[i])
		}
	}
}

func TestKeyPoolFailsOverOnRateLimit(t *testing.T) {
	var authorizations, bodies []string
	pool := &keyPoolDoer{
		base: doerFunc(func(req *http.Request) (*http.Response, error) {
			authorizations = append(authorizations, req.Header.Get("Authorization"))
			body, _ := io.ReadAll(req.Body)
			bodies = append(bodies, string(body))
			if len(authorizations) == 1 {
				return response(http.StatusTooManyRequests), nil
			}
			return response(http.StatusOK), nil
		}),
		keys: []string{"key-one", "key-two"},
	}
	req, _ := http.NewRequest(http.MethodPost, "https://example.test/chat", bytes.NewBufferString(`{"messages":[]}`))
	resp, err := pool.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(authorizations) != 2 || authorizations[0] != "Bearer key-one" || authorizations[1] != "Bearer key-two" {
		t.Fatalf("failover authorizations = %#v", authorizations)
	}
	if len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Fatalf("request body was not replayed safely: %#v", bodies)
	}
}
