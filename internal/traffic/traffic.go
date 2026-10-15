// Package traffic is the per-mission log of real request/response pairs that
// findings cite as evidence. Every http_request / tcp_send exchange is recorded
// with a short id (t1, t2, …); findings reference those ids with a role
// (baseline/proof/verification/supporting). Shared across executors on one
// mission and appended to a JSONL file so it survives a restart.
package traffic

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Roles a finding can assign to a referenced exchange.
var Roles = map[string]bool{"baseline": true, "proof": true, "verification": true, "supporting": true}

const DefaultRole = "supporting"
const minSearch = 3

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

var slugRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func slug(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(s, "-"), "-")
	if s == "" {
		return "mission"
	}
	return s
}

var httpStatusRe = regexp.MustCompile(`^HTTP (\d{3})`)

type Exchange struct {
	ID          string  `json:"id"`
	Kind        string  `json:"kind"` // http | tcp
	Target      string  `json:"target"`
	Method      string  `json:"method,omitempty"`
	Status      int     `json:"status,omitempty"`
	RequestText string  `json:"request_text,omitempty"`
	Response    string  `json:"response_text,omitempty"`
	RespLen     int     `json:"resp_len"`
	FoundBy     string  `json:"found_by,omitempty"`
	TS          float64 `json:"ts"`
}

// Log is the shared request/response store for one mission.
type Log struct {
	MissionID string
	Path      string

	mu    sync.Mutex
	seq   int
	byID  map[string]*Exchange
	order []string
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (l *Log) nextID() string {
	l.seq++
	return "t" + strconv.Itoa(l.seq)
}

// RecordHTTP stores one HTTP exchange and returns its id.
func (l *Log) RecordHTTP(method, url string, headers map[string]string, body, response, foundBy string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	id := l.nextID()
	var hb strings.Builder
	for k, v := range headers {
		fmt.Fprintf(&hb, "%s: %s\n", k, v)
	}
	req := strings.TrimRight(strings.ToUpper(method)+" "+url+"\n"+hb.String(), "\n")
	if body != "" {
		req += "\n\n" + body
	}
	status := 0
	if m := httpStatusRe.FindStringSubmatch(response); m != nil {
		status, _ = strconv.Atoi(m[1])
	}
	ex := &Exchange{ID: id, Kind: "http", Target: clip(url, 600), Method: strings.ToUpper(method),
		Status: status, RequestText: clip(req, 8000), Response: clip(response, 24000),
		RespLen: len(response), FoundBy: foundBy, TS: now()}
	l.store(ex)
	return id
}

// RecordTCP stores one TCP exchange and returns its id.
func (l *Log) RecordTCP(host string, port int, payload, payloadHex, response, foundBy string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	id := l.nextID()
	target := fmt.Sprintf("%s:%d", host, port)
	req := "TCP " + target + "\n"
	if payloadHex != "" {
		req += "payload_hex=" + payloadHex
	} else {
		req += payload
	}
	ex := &Exchange{ID: id, Kind: "tcp", Target: target, Method: "TCP",
		RequestText: clip(req, 8000), Response: clip(response, 24000),
		RespLen: len(response), FoundBy: foundBy, TS: now()}
	l.store(ex)
	return id
}

func (l *Log) store(ex *Exchange) {
	l.byID[ex.ID] = ex
	l.order = append(l.order, ex.ID)
	l.append(ex)
}

func (l *Log) Exists(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.byID[id]
	return ok
}

type indexRow struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Method  string `json:"method"`
	Target  string `json:"target"`
	Status  int    `json:"status"`
	RespLen int    `json:"resp_len"`
}

// Search returns a lightweight index (no bodies) of captured exchanges.
func (l *Log) Search(host, urlContains, bodyContains string, page int) map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	host = strings.ToLower(strings.TrimSpace(host))
	urlSub := strings.ToLower(strings.TrimSpace(urlContains))
	bodySub := strings.TrimSpace(bodyContains)
	if bodySub != "" && len(bodySub) < minSearch {
		return map[string]any{"error": fmt.Sprintf("body_contains needs at least %d characters", minSearch)}
	}
	var hits []indexRow
	for _, id := range l.order {
		ex := l.byID[id]
		tgt := strings.ToLower(ex.Target)
		if host != "" && !strings.Contains(tgt, host) {
			continue
		}
		if urlSub != "" && !strings.Contains(tgt, urlSub) {
			continue
		}
		if bodySub != "" && !strings.Contains(ex.RequestText, bodySub) && !strings.Contains(ex.Response, bodySub) {
			continue
		}
		hits = append(hits, indexRow{ex.ID, ex.Kind, ex.Method, ex.Target, ex.Status, ex.RespLen})
	}
	const perPage = 10
	if page < 0 {
		page = 0
	}
	start := page * perPage
	end := start + perPage
	if start > len(hits) {
		start = len(hits)
	}
	if end > len(hits) {
		end = len(hits)
	}
	return map[string]any{"total": len(hits), "page": page, "per_page": perPage, "items": hits[start:end]}
}

// RecentRows returns up to limit most-recent exchanges as light rows (no bodies)
// for the console's live traffic table.
func (l *Log) RecentRows(limit int) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	ids := l.order
	if limit > 0 && len(ids) > limit {
		ids = ids[len(ids)-limit:]
	}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		ex := l.byID[id]
		out = append(out, map[string]any{
			"id": ex.ID, "kind": ex.Kind, "method": ex.Method, "target": ex.Target,
			"status": ex.Status, "resp_len": ex.RespLen, "by": ex.FoundBy, "ts": ex.TS,
		})
	}
	return out
}

// Get returns one exchange's request/response text, bounded.
func (l *Log) Get(id, side string, offset, length int) map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	ex := l.byID[id]
	if ex == nil {
		return map[string]any{"error": "no exchange " + id}
	}
	if length < 1 || length > 8192 {
		length = 8192
	}
	if offset < 0 {
		offset = 0
	}
	sl := func(s string) string {
		if offset >= len(s) {
			return ""
		}
		e := offset + length
		if e > len(s) {
			e = len(s)
		}
		return s[offset:e]
	}
	out := map[string]any{"id": ex.ID, "kind": ex.Kind, "target": ex.Target, "method": ex.Method, "status": ex.Status}
	if side == "request" || side == "both" || side == "" {
		out["request"] = sl(ex.RequestText)
	}
	if side == "response" || side == "both" || side == "" {
		out["response"] = sl(ex.Response)
	}
	return out
}

// ---------------------------- persistence -----------------------------------

func (l *Log) append(ex *Exchange) {
	if l.Path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(l.Path), 0o755)
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if data, err := json.Marshal(ex); err == nil {
		_, _ = f.Write(append(data, '\n'))
	}
}

// LoadOrNew loads a mission's traffic log (JSONL) or starts a fresh one.
func LoadOrNew(missionID, stateDir string) *Log {
	path := filepath.Join(stateDir, "traffic", slug(missionID)+".jsonl")
	l := &Log{MissionID: missionID, Path: path, byID: map[string]*Exchange{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return l
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ex Exchange
		if json.Unmarshal([]byte(line), &ex) != nil {
			continue
		}
		l.byID[ex.ID] = &ex
		l.order = append(l.order, ex.ID)
		if n, err := strconv.Atoi(strings.TrimPrefix(ex.ID, "t")); err == nil && n > l.seq {
			l.seq = n
		}
	}
	return l
}
