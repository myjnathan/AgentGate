package ha

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Mode
	}{
		{"", ModeActive},
		{"active", ModeActive},
		{"standby", ModeStandby},
	} {
		got, err := ParseMode(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseMode(%q) = %v, %v; want %v, nil", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"leader", "ACTIVE", "primary", "stand-by"} {
		if _, err := ParseMode(bad); err == nil {
			t.Errorf("ParseMode(%q) = nil error; want error", bad)
		}
	}
}

func TestProblems(t *testing.T) {
	if got := Problems(nil, nil, true); len(got) != 0 {
		t.Errorf("healthy Problems() = %v; want empty", got)
	}
	err := errors.New("boom")
	got := Problems(err, err, false)
	want := []string{"state_save_failed", "audit_append_failed", "audit_chain_invalid"}
	if len(got) != len(want) {
		t.Fatalf("Problems() = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Problems()[%d] = %q; want %q", i, got[i], want[i])
		}
	}
}

func TestReadyHandler(t *testing.T) {
	var storeErr, auditErr error
	h := ReadyHandler(func() error { return storeErr }, func() error { return auditErr }, true)

	// Healthy: 200 {"ready":true}.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthy readyz status = %d; want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["ready"] != true {
		t.Fatalf("healthy readyz body = %s; want {\"ready\":true}", rec.Body.String())
	}

	// One failing subsystem: 503 with the category, no raw error text.
	storeErr = errors.New("open /data/state.json.tmp: no space left on device")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unhealthy readyz status = %d; want 503", rec.Code)
	}
	body = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unhealthy readyz body not JSON: %v", err)
	}
	if body["ready"] != false {
		t.Fatalf("unhealthy readyz body = %s; want ready=false", rec.Body.String())
	}
	problems, _ := body["problems"].([]any)
	if len(problems) != 1 || problems[0] != "state_save_failed" {
		t.Fatalf("problems = %v; want [state_save_failed]", problems)
	}
	if strings.Contains(rec.Body.String(), "no space") {
		t.Fatalf("readyz body leaks raw error text: %s", rec.Body.String())
	}

	// Broken chain alone is also not ready.
	storeErr = nil
	h = ReadyHandler(func() error { return nil }, func() error { return nil }, false)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("chain-broken readyz status = %d; want 503", rec.Code)
	}
	body = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	problems, _ = body["problems"].([]any)
	if len(problems) != 1 || problems[0] != "audit_chain_invalid" {
		t.Fatalf("problems = %v; want [audit_chain_invalid]", problems)
	}
}
