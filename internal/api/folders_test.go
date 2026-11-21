package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFolderSourcesLifecycle(t *testing.T) {
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

	// Empty at first; employees may not even list.
	code, listed := call("GET", "/api/kb/folders", adminSess, nil)
	if code != 200 {
		t.Fatalf("list folders: got %d, want 200", code)
	}
	if fs, _ := listed["folders"].([]any); len(fs) != 0 {
		t.Fatalf("folders = %v, want empty", listed)
	}
	if code, _ := call("GET", "/api/kb/folders", empSess, nil); code != 403 {
		t.Fatalf("employee list folders: got %d, want 403", code)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "制度.md"), []byte("# 制度\n\n内容。\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Validation: missing folder, bad level, duplicate path.
	if code, _ := call("POST", "/api/kb/folders", adminSess, map[string]any{
		"name": "x", "path": filepath.Join(dir, "nope")}); code != 400 {
		t.Fatalf("missing path: got %d, want 400", code)
	}
	if code, _ := call("POST", "/api/kb/folders", adminSess, map[string]any{
		"name": "x", "path": dir, "level": "topsecret"}); code != 400 {
		t.Fatalf("bad level: got %d, want 400", code)
	}
	if code, _ := call("POST", "/api/kb/folders", empSess, map[string]any{
		"name": "x", "path": dir}); code != 403 {
		t.Fatalf("employee create folder: got %d, want 403", code)
	}

	code, created := call("POST", "/api/kb/folders", adminSess, map[string]any{
		"name": "测试制度", "path": dir, "level": "internal"})
	if code != 201 {
		t.Fatalf("create folder: got %d (%v), want 201", code, created)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("create folder returned no id")
	}
	if created["docs"] != float64(1) {
		t.Fatalf("created docs = %v, want 1", created["docs"])
	}
	if code, _ := call("POST", "/api/kb/folders", adminSess, map[string]any{
		"name": "重复", "path": dir}); code != 409 {
		t.Fatalf("duplicate path: got %d, want 409", code)
	}

	// Rescan picks up a new file; stats report the increment.
	if err := os.WriteFile(filepath.Join(dir, "新增.md"), []byte("# 新增\n\n新内容。\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, rescanned := call("POST", "/api/kb/folders/"+id+"/rescan", adminSess, map[string]any{})
	if code != 200 {
		t.Fatalf("rescan: got %d (%v), want 200", code, rescanned)
	}
	stats, _ := rescanned["stats"].(map[string]any)
	if stats["added"] != float64(1) || rescanned["docs"] != float64(2) {
		t.Fatalf("rescan = %+v, want 1 added / 2 docs", rescanned)
	}
	if code, _ := call("POST", "/api/kb/folders/"+id+"/rescan", empSess, map[string]any{}); code != 403 {
		t.Fatalf("employee rescan: got %d, want 403", code)
	}
	if code, _ := call("POST", "/api/kb/folders/kbf_nope/rescan", adminSess, map[string]any{}); code != 404 {
		t.Fatalf("rescan unknown: got %d, want 404", code)
	}

	// List shows the source with its doc count; delete removes it.
	code, listed = call("GET", "/api/kb/folders", adminSess, nil)
	if code != 200 {
		t.Fatalf("list after create: got %d", code)
	}
	raw, _ := json.Marshal(listed)
	if !strings.Contains(string(raw), "测试制度") || !strings.Contains(string(raw), `"docs":2`) {
		t.Fatalf("list = %s", raw)
	}
	if code, _ := call("DELETE", "/api/kb/folders/"+id, empSess, nil); code != 403 {
		t.Fatalf("employee delete: got %d, want 403", code)
	}
	code, _ = call("DELETE", "/api/kb/folders/"+id, adminSess, nil)
	if code != 200 {
		t.Fatalf("delete: got %d, want 200", code)
	}
	if code, _ := call("DELETE", "/api/kb/folders/"+id, adminSess, nil); code != 404 {
		t.Fatalf("delete twice: got %d, want 404", code)
	}
}
