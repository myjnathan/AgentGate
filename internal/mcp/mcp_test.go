package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/federation"
	"github.com/myjnathan/agentgate/internal/folderkb"
	"github.com/myjnathan/agentgate/internal/guard"
	"github.com/myjnathan/agentgate/internal/mcp"
	"github.com/myjnathan/agentgate/internal/restadapter"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/seed"
	"github.com/myjnathan/agentgate/internal/semidx"
	"github.com/myjnathan/agentgate/internal/store"
	"github.com/myjnathan/agentgate/internal/terminal"
)

type testEnv struct {
	srv     *httptest.Server
	msrv    *mcp.Server
	st      *store.Store
	byoaTok string
	mgdTok  string
}

func setup(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New("test-master-key-123456")
	if err != nil {
		t.Fatal(err)
	}
	// Demo seeds users and the demo KB. The self base URL is only used
	// for model-gateway channels, which these tests never call.
	if err := seed.Demo(st, box, "http://localhost"); err != nil {
		t.Fatal(err)
	}
	au := auth.New(st)
	al, err := audit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	det := guard.NewDetector(seed.DemoBannedWords, seed.DemoCustomWords)
	fed := federation.New(st, box)
	rest := restadapter.New(st, box)
	// Folder knowledge base (F18): one internal folder and one
	// confidential folder, indexed at setup. Keywords here (考勤,
	// 请假, 办公用品, 机密资料) deliberately avoid the seeded KB's
	// vocabulary so the two halves of kb_search stay separable.
	fk := folderkb.New(st)
	intDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(intDir, "考勤与请假制度.md"), []byte(
		"# 考勤与请假制度\n\n本制度适用于全体员工。\n\n## 请假流程\n\n员工请假须提前提交申请，年假每年 5 天起。\n\n## 加班管理\n\n加班须提前报备，每月不得超过 36 小时。\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	confDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(confDir, "机密资料.md"), []byte(
		"# 机密资料\n\n仅受管级可见的内部项目说明。\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st.WithLock(func(s *store.State) {
		s.KBFolderSources["kbf_int"] = &store.KBFolderSource{ID: "kbf_int", Name: "公司制度",
			Path: intDir, Level: store.LevelInternal, Enabled: true}
		s.KBFolderSources["kbf_conf"] = &store.KBFolderSource{ID: "kbf_conf", Name: "机密资料",
			Path: confDir, Level: store.LevelConfidential, Enabled: true}
	})
	for _, id := range []string{"kbf_int", "kbf_conf"} {
		src, ok := fk.Source(id)
		if !ok {
			t.Fatalf("folder source %s not found", id)
		}
		if _, err := fk.Rescan(src); err != nil {
			t.Fatal(err)
		}
	}
	msrv := mcp.New(st, au, al, det, fed, rest)
	msrv.SetFolderKB(fk)
	// Semantic index (F19): entries are generated at setup, like the
	// startup rescan in main.
	semix := semidx.New()
	msrv.SetSemanticIndex(semix)
	semix.RegenerateChanged(context.Background(), fk.Snapshots())
	srv := httptest.NewServer(msrv)
	t.Cleanup(srv.Close)

	// A fake upstream MCP server (F6): hr is BYOA-visible with
	// MaxDataLevel internal; ops is managed-only with MaxDataLevel
	// confidential. project_status answers with the demo custom word
	// XH-200 (confidential) so result redaction is observable.
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if req.Method == "tools/list" {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"tools": []map[string]any{
				{"name": "leave_balance", "description": "remaining leave days",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
				{"name": "project_status", "description": "project status",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
				{"name": "restart_service", "description": "restart a service",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
				{"name": "project_brief", "description": "project brief with the code in two spellings",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
			}}})
			return
		}
		text := "ok"
		switch req.Params.Name {
		case "leave_balance":
			text = `{"balance": 5, "employee": "lixiaofeng"}`
		case "project_status":
			text = "XH-200 进度正常"
		case "project_brief":
			// F32: the same custom word in two surface forms (with a
			// space, as F29 normalized detection finds it, and with
			// its hyphen) must share one placeholder for BYOA.
			text = "项目 XH 200（代号 XH-200）进度正常"
		case "restart_service":
			text = "restarted"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}}, "isError": false}})
	}))
	t.Cleanup(upSrv.Close)
	st.WithLock(func(s *store.State) {
		s.Upstreams["up_hr"] = &store.UpstreamServer{ID: "up_hr", Name: "hr", URL: upSrv.URL,
			MaxDataLevel: store.LevelInternal, MinTier: store.TierBYOA, Enabled: true}
		s.Upstreams["up_ops"] = &store.UpstreamServer{ID: "up_ops", Name: "ops", URL: upSrv.URL,
			MaxDataLevel: store.LevelConfidential, MinTier: store.TierManaged, Enabled: true}
	})

	// A fake REST API (F7): GET leave balance (query param), POST
	// leave request (JSON body), GET notice (answers with the demo
	// custom word XH-200 so result redaction is observable).
	restSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/hr/leave-balance":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"employee_id": r.URL.Query().Get("employee_id"), "balance_days": 12})
		case r.Method == http.MethodPost && r.URL.Path == "/hr/leave-request":
			var in struct {
				EmployeeID string `json:"employee_id"`
				Days       int    `json:"days"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "submitted", "employee_id": in.EmployeeID, "days": in.Days})
		case r.Method == http.MethodGet && r.URL.Path == "/hr/notice":
			_, _ = w.Write([]byte("XH-200 进度正常"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(restSrv.Close)
	st.WithLock(func(s *store.State) {
		s.RESTTools["rt_fake_balance"] = &store.RESTTool{ID: "rt_fake_balance", Name: "fake_balance",
			Description: "fake leave balance", BaseURL: restSrv.URL, Method: "GET", Path: "/hr/leave-balance",
			InputSchema:  json.RawMessage(`{"type":"object","properties":{"employee_id":{"type":"string"}},"required":["employee_id"]}`),
			MaxDataLevel: store.LevelInternal, MinTier: store.TierBYOA, Enabled: true}
		s.RESTTools["rt_fake_request"] = &store.RESTTool{ID: "rt_fake_request", Name: "fake_request",
			Description: "fake leave request", BaseURL: restSrv.URL, Method: "POST", Path: "/hr/leave-request",
			InputSchema:  json.RawMessage(`{"type":"object","properties":{"employee_id":{"type":"string"},"days":{"type":"integer"}},"required":["employee_id","days"]}`),
			MaxDataLevel: store.LevelInternal, MinTier: store.TierManaged, Enabled: true}
		s.RESTTools["rt_fake_notice"] = &store.RESTTool{ID: "rt_fake_notice", Name: "fake_notice",
			Description: "fake notice", BaseURL: restSrv.URL, Method: "GET", Path: "/hr/notice",
			InputSchema:  json.RawMessage(`{"type":"object","properties":{}}`),
			MaxDataLevel: store.LevelInternal, MinTier: store.TierBYOA, Enabled: true}
	})

	mkToken := func(username string, tier store.Tier) string {
		t.Helper()
		var user *store.User
		st.Read(func(s *store.State) {
			for _, u := range s.Users {
				if u.Username == username {
					cp := *u
					user = &cp
				}
			}
		})
		if user == nil {
			t.Fatalf("seed user %q not found", username)
		}
		dc := au.StartDeviceCode("workbuddy", tier)
		if _, _, err := au.ApproveDeviceCode(dc.Code, user, "test device"); err != nil {
			t.Fatal(err)
		}
		_, plain := au.PollDeviceCode(dc.Code, dc.Secret)
		if plain == "" {
			t.Fatal("no token issued")
		}
		return plain
	}
	return &testEnv{srv: srv, msrv: msrv, st: st,
		byoaTok: mkToken("lixiaofeng", store.TierBYOA),
		mgdTok:  mkToken("chenjing", store.TierManaged)}
}

type rpcResponse struct {
	Result *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// callTool issues a tools/call JSON-RPC request. An empty token sends no
// Authorization header at all.
func (e *testEnv) callTool(t *testing.T, token, tool string, args map[string]any) rpcResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	req, _ := http.NewRequest("POST", e.srv.URL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode RPC response: %v", err)
	}
	return out
}

func resultText(t *testing.T, r rpcResponse) string {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("unexpected RPC error %d: %s", r.Error.Code, r.Error.Message)
	}
	if r.Result == nil || len(r.Result.Content) == 0 {
		t.Fatal("RPC response has no result content")
	}
	return r.Result.Content[0].Text
}

func TestUnauthorizedRequestRejected(t *testing.T) {
	e := setup(t)
	// No token at all.
	r := e.callTool(t, "", "whoami", map[string]any{})
	if r.Error == nil || r.Error.Code != -32001 {
		t.Fatalf("no-token request: got %+v, want RPC error -32001 unauthorized", r)
	}
	// Bogus token.
	r = e.callTool(t, "ag_at_deadbeefdeadbeef", "whoami", map[string]any{})
	if r.Error == nil || r.Error.Code != -32001 {
		t.Fatalf("bogus-token request: got %+v, want RPC error -32001 unauthorized", r)
	}
}

func TestWhoamiReturnsIdentity(t *testing.T) {
	e := setup(t)
	text := resultText(t, e.callTool(t, e.byoaTok, "whoami", map[string]any{}))
	var id struct {
		Username   string `json:"username"`
		Department string `json:"department"`
		AgentType  string `json:"agent_type"`
		Tier       string `json:"tier"`
	}
	if err := json.Unmarshal([]byte(text), &id); err != nil {
		t.Fatalf("whoami result is not JSON: %v (%s)", err, text)
	}
	if id.Username != "lixiaofeng" {
		t.Fatalf("whoami username = %q, want lixiaofeng", id.Username)
	}
	if id.Department != "研发部" {
		t.Fatalf("whoami department = %q, want 研发部", id.Department)
	}
	if id.Tier != string(store.TierBYOA) {
		t.Fatalf("whoami tier = %q, want %q", id.Tier, store.TierBYOA)
	}
}

func TestKBSearchFindsReimbursementPolicy(t *testing.T) {
	e := setup(t)
	text := resultText(t, e.callTool(t, e.byoaTok, "kb_search", map[string]any{"query": "报销"}))
	if !strings.Contains(text, "kb_reimburse") {
		t.Fatalf("kb_search for 报销 should hit kb_reimburse, got %s", text)
	}
	if !strings.Contains(text, "差旅报销制度") {
		t.Fatalf("kb_search hit should cite the reimbursement policy title, got %s", text)
	}
}

func TestKBGetTierAuthorization(t *testing.T) {
	e := setup(t)
	// kb_register is a confidential document and kb_get requires the
	// managed tier at tool level: a BYOA token must be denied outright.
	// The tool is hidden from the BYOA list (F31), so the denial is
	// indistinguishable from an unknown tool.
	r := e.callTool(t, e.byoaTok, "kb_get", map[string]any{"doc_id": "kb_register"})
	if r.Error == nil || r.Error.Code != -32602 || r.Error.Message != "unknown tool" {
		t.Fatalf("BYOA kb_get: got %+v, want RPC error -32602 unknown tool", r)
	}

	// A managed-tier token may read the confidential document in full.
	text := resultText(t, e.callTool(t, e.mgdTok, "kb_get", map[string]any{"doc_id": "kb_register"}))
	if !strings.Contains(text, "二类医疗器械") {
		t.Fatalf("managed kb_get should return the registration document, got %s", text)
	}
	if !strings.Contains(text, "XH-200") {
		t.Fatalf("managed kb_get should return full text (incl. project code), got %s", text)
	}
}

// ---- F6: federated upstream tools ----

// listToolNames issues a tools/list JSON-RPC request and returns the
// advertised tool names.
func (e *testEnv) listToolNames(t *testing.T, token string) []string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	req, _ := http.NewRequest("POST", e.srv.URL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Result *struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	if out.Error != nil {
		t.Fatalf("tools/list error %d: %s", out.Error.Code, out.Error.Message)
	}
	var names []string
	for _, tl := range out.Result.Tools {
		names = append(names, tl.Name)
	}
	return names
}

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func TestFederatedToolsListedNamespaced(t *testing.T) {
	e := setup(t)
	byoa := e.listToolNames(t, e.byoaTok)
	for _, want := range []string{"kb_search", "hr.leave_balance", "hr.project_status"} {
		if !hasName(byoa, want) {
			t.Fatalf("BYOA tools/list missing %q: %v", want, byoa)
		}
	}
	if hasName(byoa, "ops.leave_balance") {
		t.Fatalf("BYOA tools/list must hide managed-only upstream tools: %v", byoa)
	}
	mgd := e.listToolNames(t, e.mgdTok)
	if !hasName(mgd, "ops.restart_service") {
		t.Fatalf("managed tools/list missing ops.restart_service: %v", mgd)
	}
}

func TestFederatedToolCallForwards(t *testing.T) {
	e := setup(t)
	text := resultText(t, e.callTool(t, e.byoaTok, "hr.leave_balance", map[string]any{}))
	if !strings.Contains(text, `"balance": 5`) {
		t.Fatalf("hr.leave_balance should return the upstream answer, got %s", text)
	}
}

func TestFederatedToolCallTierDenied(t *testing.T) {
	e := setup(t)
	// The tool is hidden from the BYOA list (F31), so the denial is
	// indistinguishable from an unknown tool.
	r := e.callTool(t, e.byoaTok, "ops.restart_service", map[string]any{})
	if r.Error == nil || r.Error.Code != -32602 || r.Error.Message != "unknown tool" {
		t.Fatalf("BYOA call to managed-only upstream: got %+v, want RPC error -32602 unknown tool", r)
	}
}

func TestFederatedToolCallArgsLevelBlocked(t *testing.T) {
	e := setup(t)
	// XH-200 is a demo custom word (confidential), above hr's
	// MaxDataLevel internal: the arguments must never reach upstream.
	r := e.callTool(t, e.byoaTok, "hr.leave_balance", map[string]any{"note": "XH-200"})
	if r.Error == nil || r.Error.Code != -32004 {
		t.Fatalf("confidential args to internal-only upstream: got %+v, want RPC error -32004", r)
	}
	// A banned word in the arguments is blocked the same way.
	r = e.callTool(t, e.byoaTok, "hr.leave_balance", map[string]any{"note": "test_sensitive"})
	if r.Error == nil || r.Error.Code != -32004 {
		t.Fatalf("banned-word args: got %+v, want RPC error -32004", r)
	}
}

func TestFederatedToolResultRedactedForBYOA(t *testing.T) {
	e := setup(t)
	// The upstream answers with the confidential custom word XH-200.
	text := resultText(t, e.callTool(t, e.byoaTok, "hr.project_status", map[string]any{}))
	if strings.Contains(text, "XH-200") {
		t.Fatalf("BYOA must not receive the confidential word in a tool result, got %s", text)
	}
	if !strings.Contains(text, "<CUSTOM_1>") {
		t.Fatalf("BYOA result should carry the redaction placeholder, got %s", text)
	}
	// A managed token receives the raw upstream answer.
	text = resultText(t, e.callTool(t, e.mgdTok, "hr.project_status", map[string]any{}))
	if !strings.Contains(text, "XH-200") {
		t.Fatalf("managed result should keep the raw upstream answer, got %s", text)
	}
}

// F32: a federated tool result holding the same confidential value in
// two surface forms is redacted to one placeholder for BYOA, so the
// model sees one entity; a managed token still receives the raw text.
func TestFederatedToolResultCanonicalPlaceholders(t *testing.T) {
	e := setup(t)
	text := resultText(t, e.callTool(t, e.byoaTok, "hr.project_brief", map[string]any{}))
	if strings.Contains(text, "XH 200") || strings.Contains(text, "XH-200") {
		t.Fatalf("BYOA must not receive the confidential word in either spelling, got %s", text)
	}
	if strings.Count(text, "<CUSTOM_1>") != 2 {
		t.Fatalf("both spellings must share one placeholder, got %s", text)
	}
	text = resultText(t, e.callTool(t, e.mgdTok, "hr.project_brief", map[string]any{}))
	if !strings.Contains(text, "XH 200") || !strings.Contains(text, "XH-200") {
		t.Fatalf("managed result should keep the raw upstream answer, got %s", text)
	}
}

func TestFederatedToolUnknownNamespace(t *testing.T) {
	e := setup(t)
	r := e.callTool(t, e.byoaTok, "nope.tool", map[string]any{})
	if r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("unknown upstream namespace: got %+v, want RPC error -32602", r)
	}
}

// ---- F7: REST-adapted tools ----

func TestRESTToolsListedNamespaced(t *testing.T) {
	e := setup(t)
	byoa := e.listToolNames(t, e.byoaTok)
	// The demo seed registers rest.leave_balance; the test doubles
	// are rest.fake_*.
	for _, want := range []string{"rest.leave_balance", "rest.fake_balance", "rest.fake_notice"} {
		if !hasName(byoa, want) {
			t.Fatalf("BYOA tools/list missing %q: %v", want, byoa)
		}
	}
	if hasName(byoa, "rest.fake_request") {
		t.Fatalf("BYOA tools/list must hide managed-only REST tools: %v", byoa)
	}
	mgd := e.listToolNames(t, e.mgdTok)
	if !hasName(mgd, "rest.fake_request") {
		t.Fatalf("managed tools/list missing rest.fake_request: %v", mgd)
	}
}

func TestRESTToolCallGET(t *testing.T) {
	e := setup(t)
	text := resultText(t, e.callTool(t, e.byoaTok, "rest.fake_balance", map[string]any{"employee_id": "lixiaofeng"}))
	if !strings.Contains(text, `"balance_days":12`) {
		t.Fatalf("rest.fake_balance should return the upstream answer, got %s", text)
	}
	if !strings.Contains(text, "lixiaofeng") {
		t.Fatalf("query param must reach the upstream, got %s", text)
	}
}

func TestRESTToolCallPOSTBody(t *testing.T) {
	e := setup(t)
	text := resultText(t, e.callTool(t, e.mgdTok, "rest.fake_request",
		map[string]any{"employee_id": "chenjing", "days": float64(2)}))
	if !strings.Contains(text, `"status":"submitted"`) || !strings.Contains(text, `"days":2`) {
		t.Fatalf("rest.fake_request should echo the JSON body, got %s", text)
	}
}

func TestRESTToolCallTierDenied(t *testing.T) {
	e := setup(t)
	// The tool is hidden from the BYOA list (F31), so the denial is
	// indistinguishable from an unknown tool.
	r := e.callTool(t, e.byoaTok, "rest.fake_request", map[string]any{"employee_id": "li", "days": float64(1)})
	if r.Error == nil || r.Error.Code != -32602 || r.Error.Message != "unknown tool" {
		t.Fatalf("BYOA call to managed-only REST tool: got %+v, want RPC error -32602 unknown tool", r)
	}
}

// ---- F31: tools/list and tools/call apply the same rule ----

func TestToolsListCallSameRule(t *testing.T) {
	e := setup(t)

	// The tier-gated built-in tool is hidden from the BYOA list
	// (previously it was listed but refused at call time) and
	// visible to managed callers.
	byoa := e.listToolNames(t, e.byoaTok)
	if hasName(byoa, "kb_get") {
		t.Fatalf("BYOA tools/list must hide managed-only kb_get: %v", byoa)
	}
	mgd := e.listToolNames(t, e.mgdTok)
	if !hasName(mgd, "kb_get") {
		t.Fatalf("managed tools/list missing kb_get: %v", mgd)
	}

	// Every statically hidden tool answers exactly like an unknown
	// tool when called directly — list filtering leaks nothing.
	for _, hidden := range []string{"kb_get", "ops.restart_service", "rest.fake_request"} {
		r := e.callTool(t, e.byoaTok, hidden, map[string]any{})
		if r.Error == nil || r.Error.Code != -32602 || r.Error.Message != "unknown tool" {
			t.Fatalf("BYOA call to hidden tool %q: got %+v, want RPC error -32602 unknown tool", hidden, r)
		}
	}

	// Invariant: anything the list advertises must pass the static
	// gate at call time — calling a listed tool with empty arguments
	// may fail on argument validation, but never as "unknown tool".
	// (rest.fake_balance with no arguments is the validation case:
	// -32602 naming the missing field, not "unknown tool".)
	for _, tok := range []string{e.byoaTok, e.mgdTok} {
		for _, name := range e.listToolNames(t, tok) {
			r := e.callTool(t, tok, name, map[string]any{})
			if r.Error != nil && r.Error.Message == "unknown tool" {
				t.Fatalf("listed tool %q for this tier is unknown at call time: %+v", name, r)
			}
		}
	}
}

func TestRESTToolCallArgsValidation(t *testing.T) {
	e := setup(t)
	// Missing required argument.
	r := e.callTool(t, e.byoaTok, "rest.fake_balance", map[string]any{})
	if r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("missing argument: got %+v, want RPC error -32602", r)
	}
	// Wrong type.
	r = e.callTool(t, e.byoaTok, "rest.fake_balance", map[string]any{"employee_id": float64(3)})
	if r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("wrong argument type: got %+v, want RPC error -32602", r)
	}
	// Unknown argument.
	r = e.callTool(t, e.byoaTok, "rest.fake_balance", map[string]any{"employee_id": "li", "extra": 1})
	if r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("unknown argument: got %+v, want RPC error -32602", r)
	}
}

func TestRESTToolCallArgsLevelBlocked(t *testing.T) {
	e := setup(t)
	// XH-200 is a demo custom word (confidential), above the tool's
	// MaxDataLevel internal: the arguments must never reach upstream.
	r := e.callTool(t, e.byoaTok, "rest.fake_balance", map[string]any{"employee_id": "XH-200"})
	if r.Error == nil || r.Error.Code != -32004 {
		t.Fatalf("confidential args to internal-only REST tool: got %+v, want RPC error -32004", r)
	}
	// A banned word in the arguments is blocked the same way.
	r = e.callTool(t, e.byoaTok, "rest.fake_balance", map[string]any{"employee_id": "test_sensitive"})
	if r.Error == nil || r.Error.Code != -32004 {
		t.Fatalf("banned-word args: got %+v, want RPC error -32004", r)
	}
}

func TestRESTToolResultRedactedForBYOA(t *testing.T) {
	e := setup(t)
	// The upstream answers with the confidential custom word XH-200.
	text := resultText(t, e.callTool(t, e.byoaTok, "rest.fake_notice", map[string]any{}))
	if strings.Contains(text, "XH-200") {
		t.Fatalf("BYOA must not receive the confidential word in a tool result, got %s", text)
	}
	if !strings.Contains(text, "<CUSTOM_1>") {
		t.Fatalf("BYOA result should carry the redaction placeholder, got %s", text)
	}
	// A managed token receives the raw upstream answer.
	text = resultText(t, e.callTool(t, e.mgdTok, "rest.fake_notice", map[string]any{}))
	if !strings.Contains(text, "XH-200") {
		t.Fatalf("managed result should keep the raw upstream answer, got %s", text)
	}
}

func TestRESTToolUnknown(t *testing.T) {
	e := setup(t)
	r := e.callTool(t, e.byoaTok, "rest.nope", map[string]any{})
	if r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("unknown REST tool: got %+v, want RPC error -32602", r)
	}
}

// ---- F8: prompt templates ----

// listPromptNames issues a prompts/list JSON-RPC request and returns
// the advertised prompt names.
func (e *testEnv) listPromptNames(t *testing.T, token string) []string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "prompts/list",
	})
	req, _ := http.NewRequest("POST", e.srv.URL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Result *struct {
			Prompts []struct {
				Name string `json:"name"`
			} `json:"prompts"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode prompts/list: %v", err)
	}
	if out.Error != nil {
		t.Fatalf("prompts/list error %d: %s", out.Error.Code, out.Error.Message)
	}
	var names []string
	for _, p := range out.Result.Prompts {
		names = append(names, p.Name)
	}
	return names
}

type promptResponse struct {
	Result *struct {
		Description string `json:"description"`
		Messages    []struct {
			Role    string `json:"role"`
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// getPrompt issues a prompts/get JSON-RPC request.
func (e *testEnv) getPrompt(t *testing.T, token, name string, args map[string]any, version string) promptResponse {
	t.Helper()
	params := map[string]any{"name": name, "arguments": args}
	if version != "" {
		params["version"] = version
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "prompts/get", "params": params,
	})
	req, _ := http.NewRequest("POST", e.srv.URL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out promptResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode prompts/get: %v", err)
	}
	return out
}

func promptText(t *testing.T, r promptResponse) string {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("unexpected prompts/get error %d: %s", r.Error.Code, r.Error.Message)
	}
	if r.Result == nil || len(r.Result.Messages) == 0 {
		t.Fatal("prompts/get response has no messages")
	}
	return r.Result.Messages[0].Content.Text
}

func TestPromptsListedPublishedOnly(t *testing.T) {
	e := setup(t)
	byoa := e.listPromptNames(t, e.byoaTok)
	if !hasName(byoa, "weekly_report") {
		t.Fatalf("BYOA prompts/list missing seeded weekly_report: %v", byoa)
	}
	// A draft and a managed-only template must stay hidden from BYOA.
	e.st.WithLock(func(s *store.State) {
		s.Prompts["pt_draft"] = &store.PromptTemplate{ID: "pt_draft", Name: "draft_one",
			Scope: "*", MaxDataLevel: store.LevelInternal, MinTier: store.TierBYOA,
			Status: "in_review", CurrentVersion: 1,
			Versions: []store.PromptVersion{{Version: 1, Content: "草稿 {{name}}",
				Arguments: []store.PromptArgument{{Name: "name", Required: true}}}}}
		s.Prompts["pt_mgd"] = &store.PromptTemplate{ID: "pt_mgd", Name: "managed_only",
			Scope: "*", MaxDataLevel: store.LevelInternal, MinTier: store.TierManaged,
			Status: "published", CurrentVersion: 1, PublishedVersion: 1,
			Versions: []store.PromptVersion{{Version: 1, Content: "受管 {{name}}",
				Arguments: []store.PromptArgument{{Name: "name", Required: true}}}}}
	})
	byoa = e.listPromptNames(t, e.byoaTok)
	if hasName(byoa, "draft_one") {
		t.Fatalf("BYOA prompts/list must hide drafts: %v", byoa)
	}
	if hasName(byoa, "managed_only") {
		t.Fatalf("BYOA prompts/list must hide managed-only templates: %v", byoa)
	}
	mgd := e.listPromptNames(t, e.mgdTok)
	if !hasName(mgd, "managed_only") {
		t.Fatalf("managed prompts/list missing managed_only: %v", mgd)
	}
	if hasName(mgd, "draft_one") {
		t.Fatalf("managed prompts/list must hide drafts: %v", mgd)
	}
}

func TestPromptGetRendersPublished(t *testing.T) {
	e := setup(t)
	text := promptText(t, e.getPrompt(t, e.byoaTok, "weekly_report",
		map[string]any{"name": "李晓峰", "department": "研发部", "week": "2026-W47"}, ""))
	if !strings.Contains(text, "研发部的李晓峰") || !strings.Contains(text, "2026-W47") {
		t.Fatalf("weekly_report render missing substitutions: %q", text)
	}
	if strings.Contains(text, "{{") {
		t.Fatalf("rendered prompt leaks placeholder syntax: %q", text)
	}
}

func TestPromptGetVersionPinning(t *testing.T) {
	e := setup(t)
	e.st.WithLock(func(s *store.State) {
		s.Prompts["pt_pin"] = &store.PromptTemplate{ID: "pt_pin", Name: "pin_test",
			Scope: "*", MaxDataLevel: store.LevelInternal, MinTier: store.TierBYOA,
			Status: "published", CurrentVersion: 2, PublishedVersion: 1,
			Versions: []store.PromptVersion{
				{Version: 1, Content: "版本一 {{name}}",
					Arguments: []store.PromptArgument{{Name: "name", Required: true}}},
				{Version: 2, Content: "版本二 {{name}}",
					Arguments: []store.PromptArgument{{Name: "name", Required: true}}},
			}}
	})
	args := map[string]any{"name": "李晓峰"}
	// v2 exists but is not published: pinning it must fail, and
	// @latest means the published version, not the newest one.
	r := e.getPrompt(t, e.byoaTok, "pin_test", args, "2")
	if r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("pin unpublished v2: got %+v, want RPC error -32602", r)
	}
	if text := promptText(t, e.getPrompt(t, e.byoaTok, "pin_test", args, "@latest")); !strings.Contains(text, "版本一") {
		t.Fatalf("@latest must render the published v1, got %q", text)
	}
	if text := promptText(t, e.getPrompt(t, e.byoaTok, "pin_test", args, "1")); !strings.Contains(text, "版本一") {
		t.Fatalf("pin v1 must render v1, got %q", text)
	}
	// Rollback/publish v2: now @latest and pin "2" render v2, and
	// pin "1" fails (v1 is no longer published).
	e.st.WithLock(func(s *store.State) { s.Prompts["pt_pin"].PublishedVersion = 2 })
	if text := promptText(t, e.getPrompt(t, e.byoaTok, "pin_test", args, "@latest")); !strings.Contains(text, "版本二") {
		t.Fatalf("after publish v2, @latest must render v2, got %q", text)
	}
	r = e.getPrompt(t, e.byoaTok, "pin_test", args, "1")
	if r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("pin superseded v1: got %+v, want RPC error -32602", r)
	}
	r = e.getPrompt(t, e.byoaTok, "pin_test", args, "@draft")
	if r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("@draft selector: got %+v, want RPC error -32602", r)
	}
}

func TestPromptGetArgsValidation(t *testing.T) {
	e := setup(t)
	r := e.getPrompt(t, e.byoaTok, "weekly_report", map[string]any{"name": "李晓峰"}, "")
	if r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("missing required argument: got %+v, want RPC error -32602", r)
	}
	r = e.getPrompt(t, e.byoaTok, "weekly_report",
		map[string]any{"name": "李晓峰", "department": "研发部", "week": "2026-W47", "extra": "x"}, "")
	if r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("unknown argument: got %+v, want RPC error -32602", r)
	}
	r = e.getPrompt(t, e.byoaTok, "weekly_report",
		map[string]any{"name": "李晓峰", "department": "研发部", "week": 47}, "")
	if r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("non-string argument: got %+v, want RPC error -32602", r)
	}
}

func TestPromptGetArgsLevelBlocked(t *testing.T) {
	e := setup(t)
	// XH-200 is confidential, above weekly_report's MaxDataLevel
	// internal: the argument must never be rendered.
	r := e.getPrompt(t, e.byoaTok, "weekly_report",
		map[string]any{"name": "XH-200", "department": "研发部", "week": "2026-W47"}, "")
	if r.Error == nil || r.Error.Code != -32004 {
		t.Fatalf("confidential argument: got %+v, want RPC error -32004", r)
	}
	r = e.getPrompt(t, e.byoaTok, "weekly_report",
		map[string]any{"name": "test_sensitive", "department": "研发部", "week": "2026-W47"}, "")
	if r.Error == nil || r.Error.Code != -32004 {
		t.Fatalf("banned-word argument: got %+v, want RPC error -32004", r)
	}
}

func TestPromptGetResultRedactedForBYOA(t *testing.T) {
	e := setup(t)
	e.st.WithLock(func(s *store.State) {
		s.Prompts["pt_notice"] = &store.PromptTemplate{ID: "pt_notice", Name: "notice",
			Scope: "*", MaxDataLevel: store.LevelConfidential, MinTier: store.TierBYOA,
			Status: "published", CurrentVersion: 1, PublishedVersion: 1,
			Versions: []store.PromptVersion{{Version: 1,
				Content:   "项目进展：XH-200 正常。请向{{name}}汇报。",
				Arguments: []store.PromptArgument{{Name: "name", Required: true}}}}}
	})
	// The template text itself carries the confidential custom word.
	text := promptText(t, e.getPrompt(t, e.byoaTok, "notice", map[string]any{"name": "李晓峰"}, ""))
	if strings.Contains(text, "XH-200") {
		t.Fatalf("BYOA must not receive the confidential word in a rendered prompt, got %q", text)
	}
	if !strings.Contains(text, "<CUSTOM_1>") {
		t.Fatalf("BYOA render should carry the redaction placeholder, got %q", text)
	}
	text = promptText(t, e.getPrompt(t, e.mgdTok, "notice", map[string]any{"name": "陈静"}, ""))
	if !strings.Contains(text, "XH-200") {
		t.Fatalf("managed render should keep the raw template text, got %q", text)
	}
}

func TestPromptGetTierDeniedAndUnknown(t *testing.T) {
	e := setup(t)
	e.st.WithLock(func(s *store.State) {
		s.Prompts["pt_mgd"] = &store.PromptTemplate{ID: "pt_mgd", Name: "managed_only",
			Scope: "*", MaxDataLevel: store.LevelInternal, MinTier: store.TierManaged,
			Status: "published", CurrentVersion: 1, PublishedVersion: 1,
			Versions: []store.PromptVersion{{Version: 1, Content: "受管 {{name}}",
				Arguments: []store.PromptArgument{{Name: "name", Required: true}}}}}
		s.Prompts["pt_draft"] = &store.PromptTemplate{ID: "pt_draft", Name: "draft_one",
			Scope: "*", MaxDataLevel: store.LevelInternal, MinTier: store.TierBYOA,
			Status: "in_review", CurrentVersion: 1,
			Versions: []store.PromptVersion{{Version: 1, Content: "草稿 {{name}}",
				Arguments: []store.PromptArgument{{Name: "name", Required: true}}}}}
	})
	// A managed-only prompt is hidden from the BYOA list (F31), so
	// the get is indistinguishable from an unknown prompt.
	r := e.getPrompt(t, e.byoaTok, "managed_only", map[string]any{"name": "李晓峰"}, "")
	if r.Error == nil || r.Error.Code != -32602 || r.Error.Message != "unknown prompt" {
		t.Fatalf("BYOA get of managed-only prompt: got %+v, want RPC error -32602 unknown prompt", r)
	}
	// A draft's existence must not leak through prompts/get.
	r = e.getPrompt(t, e.byoaTok, "draft_one", map[string]any{"name": "李晓峰"}, "")
	if r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("draft prompt get: got %+v, want RPC error -32602", r)
	}
	r = e.getPrompt(t, e.byoaTok, "nope", map[string]any{}, "")
	if r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("unknown prompt: got %+v, want RPC error -32602", r)
	}
}

// ---- F18: folder knowledge base ----

func TestFolderCatalogStructureSection(t *testing.T) {
	e := setup(t)
	// BYOA sees only the internal folder; the confidential folder
	// is invisible in the catalog.
	text := resultText(t, e.callTool(t, e.byoaTok, "kb_catalog", map[string]any{}))
	var cat struct {
		Documents []struct {
			DocID    string   `json:"doc_id"`
			Title    string   `json:"title"`
			Source   string   `json:"source"`
			Sections []string `json:"sections"`
		} `json:"documents"`
	}
	if err := json.Unmarshal([]byte(text), &cat); err != nil {
		t.Fatalf("kb_catalog result is not JSON: %v (%s)", err, text)
	}
	if len(cat.Documents) != 1 || cat.Documents[0].Title != "考勤与请假制度" {
		t.Fatalf("BYOA catalog = %+v, want only the internal folder doc", cat.Documents)
	}
	if len(cat.Documents[0].Sections) != 2 || cat.Documents[0].Sections[0] != "请假流程" {
		t.Fatalf("catalog sections = %v", cat.Documents[0].Sections)
	}
	docID := cat.Documents[0].DocID

	// Progressive disclosure: structure first (summaries only),
	// then one section's full text with its citation.
	text = resultText(t, e.callTool(t, e.byoaTok, "kb_structure", map[string]any{"doc_id": docID}))
	var tree struct {
		Title string `json:"title"`
		Tree  struct {
			Children []struct {
				ID      string `json:"node_id"`
				Title   string `json:"title"`
				Summary string `json:"summary"`
			} `json:"children"`
		} `json:"tree"`
	}
	if err := json.Unmarshal([]byte(text), &tree); err != nil {
		t.Fatalf("kb_structure result is not JSON: %v (%s)", err, text)
	}
	if len(tree.Tree.Children) != 2 || tree.Tree.Children[0].Summary == "" {
		t.Fatalf("structure = %+v", tree)
	}
	nodeID := tree.Tree.Children[0].ID
	text = resultText(t, e.callTool(t, e.byoaTok, "kb_section",
		map[string]any{"doc_id": docID, "node_id": nodeID}))
	var sec struct {
		File    string `json:"file"`
		Section string `json:"section"`
		Pages   string `json:"pages"`
		Text    string `json:"text"`
	}
	if err := json.Unmarshal([]byte(text), &sec); err != nil {
		t.Fatalf("kb_section result is not JSON: %v (%s)", err, text)
	}
	if sec.Section != "请假流程" || !strings.Contains(sec.Text, "年假每年 5 天起") {
		t.Fatalf("section = %+v", sec)
	}
	if sec.File != "考勤与请假制度.md" || sec.Pages == "" {
		t.Fatalf("section citation = file %q pages %q", sec.File, sec.Pages)
	}
	// Unknown document / node: same "not accessible" shape, no leak.
	r := e.callTool(t, e.byoaTok, "kb_structure", map[string]any{"doc_id": "folder:nope:x.md"})
	if r.Result == nil || !r.Result.IsError {
		t.Fatalf("unknown doc structure: got %+v, want isError", r)
	}
	r = e.callTool(t, e.byoaTok, "kb_section", map[string]any{"doc_id": docID, "node_id": "n99"})
	if r.Result == nil || !r.Result.IsError {
		t.Fatalf("unknown node section: got %+v, want isError", r)
	}
}

func TestFolderConfidentialHiddenFromBYOA(t *testing.T) {
	e := setup(t)
	// The managed catalog lists both folders; BYOA search never
	// surfaces the confidential one, managed search does.
	text := resultText(t, e.callTool(t, e.mgdTok, "kb_catalog", map[string]any{}))
	if !strings.Contains(text, "机密资料") || !strings.Contains(text, "考勤与请假制度") {
		t.Fatalf("managed catalog = %s", text)
	}
	var cat struct {
		Documents []struct {
			DocID string `json:"doc_id"`
			Title string `json:"title"`
		} `json:"documents"`
	}
	if err := json.Unmarshal([]byte(text), &cat); err != nil {
		t.Fatal(err)
	}
	var confID string
	for _, d := range cat.Documents {
		if d.Title == "机密资料" {
			confID = d.DocID
		}
	}
	if confID == "" {
		t.Fatal("confidential doc missing from managed catalog")
	}
	r := e.callTool(t, e.byoaTok, "kb_structure", map[string]any{"doc_id": confID})
	if r.Result == nil || !r.Result.IsError {
		t.Fatalf("BYOA structure of confidential folder doc: got %+v, want isError", r)
	}
	r = e.callTool(t, e.byoaTok, "kb_section", map[string]any{"doc_id": confID, "node_id": "n0"})
	if r.Result == nil || !r.Result.IsError {
		t.Fatalf("BYOA section of confidential folder doc: got %+v, want isError", r)
	}
	// Managed tier reads it.
	text = resultText(t, e.callTool(t, e.mgdTok, "kb_section",
		map[string]any{"doc_id": confID, "node_id": "n0"}))
	if !strings.Contains(text, "仅受管级可见") {
		t.Fatalf("managed section = %s", text)
	}
}

func TestKBSearchMergesFolderHits(t *testing.T) {
	e := setup(t)
	// A folder keyword hits the folder doc with section + pages.
	text := resultText(t, e.callTool(t, e.byoaTok, "kb_search", map[string]any{"query": "请假 年假"}))
	if !strings.Contains(text, "考勤与请假制度") || !strings.Contains(text, `"source":"folder"`) {
		t.Fatalf("kb_search folder hit missing: %s", text)
	}
	if !strings.Contains(text, "请假流程") {
		t.Fatalf("kb_search folder hit has no section: %s", text)
	}
	// The confidential folder's keyword finds nothing for BYOA…
	text = resultText(t, e.callTool(t, e.byoaTok, "kb_search", map[string]any{"query": "机密资料"}))
	if strings.Contains(text, "机密资料") {
		t.Fatalf("BYOA kb_search leaked confidential folder doc: %s", text)
	}
	// …but the managed tier finds it.
	text = resultText(t, e.callTool(t, e.mgdTok, "kb_search", map[string]any{"query": "机密资料"}))
	if !strings.Contains(text, "机密资料") {
		t.Fatalf("managed kb_search missed confidential folder doc: %s", text)
	}
	// Seeded documents still search as before.
	text = resultText(t, e.callTool(t, e.byoaTok, "kb_search", map[string]any{"query": "报销"}))
	if !strings.Contains(text, "kb_reimburse") || !strings.Contains(text, `"source":"kb"`) {
		t.Fatalf("kb_search seeded hit missing: %s", text)
	}
}

// TestPluginBlockSensitiveKnowledgeCut covers the F38 slice 2
// ladder at the MCP server: block_sensitive denies the knowledge
// tools at every data level (and hides them from tools/list),
// downgrade only caps the data level (effective BYOA), and clearing
// the disposition restores full managed access.
func TestPluginBlockSensitiveKnowledgeCut(t *testing.T) {
	e := setup(t)
	// The managed token belongs to chenjing (workbuddy). Give her
	// terminal a disposition, as a violating heartbeat would.
	var chenjing *store.User
	e.st.Read(func(s *store.State) {
		for _, u := range s.Users {
			if u.Username == "chenjing" {
				cp := *u
				chenjing = &cp
			}
		}
	})
	setAction := func(action string) {
		e.st.WithLock(func(s *store.State) {
			s.Terminals["term_test"] = &terminal.Terminal{ID: "term_test", UserID: chenjing.ID,
				Username: "chenjing", Department: chenjing.Department, AgentType: "workbuddy",
				PluginAction: action, PluginReasons: []string{"plugin_not_allowed"}}
		})
	}
	// Baseline: the managed token reads internal knowledge.
	text := resultText(t, e.callTool(t, e.mgdTok, "kb_search", map[string]any{"query": "报销"}))
	if !strings.Contains(text, "kb_reimburse") {
		t.Fatalf("baseline kb_search: %s", text)
	}
	// block_sensitive: knowledge tools are denied at every level
	// and hidden from tools/list.
	setAction("block_sensitive")
	r := e.callTool(t, e.mgdTok, "kb_search", map[string]any{"query": "报销"})
	if r.Error == nil || r.Error.Code != -32004 {
		t.Fatalf("block_sensitive kb_search: %+v, want -32004", r.Error)
	}
	names := e.listToolNames(t, e.mgdTok)
	for _, n := range names {
		if n == "kb_search" || n == "kb_catalog" || n == "kb_get" {
			t.Fatalf("block_sensitive tools/list must hide knowledge tools: %v", names)
		}
	}
	// A non-knowledge tool still works; whoami reports the
	// effective BYOA tier.
	text = resultText(t, e.callTool(t, e.mgdTok, "whoami", map[string]any{}))
	if !strings.Contains(text, `"tier":"byoa"`) {
		t.Fatalf("whoami under block_sensitive: %s, want effective tier byoa", text)
	}
	// downgrade: internal knowledge still readable (effective BYOA
	// caps the level, it does not cut knowledge), confidential
	// knowledge is not.
	setAction("downgrade")
	text = resultText(t, e.callTool(t, e.mgdTok, "kb_search", map[string]any{"query": "报销"}))
	if !strings.Contains(text, "kb_reimburse") {
		t.Fatalf("downgrade kb_search internal: %s", text)
	}
	text = resultText(t, e.callTool(t, e.mgdTok, "kb_search", map[string]any{"query": "机密资料"}))
	if strings.Contains(text, "机密资料") {
		t.Fatalf("downgrade kb_search must not leak confidential folder doc: %s", text)
	}
	// Remediation restores full managed access.
	setAction("")
	text = resultText(t, e.callTool(t, e.mgdTok, "kb_search", map[string]any{"query": "机密资料"}))
	if !strings.Contains(text, "机密资料") {
		t.Fatalf("remediated kb_search: %s, want confidential folder doc", text)
	}
}
