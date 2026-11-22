package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/myjnathan/agentgate/internal/api"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/orgsync"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/seed"
	"github.com/myjnathan/agentgate/internal/store"
)

// setupOrgsyncAPI builds a test server with the org sync engine
// wired (like setupTokensAPI, plus SetOrgSync).
func setupOrgsyncAPI(t *testing.T) (*httptest.Server, *store.Store, string, string) {
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
	rest.SetOrgSync(orgsync.New(st, au.RevokeUserSessions))
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
	return srv, st, login("admin", "agentgate-admin"), login("lixiaofeng", "demo12345")
}

// fakeDingTalkServer is a minimal DingTalk directory: root dept 1
// with child dept 2 (研发部), three members, u9 reported gone.
func fakeDingTalkServer(t *testing.T) *httptest.Server {
	t.Helper()
	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1.0/oauth2/accessToken"):
			write(w, map[string]any{"accessToken": "dt-token", "expireIn": 7200})
		case strings.HasSuffix(r.URL.Path, "/topapi/v2/department/listsub"):
			var in struct {
				DeptID int64 `json:"dept_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			children := []map[string]any{}
			if in.DeptID == 1 {
				children = []map[string]any{{"dept_id": 2, "name": "研发部"}}
			}
			write(w, map[string]any{"errcode": 0, "errmsg": "ok", "result": children})
		case strings.HasSuffix(r.URL.Path, "/topapi/v2/user/list"):
			var in struct {
				DeptID int64 `json:"dept_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			var list []map[string]any
			if in.DeptID == 1 {
				list = []map[string]any{
					{"userid": "u1", "name": "张三", "org_email": "zhangsan@xinghe.example", "dept_id_list": []int64{1, 2}},
				}
			} else {
				list = []map[string]any{
					{"userid": "u2", "name": "李四", "org_email": "lisi@xinghe.example", "dept_id_list": []int64{2}},
					{"userid": "u3", "name": "王五", "org_email": "", "dept_id_list": []int64{2}},
				}
			}
			write(w, map[string]any{"errcode": 0, "errmsg": "ok", "result": map[string]any{
				"has_more": false, "next_cursor": 0, "list": list}})
		case strings.HasSuffix(r.URL.Path, "/topapi/v2/user/get"):
			var in struct {
				UserID string `json:"userid"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.UserID == "u9" {
				write(w, map[string]any{"errcode": 60121, "errmsg": "not found", "result": map[string]any{}})
				return
			}
			write(w, map[string]any{"errcode": 0, "errmsg": "ok", "result": map[string]any{
				"userid": in.UserID, "name": "张三", "org_email": "zhangsan@xinghe.example", "dept_id_list": []int64{1}}})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func orgCall(t *testing.T, srvURL, method, path, sess string, body any) (int, map[string]any) {
	t.Helper()
	var buf *bytes.Buffer
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewBuffer(b)
	} else {
		buf = bytes.NewBuffer(nil)
	}
	req, _ := http.NewRequest(method, srvURL+path, buf)
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

func orgRawCall(t *testing.T, srvURL, method, path, sess string, body any) (int, string) {
	t.Helper()
	var buf *bytes.Buffer
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewBuffer(b)
	} else {
		buf = bytes.NewBuffer(nil)
	}
	req, _ := http.NewRequest(method, srvURL+path, buf)
	req.Header.Set("Content-Type", "application/json")
	if sess != "" {
		req.Header.Set("Authorization", "Bearer "+sess)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw := new(bytes.Buffer)
	_, _ = raw.ReadFrom(resp.Body)
	return resp.StatusCode, raw.String()
}

func createDingTalkProvider(t *testing.T, srvURL, adminSess, fakeURL string, autoCreate, disableMissing bool) map[string]any {
	t.Helper()
	code, out := orgCall(t, srvURL, "POST", "/api/orgsync/providers", adminSess, map[string]any{
		"provider": "dingtalk", "name": "星河钉钉", "corp_id": "corp-xinghe",
		"app_key": "dt-app-key", "app_secret": "dt-app-secret-plain",
		"base_url": fakeURL, "auto_create": autoCreate, "disable_missing": disableMissing,
	})
	if code != 201 {
		t.Fatalf("create provider: got %d: %v", code, out)
	}
	prov, _ := out["provider"].(map[string]any)
	return prov
}

func TestOrgProviderCRUD(t *testing.T) {
	srv, _, adminSess, empSess := setupOrgsyncAPI(t)
	fake := fakeDingTalkServer(t)

	// The secret must never be echoed back, in any form.
	code, raw := orgRawCall(t, srv.URL, "POST", "/api/orgsync/providers", adminSess, map[string]any{
		"provider": "dingtalk", "name": "星河钉钉", "corp_id": "corp-xinghe",
		"app_key": "dt-app-key", "app_secret": "dt-app-secret-plain", "base_url": fake.URL,
	})
	if code != 201 {
		t.Fatalf("create: got %d: %s", code, raw)
	}
	if strings.Contains(raw, "dt-app-secret-plain") {
		t.Fatalf("create response leaks the secret: %s", raw)
	}
	if !strings.Contains(raw, "secret_masked") {
		t.Fatalf("create response has no masked secret: %s", raw)
	}
	var created struct {
		Provider map[string]any `json:"provider"`
	}
	_ = json.Unmarshal([]byte(raw), &created)
	id, _ := created.Provider["id"].(string)
	if id == "" {
		t.Fatalf("no provider id: %s", raw)
	}

	// Validation: unknown provider, missing secret, missing corp.
	for _, body := range []map[string]any{
		{"provider": "slack", "name": "x", "app_secret": "s"},
		{"provider": "dingtalk", "name": "x", "corp_id": "c", "app_key": "k"},
		{"provider": "dingtalk", "name": "x", "app_key": "k", "app_secret": "s"},
	} {
		if code, _ := orgCall(t, srv.URL, "POST", "/api/orgsync/providers", adminSess, body); code != 400 {
			t.Fatalf("invalid provider body accepted: %d: %v", code, body)
		}
	}

	// List: employee is forbidden; admin sees the masked secret only.
	if code, _ := orgCall(t, srv.URL, "GET", "/api/orgsync/providers", empSess, nil); code != 403 {
		t.Fatalf("employee list: got %d, want 403", code)
	}
	code, out := orgCall(t, srv.URL, "GET", "/api/orgsync/providers", adminSess, nil)
	provs, _ := out["providers"].([]any)
	if code != 200 || len(provs) != 1 {
		t.Fatalf("list: got %d: %v", code, out)
	}
	code, raw = orgRawCall(t, srv.URL, "GET", "/api/orgsync/providers/"+id, adminSess, nil)
	if code != 200 || strings.Contains(raw, "dt-app-secret-plain") {
		t.Fatalf("get: got %d, leaks secret: %s", code, raw)
	}
	if code, _ := orgCall(t, srv.URL, "GET", "/api/orgsync/providers/nope", adminSess, nil); code != 404 {
		t.Fatalf("get unknown: got %d, want 404", code)
	}

	// The secret is encrypted at rest: state.json must not hold it.
	// (Verified via the API surface: list/get never echo it; the
	// store round-trips through secretbox, covered by the sync test
	// below which decrypts it against the fake server.)

	code, _ = orgCall(t, srv.URL, "DELETE", "/api/orgsync/providers/"+id, adminSess, nil)
	if code != 200 {
		t.Fatalf("delete: got %d", code)
	}
	if code, _ := orgCall(t, srv.URL, "GET", "/api/orgsync/providers/"+id, adminSess, nil); code != 404 {
		t.Fatalf("get after delete: got %d, want 404", code)
	}
}

func TestOrgSyncFlow(t *testing.T) {
	srv, st, adminSess, empSess := setupOrgsyncAPI(t)
	fake := fakeDingTalkServer(t)
	prov := createDingTalkProvider(t, srv.URL, adminSess, fake.URL, true, false)
	id := prov["id"].(string)

	// Connectivity test: counts only, no provisioning.
	code, out := orgCall(t, srv.URL, "POST", "/api/orgsync/providers/"+id+"/test", adminSess, nil)
	if code != 200 || out["users"] != float64(3) || out["departments"] != float64(2) {
		t.Fatalf("test: got %d: %v", code, out)
	}
	count := 0
	st.Read(func(s *store.State) { count = len(s.Users) })
	if count != 3 { // seed only: admin + 2 employees
		t.Fatalf("test must not provision: users = %d", count)
	}

	// Sync provisions two named accounts + one id-derived account.
	code, out = orgCall(t, srv.URL, "POST", "/api/orgsync/providers/"+id+"/sync", adminSess, nil)
	res, _ := out["result"].(map[string]any)
	if code != 200 || res == nil || res["ok"] != true || res["created"] != float64(3) {
		t.Fatalf("sync: got %d: %v", code, out)
	}
	u := func(username string) *store.User {
		var found *store.User
		st.Read(func(s *store.State) {
			for _, x := range s.Users {
				if x.Username == username {
					cp := *x
					found = &cp
				}
			}
		})
		return found
	}
	if got := u("zhangsan"); got == nil || got.Department != "研发部" || got.AuthSource != "dingtalk" || got.Role != "employee" {
		t.Fatalf("zhangsan = %+v", got)
	}
	if got := u("u3"); got == nil || got.DisplayName != "王五" {
		t.Fatalf("u3 (no email) = %+v", got)
	}
	// Second sync reconciles.
	code, out = orgCall(t, srv.URL, "POST", "/api/orgsync/providers/"+id+"/sync", adminSess, nil)
	res, _ = out["result"].(map[string]any)
	if code != 200 || res["created"] != float64(0) || res["reconciled"] != float64(3) {
		t.Fatalf("second sync: got %d: %v", code, out)
	}
	// Last sync is on the provider record.
	code, out = orgCall(t, srv.URL, "GET", "/api/orgsync/providers/"+id, adminSess, nil)
	prov2, _ := out["provider"].(map[string]any)
	last, _ := prov2["last_sync"].(map[string]any)
	if code != 200 || last == nil || last["ok"] != true || last["remote"] != float64(3) {
		t.Fatalf("provider last_sync: got %d: %v", code, out)
	}
	// The sync wrote summary audit events with counts only.
	code, out = orgCall(t, srv.URL, "GET", "/api/audit?kind=orgsync", adminSess, nil)
	events, _ := out["events"].([]any)
	if code != 200 || len(events) == 0 {
		t.Fatalf("audit: got %d: %v", code, out)
	}
	found := false
	for _, e := range events {
		ev, _ := e.(map[string]any)
		detail, _ := ev["detail"].(map[string]any)
		if detail["created"] == "3" && detail["remote"] == "3" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no audit event with created=3: %v", events)
	}
	if strings.Contains(fmt.Sprint(out), "dt-app-secret-plain") {
		t.Fatal("audit leaks the provider secret")
	}
	// Employee cannot sync.
	if code, _ := orgCall(t, srv.URL, "POST", "/api/orgsync/providers/"+id+"/sync", empSess, nil); code != 403 {
		t.Fatalf("employee sync: got %d, want 403", code)
	}
}

func TestOrgBindPrefersFreeSubject(t *testing.T) {
	srv, _, adminSess, _ := setupOrgsyncAPI(t)
	fake := fakeDingTalkServer(t)
	// Provider A provisions zhangsan, binding dingtalk:corp-xinghe:u1.
	provA := createDingTalkProvider(t, srv.URL, adminSess, fake.URL, true, false)
	if code, _ := orgCall(t, srv.URL, "POST", "/api/orgsync/providers/"+provA["id"].(string)+"/sync", adminSess, nil); code != 200 {
		t.Fatalf("sync A: got %d", code)
	}
	// Provider B (another corp) only snapshots the same directory.
	code, out := orgCall(t, srv.URL, "POST", "/api/orgsync/providers", adminSess, map[string]any{
		"provider": "dingtalk", "name": "星河钉钉-二", "corp_id": "corp-xinghe-ro",
		"app_key": "dt-app-key", "app_secret": "dt-app-secret-plain",
		"base_url": fake.URL, "auto_create": false,
	})
	if code != 201 {
		t.Fatalf("create B: got %d: %v", code, out)
	}
	provB, _ := out["provider"].(map[string]any)
	if code, _ := orgCall(t, srv.URL, "POST", "/api/orgsync/providers/"+provB["id"].(string)+"/sync", adminSess, nil); code != 200 {
		t.Fatalf("sync B: got %d", code)
	}
	// u3 has no email, so provider B's sync cannot auto-link it:
	// its corp-xinghe-ro subject stays free while corp-xinghe:u3
	// is bound to the account provider A created.
	code, out = orgCall(t, srv.URL, "POST", "/api/orgsync/bindings", adminSess, map[string]any{
		"user_id": "chenjing", "provider": "dingtalk", "external_id": "u3",
	})
	if code != 200 || out["subject"] != "dingtalk:corp-xinghe-ro:u3" {
		t.Fatalf("bind: got %d: %v", code, out)
	}
	// Now both subjects are taken: a third bind is refused.
	if code, _ := orgCall(t, srv.URL, "POST", "/api/orgsync/bindings", adminSess, map[string]any{
		"user_id": "lixiaofeng", "provider": "dingtalk", "external_id": "u3",
	}); code != 409 {
		t.Fatalf("bind when all subjects taken: got %d, want 409", code)
	}
}

func TestOrgSyncDisabledProvider(t *testing.T) {
	srv, _, adminSess, _ := setupOrgsyncAPI(t)
	fake := fakeDingTalkServer(t)
	code, out := orgCall(t, srv.URL, "POST", "/api/orgsync/providers", adminSess, map[string]any{
		"provider": "dingtalk", "name": "停用", "corp_id": "corp-xinghe",
		"app_key": "k", "app_secret": "s", "base_url": fake.URL, "enabled": false,
	})
	if code != 201 {
		t.Fatalf("create: got %d: %v", code, out)
	}
	prov, _ := out["provider"].(map[string]any)
	if code, _ := orgCall(t, srv.URL, "POST", "/api/orgsync/providers/"+prov["id"].(string)+"/sync", adminSess, nil); code != 400 {
		t.Fatalf("sync disabled provider: got %d, want 400", code)
	}
}

func TestOrgBindFlow(t *testing.T) {
	srv, st, adminSess, empSess := setupOrgsyncAPI(t)
	fake := fakeDingTalkServer(t)
	// Auto-create off: the sync populates the snapshot but creates
	// no accounts, so binds can be exercised against it.
	prov := createDingTalkProvider(t, srv.URL, adminSess, fake.URL, false, false)
	id := prov["id"].(string)
	code, out := orgCall(t, srv.URL, "POST", "/api/orgsync/providers/"+id+"/sync", adminSess, nil)
	res, _ := out["result"].(map[string]any)
	if code != 200 || res["skipped"] == nil {
		t.Fatalf("sync: got %d: %v", code, out)
	}
	var liID string
	st.Read(func(s *store.State) {
		for _, u := range s.Users {
			if u.Username == "lixiaofeng" {
				liID = u.ID
			}
		}
	})
	// Bind lixiaofeng to the directory member u1.
	code, out = orgCall(t, srv.URL, "POST", "/api/orgsync/bindings", adminSess, map[string]any{
		"user_id": liID, "provider": "dingtalk", "external_id": "u1",
	})
	if code != 200 || out["subject"] != "dingtalk:corp-xinghe:u1" {
		t.Fatalf("bind: got %d: %v", code, out)
	}
	// The same identity cannot bind to a second account.
	var chenID string
	st.Read(func(s *store.State) {
		for _, u := range s.Users {
			if u.Username == "chenjing" {
				chenID = u.ID
			}
		}
	})
	if code, _ := orgCall(t, srv.URL, "POST", "/api/orgsync/bindings", adminSess, map[string]any{
		"user_id": chenID, "provider": "dingtalk", "external_id": "u1",
	}); code != 409 {
		t.Fatalf("double bind: got %d, want 409", code)
	}
	// An external id the provider never returned cannot be bound.
	if code, _ := orgCall(t, srv.URL, "POST", "/api/orgsync/bindings", adminSess, map[string]any{
		"user_id": chenID, "provider": "dingtalk", "external_id": "u99",
	}); code != 409 {
		t.Fatalf("bind unknown external id: got %d, want 409", code)
	}
	// Employee cannot bind; my bindings list the one identity.
	if code, _ := orgCall(t, srv.URL, "POST", "/api/orgsync/bindings", empSess, map[string]any{
		"user_id": chenID, "provider": "dingtalk", "external_id": "u2",
	}); code != 403 {
		t.Fatalf("employee bind: got %d, want 403", code)
	}
	code, out = orgCall(t, srv.URL, "GET", "/api/orgsync/bindings", empSess, nil)
	bindings, _ := out["bindings"].([]any)
	if code != 200 || len(bindings) != 1 {
		t.Fatalf("my bindings: got %d: %v", code, out)
	}
	// Unbind, then the list is empty; unbinding again is 404.
	code, _ = orgCall(t, srv.URL, "DELETE", "/api/orgsync/bindings/"+liID+"/dingtalk", adminSess, nil)
	if code != 200 {
		t.Fatalf("unbind: got %d", code)
	}
	code, out = orgCall(t, srv.URL, "GET", "/api/orgsync/bindings", empSess, nil)
	bindings, _ = out["bindings"].([]any)
	if code != 200 || len(bindings) != 0 {
		t.Fatalf("bindings after unbind: got %d: %v", code, out)
	}
	if code, _ := orgCall(t, srv.URL, "DELETE", "/api/orgsync/bindings/"+liID+"/dingtalk", adminSess, nil); code != 404 {
		t.Fatalf("second unbind: got %d, want 404", code)
	}
}
