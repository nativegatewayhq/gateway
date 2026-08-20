package providercredentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestLoadOptionalCredentialsAndMetadata(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"GATEWAY_GOOGLE_API_KEY": "google-provider-secret",
		"GATEWAY_XAI_API_KEY":    "xai-provider-secret",
	}
	registry, err := Load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	providers := registry.ConfiguredProviders()
	if fmt.Sprint(providers) != "[google xai]" {
		t.Fatalf("providers = %v", providers)
	}
	if _, err := registry.Credential(OpenAI); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("missing credential error = %v", err)
	}
	if _, err := registry.Credential(ProviderID("unknown")); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("unknown provider error = %v", err)
	}
}

func TestLoadRejectsMalformedWithoutLeakingValue(t *testing.T) {
	t.Parallel()
	tests := []string{"", " leading", "trailing ", "control\nvalue", strings.Repeat("x", maxCredentialLength+1)}
	for index, value := range tests {
		value := value
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			registry, err := Load(func(key string) (string, bool) {
				if key == "GATEWAY_OPENAI_API_KEY" {
					return value, true
				}
				return "", false
			})
			if registry != nil || !errors.Is(err, ErrMalformedCredential) {
				t.Fatalf("Load() = %v, %v", registry, err)
			}
			if value != "" && strings.Contains(err.Error(), value) {
				t.Fatalf("error leaked credential: %v", err)
			}
			if !strings.Contains(err.Error(), "GATEWAY_OPENAI_API_KEY") {
				t.Fatalf("error omitted setting name: %v", err)
			}
		})
	}
}

func TestCredentialFormattingAndJSONDoNotExposePlaintext(t *testing.T) {
	t.Parallel()
	secret := "formatting-provider-secret"
	registry, err := Load(func(key string) (string, bool) {
		if key == "GATEWAY_GOOGLE_API_KEY" {
			return secret, true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := registry.Credential(Google)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(struct {
		Registry   *Registry  `json:"registry"`
		Credential Credential `json:"credential"`
	}{registry, credential})
	if err != nil {
		t.Fatal(err)
	}
	combined := fmt.Sprintf("%v %+v %#v %s", registry, credential, credential, encoded)
	if strings.Contains(combined, secret) {
		t.Fatalf("formatting leaked credential: %s", combined)
	}
}

func TestLegacyChannelAvailabilityDoesNotDestroyRegistryCredential(t *testing.T) {
	registry, err := Load(func(key string) (string, bool) {
		if key == "GATEWAY_OPENAI_API_KEY" {
			return "persistent-legacy-secret", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	channel, _ := LegacyChannel(OpenAI)
	for index := 0; index < 2; index++ {
		if !registry.ConfiguredChannel(context.Background(), channel, OpenAI) {
			t.Fatalf("availability call %d failed", index)
		}
	}
	credential, err := registry.Resolve(context.Background(), channel, OpenAI)
	if err != nil || string(credential.value) != "persistent-legacy-secret" {
		t.Fatalf("credential=%v err=%v", credential, err)
	}
	credential.Destroy()
}
