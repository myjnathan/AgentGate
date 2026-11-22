package gateway_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/myjnathan/agentgate/internal/audit"
)

// TestSidecarHeaderRecordedInAudit: a request that arrived through
// the endpoint sidecar (F15) carries X-AgentGate-Sidecar; the
// gateway records it on the model_call audit event so operators can
// see endpoint coverage (and GET /api/sidecar/stats can count it).
func TestSidecarHeaderRecordedInAudit(t *testing.T) {
	e := setup(t)
	body, _ := json.Marshal(map[string]any{
		"model": "mock-eco", "messages": []map[string]string{{"role": "user", "content": "帮我写请假条"}},
	})
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.byoaTok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-AgentGate-Sidecar", "ag-guard/0.1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	ev, ok := lastModelCall(e.al.All())
	if !ok {
		t.Fatal("no model_call audit event")
	}
	if ev.Detail["sidecar"] != "ag-guard/0.1.0" {
		t.Fatalf("want sidecar detail ag-guard/0.1.0, got %+v", ev.Detail)
	}
}

// TestSidecarHeaderOnCacheHit: the same signal is recorded on
// cache_hit events, so sidecar-routed cached answers count too.
func TestSidecarHeaderOnCacheHit(t *testing.T) {
	e := setup(t)
	do := func() int {
		body, _ := json.Marshal(map[string]any{
			"model": "mock-eco", "messages": []map[string]string{{"role": "user", "content": "缓存侧车测试"}},
		})
		req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+e.byoaTok)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-AgentGate-Sidecar", "ag-guard/0.1.0")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := do(); code != 200 {
		t.Fatalf("first call want 200, got %d", code)
	}
	if code := do(); code != 200 {
		t.Fatalf("second call want 200, got %d", code)
	}
	var hit *audit.Event
	for _, ev := range e.al.All() {
		if ev.Kind == "cache_hit" {
			cp := ev
			hit = &cp
		}
	}
	if hit == nil {
		t.Fatal("no cache_hit audit event")
	}
	if hit.Detail["sidecar"] != "ag-guard/0.1.0" {
		t.Fatalf("want sidecar detail on cache_hit, got %+v", hit.Detail)
	}
}

// TestNoSidecarHeaderNoDetail: a direct request carries no sidecar
// marker in audit.
func TestNoSidecarHeaderNoDetail(t *testing.T) {
	e := setup(t)
	if code, _ := e.chat(t, e.byoaTok, "mock-eco", "帮我写请假条"); code != 200 {
		t.Fatalf("want 200, got %d", code)
	}
	ev, ok := lastModelCall(e.al.All())
	if !ok {
		t.Fatal("no model_call audit event")
	}
	if _, present := ev.Detail["sidecar"]; present {
		t.Fatalf("direct request must not carry a sidecar detail, got %+v", ev.Detail)
	}
}
