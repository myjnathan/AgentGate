package orgsync_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/myjnathan/agentgate/internal/orgsync"
	"github.com/myjnathan/agentgate/internal/store"
)

// ---- fake DingTalk server ----

type fakeDingTalk struct {
	usersByDept map[string][][]map[string]any // dept id -> pages -> users
	children    map[string][]map[string]any
	lookup      map[string]map[string]any // userid -> user object, or {"errcode": N}
	tokenCalls  int
}

func (f *fakeDingTalk) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	write := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/v1.0/oauth2/accessToken"):
		f.tokenCalls++
		write(map[string]any{"accessToken": "dt-token", "expireIn": 7200})
	case strings.HasSuffix(r.URL.Path, "/topapi/v2/department/listsub"):
		var in struct {
			DeptID int64 `json:"dept_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		key := fmt.Sprint(in.DeptID)
		write(map[string]any{"errcode": 0, "errmsg": "ok", "result": f.children[key]})
	case strings.HasSuffix(r.URL.Path, "/topapi/v2/user/list"):
		var in struct {
			DeptID int64 `json:"dept_id"`
			Cursor int64 `json:"cursor"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		pages := f.usersByDept[fmt.Sprint(in.DeptID)]
		page := pages[in.Cursor]
		hasMore := in.Cursor+1 < int64(len(pages))
		write(map[string]any{"errcode": 0, "errmsg": "ok", "result": map[string]any{
			"has_more": hasMore, "next_cursor": in.Cursor + 1, "list": page}})
	case strings.HasSuffix(r.URL.Path, "/topapi/v2/user/get"):
		var in struct {
			UserID string `json:"userid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if u, ok := f.lookup[in.UserID]; ok {
			if code, isErr := u["errcode"]; isErr {
				write(map[string]any{"errcode": code, "errmsg": "biz error", "result": map[string]any{}})
				return
			}
			write(map[string]any{"errcode": 0, "errmsg": "ok", "result": u})
			return
		}
		write(map[string]any{"errcode": 60121, "errmsg": "user not found", "result": map[string]any{}})
	default:
		w.WriteHeader(404)
	}
}

func dingTalkUser(id, name, email string, depts ...int64) map[string]any {
	return map[string]any{"userid": id, "name": name, "org_email": email, "dept_id_list": depts}
}

func newFakeDingTalk() (*httptest.Server, *fakeDingTalk) {
	f := &fakeDingTalk{
		usersByDept: map[string][][]map[string]any{
			"1": {{dingTalkUser("u1", "张三", "zhangsan@xinghe.example", 1, 2)}, {dingTalkUser("u2", "李四", "lisi@xinghe.example", 2)}},
			"2": {{dingTalkUser("u2", "李四", "lisi@xinghe.example", 2), dingTalkUser("u3", "王五", "", 2)}},
		},
		children: map[string][]map[string]any{
			"1": {{"dept_id": 2, "name": "研发部"}},
			"2": {},
		},
		lookup: map[string]map[string]any{
			"u1": dingTalkUser("u1", "张三", "zhangsan@xinghe.example", 1, 2),
			"u9": {"errcode": 60121},
			"u8": {"errcode": 50002},
		},
	}
	srv := httptest.NewServer(f)
	return srv, f
}

func dtConfig(srvURL string) orgsync.Config {
	return orgsync.Config{
		Provider: orgsync.ProviderDingTalk, CorpID: "corp-xinghe",
		AppKey: "dt-app-key", AppSecret: "dt-app-secret", BaseURL: srvURL,
	}
}

func TestDingTalkClientDirectory(t *testing.T) {
	srv, f := newFakeDingTalk()
	defer srv.Close()
	c, err := orgsync.NewClient(dtConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := c.Directory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dir.Departments["2"] != "研发部" {
		t.Fatalf("dept 2 = %q", dir.Departments["2"])
	}
	if len(dir.Users) != 3 {
		t.Fatalf("users = %d, want 3 (u2 deduped across departments)", len(dir.Users))
	}
	// Sorted by external id; u3 has no email.
	if dir.Users[0].ExternalID != "u1" || dir.Users[1].ExternalID != "u2" || dir.Users[2].ExternalID != "u3" {
		t.Fatalf("order = %v", dir.Users)
	}
	if dir.Users[2].Email != "" {
		t.Fatalf("u3 email = %q, want empty", dir.Users[2].Email)
	}
	if dir.Users[0].Email != "zhangsan@xinghe.example" {
		t.Fatalf("u1 email = %q", dir.Users[0].Email)
	}
	if f.tokenCalls != 1 {
		t.Fatalf("token fetched %d times, want 1 (cached)", f.tokenCalls)
	}
}

func TestDingTalkClientError(t *testing.T) {
	srv, _ := newFakeDingTalk()
	defer srv.Close()
	// Point at a server that returns an oapi business error.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/v1.0/oauth2/accessToken") {
			_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "dt-token", "expireIn": 7200})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 50004, "errmsg": "dept out of scope dt-token", "result": nil})
	}))
	defer bad.Close()
	c, _ := orgsync.NewClient(dtConfig(bad.URL))
	_, err := c.Directory(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "dt-token") {
		t.Fatalf("error leaks the token: %v", err)
	}
	_ = srv
}

func TestDingTalkUserState(t *testing.T) {
	srv, _ := newFakeDingTalk()
	defer srv.Close()
	c, _ := orgsync.NewClient(dtConfig(srv.URL))
	ctx := context.Background()
	if st, err := c.UserState(ctx, "u1"); err != nil || st != orgsync.UserActive {
		t.Fatalf("u1: state=%v err=%v", st, err)
	}
	if st, err := c.UserState(ctx, "u9"); err != nil || st != orgsync.UserGone {
		t.Fatalf("u9: state=%v err=%v", st, err)
	}
	if st, err := c.UserState(ctx, "u8"); err != nil || st != orgsync.UserOutOfScope {
		t.Fatalf("u8: state=%v err=%v", st, err)
	}
}

// ---- fake Feishu server ----

type fakeFeishu struct {
	tokenCalls int
}

func (f *fakeFeishu) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	write := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/open-apis/auth/v3/tenant_access_token/internal"):
		f.tokenCalls++
		write(map[string]any{"code": 0, "msg": "ok", "tenant_access_token": "fs-token", "expire": 7200})
	case strings.Contains(r.URL.Path, "/contact/v3/departments/parentchildren"):
		dept := r.URL.Query().Get("department_id")
		items := []map[string]any{}
		if dept == "0" {
			items = []map[string]any{{"open_department_id": "d1", "name": "研发部"}}
		}
		write(map[string]any{"code": 0, "msg": "ok", "data": map[string]any{
			"has_more": false, "page_token": "", "items": items}})
	case strings.Contains(r.URL.Path, "/contact/v3/users/find_by_department"):
		dept := r.URL.Query().Get("department_id")
		token := r.URL.Query().Get("page_token")
		var items []map[string]any
		var hasMore bool
		var next string
		switch dept {
		case "0":
			if token == "" {
				items = []map[string]any{{"open_id": "ou1", "name": "赵六", "email": "personal@example.com", "enterprise_email": "zhaoliu@xinghe.example", "department_ids": []string{"d1"}}}
				hasMore, next = true, "p2"
			} else {
				items = []map[string]any{{"open_id": "ou2", "name": "孙七", "email": "sunqi@xinghe.example", "department_ids": []string{"d1"}}}
			}
		case "d1":
			items = []map[string]any{{"open_id": "ou1", "name": "赵六", "email": "personal@example.com", "enterprise_email": "zhaoliu@xinghe.example", "department_ids": []string{"d1"}}}
		}
		write(map[string]any{"code": 0, "msg": "ok", "data": map[string]any{
			"has_more": hasMore, "page_token": next, "items": items}})
	case strings.Contains(r.URL.Path, "/contact/v3/users/ou1"):
		write(map[string]any{"code": 0, "msg": "ok", "data": map[string]any{
			"open_id": "ou1", "name": "赵六", "enterprise_email": "zhaoliu@xinghe.example", "department_ids": []string{"d1"}}})
	case strings.Contains(r.URL.Path, "/contact/v3/users/ou9"):
		write(map[string]any{"code": 41012, "msg": "user not found", "data": nil})
	case strings.Contains(r.URL.Path, "/contact/v3/users/ou8"):
		write(map[string]any{"code": 41050, "msg": "no authority", "data": nil})
	default:
		w.WriteHeader(404)
	}
}

func TestFeishuClientDirectory(t *testing.T) {
	srv := httptest.NewServer(&fakeFeishu{})
	defer srv.Close()
	c, err := orgsync.NewClient(orgsync.Config{
		Provider: orgsync.ProviderFeishu, AppKey: "fs-app-id",
		AppSecret: "fs-secret", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := c.Directory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dir.Departments["d1"] != "研发部" {
		t.Fatalf("dept d1 = %q", dir.Departments["d1"])
	}
	if len(dir.Users) != 2 {
		t.Fatalf("users = %d, want 2", len(dir.Users))
	}
	// Enterprise email wins over the personal one.
	if dir.Users[0].Email != "zhaoliu@xinghe.example" {
		t.Fatalf("ou1 email = %q", dir.Users[0].Email)
	}
	if st, err := c.UserState(context.Background(), "ou9"); err != nil || st != orgsync.UserGone {
		t.Fatalf("ou9: state=%v err=%v", st, err)
	}
	if st, err := c.UserState(context.Background(), "ou8"); err != nil || st != orgsync.UserOutOfScope {
		t.Fatalf("ou8: state=%v err=%v", st, err)
	}
}

// ---- fake WeCom server ----

type fakeWeCom struct {
	tokenCalls int
}

func (f *fakeWeCom) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	write := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/cgi-bin/gettoken"):
		f.tokenCalls++
		write(map[string]any{"errcode": 0, "errmsg": "ok", "access_token": "wx-token", "expires_in": 7200})
	case strings.HasSuffix(r.URL.Path, "/cgi-bin/department/list"):
		write(map[string]any{"errcode": 0, "errmsg": "ok", "department": []map[string]any{
			{"id": 1, "name": "星河精密", "parentid": 0},
			{"id": 2, "name": "研发部", "parentid": 1},
		}})
	case strings.HasSuffix(r.URL.Path, "/cgi-bin/user/list"):
		write(map[string]any{"errcode": 0, "errmsg": "ok", "userlist": []map[string]any{
			{"userid": "w1", "name": "周八", "email": "zhouba@xinghe.example", "department": []int{2}},
			{"userid": "w2", "name": "吴九", "email": "", "department": []int{2}},
		}})
	case strings.HasSuffix(r.URL.Path, "/cgi-bin/user/get"):
		id := r.URL.Query().Get("userid")
		switch id {
		case "w1":
			write(map[string]any{"errcode": 0, "errmsg": "ok", "userid": "w1", "name": "周八", "email": "zhouba@xinghe.example", "department": []int{2}})
		case "w9":
			write(map[string]any{"errcode": 60111, "errmsg": "userid not found"})
		default:
			write(map[string]any{"errcode": 60111, "errmsg": "userid not found"})
		}
	default:
		w.WriteHeader(404)
	}
}

func TestWeComClientDirectory(t *testing.T) {
	f := &fakeWeCom{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c, err := orgsync.NewClient(orgsync.Config{
		Provider: orgsync.ProviderWeCom, CorpID: "wx-corp",
		AppSecret: "wx-secret", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := c.Directory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dir.Departments["2"] != "研发部" {
		t.Fatalf("dept 2 = %q", dir.Departments["2"])
	}
	if len(dir.Users) != 2 || dir.Users[0].ExternalID != "w1" {
		t.Fatalf("users = %v", dir.Users)
	}
	if f.tokenCalls != 1 {
		t.Fatalf("token fetched %d times, want 1 (cached)", f.tokenCalls)
	}
	if st, err := c.UserState(context.Background(), "w9"); err != nil || st != orgsync.UserGone {
		t.Fatalf("w9: state=%v err=%v", st, err)
	}
}

// ---- config validation ----

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		cfg orgsync.Config
		ok  bool
	}{
		{orgsync.Config{Provider: "dingtalk", CorpID: "c", AppKey: "k", AppSecret: "s"}, true},
		{orgsync.Config{Provider: "dingtalk", AppKey: "k", AppSecret: "s"}, false}, // no corp
		{orgsync.Config{Provider: "feishu", AppKey: "k", AppSecret: "s"}, true},    // no corp needed
		{orgsync.Config{Provider: "wecom", CorpID: "c", AppSecret: "s"}, true},
		{orgsync.Config{Provider: "dingtalk", CorpID: "c", AppKey: "k"}, false}, // no secret
		{orgsync.Config{Provider: "dingtalk", CorpID: "c", AppKey: "k", AppSecret: "s", BaseURL: "ftp://x"}, false},
		{orgsync.Config{Provider: "other", AppSecret: "s"}, false},
	}
	for i, tc := range cases {
		if err := tc.cfg.Validate(); (err == nil) != tc.ok {
			t.Errorf("case %d: validate err=%v, want ok=%v", i, err, tc.ok)
		}
	}
	if _, ok := orgsync.ParseProvider("DingTalk"); !ok {
		t.Error("ParseProvider should be case-insensitive")
	}
	if _, ok := orgsync.ParseProvider("slack"); ok {
		t.Error("ParseProvider accepted an unknown provider")
	}
}

// ---- sync engine ----

type stubClient struct {
	dir      *orgsync.Directory
	dirErr   error
	states   map[string]orgsync.UserState
	stateErr error
}

func (s *stubClient) Directory(ctx context.Context) (*orgsync.Directory, error) {
	if s.dirErr != nil {
		return nil, s.dirErr
	}
	return s.dir, nil
}

func (s *stubClient) UserState(ctx context.Context, id string) (orgsync.UserState, error) {
	if s.stateErr != nil {
		return orgsync.UserActive, s.stateErr
	}
	if st, ok := s.states[id]; ok {
		return st, nil
	}
	return orgsync.UserActive, nil
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func syncCfg() orgsync.Config {
	return orgsync.Config{
		Provider: orgsync.ProviderDingTalk, CorpID: "corp-xinghe",
		AppKey: "k", AppSecret: "s", AutoCreate: true,
	}
}

func findUser(t *testing.T, st *store.Store, username string) *store.User {
	t.Helper()
	var found *store.User
	st.Read(func(s *store.State) {
		for _, u := range s.Users {
			if u.Username == username {
				cp := *u
				found = &cp
			}
		}
	})
	if found == nil {
		t.Fatalf("user %q not found", username)
	}
	return found
}

func TestSyncProvisionsAndReconciles(t *testing.T) {
	st := newTestStore(t)
	syncer := orgsync.New(st, nil)
	dir := &orgsync.Directory{
		Departments: map[string]string{"1": "星河精密", "2": "研发部"},
		Users: []orgsync.DirectoryUser{
			{ExternalID: "u1", Name: "张三", Email: "zhangsan@xinghe.example", DeptIDs: []string{"2"}},
			{ExternalID: "u2", Name: "李四", Email: "lisi@xinghe.example", DeptIDs: []string{"2"}},
		},
	}
	res, err := syncer.Sync(context.Background(), "org_1", syncCfg(), &stubClient{dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Created != 2 || res.Remote != 2 {
		t.Fatalf("res = %+v", res)
	}
	u1 := findUser(t, st, "zhangsan")
	if u1.Role != "employee" || u1.AuthSource != "dingtalk" || u1.Department != "研发部" {
		t.Fatalf("u1 = %+v", u1)
	}
	if u1.PasswordHash != "" {
		t.Fatal("synced account must have no local password")
	}
	if len(u1.ExternalIdentities) != 1 || u1.ExternalIdentities[0].Subject != "dingtalk:corp-xinghe:u1" {
		t.Fatalf("u1 identities = %+v", u1.ExternalIdentities)
	}
	// Second sync with a renamed member reconciles instead of creating.
	dir.Users[0].Name = "张三丰"
	res, err = syncer.Sync(context.Background(), "org_1", syncCfg(), &stubClient{dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Created != 0 || res.Reconciled != 2 {
		t.Fatalf("second sync res = %+v", res)
	}
	if got := findUser(t, st, "zhangsan"); got.DisplayName != "张三丰" {
		t.Fatalf("display name not refreshed: %q", got.DisplayName)
	}
	// The snapshot feeds admin binds.
	snap, ok := syncer.Snapshot("org_1")
	if !ok || snap["u1"].Name != "张三丰" {
		t.Fatalf("snapshot = %v, ok=%v", snap, ok)
	}
}

func TestSyncAutoCreateOffSkips(t *testing.T) {
	st := newTestStore(t)
	syncer := orgsync.New(st, nil)
	cfg := syncCfg()
	cfg.AutoCreate = false
	dir := &orgsync.Directory{
		Departments: map[string]string{"2": "研发部"},
		Users:       []orgsync.DirectoryUser{{ExternalID: "u1", Name: "张三", Email: "zhangsan@xinghe.example", DeptIDs: []string{"2"}}},
	}
	res, err := syncer.Sync(context.Background(), "org_1", cfg, &stubClient{dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Created != 0 || len(res.Skipped) != 1 {
		t.Fatalf("res = %+v", res)
	}
	count := 0
	st.Read(func(s *store.State) { count = len(s.Users) })
	if count != 0 {
		t.Fatalf("users = %d, want 0", count)
	}
}

func TestSyncEmailAutoLink(t *testing.T) {
	st := newTestStore(t)
	local := &store.User{ID: store.NewID("usr"), Username: "zhangsan", DisplayName: "张三",
		Department: "研发部", Role: "employee", Email: "zhangsan@xinghe.example", AuthSource: "local"}
	st.WithLock(func(s *store.State) { s.Users[local.ID] = local })
	syncer := orgsync.New(st, nil)
	dir := &orgsync.Directory{
		Departments: map[string]string{"2": "研发部"},
		Users:       []orgsync.DirectoryUser{{ExternalID: "u1", Name: "张三", Email: "zhangsan@xinghe.example", DeptIDs: []string{"2"}}},
	}
	res, err := syncer.Sync(context.Background(), "org_1", syncCfg(), &stubClient{dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Linked != 1 || res.Created != 0 {
		t.Fatalf("res = %+v", res)
	}
	u := findUser(t, st, "zhangsan")
	if len(u.ExternalIdentities) != 1 || u.ExternalIdentities[0].Subject != "dingtalk:corp-xinghe:u1" {
		t.Fatalf("identities = %+v", u.ExternalIdentities)
	}
	if u.AuthSource != "local" {
		t.Fatalf("auto-link must not change the account source: %q", u.AuthSource)
	}
}

func TestSyncDisabledNotRestored(t *testing.T) {
	st := newTestStore(t)
	u := &store.User{ID: store.NewID("usr"), Username: "zhangsan", DisplayName: "张三",
		Role: "employee", AuthSource: "dingtalk", Disabled: true,
		ExternalIdentities: []store.ExternalIdentity{{Provider: "dingtalk", ExternalID: "u1", Subject: "dingtalk:corp-xinghe:u1"}}}
	st.WithLock(func(s *store.State) { s.Users[u.ID] = u })
	syncer := orgsync.New(st, nil)
	dir := &orgsync.Directory{
		Departments: map[string]string{"2": "研发部"},
		Users:       []orgsync.DirectoryUser{{ExternalID: "u1", Name: "张三", Email: "zhangsan@xinghe.example", DeptIDs: []string{"2"}}},
	}
	res, err := syncer.Sync(context.Background(), "org_1", syncCfg(), &stubClient{dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || res.Created != 0 {
		t.Fatalf("res = %+v", res)
	}
	if got := findUser(t, st, "zhangsan"); !got.Disabled {
		t.Fatal("locally disabled account was restored by sync")
	}
}

func TestSyncEmptyDirectoryAborts(t *testing.T) {
	st := newTestStore(t)
	syncer := orgsync.New(st, nil)
	res, err := syncer.Sync(context.Background(), "org_1", syncCfg(),
		&stubClient{dir: &orgsync.Directory{Departments: map[string]string{}, Users: nil}})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || res.Error == "" {
		t.Fatalf("empty directory must fail the run: %+v", res)
	}
	count := 0
	st.Read(func(s *store.State) { count = len(s.Users) })
	if count != 0 {
		t.Fatalf("users = %d after aborted sync", count)
	}
}

func TestSyncFetchErrorChangesNothing(t *testing.T) {
	st := newTestStore(t)
	syncer := orgsync.New(st, nil)
	res, err := syncer.Sync(context.Background(), "org_1", syncCfg(),
		&stubClient{dirErr: fmt.Errorf("DingTalk departments: HTTP 500")})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || !strings.Contains(res.Error, "HTTP 500") {
		t.Fatalf("res = %+v", res)
	}
}

func TestSyncDisableMissing(t *testing.T) {
	st := newTestStore(t)
	// Provider-created account, absent from the next directory.
	gone := &store.User{ID: store.NewID("usr"), Username: "gone-user", DisplayName: "离职人",
		Role: "employee", AuthSource: "dingtalk",
		ExternalIdentities: []store.ExternalIdentity{{Provider: "dingtalk", ExternalID: "u9", Subject: "dingtalk:corp-xinghe:u9"}}}
	// Account owned by another source, dingtalk identity only linked.
	linked := &store.User{ID: store.NewID("usr"), Username: "linked-user", DisplayName: "关联人",
		Role: "employee", AuthSource: "oidc", OIDCSubject: "oidc-sub-1",
		ExternalIdentities: []store.ExternalIdentity{
			{Provider: "oidc", ExternalID: "oidc-sub-1", Subject: "oidc:oidc-sub-1"},
			{Provider: "dingtalk", ExternalID: "u9b", Subject: "dingtalk:corp-xinghe:u9b"},
		}}
	// Out of the app's directory scope: must be kept.
	scoped := &store.User{ID: store.NewID("usr"), Username: "scoped-user", DisplayName: "范围外",
		Role: "employee", AuthSource: "dingtalk",
		ExternalIdentities: []store.ExternalIdentity{{Provider: "dingtalk", ExternalID: "u8", Subject: "dingtalk:corp-xinghe:u8"}}}
	st.WithLock(func(s *store.State) {
		s.Users[gone.ID] = gone
		s.Users[linked.ID] = linked
		s.Users[scoped.ID] = scoped
	})
	revoked := map[string]bool{}
	syncer := orgsync.New(st, func(userID string) { revoked[userID] = true })
	cfg := syncCfg()
	cfg.DisableMissing = true
	dir := &orgsync.Directory{
		Departments: map[string]string{"2": "研发部"},
		Users:       []orgsync.DirectoryUser{{ExternalID: "u1", Name: "张三", Email: "zhangsan@xinghe.example", DeptIDs: []string{"2"}}},
	}
	client := &stubClient{dir: dir, states: map[string]orgsync.UserState{
		"u9": orgsync.UserGone, "u9b": orgsync.UserGone, "u8": orgsync.UserOutOfScope,
	}}
	res, err := syncer.Sync(context.Background(), "org_1", cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("res = %+v", res)
	}
	if len(res.Disabled) != 1 || res.Disabled[0] != "gone-user" {
		t.Fatalf("disabled = %v", res.Disabled)
	}
	if len(res.Unlinked) != 1 || res.Unlinked[0] != "linked-user" {
		t.Fatalf("unlinked = %v", res.Unlinked)
	}
	if len(res.Kept) != 1 {
		t.Fatalf("kept = %v", res.Kept)
	}
	if !revoked[gone.ID] {
		t.Fatal("disabled account's sessions were not revoked")
	}
	if got := findUser(t, st, "gone-user"); !got.Disabled {
		t.Fatal("gone account not disabled")
	}
	u := findUser(t, st, "linked-user")
	if u.Disabled || len(u.ExternalIdentities) != 1 || u.ExternalIdentities[0].Provider != "oidc" {
		t.Fatalf("linked account wrongly disabled or identities = %+v", u.ExternalIdentities)
	}
	if got := findUser(t, st, "scoped-user"); got.Disabled {
		t.Fatal("out-of-scope account was disabled")
	}
	// LastSync is persisted on the provider record.
	st.WithLock(func(s *store.State) {
		s.OrgProviders["org_1"] = &store.OrgProvider{ID: "org_1", Provider: "dingtalk", Name: "钉钉"}
	})
	res, err = syncer.Sync(context.Background(), "org_1", cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("second sync res = %+v", res)
	}
	var last *store.OrgSyncResult
	st.Read(func(s *store.State) {
		if p, ok := s.OrgProviders["org_1"]; ok {
			last = p.LastSync
		}
	})
	if last == nil || !last.OK || last.Remote != 1 {
		t.Fatalf("last sync = %+v", last)
	}
}
