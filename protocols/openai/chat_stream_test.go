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

	"github.com/nativegatewayhq/gateway/internal/chatpricing"
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/providerhealth"
	chatoperation "github.com/nativegatewayhq/gateway/operations/chat"
	openaiProvider "github.com/nativegatewayhq/gateway/providers/openai"
)

func TestRelayNativeStreamPreservesBytesAndObservesUsage(t *testing.T) {
	input := ": keepalive\r\n\r\ndata: {\"id\":\"one\",\"choices\":[]}\r\n\r\ndata: {\"id\":\"one\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"prompt_tokens_details\":{\"cached_tokens\":3},\"completion_tokens\":4}}\r\n\r\ndata: [DONE]\r\n\r\n"
	w := httptest.NewRecorder()
	result, err := relayNativeStream(w, strings.NewReader(input), 4096, true)
	if err != nil || w.Body.String() != input || !result.Done || !result.UsageFound || result.Usage != (chatpricing.Usage{PromptTokens: 12, CachedInputTokens: 3, CompletionTokens: 4}) {
		t.Fatalf("result=%+v err=%v body=%q", result, err, w.Body.String())
	}
}

func TestRelayRejectsDuplicateOrMissingTerminalUsage(t *testing.T) {
	duplicate := "data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n"
	if _, err := relayNativeStream(httptest.NewRecorder(), strings.NewReader(duplicate), 4096, true); !errors.Is(err, errStreamProtocol) {
		t.Fatalf("duplicate err=%v", err)
	}
	result, err := relayNativeStream(httptest.NewRecorder(), strings.NewReader("data: [DONE]\n\n"), 4096, true)
	if err != nil || result.UsageFound {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestStreamingUsageRequestedStrictly(t *testing.T) {
	valid := `{"stream_options":{"include_usage":true}}`
	if ok, err := streamingUsageRequested([]byte(valid)); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	for _, body := range []string{`{}`, `{"stream_options":{"include_usage":false}}`, `{"stream_options":{"include_usage":true,"include_usage":true}}`, `{"stream_options":{"include_usage":"yes"}}`} {
		if _, err := streamingUsageRequested([]byte(body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestBillableStreamingSettlesTerminalUsageAndPreservesWire(t *testing.T) {
	registry, _ := chatoperation.NewRegistryWithLimits([]string{"gpt-4.1"}, map[string]chatoperation.Limits{"gpt-4.1": {MaximumInputTokens: 4096, MaximumOutputTokens: 1024}})
	billingFake := &chatBillingFake{}
	stream := "data: {\"id\":\"one\",\"choices\":[]}\n\ndata: {\"id\":\"one\",\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"
	h := NewBillableChatHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	}), channelAvailability{"channel_00000000000000000000000000000001": true}, providerhealth.NoopGate{}, 4096, billingFake)
	r := chatRequest(`{"model":"gpt-4.1","messages":[],"stream":true,"stream_options":{"include_usage":true},"max_completion_tokens":20}`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != stream || billingFake.beginCalls != 1 || billingFake.completeCalls != 1 || billingFake.reconcileCalls != 0 {
		t.Fatalf("status=%d body=%q billing=%+v", w.Code, w.Body.String(), billingFake)
	}
}

func TestBillableStreamingMissingUsageHoldsReservation(t *testing.T) {
	registry, _ := chatoperation.NewRegistryWithLimits([]string{"gpt-4.1"}, map[string]chatoperation.Limits{"gpt-4.1": {MaximumInputTokens: 4096, MaximumOutputTokens: 1024}})
	billingFake := &chatBillingFake{}
	h := NewBillableChatHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
	}), channelAvailability{"channel_00000000000000000000000000000001": true}, providerhealth.NoopGate{}, 4096, billingFake)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, chatRequest(`{"model":"gpt-4.1","stream":true,"stream_options":{"include_usage":true},"max_tokens":20}`))
	if billingFake.completeCalls != 0 || billingFake.reconcileCalls != 1 {
		t.Fatalf("billing=%+v", billingFake)
	}
}

type disconnectWriter struct {
	header http.Header
	status int
}

func (w *disconnectWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *disconnectWriter) WriteHeader(status int)  { w.status = status }
func (*disconnectWriter) Write([]byte) (int, error) { return 0, errors.New("client disconnected") }
func (*disconnectWriter) Flush()                    {}

func TestBillableStreamingClientDisconnectHoldsReservation(t *testing.T) {
	registry, _ := chatoperation.NewRegistryWithLimits([]string{"gpt-4.1"}, map[string]chatoperation.Limits{"gpt-4.1": {MaximumInputTokens: 4096, MaximumOutputTokens: 1024}})
	billingFake := &chatBillingFake{}
	h := NewBillableChatHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"choices\":[]}\n\n"))}, nil
	}), channelAvailability{"channel_00000000000000000000000000000001": true}, providerhealth.NoopGate{}, 4096, billingFake)
	h.ServeHTTP(&disconnectWriter{}, chatRequest(`{"model":"gpt-4.1","stream":true,"stream_options":{"include_usage":true},"max_tokens":20}`))
	if billingFake.completeCalls != 0 || billingFake.releaseCalls != 0 || billingFake.reconcileCalls != 1 {
		t.Fatalf("billing=%+v", billingFake)
	}
}

func TestRoutedStreamingDisconnectNeverDispatchesFallbackProvider(t *testing.T) {
	registry := routedChatRegistry(t, chatoperation.Priority,
		chatoperation.Candidate{ID: "candidate_openai", Provider: providercredentials.OpenAI, ProviderModel: "gpt", ChannelID: "channel_00000000000000000000000000000001", Enabled: true, Priority: 1, Capabilities: chatoperation.Capabilities{Streaming: true}},
		chatoperation.Candidate{ID: "candidate_xai", Provider: providercredentials.XAI, ProviderModel: "grok", ChannelID: "channel_00000000000000000000000000000002", Enabled: true, Priority: 2, Capabilities: chatoperation.Capabilities{Streaming: true}},
	)
	billingFake := &routedBillingFake{chatBillingFake: &chatBillingFake{}, quotes: map[string]int64{"channel_00000000000000000000000000000001": 10, "channel_00000000000000000000000000000002": 20}, beginErrors: map[string]error{}}
	calls := map[providercredentials.ProviderID]int{}
	executors := map[providercredentials.ProviderID]ChatExecutor{providercredentials.OpenAI: chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		calls[providercredentials.OpenAI]++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"choices\":[]}\n\n"))}, nil
	}), providercredentials.XAI: chatExecutorFunc(func(context.Context, openaiProvider.ChatRequest) (*http.Response, error) {
		calls[providercredentials.XAI]++
		return nil, nil
	})}
	availability := channelAvailability{"channel_00000000000000000000000000000001": true, "channel_00000000000000000000000000000002": true}
	handler := NewBillableRoutedChatHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), acceptingAuth(t), registry, executors, availability, providerhealth.NoopGate{}, 4096, billingFake)
	handler.ServeHTTP(&disconnectWriter{}, chatRequest(`{"model":"logical-chat","stream":true,"stream_options":{"include_usage":true},"max_tokens":20}`))
	if calls[providercredentials.OpenAI] != 1 || calls[providercredentials.XAI] != 0 || billingFake.reconcileCalls != 1 || billingFake.releaseCalls != 0 {
		t.Fatalf("calls=%v billing=%+v", calls, billingFake)
	}
}
