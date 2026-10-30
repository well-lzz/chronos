package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/well-lzz/chronos/internal/config"
	"github.com/well-lzz/chronos/internal/llmconfig"
	"github.com/well-lzz/chronos/internal/llmrec"
)

type breakerState struct {
	Failures int       `json:"failures"`
	Trips    int       `json:"trips"`
	Until    time.Time `json:"until"`
}

// Registry stores shared provider health without persisting credentials.
type Registry struct {
	mu     sync.Mutex
	path   string
	states map[string]breakerState
	next   uint64
}

var registries sync.Map

func SharedRegistry(stateDir string) *Registry {
	key := filepath.Clean(stateDir)
	if existing, ok := registries.Load(key); ok {
		return existing.(*Registry)
	}
	r := &Registry{path: filepath.Join(stateDir, "llm_breakers.json"), states: map[string]breakerState{}}
	if data, err := os.ReadFile(r.path); err == nil {
		_ = json.Unmarshal(data, &r.states)
	}
	actual, _ := registries.LoadOrStore(key, r)
	return actual.(*Registry)
}

func profileKey(p llmconfig.Profile) string {
	sum := sha256.Sum256([]byte(p.BaseURL + "\x00" + p.Model + "\x00" + p.APIKey))
	return hex.EncodeToString(sum[:])
}

func (r *Registry) cooling(p llmconfig.Profile, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return now.Before(r.states[profileKey(p)].Until)
}

func (r *Registry) success(p llmconfig.Profile) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.states[profileKey(p)]
	state.Failures = 0
	state.Until = time.Time{}
	r.states[profileKey(p)] = state
	r.persist()
}

func (r *Registry) failure(p llmconfig.Profile, err error, now time.Time) {
	if !shouldFailover(err) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := profileKey(p)
	state := r.states[key]
	state.Failures++
	if hardFailure(err) || state.Failures >= 3 {
		state.Trips++
		state.Failures = 0
		cooldowns := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}
		index := min(state.Trips-1, len(cooldowns)-1)
		state.Until = now.Add(cooldowns[index])
	}
	r.states[key] = state
	r.persist()
}

func (r *Registry) persist() {
	if r.path == "" {
		return
	}
	data, err := json.MarshalIndent(r.states, "", "  ")
	if err != nil || os.MkdirAll(filepath.Dir(r.path), 0o700) != nil {
		return
	}
	tmp := r.path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		if os.Rename(tmp, r.path) == nil {
			return
		}
	}
	_ = os.Remove(tmp)
}

func hardFailure(err error) bool {
	var apiErr *openai.APIError
	return errors.As(err, &apiErr) && (apiErr.HTTPStatusCode == 401 || apiErr.HTTPStatusCode == 402 || apiErr.HTTPStatusCode == 403 || apiErr.HTTPStatusCode == 404)
}

func shouldFailover(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		status := apiErr.HTTPStatusCode
		return hardFailure(err) || status == 408 || status == 429 || status >= 500
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

func profileChain(primary llmconfig.Profile, rotation uint64) []llmconfig.Profile {
	chain := []llmconfig.Profile{primary}
	backups := append([]llmconfig.FallbackProfile(nil), primary.Fallbacks...)
	sort.SliceStable(backups, func(i, j int) bool { return backups[i].Priority < backups[j].Priority })
	for start := 0; start < len(backups); {
		end := start + 1
		for end < len(backups) && backups[end].Priority == backups[start].Priority {
			end++
		}
		group := backups[start:end]
		for offset := range group {
			item := group[(offset+int(rotation%uint64(len(group))))%len(group)]
			chain = append(chain, llmconfig.Profile{Provider: item.Provider, Model: item.Model, BaseURL: item.BaseURL,
				APIKey: item.APIKey, ContextWindowTokens: item.ContextWindowTokens})
		}
		start = end
	}
	return chain
}

// Complete retries only failures that happen before a complete response. The
// current Chronos model path is non-streaming, so no text or tool call can have
// been yielded on an error. Context cancellation and request errors never
// spend a backup credential.
func (r *Registry) Complete(ctx context.Context, cfg config.Settings, primary llmconfig.Profile, request openai.ChatCompletionRequest, recorder *llmrec.Recorder, mission, executor string) (openai.ChatCompletionResponse, error) {
	return r.complete(ctx, cfg, primary, request, recorder, mission, executor, false)
}

// CompleteExact keeps models-file credentials isolated from environment pools.
func (r *Registry) CompleteExact(ctx context.Context, cfg config.Settings, primary llmconfig.Profile, request openai.ChatCompletionRequest, recorder *llmrec.Recorder, mission, executor string) (openai.ChatCompletionResponse, error) {
	return r.complete(ctx, cfg, primary, request, recorder, mission, executor, true)
}

func (r *Registry) complete(ctx context.Context, cfg config.Settings, primary llmconfig.Profile, request openai.ChatCompletionRequest, recorder *llmrec.Recorder, mission, executor string, exact bool) (openai.ChatCompletionResponse, error) {
	r.mu.Lock()
	rotation := r.next
	r.next++
	r.mu.Unlock()
	chain := profileChain(primary, rotation)
	order := make([]llmconfig.Profile, 0, len(chain))
	now := time.Now()
	// A rough upper bound catches obviously undersized context windows. Actual
	// context errors remain with the solver's existing compaction recovery.
	encoded, _ := json.Marshal(struct {
		Messages any `json:"messages"`
		Tools    any `json:"tools"`
	}{request.Messages, request.Tools})
	estimatedTokens := len(encoded) / 3
	for _, p := range chain {
		window := p.ContextWindowTokens
		if window == 0 {
			window = llmconfig.KnownContextWindowTokens(p.Model)
		}
		if !r.cooling(p, now) && (window == 0 || estimatedTokens < window*4/5) {
			order = append(order, p)
		}
	}
	if len(order) == 0 {
		order = append(order, chain[0]) // one probe keeps the task recoverable
	}
	var lastErr error
	for _, p := range order {
		if ctx.Err() != nil {
			return openai.ChatCompletionResponse{}, ctx.Err()
		}
		var client *openai.Client
		var model string
		var err error
		if !exact && profileKey(p) == profileKey(primary) {
			client, model, err = FromProfileWithSettings(p, cfg)
		} else {
			client, model, err = FromExactProfileWithSettings(p, cfg)
		}
		if err != nil {
			return openai.ChatCompletionResponse{}, err
		}
		attempt := request
		attempt.Model = model
		call := client.CreateChatCompletion
		var response openai.ChatCompletionResponse
		if recorder != nil {
			response, err = recorder.Call(ctx, mission, executor, attempt, call)
		} else {
			response, err = call(ctx, attempt)
		}
		if err == nil {
			r.success(p)
			return response, nil
		}
		lastErr = err
		r.failure(p, err, time.Now())
		if !shouldFailover(err) || ctx.Err() != nil {
			return openai.ChatCompletionResponse{}, err
		}
	}
	return openai.ChatCompletionResponse{}, fmt.Errorf("all configured model profiles failed: %w", lastErr)
}
