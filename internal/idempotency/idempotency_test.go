package idempotency

import (
	"errors"
	"net/http"
	"testing"
)

func TestExtractValidatesSingleVisibleASCIIKey(t *testing.T) {
	if key, err := Extract(http.Header{}); err != nil || key != "" {
		t.Fatalf("missing=%q %v", key, err)
	}
	header := http.Header{HeaderName: {"client-key_1"}}
	if key, err := Extract(header); err != nil || key != "client-key_1" {
		t.Fatalf("valid=%q %v", key, err)
	}
	for _, values := range [][]string{{"one", "two"}, {"with space"}, {"\n"}, {""}} {
		header[HeaderName] = values
		if _, err := Extract(header); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("values=%q error=%v", values, err)
		}
	}
}

func TestFingerprintHasUnambiguousFieldBoundariesAndExactBody(t *testing.T) {
	base := Fingerprint("openai", "image.generate", "model", "channel", "application/json", []byte(`{"a":1}`))
	if base != Fingerprint("openai", "image.generate", "model", "channel", "application/json", []byte(`{"a":1}`)) {
		t.Fatal("same request produced different fingerprint")
	}
	for _, changed := range [][32]byte{
		Fingerprint("openai", "image.edit", "model", "channel", "application/json", []byte(`{"a":1}`)),
		Fingerprint("openai", "image.generate", "model", "channel", "application/json", []byte(`{ "a":1}`)),
		Fingerprint("opena", "iimage.generate", "model", "channel", "application/json", []byte(`{"a":1}`)),
	} {
		if changed == base {
			t.Fatal("different request produced same fingerprint")
		}
	}
}
