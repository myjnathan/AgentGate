package api

import (
	"net/http"
	"time"

	"github.com/myjnathan/agentgate/internal/otel"
	"github.com/myjnathan/agentgate/internal/store"
)

// GET /api/otel — OpenTelemetry export status (F16).
//
// Reports whether tracing is enabled (OTEL_EXPORTER_OTLP_ENDPOINT),
// the collector endpoint and service name, and the exporter's
// counters (spans exported/dropped, export errors, last error,
// queued spans, cumulative counters). The endpoint is a collector
// address, not a credential, so it is shown as configured.
type otelStatusResponse struct {
	Enabled       bool             `json:"enabled"`
	Endpoint      string           `json:"endpoint,omitempty"`
	ServiceName   string           `json:"service_name"`
	SpansExported int64            `json:"spans_exported"`
	SpansDropped  int64            `json:"spans_dropped"`
	ExportErrors  int64            `json:"export_errors"`
	LastError     string           `json:"last_error,omitempty"`
	LastErrorAt   *time.Time       `json:"last_error_at,omitempty"`
	QueuedSpans   int              `json:"queued_spans"`
	Counters      map[string]int64 `json:"counters"`
}

func (a *API) otelStatus(w http.ResponseWriter, r *http.Request, u *store.User) {
	st := a.tracer.Stats()
	resp := otelStatusResponse{
		Enabled:       st.Enabled,
		Endpoint:      st.Endpoint,
		ServiceName:   st.ServiceName,
		SpansExported: st.SpansExported,
		SpansDropped:  st.SpansDropped,
		ExportErrors:  st.ExportErrors,
		LastError:     st.LastError,
		QueuedSpans:   st.QueuedSpans,
		Counters:      st.Counters,
	}
	if !st.LastErrorAt.IsZero() {
		t := st.LastErrorAt
		resp.LastErrorAt = &t
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /api/otel/test — connectivity check (F16).
//
// Emits one synthetic span (agentgate.test, no request data) and
// flushes synchronously, so the operator sees immediately whether
// the collector accepts OTLP/HTTP JSON. Disabled tracers answer
// 503; a failed export answers 502 with only the error class (no
// collector internals leak to the console).
func (a *API) otelTest(w http.ResponseWriter, r *http.Request, u *store.User) {
	if a.tracer == nil || !a.tracer.Enabled() {
		writeErr(w, http.StatusServiceUnavailable, "otel_disabled")
		return
	}
	span := a.tracer.Start("agentgate.test", otel.TraceContext{},
		otel.StringAttr("agentgate.test", "true"))
	span.End(1, "")
	a.tracer.Flush()
	st := a.tracer.Stats()
	if st.ExportErrors > 0 && st.SpansExported == 0 {
		writeErr(w, http.StatusBadGateway, "otel_export_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"spans_exported": st.SpansExported,
		"export_errors":  st.ExportErrors,
	})
}
