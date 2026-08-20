package openai

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	chargebilling "github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/pricing"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	"github.com/nativegatewayhq/gateway/providers/openaiimages"
)

type billingFake struct {
	beginRequest chargebilling.BeginRequest
	beginCharge  chargebilling.Charge
	beginErr     error
	captureErr   error
	releaseErr   error
	maxResponse  int64
	events       []string
}

func (fake *billingFake) Begin(_ context.Context, request chargebilling.BeginRequest) (chargebilling.Charge, error) {
	fake.beginRequest = request
	fake.events = append(fake.events, "begin")
	charge := fake.beginCharge
	if charge.ID == "" {
		charge.ID = "charge_00000000000000000000000000000001"
	}
	return charge, fake.beginErr
}
func (fake *billingFake) Capture(context.Context, string) (chargebilling.Charge, error) {
	fake.events = append(fake.events, "capture")
	return chargebilling.Charge{}, fake.captureErr
}
func (fake *billingFake) Release(context.Context, string) (chargebilling.Charge, error) {
	fake.events = append(fake.events, "release")
	return chargebilling.Charge{}, fake.releaseErr
}
func (fake *billingFake) MarkReconciling(context.Context, string) error {
	fake.events = append(fake.events, "reconciling")
	return nil
}
func (fake *billingFake) Complete(_ context.Context, _ string, success bool, snapshot chargebilling.ResponseSnapshot) (chargebilling.Charge, error) {
	if success {
		fake.events = append(fake.events, "capture")
		return chargebilling.Charge{Response: snapshot}, fake.captureErr
	}
	fake.events = append(fake.events, "release")
	return chargebilling.Charge{Response: snapshot}, fake.releaseErr
}
func (fake *billingFake) MaximumResponseBytes() int64 {
	if fake.maxResponse > 0 {
		return fake.maxResponse
	}
	return 1024 * 1024
}

func billingAuth() Authenticator {
	return authFunc(func(context.Context, string) (apikey.Principal, error) {
		return apikey.Principal{OrganizationID: "org_billing", ProjectID: "project_billing"}, nil
	})
}

func TestBillableImagesCapturesSuccessBeforeReturningNativeBody(t *testing.T) {
	fake := &billingFake{}
	handler := NewBillableImagesHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), billingAuth(), testRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			fake.events = append(fake.events, "provider")
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[{"url":"native"}]}`))}, nil
		}),
	}, 1024, fake)
	response := billableImageRequest(handler, `{"model":"gpt-image-1","n":2,"size":"1024x1024","quality":"high"}`)
	if response.Code != 200 || response.Body.String() != `{"data":[{"url":"native"}]}` {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
	if strings.Join(fake.events, ",") != "begin,provider,capture" {
		t.Fatalf("events=%v", fake.events)
	}
	request := fake.beginRequest
	if request.OrganizationID != "org_billing" || request.ProjectID != "project_billing" || request.RequestID != "client-request" || request.ChannelID != "channel_00000000000000000000000000000001" || request.Quantity != 2 || request.Size != "1024x1024" || request.Quality != "high" {
		t.Fatalf("begin request=%+v", request)
	}
}

func TestBillableImagesReleasesProviderFailures(t *testing.T) {
	for _, test := range []struct {
		name        string
		response    *http.Response
		executorErr error
		want        int
	}{
		{"native non-2xx", &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"native"}`))}, nil, 429},
		{"executor error", nil, openaiimages.ErrTimeout, 504},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &billingFake{}
			handler := NewBillableImagesHandler(slog.Default(), billingAuth(), testRegistry(t), map[providercredentials.ProviderID]Executor{
				providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
					fake.events = append(fake.events, "provider")
					return test.response, test.executorErr
				}),
			}, 1024, fake)
			response := billableImageRequest(handler, `{"model":"gpt-image-1"}`)
			if response.Code != test.want || strings.Join(fake.events, ",") != "begin,provider,release" {
				t.Fatalf("response=%d events=%v body=%s", response.Code, fake.events, response.Body.String())
			}
		})
	}
}

func TestBillableImagesFailsBeforeProviderAndReconcilesSettlement(t *testing.T) {
	providerCalls := 0
	fake := &billingFake{beginErr: ledger.ErrInsufficientFunds}
	handler := NewBillableImagesHandler(slog.Default(), billingAuth(), testRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { providerCalls++; return nil, nil }),
	}, 1024, fake)
	if response := billableImageRequest(handler, `{"model":"gpt-image-1"}`); response.Code != http.StatusPaymentRequired || providerCalls != 0 {
		t.Fatalf("begin failure=%d provider calls=%d", response.Code, providerCalls)
	}

	fake = &billingFake{captureErr: errors.New("database unavailable")}
	handler = NewBillableImagesHandler(slog.Default(), billingAuth(), testRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"secret":"provider-success"}`))}, nil
		}),
	}, 1024, fake)
	response := billableImageRequest(handler, `{"model":"gpt-image-1"}`)
	if response.Code != 503 || strings.Contains(response.Body.String(), "provider-success") || strings.Join(fake.events, ",") != "begin,capture,reconciling" {
		t.Fatalf("settlement failure=%d events=%v body=%s", response.Code, fake.events, response.Body.String())
	}
}

func TestBillableImagesRejectsUnavailablePriceBeforeProvider(t *testing.T) {
	providerCalls := 0
	fake := &billingFake{beginErr: pricing.ErrPriceUnavailable}
	handler := NewBillableImagesHandler(slog.Default(), billingAuth(), testRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { providerCalls++; return nil, nil }),
	}, 1024, fake)
	response := billableImageRequest(handler, `{"model":"gpt-image-1"}`)
	if response.Code != 503 || providerCalls != 0 {
		t.Fatalf("response=%d provider calls=%d", response.Code, providerCalls)
	}
}

func TestBillableImagesReplaysStoredResponseWithoutProvider(t *testing.T) {
	providerCalls := 0
	fake := &billingFake{beginCharge: chargebilling.Charge{ID: "charge_00000000000000000000000000000001", Replay: true, Response: chargebilling.ResponseSnapshot{Status: 202, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: []byte(`{"stored":true}`)}}}
	handler := NewBillableImagesHandler(slog.Default(), billingAuth(), testRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { providerCalls++; return nil, nil }),
	}, 1024, fake)
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer service-secret")
	request.Header.Set(requestid.HeaderName, "replay-request")
	request.Header.Set("Idempotency-Key", "replay-key")
	response := httptest.NewRecorder()
	requestid.Middleware(handler).ServeHTTP(response, request)
	if response.Code != 202 || response.Body.String() != `{"stored":true}` || response.Header().Get("Idempotency-Replayed") != "true" || providerCalls != 0 {
		t.Fatalf("response=%d %s headers=%v calls=%d", response.Code, response.Body.String(), response.Header(), providerCalls)
	}
	if fake.beginRequest.IdempotencyKey != "replay-key" || fake.beginRequest.RequestFingerprint == ([32]byte{}) {
		t.Fatalf("begin=%+v", fake.beginRequest)
	}
}

func TestBillableImagesOversizedProviderResponseBecomesReconciling(t *testing.T) {
	fake := &billingFake{maxResponse: 4}
	handler := NewBillableImagesHandler(slog.Default(), billingAuth(), testRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("12345"))}, nil
		}),
	}, 1024, fake)
	response := billableImageRequest(handler, `{"model":"gpt-image-1"}`)
	if response.Code != 503 || strings.Join(fake.events, ",") != "begin,reconciling" {
		t.Fatalf("response=%d events=%v body=%s", response.Code, fake.events, response.Body.String())
	}
}

func TestBillableMultipartEditExtractsSelectorWithoutChangingBody(t *testing.T) {
	body, contentType := multipartEdit(t, "gpt-image-1")
	fake := &billingFake{}
	handler := NewBillableEditHandler(slog.Default(), billingAuth(), testRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(_ context.Context, request openaiimages.Request) (*http.Response, error) {
			got, _ := io.ReadAll(request.Body)
			if string(got) != string(body) {
				t.Fatal("multipart body changed")
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}, nil
		}),
	}, int64(len(body)+1), 1, fake)
	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Authorization", "Bearer service-secret")
	request.Header.Set(requestid.HeaderName, "edit-request")
	request.Header.Set("Idempotency-Key", "multipart-edit-key")
	response := httptest.NewRecorder()
	requestid.Middleware(handler).ServeHTTP(response, request)
	if response.Code != 200 || fake.beginRequest.Operation != "image.edit" || fake.beginRequest.Quantity != 1 || fake.beginRequest.IdempotencyKey != "multipart-edit-key" || fake.beginRequest.RequestFingerprint == ([32]byte{}) || strings.Join(fake.events, ",") != "begin,capture" {
		t.Fatalf("response=%d begin=%+v events=%v", response.Code, fake.beginRequest, fake.events)
	}
}

func TestBillableXAIJSONEditUsesXAIChannel(t *testing.T) {
	fake := &billingFake{}
	handler := NewBillableEditHandler(slog.Default(), billingAuth(), testRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.XAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}, nil
		}),
	}, 2048, 1, fake)
	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(`{"model":"grok-imagine-image-quality","quality":"high","image":{"url":"https://example.invalid/image"}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer service-secret")
	request.Header.Set(requestid.HeaderName, "xai-edit-request")
	response := httptest.NewRecorder()
	requestid.Middleware(handler).ServeHTTP(response, request)
	if response.Code != 200 || fake.beginRequest.ChannelID != "channel_00000000000000000000000000000002" || fake.beginRequest.Quality != "high" || strings.Join(fake.events, ",") != "begin,capture" {
		t.Fatalf("response=%d begin=%+v events=%v", response.Code, fake.beginRequest, fake.events)
	}
}

func billableImageRequest(handler http.Handler, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer service-secret")
	request.Header.Set(requestid.HeaderName, "client-request")
	response := httptest.NewRecorder()
	requestid.Middleware(handler).ServeHTTP(response, request)
	return response
}
