package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	chargebilling "github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/chatbilling"
	"github.com/nativegatewayhq/gateway/internal/chatpricing"
	"github.com/nativegatewayhq/gateway/internal/networkauth"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/ratelimit"
	geminioperation "github.com/nativegatewayhq/gateway/operations/gemini"
	"github.com/nativegatewayhq/gateway/providers/google"
)

type stubAuthenticator struct {
	principal apikey.Principal
	err       error
	raw       string
	calls     int
}

func (authenticator *stubAuthenticator) Authenticate(_ context.Context, raw string) (apikey.Principal, error) {
	authenticator.calls++
	authenticator.raw = raw
	return authenticator.principal, authenticator.err
}

type stubExecutor struct {
	response *http.Response
	err      error
	request  google.GenerateContentRequest
	calls    int
	panic    bool
}

type geminiPanicReader struct{}

func (geminiPanicReader) Read([]byte) (int, error) { panic("body read before rate limit response") }

func (executor *stubExecutor) GenerateContent(_ context.Context, request google.GenerateContentRequest) (*http.Response, error) {
	executor.calls++
	executor.request = request
	if executor.panic {
		panic("provider-secret prompt-secret")
	}
	return executor.response, executor.err
}

func testLogger(output io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(output, nil))
}

func geminiRequest(body io.Reader) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-image:generateContent?safe=value&key=service-key", body)
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "google-genai-test")
	request.Header.Set("x-goog-api-client", "google-genai-sdk/test")
	return request
}

func TestGenerateContentNativeSuccessPassThrough(t *testing.T) {
	t.Parallel()
	providerBody := `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"aW1hZ2UtYnl0ZXM="}}]}}]}`
	executor := &stubExecutor{response: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":      {"application/json"},
			"X-Goog-Request-Id": {"google-request-1"},
			"Set-Cookie":        {"upstream=secret"},
		},
		Body: io.NopCloser(strings.NewReader(providerBody)),
	}}
	authenticator := &stubAuthenticator{principal: apikey.Principal{APIKeyID: "key_1"}}
	var logs bytes.Buffer
	handler := NewHandler(testLogger(&logs), authenticator, executor, 1024)
	requestBody := ` {"contents":[{"parts":[{"text":"draw a cat"}]}]} `
	request := geminiRequest(strings.NewReader(requestBody))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != providerBody {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	if response.Header().Get("X-Goog-Request-Id") != "google-request-1" || response.Header().Get("Set-Cookie") != "" {
		t.Fatalf("headers = %v", response.Header())
	}
	forwarded, _ := io.ReadAll(executor.request.Body)
	if string(forwarded) != requestBody || executor.request.Model != "gemini-image" || executor.request.Query.Get("safe") != "value" {
		t.Fatalf("upstream request = model=%q query=%v body=%q", executor.request.Model, executor.request.Query, forwarded)
	}
	if executor.request.APIClient != "google-genai-sdk/test" {
		t.Fatalf("API client header = %q", executor.request.APIClient)
	}
	if authenticator.raw != "service-key" || executor.calls != 1 {
		t.Fatalf("auth=%q calls=%d", authenticator.raw, executor.calls)
	}
	for _, secret := range []string{"service-key", "draw a cat", "aW1hZ2UtYnl0ZXM="} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs leaked %q: %s", secret, logs.String())
		}
	}
}

func TestLLMGenerateContentUsesConfiguredOperationAndPreservesNativeBytes(t *testing.T) {
	models, err := geminioperation.NewRegistry([]string{"gemini-2.5-pro"})
	if err != nil {
		t.Fatal(err)
	}
	providerBody := `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"weather","args":{"city":"Seoul"}}}]}}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":4,"totalTokenCount":12}}`
	executor := &stubExecutor{response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(providerBody))}}
	principal := apikey.Principal{ModelAccessMode: apikey.ModelAccessAllowlist, ModelPermissions: []apikey.ModelPermission{{Protocol: "gemini", Operation: "chat.completions", Model: "gemini-2.5-pro"}}}
	handler := NewHandlerWithLLMModels(testLogger(io.Discard), &stubAuthenticator{principal: principal}, executor, 4096, nil, models)
	requestBody := ` {"contents":[{"parts":[{"text":"secret"}]}],"systemInstruction":{"parts":[{"text":"system"}]},"tools":[{"functionDeclarations":[{"name":"weather"}]}],"futureField":{"kept":true}} `
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent?key=service-key", strings.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	forwarded, _ := io.ReadAll(executor.request.Body)
	if response.Code != 200 || response.Body.String() != providerBody || string(forwarded) != requestBody || executor.request.Model != "gemini-2.5-pro" {
		t.Fatalf("response=%d %q request=%q model=%q", response.Code, response.Body.String(), forwarded, executor.request.Model)
	}
}

func TestManagedLLMGenerateContentFailsBeforeBodyBillingAndProvider(t *testing.T) {
	models, _ := geminioperation.NewRegistry([]string{"gemini-2.5-pro"})
	bill := &geminiBillingFake{}
	executor := &stubExecutor{}
	handler := NewBillableHandlerWithLLMModels(testLogger(io.Discard), &stubAuthenticator{}, nil, executor, 4096, bill, nil, nil, models)
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent?key=service-key", geminiPanicReader{})
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusPreconditionFailed || executor.calls != 0 || len(bill.events) != 0 || bill.replayCalls != 0 {
		t.Fatalf("response=%d body=%s provider=%d billing_events=%v replay=%d", response.Code, response.Body.String(), executor.calls, bill.events, bill.replayCalls)
	}
}

type geminiLLMBillingFake struct {
	beginRequest chatbilling.BeginRequest
	usage        chatpricing.Usage
	snapshot     chargebilling.ResponseSnapshot
	charge       chatbilling.Charge
	reason       string
	beginCalls   int
	replay       bool
	released     bool
}

func (fake *geminiLLMBillingFake) Begin(_ context.Context, request chatbilling.BeginRequest) (chatbilling.Charge, error) {
	fake.beginCalls++
	fake.beginRequest = request
	charge := fake.charge
	if charge.ID == "" {
		charge.ID = "chc_00000000000000000000000000000001"
	}
	charge.MaximumInputTokens = request.MaximumInputTokens
	charge.MaximumOutputTokens = request.MaximumOutputTokens
	return charge, nil
}
func (fake *geminiLLMBillingFake) Replay(_ context.Context, _ chatbilling.BeginRequest) (chatbilling.Charge, bool, error) {
	return fake.charge, fake.replay, nil
}
func (fake *geminiLLMBillingFake) CompleteUsage(_ context.Context, _ string, usage chatpricing.Usage, snapshot chargebilling.ResponseSnapshot) (chatbilling.Charge, error) {
	fake.usage, fake.snapshot = usage, snapshot
	return chatbilling.Charge{Response: snapshot}, nil
}
func (fake *geminiLLMBillingFake) Release(_ context.Context, _ string, snapshot chargebilling.ResponseSnapshot) (chatbilling.Charge, error) {
	fake.released, fake.snapshot = true, snapshot
	return chatbilling.Charge{Response: snapshot}, nil
}
func (fake *geminiLLMBillingFake) MarkReconciling(_ context.Context, _ string, reason string, _ *chargebilling.ResponseSnapshot) error {
	fake.reason = reason
	return nil
}
func (fake *geminiLLMBillingFake) MarkReconcilingUsage(_ context.Context, _ string, reason string, _ *chargebilling.ResponseSnapshot, usage chatpricing.Usage) error {
	fake.reason, fake.usage = reason, usage
	return nil
}

func TestManagedLLMReservesAndCapturesStrictGeminiUsage(t *testing.T) {
	models, _ := geminioperation.NewRegistryWithLimits([]string{"gemini-2.5-pro"}, map[string]geminioperation.Limits{"gemini-2.5-pro": {MaximumInputTokens: 4096, MaximumOutputTokens: 100}})
	providerBody := `{"candidates":[],"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":3,"toolUsePromptTokenCount":2,"candidatesTokenCount":5,"thoughtsTokenCount":4,"totalTokenCount":19}}`
	executor := &stubExecutor{response: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(providerBody))}}
	tokenBilling := &geminiLLMBillingFake{}
	imageBilling := &geminiBillingFake{}
	principal := apikey.Principal{OrganizationID: "org_test", ProjectID: "project_test", APIKeyID: "key_test"}
	handler := NewBillableHandlerWithLLMTokenBilling(testLogger(io.Discard), &stubAuthenticator{principal: principal}, nil, executor, 4096, imageBilling, tokenBilling, nil, nil, models)
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent?key=service-key", strings.NewReader(`{"contents":[{"parts":[{"text":"secret"}]}],"generationConfig":{"maxOutputTokens":20}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "gemini-managed-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || response.Body.String() != providerBody || executor.calls != 1 || tokenBilling.beginCalls != 1 {
		t.Fatalf("response=%d %s provider=%d begin=%d", response.Code, response.Body.String(), executor.calls, tokenBilling.beginCalls)
	}
	if tokenBilling.beginRequest.Protocol != "gemini" || tokenBilling.beginRequest.Operation != "chat.completions" || tokenBilling.beginRequest.MaximumOutputTokens != 20 || tokenBilling.beginRequest.Fingerprint == ([32]byte{}) {
		t.Fatalf("begin=%+v", tokenBilling.beginRequest)
	}
	if tokenBilling.usage != (chatpricing.Usage{PromptTokens: 12, CachedInputTokens: 3, CompletionTokens: 9, ToolUsePromptTokens: 2, ThoughtsTokens: 4}) {
		t.Fatalf("usage=%+v", tokenBilling.usage)
	}
}

func TestManagedLLMInvalidUsageHoldsReservation(t *testing.T) {
	models, _ := geminioperation.NewRegistryWithLimits([]string{"gemini-2.5-pro"}, map[string]geminioperation.Limits{"gemini-2.5-pro": {MaximumInputTokens: 4096, MaximumOutputTokens: 100}})
	executor := &stubExecutor{response: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":99}}`))}}
	tokenBilling := &geminiLLMBillingFake{}
	handler := NewBillableHandlerWithLLMTokenBilling(testLogger(io.Discard), &stubAuthenticator{principal: apikey.Principal{OrganizationID: "org_test", ProjectID: "project_test", APIKeyID: "key_test"}}, nil, executor, 4096, &geminiBillingFake{}, tokenBilling, nil, nil, models)
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent?key=service-key", strings.NewReader(`{"contents":[],"generationConfig":{"maxOutputTokens":20}}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || tokenBilling.reason != "usage_invalid" || tokenBilling.released {
		t.Fatalf("response=%d reason=%q released=%v", response.Code, tokenBilling.reason, tokenBilling.released)
	}
}

func TestGeminiManagedParsersRejectDuplicateAndInvalidUsage(t *testing.T) {
	for _, body := range []string{`{"generationConfig":{"maxOutputTokens":1,"maxOutputTokens":2}}`, `{"generationConfig":{"maxOutputTokens":1.5}}`, `{"generationConfig":{}}`} {
		if _, err := extractGeminiMaximumOutput([]byte(body)); err == nil {
			t.Fatalf("accepted limit %s", body)
		}
	}
	for _, body := range []string{`{"usageMetadata":{"promptTokenCount":1,"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`, `{"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":-1,"totalTokenCount":0}}`, `{"usageMetadata":{"promptTokenCount":1,"cachedContentTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":2}}`} {
		if _, err := extractGeminiUsage([]byte(body)); err == nil {
			t.Fatalf("accepted usage %s", body)
		}
	}
}

func TestGenerateContentMapsRateLimitBeforeBodyAndProvider(t *testing.T) {
	reset := time.Unix(2_000_000_000, 0)
	for _, test := range []struct {
		name   string
		err    error
		status int
		marker string
	}{
		{"limited", &ratelimit.LimitError{Decision: ratelimit.Decision{Limit: 30, RetryAfter: time.Second, ResetAt: reset}}, 429, "RESOURCE_EXHAUSTED"},
		{"unavailable", ratelimit.ErrUnavailable, 503, "UNAVAILABLE"},
		{"network denied", &networkauth.DeniedError{APIKeyID: "key_test", ProjectID: "project_test"}, 403, "PERMISSION_DENIED"},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor := &stubExecutor{}
			handler := NewHandler(testLogger(io.Discard), &stubAuthenticator{err: test.err}, executor, 1024)
			request := geminiRequest(geminiPanicReader{})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status || !strings.Contains(response.Body.String(), test.marker) || executor.calls != 0 {
				t.Fatalf("response=%d %s calls=%d", response.Code, response.Body.String(), executor.calls)
			}
			if test.status == 429 && (response.Header().Get("Retry-After") != "1" || response.Header().Get("X-RateLimit-Limit") != "30") {
				t.Fatalf("headers=%v", response.Header())
			}
		})
	}
}

func TestGenerateContentEnforcesModelPermissionBeforeBodyAndProvider(t *testing.T) {
	principal := apikey.Principal{APIKeyID: "key_denied", ProjectID: "project_denied", ModelAccessMode: apikey.ModelAccessAllowlist, ModelPermissions: []apikey.ModelPermission{{Protocol: "openai", Operation: "image.generate", Model: "gemini-image"}}}
	executor := &stubExecutor{}
	handler := NewHandler(testLogger(io.Discard), &stubAuthenticator{principal: principal}, executor, 1024)
	request := geminiRequest(geminiPanicReader{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 403 || !strings.Contains(response.Body.String(), "PERMISSION_DENIED") || executor.calls != 0 {
		t.Fatalf("response=%d %s calls=%d", response.Code, response.Body.String(), executor.calls)
	}
}

func TestGoogleNativeErrorPassThrough(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			body := fmt.Sprintf(`{"error":{"code":%d,"message":"native error","status":"NATIVE"}}`, status)
			executor := &stubExecutor{response: &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": {"application/json"}, "Retry-After": {"10"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}}
			handler := NewHandler(testLogger(io.Discard), &stubAuthenticator{}, executor, 1024)
			request := geminiRequest(strings.NewReader(`{}`))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != status || response.Body.String() != body || response.Header().Get("Retry-After") != "10" {
				t.Fatalf("response = %d %q headers=%v", response.Code, response.Body.String(), response.Header())
			}
		})
	}
}

func TestSupportedCredentialLocations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(*http.Request)
	}{
		{"query", func(*http.Request) {}},
		{"bearer", func(request *http.Request) {
			request.URL.RawQuery = "safe=value"
			request.Header.Set("Authorization", "Bearer service-key")
		}},
		{"api key", func(request *http.Request) {
			request.URL.RawQuery = "safe=value"
			request.Header.Set("x-api-key", "service-key")
		}},
		{"google key", func(request *http.Request) {
			request.URL.RawQuery = "safe=value"
			request.Header.Set("x-goog-api-key", "service-key")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authenticator := &stubAuthenticator{}
			executor := &stubExecutor{response: &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}}
			handler := NewHandler(testLogger(io.Discard), authenticator, executor, 1024)
			request := geminiRequest(strings.NewReader(`{malformed-json`))
			test.setup(request)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || authenticator.raw != "service-key" || executor.calls != 1 {
				t.Fatalf("status=%d raw=%q calls=%d", response.Code, authenticator.raw, executor.calls)
			}
			body, _ := io.ReadAll(executor.request.Body)
			if string(body) != `{malformed-json` {
				t.Fatalf("body was transformed: %q", body)
			}
		})
	}
}

func TestAuthenticationFailuresDoNotReadBodyOrCallProvider(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		setup   func(*http.Request)
		authErr error
		status  int
		code    string
	}{
		{"missing", func(request *http.Request) { request.URL.RawQuery = "" }, nil, http.StatusUnauthorized, "UNAUTHENTICATED"},
		{"ambiguous", func(request *http.Request) { request.Header.Set("x-api-key", "second") }, nil, http.StatusBadRequest, "INVALID_ARGUMENT"},
		{"unknown", func(*http.Request) {}, apikey.ErrUnauthorized, http.StatusUnauthorized, "UNAUTHENTICATED"},
		{"store unavailable", func(*http.Request) {}, apikey.ErrUnavailable, http.StatusServiceUnavailable, "UNAVAILABLE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := &countingReader{content: []byte(`{"secret":"prompt"}`)}
			authenticator := &stubAuthenticator{err: test.authErr}
			executor := &stubExecutor{}
			handler := NewHandler(testLogger(io.Discard), authenticator, executor, 1024)
			request := geminiRequest(body)
			test.setup(request)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status || !strings.Contains(response.Body.String(), test.code) {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
			if body.reads != 0 || executor.calls != 0 {
				t.Fatalf("body reads=%d executor calls=%d", body.reads, executor.calls)
			}
		})
	}
}

func TestBodyMediaModelAndMethodValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		request    func() *http.Request
		maximum    int64
		wantStatus int
	}{
		{"method", func() *http.Request {
			request := geminiRequest(strings.NewReader(`{}`))
			request.Method = http.MethodGet
			return request
		}, 1024, http.StatusMethodNotAllowed},
		{"model", func() *http.Request {
			request := geminiRequest(strings.NewReader(`{}`))
			request.URL.Path = "/v1beta/models/bad%2Fmodel:generateContent"
			request.URL.RawPath = request.URL.Path
			return request
		}, 1024, http.StatusBadRequest},
		{"media", func() *http.Request {
			request := geminiRequest(strings.NewReader(`{}`))
			request.Header.Set("Content-Type", "text/plain")
			return request
		}, 1024, http.StatusBadRequest},
		{"fixed length", func() *http.Request {
			request := geminiRequest(strings.NewReader("12345"))
			request.ContentLength = 5
			return request
		}, 4, http.StatusRequestEntityTooLarge},
		{"chunked", func() *http.Request {
			request := geminiRequest(strings.NewReader("12345"))
			request.ContentLength = -1
			return request
		}, 4, http.StatusRequestEntityTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &stubExecutor{}
			handler := NewHandler(testLogger(io.Discard), &stubAuthenticator{}, executor, test.maximum)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, test.request())
			if response.Code != test.wantStatus || executor.calls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, executor.calls, response.Body.String())
			}
		})
	}
}

func TestExecutorErrorMappingAndPanicRecovery(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		err    error
		panic  bool
		status int
		code   string
	}{
		{"credential", providercredentials.ErrCredentialUnavailable, false, http.StatusServiceUnavailable, "UNAVAILABLE"},
		{"timeout", google.ErrTimeout, false, http.StatusGatewayTimeout, "DEADLINE_EXCEEDED"},
		{"canceled", google.ErrCanceled, false, 499, "CANCELLED"},
		{"upstream", google.ErrUpstream, false, http.StatusBadGateway, "UNAVAILABLE"},
		{"internal", errors.New("provider-secret"), false, http.StatusInternalServerError, "INTERNAL"},
		{"panic", nil, true, http.StatusInternalServerError, "INTERNAL"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			executor := &stubExecutor{err: test.err, panic: test.panic}
			handler := NewHandler(testLogger(&logs), &stubAuthenticator{}, executor, 1024)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, geminiRequest(strings.NewReader(`{"prompt":"prompt-secret"}`)))
			if response.Code != test.status || !strings.Contains(response.Body.String(), test.code) {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
			for _, secret := range []string{"provider-secret", "prompt-secret"} {
				if strings.Contains(response.Body.String()+logs.String(), secret) {
					t.Fatalf("output leaked %q", secret)
				}
			}
		})
	}
}

func TestGatewayErrorEnvelope(t *testing.T) {
	t.Parallel()
	handler := NewHandler(testLogger(io.Discard), &stubAuthenticator{err: apikey.ErrUnauthorized}, &stubExecutor{}, 1024)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, geminiRequest(strings.NewReader(`{}`)))
	var envelope errorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != http.StatusUnauthorized || envelope.Error.Status != "UNAUTHENTICATED" || envelope.Error.Message == "" {
		t.Fatalf("error = %+v", envelope.Error)
	}
}

func TestTruncatedUpstreamResponseKeepsOriginalStatus(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	executor := &stubExecutor{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(&failingReader{}),
	}}
	handler := NewHandler(testLogger(&logs), &stubAuthenticator{}, executor, 1024)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, geminiRequest(strings.NewReader(`{}`)))
	if response.Code != http.StatusOK || response.Body.String() != "partial" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	if !strings.Contains(logs.String(), "response_copy_failed") || strings.Contains(logs.String(), "provider-secret") {
		t.Fatalf("logs = %s", logs.String())
	}
}

type countingReader struct {
	content []byte
	reads   int
}

type failingReader struct{ sent bool }

func (reader *failingReader) Read(output []byte) (int, error) {
	if reader.sent {
		return 0, errors.New("truncated provider-secret")
	}
	reader.sent = true
	return copy(output, "partial"), nil
}

func (reader *countingReader) Read(output []byte) (int, error) {
	reader.reads++
	if len(reader.content) == 0 {
		return 0, io.EOF
	}
	count := copy(output, reader.content)
	reader.content = reader.content[count:]
	return count, nil
}
