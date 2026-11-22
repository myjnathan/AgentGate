// Package otel exports desensitized traces and metrics to an
// OTLP/HTTP collector (backlog F16), e.g. Jaeger, Tempo or Arize
// Phoenix. It is a thin, standard-library-only implementation of the
// OTLP/HTTP JSON protocol (POST /v1/traces and /v1/metrics,
// Content-Type: application/json): the project stays dependency-free
// (the same call the F9 OIDC round made against oauth2/oidc), and
// the wire format is a stable, published spec, so any compliant
// backend accepts the export without re-instrumentation.
//
// What is exported — and what is never exported:
//
//   - One SERVER span per model-gateway call (gateway.chat_completions)
//     and per MCP tool/prompt call (mcp.tools.call, mcp.prompts.get),
//     with duration, HTTP/RPC status and metadata attributes only:
//     model, channel, data level, decision, tier, agent type,
//     token counts, cache hit/miss. Prompt text, tool arguments,
//     model responses and DLP finding details are NEVER attributes —
//     a span can only carry the allowlisted fields set by the call
//     sites in internal/gateway and internal/mcp.
//   - Counters: gateway calls by decision, gateway tokens by channel,
//     MCP tool calls by tool and decision. Cumulative OTLP Sums.
//   - Incoming W3C traceparent headers are honoured (the call joins
//     the caller's trace) and propagated to the model upstream.
//
// Disabled by default: tracing starts only when
// OTEL_EXPORTER_OTLP_ENDPOINT is set. When disabled the tracer is a
// no-op (zero overhead, mirroring ContextForge's "zero-overhead when
// disabled"). Export failures never affect request handling — spans
// are dropped after the queue fills and the failure is counted for
// GET /api/otel.
package otel

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Scope identifies this instrumentation library in OTLP payloads.
const ScopeName = "github.com/myjnathan/agentgate/internal/otel"

// DefaultServiceName is used when OTEL_SERVICE_NAME is unset.
const DefaultServiceName = "agentgate"

// maxQueue is how many finished spans wait for export before new
// ones are dropped (and counted). Export is best-effort by design.
const maxQueue = 1000

// flushInterval is how often the background loop exports.
const flushInterval = 5 * time.Second

// Config is built from the standard OpenTelemetry environment
// variables (the same names ContextForge documents, so operators
// moving from it need no re-learning):
//
//	OTEL_EXPORTER_OTLP_ENDPOINT  base URL of the collector, e.g.
//	                             http://localhost:4318 (OTLP/HTTP).
//	                             Empty = tracing disabled.
//	OTEL_SERVICE_NAME            service.name resource attribute
//	                             (default "agentgate").
//	OTEL_EXPORTER_OTLP_PROTOCOL  only "http/json" is supported by
//	                             this exporter; "grpc" or
//	                             "http/protobuf" disables it with a
//	                             logged reason instead of failing.
type Config struct {
	Endpoint       string
	ServiceName    string
	ServiceVersion string
	Protocol       string
}

// ConfigFromEnv reads the OTel configuration from the environment.
func ConfigFromEnv(getenv func(string) string) Config {
	return Config{
		Endpoint:       strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_ENDPOINT")),
		ServiceName:    strings.TrimSpace(getenv("OTEL_SERVICE_NAME")),
		Protocol:       strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_PROTOCOL")),
		ServiceVersion: "dev",
	}
}

// Enabled reports whether tracing is configured. An endpoint that is
// not an http(s) URL, or a protocol other than http/json, disables
// the exporter — a bad observability config must never stop the
// gateway from starting.
func (c Config) Enabled() bool {
	if c.Endpoint == "" {
		return false
	}
	if c.Protocol != "" && c.Protocol != "http/json" {
		return false
	}
	u, err := url.Parse(c.Endpoint)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// Validate returns a human-readable reason when the config disables
// tracing despite an endpoint being set, so main can log it.
func (c Config) Validate() error {
	if c.Endpoint == "" {
		return fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT is not set")
	}
	if c.Protocol != "" && c.Protocol != "http/json" {
		return fmt.Errorf("OTEL_EXPORTER_OTLP_PROTOCOL=%q is not supported (only http/json)", c.Protocol)
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT=%q is not an http(s) URL", c.Endpoint)
	}
	return nil
}

// ServiceNameOrDefault returns the configured service name.
func (c Config) ServiceNameOrDefault() string {
	if c.ServiceName == "" {
		return DefaultServiceName
	}
	return c.ServiceName
}

// Attr is one span/counter attribute. Values are restricted to the
// OTLP scalar types; anything else is dropped at encoding time.
type Attr struct {
	Key   string
	Value any // string | int64 | float64 | bool
}

// StringAttr / IntAttr / BoolAttr are the call-site helpers.
func StringAttr(k, v string) Attr    { return Attr{Key: k, Value: v} }
func IntAttr(k string, v int64) Attr { return Attr{Key: k, Value: v} }
func BoolAttr(k string, v bool) Attr { return Attr{Key: k, Value: v} }

// TraceContext is the W3C traceparent identity of a call.
type TraceContext struct {
	TraceID string // 32 lowercase hex chars
	SpanID  string // 16 lowercase hex chars
}

// ParseTraceParent parses a W3C traceparent header
// ("00-<trace-id>-<span-id>-<flags>"). It reports false for any
// malformed value; callers then start a fresh trace.
func ParseTraceParent(h string) (TraceContext, bool) {
	parts := strings.Split(h, "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return TraceContext{}, false
	}
	if !isHex(parts[1]) || !isHex(parts[2]) || !isHex(parts[3]) {
		return TraceContext{}, false
	}
	if parts[1] == strings.Repeat("0", 32) || parts[2] == strings.Repeat("0", 16) {
		return TraceContext{}, false
	}
	return TraceContext{TraceID: parts[1], SpanID: parts[2]}, true
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// Format returns the W3C traceparent header for this context.
func (tc TraceContext) Format() string {
	return "00-" + tc.TraceID + "-" + tc.SpanID + "-01"
}

// Span is one finished (or in-flight) trace span.
type Span struct {
	tracer       *Tracer
	Name         string
	TraceID      string
	SpanID       string
	ParentSpanID string
	Kind         int // OTLP kind: 2 = SERVER
	Start        time.Time
	EndTime      time.Time
	Attrs        []Attr
	StatusCode   int // 0 unset, 1 OK, 2 ERROR
	StatusMsg    string
	ended        bool
	noop         bool
}

// SetAttrs appends attributes to an in-flight span.
func (s *Span) SetAttrs(attrs ...Attr) {
	if s == nil || s.noop {
		return
	}
	s.Attrs = append(s.Attrs, attrs...)
}

// End finishes the span and hands it to the exporter. statusCode is
// the OTLP status (1 OK, 2 ERROR); statusMsg is a short category
// code such as "data_level_forbidden", never request content.
func (s *Span) End(statusCode int, statusMsg string) {
	if s == nil || s.noop || s.ended {
		return
	}
	s.ended = true
	s.EndTime = time.Now()
	s.StatusCode = statusCode
	s.StatusMsg = statusMsg
	s.tracer.enqueue(s)
}

// TraceParent returns the W3C traceparent header for this span, for
// propagation to the upstream. Empty for a no-op span.
func (s *Span) TraceParent() string {
	if s == nil || s.noop {
		return ""
	}
	return TraceContext{TraceID: s.TraceID, SpanID: s.SpanID}.Format()
}

// Stats is the exporter's observable state, served by GET /api/otel.
type Stats struct {
	Enabled       bool
	Endpoint      string
	ServiceName   string
	SpansExported int64
	SpansDropped  int64
	ExportErrors  int64
	LastError     string
	LastErrorAt   time.Time
	QueuedSpans   int
	Counters      map[string]int64
}

type counterKey struct {
	name  string
	attrs string // sorted "k=v,k=v"
}

// Tracer collects spans and counters and exports them as OTLP/HTTP
// JSON. A nil *Tracer is safe to use: every method no-ops, so call
// sites never branch on "is tracing on".
type Tracer struct {
	cfg       Config
	client    *http.Client
	startedAt time.Time

	mu        sync.Mutex
	queue     []*Span
	dropped   int64
	exported  int64
	errs      int64
	lastErr   string
	lastErrAt time.Time
	counters  map[counterKey]int64

	exportMu sync.Mutex
	done     chan struct{}
	flushReq chan struct{}
	once     sync.Once
}

// New builds a tracer; when cfg is disabled it returns a tracer
// whose methods no-op (and Enabled() is false). Start the background
// flush loop with t.Run().
func New(cfg Config) *Tracer {
	return &Tracer{
		cfg:       cfg,
		client:    &http.Client{Timeout: 5 * time.Second},
		startedAt: time.Now(),
		counters:  map[counterKey]int64{},
		done:      make(chan struct{}),
		flushReq:  make(chan struct{}, 1),
	}
}

// Enabled reports whether this tracer exports anything.
func (t *Tracer) Enabled() bool {
	return t != nil && t.cfg.Enabled()
}

// Run launches the background export loop. Safe to call on a
// disabled tracer (it just waits for Shutdown).
func (t *Tracer) Run() {
	if t == nil {
		return
	}
	go t.loop()
}

func (t *Tracer) loop() {
	tick := time.NewTicker(flushInterval)
	defer tick.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-t.flushReq:
			t.export()
		case <-tick.C:
			t.export()
		}
	}
}

// Shutdown stops the background loop and makes a final export
// attempt. Safe on a nil or disabled tracer.
func (t *Tracer) Shutdown() {
	if t == nil {
		return
	}
	t.once.Do(func() { close(t.done) })
	// Best-effort final flush; a stuck collector must not block
	// shutdown (the client has its own 5s timeout).
	t.export()
}

// Start begins a span. parent joins an incoming trace when valid;
// otherwise a fresh trace ID is generated. On a nil or disabled
// tracer it returns a no-op span.
func (t *Tracer) Start(name string, parent TraceContext, attrs ...Attr) *Span {
	if !t.Enabled() {
		return &Span{noop: true}
	}
	traceID := parent.TraceID
	if traceID == "" {
		traceID = newID(16)
	}
	return &Span{
		tracer:       t,
		Name:         name,
		TraceID:      traceID,
		SpanID:       newID(8),
		ParentSpanID: parent.SpanID,
		Kind:         2, // SERVER
		Start:        time.Now(),
		Attrs:        append([]Attr{}, attrs...),
	}
}

// AddCounter adds value to a cumulative counter. Counters are
// exported as OTLP Sums on the same flush tick as spans. attrs keys
// are sorted at export time, so call order does not matter.
func (t *Tracer) AddCounter(name string, value int64, attrs map[string]string) {
	if !t.Enabled() || value == 0 {
		return
	}
	var parts []string
	for k, v := range attrs {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	t.mu.Lock()
	t.counters[counterKey{name: name, attrs: strings.Join(parts, ",")}] += value
	t.mu.Unlock()
}

// Flush exports queued spans and counters synchronously. It is used
// by POST /api/otel/test and by tests; the background loop calls
// export on its own schedule.
func (t *Tracer) Flush() {
	if t == nil {
		return
	}
	t.export()
}

func (t *Tracer) enqueue(s *Span) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.queue) >= maxQueue {
		t.dropped++
		return
	}
	t.queue = append(t.queue, s)
	// Wake the background loop when the queue grows; the flush
	// trigger is lossy (buffer 1) so a burst does not spam it.
	if len(t.queue) >= 100 {
		select {
		case t.flushReq <- struct{}{}:
		default:
		}
	}
}

// Stats snapshots the exporter's state.
func (t *Tracer) Stats() Stats {
	if t == nil {
		return Stats{ServiceName: DefaultServiceName}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := Stats{
		Enabled:       t.cfg.Enabled(),
		Endpoint:      t.cfg.Endpoint,
		ServiceName:   t.cfg.ServiceNameOrDefault(),
		SpansExported: t.exported,
		SpansDropped:  t.dropped,
		ExportErrors:  t.errs,
		LastError:     t.lastErr,
		LastErrorAt:   t.lastErrAt,
		QueuedSpans:   len(t.queue),
		Counters:      map[string]int64{},
	}
	for k, v := range t.counters {
		key := k.name
		if k.attrs != "" {
			key += "{" + k.attrs + "}"
		}
		st.Counters[key] = v
	}
	return st
}

// export drains the queue and counters and POSTs them. It never
// returns an error to callers — failures are counted and remembered
// in Stats. Spans that fail to export are dropped, not re-queued:
// telemetry must never grow unbounded or block request handling.
func (t *Tracer) export() {
	if t == nil || !t.cfg.Enabled() {
		return
	}
	t.exportMu.Lock()
	defer t.exportMu.Unlock()

	t.mu.Lock()
	spans := t.queue
	t.queue = nil
	// Counters are cumulative: snapshot them, never drain — a
	// failed metrics export must not lose counts, the next flush
	// re-reports the same (larger) totals.
	counters := make(map[counterKey]int64, len(t.counters))
	for k, v := range t.counters {
		counters[k] = v
	}
	t.mu.Unlock()

	if len(spans) > 0 {
		if err := t.postJSON("/v1/traces", buildTracesPayload(t.cfg, spans)); err != nil {
			t.recordErr("traces: " + err.Error())
		} else {
			t.mu.Lock()
			t.exported += int64(len(spans))
			t.mu.Unlock()
		}
	}
	if len(counters) > 0 {
		if err := t.postJSON("/v1/metrics", buildMetricsPayload(t.cfg, t.startedAt, counters)); err != nil {
			t.recordErr("metrics: " + err.Error())
		}
	}
}

func (t *Tracer) recordErr(msg string) {
	t.mu.Lock()
	t.errs++
	t.lastErr = msg
	t.lastErrAt = time.Now()
	t.mu.Unlock()
}

func (t *Tracer) postJSON(path string, body []byte) error {
	endpoint := strings.TrimSuffix(t.cfg.Endpoint, "/") + path
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("collector returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func newID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Fall back to time-based bytes; IDs only need to be unique
		// per trace, not secret.
		for i := range b {
			b[i] = byte(time.Now().UnixNano() >> (uint(i%8) * 8))
		}
	}
	return hex.EncodeToString(b)
}

// ---- OTLP/JSON encoding (opentelemetry-proto, JSON mapping) ----

func encodeAttrValue(v any) map[string]any {
	switch x := v.(type) {
	case string:
		return map[string]any{"stringValue": x}
	case int:
		return map[string]any{"intValue": strconv.FormatInt(int64(x), 10)}
	case int64:
		return map[string]any{"intValue": strconv.FormatInt(x, 10)}
	case float64:
		return map[string]any{"doubleValue": x}
	case bool:
		return map[string]any{"boolValue": x}
	default:
		return nil
	}
}

func encodeAttrs(attrs []Attr) []any {
	out := []any{}
	for _, a := range attrs {
		v := encodeAttrValue(a.Value)
		if v == nil {
			continue
		}
		out = append(out, map[string]any{"key": a.Key, "value": v})
	}
	return out
}

func resourceAttrs(cfg Config) []any {
	return encodeAttrs([]Attr{
		StringAttr("service.name", cfg.ServiceNameOrDefault()),
		StringAttr("service.version", cfg.ServiceVersion),
	})
}

func buildTracesPayload(cfg Config, spans []*Span) []byte {
	outSpans := make([]any, 0, len(spans))
	for _, s := range spans {
		m := map[string]any{
			"traceId":           s.TraceID,
			"spanId":            s.SpanID,
			"name":              s.Name,
			"kind":              s.Kind,
			"startTimeUnixNano": strconv.FormatInt(s.Start.UnixNano(), 10),
			"endTimeUnixNano":   strconv.FormatInt(s.EndTime.UnixNano(), 10),
			"attributes":        encodeAttrs(s.Attrs),
			"status":            map[string]any{"code": s.StatusCode},
		}
		if s.ParentSpanID != "" {
			m["parentSpanId"] = s.ParentSpanID
		}
		if s.StatusMsg != "" {
			m["status"].(map[string]any)["message"] = s.StatusMsg
		}
		outSpans = append(outSpans, m)
	}
	payload, _ := json.Marshal(map[string]any{
		"resourceSpans": []any{map[string]any{
			"resource":   map[string]any{"attributes": resourceAttrs(cfg)},
			"scopeSpans": []any{map[string]any{"scope": map[string]any{"name": ScopeName, "version": cfg.ServiceVersion}, "spans": outSpans}},
			"schemaUrl":  "https://opentelemetry.io/schemas/1.24.0",
		}},
	})
	return payload
}

func buildMetricsPayload(cfg Config, startedAt time.Time, counters map[counterKey]int64) []byte {
	// Group data points by metric name; one Sum per name.
	byName := map[string][]counterKey{}
	for k := range counters {
		byName[k.name] = append(byName[k.name], k)
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	now := time.Now()
	metrics := make([]any, 0, len(names))
	for _, name := range names {
		keys := byName[name]
		sort.Slice(keys, func(i, j int) bool { return keys[i].attrs < keys[j].attrs })
		points := make([]any, 0, len(keys))
		for _, k := range keys {
			var attrs []Attr
			if k.attrs != "" {
				for _, kv := range strings.Split(k.attrs, ",") {
					if i := strings.Index(kv, "="); i > 0 {
						attrs = append(attrs, StringAttr(kv[:i], kv[i+1:]))
					}
				}
			}
			points = append(points, map[string]any{
				"attributes":        encodeAttrs(attrs),
				"startTimeUnixNano": strconv.FormatInt(startedAt.UnixNano(), 10),
				"timeUnixNano":      strconv.FormatInt(now.UnixNano(), 10),
				"asInt":             strconv.FormatInt(counters[k], 10),
			})
		}
		metrics = append(metrics, map[string]any{
			"name": name,
			"sum": map[string]any{
				"dataPoints":             points,
				"aggregationTemporality": 2, // CUMULATIVE
				"isMonotonic":            true,
			},
		})
	}
	payload, _ := json.Marshal(map[string]any{
		"resourceMetrics": []any{map[string]any{
			"resource":     map[string]any{"attributes": resourceAttrs(cfg)},
			"scopeMetrics": []any{map[string]any{"scope": map[string]any{"name": ScopeName, "version": cfg.ServiceVersion}, "metrics": metrics}},
			"schemaUrl":    "https://opentelemetry.io/schemas/1.24.0",
		}},
	})
	return payload
}
