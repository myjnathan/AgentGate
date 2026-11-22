package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/api"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/seed"
	"github.com/myjnathan/agentgate/internal/store"
)

// reportShape mirrors the wire shape of a generated report (only the
// fields this test asserts on).
type reportShape struct {
	ID           string    `json:"id"`
	Period       string    `json:"period"`
	PeriodStart  time.Time `json:"period_start"`
	PeriodEnd    time.Time `json:"period_end"`
	Calls        int64     `json:"calls"`
	Tokens       int64     `json:"tokens"`
	Blocked      int64     `json:"blocked"`
	Desensitised int64     `json:"desensitised"`
	Audit        struct {
		EventsInWindow int64 `json:"events_in_window"`
		ChainValid     bool  `json:"chain_valid"`
	} `json:"audit"`
	ChannelTotal int `json:"channel_total"`
	TokensOv     struct {
		Total int64 `json:"total"`
	} `json:"token_overview"`
	Terminals struct {
		Total int64 `json:"total"`
	} `json:"terminals"`
	Exceptions struct {
		Requested int64 `json:"requested"`
	} `json:"exceptions"`
	Note string `json:"note"`
}

func setupReportsAPI(t *testing.T) (*httptest.Server, *audit.Log, string, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secretbox.New("test-master-key-123456")
	au := auth.New(st)
	if err := seed.Demo(st, box, "http://127.0.0.1:9"); err != nil {
		t.Fatal(err)
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

	login := func(username, password string) string {
		body := strings.NewReader(`{"username":"` + username + `","password":"` + password + `"}`)
		resp, err := http.Post(srv.URL+"/api/login", "application/json", body)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var d struct {
			Session string `json:"session"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&d)
		if d.Session == "" {
			t.Fatalf("login %s failed", username)
		}
		return d.Session
	}
	return srv, al, login("admin", "agentgate-admin"), login("lixiaofeng", "demo12345")
}

func TestReportsFlow(t *testing.T) {
	srv, al, adminSess, empSess := setupReportsAPI(t)

	call := func(method, path, sess string, body any) (int, []byte) {
		t.Helper()
		var buf *bytes.Buffer
		if body != nil {
			b, _ := json.Marshal(body)
			buf = bytes.NewBuffer(b)
		} else {
			buf = bytes.NewBuffer(nil)
		}
		req, _ := http.NewRequest(method, srv.URL+path, buf)
		req.Header.Set("Content-Type", "application/json")
		if sess != "" {
			req.Header.Set("Authorization", "Bearer "+sess)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}
	decode := func(raw []byte, v any) {
		t.Helper()
		if err := json.Unmarshal(raw, v); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
	}

	// Seed the audit log with in-scope calls: 3 model calls (one
	// redacted) + 1 blocked call + 1 exception request + noise.
	// All land in "now", i.e. inside this week's window. Actor IDs
	// come from the login events the setup already recorded, so the
	// department dimension resolves the same way it does in
	// production.
	var adminID, lixID string
	for _, ev := range al.All() {
		if ev.Actor == "admin" {
			adminID = ev.ActorID
		}
		if ev.Actor == "lixiaofeng" {
			lixID = ev.ActorID
		}
	}
	if adminID == "" || lixID == "" {
		t.Fatal("seed logins did not record actor IDs")
	}
	al.Append(audit.Event{Kind: "model_call", Action: "chat_completions", Actor: "lixiaofeng", ActorID: lixID,
		AgentType: "claude-code", Model: "mock-eco", Channel: "eco", DataLevel: "internal", Decision: "allow", Tokens: 100})
	al.Append(audit.Event{Kind: "model_call", Action: "chat_completions", Actor: "lixiaofeng", ActorID: lixID,
		AgentType: "claude-code", Model: "mock-eco", Channel: "eco", DataLevel: "internal", Decision: "redact", Tokens: 50})
	al.Append(audit.Event{Kind: "model_call", Action: "chat_completions", Actor: "admin", ActorID: adminID,
		AgentType: "dsh", Model: "mock-pro", Channel: "pro", DataLevel: "confidential", Decision: "reroute", Tokens: 200})
	al.Append(audit.Event{Kind: "violation", Action: "model_call_blocked", Actor: "lixiaofeng", ActorID: lixID,
		AgentType: "claude-code", Model: "mock-pro", DataLevel: "confidential", Decision: "block",
		Detail: map[string]string{"findings": "banned_word"}})
	al.Append(audit.Event{Kind: "exception", Action: "exception_requested", Actor: "lixiaofeng", ActorID: lixID, Decision: "allow"})
	al.Append(audit.Event{Kind: "auth", Action: "login", Actor: "lixiaofeng", ActorID: lixID, Decision: "allow"})

	// Auth gating first: anonymous 401, employee 403.
	if code, _ := call("GET", "/api/reports", "", nil); code != 401 {
		t.Fatalf("anonymous list reports: got %d, want 401", code)
	}
	if code, _ := call("GET", "/api/reports", empSess, nil); code != 403 {
		t.Fatalf("employee list reports: got %d, want 403", code)
	}
	if code, _ := call("POST", "/api/reports", empSess, map[string]any{"period": "weekly"}); code != 403 {
		t.Fatalf("employee generate report: got %d, want 403", code)
	}

	// Validation matrix.
	for name, body := range map[string]map[string]any{
		"bad period": {"period": "daily"},
		"no period":  {},
		"bad date":   {"period": "weekly", "date": "22-11-2026"},
	} {
		if code, _ := call("POST", "/api/reports", adminSess, body); code != 400 {
			t.Fatalf("%s: got %d, want 400", name, code)
		}
	}

	// Generate this week's report. Events in window: 2 logins + 6
	// seeded = 8; the report_generated event itself lands after.
	code, raw := call("POST", "/api/reports", adminSess, map[string]any{"period": "weekly"})
	if code != 201 {
		t.Fatalf("generate report: got %d (%s), want 201", code, raw)
	}
	var rep reportShape
	decode(raw, &rep)
	if rep.ID == "" || !strings.HasPrefix(rep.ID, "rep_") {
		t.Fatalf("report id = %q, want rep_ prefix", rep.ID)
	}
	if rep.Period != "weekly" {
		t.Fatalf("period = %q, want weekly", rep.Period)
	}
	// Monday 00:00 UTC to next Monday.
	if rep.PeriodStart.Weekday() != time.Monday || rep.PeriodStart.Hour() != 0 {
		t.Fatalf("period_start = %s, want a Monday midnight UTC", rep.PeriodStart)
	}
	if !rep.PeriodEnd.Equal(rep.PeriodStart.AddDate(0, 0, 7)) {
		t.Fatalf("period_end = %s, want start+7d", rep.PeriodEnd)
	}
	if rep.Calls != 4 || rep.Tokens != 350 || rep.Blocked != 1 {
		t.Fatalf("usage = calls %d tokens %d blocked %d, want 4/350/1", rep.Calls, rep.Tokens, rep.Blocked)
	}
	if rep.Desensitised != 1 {
		t.Fatalf("desensitised = %d, want 1", rep.Desensitised)
	}
	if rep.Audit.EventsInWindow != 8 {
		t.Fatalf("events_in_window = %d, want 8", rep.Audit.EventsInWindow)
	}
	if !rep.Audit.ChainValid {
		t.Fatal("chain_valid must be true for the test log")
	}
	if rep.ChannelTotal < 1 {
		t.Fatalf("snapshot channels = %d, want at least the seeded channels", rep.ChannelTotal)
	}
	// The demo seed ships no API tokens (tokens only exist after a
	// device approval) and no terminals yet — both overviews are
	// legitimately zero here; the smoke test covers them populated.
	if rep.TokensOv.Total != 0 || rep.Terminals.Total != 0 {
		t.Fatalf("snapshot tokens/terminals = %d/%d, want 0/0 on a fresh seed",
			rep.TokensOv.Total, rep.Terminals.Total)
	}
	if rep.Exceptions.Requested != 1 {
		t.Fatalf("exceptions.requested = %d, want 1", rep.Exceptions.Requested)
	}
	if !strings.Contains(rep.Note, "不是合规认证") {
		t.Fatalf("report note missing disclaimer: %q", rep.Note)
	}

	// List shows it; get returns the same report.
	code, raw = call("GET", "/api/reports", adminSess, nil)
	if code != 200 {
		t.Fatalf("list reports: got %d, want 200", code)
	}
	var listed struct {
		Reports []reportShape `json:"reports"`
	}
	decode(raw, &listed)
	if len(listed.Reports) != 1 || listed.Reports[0].ID != rep.ID {
		t.Fatalf("listed = %+v, want the one generated report", listed.Reports)
	}
	code, raw = call("GET", "/api/reports/"+rep.ID, adminSess, nil)
	if code != 200 {
		t.Fatalf("get report: got %d, want 200", code)
	}
	var got reportShape
	decode(raw, &got)
	if got.ID != rep.ID || got.Calls != 4 {
		t.Fatalf("get report = %+v, want id %s calls 4", got, rep.ID)
	}

	// Export: CSV download with the totals row and the finding row.
	code, raw = call("GET", "/api/reports/"+rep.ID+"/export", adminSess, nil)
	if code != 200 {
		t.Fatalf("export report: got %d, want 200", code)
	}
	csvText := string(raw)
	for _, want := range []string{
		"report_id," + rep.ID,
		"section,key,calls,tokens,blocked,cost_usd",
		"usage,total,4,350,1,",
		"blocked_by_finding,banned_word,1,0,0,0",
		"exceptions,requested,1,0,0,0",
	} {
		if !strings.Contains(csvText, want) {
			t.Fatalf("csv missing %q:\n%s", want, csvText)
		}
	}

	// Regenerating the same window creates a second report.
	code, raw = call("POST", "/api/reports", adminSess, map[string]any{"period": "weekly"})
	if code != 201 {
		t.Fatalf("regenerate report: got %d, want 201", code)
	}
	var rep2 reportShape
	decode(raw, &rep2)
	if rep2.ID == rep.ID {
		t.Fatal("regeneration must create a new report, not overwrite")
	}

	// Unknown IDs 404 on all three single-report endpoints.
	for _, path := range []string{"/api/reports/rep_nope", "/api/reports/rep_nope/export"} {
		if code, _ := call("GET", path, adminSess, nil); code != 404 {
			t.Fatalf("GET %s: got %d, want 404", path, code)
		}
	}
	if code, _ := call("DELETE", "/api/reports/rep_nope", adminSess, nil); code != 404 {
		t.Fatalf("DELETE unknown report: got %d, want 404", code)
	}

	// Delete the first report; the second stays.
	if code, _ := call("DELETE", "/api/reports/"+rep.ID, adminSess, nil); code != 200 {
		t.Fatalf("delete report: got %d, want 200", code)
	}
	if code, _ := call("GET", "/api/reports/"+rep.ID, adminSess, nil); code != 404 {
		t.Fatalf("get deleted report: got %d, want 404", code)
	}
	code, raw = call("GET", "/api/reports", adminSess, nil)
	decode(raw, &listed)
	if len(listed.Reports) != 1 || listed.Reports[0].ID != rep2.ID {
		t.Fatalf("listed after delete = %+v, want only %s", listed.Reports, rep2.ID)
	}
}

// TestReportsRetentionCap generates more than MaxStoredReports and
// checks the store keeps only the newest 52.
func TestReportsRetentionCap(t *testing.T) {
	srv, _, adminSess, _ := setupReportsAPI(t)
	call := func(method, path string, body any) (int, []byte) {
		t.Helper()
		var buf *bytes.Buffer
		if body != nil {
			b, _ := json.Marshal(body)
			buf = bytes.NewBuffer(b)
		} else {
			buf = bytes.NewBuffer(nil)
		}
		req, _ := http.NewRequest(method, srv.URL+path, buf)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+adminSess)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}
	for i := 0; i < 54; i++ {
		code, _ := call("POST", "/api/reports", map[string]any{"period": "monthly", "date": "2026-11-01"})
		if code != 201 {
			t.Fatalf("generate %d: got %d, want 201", i, code)
		}
	}
	code, raw := call("GET", "/api/reports", nil)
	if code != 200 {
		t.Fatalf("list reports: got %d, want 200", code)
	}
	var listed struct {
		Reports []reportShape `json:"reports"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Reports) != 52 {
		t.Fatalf("retained reports = %d, want 52", len(listed.Reports))
	}
}
