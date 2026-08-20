package xai

import (
	"net/http"
	"time"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
)

type ChatExecutor = openaiProvider.ChatExecutor

func NewChat(credentials *providercredentials.Registry, timeout time.Duration, streamIdle ...time.Duration) *ChatExecutor {
	return openaiProvider.NewChatForProvider(providercredentials.XAI, credentials, timeout, streamIdle...)
}

func NewChatWithClient(credentials *providercredentials.Registry, timeout time.Duration, client *http.Client) *ChatExecutor {
	return openaiProvider.NewChatWithClientForProvider(providercredentials.XAI, credentials, timeout, client)
}
