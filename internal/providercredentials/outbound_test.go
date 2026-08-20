package providercredentials

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	values := map[string]string{
		"GATEWAY_GOOGLE_API_KEY": "google-upstream-secret",
		"GATEWAY_OPENAI_API_KEY": "openai-upstream-secret",
		"GATEWAY_XAI_API_KEY":    "xai-upstream-secret",
	}
	registry, err := Load(func(key string) (string, bool) { value, ok := values[key]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestPrepareOutboundSanitizesAndAppliesProviderCredential(t *testing.T) {
	t.Parallel()
	tests := []struct {
		provider ProviderID
		header   string
		want     string
	}{
		{Google, "x-goog-api-key", "google-upstream-secret"},
		{OpenAI, "Authorization", "Bearer openai-upstream-secret"},
		{XAI, "Authorization", "Bearer xai-upstream-secret"},
	}
	for _, test := range tests {
		t.Run(string(test.provider), func(t *testing.T) {
			inputURL, _ := url.Parse("https://service-secret@upstream.invalid/path?key=service-secret&API_KEY=other&safe=value&token=t")
			request := &http.Request{Method: http.MethodPost, URL: inputURL, Header: http.Header{
				"authorization":       {"Bearer service-secret"},
				"Proxy-Authorization": {"proxy-secret"},
				"X-Api-Key":           {"service-secret"},
				"X-Goog-Api-Key":      {"service-secret"},
				"Cookie":              {"session=secret"},
				"Content-Type":        {"application/json"},
			}}
			outbound, err := PrepareOutbound(request, test.provider, testRegistry(t))
			if err != nil {
				t.Fatal(err)
			}
			if outbound == request || outbound.URL == request.URL {
				t.Fatal("request or URL was not cloned")
			}
			if outbound.URL.User != nil || request.URL.User == nil {
				t.Fatal("URL userinfo was not sanitized on an independent copy")
			}
			if got := outbound.Header.Get(test.header); got != test.want {
				t.Fatalf("provider header = %q", got)
			}
			for header, values := range outbound.Header {
				if _, sensitive := sensitiveHeaders[strings.ToLower(header)]; sensitive && !strings.EqualFold(header, test.header) {
					t.Fatalf("sensitive header %s survived: %q", header, values)
				}
			}
			if outbound.URL.Query().Get("safe") != "value" || strings.Contains(strings.ToLower(outbound.URL.RawQuery), "key=") || strings.Contains(strings.ToLower(outbound.URL.RawQuery), "token=") {
				t.Fatalf("unsafe outbound query = %q", outbound.URL.RawQuery)
			}
			if request.Header["authorization"][0] != "Bearer service-secret" || request.URL.Query().Get("key") != "service-secret" {
				t.Fatal("input request was mutated")
			}
		})
	}
}

func TestPrepareOutboundHandlesMalformedAndEmptyRequests(t *testing.T) {
	t.Parallel()
	registry := testRegistry(t)
	if _, err := PrepareOutbound(nil, Google, registry); err == nil {
		t.Fatal("nil request accepted")
	}
	if _, err := PrepareOutbound(&http.Request{}, Google, registry); err == nil {
		t.Fatal("nil URL accepted")
	}
	request := &http.Request{URL: &url.URL{Scheme: "https", Host: "upstream.invalid"}}
	outbound, err := PrepareOutbound(request, Google, registry)
	if err != nil {
		t.Fatal(err)
	}
	if outbound.Header.Get("x-goog-api-key") != "google-upstream-secret" {
		t.Fatal("credential was not applied to initially empty headers")
	}
}

func TestCredentialScopeMismatchAndUnavailableFailBeforeMutation(t *testing.T) {
	t.Parallel()
	registry := testRegistry(t)
	credential, err := registry.Credential(Google)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://upstream.invalid", nil)
	if err := credential.Apply(request, OpenAI); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("scope error = %v", err)
	}
	if request.Header.Get("Authorization") != "" {
		t.Fatal("scope mismatch mutated request")
	}
	empty, err := Load(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareOutbound(request, Google, empty); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("unavailable error = %v", err)
	}
}

func TestRegistryConcurrentPreparation(t *testing.T) {
	t.Parallel()
	registry := testRegistry(t)
	request, _ := http.NewRequest(http.MethodGet, "https://upstream.invalid?key=service-secret", nil)
	var wait sync.WaitGroup
	for index := 0; index < 32; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			outbound, err := PrepareOutbound(request, Google, registry)
			if err != nil || outbound.Header.Get("x-goog-api-key") != "google-upstream-secret" {
				t.Errorf("PrepareOutbound() = %v, %v", outbound, err)
			}
		}()
	}
	wait.Wait()
}
