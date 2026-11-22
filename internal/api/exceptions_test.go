package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/myjnathan/agentgate/internal/store"
)

func TestExceptionApprovalFlow(t *testing.T) {
	srv, st, adminSess, empSess := setupTokensAPI(t)

	call := func(method, path, auth string, body any) (int, map[string]any) {
		t.Helper()
		var buf *bytes.Buffer
		if body != nil {
			b, _ := json.Marshal(body)
			buf = bytes.NewBuffer(b)
		} else {
			buf = bytes.NewBuffer(nil)
		}
		req, _ := http.NewRequest(method, srv.URL+path, buf)
		req.Header.Set("Content-Type", "application/json")
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
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

	// No session at all: 401.
	if code, _ := call("POST", "/api/exceptions", "", map[string]any{
		"data_level": "confidential", "reason": "need", "scope": "mock-pro", "hours": 4}); code != 401 {
		t.Fatalf("apply without auth: want 401, got %d", code)
	}
	// Validation: internal data needs no exception, empty reason,
	// bad hours, and top secret is closed by default.
	for _, bad := range []map[string]any{
		{"data_level": "internal", "reason": "need", "scope": "mock-pro", "hours": 4},
		{"data_level": "confidential", "reason": "", "scope": "mock-pro", "hours": 4},
		{"data_level": "confidential", "reason": "need", "scope": "mock-pro", "hours": 0},
		{"data_level": "secret", "reason": "need", "scope": "mock-pro", "hours": 4},
		{"data_level": "bogus", "reason": "need", "scope": "mock-pro", "hours": 4},
	} {
		if code, out := call("POST", "/api/exceptions", empSess, bad); code != 400 {
			t.Fatalf("bad application %+v: want 400, got %d %v", bad, code, out)
		}
	}
	// A good application: 201, pending, and no grant anywhere.
	code, created := call("POST", "/api/exceptions", empSess, map[string]any{
		"data_level": "confidential", "reason": "XH-200 季度汇总需走自带 Agent",
		"scope": "mock-pro 调用，汇总星河精密 XH-200 进度", "hours": 4})
	if code != 201 {
		t.Fatalf("apply: want 201, got %d %v", code, created)
	}
	id, _ := created["id"].(string)
	if id == "" || created["status"] != "pending" {
		t.Fatalf("created exception shape wrong: %v", created)
	}
	if _, hasGrant := created["grant"]; hasGrant {
		t.Fatal("pending request must not carry a grant")
	}
	if _, hasHash := created["grant_hash"]; hasHash {
		t.Fatal("grant hash must never be exposed by the API")
	}

	// The applicant sees it in their own list; the admin sees all.
	code, mine := call("GET", "/api/exceptions", empSess, nil)
	if code != 200 {
		t.Fatalf("employee list: want 200, got %d", code)
	}
	list, _ := mine["exceptions"].([]any)
	if len(list) != 1 {
		t.Fatalf("employee list: want 1 own request, got %v", mine)
	}
	code, all := call("GET", "/api/exceptions?status=pending", adminSess, nil)
	if code != 200 {
		t.Fatalf("admin list: want 200, got %d", code)
	}
	if alist, _ := all["exceptions"].([]any); len(alist) != 1 {
		t.Fatalf("admin list: want 1 pending, got %v", all)
	}

	// Employees cannot approve; only admins can.
	if code, _ := call("POST", "/api/exceptions/"+id+"/approve", empSess, map[string]any{}); code != 403 {
		t.Fatalf("employee approve: want 403, got %d", code)
	}
	// Approve: 200, status approved, and the grant appears exactly
	// once — in this response only.
	code, approved := call("POST", "/api/exceptions/"+id+"/approve", adminSess, map[string]any{"note": "同意，仅此一次"})
	if code != 200 {
		t.Fatalf("approve: want 200, got %d %v", code, approved)
	}
	grant, _ := approved["grant"].(string)
	if grant == "" || len(grant) < 40 {
		t.Fatalf("approve must return the one-time grant once: %v", approved)
	}
	exc, _ := approved["exception"].(map[string]any)
	if exc["status"] != "approved" || exc["expires_at"] == nil || exc["decided_by"] != "admin" {
		t.Fatalf("approved exception shape wrong: %v", approved)
	}
	// A second read never returns the grant again.
	code, got := call("GET", "/api/exceptions/"+id, empSess, nil)
	if code != 200 {
		t.Fatalf("get after approve: want 200, got %d", code)
	}
	if _, hasGrant := got["grant"]; hasGrant {
		t.Fatal("grant must be returned exactly once (the approve response)")
	}
	// A decided request cannot be decided again.
	if code, _ := call("POST", "/api/exceptions/"+id+"/approve", adminSess, map[string]any{}); code != 400 {
		t.Fatalf("re-approve: want 400, got %d", code)
	}
	if code, _ := call("POST", "/api/exceptions/"+id+"/reject", adminSess, map[string]any{}); code != 400 {
		t.Fatalf("reject after approve: want 400, got %d", code)
	}
	// Revoke cancels the unused grant; a revoked grant is inert.
	if code, revoked := call("POST", "/api/exceptions/"+id+"/revoke", adminSess, map[string]any{"note": "需求取消"}); code != 200 {
		t.Fatalf("revoke: want 200, got %d %v", code, revoked)
	} else if revoked["status"] != "revoked" {
		t.Fatalf("revoked status wrong: %v", revoked)
	}

	// A second request goes through the reject path.
	code, second := call("POST", "/api/exceptions", empSess, map[string]any{
		"data_level": "confidential", "reason": "另一次调用",
		"scope": "mock-pro 调用", "hours": 2})
	if code != 201 {
		t.Fatalf("second apply: want 201, got %d %v", code, second)
	}
	id2, _ := second["id"].(string)
	code, rejected := call("POST", "/api/exceptions/"+id2+"/reject", adminSess, map[string]any{"note": "理由不足"})
	if code != 200 || rejected["status"] != "rejected" {
		t.Fatalf("reject: want 200 rejected, got %d %v", code, rejected)
	}
	// The store only ever held hashes: the grant token issued above
	// is nowhere in the persisted record — only its SHA-256.
	var hash string
	st.Read(func(s *store.State) {
		hash = s.Exceptions[id].GrantHash
	})
	if hash == "" || hash == grant {
		t.Fatalf("store must hold only the grant hash, got %q", hash)
	}
}
