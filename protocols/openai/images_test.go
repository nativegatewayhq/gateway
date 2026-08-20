package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/networkauth"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/ratelimit"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	"github.com/nativegatewayhq/gateway/providers/openaiimages"
)

type authFunc func(context.Context, string) (apikey.Principal, error)

func (function authFunc) Authenticate(ctx context.Context, key string) (apikey.Principal, error) {
	return function(ctx, key)
}

type executorFunc func(context.Context, openaiimages.Request) (*http.Response, error)

type rateLimiterFunc func(context.Context, string, ratelimit.Policy) (ratelimit.Decision, error)

func (function rateLimiterFunc) Allow(ctx context.Context, key string, policy ratelimit.Policy) (ratelimit.Decision, error) {
	return function(ctx, key, policy)
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("body read before rate limit response") }

func (function executorFunc) Generate(ctx context.Context, request openaiimages.Request) (*http.Response, error) {
	return function(ctx, request)
}

func testRegistry(t *testing.T) *imageoperation.Registry {
	t.Helper()
	registry, err := imageoperation.NewRegistry(
		imageoperation.ModelRoute{Protocol: "openai", Model: "gpt-image-1", Owner: "openai", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}, {Operation: imageoperation.Edit, MediaType: imageoperation.Multipart}}, Policy: imageoperation.Fixed, FixedCandidateID: "candidate_openai", Candidates: []imageoperation.ChannelCandidate{{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "gpt-image-1", ChannelID: "channel_00000000000000000000000000000001", Enabled: true}}},
		imageoperation.ModelRoute{Protocol: "openai", Model: "grok-imagine-image-quality", Owner: "xai", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}, {Operation: imageoperation.Edit, MediaType: imageoperation.JSON}}, Policy: imageoperation.Fixed, FixedCandidateID: "candidate_xai", Candidates: []imageoperation.ChannelCandidate{{ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "grok-imagine-image-quality", ChannelID: "channel_00000000000000000000000000000002", Enabled: true}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func acceptingAuth(t *testing.T) Authenticator {
	t.Helper()
	return authFunc(func(_ context.Context, key string) (apikey.Principal, error) {
		if key != "service-secret" {
			t.Fatalf("key = %q", key)
		}
		return apikey.Principal{}, nil
	})
}

func TestImagesHandlerRoutesExactModelAndPreservesBytes(t *testing.T) {
	t.Parallel()
	body := "{\n  \"prompt\": \"sensitive prompt\", \"model\":\"grok-imagine-image-quality\", \"extra\": {\"x\":1}\n}"
	var openAICalls, xAICalls int
	handler := NewImagesHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), testRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			openAICalls++
			return nil, errors.New("wrong provider")
		}),
		providercredentials.XAI: executorFunc(func(_ context.Context, request openaiimages.Request) (*http.Response, error) {
			xAICalls++
			got, _ := io.ReadAll(request.Body)
			if string(got) != body {
				t.Fatalf("body changed: %q", got)
			}
			header := make(http.Header)
			header.Set("Content-Type", "application/json")
			header.Set("Retry-After", "2")
			header.Set("Set-Cookie", "secret=cookie")
			return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(`{"data":[{"url":"https://temporary.invalid/image"}],"usage":{"cost_in_usd_ticks":200000000}}`))}, nil
		}),
	}, 1024*1024)

	response := requestImages(handler, http.MethodPost, body, "Authorization", "Bearer service-secret")
	if response.Code != 200 || openAICalls != 0 || xAICalls != 1 {
		t.Fatalf("status/calls = %d/%d/%d: %s", response.Code, openAICalls, xAICalls, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "cost_in_usd_ticks") || response.Header().Get("Retry-After") != "2" || response.Header().Get("Set-Cookie") != "" {
		t.Fatalf("response not preserved safely: headers=%v body=%s", response.Header(), response.Body.String())
	}
}

func TestImagesHandlerAcceptsAllCredentialLocations(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, target, header, value string }{
		{"bearer", "/v1/images/generations", "Authorization", "Bearer service-secret"},
		{"x-api-key", "/v1/images/generations", "x-api-key", "service-secret"},
		{"x-goog-api-key", "/v1/images/generations", "x-goog-api-key", "service-secret"},
		{"query", "/v1/images/generations?key=service-secret", "", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := successHandler(t)
			request := httptest.NewRequest(http.MethodPost, test.target, strings.NewReader(`{"model":"gpt-image-1","prompt":"p"}`))
			request.Header.Set("Content-Type", "application/json")
			if test.header != "" {
				request.Header.Set(test.header, test.value)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 200 {
				t.Fatalf("status = %d: %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestImagesHandlerRejectsMissingAndAmbiguousCredentials(t *testing.T) {
	t.Parallel()
	calls := 0
	handler := NewImagesHandler(slog.Default(), authFunc(func(context.Context, string) (apikey.Principal, error) {
		calls++
		return apikey.Principal{}, apikey.ErrUnauthorized
	}), testRegistry(t), nil, 1024)

	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 401 || calls != 0 {
		t.Fatalf("missing credential status/calls = %d/%d", response.Code, calls)
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/images/generations?key=service-secret", strings.NewReader(`{"model":"gpt-image-1"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer service-secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 400 || calls != 0 {
		t.Fatalf("ambiguous credential status/calls = %d/%d", response.Code, calls)
	}
}

func TestImagesHandlerMapsRateLimitBeforeBodyAndProvider(t *testing.T) {
	reset := time.Unix(2_000_000_000, 0)
	for _, test := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"limited", &ratelimit.LimitError{Decision: ratelimit.Decision{Limit: 60, Remaining: 0, RetryAfter: 1500 * time.Millisecond, ResetAt: reset}}, 429, "rate_limit_exceeded"},
		{"unavailable", ratelimit.ErrUnavailable, 503, "rate_limit_unavailable"},
		{"network denied", &networkauth.DeniedError{APIKeyID: "key_test", ProjectID: "project_test"}, 403, "network_not_allowed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			providerCalls := 0
			handler := NewImagesHandler(slog.Default(), authFunc(func(context.Context, string) (apikey.Principal, error) { return apikey.Principal{}, test.err }), testRegistry(t), map[providercredentials.ProviderID]Executor{
				providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { providerCalls++; return nil, nil }),
			}, 1024)
			request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", panicReader{})
			request.Header.Set("Authorization", "Bearer service-secret")
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status || !strings.Contains(response.Body.String(), test.code) || providerCalls != 0 {
				t.Fatalf("response=%d %s calls=%d", response.Code, response.Body.String(), providerCalls)
			}
			if test.status == 429 && (response.Header().Get("Retry-After") != "2" || response.Header().Get("X-RateLimit-Limit") != "60" || response.Header().Get("X-RateLimit-Remaining") != "0" || response.Header().Get("X-RateLimit-Reset") != "2000000000") {
				t.Fatalf("headers=%v", response.Header())
			}
		})
	}
}

func TestImagesHandlerReturnsAllowedRateLimitHeaders(t *testing.T) {
	principal := apikey.Principal{RateLimitState: &apikey.RateLimitState{Limit: 60, Remaining: 4, ResetAt: time.Unix(2_000_000_000, 0)}}
	handler := NewImagesHandler(slog.Default(), authFunc(func(context.Context, string) (apikey.Principal, error) { return principal, nil }), testRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}),
	}, 1024)
	response := requestImages(handler, http.MethodPost, `{"model":"gpt-image-1"}`, "Authorization", "Bearer service-secret")
	if response.Code != 200 || response.Header().Get("X-RateLimit-Limit") != "60" || response.Header().Get("X-RateLimit-Remaining") != "4" {
		t.Fatalf("response=%d headers=%v", response.Code, response.Header())
	}
}

func TestImagesHandlerEnforcesLogicalModelPermissionAfterRateLimit(t *testing.T) {
	rateCalls, providerCalls := 0, 0
	principal := apikey.Principal{APIKeyID: "key_policy", ProjectID: "project_policy", RateLimit: apikey.RateLimitPolicy{RequestsPerMinute: 60, Burst: 2}, ModelAccessMode: apikey.ModelAccessAllowlist, ModelPermissions: []apikey.ModelPermission{{Protocol: "openai", Operation: "image.generate", Model: "grok-imagine-image-quality"}}}
	guard, err := ratelimit.NewGuardedAuthenticator(authFunc(func(context.Context, string) (apikey.Principal, error) { return principal, nil }), rateLimiterFunc(func(context.Context, string, ratelimit.Policy) (ratelimit.Decision, error) {
		rateCalls++
		return ratelimit.Decision{Allowed: true, Limit: 60, Remaining: 1, ResetAt: time.Unix(2_000_000_000, 0)}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	handler := NewImagesHandler(slog.Default(), guard, testRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { providerCalls++; return nil, nil }),
		providercredentials.XAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			providerCalls++
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}),
	}, 1024)
	response := requestImages(handler, http.MethodPost, `{"model":"gpt-image-1"}`, "Authorization", "Bearer service-secret")
	if response.Code != 403 || !strings.Contains(response.Body.String(), "model_not_allowed") || rateCalls != 1 || providerCalls != 0 {
		t.Fatalf("response=%d %s rate=%d provider=%d", response.Code, response.Body.String(), rateCalls, providerCalls)
	}
	unknown := requestImages(handler, http.MethodPost, `{"model":"missing-model"}`, "Authorization", "Bearer service-secret")
	if unknown.Code != 404 || rateCalls != 2 {
		t.Fatalf("unknown=%d %s rate=%d", unknown.Code, unknown.Body.String(), rateCalls)
	}
	allowed := requestImages(handler, http.MethodPost, `{"model":"grok-imagine-image-quality"}`, "Authorization", "Bearer service-secret")
	if allowed.Code != 200 || rateCalls != 3 || providerCalls != 1 {
		t.Fatalf("allowed=%d rate=%d provider=%d", allowed.Code, rateCalls, providerCalls)
	}
}

func TestModelAuthorizationLogUsesLogicalIdentityWithoutProviderModelOrBody(t *testing.T) {
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "openai", Model: "logical-denied", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.Fixed, FixedCandidateID: "candidate", Candidates: []imageoperation.ChannelCandidate{{ID: "candidate", Provider: providercredentials.OpenAI, ProviderModel: "provider-secret-model", ChannelID: "channel_00000000000000000000000000000001", Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	principal := apikey.Principal{APIKeyID: "key_safe", ProjectID: "project_safe", ModelAccessMode: apikey.ModelAccessAllowlist, ModelPermissions: []apikey.ModelPermission{{Protocol: "openai", Operation: "image.generate", Model: "another-model"}}}
	var logs bytes.Buffer
	handler := NewImagesHandler(slog.New(slog.NewJSONHandler(&logs, nil)), authFunc(func(context.Context, string) (apikey.Principal, error) { return principal, nil }), registry, nil, 1024)
	response := requestImages(handler, http.MethodPost, `{"model":"logical-denied","prompt":"body-secret"}`, "Authorization", "Bearer service-secret")
	if response.Code != 403 || !strings.Contains(logs.String(), "logical-denied") {
		t.Fatalf("response=%d logs=%s", response.Code, logs.String())
	}
	for _, secret := range []string{"provider-secret-model", "body-secret", "service-secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("log leaked %q: %s", secret, logs.String())
		}
	}
}

func TestImagesHandlerRejectsInvalidRequestsBeforeExecutor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		method      string
		body        string
		contentType string
		want        int
	}{
		{"method", http.MethodGet, `{"model":"gpt-image-1"}`, "application/json", 405},
		{"media", http.MethodPost, `{"model":"gpt-image-1"}`, "text/plain", 400},
		{"empty", http.MethodPost, ``, "application/json", 400},
		{"malformed", http.MethodPost, `{"model":`, "application/json", 400},
		{"array", http.MethodPost, `[]`, "application/json", 400},
		{"missing", http.MethodPost, `{"prompt":"p"}`, "application/json", 400},
		{"duplicate", http.MethodPost, `{"model":"gpt-image-1","model":"gpt-image-1"}`, "application/json", 400},
		{"unknown", http.MethodPost, `{"model":"gpt-image-1-preview"}`, "application/json", 404},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			handler := NewImagesHandler(slog.Default(), acceptingAuth(t), testRegistry(t), map[providercredentials.ProviderID]Executor{
				providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { calls++; return nil, nil }),
			}, 1024*1024)
			request := httptest.NewRequest(test.method, "/v1/images/generations", strings.NewReader(test.body))
			request.Header.Set("Content-Type", test.contentType)
			request.Header.Set("Authorization", "Bearer service-secret")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want || calls != 0 {
				t.Fatalf("status/calls = %d/%d, want %d/0: %s", response.Code, calls, test.want, response.Body.String())
			}
			assertOpenAIError(t, response)
		})
	}
}

func TestImagesHandlerBodyLimitNativeErrorAndGatewayErrors(t *testing.T) {
	t.Parallel()
	limited := NewImagesHandler(slog.Default(), acceptingAuth(t), testRegistry(t), nil, 8)
	response := requestImages(limited, http.MethodPost, `{"model":"gpt-image-1"}`, "Authorization", "Bearer service-secret")
	if response.Code != 413 {
		t.Fatalf("body limit status = %d", response.Code)
	}
	chunkedRequest := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1"}`))
	chunkedRequest.ContentLength = -1
	chunkedRequest.Header.Set("Content-Type", "application/json")
	chunkedRequest.Header.Set("Authorization", "Bearer service-secret")
	response = httptest.NewRecorder()
	limited.ServeHTTP(response, chunkedRequest)
	if response.Code != 413 {
		t.Fatalf("chunked body limit status = %d", response.Code)
	}

	native := `{"error":{"message":"provider rate limit","type":"rate_limit_error"}}`
	handler := NewImagesHandler(slog.Default(), acceptingAuth(t), testRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(native))}, nil
		}),
	}, 1024)
	response = requestImages(handler, http.MethodPost, `{"model":"gpt-image-1"}`, "Authorization", "Bearer service-secret")
	if response.Code != 429 || response.Body.String() != native {
		t.Fatalf("native error changed: %d %q", response.Code, response.Body.String())
	}

	for _, test := range []struct {
		err  error
		want int
	}{
		{openaiimages.ErrTimeout, 504}, {openaiimages.ErrCanceled, 499}, {openaiimages.ErrUpstream, 502}, {providercredentials.ErrCredentialUnavailable, 503},
	} {
		handler = NewImagesHandler(slog.Default(), acceptingAuth(t), testRegistry(t), map[providercredentials.ProviderID]Executor{
			providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { return nil, test.err }),
		}, 1024)
		response = requestImages(handler, http.MethodPost, `{"model":"gpt-image-1"}`, "Authorization", "Bearer service-secret")
		if response.Code != test.want {
			t.Fatalf("error %v status = %d, want %d", test.err, response.Code, test.want)
		}
	}
}

func TestImagesHandlerRedactsSecretsFromLogs(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	handler := NewImagesHandler(slog.New(slog.NewJSONHandler(&logs, nil)), acceptingAuth(t), testRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { panic("provider-secret") }),
	}, 1024)
	response := requestImages(handler, http.MethodPost, `{"model":"gpt-image-1","prompt":"secret-prompt","b64_json":"secret-image"}`, "Authorization", "Bearer service-secret")
	if response.Code != 500 {
		t.Fatalf("status = %d", response.Code)
	}
	combined := logs.String() + response.Body.String()
	for _, secret := range []string{"provider-secret", "service-secret", "secret-prompt", "secret-image"} {
		if strings.Contains(combined, secret) {
			t.Fatalf("leaked %q: %s", secret, combined)
		}
	}
}

func successHandler(t *testing.T) *Handler {
	t.Helper()
	return NewImagesHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), testRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}, nil
		}),
	}, 1024)
}

func requestImages(handler http.Handler, method, body, header, value string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "/v1/images/generations", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(header, value)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertOpenAIError(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	var envelope errorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Error.Type == "" || envelope.Error.Code == "" {
		t.Fatalf("invalid OpenAI error: %v %+v", err, envelope)
	}
}
