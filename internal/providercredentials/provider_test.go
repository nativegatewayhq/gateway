package providercredentials

import (
	"errors"
	"testing"
)

func TestParseProviderID(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"google", "openai", "xai"} {
		provider, err := ParseProviderID(value)
		if err != nil || string(provider) != value {
			t.Fatalf("ParseProviderID(%q) = %q, %v", value, provider, err)
		}
	}
	if _, err := ParseProviderID("unknown"); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("unknown error = %v", err)
	}
	if ProviderID(" Google ").Valid() {
		t.Fatal("non-canonical provider ID reported valid")
	}
}
