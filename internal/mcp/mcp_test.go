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
	"github.com/myjnathan/agentgate/internal/guard"
	"github.com/myjnathan/agentgate/internal/mcp"
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
	srv := httptest.NewServer(mcp.New(st, au, al, det))
	t.Cleanup(srv.Close)

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
