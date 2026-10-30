package solver

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	openai "github.com/sashabaranov/go-openai"
)

const maxParallelReads = 3

// Only a batch made entirely of bodyless GET/HEAD requests can run in parallel.
// Blackboard writes, flag submissions, Python and raw TCP preserve model order.
func parallelReadBatch(calls []openai.ToolCall) bool {
	if len(calls) < 2 {
		return false
	}
	for _, call := range calls {
		if call.Function.Name != "http_request" {
			return false
		}
		var args struct {
			Method string `json:"method"`
			Body   string `json:"body"`
		}
		if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || args.Body != "" {
			return false
		}
		method := strings.ToUpper(strings.TrimSpace(args.Method))
		if method != "" && method != "GET" && method != "HEAD" {
			return false
		}
	}
	return true
}

func parallelDelegateBatch(calls []openai.ToolCall) bool {
	if len(calls) < 2 {
		return false
	}
	for _, call := range calls {
		if call.Function.Name != "delegate" && call.Function.Name != "delegate_resume" {
			return false
		}
	}
	return true
}

// runToolBatch returns one result per tool call in the model's original order.
// Calls over the cycle budget receive a result but are never executed, keeping
// the API's tool-call/result pairing valid.
func runToolBatch(ctx context.Context, calls []openai.ToolCall, remaining int, dispatch func(context.Context, string, string) string) []string {
	results := make([]string, len(calls))
	limit := remaining
	if limit < 0 {
		limit = 0
	}
	if limit > len(calls) {
		limit = len(calls)
	}
	for i := limit; i < len(calls); i++ {
		results[i] = "tool budget exhausted; call was not executed"
	}
	if !parallelReadBatch(calls[:limit]) && !parallelDelegateBatch(calls[:limit]) {
		for i := 0; i < limit; i++ {
			if ctx.Err() != nil {
				results[i] = "cancelled; call was not executed"
				continue
			}
			results[i] = dispatch(ctx, calls[i].Function.Name, calls[i].Function.Arguments)
		}
		return results
	}

	jobs := make(chan int)
	var workers sync.WaitGroup
	count := maxParallelReads
	if limit < count {
		count = limit
	}
	for worker := 0; worker < count; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					results[i] = "cancelled; call was not executed"
					continue
				}
				results[i] = dispatch(ctx, calls[i].Function.Name, calls[i].Function.Arguments)
			}
		}()
	}
	for i := 0; i < limit; i++ {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
	return results
}
