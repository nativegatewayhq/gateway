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
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	chargebilling "github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/pricing"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/ratelimit"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	"github.com/nativegatewayhq/gateway/providers/openaiimages"
)

type billingFake struct {
	beginRequest  chargebilling.BeginRequest
	beginCharge   chargebilling.Charge
	beginErr      error
	captureErr    error
	releaseErr    error
	maxResponse   int64
	observation   chargebilling.Observation
	events        []string
	quoteErrors   map[string]error
	beginChannels []string
	replayCharge  chargebilling.Charge
	replayFound   bool
	replayErr     error
}

func (fake *billingFake) Begin(_ context.Context, request chargebilling.BeginRequest) (chargebilling.Charge, error) {
	fake.beginRequest = request
	fake.beginChannels = append(fake.beginChannels, request.ChannelID)
	fake.events = append(fake.events, "begin")
	charge := fake.beginCharge
	if charge.ID == "" {
		charge.ID = "charge_00000000000000000000000000000001"
	}
	return charge, fake.beginErr
}
func (fake *billingFake) Replay(context.Context, chargebilling.BeginRequest) (chargebilling.Charge, bool, error) {
	return fake.replayCharge, fake.replayFound, fake.replayErr
}
func (fake *billingFake) Quote(_ context.Context, request chargebilling.BeginRequest) (pricing.Estimate, error) {
	return pricing.Estimate{}, fake.quoteErrors[request.ChannelID]
}
func (fake *billingFake) Capture(context.Context, string) (chargebilling.Charge, error) {
	fake.events = append(fake.events, "capture")
	return chargebilling.Charge{}, fake.captureErr
}
func (fake *billingFake) Release(context.Context, string) (chargebilling.Charge, error) {
	fake.events = append(fake.events, "release")
	return chargebilling.Charge{}, fake.releaseErr
}
func (fake *billingFake) MarkReconciling(_ context.Context, _ string, observation chargebilling.Observation) error {
	fake.observation = observation
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

func TestBillableImagesUsesPriorityCandidateChannelAndProviderModel(t *testing.T) {
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "openai", Model: "logical-image", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.Priority, Candidates: []imageoperation.ChannelCandidate{
		{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "openai-provider-model", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Priority: 20},
		{ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "xai-provider-model", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Priority: 10},
	}})
	if err != nil {
		t.Fatal(err)
	}
	fake := &billingFake{}
	openAICalls, xAICalls := 0, 0
	handler := NewBillableImagesHandler(slog.Default(), billingAuth(), registry, map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			openAICalls++
			return nil, errors.New("unexpected OpenAI call")
		}),
		providercredentials.XAI: executorFunc(func(_ context.Context, request openaiimages.Request) (*http.Response, error) {
			xAICalls++
			body, _ := io.ReadAll(request.Body)
			if string(body) != `{"model":"xai-provider-model","prompt":"secret"}` {
				t.Fatalf("provider body=%s", body)
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
		}),
	}, 1024, fake)
	response := billableImageRequest(handler, `{"model":"logical-image","prompt":"secret"}`)
	if response.Code != 200 || openAICalls != 0 || xAICalls != 1 || fake.beginRequest.Model != "logical-image" || fake.beginRequest.ChannelID != "channel_00000000000000000000000000000002" {
		t.Fatalf("response=%d calls=%d/%d begin=%+v", response.Code, openAICalls, xAICalls, fake.beginRequest)
	}
}

func TestBillableImagesFallsBackBeforeReserveWhenFirstCandidatePriceIsUnavailable(t *testing.T) {
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "openai", Model: "logical-image", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.Priority, Candidates: []imageoperation.ChannelCandidate{
		{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "openai-provider-model", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Priority: 1},
		{ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "xai-provider-model", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Priority: 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	fake := &billingFake{quoteErrors: map[string]error{"channel_00000000000000000000000000000001": pricing.ErrPriceUnavailable}}
	openAICalls, xAICalls := 0, 0
	handler := NewBillableImagesHandlerWithAvailability(slog.Default(), billingAuth(), registry, map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			openAICalls++
			return nil, errors.New("unexpected call")
		}),
		providercredentials.XAI: executorFunc(func(_ context.Context, request openaiimages.Request) (*http.Response, error) {
			xAICalls++
			body, _ := io.ReadAll(request.Body)
			if string(body) != `{"model":"xai-provider-model","prompt":"secret"}` {
				t.Fatalf("provider body=%s", body)
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
		}),
	}, 1024, fake, availability{providercredentials.OpenAI, providercredentials.XAI})
	response := billableImageRequest(handler, `{"model":"logical-image","prompt":"secret"}`)
	if response.Code != 200 || openAICalls != 0 || xAICalls != 1 || len(fake.beginChannels) != 1 || fake.beginChannels[0] != "channel_00000000000000000000000000000002" {
		t.Fatalf("response=%d calls=%d/%d begin=%v", response.Code, openAICalls, xAICalls, fake.beginChannels)
	}
}

func TestBillableImagesDoesNotFallbackAfterReserveOnTimeout(t *testing.T) {
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "openai", Model: "logical-image", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.Priority, Candidates: []imageoperation.ChannelCandidate{
		{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "openai-provider-model", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Priority: 1},
		{ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "xai-provider-model", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Priority: 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	fake := &billingFake{}
	secondCalls := 0
	handler := NewBillableImagesHandlerWithAvailability(slog.Default(), billingAuth(), registry, map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			return nil, openaiimages.ErrTimeout
		}),
		providercredentials.XAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			secondCalls++
			return nil, errors.New("unexpected call")
		}),
	}, 1024, fake, availability{providercredentials.OpenAI, providercredentials.XAI})
	response := billableImageRequest(handler, `{"model":"logical-image","prompt":"secret"}`)
	if response.Code != 503 || secondCalls != 0 || len(fake.beginChannels) != 1 || fake.observation.Outcome != chargebilling.Unknown {
		t.Fatalf("response=%d second=%d begin=%v observation=%+v", response.Code, secondCalls, fake.beginChannels, fake.observation)
	}
}

func TestBillableImagesAllCandidatesUnavailableHasNoFinancialOrProviderEffect(t *testing.T) {
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "openai", Model: "logical-image", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.Priority, Candidates: []imageoperation.ChannelCandidate{
		{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "openai-model", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Priority: 1},
		{ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "xai-model", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Priority: 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	fake := &billingFake{}
	providerCalls := 0
	handler := NewBillableImagesHandlerWithAvailability(slog.Default(), billingAuth(), registry, map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { providerCalls++; return nil, nil }),
		providercredentials.XAI:    executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { providerCalls++; return nil, nil }),
	}, 1024, fake, availability{})
	response := billableImageRequest(handler, `{"model":"logical-image","prompt":"secret"}`)
	if response.Code != 503 || providerCalls != 0 || len(fake.events) != 0 || len(fake.beginChannels) != 0 {
		t.Fatalf("response=%d providers=%d events=%v begin=%v", response.Code, providerCalls, fake.events, fake.beginChannels)
	}
}

func TestBillableImagesRateLimitHasNoBillingOrProviderEffect(t *testing.T) {
	fake := &billingFake{}
	providerCalls := 0
	handler := NewBillableImagesHandler(slog.Default(), authFunc(func(context.Context, string) (apikey.Principal, error) {
		return apikey.Principal{}, &ratelimit.LimitError{Decision: ratelimit.Decision{Limit: 60, RetryAfter: time.Second, ResetAt: time.Unix(2_000_000_000, 0)}}
	}), testRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { providerCalls++; return nil, nil }),
	}, 1024, fake)
	response := requestImages(handler, http.MethodPost, `{"model":"gpt-image-1"}`, "Authorization", "Bearer service-secret")
	if response.Code != 429 || providerCalls != 0 || len(fake.events) != 0 || len(fake.beginChannels) != 0 {
		t.Fatalf("response=%d providers=%d events=%v begin=%v", response.Code, providerCalls, fake.events, fake.beginChannels)
	}
}

func TestBillableImagesTerminalReplayIgnoresCurrentAvailability(t *testing.T) {
	fake := &billingFake{replayFound: true, replayCharge: chargebilling.Charge{Replay: true, Response: chargebilling.ResponseSnapshot{Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: []byte(`{"stored":true}`)}}}
	providerCalls := 0
	handler := NewBillableImagesHandlerWithAvailability(slog.Default(), billingAuth(), testRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { providerCalls++; return nil, nil }),
	}, 1024, fake, availability{})
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1","prompt":"secret"}`))
	request.Header.Set("Authorization", "Bearer service-key")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "terminal-replay")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || response.Body.String() != `{"stored":true}` || providerCalls != 0 || len(fake.events) != 0 {
		t.Fatalf("response=%d %s providers=%d events=%v", response.Code, response.Body.String(), providerCalls, fake.events)
	}
}

func TestBillableImagesReleasesProviderFailures(t *testing.T) {
	for _, test := range []struct {
		name        string
		response    *http.Response
		executorErr error
		want        int
		events      string
	}{
		{"native non-2xx", &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"native"}`))}, nil, 429, "begin,provider,release"},
		{"executor timeout", nil, openaiimages.ErrTimeout, 503, "begin,provider,reconciling"},
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
			if response.Code != test.want || strings.Join(fake.events, ",") != test.events {
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
