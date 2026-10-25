package solver

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

// retryableModelError separates provider availability from search quality.
// Context cancellation is checked by the caller before reaching this function.
func retryableModelError(err error) bool {
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.HTTPStatusCode
		return code == 408 || code == 409 || code == 429 || code >= 500
	}
	var netErr net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.As(err, &netErr)
}

func isContextLimitError(err error) bool {
	var apiErr *openai.APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatusCode != 400 {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "context_length") || strings.Contains(msg, "context length") || strings.Contains(msg, "maximum context") || strings.Contains(msg, "token limit")
}
