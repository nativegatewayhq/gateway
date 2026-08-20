package openai

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	chargebilling "github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/pricing"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	"github.com/nativegatewayhq/gateway/providers/openaiimages"
)

type healthGateFake struct {
	snapshots    map[string]providerhealth.Snapshot
	inspectErr   error
	claimPermits map[string]providerhealth.Permit
	claimErrors  map[string]error
	inspected    []string
	claimed      []string
	released     []providerhealth.Permit
	observed     []providerhealth.Observation
}

func (fake *healthGateFake) Inspect(_ context.Context, channelID string) (providerhealth.Snapshot, error) {
	fake.inspected = append(fake.inspected, channelID)
	if fake.inspectErr != nil {
		return providerhealth.Snapshot{}, fake.inspectErr
	}
	snapshot := fake.snapshots[channelID]
	if snapshot.State == "" {
		snapshot.State = providerhealth.Closed
	}
	return snapshot, nil
}

func (fake *healthGateFake) ClaimProbe(_ context.Context, channelID, _ string) (providerhealth.Permit, error) {
	fake.claimed = append(fake.claimed, channelID)
	if err := fake.claimErrors[channelID]; err != nil {
		return providerhealth.Permit{}, err
	}
	permit := fake.claimPermits[channelID]
	if permit.ChannelID == "" {
		permit.ChannelID = channelID
	}
	return permit, nil
}

func (fake *healthGateFake) Release(_ context.Context, permit providerhealth.Permit) error {
	fake.released = append(fake.released, permit)
	return nil
}

func (fake *healthGateFake) Observe(_ context.Context, observation providerhealth.Observation) (providerhealth.Snapshot, error) {
	fake.observed = append(fake.observed, observation)
	return providerhealth.Snapshot{State: providerhealth.Closed}, nil
}

func healthPriorityRegistry(t *testing.T) *imageoperation.Registry {
	t.Helper()
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "openai", Model: "health-image", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.Priority, Candidates: []imageoperation.ChannelCandidate{
		{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "model-openai", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Priority: 1},
		{ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "model-xai", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Priority: 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestHealthOpenCandidateIsExcludedBeforeBilling(t *testing.T) {
	health := &healthGateFake{snapshots: map[string]providerhealth.Snapshot{"channel_00000000000000000000000000000001": {State: providerhealth.Open}}}
	billing := &billingFake{}
	handler := NewBillableImagesHandlerWithAvailabilityAndHealth(slog.Default(), billingAuth(), healthPriorityRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			t.Fatal("open circuit dispatched")
			return nil, nil
		}),
		providercredentials.XAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}),
	}, 1024, billing, availability{providercredentials.OpenAI, providercredentials.XAI}, health)
	response := billableImageRequest(handler, `{"model":"health-image"}`)
	if response.Code != 200 || len(billing.quoteRequests) != 1 || billing.beginRequest.ChannelID != "channel_00000000000000000000000000000002" || len(health.observed) != 1 || health.observed[0].Outcome != providerhealth.Success {
		t.Fatalf("response=%d quotes=%+v begin=%+v health=%+v", response.Code, billing.quoteRequests, billing.beginRequest, health)
	}
}

func TestHealthProbeIsReleasedWhenCandidateFailsBeforeDispatch(t *testing.T) {
	probe := providerhealth.Permit{ChannelID: "channel_00000000000000000000000000000001", Token: strings.Repeat("a", 32), Probe: true}
	health := &healthGateFake{snapshots: map[string]providerhealth.Snapshot{"channel_00000000000000000000000000000001": {State: providerhealth.HalfOpen}}, claimPermits: map[string]providerhealth.Permit{"channel_00000000000000000000000000000001": probe}}
	billing := &billingFake{quoteErrors: map[string]error{"channel_00000000000000000000000000000001": pricing.ErrPriceUnavailable}}
	handler := NewBillableImagesHandlerWithAvailabilityAndHealth(slog.Default(), billingAuth(), healthPriorityRegistry(t), map[providercredentials.ProviderID]Executor{
		providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			t.Fatal("unpriced probe dispatched")
			return nil, nil
		}),
		providercredentials.XAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}),
	}, 1024, billing, availability{providercredentials.OpenAI, providercredentials.XAI}, health)
	response := billableImageRequest(handler, `{"model":"health-image"}`)
	if response.Code != 200 || len(health.released) != 1 || health.released[0] != probe || len(health.observed) != 1 || health.observed[0].ChannelID != "channel_00000000000000000000000000000002" {
		t.Fatalf("response=%d released=%+v observed=%+v", response.Code, health.released, health.observed)
	}
}

func TestHealthTerminalReplayDoesNotInspectClaimOrObserve(t *testing.T) {
	health := &healthGateFake{inspectErr: providerhealth.ErrUnavailable}
	billing := &billingFake{replayFound: true, replayCharge: chargebilling.Charge{Response: chargebilling.ResponseSnapshot{Status: 200, Body: []byte(`{"stored":true}`)}}}
	handler := NewBillableImagesHandlerWithAvailabilityAndHealth(slog.Default(), billingAuth(), healthPriorityRegistry(t), map[providercredentials.ProviderID]Executor{}, 1024, billing, nil, health)
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"health-image"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer service-secret")
	request.Header.Set("Idempotency-Key", "health-replay")
	response := httptest.NewRecorder()
	requestid.Middleware(handler).ServeHTTP(response, request)
	if response.Code != 200 || len(health.inspected) != 0 || len(health.claimed) != 0 || len(health.observed) != 0 {
		t.Fatalf("response=%d health=%+v", response.Code, health)
	}
}

func TestHealthFixedOpenFailsBeforeUnbilledDispatch(t *testing.T) {
	health := &healthGateFake{snapshots: map[string]providerhealth.Snapshot{"channel_00000000000000000000000000000001": {State: providerhealth.Open}}}
	providerCalls := 0
	handler := NewImagesHandlerWithHealth(slog.Default(), billingAuth(), testRegistry(t), map[providercredentials.ProviderID]Executor{providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) { providerCalls++; return nil, nil })}, 1024, health)
	response := billableImageRequest(handler, `{"model":"gpt-image-1"}`)
	if response.Code != http.StatusServiceUnavailable || providerCalls != 0 || len(health.claimed) != 0 || len(health.observed) != 0 {
		t.Fatalf("response=%d providers=%d health=%+v", response.Code, providerCalls, health)
	}
}

func TestHealthOutcomeClassificationIsBounded(t *testing.T) {
	health := &healthGateFake{}
	handler := NewImagesHandlerWithHealth(slog.Default(), billingAuth(), testRegistry(t), nil, 1024, health)
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	route := imageoperation.RoutingDecision{Provider: providercredentials.OpenAI, ChannelID: "channel_00000000000000000000000000000001"}
	for _, test := range []struct {
		response *http.Response
		err      error
		want     providerhealth.Outcome
	}{{&http.Response{StatusCode: 200}, nil, providerhealth.Success}, {&http.Response{StatusCode: 429}, nil, providerhealth.RateLimited}, {&http.Response{StatusCode: 503}, nil, providerhealth.ServerError}, {&http.Response{StatusCode: 400}, nil, providerhealth.Neutral}, {nil, openaiimages.ErrTimeout, providerhealth.Timeout}, {nil, openaiimages.ErrUpstream, providerhealth.Connection}, {nil, openaiimages.ErrCanceled, providerhealth.Neutral}} {
		handler.observeHealth(request, route, providerhealth.Permit{ChannelID: route.ChannelID}, test.response, test.err)
		if got := health.observed[len(health.observed)-1].Outcome; got != test.want {
			t.Fatalf("status=%v err=%v got=%s want=%s", test.response, test.err, got, test.want)
		}
	}
	before := len(health.observed)
	handler.observeHealth(request, route, providerhealth.Permit{ChannelID: route.ChannelID, Probe: true}, nil, providercredentials.ErrCredentialUnavailable)
	if len(health.observed) != before || len(health.released) != 1 {
		t.Fatalf("credential race observed=%d released=%d", len(health.observed), len(health.released))
	}
}
