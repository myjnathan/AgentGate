package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/alert"
	"github.com/myjnathan/agentgate/internal/api"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/seed"
	"github.com/myjnathan/agentgate/internal/store"
)

// alertTestServer is exportTestServer plus an alert dispatcher wired
// into the API, so the webhook endpoints behave as in production.
func alertTestServer(t *testing.T) (*httptest.Server, string, string, *store.Store, *secretbox.Box) {
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
	d := alert.New(st, box, alert.WithBackoff([]time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}))
	rest := api.New(st, au, box, al)
	rest.SetAlertDispatcher(d)
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
		var dd struct {
			Session string `json:"session"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&dd)
		if resp.StatusCode != 200 || dd.Session == "" {
			t.Fatalf("login %s failed: %d", username, resp.StatusCode)
		}
		return dd.Session
	}
	return srv, login("admin", "agentgate-admin"), login("lixiaofeng", "demo12345"), st, box
}

func callJSON(t *testing.T, method, url, sess string, in any) (int, []byte) {
	t.Helper()
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, body)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if sess != "" {
		req.Header.Set("Authorization", "Bearer "+sess)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestWebhookCRUD(t *testing.T) {
	srv, admin, emp, st, box := alertTestServer(t)

	// Unauthenticated and employee are refused.
	if code, _ := callJSON(t, "GET", srv.URL+"/api/alerts/webhooks", "", nil); code != 401 {
		t.Fatalf("unauthenticated list = %d, want 401", code)
	}
	if code, _ := callJSON(t, "GET", srv.URL+"/api/alerts/webhooks", emp, nil); code != 403 {
		t.Fatalf("employee list = %d, want 403", code)
	}
	if code, _ := callJSON(t, "POST", srv.URL+"/api/alerts/webhooks", emp,
		map[string]any{"name": "x", "url": "https://example.com/hook", "kind": "generic"}); code != 403 {
		t.Fatalf("employee create = %d, want 403", code)
	}

	// Validation.
	for _, in := range []map[string]any{
		{"name": "", "url": "https://example.com/hook", "kind": "generic"},
		{"name": "x", "url": "https://example.com/hook", "kind": "slack"},
		{"name": "x", "url": "http://example.com/hook", "kind": "generic"},
	} {
		if code, body := callJSON(t, "POST", srv.URL+"/api/alerts/webhooks", admin, in); code != 400 {
			t.Fatalf("invalid create = %d (%s), want 400", code, body)
		}
	}

	// Create with a signing secret: stored encrypted, never returned.
	code, body := callJSON(t, "POST", srv.URL+"/api/alerts/webhooks", admin,
		map[string]any{"name": "安全群", "url": "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=abc123",
			"kind": "wecom", "secret": "SECtestsecret"})
	if code != 201 {
		t.Fatalf("create = %d (%s), want 201", code, body)
	}
	var created struct {
		ID        string `json:"id"`
		HasSecret bool   `json:"has_secret"`
	}
	_ = json.Unmarshal(body, &created)
	if created.ID == "" || !created.HasSecret {
		t.Fatalf("unexpected create response: %s", body)
	}
	if strings.Contains(string(body), "SECtestsecret") {
		t.Fatalf("create response leaks the secret: %s", body)
	}
	st.Read(func(s *store.State) {
		wh := s.Webhooks[created.ID]
		if wh == nil || wh.SecretEnc == "" {
			t.Fatal("webhook not stored")
		}
		plain, err := box.Decrypt(wh.SecretEnc)
		if err != nil || plain != "SECtestsecret" {
			t.Fatalf("stored secret does not decrypt to the original: %v", err)
		}
	})

	// List masks the URL query and never exposes the secret.
	code, body = callJSON(t, "GET", srv.URL+"/api/alerts/webhooks", admin, nil)
	if code != 200 {
		t.Fatalf("list = %d, want 200", code)
	}
	if strings.Contains(string(body), "SECtestsecret") || strings.Contains(string(body), "secret_enc") || strings.Contains(string(body), "key=abc123") {
		t.Fatalf("list leaks secret or bot key: %s", body)
	}
	var listed struct {
		Webhooks []struct {
			URL string `json:"url"`
		} `json:"webhooks"`
	}
	_ = json.Unmarshal(body, &listed)
	if len(listed.Webhooks) != 1 || listed.Webhooks[0].URL != "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=%2A%2A%2A" {
		t.Fatalf("list should mask the bot key: %s", body)
	}

	// Delete, then deleting again is 404.
	code, _ = callJSON(t, "DELETE", srv.URL+"/api/alerts/webhooks/"+created.ID, admin, nil)
	if code != 200 {
		t.Fatalf("delete = %d, want 200", code)
	}
	code, _ = callJSON(t, "DELETE", srv.URL+"/api/alerts/webhooks/"+created.ID, admin, nil)
	if code != 404 {
		t.Fatalf("second delete = %d, want 404", code)
	}
}

func TestWebhookTestEndpoint(t *testing.T) {
	srv, admin, _, _, _ := alertTestServer(t)

	var mu sync.Mutex
	var bodies []string
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer recv.Close()

	code, body := callJSON(t, "POST", srv.URL+"/api/alerts/webhooks", admin,
		map[string]any{"name": "receiver", "url": recv.URL, "kind": "generic"})
	if code != 201 {
		t.Fatalf("create = %d (%s), want 201", code, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &created)

	code, body = callJSON(t, "POST", srv.URL+"/api/alerts/webhooks/"+created.ID+"/test", admin, nil)
	if code != 200 {
		t.Fatalf("test = %d (%s), want 200", code, body)
	}
	var res struct {
		Delivered bool `json:"delivered"`
		Attempts  int  `json:"attempts"`
	}
	_ = json.Unmarshal(body, &res)
	if !res.Delivered || res.Attempts != 1 {
		t.Fatalf("unexpected test result: %s", body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 || !strings.Contains(bodies[0], "测试告警") {
		t.Fatalf("receiver got %d bodies, want 1 test alert: %v", len(bodies), bodies)
	}

	// Unknown webhook is 404.
	code, _ = callJSON(t, "POST", srv.URL+"/api/alerts/webhooks/wh_missing/test", admin, nil)
	if code != 404 {
		t.Fatalf("test missing = %d, want 404", code)
	}

	// A failing receiver: 4 attempts (1 + 3 retries), then 502 with
	// only an error class, never the URL.
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer fail.Close()
	code, body = callJSON(t, "POST", srv.URL+"/api/alerts/webhooks", admin,
		map[string]any{"name": "failing", "url": fail.URL, "kind": "generic"})
	_ = json.Unmarshal(body, &created)
	code, body = callJSON(t, "POST", srv.URL+"/api/alerts/webhooks/"+created.ID+"/test", admin, nil)
	if code != 502 {
		t.Fatalf("test failing = %d (%s), want 502", code, body)
	}
	_ = json.Unmarshal(body, &res)
	if res.Delivered || res.Attempts != 4 {
		t.Fatalf("unexpected failing test result: %s", body)
	}
	if !strings.Contains(string(body), "http_503") || strings.Contains(string(body), fail.URL) {
		t.Fatalf("failing test must report only the error class: %s", body)
	}
}

func TestLoginLockoutAuditedAsAccountLocked(t *testing.T) {
	srv, admin, _, _, _ := alertTestServer(t)
	// 20 wrong passwords, then the 21st must hit the lockout window.
	for i := 0; i < 21; i++ {
		code, _ := callJSON(t, "POST", srv.URL+"/api/login", "",
			map[string]any{"username": "lixiaofeng", "password": "wrong-password"})
		if code != 401 {
			t.Fatalf("attempt %d: login = %d, want 401", i+1, code)
		}
	}
	code, body := callJSON(t, "GET", srv.URL+"/api/audit?kind=auth", admin, nil)
	if code != 200 {
		t.Fatalf("audit query = %d, want 200", code)
	}
	if !strings.Contains(string(body), "account_locked") {
		t.Fatalf("audit log must contain an account_locked event: %s", body)
	}
}
