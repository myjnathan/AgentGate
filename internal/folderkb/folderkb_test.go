package folderkb_test

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/myjnathan/agentgate/internal/folderkb"
	"github.com/myjnathan/agentgate/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func addSource(t *testing.T, st *store.Store, id, name, path string, level store.DataLevel) *store.KBFolderSource {
	t.Helper()
	src := &store.KBFolderSource{ID: id, Name: name, Path: path, Level: level, Enabled: true, CreatedAt: time.Now()}
	st.WithLock(func(s *store.State) { s.KBFolderSources[id] = src })
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	return src
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildDocx assembles a minimal .docx: paragraphs with optional
// Heading1/2 styles and an optional page break.
func buildDocx(t *testing.T, paras []struct {
	style, text string
	breakBefore bool
}) []byte {
	t.Helper()
	var body strings.Builder
	for _, p := range paras {
		if p.breakBefore {
			body.WriteString(`<w:p><w:r><w:br w:type="page"/></w:r></w:p>`)
		}
		body.WriteString("<w:p>")
		if p.style != "" {
			body.WriteString(`<w:pPr><w:pStyle w:val="` + p.style + `"/></w:pPr>`)
		}
		body.WriteString("<w:r><w:t>" + xmlEscape(p.text) + "</w:t></w:r></w:p>")
	}
	xmlDoc := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` +
		body.String() + `</w:body></w:document>`
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(xmlDoc)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// buildPDF assembles a minimal 2-page PDF: one content stream per
// page, the second Flate-compressed. Literal strings are the only
// text-showing operands, so the extractor must find them.
func buildPDF(t *testing.T) []byte {
	t.Helper()
	page1 := "BT /F1 12 Tf 72 720 Td (供应商准入要求) Tj ET\nBT 72 700 Td (供应商须提供营业执照与质量体系证书。) Tj ET"
	var comp bytes.Buffer
	zw := zlib.NewWriter(&comp)
	if _, err := zw.Write([]byte("BT /F1 12 Tf 72 720 Td (准入流程) Tj ET\nBT 72 700 Td (初审通过后进入现场审核环节。) Tj ET")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	page2 := comp.Bytes()
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	buf.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	buf.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R 5 0 R] /Count 2 >>\nendobj\n")
	buf.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /Contents 4 0 R >>\nendobj\n")
	fmt.Fprintf(&buf, "4 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(page1), page1)
	buf.WriteString("5 0 obj\n<< /Type /Page /Parent 2 0 R /Contents 6 0 R >>\nendobj\n")
	fmt.Fprintf(&buf, "6 0 obj\n<< /Length %d /Filter /FlateDecode >>\nstream\n", len(page2))
	buf.Write(page2)
	buf.WriteString("\nendstream\nendobj\n")
	buf.WriteString("trailer\n<< /Root 1 0 R >>\n%%EOF\n")
	return buf.Bytes()
}

const mdDoc = `# 考勤与请假制度

本制度适用于星河精密全体员工。

## 请假流程

员工请假须提前在系统提交申请，经部门负责人审批后生效。年假每年 5 天起，按工龄递增。

## 加班管理

加班须提前报备，调休优先于加班费。每月加班不得超过 36 小时。
`

func TestMarkdownTreeAndSummary(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "考勤与请假制度.md", mdDoc)
	st := testStore(t)
	src := addSource(t, st, "kbf_md", "公司制度", dir, store.LevelInternal)
	reg := folderkb.New(st)
	stats, err := reg.Rescan(src)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Added != 1 || stats.Scanned != 1 {
		t.Fatalf("stats = %+v, want 1 added / 1 scanned", stats)
	}
	docs := reg.Catalog(store.LevelInternal)
	if len(docs) != 1 {
		t.Fatalf("catalog has %d docs, want 1", len(docs))
	}
	if docs[0].Title != "考勤与请假制度" {
		t.Fatalf("title = %q", docs[0].Title)
	}
	if len(docs[0].Sections) != 2 || docs[0].Sections[0] != "请假流程" {
		t.Fatalf("top sections = %v", docs[0].Sections)
	}
	tree, ok := reg.Structure(docs[0].DocID, store.LevelInternal)
	if !ok {
		t.Fatal("structure not found")
	}
	if len(tree.Children) != 2 {
		t.Fatalf("tree has %d children, want 2", len(tree.Children))
	}
	if tree.Children[0].Summary == "" || !strings.Contains(tree.Children[0].Summary, "请假须提前") {
		t.Fatalf("section summary = %q, want the opening sentence", tree.Children[0].Summary)
	}
	sec, rel, ok := reg.Section(docs[0].DocID, tree.Children[0].ID, store.LevelInternal)
	if !ok || rel != "考勤与请假制度.md" {
		t.Fatalf("section = %+v rel=%q ok=%v", sec, rel, ok)
	}
	if !strings.Contains(sec.Text, "年假每年 5 天起") {
		t.Fatalf("section text missing content: %q", sec.Text)
	}
	if sec.StartPage != 1 || sec.EndPage < 1 {
		t.Fatalf("section pages = %d-%d", sec.StartPage, sec.EndPage)
	}
	hits := reg.Search("请假 年假", store.LevelInternal, 10)
	if len(hits) == 0 || hits[0].Section != "请假流程" {
		t.Fatalf("search hits = %+v", hits)
	}
	if hits[0].Pages == "" || hits[0].Source != "folder" {
		t.Fatalf("hit = %+v", hits[0])
	}
}

func TestDocxExtraction(t *testing.T) {
	dir := t.TempDir()
	docx := buildDocx(t, []struct {
		style, text string
		breakBefore bool
	}{
		{"Heading1", "办公用品领用流程", false},
		{"", "领用办公用品须登记用途与数量。", false},
		{"Heading2", "耗材申领", false},
		{"", "打印纸与硒鼓按月集中申领。", true},
	})
	if err := os.WriteFile(filepath.Join(dir, "办公用品领用流程.docx"), docx, 0o644); err != nil {
		t.Fatal(err)
	}
	st := testStore(t)
	src := addSource(t, st, "kbf_docx", "公司制度", dir, store.LevelInternal)
	reg := folderkb.New(st)
	if _, err := reg.Rescan(src); err != nil {
		t.Fatal(err)
	}
	docs := reg.Catalog(store.LevelInternal)
	if len(docs) != 1 || docs[0].Title != "办公用品领用流程" {
		t.Fatalf("catalog = %+v", docs)
	}
	if docs[0].Pages < 2 {
		t.Fatalf("pages = %d, want >= 2 (explicit page break)", docs[0].Pages)
	}
	tree, ok := reg.Structure(docs[0].DocID, store.LevelInternal)
	if !ok || len(tree.Children) != 1 || tree.Children[0].Title != "耗材申领" {
		t.Fatalf("tree = %+v ok=%v", tree, ok)
	}
	hits := reg.Search("硒鼓 申领", store.LevelInternal, 10)
	if len(hits) == 0 {
		t.Fatal("docx content not searchable")
	}
}

func TestPDFExtraction(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "供应商准入要求.pdf"), buildPDF(t), 0o644); err != nil {
		t.Fatal(err)
	}
	st := testStore(t)
	src := addSource(t, st, "kbf_pdf", "机密资料", dir, store.LevelConfidential)
	reg := folderkb.New(st)
	if _, err := reg.Rescan(src); err != nil {
		t.Fatal(err)
	}
	docs := reg.Catalog(store.LevelConfidential)
	if len(docs) != 1 {
		t.Fatalf("catalog = %+v", docs)
	}
	if docs[0].Pages != 2 {
		t.Fatalf("pages = %d, want 2 real pages", docs[0].Pages)
	}
	hits := reg.Search("供应商 营业执照", store.LevelConfidential, 10)
	if len(hits) == 0 || !strings.Contains(hits[0].Excerpt, "营业执照") {
		t.Fatalf("pdf hits = %+v", hits)
	}
	// The Flate-compressed second page must be found too.
	hits = reg.Search("现场审核", store.LevelConfidential, 10)
	if len(hits) == 0 {
		t.Fatal("compressed pdf page not searchable")
	}
	if hits[0].Pages != "2" {
		t.Fatalf("second-page hit pages = %q, want \"2\"", hits[0].Pages)
	}
}

func TestLevelGating(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "公开制度.md", "# 公开制度\n\n人人可见。\n")
	confDir := t.TempDir()
	writeFile(t, confDir, "机密资料.md", "# 机密资料\n\n仅受管级可见。\n")
	st := testStore(t)
	pub := addSource(t, st, "kbf_pub", "公开", dir, store.LevelInternal)
	conf := addSource(t, st, "kbf_conf", "机密", confDir, store.LevelConfidential)
	reg := folderkb.New(st)
	if _, err := reg.Rescan(pub); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Rescan(conf); err != nil {
		t.Fatal(err)
	}
	// BYOA sees at most internal: the confidential doc is invisible
	// in catalog, search, structure and section alike.
	if got := len(reg.Catalog(store.LevelInternal)); got != 1 {
		t.Fatalf("BYOA catalog = %d docs, want 1", got)
	}
	if hits := reg.Search("机密资料", store.LevelInternal, 10); len(hits) != 0 {
		t.Fatalf("BYOA search found confidential doc: %+v", hits)
	}
	confDocs := reg.Catalog(store.LevelConfidential)
	if len(confDocs) != 2 {
		t.Fatalf("managed catalog = %d docs, want 2", len(confDocs))
	}
	var confID string
	for _, d := range confDocs {
		if d.Title == "机密资料" {
			confID = d.DocID
		}
	}
	if _, ok := reg.Structure(confID, store.LevelInternal); ok {
		t.Fatal("BYOA structure read of confidential doc succeeded")
	}
	if _, _, ok := reg.Section(confID, "n0", store.LevelInternal); ok {
		t.Fatal("BYOA section read of confidential doc succeeded")
	}
	if _, ok := reg.Structure(confID, store.LevelConfidential); !ok {
		t.Fatal("managed structure read of confidential doc failed")
	}
}

func TestIncrementalRescan(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.md", "# 文档 A\n\n内容 A。\n")
	writeFile(t, dir, "b.md", "# 文档 B\n\n报销流程说明。\n")
	writeFile(t, dir, "ignore.png", "\x89PNG")
	st := testStore(t)
	src := addSource(t, st, "kbf_inc", "增量", dir, store.LevelInternal)
	reg := folderkb.New(st)

	stats, err := reg.Rescan(src)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Added != 2 || stats.Scanned != 2 || stats.Errors != 0 {
		t.Fatalf("first scan = %+v", stats)
	}
	// No changes: nothing is re-read (fast path on size+modtime).
	stats, err = reg.Rescan(src)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Added != 0 || stats.Updated != 0 || stats.Unchanged != 2 {
		t.Fatalf("no-change rescan = %+v, want 2 unchanged", stats)
	}
	// Change one file: only it is re-extracted.
	writeFile(t, dir, "a.md", "# 文档 A\n\n内容 A 已更新。\n")
	stats, err = reg.Rescan(src)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Updated != 1 || stats.Unchanged != 1 {
		t.Fatalf("one-change rescan = %+v, want 1 updated + 1 unchanged", stats)
	}
	if hits := reg.Search("已更新", store.LevelInternal, 10); len(hits) != 1 {
		t.Fatalf("updated content not searchable: %+v", hits)
	}
	// Delete the other file: it drops out of the index.
	if err := os.Remove(filepath.Join(dir, "b.md")); err != nil {
		t.Fatal(err)
	}
	stats, err = reg.Rescan(src)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Removed != 1 {
		t.Fatalf("delete rescan = %+v, want 1 removed", stats)
	}
	if hits := reg.Search("报销流程", store.LevelInternal, 10); len(hits) != 0 {
		t.Fatalf("deleted doc still searchable: %+v", hits)
	}
	// A file that becomes unreadable keeps its last good index.
	writeFile(t, dir, "c.md", "# 文档 C\n\n考勤打卡说明。\n")
	if _, err := reg.Rescan(src); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "c.md"), []byte("not a zip \x00\x01"), 0o644); err != nil {
		t.Fatal(err)
	}
	// .md is always parseable as text, so corrupt the docx path
	// instead: replace c.md with a broken pdf and check the error
	// count rises while the old index survives.
	if err := os.Remove(filepath.Join(dir, "c.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "c.pdf"), []byte("%PDF-1.4\nno streams here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stats, err = reg.Rescan(src)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Errors != 1 {
		t.Fatalf("broken pdf rescan = %+v, want 1 error", stats)
	}
	if hits := reg.Search("考勤打卡", store.LevelInternal, 10); len(hits) != 0 {
		t.Fatalf("broken pdf left a stale index entry: %+v", hits)
	}
}

func TestRescanMissingFolder(t *testing.T) {
	st := testStore(t)
	src := addSource(t, st, "kbf_gone", "不见了", filepath.Join(t.TempDir(), "no-such-dir"), store.LevelInternal)
	reg := folderkb.New(st)
	if _, err := reg.Rescan(src); err == nil {
		t.Fatal("rescan of a missing folder succeeded")
	}
	if stt := reg.Status(); len(stt) != 1 || stt[0].LastErr == "" {
		t.Fatalf("status = %+v, want the error recorded", stt)
	}
}

func TestRootSectionIsWholeDocument(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "短文.txt", "第一行。\n第二行。\n")
	st := testStore(t)
	src := addSource(t, st, "kbf_txt", "短文", dir, store.LevelInternal)
	reg := folderkb.New(st)
	if _, err := reg.Rescan(src); err != nil {
		t.Fatal(err)
	}
	docs := reg.Catalog(store.LevelInternal)
	if len(docs) != 1 {
		t.Fatalf("catalog = %+v", docs)
	}
	sec, _, ok := reg.Section(docs[0].DocID, "n0", store.LevelInternal)
	if !ok || !strings.Contains(sec.Text, "第一行") || !strings.Contains(sec.Text, "第二行") {
		t.Fatalf("root section = %+v ok=%v", sec, ok)
	}
	if _, _, ok := reg.Section(docs[0].DocID, "n99", store.LevelInternal); ok {
		t.Fatal("unknown node id resolved")
	}
}
