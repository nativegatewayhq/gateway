package apikey

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractCredentialLocations(t *testing.T) {
	tests := []struct{ name, target, header, value string }{
		{"bearer", "/", "Authorization", "Bearer ngw_sk_value"},
		{"api key", "/", "x-api-key", "ngw_sk_value"},
		{"google", "/", "x-goog-api-key", "ngw_sk_value"},
		{"query", "/?key=ngw_sk_value", "", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", test.target, nil)
			if test.header != "" {
				request.Header.Set(test.header, test.value)
			}
			got, err := Extract(request)
			if err != nil || got != "ngw_sk_value" {
				t.Fatalf("Extract() = %q, %v", got, err)
			}
		})
	}
}

func TestExtractRejectsUnsafeInputs(t *testing.T) {
	request := httptest.NewRequest("GET", "/", nil)
	if _, err := Extract(request); !errors.Is(err, ErrMissing) {
		t.Fatalf("missing error = %v", err)
	}
	request = httptest.NewRequest("GET", "/?key=a&key=b", nil)
	if _, err := Extract(request); !errors.Is(err, ErrMalformed) {
		t.Fatalf("duplicate query error = %v", err)
	}
	request = httptest.NewRequest("GET", "/", nil)
	request.Header.Add("x-api-key", "ngw_sk_a")
	request.Header.Add("x-api-key", "ngw_sk_a")
	if _, err := Extract(request); !errors.Is(err, ErrMalformed) {
		t.Fatalf("duplicate header error = %v", err)
	}
	request = httptest.NewRequest("GET", "/?key=ngw_sk_a", nil)
	request.Header.Set("x-api-key", "ngw_sk_a")
	if _, err := Extract(request); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("ambiguous error = %v", err)
	}
	request = httptest.NewRequest("GET", "/", nil)
	request.Header.Set("Authorization", "Basic secret")
	if _, err := Extract(request); !errors.Is(err, ErrMalformed) {
		t.Fatalf("scheme error = %v", err)
	}
	request = httptest.NewRequest("GET", "/", nil)
	request.Header.Set("x-api-key", strings.Repeat("a", maxKeyLength+1))
	if _, err := Extract(request); !errors.Is(err, ErrMalformed) {
		t.Fatalf("length error = %v", err)
	}
	request = httptest.NewRequest("GET", "/", nil)
	request.Header["X-Api-Key"] = []string{"ngw_sk_a\x7f"}
	if _, err := Extract(request); !errors.Is(err, ErrMalformed) {
		t.Fatalf("control character error = %v", err)
	}
}
