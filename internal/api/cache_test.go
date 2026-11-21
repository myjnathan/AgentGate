package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/api"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/cache"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/seed"
	"github.com/myjnathan/agentgate/internal/store"
)

func setupCacheAPI(t *testing.T) (*httptest.Server, string, string) {
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
	rest.SetCache(cache.New(time.Hour, 0))
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
	return srv, login("admin", "agentgate-admin"), login("lixiaofeng", "demo12345")
}

func TestCacheEndpoints(t *testing.T) {
	srv, adminSess, empSess := setupCacheAPI(t)

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

	// Status: admin sees the defaults (enabled, 1h TTL, empty).
	code, d := call("GET", "/api/cache", adminSess, nil)
	if code != 200 {
		t.Fatalf("GET /api/cache = %d, want 200", code)
	}
	if d["enabled"] != true || d["ttl_seconds"] != float64(3600) ||
		d["max_entries"] != float64(1000) || d["entries"] != float64(0) ||
		d["hits"] != float64(0) || d["misses"] != float64(0) {
		t.Fatalf("cache status = %v, want enabled/3600/1000/0/0/0", d)
	}

	// Gating.
	if code, _ := call("GET", "/api/cache", empSess, nil); code != 403 {
		t.Fatalf("employee status: %d, want 403", code)
	}
	if code, _ := call("GET", "/api/cache", "", nil); code != 401 {
		t.Fatalf("anonymous status: %d, want 401", code)
	}

	// Toggle off, then back on.
	code, d = call("POST", "/api/cache", adminSess, map[string]any{"enabled": false})
	if code != 200 || d["enabled"] != false {
		t.Fatalf("toggle off = %d %v, want 200 enabled=false", code, d)
	}
	if _, d := call("GET", "/api/cache", adminSess, nil); d["enabled"] != false {
		t.Fatalf("status after toggle = %v, want enabled=false", d)
	}
	code, d = call("POST", "/api/cache", adminSess, map[string]any{"enabled": true})
	if code != 200 || d["enabled"] != true {
		t.Fatalf("toggle on = %d %v, want 200 enabled=true", code, d)
	}

	// Validation and gating.
	if code, _ := call("POST", "/api/cache", adminSess, map[string]any{}); code != 400 {
		t.Fatalf("missing enabled: %d, want 400", code)
	}
	if code, _ := call("POST", "/api/cache", empSess, map[string]any{"enabled": false}); code != 403 {
		t.Fatalf("employee toggle: %d, want 403", code)
	}

	// Clear.
	code, d = call("POST", "/api/cache/clear", adminSess, nil)
	if code != 200 || d["cleared"] != float64(0) {
		t.Fatalf("clear = %d %v, want 200 cleared=0", code, d)
	}
	if code, _ := call("POST", "/api/cache/clear", empSess, nil); code != 403 {
		t.Fatalf("employee clear: %d, want 403", code)
	}
}
