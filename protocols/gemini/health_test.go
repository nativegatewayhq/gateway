package gemini

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nativegatewayhq/gateway/internal/apikey"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
)

type geminiHealthFake struct {
	snapshots  map[string]providerhealth.Snapshot
	inspectErr error
	inspected  []string
	claimed    []string
	released   []providerhealth.Permit
	observed   []providerhealth.Observation
}

func (fake *geminiHealthFake) Inspect(_ context.Context, channelID string) (providerhealth.Snapshot, error) {
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

func (fake *geminiHealthFake) ClaimProbe(_ context.Context, channelID, _ string) (providerhealth.Permit, error) {
	fake.claimed = append(fake.claimed, channelID)
	return providerhealth.Permit{ChannelID: channelID}, nil
}

func (fake *geminiHealthFake) Release(_ context.Context, permit providerhealth.Permit) error {
	fake.released = append(fake.released, permit)
	return nil
}

func (fake *geminiHealthFake) Observe(_ context.Context, observation providerhealth.Observation) (providerhealth.Snapshot, error) {
	fake.observed = append(fake.observed, observation)
	return providerhealth.Snapshot{State: providerhealth.Closed}, nil
}

func TestGeminiHealthOpenFixedChannelFailsBeforeDispatch(t *testing.T) {
	health := &geminiHealthFake{snapshots: map[string]providerhealth.Snapshot{"channel_00000000000000000000000000000003": {State: providerhealth.Open}}}
	executor := &stubExecutor{response: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}}
	handler := NewHandlerWithHealth(slog.Default(), &stubAuthenticator{principal: apikey.Principal{}}, executor, 4096, health)
	request := geminiRequest(strings.NewReader(`{"contents":[]}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || executor.calls != 0 || len(health.claimed) != 0 || len(health.observed) != 0 {
		t.Fatalf("response=%d calls=%d health=%+v", response.Code, executor.calls, health)
	}
}

func TestGeminiHealthOpenCandidateFallsBackBeforeBilling(t *testing.T) {
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "gemini", Model: "health-gemini", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.Priority, Candidates: []imageoperation.ChannelCandidate{
		{ID: "candidate_a", Provider: providercredentials.Google, ProviderModel: "open-model", ChannelID: "channel_00000000000000000000000000000003", Enabled: true, Priority: 1},
		{ID: "candidate_b", Provider: providercredentials.Google, ProviderModel: "healthy-model", ChannelID: "channel_00000000000000000000000000000004", Enabled: true, Priority: 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	health := &geminiHealthFake{snapshots: map[string]providerhealth.Snapshot{"channel_00000000000000000000000000000003": {State: providerhealth.Open}}}
	billing := &geminiBillingFake{}
	executor := &stubExecutor{response: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}}
	handler := NewBillableHandlerWithAvailabilityAndHealth(slog.Default(), &stubAuthenticator{principal: apikey.Principal{OrganizationID: "org_test", ProjectID: "project_test"}}, registry, executor, 4096, billing, geminiChannelAvailability{"channel_00000000000000000000000000000003": true, "channel_00000000000000000000000000000004": true}, health)
	request := geminiRequest(strings.NewReader(`{"contents":[]}`))
	request.URL.Path = "/v1beta/models/health-gemini:generateContent"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || executor.request.Model != "healthy-model" || billing.beginRequest.ChannelID != "channel_00000000000000000000000000000004" || len(health.observed) != 1 || health.observed[0].Outcome != providerhealth.Success {
		t.Fatalf("response=%d executor=%+v begin=%+v health=%+v", response.Code, executor.request, billing.beginRequest, health)
	}
}
