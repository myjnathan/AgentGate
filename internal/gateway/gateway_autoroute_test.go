package gateway_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/myjnathan/agentgate/internal/autorouter"
	"github.com/myjnathan/agentgate/internal/store"
)

// Smart routing (backlog F30): model=auto resolves to a concrete
// model from the simple (mock-eco) or complex (mock-pro) group.
// The mock upstream echoes the model it received, so the response
// body proves which model the gateway chose.

func (e *testEnv) chatAuto(t *testing.T, token, content string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":    "auto",
		"messages": []map[string]string{{"role": "user", "content": content}},
	})
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(buf.Bytes(), &out)
	return resp.StatusCode, out
}

func servedModel(t *testing.T, body map[string]any) string {
	t.Helper()
	m, _ := body["model"].(string)
	if m == "" {
		t.Fatalf("response has no model field: %v", body)
	}
	return m
}

func setAutoRoute(t *testing.T, e *testEnv, cfg *store.AutoRouteConfig) {
	t.Helper()
	e.st.WithLock(func(st *store.State) { st.AutoRoute = cfg })
	if err := e.st.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestAutoRouteSimpleQuestionToEco(t *testing.T) {
	e := setup(t)
	code, body := e.chatAuto(t, e.byoaTok, "今天是星期几？", nil)
	if code != 200 {
		t.Fatalf("auto simple: got %d, want 200 (%v)", code, body)
	}
	if got := servedModel(t, body); got != "mock-eco" {
		t.Errorf("auto simple: served %s, want mock-eco", got)
	}
}

func TestAutoRouteComplexQuestionToPro(t *testing.T) {
	e := setup(t)
	code, body := e.chatAuto(t, e.byoaTok, "分析一下上季度供应商报价单的成本异常，给三条降本建议", nil)
	if code != 200 {
		t.Fatalf("auto complex: got %d, want 200 (%v)", code, body)
	}
	if got := servedModel(t, body); got != "mock-pro" {
		t.Errorf("auto complex: served %s, want mock-pro", got)
	}
}

func TestAutoRouteRuleFallback(t *testing.T) {
	e := setup(t)
	// No sample matches; no rule signal either: simple by default.
	code, body := e.chatAuto(t, e.byoaTok, "帮我写请假条", nil)
	if code != 200 {
		t.Fatalf("auto rule fallback: got %d, want 200 (%v)", code, body)
	}
	if got := servedModel(t, body); got != "mock-eco" {
		t.Errorf("auto rule fallback: served %s, want mock-eco", got)
	}
	// Long text is a rule signal for complex.
	long := strings.Repeat("这是一段关于日常工作的普通描述。", 150)
	code, body = e.chatAuto(t, e.byoaTok, long, nil)
	if code != 200 {
		t.Fatalf("auto long text: got %d, want 200 (%v)", code, body)
	}
	if got := servedModel(t, body); got != "mock-pro" {
		t.Errorf("auto long text: served %s, want mock-pro", got)
	}
}

func TestAutoRouteDisabled(t *testing.T) {
	e := setup(t)
	setAutoRoute(t, e, &store.AutoRouteConfig{Enabled: false, Threshold: 0.35,
		SimpleGroup: []string{"mock-eco"}, ComplexGroup: []string{"mock-pro"}})
	code, body := e.chatAuto(t, e.byoaTok, "今天星期几？", nil)
	if code != 400 {
		t.Fatalf("auto disabled: got %d, want 400 (%v)", code, body)
	}
	if errObj, _ := body["error"].(map[string]any); errObj["code"] != "model_not_available" {
		t.Errorf("auto disabled: error code %v, want model_not_available", errObj["code"])
	}
}

func TestAutoRouteTierFallback(t *testing.T) {
	e := setup(t)
	// The simple group names a model no channel serves: the
	// request falls back to the complex group, and the audit
	// decision log records both the classified tier and the tier
	// that actually served it.
	setAutoRoute(t, e, &store.AutoRouteConfig{Enabled: true, Threshold: 0.35,
		SimpleGroup: []string{"mock-nope"}, ComplexGroup: []string{"mock-pro"},
		Samples: autorouter.DefaultSamples()})
	code, body := e.chatAuto(t, e.byoaTok, "今天星期几？", nil)
	if code != 200 {
		t.Fatalf("auto tier fallback: got %d, want 200 (%v)", code, body)
	}
	if got := servedModel(t, body); got != "mock-pro" {
		t.Errorf("auto tier fallback: served %s, want mock-pro", got)
	}
	ev, ok := lastModelCall(e.al.All())
	if !ok {
		t.Fatal("no model_call audit event")
	}
	if ev.Model != "mock-pro" {
		t.Errorf("audit model: got %s, want mock-pro (resolved, not auto)", ev.Model)
	}
	if ev.Detail["auto_route"] != "simple:sample" {
		t.Errorf("audit auto_route: got %q, want simple:sample", ev.Detail["auto_route"])
	}
	if ev.Detail["auto_route_requested"] != "auto" {
		t.Errorf("audit auto_route_requested: got %q, want auto", ev.Detail["auto_route_requested"])
	}
	if ev.Detail["auto_route_fallback"] != "complex" {
		t.Errorf("audit auto_route_fallback: got %q, want complex", ev.Detail["auto_route_fallback"])
	}
	if ev.Detail["auto_route_confidence"] == "" {
		t.Error("audit auto_route_confidence missing")
	}
	if ev.Detail["auto_route_sample"] == "" {
		t.Error("audit auto_route_sample missing")
	}
}

func TestAutoRouteAuditDetail(t *testing.T) {
	e := setup(t)
	code, _ := e.chatAuto(t, e.byoaTok, "今天是星期几？", nil)
	if code != 200 {
		t.Fatalf("auto audit: got %d, want 200", code)
	}
	ev, ok := lastModelCall(e.al.All())
	if !ok {
		t.Fatal("no model_call audit event")
	}
	if ev.Detail["auto_route"] != "simple:sample" {
		t.Errorf("audit auto_route: got %q, want simple:sample", ev.Detail["auto_route"])
	}
	if _, ok := ev.Detail["auto_route_fallback"]; ok {
		t.Errorf("audit auto_route_fallback present without fallback: %v", ev.Detail)
	}
	stats := e.gw.AutoRouteStats()
	if stats.Auto != 1 || stats.Simple != 1 || stats.Sample != 1 {
		t.Errorf("runtime stats: %+v, want auto=1 simple=1 sample=1", stats)
	}
}

func TestAutoRouteRespectsDataLevel(t *testing.T) {
	e := setup(t)
	// Confidential content (XH-200): the simple group model
	// (mock-eco, internal only) cannot receive it, so the request
	// falls back to the complex group model (mock-pro) on the
	// confidential channel — complexity routing never widens the
	// compliant set, it only chooses inside it.
	code, body := e.chatAuto(t, e.mgdTok, "请分析 XH-200 项目的进展", nil)
	if code != 200 {
		t.Fatalf("auto confidential: got %d, want 200 (%v)", code, body)
	}
	if got := servedModel(t, body); got != "mock-pro" {
		t.Errorf("auto confidential: served %s, want mock-pro", got)
	}
	ev, ok := lastModelCall(e.al.All())
	if !ok {
		t.Fatal("no model_call audit event")
	}
	if ev.Detail["auto_route_fallback"] != "complex" {
		t.Errorf("audit auto_route_fallback: got %q, want complex", ev.Detail["auto_route_fallback"])
	}
	if ev.DataLevel != "confidential" {
		t.Errorf("audit data level: got %s, want confidential", ev.DataLevel)
	}
}

func TestAutoRouteSecretGoesLocal(t *testing.T) {
	e := setup(t)
	// Secret data: only the local-model channel may receive it.
	// mock-eco cannot serve secret at all; mock-pro serves it on
	// the local channel. The routing decision still records the
	// classified tier (simple) and the tier that served (complex).
	code, body := e.chatAuto(t, e.mgdTok, "今天星期几？",
		map[string]string{"X-AgentGate-Data-Level": "secret"})
	if code != 200 {
		t.Fatalf("auto secret: got %d, want 200 (%v)", code, body)
	}
	if got := servedModel(t, body); got != "mock-pro" {
		t.Errorf("auto secret: served %s, want mock-pro", got)
	}
	ev, ok := lastModelCall(e.al.All())
	if !ok {
		t.Fatal("no model_call audit event")
	}
	if ev.Channel != "local-model (mock)" {
		t.Errorf("audit channel: got %s, want local-model (mock)", ev.Channel)
	}
}

func TestAutoRouteNoCompliantChannel(t *testing.T) {
	e := setup(t)
	// Both groups name models no channel serves at the secret
	// level: the request fails with the same explicit error as a
	// direct model request, not with a routing fallback.
	setAutoRoute(t, e, &store.AutoRouteConfig{Enabled: true, Threshold: 0.35,
		SimpleGroup: []string{"mock-eco"}, ComplexGroup: []string{"mock-eco"}})
	code, body := e.chatAuto(t, e.mgdTok, "今天星期几？",
		map[string]string{"X-AgentGate-Data-Level": "secret"})
	if code != 403 {
		t.Fatalf("auto no compliant channel: got %d, want 403 (%v)", code, body)
	}
	if errObj, _ := body["error"].(map[string]any); errObj["code"] != "no_channel_for_data_level" {
		t.Errorf("auto no compliant channel: error code %v, want no_channel_for_data_level", errObj["code"])
	}
}

func TestModelsListsAuto(t *testing.T) {
	e := setup(t)
	req, _ := http.NewRequest("GET", e.srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+e.byoaTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range out.Data {
		if m.ID == "auto" {
			found = true
		}
	}
	if !found {
		t.Errorf("models list has no auto: %+v", out.Data)
	}
	// Disabled: auto disappears from the list.
	setAutoRoute(t, e, &store.AutoRouteConfig{Enabled: false, Threshold: 0.35,
		SimpleGroup: []string{"mock-eco"}, ComplexGroup: []string{"mock-pro"}})
	req, _ = http.NewRequest("GET", e.srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+e.byoaTok)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out = struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	for _, m := range out.Data {
		if m.ID == "auto" {
			t.Errorf("models list still has auto after disabling: %+v", out.Data)
		}
	}
}
