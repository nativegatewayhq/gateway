// Package openai implements the trusted OpenAI transport.
package openai

import (
	"net/http"
	"time"

	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/providers/openaiimages"
)

type Executor = openaiimages.Executor
type ImageGenerationRequest = openaiimages.Request

func New(credentials *providercredentials.Registry, timeout time.Duration) *Executor {
	return openaiimages.New(providercredentials.OpenAI, credentials, timeout)
}

func NewWithClient(credentials *providercredentials.Registry, timeout time.Duration, client *http.Client) *Executor {
	return openaiimages.NewWithClient(providercredentials.OpenAI, credentials, timeout, client)
}
