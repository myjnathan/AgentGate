package otel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func enabledCfg(srv *httptest.Server) Config {
	return Config{Endpoint: srv.URL, ServiceName: "agentgate-test", ServiceVersion: "test"}
}

func TestConfigFromEnv(t *testing.T) {
	get := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	c := ConfigFromEnv(get(map[string]string{}))
	if c.Enabled() {
		t.Error("empty endpoint must be disabled")
	}
	c = ConfigFromEnv(get(map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318",
		"OTEL_SERVICE_NAME":           "my-gateway",
	}))
	if !c.Enabled() || c.ServiceNameOrDefault() != "my-gateway" {
		t.Errorf("expected enabled with service name, got %+v", c)
	}
	c = ConfigFromEnv(get(map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318",
		"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc",
	}))
	if c.Enabled() {
		t.Error("grpc protocol must disable this http/json exporter")
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "grpc") {
		t.Errorf("Validate should explain grpc rejection, got %v", err)
	}
	c = ConfigFromEnv(get(map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "localhost:4318",
	}))
	if c.Enabled() {
		t.Error("endpoint without scheme must be disabled")
	}
}

func TestParseTraceParent(t *testing.T) {
	tc, ok := ParseTraceParent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	if !ok || tc.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || tc.SpanID != "00f067aa0ba902b7" {
		t.Errorf("valid traceparent rejected: %+v %v", tc, ok)
	}
	for _, bad := range []string{
		"", "00-xyz-00f067aa0ba902b7-01",
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
		"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7",
	} {
		if _, ok := ParseTraceParent(bad); ok {
			t.Errorf("malformed traceparent accepted: %q", bad)
		}
	}
}

func TestNilTracerNoop(t *testing.T) {
	var tr *Tracer
	if tr.Enabled() {
		t.Error("nil tracer must report disabled")
	}
	s := tr.Start("x", TraceContext{})
	s.SetAttrs(StringAttr("k", "v"))
	s.End(1, "")
	if s.TraceParent() != "" {
		t.Error("nil tracer span must have no traceparent")
	}
	tr.AddCounter("c", 1, nil)
	tr.Flush()
	tr.Shutdown()
	if st := tr.Stats(); st.Enabled {
		t.Error("nil tracer stats must report disabled")
	}
}

func TestDisabledTracerNoop(t *testing.T) {
	tr := New(Config{})
	if tr.Enabled() {
		t.Error("empty config must be disabled")
	}
	s := tr.Start("x", TraceContext{})
	s.End(1, "")
	tr.Flush()
	if st := tr.Stats(); st.SpansExported != 0 || st.QueuedSpans != 0 {
		t.Errorf("disabled tracer must not queue: %+v", st)
	}
}

// collector captures OTLP/HTTP JSON payloads for assertions.
type collector struct {
	mu      sync.Mutex
	traces  []map[string]any
	metrics []map[string]any
	status  int
}

func (c *collector) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	c.mu.Lock()
	defer c.mu.Unlock()
	switch r.URL.Path {
	case "/v1/traces":
		c.traces = append(c.traces, payload)
	case "/v1/metrics":
		c.metrics = append(c.metrics, payload)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.WriteHeader(c.status)
}

func (c *collector) counts() (traces, metrics int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.traces), len(c.metrics)
}

func TestExportTracesPayload(t *testing.T) {
	col := &collector{status: 200}
	srv := httptest.NewServer(http.HandlerFunc(col.handler))
	defer srv.Close()

	tr := New(enabledCfg(srv))
	parent, _ := ParseTraceParent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	s := tr.Start("gateway.chat_completions", parent,
		StringAttr("agentgate.model", "mock-eco"),
		StringAttr("agentgate.data_level", "internal"),
		StringAttr("agentgate.decision", "allow"),
		IntAttr("agentgate.tokens_total", 42))
	if s.TraceID != parent.TraceID || s.ParentSpanID != parent.SpanID {
		t.Errorf("span must join incoming trace: %+v", s)
	}
	if len(s.TraceID) != 32 || len(s.SpanID) != 16 {
		t.Errorf("bad ids: %q %q", s.TraceID, s.SpanID)
	}
	s.End(1, "")
	tr.AddCounter("agentgate.gateway.calls", 1, map[string]string{"decision": "allow"})
	tr.Flush()

	nT, nM := col.counts()
	if nT != 1 || nM != 1 {
		t.Fatalf("expected 1 traces + 1 metrics export, got %d/%d", nT, nM)
	}
	// Snapshot the payloads, then release the lock before any
	// further Flush/counts call: the collector lock is not
	// reentrant and the exporter needs it too.
	col.mu.Lock()
	tracePayload := col.traces[0]
	metricPayload := col.metrics[0]
	col.mu.Unlock()
	payload := tracePayload
	rs, ok := payload["resourceSpans"].([]any)
	if !ok || len(rs) != 1 {
		t.Fatalf("resourceSpans missing: %v", payload)
	}
	res := rs[0].(map[string]any)
	attrs := res["resource"].(map[string]any)["attributes"].([]any)
	var svcName string
	for _, a := range attrs {
		kv := a.(map[string]any)
		if kv["key"] == "service.name" {
			svcName = kv["value"].(map[string]any)["stringValue"].(string)
		}
	}
	if svcName != "agentgate-test" {
		t.Errorf("service.name = %q", svcName)
	}
	spans := res["scopeSpans"].([]any)[0].(map[string]any)["spans"].([]any)
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	sp := spans[0].(map[string]any)
	if sp["traceId"] != "4bf92f3577b34da6a3ce929d0e0e4736" || sp["parentSpanId"] != "00f067aa0ba902b7" {
		t.Errorf("trace/parent ids wrong: %v", sp)
	}
	if sp["kind"] != float64(2) {
		t.Errorf("kind must be 2 (SERVER), got %v", sp["kind"])
	}
	if sp["status"].(map[string]any)["code"] != float64(1) {
		t.Errorf("status must be OK: %v", sp["status"])
	}
	startNano := sp["startTimeUnixNano"].(string)
	endNano := sp["endTimeUnixNano"].(string)
	if len(startNano) < 18 || len(endNano) < 18 || startNano >= endNano {
		t.Errorf("timestamps must be nanosecond strings with start<end: %q %q", startNano, endNano)
	}
	found := map[string]any{}
	for _, a := range sp["attributes"].([]any) {
		kv := a.(map[string]any)
		found[kv["key"].(string)] = kv["value"]
	}
	if found["agentgate.model"].(map[string]any)["stringValue"] != "mock-eco" {
		t.Errorf("model attr wrong: %v", found["agentgate.model"])
	}
	if found["agentgate.tokens_total"].(map[string]any)["intValue"] != "42" {
		t.Errorf("tokens attr must be intValue string: %v", found["agentgate.tokens_total"])
	}
	// Metrics payload: one cumulative Sum with the counter.
	mp := metricPayload
	rm := mp["resourceMetrics"].([]any)[0].(map[string]any)
	ms := rm["scopeMetrics"].([]any)[0].(map[string]any)["metrics"].([]any)
	if len(ms) != 1 || ms[0].(map[string]any)["name"] != "agentgate.gateway.calls" {
		t.Fatalf("metrics wrong: %v", ms)
	}
	sum := ms[0].(map[string]any)["sum"].(map[string]any)
	if sum["aggregationTemporality"] != float64(2) || sum["isMonotonic"] != true {
		t.Errorf("sum must be cumulative+monotonic: %v", sum)
	}
	pt := sum["dataPoints"].([]any)[0].(map[string]any)
	if pt["asInt"] != "1" {
		t.Errorf("counter value must be 1, got %v", pt["asInt"])
	}
	// Exported spans are counted; a second flush exports nothing.
	st := tr.Stats()
	if st.SpansExported != 1 || st.SpansDropped != 0 || st.ExportErrors != 0 {
		t.Errorf("stats wrong: %+v", st)
	}
	tr.Flush()
	if nT2, _ := col.counts(); nT2 != 1 {
		t.Errorf("second flush must not re-export: %d", nT2)
	}
}

func TestExportFailureCountedNotFatal(t *testing.T) {
	col := &collector{status: 500}
	srv := httptest.NewServer(http.HandlerFunc(col.handler))
	defer srv.Close()
	tr := New(enabledCfg(srv))
	s := tr.Start("x", TraceContext{})
	s.End(2, "boom")
	tr.Flush()
	st := tr.Stats()
	if st.ExportErrors == 0 || st.LastError == "" {
		t.Errorf("failed export must be counted: %+v", st)
	}
	if st.SpansExported != 0 {
		t.Errorf("failed export must not count as exported: %+v", st)
	}
}

func TestQueueDropCounted(t *testing.T) {
	col := &collector{status: 200}
	srv := httptest.NewServer(http.HandlerFunc(col.handler))
	defer srv.Close()
	tr := New(enabledCfg(srv))
	for i := 0; i < maxQueue+10; i++ {
		s := tr.Start("x", TraceContext{})
		s.End(1, "")
	}
	st := tr.Stats()
	if st.SpansDropped != 10 || st.QueuedSpans != maxQueue {
		t.Errorf("drop accounting wrong: %+v", st)
	}
}

func TestSpanWithoutParentGetsFreshTrace(t *testing.T) {
	tr := New(Config{Endpoint: "http://localhost:4318"})
	a := tr.Start("a", TraceContext{})
	b := tr.Start("b", TraceContext{})
	if a.TraceID == b.TraceID {
		t.Error("two root spans must have different trace IDs")
	}
	if a.ParentSpanID != "" {
		t.Error("root span must have no parent")
	}
	if a.TraceParent() == "" || !strings.HasPrefix(a.TraceParent(), "00-"+a.TraceID+"-"+a.SpanID+"-") {
		t.Errorf("traceparent malformed: %q", a.TraceParent())
	}
}
