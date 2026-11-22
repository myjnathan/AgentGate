// Package semidx implements the semantic index layer over the folder
// knowledge base (backlog F19): one FRAS semantic entry per section
// (F function / R relations / A applicability / S hard constraints)
// plus tag coordinates (department / type / importance), served as
// the catalog layer so an agent can judge relevance before drilling
// down to the section tree (F18) and the section text.
//
// The method is borrowed from AOCI-CODE (aoci-spec/aoci-code): one
// semantic entry per object, tag coordinates, and index governance
// (format validation, drift detection, coverage reporting). Only the
// method is borrowed — AOCI-CODE is licensed FSL-1.1-MIT (Fair
// Source), which is incompatible with this project's AGPL-3.0, so no
// AOCI code is copied; everything here is written from scratch.
//
// Two generators produce entries:
//
//   - Extractive (default): deterministic, built only from the
//     section tree the indexer already extracted (titles, summaries,
//     page ranges, tree position) plus folder metadata. No model is
//     involved, so generation cannot become a leak channel — the
//     same line F18 took for summaries.
//   - Model (opt-in, see model.go): entries are written by a model
//     reached through the gateway's chat-completions API, so
//     classification routing and desensitization (F17) apply to the
//     generation request itself. The prompt carries only chapter
//     titles and extractive summaries, never full section text.
//
// Redlines, enforced here rather than left to callers:
//
//   - Every entry carries a source back-reference (file + chapter +
//     pages) filled in from the index tree, never from model text:
//     a model cannot invent a citation.
//   - Secret (绝密) documents never reach a model: they get
//     title-only entries (chapter titles and page ranges, no FRAS
//     content) however generation was requested.
//   - Entries are served under the same level gate as their document
//     (see Entries): a tier that cannot read the document cannot
//     read its entries either.
package semidx

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/myjnathan/agentgate/internal/folderkb"
	"github.com/myjnathan/agentgate/internal/store"
)

// GeneratedBy records how an entry was produced.
const (
	GeneratedExtractive = "extractive" // built from the section tree, no model
	GeneratedModel      = "model"      // written by a model via the gateway
	GeneratedMixed      = "mixed"      // model entries + extractive fill-ins
	GeneratedTitleOnly  = "title-only" // secret docs: titles only, no FRAS content
)

// Importance values for the tag coordinates.
const (
	ImportanceLow      = "low"
	ImportanceMedium   = "medium"
	ImportanceHigh     = "high"
	ImportanceCritical = "critical"
)

// MaxFRASRunes bounds one FRAS field: model output is capped so a
// runaway completion cannot bloat the index.
const MaxFRASRunes = 500

// Tags are the coordinate labels of an entry (AOCI's tag coordinates):
// which department owns the document, what kind of document it is,
// and how important it is (derived from the data level).
type Tags struct {
	Department string `json:"department"`
	Type       string `json:"type"`
	Importance string `json:"importance"`
}

// SourceRef is the mandatory back-reference of an entry: the file,
// the chapter and the page range the entry was drawn from. It is
// always filled in from the index tree, never from model text.
type SourceRef struct {
	File    string `json:"file"`
	Chapter string `json:"chapter"`
	Pages   string `json:"pages"` // "3" or "3-5"
}

// Entry is one FRAS semantic entry for one section-tree node.
type Entry struct {
	DocID         string    `json:"doc_id"`
	NodeID        string    `json:"node_id"`
	Function      string    `json:"function"`
	Relations     string    `json:"relations"`
	Applicability string    `json:"applicability"`
	Constraints   string    `json:"constraints"`
	Tags          Tags      `json:"tags"`
	Source        SourceRef `json:"source"`
	GeneratedBy   string    `json:"generated_by"`
	GeneratedAt   time.Time `json:"generated_at"`
}

var pagesRe = regexp.MustCompile(`^\d+(-\d+)?$`)

// Validate checks one entry's format: required fields, bounded FRAS
// text, known tag values, and a well-formed back-reference. Title-only
// entries (secret documents) carry only the chapter title in Function
// and are exempt from the non-empty FRAS rule.
func Validate(e Entry) error {
	if e.DocID == "" {
		return fmt.Errorf("doc_id is required")
	}
	if e.NodeID == "" {
		return fmt.Errorf("node_id is required")
	}
	switch e.GeneratedBy {
	case GeneratedExtractive, GeneratedModel, GeneratedMixed, GeneratedTitleOnly:
	default:
		return fmt.Errorf("unknown generated_by %q", e.GeneratedBy)
	}
	if e.GeneratedBy == GeneratedTitleOnly {
		if e.Function == "" {
			return fmt.Errorf("title-only entry requires the chapter title in function")
		}
	} else {
		for name, v := range map[string]string{
			"function": e.Function, "relations": e.Relations,
			"applicability": e.Applicability, "constraints": e.Constraints,
		} {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("%s is required", name)
			}
			if len([]rune(v)) > MaxFRASRunes {
				return fmt.Errorf("%s exceeds %d runes", name, MaxFRASRunes)
			}
		}
	}
	if len([]rune(e.Function)) > MaxFRASRunes {
		return fmt.Errorf("function exceeds %d runes", MaxFRASRunes)
	}
	if strings.TrimSpace(e.Tags.Department) == "" || len([]rune(e.Tags.Department)) > 64 {
		return fmt.Errorf("tags.department is required (max 64 runes)")
	}
	if strings.TrimSpace(e.Tags.Type) == "" || len([]rune(e.Tags.Type)) > 32 {
		return fmt.Errorf("tags.type is required (max 32 runes)")
	}
	switch e.Tags.Importance {
	case ImportanceLow, ImportanceMedium, ImportanceHigh, ImportanceCritical:
	default:
		return fmt.Errorf("tags.importance must be low|medium|high|critical")
	}
	if e.Source.File == "" || e.Source.Chapter == "" {
		return fmt.Errorf("source file and chapter are required")
	}
	if !pagesRe.MatchString(e.Source.Pages) {
		return fmt.Errorf("source pages %q must be \"N\" or \"N-M\"", e.Source.Pages)
	}
	return nil
}

// DocEntries is the stored semantic index of one document.
type DocEntries struct {
	DocID       string    `json:"doc_id"`
	SourceID    string    `json:"source_id"`
	Title       string    `json:"title"`
	Level       string    `json:"level"`
	SourceHash  string    `json:"source_hash"`
	Entries     []Entry   `json:"entries"`
	GeneratedBy string    `json:"generated_by"`
	GeneratedAt time.Time `json:"generated_at"`
	ModelError  string    `json:"model_error,omitempty"`
	Stale       bool      `json:"stale"`
}

// DocStatus is the admin-facing per-document row of a coverage report.
type DocStatus struct {
	DocID       string `json:"doc_id"`
	Title       string `json:"title"`
	Level       string `json:"level"`
	Entries     int    `json:"entries"`
	Chapters    int    `json:"chapters"`
	Stale       bool   `json:"stale"`
	GeneratedBy string `json:"generated_by"`
	GeneratedAt string `json:"generated_at,omitempty"`
	ModelError  string `json:"model_error,omitempty"`
}

// Coverage is the index-governance report: how much of the folder
// knowledge base has semantic entries, and what drifted since the
// entries were generated.
type Coverage struct {
	DocsTotal       int         `json:"docs_total"`
	DocsIndexed     int         `json:"docs_indexed"`
	ChaptersTotal   int         `json:"chapters_total"`
	ChaptersIndexed int         `json:"chapters_indexed"`
	StaleDocs       []string    `json:"stale_docs"`
	MissingDocs     []string    `json:"missing_docs"`
	Documents       []DocStatus `json:"documents"`
}

// Generator produces the entries of one document snapshot.
type Generator interface {
	Generate(ctx context.Context, snap folderkb.DocSnapshot) ([]Entry, error)
}

// Index is the in-memory semantic index, keyed by document ID. Like
// the folder index it sits on, it is rebuilt from the files on
// rescan and is never written to disk.
type Index struct {
	mu   sync.RWMutex
	docs map[string]*DocEntries
}

// New returns an empty semantic index.
func New() *Index {
	return &Index{docs: map[string]*DocEntries{}}
}

// ImportanceForLevel derives the importance tag from the data level:
// a higher classification is a more important document.
func ImportanceForLevel(l store.DataLevel) string {
	switch l {
	case store.LevelPublic:
		return ImportanceLow
	case store.LevelInternal:
		return ImportanceMedium
	case store.LevelConfidential:
		return ImportanceHigh
	case store.LevelSecret:
		return ImportanceCritical
	}
	return ImportanceMedium
}

// TypeForExt derives the type tag from the file extension.
func TypeForExt(relPath string) string {
	switch {
	case strings.HasSuffix(strings.ToLower(relPath), ".md"):
		return "Markdown"
	case strings.HasSuffix(strings.ToLower(relPath), ".txt"):
		return "纯文本"
	case strings.HasSuffix(strings.ToLower(relPath), ".docx"):
		return "Word"
	case strings.HasSuffix(strings.ToLower(relPath), ".pdf"):
		return "PDF"
	}
	return "文档"
}

// DepartmentFor returns the department tag of a snapshot: the folder's
// configured department when set, otherwise the folder's own name
// (the collection is the best available owner label).
func DepartmentFor(snap folderkb.DocSnapshot) string {
	if strings.TrimSpace(snap.Department) != "" {
		return snap.Department
	}
	return snap.SourceName
}

// levelConstraint states the access rule of a document's level as the
// entry's hard constraint: it is derived from the platform's own
// level gate, not invented per document.
func levelConstraint(l store.DataLevel) string {
	switch l {
	case store.LevelPublic:
		return "公开级：无取用限制。"
	case store.LevelInternal:
		return "内部级：自带级与受管级均可见。"
	case store.LevelConfidential:
		return "机密级：仅受管级可见，自带级不可见。"
	case store.LevelSecret:
		return "绝密级：仅登记标题与章节名，不生成语义条目。"
	}
	return "内部级：自带级与受管级均可见。"
}

// nodeInfo is one tree node with its structural context, used to
// build extractive entries.
type nodeInfo struct {
	node     *folderkb.Node
	parent   *folderkb.Node
	siblings []*folderkb.Node
}

// walkNodes flattens the section tree pre-order, root first, keeping
// each node's parent and siblings for the Relations field.
func walkNodes(root *folderkb.Node) []nodeInfo {
	var out []nodeInfo
	var walk func(n, parent *folderkb.Node, siblings []*folderkb.Node)
	walk = func(n, parent *folderkb.Node, siblings []*folderkb.Node) {
		out = append(out, nodeInfo{node: n, parent: parent, siblings: siblings})
		for _, ch := range n.Children {
			walk(ch, n, n.Children)
		}
	}
	walk(root, nil, nil)
	return out
}

func pagesOf(n *folderkb.Node) string {
	if n.EndPage > n.StartPage {
		return fmt.Sprintf("%d-%d", n.StartPage, n.EndPage)
	}
	return fmt.Sprintf("%d", n.StartPage)
}

// extractiveEntries builds one entry per tree node from the index
// data alone: Function is the chapter title plus its extractive
// summary, Relations are the structural neighbours, Applicability is
// the folder scope, Constraints are the level's access rule.
func extractiveEntries(snap folderkb.DocSnapshot, now time.Time) []Entry {
	tags := Tags{Department: DepartmentFor(snap), Type: TypeForExt(snap.RelPath),
		Importance: ImportanceForLevel(snap.Level)}
	var out []Entry
	for _, ni := range walkNodes(snap.Tree) {
		n := ni.node
		fn := n.Title
		if s := strings.TrimSpace(n.Summary); s != "" {
			fn = n.Title + "：" + s
		}
		var rels []string
		if ni.parent != nil {
			rels = append(rels, "上级章节："+ni.parent.Title)
		}
		var sibs []string
		for _, s := range ni.siblings {
			if s != n {
				sibs = append(sibs, s.Title)
			}
		}
		if len(sibs) > 0 {
			rels = append(rels, "同级章节："+strings.Join(sibs, "、"))
		}
		if len(n.Children) > 0 {
			var kids []string
			for _, ch := range n.Children {
				kids = append(kids, ch.Title)
			}
			rels = append(rels, "包含章节："+strings.Join(kids, "、"))
		}
		out = append(out, Entry{
			DocID:         snap.ID,
			NodeID:        n.ID,
			Function:      fn,
			Relations:     strings.Join(rels, "；"),
			Applicability: fmt.Sprintf("适用范围：%s（%s），%s级。", snap.SourceName, tags.Department, snap.Level.String()),
			Constraints:   levelConstraint(snap.Level),
			Tags:          tags,
			Source:        SourceRef{File: snap.RelPath, Chapter: n.Title, Pages: pagesOf(n)},
			GeneratedBy:   GeneratedExtractive,
			GeneratedAt:   now,
		})
	}
	return out
}

// titleOnlyEntries builds the secret-document form: one entry per
// node carrying only the chapter title and its back-reference.
func titleOnlyEntries(snap folderkb.DocSnapshot, now time.Time) []Entry {
	tags := Tags{Department: DepartmentFor(snap), Type: TypeForExt(snap.RelPath),
		Importance: ImportanceForLevel(snap.Level)}
	var out []Entry
	for _, ni := range walkNodes(snap.Tree) {
		n := ni.node
		out = append(out, Entry{
			DocID:       snap.ID,
			NodeID:      n.ID,
			Function:    n.Title,
			Tags:        tags,
			Source:      SourceRef{File: snap.RelPath, Chapter: n.Title, Pages: pagesOf(n)},
			GeneratedBy: GeneratedTitleOnly,
			GeneratedAt: now,
		})
	}
	return out
}

// ExtractiveGenerator is the default generator: entries from the
// section tree alone, no model involved.
type ExtractiveGenerator struct{}

// Generate implements Generator.
func (ExtractiveGenerator) Generate(_ context.Context, snap folderkb.DocSnapshot) ([]Entry, error) {
	return extractiveEntries(snap, time.Now()), nil
}

// RegenerateDoc (re)generates one document's entries with gen. Secret
// documents always get title-only entries and gen is never called
// for them. When gen fails — a model that cannot be reached, or
// output that fails validation — the document falls back to
// extractive entries and the failure is recorded in ModelError, so
// the index never ends up empty and the failure stays visible.
func (ix *Index) RegenerateDoc(ctx context.Context, snap folderkb.DocSnapshot, gen Generator) *DocEntries {
	now := time.Now()
	de := &DocEntries{DocID: snap.ID, SourceID: snap.SourceID, Title: snap.Title,
		Level: snap.Level.String(), SourceHash: snap.Fingerprint, GeneratedAt: now}
	if snap.Level == store.LevelSecret {
		de.Entries = titleOnlyEntries(snap, now)
		de.GeneratedBy = GeneratedTitleOnly
		ix.store(de)
		return de
	}
	entries, err := gen.Generate(ctx, snap)
	if err != nil {
		de.ModelError = err.Error()
		de.Entries = extractiveEntries(snap, now)
		de.GeneratedBy = GeneratedExtractive
		ix.store(de)
		return de
	}
	// A model may return entries for only some nodes; fill the rest
	// extractively and say so, instead of silently dropping chapters.
	byNode := map[string]Entry{}
	invalid := false
	for _, e := range entries {
		e.DocID = snap.ID
		if err := Validate(e); err != nil {
			invalid = true
			de.ModelError = fmt.Sprintf("model entry for node %s failed validation: %v", e.NodeID, err)
			break
		}
		byNode[e.NodeID] = e
	}
	if invalid {
		de.Entries = extractiveEntries(snap, now)
		de.GeneratedBy = GeneratedExtractive
		ix.store(de)
		return de
	}
	extractive := extractiveEntries(snap, now)
	byNodeExt := map[string]Entry{}
	for _, e := range extractive {
		byNodeExt[e.NodeID] = e
	}
	var out []Entry
	modelCount := 0
	for _, ni := range walkNodes(snap.Tree) {
		if e, ok := byNode[ni.node.ID]; ok {
			out = append(out, e)
			modelCount++
			continue
		}
		out = append(out, byNodeExt[ni.node.ID])
	}
	if modelCount == 0 {
		de.ModelError = "model returned no usable entries"
		de.Entries = out
		de.GeneratedBy = GeneratedExtractive
	} else if modelCount < len(out) {
		de.ModelError = fmt.Sprintf("model covered %d of %d sections; the rest are extractive", modelCount, len(out))
		de.Entries = out
		de.GeneratedBy = GeneratedMixed
	} else {
		de.Entries = out
		de.GeneratedBy = GeneratedModel
	}
	ix.store(de)
	return de
}

func (ix *Index) store(de *DocEntries) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.docs[de.DocID] = de
}

// RegenerateChanged regenerates entries for added or changed
// documents (extractive) and drops documents that vanished from the
// folder index. Unchanged documents are left alone.
func (ix *Index) RegenerateChanged(ctx context.Context, snaps []folderkb.DocSnapshot) (regenerated, unchanged int) {
	seen := map[string]bool{}
	gen := ExtractiveGenerator{}
	for _, snap := range snaps {
		seen[snap.ID] = true
		ix.mu.RLock()
		old, ok := ix.docs[snap.ID]
		ix.mu.RUnlock()
		if ok && old.SourceHash == snap.Fingerprint && !old.Stale {
			unchanged++
			continue
		}
		ix.RegenerateDoc(ctx, snap, gen)
		regenerated++
	}
	ix.mu.Lock()
	for id := range ix.docs {
		if !seen[id] {
			delete(ix.docs, id)
		}
	}
	ix.mu.Unlock()
	return regenerated, unchanged
}

// Entries returns a document's entries when the caller's level may
// read the document; otherwise it is indistinguishable from unknown.
func (ix *Index) Entries(docID string, maxLevel store.DataLevel) ([]Entry, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	de, ok := ix.docs[docID]
	if !ok {
		return nil, false
	}
	var lvl store.DataLevel
	if v, ok := store.ParseDataLevel(de.Level); ok {
		lvl = v
	}
	if lvl > maxLevel {
		return nil, false
	}
	out := make([]Entry, len(de.Entries))
	copy(out, de.Entries)
	return out, true
}

// CheckDrift compares stored source hashes with the current folder
// index and marks entries whose document changed since generation.
// It returns the stale document IDs.
func (ix *Index) CheckDrift(snaps []folderkb.DocSnapshot) []string {
	byID := map[string]folderkb.DocSnapshot{}
	for _, s := range snaps {
		byID[s.ID] = s
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	var stale []string
	for id, de := range ix.docs {
		snap, ok := byID[id]
		if !ok {
			de.Stale = true
			stale = append(stale, id)
			continue
		}
		de.Stale = de.SourceHash != snap.Fingerprint
		if de.Stale {
			stale = append(stale, id)
		}
	}
	return stale
}

// Coverage reports the index state against the current folder index:
// totals, per-document status, stale documents and indexed documents
// whose files vanished.
func (ix *Index) Coverage(snaps []folderkb.DocSnapshot) Coverage {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	cov := Coverage{StaleDocs: []string{}, MissingDocs: []string{}, Documents: []DocStatus{}}
	byID := map[string]folderkb.DocSnapshot{}
	for _, s := range snaps {
		byID[s.ID] = s
		cov.DocsTotal++
		cov.ChaptersTotal += countNodes(s.Tree)
	}
	for id, de := range ix.docs {
		snap, ok := byID[id]
		if !ok {
			cov.MissingDocs = append(cov.MissingDocs, id)
			continue
		}
		stale := de.SourceHash != snap.Fingerprint
		if stale {
			cov.StaleDocs = append(cov.StaleDocs, id)
		} else {
			cov.DocsIndexed++
			cov.ChaptersIndexed += len(de.Entries)
		}
		st := DocStatus{DocID: id, Title: de.Title, Level: de.Level,
			Entries: len(de.Entries), Chapters: countNodes(snap.Tree),
			Stale: stale, GeneratedBy: de.GeneratedBy, ModelError: de.ModelError}
		if !de.GeneratedAt.IsZero() {
			st.GeneratedAt = de.GeneratedAt.Format(time.RFC3339)
		}
		cov.Documents = append(cov.Documents, st)
	}
	return cov
}

func countNodes(n *folderkb.Node) int {
	if n == nil {
		return 0
	}
	c := 1
	for _, ch := range n.Children {
		c += countNodes(ch)
	}
	return c
}

// Drop removes one document's entries.
func (ix *Index) Drop(docID string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	delete(ix.docs, docID)
}

// DropSource removes every entry belonging to one folder source.
func (ix *Index) DropSource(sourceID string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	for id, de := range ix.docs {
		if de.SourceID == sourceID {
			delete(ix.docs, id)
		}
	}
}
