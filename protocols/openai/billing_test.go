package openai

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	chargebilling "github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/costquota"
	"github.com/nativegatewayhq/gateway/internal/ledger"
	"github.com/nativegatewayhq/gateway/internal/pricing"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/ratelimit"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	"github.com/nativegatewayhq/gateway/internal/spendcap"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	"github.com/nativegatewayhq/gateway/providers/openaiimages"
)

type billingFake struct {
	beginRequest  chargebilling.BeginRequest
	beginCharge   chargebilling.Charge
	beginErr      error
	beginErrors   map[string]error
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
	replayCalls   int
	quotes        map[string]pricing.Estimate
	quoteRequests []chargebilling.BeginRequest
	beginSequence []error
	beginCalls    int
	completeOK    bool
	snapshot      chargebilling.ResponseSnapshot
}

func weightedEntropy(values ...uint64) imageoperation.WeightedSampler {
	content := make([]byte, 8*len(values))
	for index, value := range values {
		binary.BigEndian.PutUint64(content[index*8:], value)
	}
	sampler, _ := imageoperation.NewWeightedSampler(bytes.NewReader(content))
	return sampler
}

func (fake *billingFake) Begin(_ context.Context, request chargebilling.BeginRequest) (chargebilling.Charge, error) {
	fake.beginRequest = request
	fake.beginChannels = append(fake.beginChannels, request.ChannelID)
	fake.events = append(fake.events, "begin")
	fake.beginCalls++
	charge := fake.beginCharge
	if charge.ID == "" {
		charge.ID = "charge_00000000000000000000000000000001"
	}
	if err := fake.beginErrors[request.ChannelID]; err != nil {
		return charge, err
	}
	if fake.beginCalls <= len(fake.beginSequence) && fake.beginSequence[fake.beginCalls-1] != nil {
		return charge, fake.beginSequence[fake.beginCalls-1]
	}
	return charge, fake.beginErr
}
func (fake *billingFake) Replay(context.Context, chargebilling.BeginRequest) (chargebilling.Charge, bool, error) {
	fake.replayCalls++
	return fake.replayCharge, fake.replayFound, fake.replayErr
}
func (fake *billingFake) Quote(_ context.Context, request chargebilling.BeginRequest) (pricing.Estimate, error) {
	fake.quoteRequests = append(fake.quoteRequests, request)
	estimate := fake.quotes[request.ChannelID]
	if estimate.ChannelID == "" {
		estimate.ChannelID = request.ChannelID
	}
	if !request.EvaluationAt.IsZero() {
		estimate.EvaluatedAt = request.EvaluationAt
	}
	return estimate, fake.quoteErrors[request.ChannelID]
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
	fake.completeOK = success
	fake.snapshot = snapshot
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
		return apikey.Principal{APIKeyID: "key_billing", OrganizationID: "org_billing", ProjectID: "project_billing"}, nil
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
	if request.OrganizationID != "org_billing" || request.ProjectID != "project_billing" || request.APIKeyID != "key_billing" || request.RequestID != "client-request" || request.ChannelID != "channel_00000000000000000000000000000001" || request.Quantity != 2 || request.Size != "1024x1024" || request.Quality != "high" {
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

func TestBillableImagesUsesLowestUpstreamCostAndBoundQuote(t *testing.T) {
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "openai", Model: "logical-image", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.LowestCost, Candidates: []imageoperation.ChannelCandidate{
		{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "openai-provider-model", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Priority: 1},
		{ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "xai-provider-model", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Priority: 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	fake := &billingFake{quotes: map[string]pricing.Estimate{
		"channel_00000000000000000000000000000001": {PriceID: "price_00000000000000000000000000000001", Currency: ledger.Currency, Quantity: 1, EstimatedCost: 30, MaximumSale: 31},
		"channel_00000000000000000000000000000002": {PriceID: "price_00000000000000000000000000000002", Currency: ledger.Currency, Quantity: 1, EstimatedCost: 10, MaximumSale: 50},
	}}
	openAICalls, xAICalls := 0, 0
	handler := NewBillableImagesHandlerWithAvailability(slog.Default(), billingAuth(), registry, map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			openAICalls++
			return nil, errors.New("unexpected call")
		}),
		providercredentials.XAI: executorFunc(func(_ context.Context, request openaiimages.Request) (*http.Response, error) {
			xAICalls++
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}),
	}, 1024, fake, availability{providercredentials.OpenAI, providercredentials.XAI})
	response := billableImageRequest(handler, `{"model":"logical-image"}`)
	if response.Code != 200 || openAICalls != 0 || xAICalls != 1 || fake.beginRequest.ChannelID != "channel_00000000000000000000000000000002" || fake.beginRequest.RoutingPolicy != "lowest_cost" || fake.beginRequest.CostRank != 0 || fake.beginRequest.ExpectedQuote == nil || fake.beginRequest.ExpectedQuote.PriceID != "price_00000000000000000000000000000002" {
		t.Fatalf("response=%d calls=%d/%d begin=%+v", response.Code, openAICalls, xAICalls, fake.beginRequest)
	}
	if len(fake.quoteRequests) != 2 || fake.quoteRequests[0].EvaluationAt.IsZero() || !fake.quoteRequests[0].EvaluationAt.Equal(fake.quoteRequests[1].EvaluationAt) {
		t.Fatalf("quote requests=%+v", fake.quoteRequests)
	}
}

func TestBillableImagesLowestCostFallsBackOnCapAndReevaluatesPriceRaceOnce(t *testing.T) {
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "openai", Model: "logical-image", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.LowestCost, Candidates: []imageoperation.ChannelCandidate{
		{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "openai-provider-model", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Priority: 1},
		{ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "xai-provider-model", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Priority: 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	quotes := map[string]pricing.Estimate{
		"channel_00000000000000000000000000000001": {PriceID: "price_00000000000000000000000000000001", Currency: ledger.Currency, Quantity: 1, EstimatedCost: 20, MaximumSale: 30},
		"channel_00000000000000000000000000000002": {PriceID: "price_00000000000000000000000000000002", Currency: ledger.Currency, Quantity: 1, EstimatedCost: 10, MaximumSale: 30},
	}
	t.Run("spend cap", func(t *testing.T) {
		fake := &billingFake{quotes: quotes, beginErrors: map[string]error{"channel_00000000000000000000000000000002": spendcap.ErrExceeded}}
		handler := NewBillableImagesHandlerWithAvailability(slog.Default(), billingAuth(), registry, map[providercredentials.ProviderID]Executor{
			providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			}),
			providercredentials.XAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
				t.Fatal("capped candidate dispatched")
				return nil, nil
			}),
		}, 1024, fake, availability{providercredentials.OpenAI, providercredentials.XAI})
		response := billableImageRequest(handler, `{"model":"logical-image"}`)
		if response.Code != 200 || fake.beginRequest.ChannelID != "channel_00000000000000000000000000000001" || fake.beginRequest.CostRank != 1 || len(fake.beginChannels) != 2 {
			t.Fatalf("response=%d begin=%+v channels=%v", response.Code, fake.beginRequest, fake.beginChannels)
		}
	})
	t.Run("price race", func(t *testing.T) {
		fake := &billingFake{quotes: quotes, beginSequence: []error{chargebilling.ErrPriceSnapshotChanged}}
		handler := NewBillableImagesHandlerWithAvailability(slog.Default(), billingAuth(), registry, map[providercredentials.ProviderID]Executor{
			providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
				return nil, errors.New("unexpected call")
			}),
			providercredentials.XAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			}),
		}, 1024, fake, availability{providercredentials.OpenAI, providercredentials.XAI})
		response := billableImageRequest(handler, `{"model":"logical-image"}`)
		if response.Code != 200 || fake.beginCalls != 2 || len(fake.quoteRequests) != 4 {
			t.Fatalf("response=%d begins=%d quotes=%d", response.Code, fake.beginCalls, len(fake.quoteRequests))
		}
	})
	t.Run("repeated price race stops", func(t *testing.T) {
		fake := &billingFake{quotes: quotes, beginSequence: []error{chargebilling.ErrPriceSnapshotChanged, chargebilling.ErrPriceSnapshotChanged}}
		handler := NewBillableImagesHandlerWithAvailability(slog.Default(), billingAuth(), registry, map[providercredentials.ProviderID]Executor{
			providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
				t.Fatal("provider dispatched")
				return nil, nil
			}),
			providercredentials.XAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
				t.Fatal("provider dispatched")
				return nil, nil
			}),
		}, 1024, fake, availability{providercredentials.OpenAI, providercredentials.XAI})
		response := billableImageRequest(handler, `{"model":"logical-image"}`)
		if response.Code != http.StatusServiceUnavailable || fake.beginCalls != 2 || len(fake.quoteRequests) != 4 {
			t.Fatalf("response=%d begins=%d quotes=%d", response.Code, fake.beginCalls, len(fake.quoteRequests))
		}
	})
}

func TestBillableJSONEditUsesWeightedRoute(t *testing.T) {
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "openai", Model: "weighted-edit", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Edit, MediaType: imageoperation.JSON}, {Operation: imageoperation.Edit, MediaType: imageoperation.Multipart}}, Policy: imageoperation.Weighted, Candidates: []imageoperation.ChannelCandidate{
		{ID: "candidate_a", Provider: providercredentials.OpenAI, ProviderModel: "model-a", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Weight: 1},
		{ID: "candidate_b", Provider: providercredentials.XAI, ProviderModel: "model-b", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Weight: 9},
	}})
	if err != nil {
		t.Fatal(err)
	}
	fake := &billingFake{}
	handler := NewBillableEditHandlerWithAvailability(slog.Default(), billingAuth(), registry, map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			t.Fatal("wrong edit candidate")
			return nil, nil
		}),
		providercredentials.XAI: executorFunc(func(_ context.Context, request openaiimages.Request) (*http.Response, error) {
			body, _ := io.ReadAll(request.Body)
			if !strings.Contains(string(body), `"model":"model-b"`) {
				t.Fatalf("body=%s", body)
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}, nil
		}),
	}, 2048, 1, fake, availability{providercredentials.OpenAI, providercredentials.XAI})
	handler.common.weighted = weightedEntropy(7)
	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(`{"model":"weighted-edit","image":{"url":"https://example.invalid/image"}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer service-secret")
	response := httptest.NewRecorder()
	requestid.Middleware(handler).ServeHTTP(response, request)
	if response.Code != 200 || fake.beginRequest.ChannelID != "channel_00000000000000000000000000000002" || fake.beginRequest.RoutingPolicy != "weighted" || fake.beginRequest.CostRank != 0 {
		t.Fatalf("response=%d begin=%+v", response.Code, fake.beginRequest)
	}

	body, contentType := multipartEdit(t, "weighted-edit")
	multipartFake := &billingFake{}
	multipartHandler := NewBillableEditHandlerWithAvailability(slog.Default(), billingAuth(), registry, map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			t.Fatal("wrong multipart candidate")
			return nil, nil
		}),
		providercredentials.XAI: executorFunc(func(_ context.Context, request openaiimages.Request) (*http.Response, error) {
			providerBody, _ := io.ReadAll(request.Body)
			if !bytes.Contains(providerBody, []byte("model-b")) {
				t.Fatalf("multipart body missing provider model")
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}, nil
		}),
	}, int64(len(body)+1024), 1, multipartFake, availability{providercredentials.OpenAI, providercredentials.XAI})
	multipartHandler.common.weighted = weightedEntropy(7)
	multipartRequest := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body))
	multipartRequest.Header.Set("Content-Type", contentType)
	multipartRequest.Header.Set("Authorization", "Bearer service-secret")
	multipartResponse := httptest.NewRecorder()
	requestid.Middleware(multipartHandler).ServeHTTP(multipartResponse, multipartRequest)
	if multipartResponse.Code != 200 || multipartFake.beginRequest.ChannelID != "channel_00000000000000000000000000000002" || multipartFake.beginRequest.RoutingPolicy != "weighted" {
		t.Fatalf("multipart response=%d begin=%+v", multipartResponse.Code, multipartFake.beginRequest)
	}
}

func TestBillableImagesWeightedRoutingRenormalizesAfterCandidateFailures(t *testing.T) {
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "openai", Model: "weighted-image", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.Weighted, Candidates: []imageoperation.ChannelCandidate{
		{ID: "candidate_a", Provider: providercredentials.OpenAI, ProviderModel: "fallback-model", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Weight: 1},
		{ID: "candidate_b", Provider: providercredentials.XAI, ProviderModel: "preferred-model", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Weight: 9},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("price unavailable", func(t *testing.T) {
		fake := &billingFake{quoteErrors: map[string]error{"channel_00000000000000000000000000000002": pricing.ErrPriceUnavailable}}
		handler := NewBillableImagesHandlerWithAvailability(slog.Default(), billingAuth(), registry, map[providercredentials.ProviderID]Executor{
			providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			}),
			providercredentials.XAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
				t.Fatal("unpriced candidate dispatched")
				return nil, nil
			}),
		}, 1024, fake, availability{providercredentials.OpenAI, providercredentials.XAI})
		handler.weighted = weightedEntropy(7, 0)
		response := billableImageRequest(handler, `{"model":"weighted-image"}`)
		if response.Code != 200 || len(fake.quoteRequests) != 2 || len(fake.beginChannels) != 1 || fake.beginRequest.ChannelID != "channel_00000000000000000000000000000001" || fake.beginRequest.RoutingPolicy != "weighted" || fake.beginRequest.CostRank != 1 {
			t.Fatalf("response=%d quotes=%d begin=%+v channels=%v", response.Code, len(fake.quoteRequests), fake.beginRequest, fake.beginChannels)
		}
	})
	t.Run("spend cap", func(t *testing.T) {
		fake := &billingFake{beginErrors: map[string]error{"channel_00000000000000000000000000000002": spendcap.ErrExceeded}}
		handler := NewBillableImagesHandlerWithAvailability(slog.Default(), billingAuth(), registry, map[providercredentials.ProviderID]Executor{
			providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			}),
			providercredentials.XAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
				t.Fatal("capped candidate dispatched")
				return nil, nil
			}),
		}, 1024, fake, availability{providercredentials.OpenAI, providercredentials.XAI})
		handler.weighted = weightedEntropy(7, 0)
		response := billableImageRequest(handler, `{"model":"weighted-image"}`)
		if response.Code != 200 || len(fake.beginChannels) != 2 || fake.beginChannels[0] == fake.beginChannels[1] || fake.beginRequest.CostRank != 1 {
			t.Fatalf("response=%d begin=%+v channels=%v", response.Code, fake.beginRequest, fake.beginChannels)
		}
	})
	t.Run("credential filter before draw", func(t *testing.T) {
		fake := &billingFake{}
		handler := NewBillableImagesHandlerWithAvailability(slog.Default(), billingAuth(), registry, map[providercredentials.ProviderID]Executor{
			providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			}),
			providercredentials.XAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
				t.Fatal("credential-less candidate dispatched")
				return nil, nil
			}),
		}, 1024, fake, availability{providercredentials.OpenAI})
		handler.weighted = weightedEntropy(0)
		response := billableImageRequest(handler, `{"model":"weighted-image"}`)
		if response.Code != 200 || len(fake.quoteRequests) != 1 || fake.quoteRequests[0].ChannelID != "channel_00000000000000000000000000000001" {
			t.Fatalf("response=%d quotes=%+v", response.Code, fake.quoteRequests)
		}
	})
	t.Run("global billing error does not redraw", func(t *testing.T) {
		fake := &billingFake{beginErr: ledger.ErrInsufficientFunds}
		providerCalls := 0
		handler := NewBillableImagesHandlerWithAvailability(slog.Default(), billingAuth(), registry, map[providercredentials.ProviderID]Executor{
			providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { providerCalls++; return nil, nil }),
			providercredentials.XAI:    executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { providerCalls++; return nil, nil }),
		}, 1024, fake, availability{providercredentials.OpenAI, providercredentials.XAI})
		handler.weighted = weightedEntropy(7, 0)
		response := billableImageRequest(handler, `{"model":"weighted-image"}`)
		if response.Code != http.StatusPaymentRequired || fake.beginCalls != 1 || providerCalls != 0 {
			t.Fatalf("response=%d begins=%d providers=%d", response.Code, fake.beginCalls, providerCalls)
		}
	})
	t.Run("entropy failure fails closed", func(t *testing.T) {
		fake := &billingFake{}
		providerCalls := 0
		handler := NewBillableImagesHandlerWithAvailability(slog.Default(), billingAuth(), registry, map[providercredentials.ProviderID]Executor{
			providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { providerCalls++; return nil, nil }),
			providercredentials.XAI:    executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { providerCalls++; return nil, nil }),
		}, 1024, fake, availability{providercredentials.OpenAI, providercredentials.XAI})
		handler.weighted = weightedEntropy()
		response := billableImageRequest(handler, `{"model":"weighted-image"}`)
		if response.Code != http.StatusServiceUnavailable || len(fake.quoteRequests) != 0 || fake.beginCalls != 0 || providerCalls != 0 {
			t.Fatalf("response=%d quotes=%d begins=%d providers=%d", response.Code, len(fake.quoteRequests), fake.beginCalls, providerCalls)
		}
	})
	t.Run("all candidate prices unavailable", func(t *testing.T) {
		fake := &billingFake{quoteErrors: map[string]error{
			"channel_00000000000000000000000000000001": pricing.ErrPriceUnavailable,
			"channel_00000000000000000000000000000002": pricing.ErrMarginViolation,
		}}
		handler := NewBillableImagesHandlerWithAvailability(slog.Default(), billingAuth(), registry, map[providercredentials.ProviderID]Executor{
			providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
				t.Fatal("provider dispatched")
				return nil, nil
			}),
			providercredentials.XAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
				t.Fatal("provider dispatched")
				return nil, nil
			}),
		}, 1024, fake, availability{providercredentials.OpenAI, providercredentials.XAI})
		handler.weighted = weightedEntropy(7, 0)
		response := billableImageRequest(handler, `{"model":"weighted-image"}`)
		if response.Code != http.StatusServiceUnavailable || len(fake.quoteRequests) != 2 || fake.beginCalls != 0 {
			t.Fatalf("response=%d quotes=%d begins=%d", response.Code, len(fake.quoteRequests), fake.beginCalls)
		}
	})
	t.Run("post dispatch failure does not redraw", func(t *testing.T) {
		fake := &billingFake{}
		openAICalls, xAICalls := 0, 0
		handler := NewBillableImagesHandlerWithAvailability(slog.Default(), billingAuth(), registry, map[providercredentials.ProviderID]Executor{
			providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { openAICalls++; return nil, nil }),
			providercredentials.XAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
				xAICalls++
				return nil, openaiimages.ErrUpstream
			}),
		}, 1024, fake, availability{providercredentials.OpenAI, providercredentials.XAI})
		handler.weighted = weightedEntropy(7, 0)
		response := billableImageRequest(handler, `{"model":"weighted-image"}`)
		if response.Code != http.StatusServiceUnavailable || fake.beginCalls != 1 || openAICalls != 0 || xAICalls != 1 {
			t.Fatalf("response=%d begins=%d calls=%d/%d", response.Code, fake.beginCalls, openAICalls, xAICalls)
		}
	})
}

func TestBillableImagesWeightedTerminalReplayDoesNotDrawOrQuote(t *testing.T) {
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "openai", Model: "weighted-replay", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.Weighted, Candidates: []imageoperation.ChannelCandidate{{ID: "candidate", Provider: providercredentials.OpenAI, ProviderModel: "provider-model", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Weight: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	fake := &billingFake{replayFound: true, replayCharge: chargebilling.Charge{Response: chargebilling.ResponseSnapshot{Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: []byte(`{"replayed":true}`)}}}
	handler := NewBillableImagesHandler(slog.Default(), billingAuth(), registry, map[providercredentials.ProviderID]Executor{providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
		t.Fatal("provider dispatched")
		return nil, nil
	})}, 1024, fake)
	handler.weighted = weightedEntropy()
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"weighted-replay"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer service-secret")
	request.Header.Set("Idempotency-Key", "weighted-replay-key")
	response := httptest.NewRecorder()
	requestid.Middleware(handler).ServeHTTP(response, request)
	if response.Code != 200 || len(fake.quoteRequests) != 0 || fake.beginCalls != 0 || response.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("response=%d quotes=%d begins=%d", response.Code, len(fake.quoteRequests), fake.beginCalls)
	}
}

func TestBillableJSONEditUsesLowestCostRoute(t *testing.T) {
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "openai", Model: "logical-edit", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Edit, MediaType: imageoperation.JSON}}, Policy: imageoperation.LowestCost, Candidates: []imageoperation.ChannelCandidate{
		{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "expensive-edit", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Priority: 1},
		{ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "cheap-edit", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Priority: 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	fake := &billingFake{quotes: map[string]pricing.Estimate{
		"channel_00000000000000000000000000000001": {PriceID: "price_00000000000000000000000000000001", Currency: ledger.Currency, Quantity: 1, EstimatedCost: 40, MaximumSale: 50},
		"channel_00000000000000000000000000000002": {PriceID: "price_00000000000000000000000000000002", Currency: ledger.Currency, Quantity: 1, EstimatedCost: 10, MaximumSale: 30},
	}}
	handler := NewBillableEditHandlerWithAvailability(slog.Default(), billingAuth(), registry, map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			t.Fatal("expensive candidate dispatched")
			return nil, nil
		}),
		providercredentials.XAI: executorFunc(func(_ context.Context, request openaiimages.Request) (*http.Response, error) {
			body, _ := io.ReadAll(request.Body)
			if !strings.Contains(string(body), `"model":"cheap-edit"`) {
				t.Fatalf("body=%s", body)
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}, nil
		}),
	}, 2048, 1, fake, availability{providercredentials.OpenAI, providercredentials.XAI})
	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(`{"model":"logical-edit","image":{"url":"https://example.invalid/image"}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer service-secret")
	request.Header.Set(requestid.HeaderName, "lowest-edit-request")
	response := httptest.NewRecorder()
	requestid.Middleware(handler).ServeHTTP(response, request)
	if response.Code != 200 || fake.beginRequest.ChannelID != "channel_00000000000000000000000000000002" || fake.beginRequest.CostRank != 0 || fake.beginRequest.ExpectedQuote == nil {
		t.Fatalf("response=%d begin=%+v", response.Code, fake.beginRequest)
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

func TestBillableImagesFallsBackBeforeReserveWhenChannelCredentialIsUnavailable(t *testing.T) {
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "openai", Model: "logical-image", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.Priority, Candidates: []imageoperation.ChannelCandidate{
		{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "openai-provider-model", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Priority: 1},
		{ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "xai-provider-model", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Priority: 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	fake := &billingFake{}
	providerCalls := 0
	var logs bytes.Buffer
	handler := NewBillableImagesHandlerWithAvailability(slog.New(slog.NewTextHandler(&logs, nil)), billingAuth(), registry, map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			t.Fatal("unavailable credential dispatched")
			return nil, nil
		}),
		providercredentials.XAI: executorFunc(func(_ context.Context, request openaiimages.Request) (*http.Response, error) {
			providerCalls++
			if request.ChannelID != "channel_00000000000000000000000000000002" {
				t.Fatalf("channel=%s", request.ChannelID)
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}),
	}, 1024, fake, channelAvailability{"channel_00000000000000000000000000000002": true})
	response := billableImageRequest(handler, `{"model":"logical-image"}`)
	if response.Code != 200 || providerCalls != 1 || len(fake.beginChannels) != 1 || fake.beginChannels[0] != "channel_00000000000000000000000000000002" {
		t.Fatalf("response=%d calls=%d begins=%v", response.Code, providerCalls, fake.beginChannels)
	}
	credentialLog := ""
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "category=credential_unavailable") {
			credentialLog = line
			break
		}
	}
	if credentialLog == "" || strings.Contains(credentialLog, "logical-image") || strings.Contains(credentialLog, "candidate_openai") {
		t.Fatalf("missing or unsafe credential skip log: %s", credentialLog)
	}
}

func TestBillableImagesSkipsExhaustedSpendCapCandidates(t *testing.T) {
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "openai", Model: "logical-image", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.Priority, Candidates: []imageoperation.ChannelCandidate{
		{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "openai-provider-model", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Priority: 1},
		{ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "xai-provider-model", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Priority: 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("fallback", func(t *testing.T) {
		resetAt := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
		fake := &billingFake{beginErrors: map[string]error{"channel_00000000000000000000000000000001": &spendcap.LimitError{ChannelID: "channel_00000000000000000000000000000001", Period: spendcap.Day, ResetAt: resetAt}}}
		openAICalls, xAICalls := 0, 0
		var logs bytes.Buffer
		handler := NewBillableImagesHandlerWithAvailability(slog.New(slog.NewTextHandler(&logs, nil)), billingAuth(), registry, map[providercredentials.ProviderID]Executor{providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { openAICalls++; return nil, nil }), providercredentials.XAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			xAICalls++
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		})}, 1024, fake, availability{providercredentials.OpenAI, providercredentials.XAI})
		response := billableImageRequest(handler, `{"model":"logical-image"}`)
		if response.Code != 200 || openAICalls != 0 || xAICalls != 1 || len(fake.beginChannels) != 2 {
			t.Fatalf("response=%d calls=%d/%d begins=%v", response.Code, openAICalls, xAICalls, fake.beginChannels)
		}
		var spendCapLog string
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, "category=spend_cap_exhausted") {
				spendCapLog = line
				break
			}
		}
		if !strings.Contains(spendCapLog, "period=day") || strings.Contains(spendCapLog, "logical-image") || strings.Contains(spendCapLog, "candidate_openai") {
			t.Fatalf("unsafe or incomplete spend cap log: %s", spendCapLog)
		}
	})
	t.Run("all exhausted", func(t *testing.T) {
		fake := &billingFake{beginErrors: map[string]error{"channel_00000000000000000000000000000001": spendcap.ErrExceeded, "channel_00000000000000000000000000000002": spendcap.ErrExceeded}}
		providerCalls := 0
		handler := NewBillableImagesHandlerWithAvailability(slog.Default(), billingAuth(), registry, map[providercredentials.ProviderID]Executor{providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { providerCalls++; return nil, nil }), providercredentials.XAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { providerCalls++; return nil, nil })}, 1024, fake, availability{providercredentials.OpenAI, providercredentials.XAI})
		response := billableImageRequest(handler, `{"model":"logical-image"}`)
		if response.Code != 503 || providerCalls != 0 || len(fake.beginChannels) != 2 || !strings.Contains(response.Body.String(), "provider_unavailable") {
			t.Fatalf("response=%d body=%s calls=%d begins=%v", response.Code, response.Body.String(), providerCalls, fake.beginChannels)
		}
	})
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

func TestBillableImagesDoesNotFallbackAfterReservedCredentialRace(t *testing.T) {
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
			return nil, providercredentials.ErrCredentialUnavailable
		}),
		providercredentials.XAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			secondCalls++
			return nil, errors.New("unexpected call")
		}),
	}, 1024, fake, availability{providercredentials.OpenAI, providercredentials.XAI})
	response := billableImageRequest(handler, `{"model":"logical-image"}`)
	if response.Code != 503 || secondCalls != 0 || len(fake.beginChannels) != 1 || strings.Join(fake.events, ",") != "begin,release" {
		t.Fatalf("response=%d second=%d begin=%v events=%v", response.Code, secondCalls, fake.beginChannels, fake.events)
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

func TestBillableImagesPermissionDenialHasNoBillingReplayOrProviderEffect(t *testing.T) {
	fake := &billingFake{}
	providerCalls := 0
	principal := apikey.Principal{ModelAccessMode: apikey.ModelAccessAllowlist, ModelPermissions: []apikey.ModelPermission{{Protocol: "openai", Operation: "image.edit", Model: "gpt-image-1"}}}
	handler := NewBillableImagesHandler(slog.Default(), authFunc(func(context.Context, string) (apikey.Principal, error) { return principal, nil }), testRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { providerCalls++; return nil, nil }),
	}, 1024, fake)
	response := requestImages(handler, http.MethodPost, `{"model":"gpt-image-1"}`, "Authorization", "Bearer service-secret")
	if response.Code != 403 || providerCalls != 0 || fake.replayCalls != 0 || len(fake.events) != 0 || len(fake.beginChannels) != 0 {
		t.Fatalf("response=%d providers=%d replay=%d events=%v begin=%v", response.Code, providerCalls, fake.replayCalls, fake.events, fake.beginChannels)
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
		{"credential rotation race", nil, providercredentials.ErrCredentialUnavailable, 503, "begin,provider,release"},
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

func TestBillableImagesMapsCostQuotaBeforeProvider(t *testing.T) {
	providerCalls := 0
	var logs bytes.Buffer
	reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	fake := &billingFake{beginErr: &costquota.LimitError{ScopeType: costquota.Project, Period: costquota.Day, ResetAt: reset, APIKeyID: "key_billing", ProjectID: "project_billing"}}
	handler := NewBillableImagesHandler(slog.New(slog.NewTextHandler(&logs, nil)), billingAuth(), testRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { providerCalls++; return nil, nil }),
	}, 1024, fake)
	response := billableImageRequest(handler, `{"model":"gpt-image-1"}`)
	if response.Code != http.StatusTooManyRequests || !strings.Contains(response.Body.String(), "quota_exceeded") || response.Header().Get("X-Quota-Reset") != strconv.FormatInt(reset.Unix(), 10) || providerCalls != 0 {
		t.Fatalf("response=%d body=%s headers=%v calls=%d", response.Code, response.Body.String(), response.Header(), providerCalls)
	}
	if strings.Contains(response.Body.String(), "key_billing") || strings.Contains(response.Body.String(), "project_billing") || strings.Contains(response.Body.String(), "project") || !strings.Contains(logs.String(), "category=quota_exceeded") || !strings.Contains(logs.String(), "scope_type=project") {
		t.Fatalf("unsafe response/log body=%s logs=%s", response.Body.String(), logs.String())
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
