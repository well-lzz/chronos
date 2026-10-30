// Package llmrec accounts for every model request and optionally keeps its
// full request/response. It stores no provider credentials.
package llmrec

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

const ringSize = 2000

type Usage struct {
	ID           string    `json:"id"`
	Time         time.Time `json:"time"`
	Mission      string    `json:"mission"`
	Executor     string    `json:"executor"`
	Model        string    `json:"model"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	TotalTokens  int       `json:"total_tokens"`
	LatencyMS    int64     `json:"latency_ms"`
	Status       string    `json:"status"`
	HTTPStatus   int       `json:"http_status,omitempty"`
	Error        string    `json:"error,omitempty"`
}

type Trace struct {
	Usage         Usage                          `json:"usage"`
	Request       openai.ChatCompletionRequest   `json:"request"`
	Response      *openai.ChatCompletionResponse `json:"response,omitempty"`
	ProviderError string                         `json:"provider_error,omitempty"`
}

type Recorder struct {
	mu       sync.Mutex
	stateDir string
	heavy    bool
	rows     []Usage
}

var shared sync.Map

// Shared returns one recorder per state directory so concurrent missions share
// the same audit ring and append-only usage log.
func Shared(stateDir string, heavy bool) *Recorder {
	key := filepath.Clean(stateDir)
	if existing, ok := shared.Load(key); ok {
		r := existing.(*Recorder)
		if heavy {
			r.mu.Lock()
			r.heavy = true
			r.mu.Unlock()
		}
		return r
	}
	r := &Recorder{stateDir: stateDir, heavy: heavy}
	if stateDir != "" {
		if file, err := os.Open(filepath.Join(stateDir, "llm_usage.jsonl")); err == nil {
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				var row Usage
				if json.Unmarshal(scanner.Bytes(), &row) == nil {
					r.rows = append(r.rows, row)
					if len(r.rows) > ringSize {
						r.rows = r.rows[1:]
					}
				}
			}
			_ = file.Close()
		}
	}
	actual, loaded := shared.LoadOrStore(key, r)
	if loaded && heavy {
		current := actual.(*Recorder)
		current.mu.Lock()
		current.heavy = true
		current.mu.Unlock()
	}
	return actual.(*Recorder)
}

// Call writes usage even when the provider returns an error or the context is
// cancelled. Full messages and response are saved only when enabled.
func (r *Recorder) Call(ctx context.Context, mission, executor string, request openai.ChatCompletionRequest, complete func(context.Context, openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error)) (openai.ChatCompletionResponse, error) {
	started := time.Now()
	response, err := complete(ctx, request)
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	row := Usage{ID: hex.EncodeToString(nonce[:]), Time: started.UTC(), Mission: mission, Executor: executor,
		Model: request.Model, InputTokens: response.Usage.PromptTokens, OutputTokens: response.Usage.CompletionTokens,
		TotalTokens: response.Usage.TotalTokens, LatencyMS: time.Since(started).Milliseconds(), Status: "ok"}
	if row.TotalTokens == 0 {
		row.TotalTokens = row.InputTokens + row.OutputTokens
	}
	if err != nil {
		row.Status = "error"
		row.Error = "provider request failed"
		if ctx.Err() != nil {
			row.Status = "cancelled"
			row.Error = "context cancelled"
		}
		var apiErr *openai.APIError
		if errors.As(err, &apiErr) {
			row.HTTPStatus = apiErr.HTTPStatusCode
			row.Error = fmt.Sprintf("provider HTTP %d", apiErr.HTTPStatusCode)
		}
	}
	r.mu.Lock()
	r.rows = append(r.rows, row)
	if len(r.rows) > ringSize {
		r.rows = append([]Usage(nil), r.rows[len(r.rows)-ringSize:]...)
	}
	if r.stateDir != "" {
		if writeErr := appendJSONL(filepath.Join(r.stateDir, "llm_usage.jsonl"), row); writeErr != nil {
			log.Printf("LLM usage persistence failed: %v", writeErr)
		}
		if r.heavy {
			trace := Trace{Usage: row, Request: request}
			if err == nil {
				trace.Response = &response
			} else {
				trace.ProviderError = err.Error()
			}
			if writeErr := appendJSONL(filepath.Join(r.stateDir, "llm_records", safeName(mission)+".jsonl"), trace); writeErr != nil {
				log.Printf("LLM trace persistence failed: %v", writeErr)
			}
		}
	}
	r.mu.Unlock()
	return response, err
}

func (r *Recorder) Recent(limit int) []Usage {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit <= 0 || limit > ringSize {
		limit = 100
	}
	if limit > len(r.rows) {
		limit = len(r.rows)
	}
	return append([]Usage(nil), r.rows[len(r.rows)-limit:]...)
}

// TraceByID reads one opt-in full record for console replay.
func (r *Recorder) TraceByID(mission, id string) (Trace, bool) {
	if r.stateDir == "" || len(id) != 16 {
		return Trace{}, false
	}
	file, err := os.Open(filepath.Join(r.stateDir, "llm_records", safeName(mission)+".jsonl"))
	if err != nil {
		return Trace{}, false
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 64<<20)
	for scanner.Scan() {
		var trace Trace
		if json.Unmarshal(scanner.Bytes(), &trace) == nil && trace.Usage.ID == id && trace.Usage.Mission == mission {
			return trace, true
		}
	}
	return Trace{}, false
}

func appendJSONL(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(append(data, '\n'))
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(name string) string {
	name = unsafeName.ReplaceAllString(name, "-")
	if name == "" {
		return "mission"
	}
	return name
}
