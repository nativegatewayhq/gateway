package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestMiddlewareExtractsRemoteParentAndUsesBoundedAttributes(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	traces := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(spans))
	reader := metric.NewManualReader()
	metrics := metric.NewMeterProvider(metric.WithReader(reader))
	recorder, err := NewRecorder(traces, metrics)
	if err != nil {
		t.Fatal(err)
	}
	handler := recorder.Middleware(propagation.TraceContext{}, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("baggage") == "" {
			t.Fatal("test request lost header")
		}
		writer.WriteHeader(http.StatusCreated)
	}))
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations?key=secret", strings.NewReader("secret prompt"))
	request.Header.Set("traceparent", "00-0102030405060708090a0b0c0d0e0f10-0102030405060708-01")
	request.Header.Set("baggage", "customer_id=secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	ended := spans.Ended()
	if response.Code != http.StatusCreated || len(ended) != 1 || ended[0].Parent().TraceID().String() != "0102030405060708090a0b0c0d0e0f10" {
		t.Fatalf("response=%d spans=%+v", response.Code, ended)
	}
	attributes := ended[0].Attributes()
	for _, item := range attributes {
		value := item.Value.Emit()
		if strings.Contains(value, "secret") || strings.Contains(value, "key=") || strings.Contains(value, "customer") {
			t.Fatalf("unsafe attribute=%s:%s", item.Key, value)
		}
	}
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	foundRequests, foundDuration, foundActive := false, false, false
	for _, scope := range collected.ScopeMetrics {
		for _, item := range scope.Metrics {
			switch item.Name {
			case "gateway.http.server.requests":
				foundRequests = true
			case "gateway.http.server.duration":
				foundDuration = true
			case "gateway.http.server.active_requests":
				foundActive = true
			}
		}
	}
	if !foundRequests || !foundDuration || !foundActive {
		t.Fatalf("metrics=%+v", collected.ScopeMetrics)
	}
}

func TestTypedRecordsCollapseUnknownCardinality(t *testing.T) {
	reader := metric.NewManualReader()
	metrics := metric.NewMeterProvider(metric.WithReader(reader))
	recorder, err := NewRecorder(sdktrace.NewTracerProvider(), metrics)
	if err != nil {
		t.Fatal(err)
	}
	recorder.Route(context.Background(), RouteRecord{Protocol: "customer-secret", Operation: "model-secret", Policy: "candidate-secret", Outcome: "raw-error-secret"})
	recorder.Job(context.Background(), JobRecord{Protocol: "tenant-secret", Stage: "raw-stage-secret", Status: "raw-status-secret", Outcome: "raw-error-secret"})
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	for _, scope := range collected.ScopeMetrics {
		for _, item := range scope.Metrics {
			if sum, ok := item.Data.(metricdata.Sum[int64]); ok {
				for _, point := range sum.DataPoints {
					for _, attribute := range point.Attributes.ToSlice() {
						if strings.Contains(attribute.Value.Emit(), "secret") {
							t.Fatalf("metric leaked value: %+v", attribute)
						}
					}
				}
			}
		}
	}
}
