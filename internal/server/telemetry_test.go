package server

import (
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/skpjr001/fluid-config/internal/config"
	"github.com/skpjr001/fluid-config/internal/telemetry"
)

// recordSpans installs an in-memory exporter for the test's duration.
func recordSpans(t *testing.T) *tracetest.InMemoryExporter {
	exp := tracetest.NewInMemoryExporter()
	prevTracer, prevProvider := telemetry.Tracer(), otel.GetTracerProvider()
	tp := telemetry.NewTracerProvider(telemetry.Config{ServiceName: "fluid-config", SampleRatio: 1}, version, sdktrace.WithSyncer(exp))
	t.Cleanup(func() {
		tp.Shutdown(t.Context())
		telemetry.SetTracer(prevTracer)
		otel.SetTracerProvider(prevProvider)
	})
	return exp
}

func spanAttr(s tracetest.SpanStub, key string) attribute.Value {
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value
		}
	}
	return attribute.Value{}
}

func TestRequestSpansContinueTracesAndReachTheLogs(t *testing.T) {
	exp := recordSpans(t)
	spec := holidays()
	spec["cache"] = map[string]any{"enabled": true}
	ta := newTestApp(t, nil, map[string]any{"apis/holidays.json": spec})

	const traceID, parentID = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	if r := ta.call("GET", "/v1/holidays", nil, "Traceparent", "00-"+traceID+"-"+parentID+"-01"); r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	ta.call("GET", "/livez", nil) // probes are not traced
	ta.call("POST", "/v1/holidays", map[string]any{"date": "2026-08-15"}, bearer(apiToken)...)

	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("want 2 spans (no probe), got %d", len(spans))
	}
	s := spans[0]
	if s.Name != "GET v1/holidays" || s.SpanKind != trace.SpanKindServer {
		t.Fatalf("span %q kind %v", s.Name, s.SpanKind)
	}
	if s.SpanContext.TraceID().String() != traceID || s.Parent.SpanID().String() != parentID {
		t.Fatalf("the caller's trace must continue: trace %s parent %s", s.SpanContext.TraceID(), s.Parent.SpanID())
	}
	if spanAttr(s, "fluid.api").AsString() != "v1/holidays" || spanAttr(s, "http.response.status_code").AsInt64() != 200 ||
		spanAttr(s, "fluid.cache").AsString() != "miss" {
		t.Fatalf("attributes: %v", s.Attributes)
	}
	if w := spans[1]; w.Name != "POST v1/holidays" || spanAttr(w, "fluid.client").AsString() == "" ||
		w.SpanContext.TraceID().String() == traceID {
		t.Fatalf("write span: %q %v (a request without traceparent starts a new trace)", w.Name, w.Attributes)
	}

	eventually(t, "log row with the trace id", func() bool {
		r := ta.console("GET", "/api/logs?kind=api&trace_id="+traceID, viewerToken, nil)
		return strings.Contains(string(r.body), `"trace_id":"`+traceID+`"`)
	})
	if r := ta.console("GET", "/api/status", viewerToken, nil); !strings.Contains(string(r.body), `"tracing":false`) {
		t.Fatalf("no endpoint configured: tracing reported as off: %s", r.body)
	}
}

func TestCacheAndLatencyMetrics(t *testing.T) {
	spec := holidays()
	spec["cache"] = map[string]any{"enabled": true}
	ta := newTestApp(t, nil, map[string]any{"apis/holidays.json": spec})
	ta.call("GET", "/v1/holidays", nil)
	ta.call("GET", "/v1/holidays", nil)
	body := string(ta.console("GET", "/metrics", viewerToken, nil).body)
	for _, want := range []string{
		`fluid_cache_requests_total{api="v1/holidays",result="miss"} 1`,
		`fluid_cache_requests_total{api="v1/holidays",result="hit"} 1`,
		"# TYPE fluid_api_request_duration_seconds histogram",
		`fluid_api_request_duration_seconds_bucket{api="v1/holidays",le="+Inf"} 2`,
		`fluid_api_request_duration_seconds_count{api="v1/holidays"} 2`,
		`fluid_api_request_duration_seconds_sum{api="v1/holidays"} `,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}

func TestTelemetryConfig(t *testing.T) {
	c := config.Default()
	if c.Telemetry.ServiceName != "fluid-config" || c.Telemetry.SampleRatio != 1 {
		t.Fatalf("defaults: %+v", c.Telemetry)
	}
	c.Telemetry.SampleRatio = 1.5
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "sample_ratio") {
		t.Fatalf("sample_ratio 1.5: %v", err)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	if telemetry.Endpoint(telemetry.Config{}) != "http://collector:4318" || telemetry.Endpoint(telemetry.Config{OTLPEndpoint: "http://a"}) != "http://a" {
		t.Fatal("telemetry.otlp_endpoint wins over the environment")
	}
	snap := loadDir(t.TempDir(), nil)
	c = config.Default()
	found := false
	for _, n := range deploymentNotes(c, snap, hostEnv{}, false, time.Now()) {
		found = found || (n.Topic == "telemetry" && strings.Contains(n.Message, "http://collector:4318/v1/traces"))
	}
	if !found {
		t.Fatal("a deployment note names the trace endpoint")
	}
}
