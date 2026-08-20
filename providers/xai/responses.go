package xai

import (
	"net/http"
	"time"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
)

type ResponsesExecutor = openaiProvider.ResponsesExecutor

func NewResponses(credentials *providercredentials.Registry, timeout time.Duration, streamIdle ...time.Duration) *ResponsesExecutor {
	return openaiProvider.NewResponsesForProvider(providercredentials.XAI, credentials, timeout, streamIdle...)
}

func NewResponsesWithClient(credentials *providercredentials.Registry, timeout time.Duration, client *http.Client) *ResponsesExecutor {
	return openaiProvider.NewResponsesWithClientForProvider(providercredentials.XAI, credentials, timeout, client)
}
