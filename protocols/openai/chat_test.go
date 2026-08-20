package openai

import (
	"bytes"
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
	"github.com/nativegatewayhq/gateway/internal/billing"
	"github.com/nativegatewayhq/gateway/internal/chatbilling"
	"github.com/nativegatewayhq/gateway/internal/chatpricing"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	"github.com/nativegatewayhq/gateway/internal/spendcap"
	chatoperation "github.com/nativegatewayhq/gateway/operations/chat"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
)

type chatBillingFake struct {
	beginCalls, completeCalls, releaseCalls, reconcileCalls int
	charge                                                  chatbilling.Charge
	usage                                                   chatpricing.Usage
	replayCharge                                            chatbilling.Charge
	replayFound                                             bool
	replayErr                                               error
}

func (f *chatBillingFake) Quote(_ context.Context, r chatbilling.BeginRequest) (chatpricing.Estimate, error) {
	return chatpricing.Estimate{Price: chatpricing.Price{ChannelID: r.ChannelID, EffectiveFrom: time.Now()}, EstimatedCost: 1, MaximumSale: 2}, nil
}

func (f *chatBillingFake) Begin(_ context.Context, r chatbilling.BeginRequest) (chatbilling.Charge, error) {
	f.beginCalls++
	f.charge = chatbilling.Charge{ID: "chc_00000000000000000000000000000001", MaximumInputTokens: r.MaximumInputTokens, MaximumOutputTokens: r.MaximumOutputTokens}
	return f.charge, nil
}
func (f *chatBillingFake) Replay(context.Context, chatbilling.BeginRequest) (chatbilling.Charge, bool, error) {
	return f.replayCharge, f.replayFound, f.replayErr
}
func (f *chatBillingFake) CompleteUsage(_ context.Context, _ string, u chatpricing.Usage, s billing.ResponseSnapshot) (chatbilling.Charge, error) {
	f.completeCalls++
	f.usage = u
	f.charge.Response = s
	return f.charge, nil
}
func (f *chatBillingFake) CompleteStreamUsage(_ context.Context, _ string, u chatpricing.Usage, _ [32]byte) (chatbilling.Charge, error) {
	f.completeCalls++
	f.usage = u
	return f.charge, nil
}
func (f *chatBillingFake) Release(_ context.Context, _ string, s billing.ResponseSnapshot) (chatbilling.Charge, error) {
	f.releaseCalls++
	f.charge.Response = s
	return f.charge, nil
}
func (f *chatBillingFake) MarkReconciling(context.Context, string, string, *billing.ResponseSnapshot) error {
	f.reconcileCalls++
	return nil
}
func (f *chatBillingFake) MarkReconcilingUsage(context.Context, string, string, *billing.ResponseSnapshot, chatpricing.Usage) error {
	f.reconcileCalls++
	return nil
}
func (f *chatBillingFake) MarkStreamReconcilingUsage(context.Context, string, chatpricing.Usage, [32]byte) error {
	f.reconcileCalls++
	return nil
}
func (f *chatBillingFake) MarkStreamReconciling(context.Context, string, string, string, string) error {
	f.reconcileCalls++
	return nil
}

type chatExecutorFunc func(context.Context, openaiProvider.ChatRequest) (*http.Response, error)

func (f chatExecutorFunc) Complete(ctx context.Context, r openaiProvider.ChatRequest) (*http.Response, error) {
	return f(ctx, r)
}
func chatRegistry(t *testing.T) *chatoperation.Registry {
	t.Helper()
	r, err := chatoperation.NewRegistry([]string{"gpt-4.1"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func chatHandler(t *testing.T, auth Authenticator, executor ChatExecutor, maximum int64) *ChatHandler {
	t.Helper()
	return NewChatHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), auth, chatRegistry(t), executor, channelAvailability{"channel_00000000000000000000000000000001": true}, providerhealth.NoopGate{}, maximum)
}
func chatRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer service-secret")
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestChatPreservesNativeBodyAndResponse(t *testing.T) {
	input := `{"model":"gpt-4.1","messages":[{"role":"user","content":"secret prompt"}],"future_field":{"x":1},"stream":false}`
	calls := 0
	handler := chatHandler(t, acceptingAuth(t), chatExecutorFunc(func(_ context.Context, r openaiProvider.ChatRequest) (*http.Response, error) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, []byte(input)) {
			t.Fatalf("body=%s", body)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}, "Set-Cookie": {"secret=x"}, "Authorization": {"provider-secret"}}, Body: io.NopCloser(strings.NewReader(`{"id":"chatcmpl_1","choices":[]}`))}, nil
	}), 4096)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(input))
	if w.Code != 200 || calls != 1 || w.Body.String() != `{"id":"chatcmpl_1","choices":[]}` || w.Header().Get("Set-Cookie") != "" || w.Header().Get("Authorization") != "" {
		t.Fatalf("status=%d calls=%d headers=%v body=%q", w.Code, calls, w.Header(), w.Body.String())
	}
}

func TestChatRejectsBeforeDispatch(t *testing.T) {
	calls := 0
	executor := chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) { calls++; return nil, nil })
	principal := apikey.Principal{ModelAccessMode: apikey.ModelAccessAllowlist, ModelPermissions: []apikey.ModelPermission{{Protocol: "openai", Operation: "image.generate", Model: "gpt-4.1"}}}
	tests := []struct {
		name, body string
		mutate     func(*http.Request)
		status     int
	}{{"trailing", `{"model":"gpt-4.1"}{}`, nil, 400}, {"missing model", `{"messages":[]}`, nil, 400}, {"compressed", `{"model":"gpt-4.1"}`, func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }, 415}, {"unauthorized model", `{"model":"gpt-4.1"}`, nil, 403}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth := acceptingAuth(t)
			if test.name == "unauthorized model" {
				auth = authFunc(func(context.Context, string) (apikey.Principal, error) { return principal, nil })
			}
			handler := chatHandler(t, auth, executor, 4096)
			r := chatRequest(test.body)
			if test.mutate != nil {
				test.mutate(r)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	if calls != 0 {
		t.Fatalf("provider calls=%d", calls)
	}
}

func TestChatBoundsAndMapsExecutorFailures(t *testing.T) {
	handler := chatHandler(t, acceptingAuth(t), chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		return nil, openaiProvider.ErrChatTimeout
	}), 64)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(`{"model":"gpt-4.1"}`))
	if w.Code != 504 {
		t.Fatalf("timeout=%d", w.Code)
	}
	large := chatHandler(t, acceptingAuth(t), chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		return nil, errors.New("unexpected")
	}), 8)
	w = httptest.NewRecorder()
	large.ServeHTTP(w, chatRequest(`{"model":"gpt-4.1"}`))
	if w.Code != 413 {
		t.Fatalf("large=%d", w.Code)
	}
	responseLarge := chatHandler(t, acceptingAuth(t), chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 100)))}, nil
	}), 64)
	w = httptest.NewRecorder()
	responseLarge.ServeHTTP(w, chatRequest(`{"model":"gpt-4.1"}`))
	if w.Code != 502 {
		t.Fatalf("large response=%d", w.Code)
	}
}

func TestBillableChatReservesThenSettlesNativeUsage(t *testing.T) {
	registry, err := chatoperation.NewRegistryWithLimits([]string{"gpt-4.1"}, map[string]chatoperation.Limits{"gpt-4.1": {MaximumInputTokens: 4096, MaximumOutputTokens: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	billingFake := &chatBillingFake{}
	dispatchedAfterReserve := false
	handler := NewBillableChatHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		dispatchedAfterReserve = billingFake.beginCalls == 1
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"chatcmpl_1","choices":[],"usage":{"prompt_tokens":12,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens":4}}`))}, nil
	}), channelAvailability{"channel_00000000000000000000000000000001": true}, providerhealth.NoopGate{}, 4096, billingFake)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(`{"model":"gpt-4.1","messages":[],"max_completion_tokens":20}`))
	if w.Code != 200 || !dispatchedAfterReserve || billingFake.completeCalls != 1 || billingFake.usage != (chatpricing.Usage{PromptTokens: 12, CachedInputTokens: 3, CompletionTokens: 4}) {
		t.Fatalf("status=%d reserve=%d complete=%d usage=%+v", w.Code, billingFake.beginCalls, billingFake.completeCalls, billingFake.usage)
	}
}

func TestBillableChatRejectsMissingOutputLimitBeforeReserve(t *testing.T) {
	registry, _ := chatoperation.NewRegistryWithLimits([]string{"gpt-4.1"}, map[string]chatoperation.Limits{"gpt-4.1": {MaximumInputTokens: 4096, MaximumOutputTokens: 1024}})
	billingFake := &chatBillingFake{}
	calls := 0
	handler := NewBillableChatHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) { calls++; return nil, nil }), channelAvailability{"channel_00000000000000000000000000000001": true}, providerhealth.NoopGate{}, 4096, billingFake)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(`{"model":"gpt-4.1","messages":[]}`))
	if w.Code != 400 || calls != 0 || billingFake.beginCalls != 0 {
		t.Fatalf("status=%d provider=%d reserve=%d", w.Code, calls, billingFake.beginCalls)
	}
}

func TestBillableChatReplaysBeforeCurrentRouteAvailability(t *testing.T) {
	registry, _ := chatoperation.NewRegistryWithLimits([]string{"gpt-4.1"}, map[string]chatoperation.Limits{"gpt-4.1": {MaximumInputTokens: 4096, MaximumOutputTokens: 1024}})
	billingFake := &chatBillingFake{replayFound: true, replayCharge: chatbilling.Charge{Response: billing.ResponseSnapshot{Status: 200, Headers: map[string][]string{"Content-Type": {"application/json"}}, Body: []byte(`{"stored":true}`)}}}
	handler := NewBillableRoutedChatHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, nil, nil, &healthGateFake{inspectErr: providerhealth.ErrUnavailable}, 4096, billingFake)
	request := chatRequest(`{"model":"gpt-4.1","messages":[],"max_completion_tokens":20}`)
	request.Header.Set("Idempotency-Key", "stored-route")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, request)
	if w.Code != 200 || w.Body.String() != `{"stored":true}` || billingFake.beginCalls != 0 || len(handler.executors) != 0 {
		t.Fatalf("status=%d body=%s begins=%d", w.Code, w.Body.String(), billingFake.beginCalls)
	}
}

func TestExtractChatUsageRejectsMalformedValues(t *testing.T) {
	for _, body := range []string{`{}`, `{"usage":{"prompt_tokens":1.5,"completion_tokens":1}}`, `{"usage":{"prompt_tokens":1,"prompt_tokens_details":{"cached_tokens":2},"completion_tokens":1}}`} {
		if _, err := extractChatUsage([]byte(body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func routedChatRegistry(t *testing.T, policy chatoperation.Policy, candidates ...chatoperation.Candidate) *chatoperation.Registry {
	t.Helper()
	fixed := ""
	if policy == chatoperation.Fixed {
		fixed = candidates[0].ID
	}
	r, err := chatoperation.NewRouteRegistry([]chatoperation.Route{{Model: "logical-chat", Owner: "gateway", Policy: policy, FixedCandidateID: fixed, MaximumInputTokens: 4096, MaximumOutputTokens: 1024, Candidates: candidates}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestChatPriorityRouteFallsBackBeforeDispatchAndRewritesOnlyModel(t *testing.T) {
	registry := routedChatRegistry(t, chatoperation.Priority,
		chatoperation.Candidate{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "gpt-provider", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Priority: 1, Capabilities: chatoperation.Capabilities{Tools: true}},
		chatoperation.Candidate{ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "grok-provider", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Priority: 2, Capabilities: chatoperation.Capabilities{Tools: true}},
	)
	openAICalls, xAICalls := 0, 0
	executors := map[providercredentials.ProviderID]ChatExecutor{
		providercredentials.OpenAI: chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
			openAICalls++
			return nil, errors.New("must not dispatch")
		}),
		providercredentials.XAI: chatExecutorFunc(func(_ context.Context, request openaiProvider.ChatRequest) (*http.Response, error) {
			xAICalls++
			got, _ := io.ReadAll(request.Body)
			want := "{ \"future\" : [1,2], \"model\" : \"grok-provider\", \"messages\" : [] }"
			if string(got) != want {
				t.Fatalf("rewritten=%q", got)
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"xai","choices":[]}`))}, nil
		}),
	}
	handler := NewRoutedChatHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, executors, channelAvailability{"channel_00000000000000000000000000000002": true}, providerhealth.NoopGate{}, 4096)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(`{ "future" : [1,2], "model" : "logical-chat", "messages" : [] }`))
	if w.Code != 200 || openAICalls != 0 || xAICalls != 1 {
		t.Fatalf("status=%d openai=%d xai=%d body=%s", w.Code, openAICalls, xAICalls, w.Body.String())
	}
}

func TestChatDoesNotFallbackAfterProviderDispatch(t *testing.T) {
	registry := routedChatRegistry(t, chatoperation.Priority,
		chatoperation.Candidate{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "gpt-provider", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Priority: 1},
		chatoperation.Candidate{ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "grok-provider", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Priority: 2},
	)
	calls := map[providercredentials.ProviderID]int{}
	executors := map[providercredentials.ProviderID]ChatExecutor{
		providercredentials.OpenAI: chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
			calls[providercredentials.OpenAI]++
			return &http.Response{StatusCode: 500, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":"upstream"}`))}, nil
		}),
		providercredentials.XAI: chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
			calls[providercredentials.XAI]++
			return nil, nil
		}),
	}
	availability := channelAvailability{"channel_00000000000000000000000000000001": true, "channel_00000000000000000000000000000002": true}
	handler := NewRoutedChatHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, executors, availability, providerhealth.NoopGate{}, 4096)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(`{"model":"logical-chat","messages":[]}`))
	if w.Code != 500 || calls[providercredentials.OpenAI] != 1 || calls[providercredentials.XAI] != 0 {
		t.Fatalf("status=%d calls=%v", w.Code, calls)
	}
}

func TestChatOpenCircuitAndFailedHalfOpenProbeFallbackBeforeDispatch(t *testing.T) {
	registry := routedChatRegistry(t, chatoperation.Priority,
		chatoperation.Candidate{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "gpt-provider", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Priority: 1},
		chatoperation.Candidate{ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "grok-provider", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Priority: 2},
	)
	health := &healthGateFake{snapshots: map[string]providerhealth.Snapshot{"channel_00000000000000000000000000000001": {State: providerhealth.HalfOpen}}, claimErrors: map[string]error{"channel_00000000000000000000000000000001": providerhealth.ErrUnavailable}}
	calls := map[providercredentials.ProviderID]int{}
	executors := map[providercredentials.ProviderID]ChatExecutor{providercredentials.OpenAI: chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		calls[providercredentials.OpenAI]++
		return nil, nil
	}), providercredentials.XAI: chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		calls[providercredentials.XAI]++
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"choices":[]}`))}, nil
	})}
	availability := channelAvailability{"channel_00000000000000000000000000000001": true, "channel_00000000000000000000000000000002": true}
	handler := NewRoutedChatHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, executors, availability, health, 4096)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(`{"model":"logical-chat","messages":[]}`))
	if w.Code != 200 || calls[providercredentials.OpenAI] != 0 || calls[providercredentials.XAI] != 1 || len(health.claimed) != 1 {
		t.Fatalf("status=%d calls=%v claimed=%v", w.Code, calls, health.claimed)
	}
}

type routedBillingFake struct {
	*chatBillingFake
	quotes      map[string]int64
	beginErrors map[string]error
	begins      []chatbilling.BeginRequest
}

func (f *routedBillingFake) Quote(_ context.Context, request chatbilling.BeginRequest) (chatpricing.Estimate, error) {
	cost, ok := f.quotes[request.ChannelID]
	if !ok {
		return chatpricing.Estimate{}, chatpricing.ErrUnavailable
	}
	return chatpricing.Estimate{Price: chatpricing.Price{ChannelID: request.ChannelID}, EstimatedCost: cost, MaximumSale: cost + 10}, nil
}
func (f *routedBillingFake) Begin(_ context.Context, request chatbilling.BeginRequest) (chatbilling.Charge, error) {
	f.begins = append(f.begins, request)
	if err := f.beginErrors[request.ChannelID]; err != nil {
		return chatbilling.Charge{}, err
	}
	f.charge = chatbilling.Charge{ID: "chc_00000000000000000000000000000001", MaximumInputTokens: request.MaximumInputTokens, MaximumOutputTokens: request.MaximumOutputTokens}
	return f.charge, nil
}

func TestBillableChatLowestCostAndSpendCapFallbackReserveOneRoute(t *testing.T) {
	registry := routedChatRegistry(t, chatoperation.LowestCost,
		chatoperation.Candidate{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "gpt-provider", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Priority: 1},
		chatoperation.Candidate{ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "grok-provider", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Priority: 2},
	)
	billingFake := &routedBillingFake{chatBillingFake: &chatBillingFake{}, quotes: map[string]int64{"channel_00000000000000000000000000000001": 20, "channel_00000000000000000000000000000002": 10}, beginErrors: map[string]error{"channel_00000000000000000000000000000002": spendcap.ErrExceeded}}
	calls := map[providercredentials.ProviderID]int{}
	executors := map[providercredentials.ProviderID]ChatExecutor{
		providercredentials.OpenAI: chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
			calls[providercredentials.OpenAI]++
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1}}`))}, nil
		}),
		providercredentials.XAI: chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
			calls[providercredentials.XAI]++
			return nil, nil
		}),
	}
	availability := channelAvailability{"channel_00000000000000000000000000000001": true, "channel_00000000000000000000000000000002": true}
	handler := NewBillableRoutedChatHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, executors, availability, providerhealth.NoopGate{}, 4096, billingFake)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(`{"model":"logical-chat","messages":[],"max_completion_tokens":20}`))
	if w.Code != 200 || len(billingFake.begins) != 2 || billingFake.begins[0].Provider != "xai" || billingFake.begins[1].Provider != "openai" || billingFake.begins[1].RouteRank != 1 || calls[providercredentials.OpenAI] != 1 || calls[providercredentials.XAI] != 0 {
		t.Fatalf("status=%d begins=%+v calls=%v body=%s", w.Code, billingFake.begins, calls, w.Body.String())
	}
}

func TestBillableWeightedRouteResamplesRemainingCandidates(t *testing.T) {
	registry := routedChatRegistry(t, chatoperation.Weighted,
		chatoperation.Candidate{ID: "candidate_a", Provider: providercredentials.OpenAI, ProviderModel: "a", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Weight: 1},
		chatoperation.Candidate{ID: "candidate_b", Provider: providercredentials.XAI, ProviderModel: "b", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Weight: 1},
	)
	billingFake := &routedBillingFake{chatBillingFake: &chatBillingFake{}, quotes: map[string]int64{"channel_00000000000000000000000000000001": 10, "channel_00000000000000000000000000000002": 10}, beginErrors: map[string]error{"channel_00000000000000000000000000000001": spendcap.ErrExceeded}}
	executors := map[providercredentials.ProviderID]ChatExecutor{providercredentials.OpenAI: chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		t.Fatal("exhausted weighted route dispatched")
		return nil, nil
	}), providercredentials.XAI: chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1}}`))}, nil
	})}
	availability := channelAvailability{"channel_00000000000000000000000000000001": true, "channel_00000000000000000000000000000002": true}
	handler := NewBillableRoutedChatHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, executors, availability, providerhealth.NoopGate{}, 4096, billingFake)
	handler.weighted, _ = chatoperation.NewWeightedSampler(bytes.NewReader(make([]byte, 64)))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(`{"model":"logical-chat","messages":[],"max_completion_tokens":20}`))
	if w.Code != 200 || len(billingFake.begins) != 2 || billingFake.begins[0].CandidateID != "candidate_a" || billingFake.begins[1].CandidateID != "candidate_b" {
		t.Fatalf("status=%d begins=%+v", w.Code, billingFake.begins)
	}
}

func TestBillableChatSkipsCandidateWithoutExactPriceBeforeReserve(t *testing.T) {
	registry := routedChatRegistry(t, chatoperation.Priority,
		chatoperation.Candidate{ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "grok", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Priority: 1},
		chatoperation.Candidate{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "gpt", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Priority: 2},
	)
	billingFake := &routedBillingFake{chatBillingFake: &chatBillingFake{}, quotes: map[string]int64{"channel_00000000000000000000000000000001": 10}, beginErrors: map[string]error{}}
	executors := map[providercredentials.ProviderID]ChatExecutor{providercredentials.OpenAI: chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))}, nil
	}), providercredentials.XAI: chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		t.Fatal("unpriced route dispatched")
		return nil, nil
	})}
	availability := channelAvailability{"channel_00000000000000000000000000000001": true, "channel_00000000000000000000000000000002": true}
	handler := NewBillableRoutedChatHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, executors, availability, providerhealth.NoopGate{}, 4096, billingFake)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, chatRequest(`{"model":"logical-chat","messages":[],"max_tokens":20}`))
	if w.Code != 200 || len(billingFake.begins) != 1 || billingFake.begins[0].CandidateID != "candidate_openai" {
		t.Fatalf("status=%d begins=%+v", w.Code, billingFake.begins)
	}
}
