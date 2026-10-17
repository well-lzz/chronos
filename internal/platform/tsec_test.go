package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testJSONResponse(status int, value any) *http.Response {
	data, _ := json.Marshal(value)
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(data)),
	}
}

func TestTSecAPIContractAndShutdownCleanup(t *testing.T) {
	var mu sync.Mutex
	started := false
	closeCalls := 0
	badToken := false
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("BENCHMARK_TOKEN") != "test-token" {
			badToken = true
			return testJSONResponse(http.StatusNotFound, apiErrorBody{Code: "task_not_found"}), nil
		}
		switch r.URL.Path {
		case "/openapi/v1/challenges":
			challenge := map[string]any{
				"unique_code": "web-1", "name": "Web One", "difficulty": "easy",
				"description": nil, "total_score": 100, "level": 1, "flag_count": 1,
				"correct_flag_count": 0, "is_completed": false, "container_status": "closed",
				"container_addr": []string{},
			}
			if started {
				challenge["container_status"] = "available"
				challenge["container_addr"] = []string{"10.0.0.8:8080"}
			}
			return testJSONResponse(http.StatusOK, []any{challenge}), nil
		case "/openapi/v1/challenges/start":
			if r.Method != http.MethodPost || r.URL.Query().Get("unique_code") != "web-1" {
				return testJSONResponse(http.StatusBadRequest, apiErrorBody{Code: "bad_request"}), nil
			}
			started = true
			return testJSONResponse(http.StatusOK, map[string]any{"unique_code": "web-1", "container_addr": []string{"10.0.0.8:8080"}}), nil
		case "/openapi/v1/challenges/submit":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["unique_code"] != "web-1" || body["flag"] != "flag{ok}" {
				return testJSONResponse(http.StatusBadRequest, apiErrorBody{Code: "bad_request"}), nil
			}
			return testJSONResponse(http.StatusOK, map[string]any{
				"correct": true, "awarded": 100, "cumulative_score": 100,
				"correct_flag_count": 1, "total_flag_count": 1, "matched_flag_index": 0,
			}), nil
		case "/openapi/v1/challenges/close":
			closeCalls++
			started = false
			return testJSONResponse(http.StatusOK, map[string]any{"unique_code": "web-1", "closed": true}), nil
		default:
			return testJSONResponse(http.StatusNotFound, apiErrorBody{Code: "not_found"}), nil
		}
	})

	adapter := NewTSec("https://benchmark.example", "test-token", false)
	adapter.client.Transport = transport
	challenges, err := adapter.FetchAllChallenges()
	if err != nil || len(challenges) != 1 {
		t.Fatalf("FetchAllChallenges() = %v, %v", challenges, err)
	}
	meta, err := adapter.PullChallenge(context.Background(), challenges[0])
	if err != nil {
		t.Fatal(err)
	}
	if meta.ConnectionInfo != "10.0.0.8:8080" {
		t.Fatalf("connection info = %q", meta.ConnectionInfo)
	}
	result, err := adapter.SubmitFlag("Web One", " flag{ok} ")
	if err != nil || !result.Confirmed() || !result.ChallengeComplete {
		t.Fatalf("SubmitFlag() = %+v, %v", result, err)
	}

	adapter.Close()
	mu.Lock()
	defer mu.Unlock()
	if badToken {
		t.Fatal("adapter did not send BENCHMARK_TOKEN")
	}
	if closeCalls != 1 {
		t.Fatalf("shutdown close calls = %d, want 1", closeCalls)
	}
}

func TestCapacityErrorSupportsHostedAPIMessage(t *testing.T) {
	for _, body := range []string{
		`{"code":"invalid_state","message":"max active challenge limit reached"}`,
		`{"code":"invalid_state","message":"最多同时启动 3 道题目"}`,
	} {
		if !isCapacityError([]byte(body)) {
			t.Fatalf("expected capacity error for %s", body)
		}
	}
	if isCapacityError([]byte(`{"code":"invalid_state","message":"task expired"}`)) {
		t.Fatal("task expiration must not be retried as capacity pressure")
	}
}
