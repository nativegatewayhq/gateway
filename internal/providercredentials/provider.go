// Package providercredentials isolates Gateway-owned upstream credentials from
// inbound service API keys and applies them only to scoped provider requests.
package providercredentials

import (
	"errors"
	"fmt"
	"strings"
)

type ProviderID string

const (
	Google    ProviderID = "google"
	OpenAI    ProviderID = "openai"
	XAI       ProviderID = "xai"
	Replicate ProviderID = "replicate"
	Fal       ProviderID = "fal"
	Anthropic ProviderID = "anthropic"
	Runway    ProviderID = "runway"
	Plugin    ProviderID = "plugin"
)

var (
	ErrUnknownProvider       = errors.New("unknown provider")
	ErrCredentialUnavailable = errors.New("provider credential unavailable")
	ErrScopeMismatch         = errors.New("provider credential scope mismatch")
	ErrMalformedCredential   = errors.New("malformed provider credential")
)

func ParseProviderID(value string) (ProviderID, error) {
	provider := ProviderID(strings.ToLower(strings.TrimSpace(value)))
	switch provider {
	case Google, OpenAI, XAI, Replicate, Fal, Anthropic, Runway, Plugin:
		return provider, nil
	default:
		return "", ErrUnknownProvider
	}
}

func (provider ProviderID) Valid() bool {
	return provider.validateExact() == nil
}

func (provider ProviderID) validateExact() error {
	parsed, err := ParseProviderID(string(provider))
	if err != nil || parsed != provider {
		return fmt.Errorf("%w: unsupported provider ID", ErrUnknownProvider)
	}
	return nil
}
