package semidx

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/folderkb"
	"github.com/myjnathan/agentgate/internal/store"
)

func testSnapshot(level store.DataLevel) folderkb.DocSnapshot {
	return folderkb.DocSnapshot{
		ID: "folder:kbf_int:考勤与请假制度.md", SourceID: "kbf_int",
		SourceName: "公司制度", Department: "人力部",
		RelPath: "考勤与请假制度.md", Title: "考勤与请假制度",
		Level: level, Pages: 2, Fingerprint: "fp-1",
		Tree: &folderkb.Node{ID: "n0", Title: "考勤与请假制度", StartPage: 1, EndPage: 2,
			Summary: "本制度适用于星河精密全体员工。",
			Children: []*folderkb.Node{
				{ID: "n1", Title: "请假流程", StartPage: 1, EndPage: 1,
					Summary: "员工请假须提前在系统提交申请。"},
				{ID: "n2", Title: "加班管理", StartPage: 2, EndPage: 2,
					Summary: "加班须提前报备，调休优先于加班费。"},
			}},
	}
}

func validEntry() Entry {
	return Entry{
		DocID: "folder:kbf_int:考勤与请假制度.md", NodeID: "n1",
		Function:      "请假流程：员工请假须提前在系统提交申请。",
		Relations:     "上级章节：考勤与请假制度；同级章节：加班管理",
		Applicability: "适用范围：公司制度（人力部），内部级。",
		Constraints:   "内部级：自带级与受管级均可见。",
		Tags:          Tags{Department: "人力部", Type: "Markdown", Importance: "medium"},
		Source:        SourceRef{File: "考勤与请假制度.md", Chapter: "请假流程", Pages: "1"},
		GeneratedBy:   GeneratedExtractive, GeneratedAt: time.Now(),
	}
}

func TestValidate(t *testing.T) {
	if err := Validate(validEntry()); err != nil {
		t.Fatalf("valid entry rejected: %v", err)
	}
	bad := func(mut func(*Entry)) Entry {
		e := validEntry()
		mut(&e)
		return e
	}
	cases := []struct {
		name string
		e    Entry
	}{
		{"no doc", bad(func(e *Entry) { e.DocID = "" })},
		{"no node", bad(func(e *Entry) { e.NodeID = "" })},
		{"bad generator", bad(func(e *Entry) { e.GeneratedBy = "magic" })},
		{"empty function", bad(func(e *Entry) { e.Function = "" })},
		{"empty relations", bad(func(e *Entry) { e.Relations = " " })},
		{"overlong", bad(func(e *Entry) { e.Function = strings.Repeat("字", MaxFRASRunes+1) })},
		{"no department", bad(func(e *Entry) { e.Tags.Department = "" })},
		{"no type", bad(func(e *Entry) { e.Tags.Type = "" })},
		{"bad importance", bad(func(e *Entry) { e.Tags.Importance = "urgent" })},
		{"no file", bad(func(e *Entry) { e.Source.File = "" })},
		{"no chapter", bad(func(e *Entry) { e.Source.Chapter = "" })},
		{"bad pages", bad(func(e *Entry) { e.Source.Pages = "1-" })},
	}
	for _, c := range cases {
		if err := Validate(c.e); err == nil {
			t.Errorf("%s: expected validation error, got nil", c.name)
		}
	}
	// Title-only entries (secret docs) need only the chapter title.
	to := validEntry()
	to.GeneratedBy = GeneratedTitleOnly
	to.Function = "绝密章节"
	to.Relations, to.Applicability, to.Constraints = "", "", ""
	if err := Validate(to); err != nil {
		t.Errorf("title-only entry rejected: %v", err)
	}
	to.Function = ""
	if err := Validate(to); err == nil {
		t.Errorf("title-only entry without a title should fail validation")
	}
}

func TestExtractiveEntries(t *testing.T) {
	snap := testSnapshot(store.LevelInternal)
	entries := extractiveEntries(snap, time.Now())
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3 (root + 2 chapters)", len(entries))
	}
	root, n1, n2 := entries[0], entries[1], entries[2]
	if n1.NodeID != "n1" || n1.Source.Chapter != "请假流程" || n1.Source.Pages != "1" || n1.Source.File != "考勤与请假制度.md" {
		t.Errorf("n1 back-reference wrong: %+v", n1.Source)
	}
	if !strings.Contains(n1.Function, "请假流程") || !strings.Contains(n1.Function, "提前在系统提交申请") {
		t.Errorf("n1 function should be title + summary, got %q", n1.Function)
	}
	if !strings.Contains(n1.Relations, "上级章节：考勤与请假制度") || !strings.Contains(n1.Relations, "同级章节：加班管理") {
		t.Errorf("n1 relations should name parent and siblings, got %q", n1.Relations)
	}
	if n1.Tags != (Tags{Department: "人力部", Type: "Markdown", Importance: "medium"}) {
		t.Errorf("n1 tags = %+v", n1.Tags)
	}
	if !strings.Contains(n1.Constraints, "内部级") {
		t.Errorf("n1 constraints should state the level rule, got %q", n1.Constraints)
	}
	if !strings.Contains(root.Relations, "包含章节：请假流程、加班管理") {
		t.Errorf("root relations should list children, got %q", root.Relations)
	}
	if n2.Source.Pages != "2" {
		t.Errorf("n2 pages = %q, want 2", n2.Source.Pages)
	}
	// Department falls back to the folder name when unconfigured.
	snap.Department = ""
	if got := DepartmentFor(snap); got != "公司制度" {
		t.Errorf("department fallback = %q, want folder name", got)
	}
	// Importance follows the data level.
	if got := ImportanceForLevel(store.LevelConfidential); got != "high" {
		t.Errorf("confidential importance = %q, want high", got)
	}
	if got := ImportanceForLevel(store.LevelSecret); got != "critical" {
		t.Errorf("secret importance = %q, want critical", got)
	}
}

type failingGenerator struct{ err error }

func (f failingGenerator) Generate(_ context.Context, _ folderkb.DocSnapshot) ([]Entry, error) {
	return nil, f.err
}

type partialGenerator struct{}

func (partialGenerator) Generate(_ context.Context, snap folderkb.DocSnapshot) ([]Entry, error) {
	e := validEntry()
	e.DocID = snap.ID
	e.GeneratedBy = GeneratedModel
	return []Entry{e}, nil
}

func TestRegenerateDocSecretIsTitleOnly(t *testing.T) {
	ix := New()
	snap := testSnapshot(store.LevelSecret)
	de := ix.RegenerateDoc(context.Background(), snap, ExtractiveGenerator{})
	if de.GeneratedBy != GeneratedTitleOnly {
		t.Fatalf("generated_by = %q, want title-only", de.GeneratedBy)
	}
	if len(de.Entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(de.Entries))
	}
	for _, e := range de.Entries {
		if e.Function == "" || e.Relations != "" || e.Applicability != "" || e.Constraints != "" {
			t.Errorf("title-only entry must carry only the title: %+v", e)
		}
		if err := Validate(e); err != nil {
			t.Errorf("title-only entry failed validation: %v", err)
		}
	}
	if de.Entries[1].Function != "请假流程" {
		t.Errorf("title-only function = %q, want chapter title", de.Entries[1].Function)
	}
}

func TestRegenerateDocFallbackOnGeneratorError(t *testing.T) {
	ix := New()
	snap := testSnapshot(store.LevelInternal)
	de := ix.RegenerateDoc(context.Background(), snap, failingGenerator{err: errors.New("model unreachable")})
	if de.GeneratedBy != GeneratedExtractive {
		t.Fatalf("generated_by = %q, want extractive fallback", de.GeneratedBy)
	}
	if de.ModelError == "" || !strings.Contains(de.ModelError, "model unreachable") {
		t.Errorf("model error should be recorded, got %q", de.ModelError)
	}
	if len(de.Entries) != 3 {
		t.Fatalf("fallback should still produce 3 entries, got %d", len(de.Entries))
	}
}

func TestRegenerateDocMixedWhenModelCoversPart(t *testing.T) {
	ix := New()
	snap := testSnapshot(store.LevelInternal)
	de := ix.RegenerateDoc(context.Background(), snap, partialGenerator{})
	if de.GeneratedBy != GeneratedMixed {
		t.Fatalf("generated_by = %q, want mixed", de.GeneratedBy)
	}
	if de.ModelError == "" {
		t.Errorf("partial coverage should be recorded in model_error")
	}
	if len(de.Entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(de.Entries))
	}
	if de.Entries[1].GeneratedBy != GeneratedModel {
		t.Errorf("n1 should keep the model entry, got %q", de.Entries[1].GeneratedBy)
	}
	if de.Entries[2].GeneratedBy != GeneratedExtractive {
		t.Errorf("n2 should be filled extractively, got %q", de.Entries[2].GeneratedBy)
	}
}

func TestRegenerateChangedAndDrift(t *testing.T) {
	ix := New()
	ctx := context.Background()
	snap := testSnapshot(store.LevelInternal)
	other := testSnapshot(store.LevelConfidential)
	other.ID = "folder:kbf_conf:机密资料.md"
	other.SourceID = "kbf_conf"
	other.Fingerprint = "fp-9"

	reg, unch := ix.RegenerateChanged(ctx, []folderkb.DocSnapshot{snap, other})
	if reg != 2 || unch != 0 {
		t.Fatalf("regenerated=%d unchanged=%d, want 2/0", reg, unch)
	}
	reg, unch = ix.RegenerateChanged(ctx, []folderkb.DocSnapshot{snap, other})
	if reg != 0 || unch != 2 {
		t.Fatalf("second pass: regenerated=%d unchanged=%d, want 0/2", reg, unch)
	}
	// The document changes: drift is detected, then a rescan clears it.
	changed := snap
	changed.Fingerprint = "fp-2"
	if stale := ix.CheckDrift([]folderkb.DocSnapshot{changed, other}); len(stale) != 1 || stale[0] != snap.ID {
		t.Fatalf("drift = %v, want [%s]", stale, snap.ID)
	}
	cov := ix.Coverage([]folderkb.DocSnapshot{changed, other})
	if cov.DocsTotal != 2 || cov.DocsIndexed != 1 || cov.StaleDocs[0] != snap.ID {
		t.Errorf("coverage after drift: %+v", cov)
	}
	if reg, _ := ix.RegenerateChanged(ctx, []folderkb.DocSnapshot{changed, other}); reg != 1 {
		t.Fatalf("changed doc should regenerate, got %d", reg)
	}
	if stale := ix.CheckDrift([]folderkb.DocSnapshot{changed, other}); len(stale) != 0 {
		t.Fatalf("drift after regeneration = %v, want none", stale)
	}
	// A vanished file drops out of the index and the coverage.
	reg, unch = ix.RegenerateChanged(ctx, []folderkb.DocSnapshot{changed})
	if reg != 0 || unch != 1 {
		t.Fatalf("after removal: regenerated=%d unchanged=%d, want 0/1", reg, unch)
	}
	cov = ix.Coverage([]folderkb.DocSnapshot{changed})
	if cov.DocsTotal != 1 || len(cov.MissingDocs) != 0 {
		t.Errorf("coverage after removal: %+v", cov)
	}
}

func TestEntriesLevelGate(t *testing.T) {
	ix := New()
	snap := testSnapshot(store.LevelConfidential)
	ix.RegenerateDoc(context.Background(), snap, ExtractiveGenerator{})
	if _, ok := ix.Entries(snap.ID, store.LevelInternal); ok {
		t.Errorf("confidential entries must be hidden at internal max level")
	}
	entries, ok := ix.Entries(snap.ID, store.LevelConfidential)
	if !ok || len(entries) != 3 {
		t.Fatalf("confidential entries should be visible at confidential level: ok=%v n=%d", ok, len(entries))
	}
	if _, ok := ix.Entries("folder:nope:x.md", store.LevelSecret); ok {
		t.Errorf("unknown doc should not be found")
	}
}

func TestDropSource(t *testing.T) {
	ix := New()
	ctx := context.Background()
	a := testSnapshot(store.LevelInternal)
	b := testSnapshot(store.LevelInternal)
	b.ID = "folder:kbf_int:另一制度.md"
	ix.RegenerateDoc(ctx, a, ExtractiveGenerator{})
	ix.RegenerateDoc(ctx, b, ExtractiveGenerator{})
	ix.DropSource("kbf_int")
	if _, ok := ix.Entries(a.ID, store.LevelSecret); ok {
		t.Errorf("entries should be dropped with their source")
	}
}
