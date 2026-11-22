package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/myjnathan/agentgate/internal/api"
	"github.com/myjnathan/agentgate/internal/audit"
	"github.com/myjnathan/agentgate/internal/auth"
	"github.com/myjnathan/agentgate/internal/folderkb"
	"github.com/myjnathan/agentgate/internal/secretbox"
	"github.com/myjnathan/agentgate/internal/seed"
	"github.com/myjnathan/agentgate/internal/semidx"
	"github.com/myjnathan/agentgate/internal/store"
)

func setupSemanticAPI(t *testing.T) (*httptest.Server, string, string, *folderkb.Registry) {
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
	fk := folderkb.New(st)
	rest.SetFolderKB(fk)
	rest.SetSemanticIndex(semidx.New())
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
	return srv, login("admin", "agentgate-admin"), login("lixiaofeng", "demo12345"), fk
}

func semanticCall(t *testing.T, srv *httptest.Server, method, path, sess string, body any) (int, map[string]any) {
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

func TestSemanticCoverageAndEntries(t *testing.T) {
	srv, adminSess, empSess, fk := setupSemanticAPI(t)

	intDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(intDir, "考勤与请假制度.md"), []byte(
		"# 考勤与请假制度\n\n本制度适用于全体员工。\n\n## 请假流程\n\n员工请假须提前提交申请，年假每年 5 天起。\n\n## 加班管理\n\n加班须提前报备，每月不得超过 36 小时。\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	secDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(secDir, "绝密纪要.md"), []byte(
		"# 绝密纪要\n\n## 决议一\n\n内容略。\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Employees may not touch the semantic endpoints at all.
	if code, _ := semanticCall(t, srv, "GET", "/api/kb/semantic/coverage", empSess, nil); code != 403 {
		t.Fatalf("employee coverage: got %d, want 403", code)
	}

	code, created := semanticCall(t, srv, "POST", "/api/kb/folders", adminSess, map[string]any{
		"name": "公司制度", "path": intDir, "level": "internal", "department": "人力部"})
	if code != 201 {
		t.Fatalf("create internal folder: got %d (%v), want 201", code, created)
	}
	intID, _ := created["id"].(string)
	code, created = semanticCall(t, srv, "POST", "/api/kb/folders", adminSess, map[string]any{
		"name": "绝密资料", "path": secDir, "level": "secret"})
	if code != 201 {
		t.Fatalf("create secret folder: got %d (%v), want 201", code, created)
	}

	// Coverage: both documents indexed, no drift.
	code, cov := semanticCall(t, srv, "GET", "/api/kb/semantic/coverage", adminSess, nil)
	if code != 200 {
		t.Fatalf("coverage: got %d (%v), want 200", code, cov)
	}
	c := cov["coverage"].(map[string]any)
	if c["docs_total"] != float64(2) || c["docs_indexed"] != float64(2) {
		t.Fatalf("coverage = %v, want 2 docs indexed", c)
	}
	if c["chapters_total"] != float64(5) || c["chapters_indexed"] != float64(5) {
		t.Fatalf("coverage chapters = %v, want 5/5 (root + 2 + root + 1)", c)
	}
	if len(c["stale_docs"].([]any)) != 0 {
		t.Fatalf("stale docs = %v, want none", c["stale_docs"])
	}

	// The internal document's entries carry FRAS fields, tags and a
	// back-reference on every entry.
	docs := c["documents"].([]any)
	var intDocID string
	for _, d := range docs {
		m := d.(map[string]any)
		if m["title"] == "考勤与请假制度" {
			intDocID = m["doc_id"].(string)
		}
		if m["title"] == "绝密纪要" && m["generated_by"] != "title-only" {
			t.Errorf("secret doc generated_by = %v, want title-only", m["generated_by"])
		}
	}
	if intDocID == "" {
		t.Fatal("internal doc missing from coverage")
	}
	code, got := semanticCall(t, srv, "GET", "/api/kb/semantic/entries?doc_id="+intDocID, adminSess, nil)
	if code != 200 {
		t.Fatalf("entries: got %d (%v), want 200", code, got)
	}
	entries := got["entries"].([]any)
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	for _, raw := range entries {
		e := raw.(map[string]any)
		for _, f := range []string{"function", "relations", "applicability", "constraints"} {
			if e[f] == "" {
				t.Errorf("entry %v missing %s", e["node_id"], f)
			}
		}
		src := e["source"].(map[string]any)
		if src["file"] == "" || src["chapter"] == "" || src["pages"] == "" {
			t.Errorf("entry %v missing back-reference: %v", e["node_id"], src)
		}
		tags := e["tags"].(map[string]any)
		if tags["department"] != "人力部" || tags["importance"] != "medium" {
			t.Errorf("entry tags = %v", tags)
		}
	}

	// Secret entries are title-only: no FRAS content beyond the
	// chapter title, but the citation is still there.
	var secDocID string
	for _, d := range docs {
		m := d.(map[string]any)
		if m["title"] == "绝密纪要" {
			secDocID = m["doc_id"].(string)
		}
	}
	code, got = semanticCall(t, srv, "GET", "/api/kb/semantic/entries?doc_id="+secDocID, adminSess, nil)
	if code != 200 {
		t.Fatalf("secret entries: got %d, want 200", code)
	}
	for _, raw := range got["entries"].([]any) {
		e := raw.(map[string]any)
		if e["generated_by"] != "title-only" || e["relations"] != "" || e["constraints"] != "" {
			t.Errorf("secret entry must be title-only: %v", e)
		}
		if e["function"] == "" {
			t.Errorf("title-only entry must carry the chapter title")
		}
	}

	// Unknown doc / missing doc_id / employee.
	if code, _ := semanticCall(t, srv, "GET", "/api/kb/semantic/entries?doc_id=folder:nope:x.md", adminSess, nil); code != 404 {
		t.Fatalf("unknown doc entries: got %d, want 404", code)
	}
	if code, _ := semanticCall(t, srv, "GET", "/api/kb/semantic/entries", adminSess, nil); code != 400 {
		t.Fatalf("missing doc_id: got %d, want 400", code)
	}
	if code, _ := semanticCall(t, srv, "GET", "/api/kb/semantic/entries?doc_id="+intDocID, empSess, nil); code != 403 {
		t.Fatalf("employee entries: got %d, want 403", code)
	}

	// Drift: edit the file and re-index the folder directly (bypassing
	// the API's automatic semantic refresh): the folder index moved
	// on, the semantic index did not, so coverage reports the doc
	// stale. A rescan through the API regenerates and clears it.
	if err := os.WriteFile(filepath.Join(intDir, "考勤与请假制度.md"), []byte(
		"# 考勤与请假制度\n\n本制度适用于全体员工。\n\n## 请假流程\n\n员工请假须提前提交申请，年假每年 5 天起。\n\n## 加班管理\n\n加班须提前报备，每月不得超过 36 小时。\n\n## 考勤打卡\n\n标准工作时间为 9:00-18:00。\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src, ok := fk.Source(intID)
	if !ok {
		t.Fatalf("folder source %s not found", intID)
	}
	if _, err := fk.Rescan(src); err != nil {
		t.Fatal(err)
	}
	_, cov = semanticCall(t, srv, "GET", "/api/kb/semantic/coverage", adminSess, nil)
	c = cov["coverage"].(map[string]any)
	if len(c["stale_docs"].([]any)) != 1 {
		t.Fatalf("after folder rescan without semantic refresh: stale = %v, want 1 doc", c["stale_docs"])
	}
	if c["chapters_total"] != float64(6) {
		t.Fatalf("chapters_total = %v, want 6 after adding a chapter", c["chapters_total"])
	}
	code, rs := semanticCall(t, srv, "POST", "/api/kb/folders/"+intID+"/rescan", adminSess, map[string]any{})
	if code != 200 {
		t.Fatalf("rescan: got %d (%v), want 200", code, rs)
	}
	_, cov = semanticCall(t, srv, "GET", "/api/kb/semantic/coverage", adminSess, nil)
	c = cov["coverage"].(map[string]any)
	if len(c["stale_docs"].([]any)) != 0 || c["chapters_indexed"] != float64(6) {
		t.Fatalf("after rescan: coverage = %v, want no stale and 6 chapters indexed", c)
	}
}

func TestSemanticRegenerateModel(t *testing.T) {
	srv, adminSess, _, _ := setupSemanticAPI(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "制度.md"), []byte(
		"# 制度\n\n总则。\n\n## 第一条\n\n内容一。\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, created := semanticCall(t, srv, "POST", "/api/kb/folders", adminSess, map[string]any{
		"name": "测试制度", "path": dir, "level": "internal"})
	if code != 201 {
		t.Fatalf("create folder: got %d (%v), want 201", code, created)
	}
	fid, _ := created["id"].(string)

	// No model configured and no inline endpoint: 503.
	if code, body := semanticCall(t, srv, "POST", "/api/kb/folders/"+fid+"/semantic", adminSess,
		map[string]any{"model": true}); code != 503 {
		t.Fatalf("model without endpoint: got %d (%v), want 503", code, body)
	}

	// A fake model that returns real FRAS JSON: entries become model
	// entries, with the back-reference still from the tree.
	modelSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-AgentGate-Data-Level") != "internal" {
			t.Errorf("model request data-level header = %q, want internal", r.Header.Get("X-AgentGate-Data-Level"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices": [{"message": {"content": "{\"entries\": [` +
			`{\"node_id\": \"n0\", \"function\": \"制度总则。\", \"relations\": \"包含章节：第一条。\", \"applicability\": \"适用于全体员工。\", \"constraints\": \"无。\", \"tags\": {\"department\": \"人力部\", \"type\": \"制度\", \"importance\": \"medium\"}}, ` +
			`{\"node_id\": \"n1\", \"function\": \"第一条的内容。\", \"relations\": \"上级章节：制度。\", \"applicability\": \"适用于全体员工。\", \"constraints\": \"内容一。\", \"tags\": {\"department\": \"人力部\", \"type\": \"制度\", \"importance\": \"medium\"}}] }"}}]}`))
	}))
	defer modelSrv.Close()
	code, out := semanticCall(t, srv, "POST", "/api/kb/folders/"+fid+"/semantic", adminSess,
		map[string]any{"model": true, "model_url": modelSrv.URL, "model_token": "tok-secret", "model_name": "mock-eco"})
	if code != 200 {
		t.Fatalf("model regenerate: got %d (%v), want 200", code, out)
	}
	if out["model_fallback"] != false {
		t.Fatalf("model regenerate should not fall back: %v", out)
	}
	docs := out["documents"].([]any)
	if len(docs) != 1 || docs[0].(map[string]any)["generated_by"] != "model" {
		t.Fatalf("regenerate result = %v, want one model-generated doc", out)
	}
	// The token must never be echoed back.
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "tok-secret") {
		t.Errorf("response leaked the model token: %s", raw)
	}

	// A mock-LLM-style echo server: the reply is not entries JSON,
	// so the document falls back to extractive entries and the
	// failure is reported.
	echoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{
			{"message": map[string]any{"content": req.Messages[0].Content}},
		}})
	}))
	defer echoSrv.Close()
	code, out = semanticCall(t, srv, "POST", "/api/kb/folders/"+fid+"/semantic", adminSess,
		map[string]any{"model": true, "model_url": echoSrv.URL, "model_name": "mock-eco"})
	if code != 200 {
		t.Fatalf("echo regenerate: got %d (%v), want 200", code, out)
	}
	if out["model_fallback"] != true {
		t.Fatalf("echo model should fall back: %v", out)
	}
	docs = out["documents"].([]any)
	if docs[0].(map[string]any)["generated_by"] != "extractive" ||
		docs[0].(map[string]any)["model_error"] == "" {
		t.Fatalf("echo fallback result = %v, want extractive + model_error", out)
	}

	// Extractive regeneration still works and clears the model error.
	code, out = semanticCall(t, srv, "POST", "/api/kb/folders/"+fid+"/semantic", adminSess, map[string]any{})
	if code != 200 || out["model_fallback"] != false {
		t.Fatalf("extractive regenerate: got %d (%v), want 200 without fallback", code, out)
	}
	if docs[0].(map[string]any)["generated_by"] != "extractive" {
		t.Fatalf("extractive result = %v", out)
	}
	if code, _ := semanticCall(t, srv, "POST", "/api/kb/folders/nope/semantic", adminSess, map[string]any{}); code != 404 {
		t.Fatalf("unknown folder regenerate: got %d, want 404", code)
	}
}
