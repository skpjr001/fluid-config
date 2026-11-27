// Package telemetry sets up OpenTelemetry tracing and W3C trace propagation.
package telemetry

import (
	"context"
	"net/http"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Config is the telemetry block of config.json.
type Config struct {
	OTLPEndpoint string            `json:"otlp_endpoint"` // e.g. "http://otel-collector:4318"; empty = tracing off
	ServiceName  string            `json:"service_name"`
	SampleRatio  float64           `json:"sample_ratio"`
	Headers      map[string]string `json:"headers,omitempty"`
}

var (
	tracer     trace.Tracer = otel.Tracer("fluid-config")
	propagator              = propagation.TraceContext{}
)

// Endpoint is telemetry.otlp_endpoint, or OTEL_EXPORTER_OTLP_ENDPOINT.
func Endpoint(cfg Config) string {
	if cfg.OTLPEndpoint != "" {
		return cfg.OTLPEndpoint
	}
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
}

// Setup enables OTLP/HTTP trace export when an endpoint is set. It
// returns a shutdown func that flushes the spans still queued.
func Setup(ctx context.Context, cfg Config, version string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagator)
	endpoint := Endpoint(cfg)
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(endpoint + "/v1/traces")}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlptracehttp.WithHeaders(cfg.Headers))
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, err
	}
	tp := NewTracerProvider(cfg, version, sdktrace.WithBatcher(exp))
	return tp.Shutdown, nil
}

// NewTracerProvider installs a tracer provider (and the tracer) built from
// cfg; tests pass an in-memory exporter.
func NewTracerProvider(cfg Config, version string, opts ...sdktrace.TracerProviderOption) *sdktrace.TracerProvider {
	res := resource.NewSchemaless(
		attribute.String("service.name", cfg.ServiceName),
		attribute.String("service.version", version),
	)
	opts = append(opts, sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))))
	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	tracer = tp.Tracer("fluid-config")
	return tp
}

// Tracer returns the tracer for the service's spans.
func Tracer() trace.Tracer { return tracer }

// SetTracer replaces the tracer (tests restore the previous one).
func SetTracer(t trace.Tracer) { tracer = t }

// TraceID returns the trace id in ctx, or "".
func TraceID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

// Extract continues a trace started by the caller (traceparent header).
func Extract(ctx context.Context, h http.Header) context.Context {
	return propagator.Extract(ctx, propagation.HeaderCarrier(h))
}

// EndSpan records the status and error on span and ends it.
func EndSpan(span trace.Span, status int, err error) {
	if status > 0 {
		span.SetAttributes(attribute.Int("http.response.status_code", status))
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else if status >= 500 {
		span.SetStatus(codes.Error, http.StatusText(status))
	}
	span.End()
}
