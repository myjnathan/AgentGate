package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestPromptsLifecycle(t *testing.T) {
	srv, _, adminSess, empSess := setupTokensAPI(t)

	call := func(method, path, sess string, body any) (int, map[string]any) {
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

	// The demo seed publishes weekly_report: an employee sees it.
	code, listed := call("GET", "/api/prompts", empSess, nil)
	if code != 200 {
		t.Fatalf("list prompts: got %d, want 200", code)
	}
	raw, _ := json.Marshal(listed)
	if !strings.Contains(string(raw), "weekly_report") {
		t.Fatalf("seeded weekly_report missing from list: %v", listed)
	}

	// Bad name, undeclared placeholder, duplicate name.
	if code, _ := call("POST", "/api/prompts", adminSess, map[string]any{
		"name": "WeeklyReport", "content": "hi {{name}}",
		"arguments": []any{map[string]any{"name": "name", "required": true}}}); code != 400 {
		t.Fatalf("bad template name: got %d, want 400", code)
	}
	if code, _ := call("POST", "/api/prompts", adminSess, map[string]any{
		"name": "greet", "content": "hi {{unknown}}",
		"arguments": []any{map[string]any{"name": "name", "required": true}}}); code != 400 {
		t.Fatalf("undeclared placeholder: got %d, want 400", code)
	}

	// An employee may submit a template (skill pipeline: submit →
	// review → publish); it starts in_review and is invisible to
	// other employees.
	code, created := call("POST", "/api/prompts", empSess, map[string]any{
		"name": "greet", "description": "问候模板", "scope": "*",
		"content": "你好，{{name}}，欢迎来到{{department}}。",
		"arguments": []any{
			map[string]any{"name": "name", "description": "姓名", "required": true},
			map[string]any{"name": "department", "description": "部门", "required": true},
		},
		"max_data_level": "internal", "min_tier": "byoa"})
	if code != 201 {
		t.Fatalf("create prompt: got %d (%v), want 201", code, created)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("create prompt returned no id")
	}
	if created["status"] != "in_review" {
		t.Fatalf("new prompt status = %v, want in_review", created["status"])
	}
	if code, _ := call("POST", "/api/prompts", adminSess, map[string]any{
		"name": "greet", "content": "x"}); code != 409 {
		t.Fatalf("duplicate template name: got %d, want 409", code)
	}

	// A new version does not change what is published (nothing is
	// published yet for this template).
	code, ver := call("POST", "/api/prompts/"+id+"/versions", empSess, map[string]any{
		"content":   "你好，{{name}}！",
		"arguments": []any{map[string]any{"name": "name", "required": true}}})
	if code != 201 || ver["version"] != float64(2) {
		t.Fatalf("add version: got %d %v, want 201 version 2", code, ver)
	}
	code, got := call("GET", "/api/prompts/"+id, adminSess, nil)
	if code != 200 || got["published_version"] != float64(0) {
		t.Fatalf("new version must not publish itself: got %d %v", code, got)
	}

	// Admin review approve publishes the current version (v2).
	code, rev := call("POST", "/api/prompts/"+id+"/review", adminSess, map[string]any{"approve": true})
	if code != 200 || rev["status"] != "published" || rev["published_version"] != float64(2) {
		t.Fatalf("review approve: got %d %v, want published v2", code, rev)
	}

	// Rollback: publish v1 explicitly.
	code, pub := call("POST", "/api/prompts/"+id+"/publish", adminSess, map[string]any{"version": 1})
	if code != 200 || pub["published_version"] != float64(1) {
		t.Fatalf("publish v1 (rollback): got %d %v, want published_version 1", code, pub)
	}
	// Publishing a version that does not exist is a 400.
	if code, _ := call("POST", "/api/prompts/"+id+"/publish", adminSess, map[string]any{"version": 9}); code != 400 {
		t.Fatalf("publish unknown version: got %d, want 400", code)
	}
	// Review and publish are admin-only.
	if code, _ := call("POST", "/api/prompts/"+id+"/publish", empSess, map[string]any{"version": 2}); code != 403 {
		t.Fatalf("employee publish: got %d, want 403", code)
	}
	if code, _ := call("POST", "/api/prompts/"+id+"/review", empSess, map[string]any{"approve": true}); code != 403 {
		t.Fatalf("employee review: got %d, want 403", code)
	}
	// Delete and re-delete.
	if code, _ := call("DELETE", "/api/prompts/"+id, adminSess, nil); code != 200 {
		t.Fatalf("delete prompt: got %d, want 200", code)
	}
	if code, _ := call("DELETE", "/api/prompts/"+id, adminSess, nil); code != 404 {
		t.Fatalf("delete again: got %d, want 404", code)
	}
	// Unknown id.
	if code, _ := call("GET", "/api/prompts/pt_nope", adminSess, nil); code != 404 {
		t.Fatalf("get unknown prompt: got %d, want 404", code)
	}
}
