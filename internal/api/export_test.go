package api_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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

// exportTestServer mirrors the stats endpoint test setup: a demo-seeded
// store with a real audit log behind an httptest server.
func exportTestServer(t *testing.T) (*httptest.Server, string, string, *audit.Log, string) {
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
	al.SetRetentionDays(180)
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
		if resp.StatusCode != 200 || d.Session == "" {
			t.Fatalf("login %s failed: %d", username, resp.StatusCode)
		}
		return d.Session
	}
	return srv, login("admin", "agentgate-admin"), login("lixiaofeng", "demo12345"), al, dir
}

func TestExportAuditEndpoint(t *testing.T) {
	srv, adminSess, empSess, al, _ := exportTestServer(t)
	// Two login events already in the log; add three more.
	al.Append(audit.Event{Kind: "model_call", Action: "chat_completions", Actor: "lixiaofeng", Model: "mock-eco", Decision: "allow"})
	al.Append(audit.Event{Kind: "model_call", Action: "chat_completions", Actor: "lixiaofeng", Model: "mock-eco", Decision: "redact"})
	al.Append(audit.Event{Kind: "violation", Action: "model_call_blocked", Actor: "lixiaofeng", Decision: "block"})

	get := func(path, sess string) (*http.Response, []byte) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if sess != "" {
			req.Header.Set("Authorization", "Bearer "+sess)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return resp, buf.Bytes()
	}

	resp, body := get("/api/audit/export", adminSess)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/audit/export = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("Content-Type = %q, want application/x-ndjson", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".ndjson") {
		t.Fatalf("Content-Disposition = %q, want an attachment .ndjson filename", cd)
	}
	if got := resp.Header.Get("X-Audit-Chain-Valid"); got != "true" {
		t.Fatalf("X-Audit-Chain-Valid = %q, want true", got)
	}
	if got := resp.Header.Get("X-Audit-Event-Count"); got != "5" {
		t.Fatalf("X-Audit-Event-Count = %q, want 5 (2 logins + 3 appended)", got)
	}
	// Body is 5 NDJSON lines, oldest first, re-verifying as a chain.
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) != 5 {
		t.Fatalf("export body has %d lines, want 5", len(lines))
	}
	var last audit.Event
	for i, line := range lines {
		var ev audit.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %d is not valid JSON: %v", i, err)
		}
		if ev.Seq != int64(i+1) {
			t.Fatalf("line %d has seq %d, want %d (oldest first)", i, ev.Seq, i+1)
		}
		last = ev
	}
	if got := resp.Header.Get("X-Audit-Head-Hash"); got != last.Hash {
		t.Fatalf("X-Audit-Head-Hash = %q, want last event hash %q", got, last.Hash)
	}
	if got := resp.Header.Get("X-Audit-First-Seq"); got != "1" {
		t.Fatalf("X-Audit-First-Seq = %q, want 1", got)
	}
	if got := resp.Header.Get("X-Audit-Last-Seq"); got != "5" {
		t.Fatalf("X-Audit-Last-Seq = %q, want 5", got)
	}
	// The export itself is audited, but after the body (not included).
	found := false
	for _, ev := range al.All() {
		if ev.Action == "audit_exported" && ev.Actor == "admin" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected an audit_exported event after the export")
	}

	// Kind filter narrows the export.
	resp, _ = get("/api/audit/export?kind=model_call", adminSess)
	if resp.StatusCode != 200 || resp.Header.Get("X-Audit-Event-Count") != "2" {
		t.Fatalf("kind filter: status=%d count=%q, want 200 and 2",
			resp.StatusCode, resp.Header.Get("X-Audit-Event-Count"))
	}
	// Future window exports nothing but still reports a valid chain.
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	resp, body = get("/api/audit/export?since="+future, adminSess)
	if resp.StatusCode != 200 || resp.Header.Get("X-Audit-Event-Count") != "0" || len(body) != 0 {
		t.Fatalf("future window: status=%d count=%q body=%d bytes, want 200, 0, 0",
			resp.StatusCode, resp.Header.Get("X-Audit-Event-Count"), len(body))
	}
	if got := resp.Header.Get("X-Audit-Chain-Valid"); got != "true" {
		t.Fatalf("empty export X-Audit-Chain-Valid = %q, want true", got)
	}
	// Bad params are 400s; gating matches the other admin endpoints.
	for _, path := range []string{
		"/api/audit/export?since=not-a-date",
		"/api/audit/export?since=2026-12-01T00:00:00Z&until=2026-11-01T00:00:00Z",
	} {
		resp, _ := get(path, adminSess)
		if resp.StatusCode != 400 {
			t.Fatalf("GET %s = %d, want 400", path, resp.StatusCode)
		}
	}
	resp, _ = get("/api/audit/export", empSess)
	if resp.StatusCode != 403 {
		t.Fatalf("employee export = %d, want 403", resp.StatusCode)
	}
	resp, _ = get("/api/audit/export", "")
	if resp.StatusCode != 401 {
		t.Fatalf("anonymous export = %d, want 401", resp.StatusCode)
	}
}

func TestArchiveAuditEndpoint(t *testing.T) {
	srv, adminSess, empSess, al, dir := exportTestServer(t)
	al.Append(audit.Event{Kind: "model_call", Action: "chat_completions", Actor: "lixiaofeng", Model: "mock-eco", Decision: "allow"})
	al.Append(audit.Event{Kind: "violation", Action: "model_call_blocked", Actor: "lixiaofeng", Decision: "block"})

	post := func(path, sess, payload string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		if sess != "" {
			req.Header.Set("Authorization", "Bearer "+sess)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	get := func(path, sess string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if sess != "" {
			req.Header.Set("Authorization", "Bearer "+sess)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// No-op archive: nothing predates 2020.
	code, rep := post("/api/audit/archive", adminSess, `{"before":"2020-01-01T00:00:00Z"}`)
	if code != 200 {
		t.Fatalf("no-op archive = %d, want 200", code)
	}
	if rep["archived"] != float64(0) || rep["archive_file"] != "" {
		t.Fatalf("no-op archive report = %+v, want archived 0, no file", rep)
	}
	// Archive everything (2 logins + 2 appended + the audit_archived
	// event the no-op archive above recorded).
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	code, rep = post("/api/audit/archive", adminSess, `{"before":"`+future+`"}`)
	if code != 200 {
		t.Fatalf("archive = %d, want 200", code)
	}
	if rep["archived"] != float64(5) {
		t.Fatalf("archive report = %+v, want archived 5", rep)
	}
	if rep["archive_file"] == "" || rep["manifest_file"] == "" {
		t.Fatalf("archive report missing file names: %+v", rep)
	}
	if rep["retention_days"] != float64(180) {
		t.Fatalf("archive report retention_days = %v, want 180", rep["retention_days"])
	}
	if _, err := os.Stat(filepath.Join(dir, rep["archive_file"].(string))); err != nil {
		t.Fatalf("archive file missing on disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, rep["manifest_file"].(string))); err != nil {
		t.Fatalf("manifest file missing on disk: %v", err)
	}
	// Live chain still verifies, and the archive itself is audited.
	code, ver := get("/api/audit/verify", adminSess)
	if code != 200 || ver["chain_valid"] != true {
		t.Fatalf("verify after archive: code=%d body=%+v, want 200 chain_valid true", code, ver)
	}
	found := false
	for _, ev := range al.All() {
		if ev.Action == "audit_archived" && ev.Actor == "admin" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected an audit_archived event after the archive")
	}

	// Bad input and gating.
	for _, payload := range []string{`{}`, `{"before":"not-a-date"}`, `not json`} {
		code, _ := post("/api/audit/archive", adminSess, payload)
		if code != 400 {
			t.Fatalf("archive %q = %d, want 400", payload, code)
		}
	}
	code, _ = post("/api/audit/archive", empSess, `{"before":"2020-01-01T00:00:00Z"}`)
	if code != 403 {
		t.Fatalf("employee archive = %d, want 403", code)
	}
	code, _ = post("/api/audit/archive", "", `{"before":"2020-01-01T00:00:00Z"}`)
	if code != 401 {
		t.Fatalf("anonymous archive = %d, want 401", code)
	}
}
