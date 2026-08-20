package gemini

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
	"github.com/nativegatewayhq/gateway/internal/spendcap"
	imageoperation "github.com/nativegatewayhq/gateway/operations/image"
	"github.com/nativegatewayhq/gateway/providers/google"
)

type geminiBillingFake struct {
	beginRequest  chargebilling.BeginRequest
	beginCharge   chargebilling.Charge
	beginErr      error
	beginErrors   map[string]error
	completeOK    bool
	snapshot      chargebilling.ResponseSnapshot
	completeErr   error
	observation   chargebilling.Observation
	events        []string
	quoteErrors   map[string]error
	beginChannels []string
	replayCalls   int
	quotes        map[string]pricing.Estimate
	quoteRequests []chargebilling.BeginRequest
}

func geminiWeightedEntropy(values ...uint64) imageoperation.WeightedSampler {
	content := make([]byte, 8*len(values))
	for index, value := range values {
		binary.BigEndian.PutUint64(content[index*8:], value)
	}
	sampler, _ := imageoperation.NewWeightedSampler(bytes.NewReader(content))
	return sampler
}

func (fake *geminiBillingFake) Begin(_ context.Context, request chargebilling.BeginRequest) (chargebilling.Charge, error) {
	fake.events = append(fake.events, "begin")
	fake.beginChannels = append(fake.beginChannels, request.ChannelID)
	fake.beginRequest = request
	if err := fake.beginErrors[request.ChannelID]; err != nil {
		return fake.beginCharge, err
	}
	return fake.beginCharge, fake.beginErr
}

func (fake *geminiBillingFake) Replay(context.Context, chargebilling.BeginRequest) (chargebilling.Charge, bool, error) {
	fake.replayCalls++
	return chargebilling.Charge{}, false, nil
}

func (fake *geminiBillingFake) Quote(_ context.Context, request chargebilling.BeginRequest) (pricing.Estimate, error) {
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

func (fake *geminiBillingFake) Complete(_ context.Context, _ string, success bool, snapshot chargebilling.ResponseSnapshot) (chargebilling.Charge, error) {
	fake.events = append(fake.events, "complete")
	fake.completeOK = success
	fake.snapshot = snapshot
	if fake.completeErr != nil {
		return chargebilling.Charge{}, fake.completeErr
	}
	return chargebilling.Charge{Response: snapshot}, nil
}

func (fake *geminiBillingFake) MarkReconciling(_ context.Context, _ string, observation chargebilling.Observation) error {
	fake.events = append(fake.events, "reconciling")
	fake.observation = observation
	return nil
}

func (*geminiBillingFake) MaximumResponseBytes() int64 { return 1024 }

func billableGeminiHandler(fake *geminiBillingFake, executor *stubExecutor) *Handler {
	return NewBillableHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), &stubAuthenticator{principal: apikey.Principal{APIKeyID: "key_test", OrganizationID: "org_test", ProjectID: "project_test"}}, imageoperation.DefaultRegistry(), executor, 4096, fake)
}

func TestBillableGeminiCapturesNativeResponse(t *testing.T) {
	fake := &geminiBillingFake{beginCharge: chargebilling.Charge{ID: "charge_test"}}
	executor := &stubExecutor{response: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}, "X-Goog-Request-Id": {"google-1"}, "Set-Cookie": {"secret"}}, Body: io.NopCloser(strings.NewReader(`{"image":true}`))}}
	handler := billableGeminiHandler(fake, executor)
	request := geminiRequest(strings.NewReader(`{"contents":[{"parts":[{"text":"secret prompt"}]}],"generationConfig":{"imageConfig":{"aspectRatio":"16:9","imageSize":"2K"}}}`))
	request.Header.Set("Idempotency-Key", "gemini-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || response.Body.String() != `{"image":true}` || response.Header().Get("X-Goog-Request-Id") != "google-1" || response.Header().Get("Set-Cookie") != "" {
		t.Fatalf("response=%d %s headers=%v", response.Code, response.Body.String(), response.Header())
	}
	if fake.beginRequest.APIKeyID != "key_test" || fake.beginRequest.Protocol != "gemini" || fake.beginRequest.Operation != "image.generate" || fake.beginRequest.Model != "gemini-image" || fake.beginRequest.Size != "16:9" || fake.beginRequest.Quality != "2K" || fake.beginRequest.Quantity != 1 || fake.beginRequest.IdempotencyKey != "gemini-key" || fake.beginRequest.RequestFingerprint == ([32]byte{}) {
		t.Fatalf("begin=%+v", fake.beginRequest)
	}
	if !fake.completeOK || strings.Join(fake.events, ",") != "begin,complete" || executor.calls != 1 {
		t.Fatalf("events=%v success=%v calls=%d", fake.events, fake.completeOK, executor.calls)
	}
}

func TestBillableGeminiUsesSelectedProviderModel(t *testing.T) {
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "gemini", Model: "gemini-logical", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.Priority, Candidates: []imageoperation.ChannelCandidate{
		{ID: "candidate_disabled", Provider: providercredentials.Google, ProviderModel: "disabled-model", ChannelID: "channel_00000000000000000000000000000004", Enabled: false, Priority: 0},
		{ID: "candidate_google", Provider: providercredentials.Google, ProviderModel: "google-provider-model", ChannelID: "channel_00000000000000000000000000000003", Enabled: true, Priority: 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	fake := &geminiBillingFake{beginCharge: chargebilling.Charge{ID: "charge_test"}}
	executor := &stubExecutor{response: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}}
	handler := NewBillableHandler(slog.Default(), &stubAuthenticator{principal: apikey.Principal{OrganizationID: "org_test", ProjectID: "project_test"}}, registry, executor, 4096, fake)
	request := geminiRequest(strings.NewReader(`{"contents":[]}`))
	request.URL.Path = "/v1beta/models/gemini-logical:generateContent"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || executor.request.Model != "google-provider-model" || fake.beginRequest.Model != "gemini-logical" || fake.beginRequest.ChannelID != "channel_00000000000000000000000000000003" {
		t.Fatalf("response=%d providerModel=%s begin=%+v", response.Code, executor.request.Model, fake.beginRequest)
	}
}

func TestBillableGeminiFallsBackToNextExactPricedCandidate(t *testing.T) {
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "gemini", Model: "gemini-logical", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.Priority, Candidates: []imageoperation.ChannelCandidate{
		{ID: "candidate_first", Provider: providercredentials.Google, ProviderModel: "first-model", ChannelID: "channel_00000000000000000000000000000003", Enabled: true, Priority: 1},
		{ID: "candidate_second", Provider: providercredentials.Google, ProviderModel: "second-model", ChannelID: "channel_00000000000000000000000000000004", Enabled: true, Priority: 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	fake := &geminiBillingFake{beginCharge: chargebilling.Charge{ID: "charge_test"}, quoteErrors: map[string]error{"channel_00000000000000000000000000000003": pricing.ErrPriceUnavailable}}
	executor := &stubExecutor{response: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}}
	handler := NewBillableHandler(slog.Default(), &stubAuthenticator{principal: apikey.Principal{OrganizationID: "org_test", ProjectID: "project_test"}}, registry, executor, 4096, fake)
	request := geminiRequest(strings.NewReader(`{"contents":[]}`))
	request.URL.Path = "/v1beta/models/gemini-logical:generateContent"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || executor.request.Model != "second-model" || len(fake.beginChannels) != 1 || fake.beginChannels[0] != "channel_00000000000000000000000000000004" {
		t.Fatalf("response=%d model=%s begin=%v", response.Code, executor.request.Model, fake.beginChannels)
	}
}

func TestBillableGeminiUsesLowestCostBoundQuote(t *testing.T) {
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "gemini", Model: "gemini-logical", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.LowestCost, Candidates: []imageoperation.ChannelCandidate{
		{ID: "candidate_first", Provider: providercredentials.Google, ProviderModel: "expensive-model", ChannelID: "channel_00000000000000000000000000000003", Enabled: true, Priority: 1},
		{ID: "candidate_second", Provider: providercredentials.Google, ProviderModel: "cheap-model", ChannelID: "channel_00000000000000000000000000000004", Enabled: true, Priority: 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	fake := &geminiBillingFake{beginCharge: chargebilling.Charge{ID: "charge_test"}, quotes: map[string]pricing.Estimate{
		"channel_00000000000000000000000000000003": {PriceID: "price_00000000000000000000000000000003", Currency: ledger.Currency, Quantity: 1, EstimatedCost: 50, MaximumSale: 51},
		"channel_00000000000000000000000000000004": {PriceID: "price_00000000000000000000000000000004", Currency: ledger.Currency, Quantity: 1, EstimatedCost: 10, MaximumSale: 90},
	}}
	executor := &stubExecutor{response: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}}
	handler := NewBillableHandlerWithAvailability(slog.Default(), &stubAuthenticator{principal: apikey.Principal{OrganizationID: "org_test", ProjectID: "project_test"}}, registry, executor, 4096, fake, geminiChannelAvailability{"channel_00000000000000000000000000000003": true, "channel_00000000000000000000000000000004": true})
	request := geminiRequest(strings.NewReader(`{"contents":[]}`))
	request.URL.Path = "/v1beta/models/gemini-logical:generateContent"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || executor.request.Model != "cheap-model" || executor.request.ChannelID != "channel_00000000000000000000000000000004" || fake.beginRequest.ExpectedQuote == nil || fake.beginRequest.ExpectedQuote.PriceID != "price_00000000000000000000000000000004" || fake.beginRequest.CostRank != 0 {
		t.Fatalf("response=%d request=%+v begin=%+v", response.Code, executor.request, fake.beginRequest)
	}
	if len(fake.quoteRequests) != 2 || !fake.quoteRequests[0].EvaluationAt.Equal(fake.quoteRequests[1].EvaluationAt) {
		t.Fatalf("quotes=%+v", fake.quoteRequests)
	}
}

func TestBillableGeminiWeightedRoutingRenormalizesAfterPriceFailure(t *testing.T) {
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "gemini", Model: "weighted-gemini", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.Weighted, Candidates: []imageoperation.ChannelCandidate{
		{ID: "candidate_a", Provider: providercredentials.Google, ProviderModel: "fallback-model", ChannelID: "channel_00000000000000000000000000000003", Enabled: true, Weight: 1},
		{ID: "candidate_b", Provider: providercredentials.Google, ProviderModel: "preferred-model", ChannelID: "channel_00000000000000000000000000000004", Enabled: true, Weight: 9},
	}})
	if err != nil {
		t.Fatal(err)
	}
	fake := &geminiBillingFake{quoteErrors: map[string]error{"channel_00000000000000000000000000000004": pricing.ErrPriceUnavailable}}
	executor := &stubExecutor{response: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}}
	handler := NewBillableHandlerWithAvailability(slog.Default(), &stubAuthenticator{principal: apikey.Principal{OrganizationID: "org_test", ProjectID: "project_test"}}, registry, executor, 4096, fake, geminiChannelAvailability{"channel_00000000000000000000000000000003": true, "channel_00000000000000000000000000000004": true})
	handler.weighted = geminiWeightedEntropy(7, 0)
	request := geminiRequest(strings.NewReader(`{"contents":[]}`))
	request.URL.Path = "/v1beta/models/weighted-gemini:generateContent"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || executor.request.Model != "fallback-model" || fake.beginRequest.ChannelID != "channel_00000000000000000000000000000003" || fake.beginRequest.RoutingPolicy != "weighted" || fake.beginRequest.CostRank != 1 || len(fake.quoteRequests) != 2 {
		t.Fatalf("response=%d executor=%+v begin=%+v quotes=%d", response.Code, executor.request, fake.beginRequest, len(fake.quoteRequests))
	}
}

type geminiChannelAvailability map[string]bool

func (geminiChannelAvailability) ConfiguredProviders() []providercredentials.ProviderID { return nil }
func (value geminiChannelAvailability) ConfiguredChannel(_ context.Context, channelID string, _ providercredentials.ProviderID) bool {
	return value[channelID]
}

func TestBillableGeminiFallsBackBeforeReserveWhenChannelCredentialIsUnavailable(t *testing.T) {
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "gemini", Model: "gemini-logical", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.Priority, Candidates: []imageoperation.ChannelCandidate{
		{ID: "candidate_first", Provider: providercredentials.Google, ProviderModel: "first-model", ChannelID: "channel_00000000000000000000000000000003", Enabled: true, Priority: 1},
		{ID: "candidate_second", Provider: providercredentials.Google, ProviderModel: "second-model", ChannelID: "channel_00000000000000000000000000000004", Enabled: true, Priority: 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	fake := &geminiBillingFake{beginCharge: chargebilling.Charge{ID: "charge_test"}}
	executor := &stubExecutor{response: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}}
	var logs bytes.Buffer
	handler := NewBillableHandlerWithAvailability(slog.New(slog.NewTextHandler(&logs, nil)), &stubAuthenticator{principal: apikey.Principal{OrganizationID: "org_test", ProjectID: "project_test"}}, registry, executor, 4096, fake, geminiChannelAvailability{"channel_00000000000000000000000000000004": true})
	request := geminiRequest(strings.NewReader(`{"contents":[]}`))
	request.URL.Path = "/v1beta/models/gemini-logical:generateContent"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || executor.request.Model != "second-model" || executor.request.ChannelID != "channel_00000000000000000000000000000004" || len(fake.beginChannels) != 1 {
		t.Fatalf("response=%d request=%+v begins=%v", response.Code, executor.request, fake.beginChannels)
	}
	credentialLog := ""
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "category=credential_unavailable") {
			credentialLog = line
			break
		}
	}
	if credentialLog == "" || strings.Contains(credentialLog, "gemini-logical") || strings.Contains(credentialLog, "candidate_first") {
		t.Fatalf("missing or unsafe credential skip log: %s", credentialLog)
	}
}

func TestBillableGeminiSkipsExhaustedSpendCapCandidate(t *testing.T) {
	registry, err := imageoperation.NewRegistry(imageoperation.ModelRoute{Protocol: "gemini", Model: "gemini-logical", Owner: "gateway", Capabilities: []imageoperation.Capability{{Operation: imageoperation.Generate, MediaType: imageoperation.JSON}}, Policy: imageoperation.Priority, Candidates: []imageoperation.ChannelCandidate{{ID: "candidate_first", Provider: providercredentials.Google, ProviderModel: "first-model", ChannelID: "channel_00000000000000000000000000000003", Enabled: true, Priority: 1}, {ID: "candidate_second", Provider: providercredentials.Google, ProviderModel: "second-model", ChannelID: "channel_00000000000000000000000000000004", Enabled: true, Priority: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	fake := &geminiBillingFake{beginCharge: chargebilling.Charge{ID: "charge_test"}, beginErrors: map[string]error{"channel_00000000000000000000000000000003": spendcap.ErrExceeded}}
	executor := &stubExecutor{response: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}}
	handler := NewBillableHandler(slog.Default(), &stubAuthenticator{principal: apikey.Principal{OrganizationID: "org_test", ProjectID: "project_test"}}, registry, executor, 4096, fake)
	request := geminiRequest(strings.NewReader(`{"contents":[]}`))
	request.URL.Path = "/v1beta/models/gemini-logical:generateContent"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || executor.request.Model != "second-model" || len(fake.beginChannels) != 2 {
		t.Fatalf("response=%d model=%s begins=%v", response.Code, executor.request.Model, fake.beginChannels)
	}
}

func TestBillableGeminiReplaysWithoutProvider(t *testing.T) {
	fake := &geminiBillingFake{beginCharge: chargebilling.Charge{Replay: true, Response: chargebilling.ResponseSnapshot{Status: 202, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: []byte(`{"stored":true}`)}}}
	executor := &stubExecutor{}
	request := geminiRequest(strings.NewReader(`{"contents":[]}`))
	request.Header.Set("Idempotency-Key", "gemini-replay")
	response := httptest.NewRecorder()
	billableGeminiHandler(fake, executor).ServeHTTP(response, request)
	if response.Code != 202 || response.Body.String() != `{"stored":true}` || response.Header().Get("Idempotency-Replayed") != "true" || executor.calls != 0 {
		t.Fatalf("response=%d %s headers=%v calls=%d", response.Code, response.Body.String(), response.Header(), executor.calls)
	}
}

func TestBillableGeminiPermissionDenialPrecedesReplayAndProvider(t *testing.T) {
	fake := &geminiBillingFake{beginCharge: chargebilling.Charge{ID: "charge_test"}}
	executor := &stubExecutor{}
	principal := apikey.Principal{ModelAccessMode: apikey.ModelAccessAllowlist, ModelPermissions: []apikey.ModelPermission{{Protocol: "openai", Operation: "image.generate", Model: "gemini-image"}}}
	handler := NewBillableHandler(slog.Default(), &stubAuthenticator{principal: principal}, imageoperation.DefaultRegistry(), executor, 4096, fake)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, geminiRequest(geminiPanicReader{}))
	if response.Code != 403 || fake.replayCalls != 0 || len(fake.events) != 0 || executor.calls != 0 {
		t.Fatalf("response=%d replay=%d events=%v calls=%d", response.Code, fake.replayCalls, fake.events, executor.calls)
	}
}

func TestBillableGeminiFingerprintExcludesCredentialLocation(t *testing.T) {
	body := `{"contents":[],"generationConfig":{"imageConfig":{"imageSize":"2K"}}}`
	fingerprints := make([][32]byte, 0, 2)
	for _, setup := range []func(*http.Request){
		func(*http.Request) {},
		func(request *http.Request) {
			request.URL.RawQuery = "safe=value"
			request.Header.Set("Authorization", "Bearer service-key")
		},
	} {
		fake := &geminiBillingFake{beginCharge: chargebilling.Charge{Replay: true, Response: chargebilling.ResponseSnapshot{Status: 200, Body: []byte(`{}`)}}}
		request := geminiRequest(strings.NewReader(body))
		request.Header.Set("Idempotency-Key", "same-key")
		setup(request)
		billableGeminiHandler(fake, &stubExecutor{}).ServeHTTP(httptest.NewRecorder(), request)
		fingerprints = append(fingerprints, fake.beginRequest.RequestFingerprint)
	}
	if fingerprints[0] == ([32]byte{}) || fingerprints[0] != fingerprints[1] {
		t.Fatalf("credential location changed fingerprint: %x %x", fingerprints[0], fingerprints[1])
	}
}

func TestBillableGeminiFailureClassification(t *testing.T) {
	for _, test := range []struct {
		name        string
		executor    *stubExecutor
		wantEvent   string
		wantOutcome chargebilling.Outcome
		wantReason  chargebilling.Reason
		wantSuccess bool
	}{
		{"connection", &stubExecutor{err: errors.Join(errors.New("detail"), google.ErrUpstream)}, "reconciling", chargebilling.Unknown, chargebilling.ExecutorConnection, false},
		{"timeout", &stubExecutor{err: google.ErrTimeout}, "reconciling", chargebilling.Unknown, chargebilling.ExecutorTimeout, false},
		{"canceled", &stubExecutor{err: google.ErrCanceled}, "reconciling", chargebilling.Unknown, chargebilling.ExecutorConnection, false},
		{"response loss", &stubExecutor{response: &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(&failingReader{})}}, "reconciling", chargebilling.KnownSuccess, chargebilling.ResponseUnavailable, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &geminiBillingFake{beginCharge: chargebilling.Charge{ID: "charge_test"}}
			response := httptest.NewRecorder()
			billableGeminiHandler(fake, test.executor).ServeHTTP(response, geminiRequest(strings.NewReader(`{"contents":[]}`)))
			if response.Code != http.StatusServiceUnavailable || fake.observation.Outcome != test.wantOutcome || fake.observation.Reason != test.wantReason || fake.completeOK != test.wantSuccess || fake.events[len(fake.events)-1] != test.wantEvent {
				t.Fatalf("response=%d events=%v observation=%+v", response.Code, fake.events, fake.observation)
			}
		})
	}
}

func TestBillableGeminiCredentialUnavailableReleases(t *testing.T) {
	fake := &geminiBillingFake{beginCharge: chargebilling.Charge{ID: "charge_test"}}
	executor := &stubExecutor{err: errors.Join(errors.New("detail"), providercredentials.ErrCredentialUnavailable)}
	response := httptest.NewRecorder()
	billableGeminiHandler(fake, executor).ServeHTTP(response, geminiRequest(strings.NewReader(`{"contents":[]}`)))
	if response.Code != http.StatusServiceUnavailable || fake.completeOK || strings.Join(fake.events, ",") != "begin,complete" {
		t.Fatalf("response=%d events=%v", response.Code, fake.events)
	}
}

func TestBillableGeminiMapsCostQuotaBeforeProvider(t *testing.T) {
	reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	fake := &geminiBillingFake{beginErr: &costquota.LimitError{ScopeType: costquota.APIKey, ResetAt: reset}}
	executor := &stubExecutor{}
	response := httptest.NewRecorder()
	billableGeminiHandler(fake, executor).ServeHTTP(response, geminiRequest(strings.NewReader(`{"contents":[]}`)))
	if response.Code != http.StatusTooManyRequests || !strings.Contains(response.Body.String(), "RESOURCE_EXHAUSTED") || response.Header().Get("X-Quota-Reset") != strconv.FormatInt(reset.Unix(), 10) || executor.calls != 0 {
		t.Fatalf("response=%d body=%s headers=%v calls=%d", response.Code, response.Body.String(), response.Header(), executor.calls)
	}
}

func TestBillableGeminiPanicAndSettlementFailureReconcile(t *testing.T) {
	t.Run("provider panic", func(t *testing.T) {
		fake := &geminiBillingFake{beginCharge: chargebilling.Charge{ID: "charge_test"}}
		response := httptest.NewRecorder()
		billableGeminiHandler(fake, &stubExecutor{panic: true}).ServeHTTP(response, geminiRequest(strings.NewReader(`{"contents":[]}`)))
		if response.Code != http.StatusServiceUnavailable || fake.observation.Outcome != chargebilling.Unknown || fake.observation.Reason != chargebilling.ProviderPanic {
			t.Fatalf("response=%d observation=%+v", response.Code, fake.observation)
		}
	})
	t.Run("known success settlement", func(t *testing.T) {
		fake := &geminiBillingFake{beginCharge: chargebilling.Charge{ID: "charge_test"}, completeErr: errors.New("database unavailable")}
		executor := &stubExecutor{response: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}}
		response := httptest.NewRecorder()
		billableGeminiHandler(fake, executor).ServeHTTP(response, geminiRequest(strings.NewReader(`{"contents":[]}`)))
		if response.Code != http.StatusServiceUnavailable || fake.observation.Outcome != chargebilling.KnownSuccess || fake.observation.Reason != chargebilling.SettlementFailed || string(fake.observation.Snapshot.Body) != `{"ok":true}` {
			t.Fatalf("response=%d observation=%+v", response.Code, fake.observation)
		}
	})
}

func TestBillableGeminiRejectsUnsupportedBeforeProvider(t *testing.T) {
	fake := &geminiBillingFake{}
	executor := &stubExecutor{}
	request := geminiRequest(strings.NewReader(`{"contents":[]}`))
	request.URL.Path = "/v1beta/models/text-model:generateContent"
	response := httptest.NewRecorder()
	billableGeminiHandler(fake, executor).ServeHTTP(response, request)
	if response.Code != http.StatusPreconditionFailed || executor.calls != 0 || len(fake.events) != 0 {
		t.Fatalf("response=%d calls=%d events=%v", response.Code, executor.calls, fake.events)
	}
}
