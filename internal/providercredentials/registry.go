package providercredentials

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"unicode"
)

const maxCredentialLength = 4096

var environmentKeys = map[ProviderID]string{
	Google:    "GATEWAY_GOOGLE_API_KEY",
	OpenAI:    "GATEWAY_OPENAI_API_KEY",
	XAI:       "GATEWAY_XAI_API_KEY",
	Replicate: "GATEWAY_REPLICATE_API_TOKEN",
	Fal:       "GATEWAY_FAL_API_KEY",
}

type LookupEnv func(string) (string, bool)

// Credential is an opaque, provider-scoped handle. It deliberately exposes no
// plaintext getter or serialization methods.
type Credential struct {
	provider  ProviderID
	channelID string
	value     []byte
}

// Registry is immutable after construction.
type Registry struct {
	credentials map[ProviderID]Credential
	store       *Store
}

func (Credential) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "<provider-credential>")
}
func (*Registry) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "<provider-credential-registry>")
}

var legacyChannels = map[string]ProviderID{
	"channel_00000000000000000000000000000001": OpenAI,
	"channel_00000000000000000000000000000002": XAI,
	"channel_00000000000000000000000000000003": Google,
	"channel_00000000000000000000000000000004": Replicate,
	"channel_00000000000000000000000000000005": Fal,
}

func LegacyChannel(provider ProviderID) (string, bool) {
	for channelID, scopedProvider := range legacyChannels {
		if scopedProvider == provider {
			return channelID, true
		}
	}
	return "", false
}

func Load(lookup LookupEnv) (*Registry, error) {
	registry := &Registry{credentials: make(map[ProviderID]Credential, len(environmentKeys))}
	for _, provider := range []ProviderID{Google, OpenAI, XAI, Replicate, Fal} {
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

func NewControlPlane(legacy *Registry, store *Store) (*Registry, error) {
	if legacy == nil {
		legacy = &Registry{credentials: map[ProviderID]Credential{}}
	}
	if store == nil {
		return nil, ErrCredentialUnavailable
	}
	return &Registry{credentials: legacy.credentials, store: store}, nil
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
	credential.value = append([]byte(nil), credential.value...)
	return credential, nil
}

func (registry *Registry) Resolve(ctx context.Context, channelID string, provider ProviderID) (Credential, error) {
	if registry == nil || !validChannelID(channelID) || provider.validateExact() != nil {
		return Credential{}, ErrCredentialUnavailable
	}
	if registry.store != nil {
		credential, err := registry.store.Resolve(ctx, channelID, provider)
		if err == nil {
			return credential, nil
		}
		if !errors.Is(err, ErrCredentialUnavailable) {
			return Credential{}, err
		}
	}
	if legacyChannels[channelID] != provider {
		return Credential{}, ErrCredentialUnavailable
	}
	credential, err := registry.Credential(provider)
	if err != nil {
		return Credential{}, err
	}
	credential.channelID = channelID
	return credential, nil
}

func (registry *Registry) ConfiguredChannel(ctx context.Context, channelID string, provider ProviderID) bool {
	credential, err := registry.Resolve(ctx, channelID, provider)
	if err != nil {
		return false
	}
	credential.Destroy()
	return true
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
	case OpenAI, XAI, Replicate:
		request.Header.Set("Authorization", "Bearer "+string(credential.value))
	case Fal:
		request.Header.Set("Authorization", "Key "+string(credential.value))
	default:
		return ErrUnknownProvider
	}
	return nil
}
