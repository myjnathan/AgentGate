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

func setupTokensAPI(t *testing.T) (*httptest.Server, *store.Store, string, string) {
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
	// Mint one BYOA token for lixiaofeng via the device-code flow.
	var li *store.User
	st.Read(func(s *store.State) {
		for _, u := range s.Users {
			if u.Username == "lixiaofeng" {
				cp := *u
				li = &cp
			}
		}
	})
	dc := au.StartDeviceCode("claude-code", store.TierBYOA)
	if _, err := au.ApproveDeviceCode(dc.Code, li, "test device"); err != nil {
		t.Fatal(err)
	}
	return srv, st, login("admin", "agentgate-admin"), login("lixiaofeng", "demo12345")
}

type tokenShape struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	AgentType  string `json:"agent_type"`
	Tier       string `json:"tier"`
	RPMLimit   int    `json:"rpm_limit"`
	TPMLimit   int    `json:"tpm_limit"`
	MonthlyCap int64  `json:"monthly_cap_tokens"`
	UsedTokens int64  `json:"used_tokens"`
	Revoked    bool   `json:"revoked"`
}

func TestTokensEndpoints(t *testing.T) {
	srv, st, adminSess, empSess := setupTokensAPI(t)

	var tokID string
	st.Read(func(s *store.State) {
		for id := range s.Tokens {
			tokID = id
		}
	})
	if tokID == "" {
		t.Fatal("no token minted")
	}

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

	// List: admin sees the token (no hash anywhere in the payload).
	code, d := call("GET", "/api/tokens", adminSess, nil)
	if code != 200 {
		t.Fatalf("GET /api/tokens = %d, want 200", code)
	}
	raw, _ := json.Marshal(d)
	if strings.Contains(string(raw), "hash") {
		t.Fatal("token list must not expose hashes")
	}
	toks, _ := d["tokens"].([]any)
	if len(toks) != 1 {
		t.Fatalf("tokens = %d, want 1", len(toks))
	}
	first, _ := json.Marshal(toks[0])
	var tv tokenShape
	_ = json.Unmarshal(first, &tv)
	if tv.Username != "lixiaofeng" || tv.AgentType != "claude-code" || tv.Tier != "byoa" {
		t.Fatalf("token view = %+v, want lixiaofeng/claude-code/byoa", tv)
	}
	if tv.RPMLimit != 0 || tv.TPMLimit != 0 {
		t.Fatalf("fresh token limits = %d/%d, want 0/0 (tier default)", tv.RPMLimit, tv.TPMLimit)
	}

	// Gating.
	if code, _ := call("GET", "/api/tokens", empSess, nil); code != 403 {
		t.Fatalf("employee list: %d, want 403", code)
	}
	if code, _ := call("GET", "/api/tokens", "", nil); code != 401 {
		t.Fatalf("anonymous list: %d, want 401", code)
	}

	// Update limits.
	code, d = call("PATCH", "/api/tokens/"+tokID, adminSess, map[string]any{"rpm_limit": 5, "tpm_limit": 1000})
	if code != 200 {
		t.Fatalf("PATCH = %d, want 200 (%v)", code, d)
	}
	raw, _ = json.Marshal(d)
	_ = json.Unmarshal(raw, &tv)
	if tv.RPMLimit != 5 || tv.TPMLimit != 1000 {
		t.Fatalf("updated token = %+v, want rpm 5 tpm 1000", tv)
	}
	st.Read(func(s *store.State) {
		if s.Tokens[tokID].RPM != 5 || s.Tokens[tokID].TPM != 1000 {
			t.Fatal("limits not persisted in store")
		}
	})

	// Partial update leaves the other limit untouched.
	code, d = call("PATCH", "/api/tokens/"+tokID, adminSess, map[string]any{"rpm_limit": 10})
	if code != 200 {
		t.Fatalf("partial PATCH = %d, want 200", code)
	}
	raw, _ = json.Marshal(d)
	_ = json.Unmarshal(raw, &tv)
	if tv.RPMLimit != 10 || tv.TPMLimit != 1000 {
		t.Fatalf("partial update = %+v, want rpm 10 tpm 1000", tv)
	}

	// Validation and gating.
	if code, _ := call("PATCH", "/api/tokens/"+tokID, adminSess, map[string]any{"rpm_limit": -1}); code != 400 {
		t.Fatalf("negative rpm: %d, want 400", code)
	}
	if code, _ := call("PATCH", "/api/tokens/tok_missing", adminSess, map[string]any{"rpm_limit": 1}); code != 404 {
		t.Fatalf("unknown token: %d, want 404", code)
	}
	if code, _ := call("PATCH", "/api/tokens/"+tokID, empSess, map[string]any{"rpm_limit": 1}); code != 403 {
		t.Fatalf("employee patch: %d, want 403", code)
	}
}
