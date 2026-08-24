package telemetry

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type Recorder struct {
	tracer             trace.Tracer
	httpRequests       metric.Int64Counter
	httpDuration       metric.Float64Histogram
	httpActive         metric.Int64UpDownCounter
	providerRequests   metric.Int64Counter
	providerDuration   metric.Float64Histogram
	routes             metric.Int64Counter
	billing            metric.Int64Counter
	storage            metric.Int64Counter
	reconciliation     metric.Int64Counter
	authentication     metric.Int64Counter
	jobs               metric.Int64Counter
	jobUsage           metric.Int64Histogram
	chatStreams        metric.Int64Counter
	chatFirstByte      metric.Float64Histogram
	chatStreamDuration metric.Float64Histogram
}

func NewRecorder(traces trace.TracerProvider, metrics metric.MeterProvider) (*Recorder, error) {
	if traces == nil || metrics == nil {
		return nil, errors.New("telemetry provider unavailable")
	}
	meter := metrics.Meter("github.com/nativegatewayhq/gateway")
	httpRequests, err := meter.Int64Counter("gateway.http.server.requests", metric.WithUnit("{request}"))
	if err != nil {
		return nil, err
	}
	httpDuration, err := meter.Float64Histogram("gateway.http.server.duration", metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}
	httpActive, err := meter.Int64UpDownCounter("gateway.http.server.active_requests", metric.WithUnit("{request}"))
	if err != nil {
		return nil, err
	}
	providerRequests, err := meter.Int64Counter("gateway.provider.requests", metric.WithUnit("{request}"))
	if err != nil {
		return nil, err
	}
	providerDuration, err := meter.Float64Histogram("gateway.provider.duration", metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}
	routes, err := meter.Int64Counter("gateway.routing.decisions", metric.WithUnit("{decision}"))
	if err != nil {
		return nil, err
	}
	billing, err := meter.Int64Counter("gateway.billing.transitions", metric.WithUnit("{transition}"))
	if err != nil {
		return nil, err
	}
	storage, err := meter.Int64Counter("gateway.storage.operations", metric.WithUnit("{operation}"))
	if err != nil {
		return nil, err
	}
	reconciliation, err := meter.Int64Counter("gateway.reconciliation.tasks", metric.WithUnit("{task}"))
	if err != nil {
		return nil, err
	}
	authentication, err := meter.Int64Counter("gateway.authentication.decisions", metric.WithUnit("{decision}"))
	if err != nil {
		return nil, err
	}
	jobs, err := meter.Int64Counter("gateway.jobs.transitions", metric.WithUnit("{transition}"))
	if err != nil {
		return nil, err
	}
	jobUsage, err := meter.Int64Histogram("gateway.jobs.usage.quantity", metric.WithUnit("{image}"))
	if err != nil {
		return nil, err
	}
	chatStreams, err := meter.Int64Counter("gateway.chat.streams", metric.WithUnit("{stream}"))
	if err != nil {
		return nil, err
	}
	chatFirstByte, err := meter.Float64Histogram("gateway.chat.stream.first_byte", metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}
	chatStreamDuration, err := meter.Float64Histogram("gateway.chat.stream.duration", metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}
	return &Recorder{tracer: traces.Tracer("github.com/nativegatewayhq/gateway"), httpRequests: httpRequests, httpDuration: httpDuration, httpActive: httpActive, providerRequests: providerRequests, providerDuration: providerDuration, routes: routes, billing: billing, storage: storage, reconciliation: reconciliation, authentication: authentication, jobs: jobs, jobUsage: jobUsage, chatStreams: chatStreams, chatFirstByte: chatFirstByte, chatStreamDuration: chatStreamDuration}, nil
}

type HTTPRecord struct {
	Protocol, Operation, Route string
	Status                     int
	Duration                   time.Duration
}

type ProviderRecord struct {
	Provider, Protocol, Operation, Outcome string
	Duration                               time.Duration
}

type RouteRecord struct{ Protocol, Operation, Policy, Outcome, Rejection string }
type AuthenticationRecord struct{ Protocol, Stage, Outcome string }
type JobRecord struct{ Protocol, Stage, Status, Outcome string }
type JobUsageRecord struct {
	Protocol, Kind, Outcome, Reason string
	Quantity                        int64
}
type BillingRecord struct{ Protocol, Operation, Transition, Outcome string }
type ChatStreamRecord struct {
	TerminalCategory, DisconnectSide string
	FirstByte, Duration              time.Duration
}
type LLMStreamRecord struct {
	Protocol, Operation, TerminalCategory, DisconnectSide string
	FirstByte, Duration                                   time.Duration
}
type StorageRecord struct{ Protocol, Stage, Source, Outcome string }
type ReconciliationRecord struct {
	Outcome string
	Count   int64
}

type contextMetadataKey struct{}
type contextMetadata struct{ protocol, operation string }

func ProtocolOperation(ctx context.Context) (string, string) {
	metadata, _ := ctx.Value(contextMetadataKey{}).(contextMetadata)
	return boundedProtocol(metadata.protocol), boundedOperation(metadata.operation)
}

func (recorder *Recorder) RecordHTTP(ctx context.Context, record HTTPRecord) {
	attributes := metric.WithAttributes(attribute.String("gateway.protocol", boundedProtocol(record.Protocol)), attribute.String("gateway.operation", boundedOperation(record.Operation)), attribute.String("http.route", boundedRoute(record.Route)), attribute.String("http.response.status_class", statusClass(record.Status)))
	recorder.httpRequests.Add(ctx, 1, attributes)
	recorder.httpDuration.Record(ctx, record.Duration.Seconds(), attributes)
}

func (recorder *Recorder) StartProvider(ctx context.Context, provider, protocol, operation string) (context.Context, trace.Span, time.Time) {
	attributes := []attribute.KeyValue{attribute.String("server.address", boundedProvider(provider)), attribute.String("gateway.protocol", boundedProtocol(protocol)), attribute.String("gateway.operation", boundedOperation(operation))}
	ctx, span := recorder.tracer.Start(ctx, "provider "+boundedOperation(operation), trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attributes...))
	return ctx, span, time.Now()
}

func (recorder *Recorder) EndProvider(ctx context.Context, span trace.Span, started time.Time, record ProviderRecord) {
	outcome := boundedOutcome(record.Outcome)
	attributes := metric.WithAttributes(attribute.String("gateway.provider", boundedProvider(record.Provider)), attribute.String("gateway.protocol", boundedProtocol(record.Protocol)), attribute.String("gateway.operation", boundedOperation(record.Operation)), attribute.String("gateway.outcome", outcome))
	recorder.providerRequests.Add(ctx, 1, attributes)
	recorder.providerDuration.Record(ctx, time.Since(started).Seconds(), attributes)
	span.SetAttributes(attribute.String("gateway.outcome", outcome))
	if outcome != "success" && outcome != "canceled" && outcome != "neutral" {
		span.SetStatus(codes.Error, outcome)
	}
	span.End()
}

func (recorder *Recorder) Route(ctx context.Context, record RouteRecord) {
	recorder.routes.Add(ctx, 1, metric.WithAttributes(attribute.String("gateway.protocol", boundedProtocol(record.Protocol)), attribute.String("gateway.operation", boundedOperation(record.Operation)), attribute.String("gateway.route.policy", boundedPolicy(record.Policy)), attribute.String("gateway.outcome", boundedOutcome(record.Outcome)), attribute.String("gateway.route.rejection", boundedRejection(record.Rejection))))
}
func (recorder *Recorder) Authentication(ctx context.Context, record AuthenticationRecord) {
	recorder.authentication.Add(ctx, 1, metric.WithAttributes(attribute.String("gateway.protocol", boundedProtocol(record.Protocol)), attribute.String("gateway.auth.stage", boundedAuthStage(record.Stage)), attribute.String("gateway.outcome", boundedOutcome(record.Outcome))))
}
func (recorder *Recorder) Job(ctx context.Context, record JobRecord) {
	recorder.jobs.Add(ctx, 1, metric.WithAttributes(attribute.String("gateway.protocol", boundedProtocol(record.Protocol)), attribute.String("gateway.job.stage", boundedJobStage(record.Stage)), attribute.String("gateway.job.status", boundedJobStatus(record.Status)), attribute.String("gateway.outcome", boundedOutcome(record.Outcome))))
}
func (recorder *Recorder) JobUsage(ctx context.Context, record JobUsageRecord) {
	if record.Quantity < 0 || record.Quantity > 128 {
		return
	}
	recorder.jobUsage.Record(ctx, record.Quantity, metric.WithAttributes(attribute.String("gateway.protocol", boundedProtocol(record.Protocol)), attribute.String("gateway.job.usage.kind", allowed(record.Kind, "estimated", "actual")), attribute.String("gateway.outcome", boundedOutcome(record.Outcome)), attribute.String("gateway.job.usage.reason", boundedUsageReason(record.Reason))))
}
func (recorder *Recorder) Billing(ctx context.Context, record BillingRecord) {
	recorder.billing.Add(ctx, 1, metric.WithAttributes(attribute.String("gateway.protocol", boundedProtocol(record.Protocol)), attribute.String("gateway.operation", boundedOperation(record.Operation)), attribute.String("gateway.billing.transition", boundedTransition(record.Transition)), attribute.String("gateway.outcome", boundedOutcome(record.Outcome))))
}
func (recorder *Recorder) ChatStream(ctx context.Context, record ChatStreamRecord) {
	attributes := metric.WithAttributes(attribute.String("gateway.stream.terminal", allowed(record.TerminalCategory, "complete", "missing_usage", "invalid_usage", "missing_done", "write_failed", "provider_error", "client_disconnect")), attribute.String("gateway.stream.disconnect_side", allowed(record.DisconnectSide, "none", "client", "provider")))
	recorder.chatStreams.Add(ctx, 1, attributes)
	if record.FirstByte > 0 {
		recorder.chatFirstByte.Record(ctx, record.FirstByte.Seconds(), attributes)
	}
	if record.Duration >= 0 {
		recorder.chatStreamDuration.Record(ctx, record.Duration.Seconds(), attributes)
	}
}
func (recorder *Recorder) LLMStream(ctx context.Context, record LLMStreamRecord) {
	attributes := metric.WithAttributes(
		attribute.String("gateway.protocol", boundedProtocol(record.Protocol)),
		attribute.String("gateway.operation", boundedOperation(record.Operation)),
		attribute.String("gateway.stream.terminal", allowed(record.TerminalCategory, "complete", "missing_usage", "invalid_usage", "missing_terminal", "write_failed", "provider_error", "client_disconnect", "response_failed", "response_incomplete", "error_event")),
		attribute.String("gateway.stream.disconnect_side", allowed(record.DisconnectSide, "none", "client", "provider")),
	)
	recorder.chatStreams.Add(ctx, 1, attributes)
	if record.FirstByte > 0 {
		recorder.chatFirstByte.Record(ctx, record.FirstByte.Seconds(), attributes)
	}
	if record.Duration >= 0 {
		recorder.chatStreamDuration.Record(ctx, record.Duration.Seconds(), attributes)
	}
}
func (recorder *Recorder) Storage(ctx context.Context, record StorageRecord) {
	recorder.storage.Add(ctx, 1, metric.WithAttributes(attribute.String("gateway.protocol", boundedProtocol(record.Protocol)), attribute.String("gateway.storage.stage", boundedStage(record.Stage)), attribute.String("gateway.storage.source", boundedSource(record.Source)), attribute.String("gateway.outcome", boundedOutcome(record.Outcome))))
}
func (recorder *Recorder) Reconciliation(ctx context.Context, record ReconciliationRecord) {
	if record.Count < 0 {
		return
	}
	recorder.reconciliation.Add(ctx, record.Count, metric.WithAttributes(attribute.String("gateway.outcome", boundedOutcome(record.Outcome))))
}

func (recorder *Recorder) Middleware(propagator propagation.TextMapPropagator, next http.Handler) http.Handler {
	if propagator == nil {
		propagator = propagation.TraceContext{}
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ctx := propagator.Extract(request.Context(), propagation.HeaderCarrier(request.Header))
		protocol, operation, route := requestMetadata(request)
		ctx = context.WithValue(ctx, contextMetadataKey{}, contextMetadata{protocol: protocol, operation: operation})
		ctx, span := recorder.tracer.Start(ctx, request.Method+" "+route, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attribute.String("http.request.method", request.Method), attribute.String("http.route", route), attribute.String("gateway.protocol", protocol), attribute.String("gateway.operation", operation)))
		started := time.Now()
		recorder.httpActive.Add(ctx, 1, metric.WithAttributes(attribute.String("gateway.protocol", protocol), attribute.String("gateway.operation", operation)))
		response := &telemetryResponseWriter{ResponseWriter: writer}
		defer func() {
			status := response.status
			if status == 0 {
				status = http.StatusOK
			}
			recorder.httpActive.Add(ctx, -1, metric.WithAttributes(attribute.String("gateway.protocol", protocol), attribute.String("gateway.operation", operation)))
			recorder.RecordHTTP(ctx, HTTPRecord{Protocol: protocol, Operation: operation, Route: route, Status: status, Duration: time.Since(started)})
			span.SetAttributes(attribute.Int("http.response.status_code", status))
			if status >= 500 {
				span.SetStatus(codes.Error, statusClass(status))
			}
			span.End()
		}()
		next.ServeHTTP(response, request.WithContext(ctx))
	})
}

type telemetryResponseWriter struct {
	http.ResponseWriter
	status int
}

func (writer *telemetryResponseWriter) WriteHeader(status int) {
	if writer.status == 0 {
		writer.status = status
		writer.ResponseWriter.WriteHeader(status)
	}
}
func (writer *telemetryResponseWriter) Write(body []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	return writer.ResponseWriter.Write(body)
}
func (writer *telemetryResponseWriter) Unwrap() http.ResponseWriter { return writer.ResponseWriter }
func (writer *telemetryResponseWriter) Flush() {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	if flusher, ok := writer.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func requestMetadata(request *http.Request) (string, string, string) {
	path := request.URL.Path
	switch {
	case path == "/v1/images/generations":
		return "openai", "image.generate", "/v1/images/generations"
	case path == "/v1/images/edits":
		return "openai", "image.edit", "/v1/images/edits"
	case path == "/v1/models":
		return "openai", "models.list", "/v1/models"
	case path == "/v1/chat/completions":
		return "openai", "chat.completions", "/v1/chat/completions"
	case path == "/v1/audio/speech":
		return "openai", "audio.speech", "/v1/audio/speech"
	case strings.HasPrefix(path, "/v1/audio/speech/assets/") && strings.HasSuffix(path, "/content"):
		return "openai", "audio.speech.asset.content", "/v1/audio/speech/assets/{id}/content"
	case strings.HasPrefix(path, "/v1/audio/speech/assets/") && request.Method == http.MethodDelete:
		return "openai", "audio.speech.asset.delete", "/v1/audio/speech/assets/{id}"
	case strings.HasPrefix(path, "/v1/audio/speech/assets/"):
		return "openai", "audio.speech.asset.get", "/v1/audio/speech/assets/{id}"
	case path == "/v1/audio/transcriptions":
		return "openai", "audio.transcription", "/v1/audio/transcriptions"
	case path == "/v1/audio/translations":
		return "openai", "audio.translation", "/v1/audio/translations"
	case path == "/v1/audio/assets":
		return "openai", "audio.asset.create", "/v1/audio/assets"
	case strings.HasPrefix(path, "/v1/audio/assets/") && request.Method == http.MethodDelete:
		return "openai", "audio.asset.delete", "/v1/audio/assets/{id}"
	case strings.HasPrefix(path, "/v1/audio/assets/"):
		return "openai", "audio.asset.get", "/v1/audio/assets/{id}"
	case path == "/gateway/v1/jobs":
		return "gateway", "jobs.list", "/gateway/v1/jobs"
	case strings.HasPrefix(path, "/gateway/v1/jobs/"):
		return "gateway", "jobs.get", "/gateway/v1/jobs/{id}"
	case path == "/v1/predictions":
		return "replicate", "image.generate", "/v1/predictions"
	case strings.HasPrefix(path, "/v1/predictions/"):
		return "replicate", "image.generate", "/v1/predictions/{id}"
	case strings.HasPrefix(path, "/internal/webhooks/replicate/"):
		return "replicate", "image.generate", "/internal/webhooks/replicate/{job}/{token}"
	case strings.HasPrefix(path, "/internal/webhooks/fal/"):
		return "fal", "image.generate", "/internal/webhooks/fal/{job}/{token}"
	case strings.HasPrefix(path, "/internal/webhooks/plugin/"):
		return "plugin", "image.generate", "/internal/webhooks/plugin/{job}/{token}"
	case strings.HasPrefix(path, "/internal/webhooks/plugin-video/"):
		return "plugin", "video.generate", "/internal/webhooks/plugin-video/{job}/{token}"
	case strings.HasPrefix(path, "/v1beta/models/"):
		return "gemini", "image.generate", "/v1beta/models/{model}:generateContent"
	case path == "/health/live":
		return "gateway", "health.live", "/health/live"
	case path == "/health/ready":
		return "gateway", "health.ready", "/health/ready"
	case strings.Contains(path, "/requests/"):
		return "fal", "image.generate", "/{model}/requests/{id}"
	case strings.Count(strings.Trim(path, "/"), "/") >= 1:
		return "fal", "image.generate", "/{model}"
	default:
		return "gateway", "unknown", "unmatched"
	}
}

func statusClass(status int) string {
	if status < 100 || status > 599 {
		return "invalid"
	}
	return strconv.Itoa(status/100) + "xx"
}
func allowed(value string, values ...string) string {
	for _, candidate := range values {
		if value == candidate {
			return value
		}
	}
	return "unknown"
}
func boundedProtocol(value string) string {
	return allowed(value, "openai", "gemini", "replicate", "fal", "gateway")
}
func boundedOperation(value string) string {
	return allowed(value, "image.generate", "image.edit", "chat.completions", "audio.speech", "audio.speech.asset.get", "audio.speech.asset.content", "audio.speech.asset.delete", "audio.transcription", "audio.translation", "audio.asset.create", "audio.asset.get", "audio.asset.delete", "models.list", "jobs.list", "jobs.get", "health.live", "health.ready", "unknown")
}
func boundedProvider(value string) string {
	return allowed(value, "openai", "xai", "google", "replicate", "fal")
}
func boundedPolicy(value string) string {
	return allowed(value, "fixed", "priority", "lowest_cost", "weighted")
}
func boundedOutcome(value string) string {
	return allowed(value, "success", "failure", "neutral", "timeout", "connection", "canceled", "rate_limited", "server_error", "replay", "resolved", "retried", "manual")
}
func boundedTransition(value string) string {
	return allowed(value, "begin", "capture", "release", "reconciling", "replay")
}
func boundedStage(value string) string {
	return allowed(value, "fetch", "upload", "capture", "transform", "delete", "cleanup")
}
func boundedAuthStage(value string) string {
	return allowed(value, "authenticate", "network", "rate_limit", "model_authorization")
}
func boundedJobStage(value string) string {
	return allowed(value, "submit", "poll", "cancel", "settlement", "recovery", "webhook")
}
func boundedJobStatus(value string) string {
	return allowed(value, "PENDING", "QUEUED", "PROCESSING", "SUCCEEDED", "FAILED", "CANCELED", "RECONCILING")
}
func boundedUsageReason(value string) string {
	return allowed(value, "none", "usage_unknown", "usage_exceeds_estimate", "partial_terminal_conflict", "usage_identity_mismatch")
}
func boundedRejection(value string) string {
	return allowed(value, "none", "circuit_open", "circuit_unavailable", "price_unavailable", "price_race_unavailable", "margin", "spend_cap_exhausted", "credential_unavailable", "provider_unavailable", "executor_unavailable")
}
func boundedSource(value string) string { return allowed(value, "url", "base64", "inline") }
func boundedRoute(value string) string {
	return allowed(value, "/v1/images/generations", "/v1/images/edits", "/v1/chat/completions", "/v1/audio/speech", "/v1/audio/speech/assets/{id}", "/v1/audio/speech/assets/{id}/content", "/v1/audio/transcriptions", "/v1/audio/translations", "/v1/audio/assets", "/v1/audio/assets/{id}", "/v1/models", "/gateway/v1/jobs", "/gateway/v1/jobs/{id}", "/v1/predictions", "/v1/predictions/{id}", "/internal/webhooks/replicate/{job}/{token}", "/internal/webhooks/fal/{job}/{token}", "/internal/webhooks/plugin/{job}/{token}", "/internal/webhooks/plugin-video/{job}/{token}", "/v1beta/models/{model}:generateContent", "/health/live", "/health/ready", "unmatched")
}
