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

func setupAutoRouteAPI(t *testing.T) (*httptest.Server, string, string, *audit.Log) {
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

func TestAutoRouteEndpoints(t *testing.T) {
	srv, adminSess, empSess, _ := setupAutoRouteAPI(t)

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

	// Defaults: enabled, seed samples, demo model groups.
	code, d := call("GET", "/api/auto-route", adminSess, nil)
	if code != 200 {
		t.Fatalf("GET auto-route: got %d", code)
	}
	if d["enabled"] != true {
		t.Errorf("defaults: enabled=%v, want true", d["enabled"])
	}
	if d["configured"] != false {
		t.Errorf("defaults: configured=%v, want false (never configured)", d["configured"])
	}
	samples, _ := d["samples"].([]any)
	if len(samples) != 12 {
		t.Errorf("defaults: %d samples, want 12", len(samples))
	}
	simple, _ := d["simple_group"].([]any)
	if len(simple) != 1 || simple[0] != "mock-eco" {
		t.Errorf("defaults: simple_group=%v, want [mock-eco]", simple)
	}
	complex, _ := d["complex_group"].([]any)
	if len(complex) != 1 || complex[0] != "mock-pro" {
		t.Errorf("defaults: complex_group=%v, want [mock-pro]", complex)
	}
	unresolved, _ := d["unresolved_models"].([]any)
	if len(unresolved) != 0 {
		t.Errorf("defaults: unresolved_models=%v, want [] (demo models exist)", unresolved)
	}

	// Employee may not read or change the policy.
	if code, _ := call("GET", "/api/auto-route", empSess, nil); code != 403 {
		t.Errorf("employee GET: got %d, want 403", code)
	}
	if code, _ := call("POST", "/api/auto-route", empSess, map[string]any{"enabled": false}); code != 403 {
		t.Errorf("employee POST: got %d, want 403", code)
	}
	if code, _ := call("GET", "/api/auto-route", "", nil); code != 401 {
		t.Errorf("anonymous GET: got %d, want 401", code)
	}

	// Validation: bad threshold, empty model name, both groups
	// empty while enabled.
	for _, bad := range []map[string]any{
		{"threshold": 1.5},
		{"simple_group": []string{""}},
		{"simple_group": []string{}, "complex_group": []string{}},
	} {
		if code, _ := call("POST", "/api/auto-route", adminSess, bad); code != 400 {
			t.Errorf("POST %v: got %d, want 400", bad, code)
		}
	}

	// Update groups: a model no channel serves is accepted but
	// reported as unresolved.
	code, d = call("POST", "/api/auto-route", adminSess,
		map[string]any{"simple_group": []string{"mock-eco", "mock-nope"}})
	if code != 200 {
		t.Fatalf("POST groups: got %d (%v)", code, d)
	}
	code, d = call("GET", "/api/auto-route", adminSess, nil)
	unresolved, _ = d["unresolved_models"].([]any)
	if len(unresolved) != 1 || unresolved[0] != "mock-nope" {
		t.Errorf("unresolved_models=%v, want [mock-nope]", unresolved)
	}
	if d["configured"] != true {
		t.Errorf("configured=%v after update, want true", d["configured"])
	}

	// Disable the layer.
	code, _ = call("POST", "/api/auto-route", adminSess, map[string]any{"enabled": false})
	if code != 200 {
		t.Fatalf("POST disable: got %d", code)
	}
	code, d = call("GET", "/api/auto-route", adminSess, nil)
	if d["enabled"] != false {
		t.Errorf("after disable: enabled=%v, want false", d["enabled"])
	}

	// Samples: add (bad tier / too short rejected), delete,
	// unknown ID 404.
	if code, _ := call("POST", "/api/auto-route/samples", adminSess,
		map[string]any{"tier": "medium", "text": "今天星期几？"}); code != 400 {
		t.Errorf("add bad tier: got %d, want 400", code)
	}
	if code, _ := call("POST", "/api/auto-route/samples", adminSess,
		map[string]any{"tier": "simple", "text": "你好"}); code != 400 {
		t.Errorf("add too short: got %d, want 400", code)
	}
	code, d = call("POST", "/api/auto-route/samples", adminSess,
		map[string]any{"tier": "simple", "text": "帮我查一下明天的天气怎么样"})
	if code != 201 {
		t.Fatalf("add sample: got %d (%v)", code, d)
	}
	id, _ := d["id"].(string)
	if id == "" {
		t.Fatal("add sample: no id returned")
	}
	if code, _ := call("POST", "/api/auto-route/samples", empSess,
		map[string]any{"tier": "simple", "text": "帮我查一下明天的天气怎么样"}); code != 403 {
		t.Errorf("employee add sample: got %d, want 403", code)
	}
	if code, _ := call("DELETE", "/api/auto-route/samples/does-not-exist", adminSess, nil); code != 404 {
		t.Errorf("delete unknown sample: got %d, want 404", code)
	}
	if code, _ := call("DELETE", "/api/auto-route/samples/"+id, adminSess, nil); code != 200 {
		t.Errorf("delete sample: got %d, want 200", code)
	}
	code, d = call("GET", "/api/auto-route", adminSess, nil)
	samples, _ = d["samples"].([]any)
	if len(samples) != 12 {
		t.Errorf("after add+delete: %d samples, want 12", len(samples))
	}
}
