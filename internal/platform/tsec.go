package platform

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
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
func NewTSec(baseURL, token string) *TSec {
	return &TSec{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
				Proxy:           nil,
			},
		},
		index:   map[string]Challenge{},
		solved:  map[string]bool{},
		started: map[string][]string{},
	}
}

func (t *TSec) do(method, path string, params url.Values, body any) (json.RawMessage, int, error) {
	u := t.BaseURL + "/" + strings.TrimLeft(path, "/")
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, rdr)
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
	data, _ := io.ReadAll(resp.Body)
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

func (t *TSec) FetchAllChallenges() ([]Challenge, error) {
	data, _, err := t.do("GET", "openapi/v1/challenges", nil, nil)
	if err != nil {
		body := strings.ToLower(string(data))
		if strings.Contains(body, "already finished") || strings.Contains(body, "task_not_found") {
			return nil, fmt.Errorf("TSec task is not runnable: %s", truncate(string(data), 300))
		}
		log.Printf("TSec fetch_all_challenges failed: %v", err)
		return nil, nil // degrade to empty for the poller
	}
	var raw []rawChallenge
	if json.Unmarshal(data, &raw) != nil {
		log.Printf("TSec API returned non-list")
		return nil, nil
	}
	out := make([]Challenge, 0, len(raw))
	t.mu.Lock()
	defer t.mu.Unlock()
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
	solved, _ := t.FetchSolvedNames()
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
	data, _, err := t.do("POST", "openapi/v1/challenges/submit", nil,
		map[string]string{"unique_code": ch.UniqueCode, "flag": strings.TrimSpace(flag)})
	if err != nil {
		var body struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &body)
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
	_ = json.Unmarshal(data, &r)
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

func (t *TSec) startChallenge(uniqueCode string) ([]string, error) {
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		data, status, err := t.do("POST", "openapi/v1/challenges/start", url.Values{"unique_code": {uniqueCode}}, nil)
		if err == nil {
			var resp struct {
				ContainerAddr []string `json:"container_addr"`
			}
			_ = json.Unmarshal(data, &resp)
			if len(resp.ContainerAddr) > 0 {
				t.mu.Lock()
				t.started[uniqueCode] = resp.ContainerAddr
				t.mu.Unlock()
			}
			return resp.ContainerAddr, nil
		}
		lastErr = err
		if status == 409 && strings.Contains(strings.ToLower(string(data)), "max active") {
			time.Sleep(time.Duration(4+attempt*2) * time.Second)
			continue
		}
		return nil, err
	}
	return nil, fmt.Errorf("TSec start %s stayed at capacity: %w", uniqueCode, lastErr)
}

func (t *TSec) closeChallenge(uniqueCode string) error {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		_, status, err := t.do("POST", "openapi/v1/challenges/close", url.Values{"unique_code": {uniqueCode}}, nil)
		if err == nil {
			t.mu.Lock()
			delete(t.started, uniqueCode)
			t.mu.Unlock()
			return nil
		}
		lastErr = err
		if status != 0 && status < 500 {
			return err // terminal (e.g. already closed)
		}
		time.Sleep([]time.Duration{3, 8, 15}[min(attempt, 2)] * time.Second)
	}
	return fmt.Errorf("TSec close %s failed: %w", uniqueCode, lastErr)
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
		}
	}
	return closed
}

func (t *TSec) FetchHint(challengeName string) (string, error) {
	ch, ok := t.lookup(challengeName)
	if !ok || ch.UniqueCode == "" {
		return "", nil
	}
	data, _, err := t.do("GET", "openapi/v1/challenges/hint", url.Values{"unique_code": {ch.UniqueCode}}, nil)
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
func (t *TSec) PullChallenge(ch Challenge) (mission.Meta, error) {
	addrs := ch.ContainerAddr
	t.mu.Lock()
	if len(addrs) == 0 {
		if a, ok := t.started[ch.UniqueCode]; ok {
			addrs = a
		}
	}
	t.mu.Unlock()

	if len(addrs) == 0 && ch.UniqueCode != "" {
		if a, err := t.startChallenge(ch.UniqueCode); err == nil && len(a) > 0 {
			addrs = a
		}
		for i := 0; len(addrs) == 0 && i < 10; i++ {
			time.Sleep(3 * time.Second)
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
	return mission.Meta{
		Name: ch.Name, Category: ch.Category, Description: ch.Description,
		ConnectionInfo: strings.Join(addrs, ", "), FlagCount: ch.FlagCount,
	}, nil
}

func (t *TSec) Close() {}
