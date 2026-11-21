package mcp_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/federation"
	"github.com/myjnathan/agentgate/internal/guard"
	"github.com/myjnathan/agentgate/internal/mcp"
	"github.com/myjnathan/agentgate/internal/restadapter"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/seed"
	"github.com/myjnathan/agentgate/internal/store"
)

type testEnv struct {
	srv     *httptest.Server
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
	srv := httptest.NewServer(mcp.New(st, au, al, det, fed, rest))
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
			}}})
			return
		}
		text := "ok"
		switch req.Params.Name {
		case "leave_balance":
			text = `{"balance": 5, "employee": "lixiaofeng"}`
		case "project_status":
			text = "XH-200 进度正常"
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
		if _, err := au.ApproveDeviceCode(dc.Code, user, "test device"); err != nil {
			t.Fatal(err)
		}
		_, plain := au.PollDeviceCode(dc.Code, dc.Secret)
		if plain == "" {
			t.Fatal("no token issued")
		}
		return plain
	}
	return &testEnv{srv: srv,
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
	r := e.callTool(t, e.byoaTok, "kb_get", map[string]any{"doc_id": "kb_register"})
	if r.Error == nil || r.Error.Code != -32003 {
		t.Fatalf("BYOA kb_get: got %+v, want RPC error -32003 (tier insufficient)", r)
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
	r := e.callTool(t, e.byoaTok, "ops.restart_service", map[string]any{})
	if r.Error == nil || r.Error.Code != -32003 {
		t.Fatalf("BYOA call to managed-only upstream: got %+v, want RPC error -32003", r)
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
	r := e.callTool(t, e.byoaTok, "rest.fake_request", map[string]any{"employee_id": "li", "days": float64(1)})
	if r.Error == nil || r.Error.Code != -32003 {
		t.Fatalf("BYOA call to managed-only REST tool: got %+v, want RPC error -32003", r)
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
