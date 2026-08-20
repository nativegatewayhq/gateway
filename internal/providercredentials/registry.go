package providercredentials

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"unicode"
)

const maxCredentialLength = 4096

var environmentKeys = map[ProviderID]string{
	Google: "GATEWAY_GOOGLE_API_KEY",
	OpenAI: "GATEWAY_OPENAI_API_KEY",
	XAI:    "GATEWAY_XAI_API_KEY",
}

type LookupEnv func(string) (string, bool)

// Credential is an opaque, provider-scoped handle. It deliberately exposes no
// plaintext getter or serialization methods.
type Credential struct {
	provider ProviderID
	value    []byte
}

// Registry is immutable after construction.
type Registry struct {
	credentials map[ProviderID]Credential
}

func Load(lookup LookupEnv) (*Registry, error) {
	registry := &Registry{credentials: make(map[ProviderID]Credential, len(environmentKeys))}
	for _, provider := range []ProviderID{Google, OpenAI, XAI} {
		environmentKey := environmentKeys[provider]
		value, configured := lookup(environmentKey)
		if !configured {
			continue
		}
		if err := validateCredential(value); err != nil {
			return nil, fmt.Errorf("%s: %w", environmentKey, ErrMalformedCredential)
		}
		registry.credentials[provider] = Credential{provider: provider, value: []byte(value)}
	}
	return registry, nil
}

func validateCredential(value string) error {
	if value == "" || len(value) > maxCredentialLength || strings.TrimSpace(value) != value {
		return ErrMalformedCredential
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return ErrMalformedCredential
		}
	}
	return nil
}

func (registry *Registry) Credential(provider ProviderID) (Credential, error) {
	if err := provider.validateExact(); err != nil {
		return Credential{}, err
	}
	credential, exists := registry.credentials[provider]
	if !exists {
		return Credential{}, ErrCredentialUnavailable
	}
	return credential, nil
}

func (registry *Registry) ConfiguredProviders() []ProviderID {
	providers := make([]ProviderID, 0, len(registry.credentials))
	for provider := range registry.credentials {
		providers = append(providers, provider)
	}
	sort.Slice(providers, func(left, right int) bool { return providers[left] < providers[right] })
	return providers
}

func (credential Credential) Apply(request *http.Request, provider ProviderID) error {
	if request == nil {
		return fmt.Errorf("%w: nil outbound request", ErrScopeMismatch)
	}
	if err := provider.validateExact(); err != nil {
		return err
	}
	if credential.provider != provider || len(credential.value) == 0 {
		return ErrScopeMismatch
	}
	switch provider {
	case Google:
		request.Header.Set("x-goog-api-key", string(credential.value))
	case OpenAI, XAI:
		request.Header.Set("Authorization", "Bearer "+string(credential.value))
	default:
		return ErrUnknownProvider
	}
	return nil
}
