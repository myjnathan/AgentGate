package mcp_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/myjnathan/agentgate/internal/store"
)

// ---- F22: curated knowledge packages ----

func seedTestPackages(t *testing.T, e *testEnv) {
	t.Helper()
	intDoc := "folder:kbf_int:考勤与请假制度.md"
	confDoc := "folder:kbf_conf:机密资料.md"
	e.st.WithLock(func(s *store.State) {
		s.KnowledgePackages["kp_onboarding"] = &store.KnowledgePackage{
			ID: "kp_onboarding", Name: "onboarding_essentials",
			Description: "新员工入职必读", Scope: "*",
			Status: "published", CurrentVersion: 1, PublishedVersion: 1,
			Versions: []store.PackageVersion{{Version: 1,
				Content:   "综合：请假须提前提交申请，年假每年 5 天起；加班须提前报备，每月不得超过 36 小时。",
				Citations: []store.PackageCitation{{DocID: intDoc, NodeID: "n1", File: "考勤与请假制度.md", Chapter: "请假流程", Pages: "1"}},
				Sources:   []store.PackageSource{{DocID: intDoc}},
				Level:     "internal"}},
		}
		s.KnowledgePackages["kp_vendor"] = &store.KnowledgePackage{
			ID: "kp_vendor", Name: "vendor_onboarding",
			Description: "供应商准入", Scope: "*",
			Status: "published", CurrentVersion: 1, PublishedVersion: 1,
			Versions: []store.PackageVersion{{Version: 1,
				Content:    "综合：机密资料仅受管级可见，并引用入职必读的请假流程。",
				Citations:  []store.PackageCitation{{DocID: confDoc, NodeID: "n0", File: "机密资料.md", Chapter: "机密资料", Pages: "1"}},
				Sources:    []store.PackageSource{{DocID: confDoc}},
				References: []string{"kp_onboarding"},
				Level:      "confidential"}},
		}
		s.KnowledgePackages["kp_draft"] = &store.KnowledgePackage{
			ID: "kp_draft", Name: "draft_pack",
			Description: "草稿包", Scope: "*",
			Status: "in_review", CurrentVersion: 1,
			Versions: []store.PackageVersion{{Version: 1,
				Content:   "草稿综合。",
				Citations: []store.PackageCitation{{DocID: intDoc, NodeID: "n1", File: "考勤与请假制度.md", Chapter: "请假流程", Pages: "1"}},
				Sources:   []store.PackageSource{{DocID: intDoc}},
				Level:     "internal"}},
		}
	})
}

type packListResult struct {
	Packages []struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Level      string `json:"level"`
		Version    int    `json:"published_version"`
		Sources    int    `json:"sources"`
		References int    `json:"references"`
	} `json:"packages"`
}

type packGetResult struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Level     string `json:"level"`
	Synthesis string `json:"synthesis"`
	Citations []struct {
		DocID   string `json:"doc_id"`
		NodeID  string `json:"node_id"`
		File    string `json:"file"`
		Chapter string `json:"chapter"`
		Pages   string `json:"pages"`
	} `json:"citations"`
	Sources []struct {
		DocID string `json:"doc_id"`
		File  string `json:"file"`
		Title string `json:"title"`
		Level string `json:"level"`
	} `json:"sources"`
	References []struct {
		Name      string `json:"name"`
		Level     string `json:"level"`
		Synthesis string `json:"synthesis"`
	} `json:"references"`
	Withheld []string `json:"withheld_references"`
}

func TestKnowledgePackagesMCP(t *testing.T) {
	e := setup(t)
	seedTestPackages(t, e)

	// BYOA lists only the internal package; the confidential one
	// and the draft are indistinguishable from unknown.
	text := resultText(t, e.callTool(t, e.byoaTok, "kb_pack_list", map[string]any{}))
	var list packListResult
	if err := json.Unmarshal([]byte(text), &list); err != nil {
		t.Fatalf("kb_pack_list result is not JSON: %v (%s)", err, text)
	}
	if len(list.Packages) != 1 || list.Packages[0].Name != "onboarding_essentials" || list.Packages[0].Level != "internal" {
		t.Fatalf("BYOA pack list = %+v, want only the internal package", list.Packages)
	}
	text = resultText(t, e.callTool(t, e.mgdTok, "kb_pack_list", map[string]any{}))
	if err := json.Unmarshal([]byte(text), &list); err != nil {
		t.Fatalf("kb_pack_list (managed) is not JSON: %v", err)
	}
	if len(list.Packages) != 2 {
		t.Fatalf("managed pack list = %d packages, want internal + confidential (no draft)", len(list.Packages))
	}

	// BYOA reads the internal package: synthesis, citation and
	// source list with file/title resolved from the folder index.
	text = resultText(t, e.callTool(t, e.byoaTok, "kb_pack_get", map[string]any{"name": "onboarding_essentials"}))
	var got packGetResult
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("kb_pack_get result is not JSON: %v (%s)", err, text)
	}
	if got.Name != "onboarding_essentials" || !strings.Contains(got.Synthesis, "请假须提前提交申请") {
		t.Fatalf("kb_pack_get = %+v, want the published synthesis", got)
	}
	if len(got.Citations) != 1 || got.Citations[0].Chapter != "请假流程" || got.Citations[0].Pages != "1" {
		t.Fatalf("citations = %+v, want the tree-filled back-reference", got.Citations)
	}
	if len(got.Sources) != 1 || got.Sources[0].File != "考勤与请假制度.md" || got.Sources[0].Title != "考勤与请假制度" {
		t.Fatalf("sources = %+v, want file/title from the folder index", got.Sources)
	}
	// Lookup by id works too.
	text = resultText(t, e.callTool(t, e.byoaTok, "kb_pack_get", map[string]any{"name": "kp_onboarding"}))
	if err := json.Unmarshal([]byte(text), &got); err != nil || got.Name != "onboarding_essentials" {
		t.Fatalf("kb_pack_get by id = %s (%v)", text, err)
	}

	// The confidential package is hidden from BYOA (isError) and
	// readable by managed — including the referenced internal
	// synthesis, served inline.
	if r := e.callTool(t, e.byoaTok, "kb_pack_get", map[string]any{"name": "vendor_onboarding"}); r.Result == nil || !r.Result.IsError {
		t.Error("BYOA read the confidential package")
	}
	text = resultText(t, e.callTool(t, e.mgdTok, "kb_pack_get", map[string]any{"name": "vendor_onboarding"}))
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("kb_pack_get (managed) is not JSON: %v (%s)", err, text)
	}
	if len(got.References) != 1 || got.References[0].Name != "onboarding_essentials" ||
		!strings.Contains(got.References[0].Synthesis, "请假须提前提交申请") {
		t.Fatalf("references = %+v, want the referenced synthesis inline", got.References)
	}
	if len(got.Withheld) != 0 {
		t.Fatalf("withheld = %v, want none", got.Withheld)
	}

	// Unknown and draft packages are indistinguishable from
	// inaccessible ones.
	if r := e.callTool(t, e.byoaTok, "kb_pack_get", map[string]any{"name": "nope"}); r.Result == nil || !r.Result.IsError {
		t.Error("unknown package returned without error")
	}
	if r := e.callTool(t, e.byoaTok, "kb_pack_get", map[string]any{"name": "draft_pack"}); r.Result == nil || !r.Result.IsError {
		t.Error("draft package returned without error")
	}

	// A withdrawn reference is withheld by name, not served: the
	// referencing package itself stays readable.
	e.st.WithLock(func(s *store.State) {
		p := s.KnowledgePackages["kp_onboarding"]
		p.Status = "rejected"
		p.PublishedVersion = 0
	})
	text = resultText(t, e.callTool(t, e.mgdTok, "kb_pack_get", map[string]any{"name": "vendor_onboarding"}))
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("kb_pack_get after withdrawal is not JSON: %v (%s)", err, text)
	}
	if len(got.References) != 0 || len(got.Withheld) != 1 || got.Withheld[0] != "onboarding_essentials" {
		t.Fatalf("references = %+v withheld = %v, want the withdrawn reference withheld by name", got.References, got.Withheld)
	}
}
