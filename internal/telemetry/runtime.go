package telemetry

import (
	"context"
	"errors"
	"net/url"
	"path"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type Runtime struct {
	config     Config
	traces     *sdktrace.TracerProvider
	metrics    *metric.MeterProvider
	Recorder   *Recorder
	Propagator propagation.TextMapPropagator
}

func New(ctx context.Context, config Config) (*Runtime, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if config.Mode == Disabled {
		recorder, _ := NewRecorder(trace.NewNoopTracerProvider(), metricnoop.NewMeterProvider())
		return &Runtime{config: config, Recorder: recorder, Propagator: propagation.TraceContext{}}, nil
	}
	resourceValue, err := resource.New(ctx, resource.WithAttributes(
		attribute.String("service.name", config.ServiceName),
		attribute.String("service.version", config.ServiceVersion),
		attribute.String("deployment.environment.name", config.Environment),
	))
	if err != nil {
		return nil, ErrInvalidConfig
	}
	headers := map[string]string{}
	if config.Authorization != "" {
		headers["Authorization"] = config.Authorization
	}
	traceExporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(signalURL(config.Endpoint, "traces")), otlptracehttp.WithHeaders(headers), otlptracehttp.WithTimeout(config.ExportTimeout))
	if err != nil {
		return nil, errors.New("telemetry trace exporter initialization failed")
	}
	traceProvider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(resourceValue),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(config.SampleRatio))),
		sdktrace.WithBatcher(traceExporter),
	)
	metricExporter, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(signalURL(config.Endpoint, "metrics")), otlpmetrichttp.WithHeaders(headers), otlpmetrichttp.WithTimeout(config.ExportTimeout))
	if err != nil {
		_ = traceProvider.Shutdown(ctx)
		return nil, errors.New("telemetry metric exporter initialization failed")
	}
	reader := metric.NewPeriodicReader(metricExporter, metric.WithInterval(config.ExportInterval), metric.WithTimeout(config.ExportTimeout))
	meterProvider := metric.NewMeterProvider(metric.WithResource(resourceValue), metric.WithReader(reader))
	recorder, err := NewRecorder(traceProvider, meterProvider)
	if err != nil {
		_ = meterProvider.Shutdown(ctx)
		_ = traceProvider.Shutdown(ctx)
		return nil, errors.New("telemetry instrument initialization failed")
	}
	return &Runtime{config: config, traces: traceProvider, metrics: meterProvider, Recorder: recorder, Propagator: propagation.TraceContext{}}, nil
}

func signalURL(base, signal string) string {
	parsed, _ := url.Parse(base)
	parsed.Path = path.Join(parsed.Path, "v1", signal)
	return parsed.String()
}

func (runtime *Runtime) Shutdown(ctx context.Context) error {
	if runtime == nil || runtime.config.Mode == Disabled {
		return nil
	}
	timeout := runtime.config.ShutdownTimeout
	shutdownContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	var result error
	if runtime.metrics != nil {
		result = errors.Join(result, runtime.metrics.Shutdown(shutdownContext))
	}
	if runtime.traces != nil {
		result = errors.Join(result, runtime.traces.Shutdown(shutdownContext))
	}
	return result
}
