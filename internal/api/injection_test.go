package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/myjnathan/agentgate/internal/api"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/seed"
	"github.com/myjnathan/agentgate/internal/store"
)

func setupInjectionAPI(t *testing.T) (*httptest.Server, string, string, *audit.Log) {
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
		if resp.StatusCode != 200 || d.Session == "" {
			t.Fatalf("login %s failed: %d", username, resp.StatusCode)
		}
		return d.Session
	}
	return srv, login("admin", "agentgate-admin"), login("lixiaofeng", "demo12345"), al
}

func TestInjectionEndpoints(t *testing.T) {
	srv, adminSess, empSess, al := setupInjectionAPI(t)

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
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// Defaults: enabled, alert mode, semantic route on at the
	// built-in threshold, never configured.
	code, d := call("GET", "/api/injection", adminSess, nil)
	if code != 200 {
		t.Fatalf("GET injection: got %d", code)
	}
	if d["enabled"] != true || d["mode"] != "alert" || d["configured"] != false {
		t.Errorf("defaults: %v, want enabled=true mode=alert configured=false", d)
	}
	if d["semantic_enabled"] != true || d["semantic_threshold"] != 0.35 {
		t.Errorf("defaults: semantic_enabled=%v semantic_threshold=%v, want true/0.35",
			d["semantic_enabled"], d["semantic_threshold"])
	}
	if _, ok := d["stats"]; !ok {
		t.Error("defaults: stats missing")
	}

	// Employee may not read or write the policy.
	if code, _ := call("GET", "/api/injection", empSess, nil); code != 403 {
		t.Errorf("GET injection as employee: got %d, want 403", code)
	}
	if code, _ := call("POST", "/api/injection", empSess, map[string]any{"mode": "block"}); code != 403 {
		t.Errorf("POST injection as employee: got %d, want 403", code)
	}

	// Unknown mode is rejected; a valid update persists.
	if code, _ := call("POST", "/api/injection", adminSess, map[string]any{"mode": "panic"}); code != 400 {
		t.Errorf("POST injection with bad mode: got %d, want 400", code)
	}
	// A bad semantic threshold is rejected; a valid semantic
	// update persists (the route switch is independent of the
	// rule layer's mode).
	if code, _ := call("POST", "/api/injection", adminSess, map[string]any{"semantic_threshold": 1.5}); code != 400 {
		t.Errorf("POST injection with bad threshold: got %d, want 400", code)
	}
	code, d = call("POST", "/api/injection", adminSess, map[string]any{"semantic_enabled": false, "semantic_threshold": 0.5})
	if code != 200 || d["semantic_enabled"] != false || d["semantic_threshold"] != 0.5 {
		t.Fatalf("POST injection semantic update: got %d %v", code, d)
	}
	code, d = call("GET", "/api/injection", adminSess, nil)
	if code != 200 || d["semantic_enabled"] != false || d["semantic_threshold"] != 0.5 {
		t.Errorf("GET after semantic update: got %d %v, want semantic_enabled=false threshold=0.5", code, d)
	}
	code, d = call("POST", "/api/injection", adminSess, map[string]any{"mode": "block"})
	if code != 200 || d["mode"] != "block" {
		t.Fatalf("POST injection block mode: got %d %v", code, d)
	}
	code, d = call("GET", "/api/injection", adminSess, nil)
	if code != 200 || d["mode"] != "block" || d["configured"] != true {
		t.Errorf("GET after update: got %d %v, want mode=block configured=true", code, d)
	}

	// The updates are audited (policy only, no request text);
	// the block-mode update is among them.
	found := false
	for _, ev := range al.All() {
		if ev.Kind == "admin" && ev.Action == "injection_policy_updated" && ev.Detail["mode"] == "block" {
			found = true
		}
	}
	if !found {
		t.Error("no injection_policy_updated audit event with mode=block")
	}
}
