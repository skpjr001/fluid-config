package main

import (
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// recordSpans installs an in-memory exporter for the test's duration.
func recordSpans(t *testing.T) *tracetest.InMemoryExporter {
	exp := tracetest.NewInMemoryExporter()
	prevTracer, prevProvider := tracer, otel.GetTracerProvider()
	tp := newTracerProvider(TelemetryConfig{ServiceName: "fluid-config", SampleRatio: 1}, sdktrace.WithSyncer(exp))
	t.Cleanup(func() {
		tp.Shutdown(t.Context())
		tracer = prevTracer
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

func TestHistogramBucketsAreCumulative(t *testing.T) {
	m := NewMetrics()
	for _, v := range []float64{0.0005, 0.003, 0.003, 9} {
		m.Observe("h", v, "api", "a")
	}
	var b strings.Builder
	m.WritePrometheus(&b)
	for _, want := range []string{`h_bucket{api="a",le="0.001"} 1`, `h_bucket{api="a",le="0.005"} 3`,
		`h_bucket{api="a",le="5"} 3`, `h_bucket{api="a",le="+Inf"} 4`, `h_count{api="a"} 4`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("lack %s in\n%s", want, b.String())
		}
	}
}

func TestTelemetryConfig(t *testing.T) {
	c := defaultStatic()
	if c.Telemetry.ServiceName != "fluid-config" || c.Telemetry.SampleRatio != 1 {
		t.Fatalf("defaults: %+v", c.Telemetry)
	}
	c.Telemetry.SampleRatio = 1.5
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "sample_ratio") {
		t.Fatalf("sample_ratio 1.5: %v", err)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	if otlpEndpoint(TelemetryConfig{}) != "http://collector:4318" || otlpEndpoint(TelemetryConfig{OTLPEndpoint: "http://a"}) != "http://a" {
		t.Fatal("telemetry.otlp_endpoint wins over the environment")
	}
	snap := loadDir(t.TempDir(), nil)
	c = defaultStatic()
	found := false
	for _, n := range deploymentNotes(c, snap, hostEnv{}, false, time.Now()) {
		found = found || (n.Topic == "telemetry" && strings.Contains(n.Message, "http://collector:4318/v1/traces"))
	}
	if !found {
		t.Fatal("a deployment note names the trace endpoint")
	}
}
