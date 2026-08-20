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
	"github.com/nativegatewayhq/gateway/internal/providercredentials"
	"github.com/nativegatewayhq/gateway/internal/requestid"
	"github.com/nativegatewayhq/gateway/internal/telemetry"
	"github.com/nativegatewayhq/gateway/providers/openaiimages"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func telemetryRecorder(t *testing.T) (*telemetry.Recorder, *tracetest.SpanRecorder) {
	t.Helper()
	spans := tracetest.NewSpanRecorder()
	traces := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(spans))
	recorder, err := telemetry.NewRecorder(traces, noop.NewMeterProvider())
	if err != nil {
		t.Fatal(err)
	}
	return recorder, spans
}

func telemetryImageRequest(handler http.Handler) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer service-secret")
	response := httptest.NewRecorder()
	requestid.Middleware(handler).ServeHTTP(response, request)
	return response
}

func TestProviderSpanIsChildOfServerSpanOnlyForDispatch(t *testing.T) {
	recorder, spans := telemetryRecorder(t)
	billing := &billingFake{}
	handler := NewBillableImagesHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), billingAuth(), testRegistry(t), map[providercredentials.ProviderID]Executor{providercredentials.OpenAI: executorFunc(func(context.Context, openaiimages.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}, nil
	})}, 1024, billing)
	handler.SetTelemetry(recorder)
	response := telemetryImageRequest(recorder.Middleware(propagation.TraceContext{}, handler))
	if response.Code != 200 {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
	ended := spans.Ended()
	if len(ended) != 2 {
		t.Fatalf("spans=%+v", ended)
	}
	var server, provider sdktrace.ReadOnlySpan
	for _, span := range ended {
		if span.Name() == "provider image.generate" {
			provider = span
		} else {
			server = span
		}
	}
	if provider == nil || server == nil || provider.Parent().SpanID() != server.SpanContext().SpanID() {
		t.Fatalf("server=%v provider=%v", server, provider)
	}
}

func TestTerminalReplayCreatesNoProviderSpan(t *testing.T) {
	recorder, spans := telemetryRecorder(t)
	billing := &billingFake{replayFound: true, replayCharge: chargebilling.Charge{Response: chargebilling.ResponseSnapshot{Status: 200, Body: []byte(`{"data":[]}`)}}}
	handler := NewBillableImagesHandler(slog.Default(), billingAuth(), testRegistry(t), nil, 1024, billing)
	handler.SetTelemetry(recorder)
	response := telemetryImageRequest(recorder.Middleware(propagation.TraceContext{}, handler))
	if response.Code != 200 || len(spans.Ended()) != 1 {
		t.Fatalf("response=%d spans=%+v", response.Code, spans.Ended())
	}
}
