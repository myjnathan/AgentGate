package api_test

import (
	"os"
	"path/filepath"
	"testing"
)

// TestKnowledgePackagesLifecycle covers the F22 curation flow:
// create (recipe + synthesis + citations) → review → publish →
// version → rollback → delete, plus the governance rejections
// (unknown source, citation outside the recipe, upward reference,
// stale publish) and the visibility filter.
func TestKnowledgePackagesLifecycle(t *testing.T) {
	srv, adminSess, empSess, _ := setupSemanticAPI(t)
	call := func(method, path, sess string, body any) (int, map[string]any) {
		return semanticCall(t, srv, method, path, sess, body)
	}

	intDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(intDir, "考勤与请假制度.md"), []byte(
		"# 考勤与请假制度\n\n本制度适用于全体员工。\n\n## 请假流程\n\n员工请假须提前提交申请，年假每年 5 天起。\n\n## 加班管理\n\n加班须提前报备，每月不得超过 36 小时。\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	confDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(confDir, "机密资料.md"), []byte(
		"# 机密资料\n\n仅受管级可见。\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, fint := call("POST", "/api/kb/folders", adminSess, map[string]any{
		"name": "公司制度", "path": intDir, "level": "internal"})
	if code != 201 {
		t.Fatalf("register internal folder: got %d (%v)", code, fint)
	}
	intFolderID, _ := fint["id"].(string)
	code, fconf := call("POST", "/api/kb/folders", adminSess, map[string]any{
		"name": "机密资料", "path": confDir, "level": "confidential"})
	if code != 201 {
		t.Fatalf("register confidential folder: got %d (%v)", code, fconf)
	}
	confFolderID, _ := fconf["id"].(string)
	intDoc := "folder:" + intFolderID + ":考勤与请假制度.md"
	confDoc := "folder:" + confFolderID + ":机密资料.md"

	mkPkg := func(name string, sources []any, cites []any, refs []any) map[string]any {
		return map[string]any{"name": name, "description": "测试包", "scope": "*",
			"content": "综合：请假须提前提交申请，加班每月不得超过 36 小时。",
			"sources": sources, "citations": cites, "references": refs, "change_note": "初版"}
	}
	intSrc := []any{map[string]any{"doc_id": intDoc}}
	intCit := []any{map[string]any{"doc_id": intDoc, "node_id": "n1"}}

	// Drafting is open to any employee (Agent 起草 → 管理员审批):
	// an employee may create a package and add versions to their
	// own, but may not touch another author's package, review,
	// publish or delete.
	if code, _ := call("POST", "/api/kb/packages", empSess, mkPkg("employee_draft", intSrc, intCit, nil)); code != 201 {
		t.Fatalf("employee create own draft: got %d, want 201", code)
	}
	// The employee draft is cleaned up at the end of the test via
	// the admin list; its id is found by name.
	code, empListed := call("GET", "/api/kb/packages", adminSess, nil)
	empDraftID := ""
	if pkgs, _ := empListed["packages"].([]any); pkgs != nil {
		for _, p := range pkgs {
			if pm, _ := p.(map[string]any); pm["name"] == "employee_draft" {
				empDraftID, _ = pm["id"].(string)
			}
		}
	}
	if empDraftID == "" {
		t.Fatal("employee draft not listed for admin")
	}
	if code, _ := call("POST", "/api/kb/packages/"+empDraftID+"/versions", empSess,
		map[string]any{"content": "综合 v2。", "sources": intSrc,
			"citations": intCit, "change_note": "v2"}); code != 201 {
		t.Fatalf("employee add version to own draft: got %d, want 201", code)
	}
	if code, _ := call("POST", "/api/kb/packages", adminSess, mkPkg("Bad Name", intSrc, intCit, nil)); code != 400 {
		t.Fatalf("bad name: got %d, want 400", code)
	}
	if code, body := call("POST", "/api/kb/packages", adminSess, mkPkg("onboarding_essentials",
		[]any{map[string]any{"doc_id": "folder:nope:x.md"}}, intCit, nil)); code != 400 {
		t.Fatalf("unknown source: got %d (%v), want 400", code, body)
	}
	if code, _ := call("POST", "/api/kb/packages", adminSess, mkPkg("onboarding_essentials", intSrc,
		[]any{map[string]any{"doc_id": intDoc, "node_id": "n9"}}, nil)); code != 400 {
		t.Fatalf("unknown section: got %d, want 400", code)
	}
	if code, _ := call("POST", "/api/kb/packages", adminSess, mkPkg("onboarding_essentials", intSrc,
		[]any{map[string]any{"doc_id": confDoc, "node_id": "n0"}}, nil)); code != 400 {
		t.Fatalf("citation outside recipe: got %d, want 400", code)
	}
	if code, _ := call("POST", "/api/kb/packages", adminSess, mkPkg("onboarding_essentials", intSrc, nil, nil)); code != 400 {
		t.Fatalf("no citations: got %d, want 400", code)
	}

	// Create: the citation back-reference is filled in from the
	// tree (file + chapter + pages), and the level is internal.
	code, created := call("POST", "/api/kb/packages", adminSess, mkPkg("onboarding_essentials", intSrc, intCit, nil))
	if code != 201 {
		t.Fatalf("create package: got %d (%v), want 201", code, created)
	}
	if created["level"] != "internal" {
		t.Fatalf("created level = %v, want internal", created["level"])
	}
	pkgID, _ := created["id"].(string)
	if code, _ := call("POST", "/api/kb/packages/"+pkgID+"/versions", empSess,
		map[string]any{"content": "综合 v2。", "sources": intSrc,
			"citations": intCit, "change_note": "v2"}); code != 403 {
		t.Fatalf("employee add version to another author's package: got %d, want 403", code)
	}
	if code, _ := call("POST", "/api/kb/packages", adminSess, mkPkg("onboarding_essentials", intSrc, intCit, nil)); code != 409 {
		t.Fatalf("duplicate name: got %d, want 409", code)
	}
	// A draft is invisible to employees (404, indistinguishable
	// from unknown); the author/admin sees the filled citation.
	if code, _ := call("GET", "/api/kb/packages/"+pkgID, empSess, nil); code != 404 {
		t.Fatalf("employee get draft: got %d, want 404", code)
	}
	code, got := call("GET", "/api/kb/packages/"+pkgID, adminSess, nil)
	if code != 200 {
		t.Fatalf("get package: got %d (%v)", code, got)
	}
	vers, _ := got["versions"].([]any)
	if len(vers) != 1 {
		t.Fatalf("versions = %d, want 1", len(vers))
	}
	v0 := vers[0].(map[string]any)
	cits, _ := v0["citations"].([]any)
	if len(cits) != 1 {
		t.Fatalf("citations = %d, want 1", len(cits))
	}
	c0 := cits[0].(map[string]any)
	if c0["file"] != "考勤与请假制度.md" || c0["chapter"] != "请假流程" || c0["pages"] != "1" {
		t.Fatalf("citation back-reference = %v, want file/chapter/pages from the tree", c0)
	}

	// Review → publish; a new version keeps serving v1 until it is
	// published; rollback republishes v1.
	code, rev := call("POST", "/api/kb/packages/"+pkgID+"/review", adminSess, map[string]any{"approve": true})
	if code != 200 || rev["published_version"] != float64(1) {
		t.Fatalf("review approve: got %d (%v)", code, rev)
	}
	code, listed := call("GET", "/api/kb/packages", empSess, nil)
	if code != 200 {
		t.Fatalf("employee list: got %d", code)
	}
	// The employee sees the published package plus their own draft.
	seen := map[string]bool{}
	if pkgs, _ := listed["packages"].([]any); pkgs != nil {
		for _, p := range pkgs {
			if pm, _ := p.(map[string]any); pm != nil {
				seen[pm["name"].(string)] = true
			}
		}
	}
	if !seen["onboarding_essentials"] || !seen["employee_draft"] {
		t.Fatalf("employee list = %v, want published package + own draft", seen)
	}
	code, added := call("POST", "/api/kb/packages/"+pkgID+"/versions", adminSess,
		map[string]any{"content": "综合 v2：请假与加班同上，另附考勤打卡说明。",
			"sources": intSrc, "citations": []any{map[string]any{"doc_id": intDoc, "node_id": "n1"}, map[string]any{"doc_id": intDoc, "node_id": "n2"}},
			"change_note": "补加班章节引用"})
	if code != 201 || added["version"] != float64(2) {
		t.Fatalf("add version: got %d (%v), want 201 version 2", code, added)
	}
	code, got = call("GET", "/api/kb/packages/"+pkgID, adminSess, nil)
	if got["published_version"] != float64(1) {
		t.Fatalf("published_version = %v after v2, want 1 (v1 keeps serving)", got["published_version"])
	}
	code, pub := call("POST", "/api/kb/packages/"+pkgID+"/publish", adminSess, map[string]any{"version": 2})
	if code != 200 || pub["published_version"] != float64(2) {
		t.Fatalf("publish v2: got %d (%v)", code, pub)
	}
	code, pub = call("POST", "/api/kb/packages/"+pkgID+"/publish", adminSess, map[string]any{"version": 1})
	if code != 200 || pub["published_version"] != float64(1) {
		t.Fatalf("rollback to v1: got %d (%v)", code, pub)
	}
	if code, _ := call("POST", "/api/kb/packages/"+pkgID+"/publish", adminSess, map[string]any{"version": 9}); code != 400 {
		t.Fatalf("publish unknown version: got %d, want 400", code)
	}
	if code, _ := call("POST", "/api/kb/packages/"+pkgID+"/review", empSess, map[string]any{"approve": true}); code != 403 {
		t.Fatalf("employee review: got %d, want 403", code)
	}

	// Reference rules: a confidential package is published first.
	confSrc := []any{map[string]any{"doc_id": confDoc}}
	confCit := []any{map[string]any{"doc_id": confDoc, "node_id": "n0"}}
	code, confCreated := call("POST", "/api/kb/packages", adminSess,
		map[string]any{"name": "vendor_onboarding", "description": "供应商准入", "scope": "*",
			"content": "综合：机密资料仅受管级可见。", "sources": confSrc, "citations": confCit, "change_note": "初版"})
	if code != 201 || confCreated["level"] != "confidential" {
		t.Fatalf("create confidential package: got %d (%v)", code, confCreated)
	}
	confPkgID, _ := confCreated["id"].(string)
	// An internal package may not reference a confidential one
	// (upward reference).
	if code, _ := call("POST", "/api/kb/packages", adminSess,
		mkPkg("bad_ref", intSrc, intCit, []any{confPkgID})); code != 400 {
		t.Fatalf("upward reference: got %d, want 400", code)
	}
	if code, _ := call("POST", "/api/kb/packages/"+confPkgID+"/review", adminSess, map[string]any{"approve": true}); code != 200 {
		t.Fatalf("review confidential package: got %d", code)
	}
	// A confidential package may reference the published internal
	// one (downward reference).
	code, added = call("POST", "/api/kb/packages/"+confPkgID+"/versions", adminSess,
		map[string]any{"content": "综合 v2：机密资料同上，并引用入职必读的请假流程。",
			"sources": confSrc, "citations": confCit, "references": []any{pkgID}, "change_note": "引用内部包"})
	if code != 201 || added["level"] != "confidential" {
		t.Fatalf("downward reference version: got %d (%v)", code, added)
	}
	// Publish-time revalidation: withdrawing the referenced package
	// refuses the publish with 409.
	code, _ = call("POST", "/api/kb/packages/"+pkgID+"/review", adminSess, map[string]any{"approve": false})
	if code != 200 {
		t.Fatalf("reject referenced package: got %d", code)
	}
	if code, body := call("POST", "/api/kb/packages/"+confPkgID+"/publish", adminSess, map[string]any{"version": 2}); code != 409 {
		t.Fatalf("publish with withdrawn reference: got %d (%v), want 409", code, body)
	}
	// The list flags the stale package for the curator.
	code, got = call("GET", "/api/kb/packages/"+confPkgID, adminSess, nil)
	if code != 200 || got["stale"] != true {
		t.Fatalf("stale flag = %v (code %d), want true", got["stale"], code)
	}
	// Restore the reference and publish v2.
	if code, _ := call("POST", "/api/kb/packages/"+pkgID+"/review", adminSess, map[string]any{"approve": true}); code != 200 {
		t.Fatalf("re-approve referenced package: got %d", code)
	}
	if code, _ := call("POST", "/api/kb/packages/"+confPkgID+"/publish", adminSess, map[string]any{"version": 2}); code != 200 {
		t.Fatalf("publish v2 after restore: got %d", code)
	}

	// First-publish via review re-validates the recipe too:
	// withdrawing the referenced package refuses the review with
	// 409, and restoring it lets the review through.
	code, staleCreated := call("POST", "/api/kb/packages", adminSess,
		map[string]any{"name": "ref_stale", "description": "引用过期测试", "scope": "*",
			"content": "综合：引用入职必读。", "sources": intSrc, "citations": intCit,
			"references": []any{pkgID}, "change_note": "初版"})
	if code != 201 {
		t.Fatalf("create ref_stale: got %d (%v)", code, staleCreated)
	}
	staleID, _ := staleCreated["id"].(string)
	if code, _ := call("POST", "/api/kb/packages/"+pkgID+"/review", adminSess, map[string]any{"approve": false}); code != 200 {
		t.Fatalf("reject referenced package again: got %d", code)
	}
	if code, body := call("POST", "/api/kb/packages/"+staleID+"/review", adminSess, map[string]any{"approve": true}); code != 409 {
		t.Fatalf("review-approve with withdrawn reference: got %d (%v), want 409", code, body)
	}
	if code, _ := call("POST", "/api/kb/packages/"+pkgID+"/review", adminSess, map[string]any{"approve": true}); code != 200 {
		t.Fatalf("re-approve referenced package: got %d", code)
	}
	if code, _ := call("POST", "/api/kb/packages/"+staleID+"/review", adminSess, map[string]any{"approve": true}); code != 200 {
		t.Fatalf("review-approve after restore: got %d", code)
	}
	if code, _ := call("DELETE", "/api/kb/packages/"+staleID, adminSess, nil); code != 200 {
		t.Fatalf("delete ref_stale: got %d", code)
	}

	// Delete removes the package; its id stays unusable.
	if code, _ := call("DELETE", "/api/kb/packages/"+pkgID, empSess, nil); code != 403 {
		t.Fatalf("employee delete: got %d, want 403", code)
	}
	if code, _ := call("DELETE", "/api/kb/packages/"+pkgID, adminSess, nil); code != 200 {
		t.Fatalf("delete: got %d", code)
	}
	if code, _ := call("GET", "/api/kb/packages/"+pkgID, adminSess, nil); code != 404 {
		t.Fatalf("get deleted: got %d, want 404", code)
	}
	if code, _ := call("DELETE", "/api/kb/packages/"+empDraftID, adminSess, nil); code != 200 {
		t.Fatalf("delete employee draft: got %d", code)
	}
}
