package platform

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/well-lzz/chronos/internal/mission"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"

// ErrTaskNotRunnable marks a terminal benchmark-task state: the platform has
// finished/expired the whole task (e.g. "task ... already finished",
// "task_not_found"), so no further start/poll will ever succeed. Callers detect
// it with IsTaskNotRunnable and stop the run cleanly instead of spinning.
var ErrTaskNotRunnable = errors.New("TSec task is not runnable")

// ErrAtCapacity marks a transient "max active challenge instances reached"
// rejection from the platform's start endpoint. It means a slot must be freed
// (a stale container reaped) before a new start can succeed — not that the task
// itself is dead.
var ErrAtCapacity = errors.New("TSec at active-instance capacity")

// IsTaskNotRunnable reports whether err is (or wraps) a terminal task state.
func IsTaskNotRunnable(err error) bool { return errors.Is(err, ErrTaskNotRunnable) }

// IsAtCapacity reports whether err is (or wraps) an active-instance capacity
// rejection, which a stale-container reconcile can recover from.
func IsAtCapacity(err error) bool { return errors.Is(err, ErrAtCapacity) }

// TSec is the TSecBench REST adapter.
type TSec struct {
	BaseURL string
	Token   string

	client *http.Client
	mu     sync.Mutex
	index  map[string]Challenge // by name
	solved map[string]bool
	// unique_code -> addrs for containers we started (avoid duplicate /start).
	started map[string][]string
}

// NewTSec builds a TSec adapter. The HTTP client never uses an env proxy (the
// platform is a direct domestic service reached over the VPN).
func NewTSec(baseURL, token string, insecureTLS bool) *TSec {
	return &TSec{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecureTLS},
				Proxy:           nil,
			},
		},
		index:   map[string]Challenge{},
		solved:  map[string]bool{},
		started: map[string][]string{},
	}
}

func (t *TSec) do(ctx context.Context, method, path string, params url.Values, body any) (json.RawMessage, int, error) {
	u := t.BaseURL + "/" + strings.TrimLeft(path, "/")
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("encode platform request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("BENCHMARK_TOKEN", t.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if readErr != nil {
		return nil, resp.StatusCode, fmt.Errorf("read platform response: %w", readErr)
	}
	if resp.StatusCode >= 400 {
		return data, resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	return data, resp.StatusCode, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// rawChallenge mirrors the platform's challenge JSON.
type rawChallenge struct {
	UniqueCode       string   `json:"unique_code"`
	Name             string   `json:"name"`
	Difficulty       string   `json:"difficulty"`
	Description      string   `json:"description"`
	TotalScore       int      `json:"total_score"`
	Level            int      `json:"level"`
	IsCompleted      bool     `json:"is_completed"`
	FlagCount        int      `json:"flag_count"`
	CorrectFlagCount int      `json:"correct_flag_count"`
	ContainerStatus  string   `json:"container_status"`
	ContainerAddr    []string `json:"container_addr"`
}

type apiErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func parseAPIError(data []byte) apiErrorBody {
	var body apiErrorBody
	_ = json.Unmarshal(data, &body)
	return body
}

func isCapacityError(data []byte) bool {
	body := parseAPIError(data)
	if body.Code != "invalid_state" {
		return false
	}
	message := strings.ToLower(body.Message + " " + string(data))
	for _, marker := range []string{"max active", "capacity", "concurrent", "limit", "最多", "同时启动", "并发", "上限"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func (t *TSec) FetchAllChallenges() ([]Challenge, error) {
	data, _, err := t.do(context.Background(), "GET", "openapi/v1/challenges", nil, nil)
	if err != nil {
		body := strings.ToLower(string(data))
		if strings.Contains(body, "already finished") || strings.Contains(body, "task_not_found") {
			return nil, fmt.Errorf("%w: %s", ErrTaskNotRunnable, truncate(string(data), 300))
		}
		return nil, fmt.Errorf("TSec fetch challenges: %w", err)
	}
	var raw []rawChallenge
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("TSec API returned invalid challenge data: %w", err)
	}
	out := make([]Challenge, 0, len(raw))
	t.mu.Lock()
	defer t.mu.Unlock()
	t.index = make(map[string]Challenge, len(raw))
	t.solved = make(map[string]bool, len(raw))
	for _, r := range raw {
		name := r.Name
		if name == "" {
			name = r.UniqueCode
		}
		fc := r.FlagCount
		if fc == 0 {
			fc = 1
		}
		c := Challenge{
			Name: name, UniqueCode: r.UniqueCode, Category: r.Difficulty, Description: r.Description,
			Value: r.TotalScore, Level: r.Level, FlagCount: fc, CorrectFlagCount: r.CorrectFlagCount,
			IsCompleted: r.IsCompleted, ContainerStatus: r.ContainerStatus, ContainerAddr: r.ContainerAddr,
		}
		out = append(out, c)
		t.index[name] = c
		if r.IsCompleted {
			t.solved[name] = true
		}
	}
	return out, nil
}

func (t *TSec) FetchSolvedNames() (map[string]bool, error) {
	t.mu.Lock()
	empty := len(t.index) == 0
	t.mu.Unlock()
	if empty {
		if _, err := t.FetchAllChallenges(); err != nil {
			return nil, err
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	cp := make(map[string]bool, len(t.solved))
	for k := range t.solved {
		cp[k] = true
	}
	return cp, nil
}

func (t *TSec) FetchPollState() ([]Challenge, map[string]bool, error) {
	ch, err := t.FetchAllChallenges()
	if err != nil {
		return nil, nil, err
	}
	solved, err := t.FetchSolvedNames()
	if err != nil {
		return nil, nil, err
	}
	return ch, solved, nil
}

type submitResp struct {
	Correct          bool `json:"correct"`
	Awarded          int  `json:"awarded"`
	CumulativeScore  int  `json:"cumulative_score"`
	CorrectFlagCount int  `json:"correct_flag_count"`
	TotalFlagCount   int  `json:"total_flag_count"`
}

func (t *TSec) lookup(name string) (Challenge, bool) {
	t.mu.Lock()
	c, ok := t.index[name]
	t.mu.Unlock()
	if !ok {
		_, _ = t.FetchAllChallenges()
		t.mu.Lock()
		c, ok = t.index[name]
		t.mu.Unlock()
	}
	return c, ok
}

func (t *TSec) SubmitFlag(challengeName, flag string) (SubmitResult, error) {
	ch, ok := t.lookup(challengeName)
	if !ok {
		return SubmitResult{Status: "unknown", Message: "not found",
			Display: fmt.Sprintf("UNKNOWN — '%s' not found on platform", challengeName)}, nil
	}
	data, _, err := t.do(context.Background(), "POST", "openapi/v1/challenges/submit", nil,
		map[string]string{"unique_code": ch.UniqueCode, "flag": strings.TrimSpace(flag)})
	if err != nil {
		body := parseAPIError(data)
		switch body.Code {
		case "duplicate":
			if ch.FlagCount > 1 {
				return SubmitResult{Status: "correct", Message: body.Message,
					Display:  fmt.Sprintf("DUPLICATE — %q already banked for %s. Other flags still open.", flag, challengeName),
					Progress: "duplicate of a banked flag"}, nil
			}
			return SubmitResult{Status: "already_solved", Message: body.Message,
				Display: fmt.Sprintf("DUPLICATE — %q already submitted for %s.", flag, challengeName), ChallengeComplete: true}, nil
		case "invalid_state":
			return SubmitResult{Status: "unknown", Message: body.Message, Display: "INVALID_STATE — " + body.Message}, nil
		default:
			msg := body.Message
			if msg == "" {
				msg = err.Error()
			}
			return SubmitResult{Status: "unknown", Message: msg, Display: fmt.Sprintf("SUBMIT ERROR (%s) — %s", body.Code, msg)}, nil
		}
	}
	var r submitResp
	if err := json.Unmarshal(data, &r); err != nil {
		return SubmitResult{}, fmt.Errorf("TSec submit returned invalid data: %w", err)
	}
	if r.Correct {
		complete := r.TotalFlagCount > 0 && r.CorrectFlagCount >= r.TotalFlagCount
		if complete {
			t.mu.Lock()
			t.solved[challengeName] = true
			t.mu.Unlock()
		}
		return SubmitResult{Status: "correct",
			Message:           fmt.Sprintf("awarded=%d cumulative=%d %d/%d", r.Awarded, r.CumulativeScore, r.CorrectFlagCount, r.TotalFlagCount),
			Display:           fmt.Sprintf("CORRECT — %q accepted. +%d pts. Progress: %d/%d.", flag, r.Awarded, r.CorrectFlagCount, r.TotalFlagCount),
			ChallengeComplete: complete, Progress: fmt.Sprintf("%d/%d flags", r.CorrectFlagCount, r.TotalFlagCount)}, nil
	}
	return SubmitResult{Status: "incorrect",
		Message:  fmt.Sprintf("cumulative=%d %d/%d", r.CumulativeScore, r.CorrectFlagCount, r.TotalFlagCount),
		Display:  fmt.Sprintf("INCORRECT — %q rejected. Progress: %d/%d.", flag, r.CorrectFlagCount, r.TotalFlagCount),
		Progress: fmt.Sprintf("%d/%d flags", r.CorrectFlagCount, r.TotalFlagCount)}, nil
}

func (t *TSec) startChallenge(ctx context.Context, uniqueCode string) ([]string, error) {
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		data, status, err := t.do(ctx, "POST", "openapi/v1/challenges/start", url.Values{"unique_code": {uniqueCode}}, nil)
		if err == nil {
			var resp struct {
				ContainerAddr []string `json:"container_addr"`
			}
			if err := json.Unmarshal(data, &resp); err != nil {
				return nil, fmt.Errorf("TSec start returned invalid data: %w", err)
			}
			if len(resp.ContainerAddr) > 0 {
				t.mu.Lock()
				t.started[uniqueCode] = resp.ContainerAddr
				t.mu.Unlock()
			}
			return resp.ContainerAddr, nil
		}
		lastErr = err
		if status == 409 && isCapacityError(data) {
			if !sleepContext(ctx, time.Duration(4+attempt*2)*time.Second) {
				return nil, ctx.Err()
			}
			continue
		}
		return nil, err
	}
	return nil, fmt.Errorf("%w: start %s: %w", ErrAtCapacity, uniqueCode, lastErr)
}

func (t *TSec) closeChallenge(uniqueCode string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return t.closeChallengeContext(ctx, uniqueCode)
}

func (t *TSec) closeChallengeContext(ctx context.Context, uniqueCode string) error {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		data, status, err := t.do(ctx, "POST", "openapi/v1/challenges/close", url.Values{"unique_code": {uniqueCode}}, nil)
		if err == nil {
			t.mu.Lock()
			delete(t.started, uniqueCode)
			t.mu.Unlock()
			return nil
		}
		lastErr = err
		body := parseAPIError(data)
		if status == http.StatusConflict && body.Code == "invalid_state" {
			closed, checkErr := t.containerClosed(ctx, uniqueCode)
			if checkErr == nil && closed {
				t.mu.Lock()
				delete(t.started, uniqueCode)
				t.mu.Unlock()
				return nil
			}
			if checkErr != nil {
				lastErr = fmt.Errorf("%w (container status check: %v)", err, checkErr)
			}
		} else if status != 0 && status < 500 {
			return err
		}
		if attempt == 3 {
			break
		}
		if !sleepContext(ctx, []time.Duration{2, 4, 8}[attempt]*time.Second) {
			return ctx.Err()
		}
	}
	return fmt.Errorf("TSec close %s failed: %w", uniqueCode, lastErr)
}

// A 409 can also mean the container is transitioning. Only the platform's
// observed closed state confirms that it is safe to forget a started container.
func (t *TSec) containerClosed(ctx context.Context, uniqueCode string) (bool, error) {
	data, _, err := t.do(ctx, http.MethodGet, "openapi/v1/challenges", nil, nil)
	if err != nil {
		return false, err
	}
	var challenges []rawChallenge
	if err := json.Unmarshal(data, &challenges); err != nil {
		return false, fmt.Errorf("decode challenge status: %w", err)
	}
	for _, ch := range challenges {
		if ch.UniqueCode == uniqueCode {
			return strings.EqualFold(ch.ContainerStatus, "closed") && len(ch.ContainerAddr) == 0, nil
		}
	}
	return false, fmt.Errorf("challenge %s missing from status response", uniqueCode)
}

func (t *TSec) ReleaseChallenge(challengeName string) bool {
	ch, ok := t.lookup(challengeName)
	if !ok || ch.UniqueCode == "" {
		return false
	}
	if err := t.closeChallenge(ch.UniqueCode); err != nil {
		log.Printf("TSec close failed for %s: %v", challengeName, err)
		return false
	}
	return true
}

func (t *TSec) CloseStaleContainers(keep map[string]bool) []string {
	chs, err := t.FetchAllChallenges()
	if err != nil {
		log.Printf("TSec stale-container check failed: %v", err)
		return nil
	}
	var closed []string
	for _, c := range chs {
		if c.ContainerStatus != "available" || keep[c.Name] || keep[c.UniqueCode] {
			continue
		}
		if err := t.closeChallenge(c.UniqueCode); err == nil {
			closed = append(closed, c.Name)
			log.Printf("Reaped stale container: %s (%s)", c.Name, c.UniqueCode)
		} else {
			log.Printf("TSec stale-container close failed for %s: %v", c.Name, err)
		}
	}
	return closed
}

func (t *TSec) FetchHint(challengeName string) (string, error) {
	ch, ok := t.lookup(challengeName)
	if !ok || ch.UniqueCode == "" {
		return "", nil
	}
	data, _, err := t.do(context.Background(), "GET", "openapi/v1/challenges/hint", url.Values{"unique_code": {ch.UniqueCode}}, nil)
	if err != nil {
		return "", err
	}
	var resp struct {
		Hint string `json:"hint"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Hint, nil
}

// PullChallenge starts the container if needed and returns a mission.Meta with
// the container address(es) in ConnectionInfo. Fails if no address appears.
func (t *TSec) PullChallenge(ctx context.Context, ch Challenge) (mission.Meta, error) {
	addrs := ch.ContainerAddr
	t.mu.Lock()
	if len(addrs) == 0 {
		if a, ok := t.started[ch.UniqueCode]; ok {
			addrs = a
		}
	}
	t.mu.Unlock()

	if len(addrs) == 0 && ch.UniqueCode != "" {
		a, err := t.startChallenge(ctx, ch.UniqueCode)
		if err != nil {
			return mission.Meta{}, err
		}
		if len(a) > 0 {
			addrs = a
		}
		for i := 0; len(addrs) == 0 && i < 10; i++ {
			if !sleepContext(ctx, 3*time.Second) {
				return mission.Meta{}, ctx.Err()
			}
			chs, err := t.FetchAllChallenges()
			if err != nil {
				continue
			}
			for _, c := range chs {
				if c.UniqueCode == ch.UniqueCode && c.ContainerStatus == "available" && len(c.ContainerAddr) > 0 {
					addrs = c.ContainerAddr
					t.mu.Lock()
					t.started[ch.UniqueCode] = addrs
					t.mu.Unlock()
					break
				}
			}
		}
	}
	if len(addrs) == 0 {
		return mission.Meta{}, fmt.Errorf("TSec: container for %s did not become available — spawn aborted, retry later", ch.Name)
	}
	t.mu.Lock()
	t.started[ch.UniqueCode] = append([]string(nil), addrs...)
	t.mu.Unlock()
	return mission.Meta{
		Name: ch.Name, Category: ch.Category, Description: ch.Description,
		ConnectionInfo: strings.Join(addrs, ", "), FlagCount: ch.FlagCount,
	}, nil
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (t *TSec) Close() {
	t.mu.Lock()
	uniqueCodes := make([]string, 0, len(t.started))
	for uniqueCode := range t.started {
		uniqueCodes = append(uniqueCodes, uniqueCode)
	}
	t.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, uniqueCode := range uniqueCodes {
		uniqueCode := uniqueCode
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := t.closeChallengeContext(ctx, uniqueCode); err != nil {
				log.Printf("TSec shutdown close failed for %s: %v", uniqueCode, err)
			}
		}()
	}
	wg.Wait()
}
