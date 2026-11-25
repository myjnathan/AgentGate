package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/myjnathan/agentgate/internal/api"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/seed"
	"github.com/myjnathan/agentgate/internal/store"
)

func setupOnboardingAPI(t *testing.T, seeded bool) (*httptest.Server, *api.API, string, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secretbox.New("test-master-key-123456")
	au := auth.New(st)
	if seeded {
		if err := seed.Demo(st, box, "http://127.0.0.1:9"); err != nil {
			t.Fatal(err)
		}
	}
	al, err := audit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	rest := api.New(st, au, box, al)
	mux := http.NewServeMux()
	rest.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	call := func(method, path, sess string, body any) (int, map[string]any) {
		var rdr *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, srv.URL+path, rdr)
		if sess != "" {
			req.Header.Set("Authorization", "Bearer "+sess)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var d map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&d)
		return resp.StatusCode, d
	}
	login := func(username, password string) string {
		s, d := call("POST", "/api/login", "", map[string]any{"username": username, "password": password})
		if s != 200 {
			t.Fatalf("login %s failed: %d %v", username, s, d)
		}
		return d["session"].(string)
	}
	var admin, emp string
	if seeded {
		admin = login("admin", "agentgate-admin")
		emp = login("lixiaofeng", "demo12345")
	}
	_ = call
	return srv, rest, admin, emp
}

func callOnboard(t *testing.T, srv *httptest.Server, method, path, sess string, body any) (int, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rdr)
	if sess != "" {
		req.Header.Set("Authorization", "Bearer "+sess)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var d map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&d)
	return resp.StatusCode, d
}

func TestSetupFirstAdmin(t *testing.T) {
	srv, rest, _, _ := setupOnboardingAPI(t, false)
	rest.SetSetupCode("ABCD-EFGH-IJKL")

	s, d := callOnboard(t, srv, "GET", "/api/setup/status", "", nil)
	if s != 200 || d["setup_required"] != true {
		t.Fatalf("fresh store must require setup: %d %v", s, d)
	}
	// Wrong code is refused and does not create anyone.
	s, _ = callOnboard(t, srv, "POST", "/api/setup", "", map[string]any{
		"code": "WRONG-CODE-0000", "username": "admin", "password": "admin-pass-1"})
	if s != 401 {
		t.Fatalf("wrong setup code must be 401, got %d", s)
	}
	// Weak password and bad username are refused even with the code.
	s, _ = callOnboard(t, srv, "POST", "/api/setup", "", map[string]any{
		"code": "ABCD-EFGH-IJKL", "username": "admin", "password": "short"})
	if s != 400 {
		t.Fatalf("short password must be 400, got %d", s)
	}
	s, _ = callOnboard(t, srv, "POST", "/api/setup", "", map[string]any{
		"code": "ABCD-EFGH-IJKL", "username": "a b", "password": "admin-pass-1"})
	if s != 400 {
		t.Fatalf("bad username must be 400, got %d", s)
	}
	// The right code creates the first admin, single-use.
	s, d = callOnboard(t, srv, "POST", "/api/setup", "", map[string]any{
		"code": "ABCD-EFGH-IJKL", "username": "admin", "password": "admin-pass-1"})
	if s != 201 || d["role"] != "admin" {
		t.Fatalf("setup with the right code must create an admin: %d %v", s, d)
	}
	s, d = callOnboard(t, srv, "GET", "/api/setup/status", "", nil)
	if s != 200 || d["setup_required"] != false {
		t.Fatalf("setup must be complete after first admin: %d %v", s, d)
	}
	s, _ = callOnboard(t, srv, "POST", "/api/setup", "", map[string]any{
		"code": "ABCD-EFGH-IJKL", "username": "admin2", "password": "admin-pass-1"})
	if s != 409 {
		t.Fatalf("second setup must be 409, got %d", s)
	}
	// The new admin can log in.
	s, d = callOnboard(t, srv, "POST", "/api/login", "", map[string]any{"username": "admin", "password": "admin-pass-1"})
	if s != 200 || d["session"] == "" {
		t.Fatalf("new admin must be able to log in: %d %v", s, d)
	}
}

func TestSetupCodeExhaustedAfterFiveFailures(t *testing.T) {
	srv, rest, _, _ := setupOnboardingAPI(t, false)
	rest.SetSetupCode("ABCD-EFGH-IJKL")
	for i := 0; i < 5; i++ {
		s, _ := callOnboard(t, srv, "POST", "/api/setup", "", map[string]any{
			"code": "WRONG-CODE-0000", "username": "admin", "password": "admin-pass-1"})
		if s != 401 {
			t.Fatalf("failed attempt %d must be 401, got %d", i+1, s)
		}
	}
	// Even the right code is void now; only a restart issues a new one.
	s, _ := callOnboard(t, srv, "POST", "/api/setup", "", map[string]any{
		"code": "ABCD-EFGH-IJKL", "username": "admin", "password": "admin-pass-1"})
	if s != 410 {
		t.Fatalf("exhausted code must be 410, got %d", s)
	}
}

func TestSetupRequiresCodeIssued(t *testing.T) {
	srv, _, _, _ := setupOnboardingAPI(t, false)
	// No SetSetupCode call: the server never issued a code.
	s, _ := callOnboard(t, srv, "POST", "/api/setup", "", map[string]any{
		"code": "ABCD-EFGH-IJKL", "username": "admin", "password": "admin-pass-1"})
	if s != 503 {
		t.Fatalf("setup without an issued code must be 503, got %d", s)
	}
}

func TestOnboardingTemplatesAndPhase(t *testing.T) {
	srv, _, admin, emp := setupOnboardingAPI(t, true)

	s, d := callOnboard(t, srv, "GET", "/api/onboarding/templates", admin, nil)
	if s != 200 {
		t.Fatalf("templates list failed: %d %v", s, d)
	}
	ts, _ := d["templates"].([]any)
	if len(ts) != 2 {
		t.Fatalf("want 2 templates, got %d", len(ts))
	}
	s, _ = callOnboard(t, srv, "GET", "/api/onboarding/templates", emp, nil)
	if s != 403 {
		t.Fatalf("employee must not list templates: %d", s)
	}
	s, _ = callOnboard(t, srv, "POST", "/api/onboarding/apply-template", admin, map[string]any{"template_id": "nope"})
	if s != 400 {
		t.Fatalf("unknown template must be 400, got %d", s)
	}
	s, d = callOnboard(t, srv, "POST", "/api/onboarding/apply-template", admin, map[string]any{"template_id": "medical-device"})
	if s != 200 || d["launch_phase"] != "observe" || d["custom_words_added"] != float64(6) {
		t.Fatalf("medical template apply failed: %d %v", s, d)
	}
	s, d = callOnboard(t, srv, "GET", "/api/onboarding/launch-phase", admin, nil)
	if s != 200 || d["launch_phase"] != "observe" {
		t.Fatalf("phase must be observe after template: %d %v", s, d)
	}
	// Re-applying the same template adds nothing new.
	s, d = callOnboard(t, srv, "POST", "/api/onboarding/apply-template", admin, map[string]any{"template_id": "medical-device"})
	if s != 200 || d["custom_words_added"] != float64(0) {
		t.Fatalf("re-apply must add 0 words: %d %v", s, d)
	}
	// Phase switching round-trips and validates.
	s, d = callOnboard(t, srv, "POST", "/api/onboarding/launch-phase", admin, map[string]any{"phase": "block"})
	if s != 200 || d["launch_phase"] != "block" || d["previous_phase"] != "observe" {
		t.Fatalf("phase switch to block failed: %d %v", s, d)
	}
	s, _ = callOnboard(t, srv, "POST", "/api/onboarding/launch-phase", admin, map[string]any{"phase": "sideways"})
	if s != 400 {
		t.Fatalf("invalid phase must be 400, got %d", s)
	}
	s, _ = callOnboard(t, srv, "POST", "/api/onboarding/launch-phase", emp, map[string]any{"phase": "warn"})
	if s != 403 {
		t.Fatalf("employee must not switch phase: %d", s)
	}
}

func TestOnboardingChecklist(t *testing.T) {
	srv, _, admin, emp := setupOnboardingAPI(t, true)

	s, _ := callOnboard(t, srv, "GET", "/api/onboarding/checklist", emp, nil)
	if s != 403 {
		t.Fatalf("employee must not read the checklist: %d", s)
	}
	s, d := callOnboard(t, srv, "GET", "/api/onboarding/checklist", admin, nil)
	if s != 200 {
		t.Fatalf("checklist failed: %d %v", s, d)
	}
	items, _ := d["items"].([]any)
	byID := map[string]map[string]any{}
	for _, it := range items {
		m, _ := it.(map[string]any)
		byID[m["id"].(string)] = m
	}
	for _, want := range []string{"admin_created", "channel_configured", "channel_healthy",
		"device_onboarded", "kb_indexed", "audit_writing", "master_key_backed_up", "launch_phase"} {
		if _, ok := byID[want]; !ok {
			t.Fatalf("checklist missing item %q", want)
		}
	}
	// Every item ships an English label/detail so the console can render
	// the checklist in EN mode (F36: checklist labels used to be zh-only).
	for id, m := range byID {
		if m["label_en"] == nil || m["label_en"] == "" {
			t.Fatalf("checklist item %q has no label_en", id)
		}
		if m["detail"] != nil && m["detail"] != "" && (m["detail_en"] == nil || m["detail_en"] == "") {
			t.Fatalf("checklist item %q has detail but no detail_en", id)
		}
	}
	// The demo seed has an admin, enabled healthy channels and audit
	// events (the logins above); no tokens and no folder index here.
	if byID["admin_created"]["ok"] != true {
		t.Fatal("admin_created must be true in the demo seed")
	}
	if byID["channel_configured"]["ok"] != true || byID["channel_healthy"]["ok"] != true {
		t.Fatal("demo channels must be configured and healthy")
	}
	if byID["audit_writing"]["ok"] != true {
		t.Fatal("audit_writing must be true after login events")
	}
	if byID["device_onboarded"]["ok"] != false {
		t.Fatal("device_onboarded must be false with no tokens issued")
	}
	if byID["kb_indexed"]["ok"] != false {
		t.Fatal("kb_indexed must be false with no folder registry wired")
	}
	if byID["master_key_backed_up"]["ok"] != false {
		t.Fatal("master_key_backed_up must start false")
	}
	if d["completed"] != false {
		t.Fatal("wizard must start uncompleted")
	}
	s, _ = callOnboard(t, srv, "POST", "/api/onboarding/ack", admin, map[string]any{"master_key_backed_up": true})
	if s != 200 {
		t.Fatalf("ack failed: %d", s)
	}
	s, d = callOnboard(t, srv, "GET", "/api/onboarding/checklist", admin, nil)
	if byID2 := checklistByID(d); byID2["master_key_backed_up"]["ok"] != true {
		t.Fatal("master_key_backed_up must be true after ack")
	}
	s, d = callOnboard(t, srv, "POST", "/api/onboarding/complete", admin, nil)
	if s != 200 || d["completed"] != true {
		t.Fatalf("complete failed: %d %v", s, d)
	}
	s, d = callOnboard(t, srv, "GET", "/api/onboarding/checklist", admin, nil)
	if d["completed"] != true {
		t.Fatal("checklist must report completed after complete")
	}
}

func checklistByID(d map[string]any) map[string]map[string]any {
	items, _ := d["items"].([]any)
	byID := map[string]map[string]any{}
	for _, it := range items {
		m, _ := it.(map[string]any)
		byID[m["id"].(string)] = m
	}
	return byID
}
