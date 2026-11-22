package knowpack_test

import (
	"testing"

	"github.com/myjnathan/agentgate/internal/folderkb"
	"github.com/myjnathan/agentgate/internal/knowpack"
	"github.com/myjnathan/agentgate/internal/store"
)

func tree() *folderkb.Node {
	return &folderkb.Node{ID: "n0", Title: "考勤与请假制度", StartPage: 1, EndPage: 1, Children: []*folderkb.Node{
		{ID: "n1", Title: "请假流程", StartPage: 1, EndPage: 1},
		{ID: "n2", Title: "加班管理", StartPage: 1, EndPage: 1},
	}}
}

func snaps() []folderkb.DocSnapshot {
	return []folderkb.DocSnapshot{
		{ID: "folder:kbf_int:考勤与请假制度.md", SourceID: "kbf_int", RelPath: "考勤与请假制度.md", Title: "考勤与请假制度", Level: store.LevelInternal, Pages: 1, Tree: tree()},
		{ID: "folder:kbf_conf:机密资料.md", SourceID: "kbf_conf", RelPath: "机密资料.md", Title: "机密资料", Level: store.LevelConfidential, Pages: 2, Tree: &folderkb.Node{ID: "n0", Title: "机密资料", StartPage: 1, EndPage: 2}},
	}
}

func intDoc() string  { return "folder:kbf_int:考勤与请假制度.md" }
func confDoc() string { return "folder:kbf_conf:机密资料.md" }

func publishedPkg(id, name, level string) *store.KnowledgePackage {
	return &store.KnowledgePackage{ID: id, Name: name, Status: "published", CurrentVersion: 1, PublishedVersion: 1,
		Versions: []store.PackageVersion{{Version: 1, Content: "综合", Level: level,
			Sources:   []store.PackageSource{{DocID: intDoc()}},
			Citations: []store.PackageCitation{{DocID: intDoc(), NodeID: "n1", File: "考勤与请假制度.md", Chapter: "请假流程", Pages: "1"}}}}}
}

func TestResolveVersion(t *testing.T) {
	empty := map[string]*store.KnowledgePackage{}
	src := []store.PackageSource{{DocID: intDoc()}}
	cit := []store.PackageCitation{{DocID: intDoc(), NodeID: "n1"}}

	// Whole-document source: level from the document, citation
	// back-reference filled in from the tree.
	lvl, filled, err := knowpack.ResolveVersion("p1", src, nil, cit, snaps(), empty)
	if err != nil {
		t.Fatalf("ResolveVersion: %v", err)
	}
	if lvl != store.LevelInternal {
		t.Fatalf("level = %v, want internal", lvl)
	}
	if len(filled) != 1 || filled[0].File != "考勤与请假制度.md" || filled[0].Chapter != "请假流程" || filled[0].Pages != "1" {
		t.Fatalf("filled citation = %+v, want file/chapter/pages from the tree", filled)
	}

	// Unknown document, unknown section, citation outside the recipe.
	if _, _, err := knowpack.ResolveVersion("p1", []store.PackageSource{{DocID: "folder:nope:x.md"}}, nil, cit, snaps(), empty); err == nil {
		t.Error("unknown source document accepted")
	}
	if _, _, err := knowpack.ResolveVersion("p1", []store.PackageSource{{DocID: intDoc(), NodeID: "n9"}}, nil, cit, snaps(), empty); err == nil {
		t.Error("unknown source section accepted")
	}
	if _, _, err := knowpack.ResolveVersion("p1", src, nil, []store.PackageCitation{{DocID: confDoc(), NodeID: "n0"}}, snaps(), empty); err == nil {
		t.Error("citation outside the recipe accepted")
	}
	if _, _, err := knowpack.ResolveVersion("p1", src, nil, []store.PackageCitation{{DocID: intDoc(), NodeID: "n9"}}, snaps(), empty); err == nil {
		t.Error("citation naming an unknown section accepted")
	}

	// Node-scoped source: a citation inside the scope passes, one
	// outside it is rejected.
	scoped := []store.PackageSource{{DocID: intDoc(), NodeID: "n1"}}
	if _, _, err := knowpack.ResolveVersion("p1", scoped, nil, cit, snaps(), empty); err != nil {
		t.Errorf("citation inside node-scoped source rejected: %v", err)
	}
	if _, _, err := knowpack.ResolveVersion("p1", scoped, nil, []store.PackageCitation{{DocID: intDoc(), NodeID: "n2"}}, snaps(), empty); err == nil {
		t.Error("citation outside node-scoped source accepted")
	}

	// A confidential source raises the level.
	lvl, _, err = knowpack.ResolveVersion("p1", []store.PackageSource{{DocID: intDoc()}, {DocID: confDoc()}}, nil, cit, snaps(), empty)
	if err != nil {
		t.Fatalf("mixed sources: %v", err)
	}
	if lvl != store.LevelConfidential {
		t.Fatalf("level = %v, want confidential", lvl)
	}
}

func TestResolveVersionReferences(t *testing.T) {
	src := []store.PackageSource{{DocID: intDoc()}}
	cit := []store.PackageCitation{{DocID: intDoc(), NodeID: "n1"}}
	pkgs := map[string]*store.KnowledgePackage{
		"pk_int":  publishedPkg("pk_int", "onboarding", "internal"),
		"pk_conf": publishedPkg("pk_conf", "vendor", "confidential"),
		"pk_draft": {ID: "pk_draft", Name: "draft", Status: "in_review", CurrentVersion: 1,
			Versions: []store.PackageVersion{{Version: 1, Content: "草稿", Level: "internal"}}},
	}

	// Downward reference (internal package referencing an internal
	// package) passes; the level stays internal.
	if lvl, _, err := knowpack.ResolveVersion("p2", src, []string{"pk_int"}, cit, snaps(), pkgs); err != nil || lvl != store.LevelInternal {
		t.Errorf("downward reference: level=%v err=%v", lvl, err)
	}
	// Upward reference: an internal package may not embed a
	// confidential synthesis.
	if _, _, err := knowpack.ResolveVersion("p2", src, []string{"pk_conf"}, cit, snaps(), pkgs); err == nil {
		t.Error("upward reference accepted")
	}
	// A confidential package may reference an internal one.
	confSrc := []store.PackageSource{{DocID: confDoc()}}
	confCit := []store.PackageCitation{{DocID: confDoc(), NodeID: "n0"}}
	if lvl, _, err := knowpack.ResolveVersion("p3", confSrc, []string{"pk_int"}, confCit, snaps(), pkgs); err != nil || lvl != store.LevelConfidential {
		t.Errorf("confidential referencing internal: level=%v err=%v", lvl, err)
	}
	// Self-reference, unknown reference and unpublished reference.
	if _, _, err := knowpack.ResolveVersion("pk_int", src, []string{"pk_int"}, cit, snaps(), pkgs); err == nil {
		t.Error("self-reference accepted")
	}
	if _, _, err := knowpack.ResolveVersion("p2", src, []string{"pk_nope"}, cit, snaps(), pkgs); err == nil {
		t.Error("unknown reference accepted")
	}
	if _, _, err := knowpack.ResolveVersion("p2", src, []string{"pk_draft"}, cit, snaps(), pkgs); err == nil {
		t.Error("unpublished reference accepted")
	}
}

func TestCheckReferencesAndSources(t *testing.T) {
	pkgs := map[string]*store.KnowledgePackage{
		"pk_int":  publishedPkg("pk_int", "onboarding", "internal"),
		"pk_conf": publishedPkg("pk_conf", "vendor", "confidential"),
	}
	v := &store.PackageVersion{Version: 1, Level: "internal",
		Sources:    []store.PackageSource{{DocID: intDoc()}},
		References: []string{"pk_int"}}
	if w := knowpack.CheckReferences(v, pkgs); len(w) != 0 {
		t.Errorf("CheckReferences = %v, want none withheld", w)
	}
	// The referenced package is republished at a higher level: the
	// reference must be withheld at read time, not served.
	pkgs["pk_int"].Versions[0].Level = "confidential"
	if w := knowpack.CheckReferences(v, pkgs); len(w) != 1 {
		t.Errorf("CheckReferences = %v, want the escalated reference withheld", w)
	}
	pkgs["pk_int"].Versions[0].Level = "internal"
	// Withdrawn (unpublished) reference is withheld too.
	pkgs["pk_int"].Status = "rejected"
	pkgs["pk_int"].PublishedVersion = 0
	if w := knowpack.CheckReferences(v, pkgs); len(w) != 1 {
		t.Errorf("CheckReferences = %v, want the withdrawn reference withheld", w)
	}

	if m := knowpack.CheckSources(v, snaps()); len(m) != 0 {
		t.Errorf("CheckSources = %v, want none missing", m)
	}
	v.Sources = append(v.Sources, store.PackageSource{DocID: "folder:gone:x.md"})
	if m := knowpack.CheckSources(v, snaps()); len(m) != 1 || m[0] != "folder:gone:x.md" {
		t.Errorf("CheckSources = %v, want the vanished document", m)
	}
}

func TestPublishedSynthesis(t *testing.T) {
	pkgs := map[string]*store.KnowledgePackage{"pk_int": publishedPkg("pk_int", "onboarding", "internal")}
	name, lvl, content, cits, ok := knowpack.PublishedSynthesis("pk_int", pkgs, store.LevelInternal)
	if !ok || name != "onboarding" || lvl != "internal" || content != "综合" || len(cits) != 1 {
		t.Fatalf("PublishedSynthesis = %q %q %q %v %v", name, lvl, content, cits, ok)
	}
	// Above the caller's level the synthesis is withheld.
	if _, _, _, _, ok := knowpack.PublishedSynthesis("pk_int", pkgs, store.LevelPublic); ok {
		t.Error("synthesis above max level served")
	}
	if _, _, _, _, ok := knowpack.PublishedSynthesis("pk_nope", pkgs, store.LevelSecret); ok {
		t.Error("unknown package served")
	}
}

func TestValidatePackageVersion(t *testing.T) {
	src := []store.PackageSource{{DocID: intDoc()}}
	cit := []store.PackageCitation{{DocID: intDoc(), NodeID: "n1"}}
	if err := store.ValidatePackageVersion("综合", cit, src, nil, "初版"); err != nil {
		t.Fatalf("valid version rejected: %v", err)
	}
	cases := []struct {
		name string
		fn   func() error
	}{
		{"empty content", func() error { return store.ValidatePackageVersion("", cit, src, nil, "") }},
		{"no sources", func() error { return store.ValidatePackageVersion("x", cit, nil, nil, "") }},
		{"duplicate source", func() error {
			return store.ValidatePackageVersion("x", cit, []store.PackageSource{{DocID: intDoc()}, {DocID: intDoc()}}, nil, "")
		}},
		{"no citations", func() error { return store.ValidatePackageVersion("x", nil, src, nil, "") }},
		{"citation without node", func() error {
			return store.ValidatePackageVersion("x", []store.PackageCitation{{DocID: intDoc()}}, src, nil, "")
		}},
		{"duplicate citation", func() error {
			return store.ValidatePackageVersion("x", []store.PackageCitation{{DocID: intDoc(), NodeID: "n1"}, {DocID: intDoc(), NodeID: "n1"}}, src, nil, "")
		}},
		{"duplicate reference", func() error { return store.ValidatePackageVersion("x", cit, src, []string{"a", "a"}, "") }},
	}
	for _, c := range cases {
		if err := c.fn(); err == nil {
			t.Errorf("%s accepted", c.name)
		}
	}
}
