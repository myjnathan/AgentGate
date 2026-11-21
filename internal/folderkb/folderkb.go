// Package folderkb implements the folder-as-knowledge-base source
// (backlog F18): a shared folder is registered with a data level and
// its files are indexed automatically — no pre-built knowledge base
// is assumed, the index grows out of the files the company already
// has. Retrieval follows PageIndex's progressive-disclosure shape
// (tree index + agent reasoning, no vector DB — the same zero
// dependency line as the rest of AgentGate):
//
//  1. Catalog: the Wiki home — which documents exist, at which level,
//     with their top-level sections.
//  2. Structure: one document's section tree (title, page range and
//     an extractive summary per node — no full text yet).
//  3. Section: the full text of a single node, cited as
//     file + section + pages.
//
// Summaries are extractive (the section's own opening sentences),
// never produced by a model call, so summarization cannot become a
// leak channel: a summary is a substring of its document and is
// served under the same level gate as the document itself.
//
// Indexing is incremental: a file whose size and modification time
// are unchanged is not even read again; a file whose content hash
// matches is kept as-is. Only added or changed files are
// re-extracted, and deleted files drop out of the index.
package folderkb

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/myjnathan/agentgate/internal/store"
)

// LinesPerPage is the pseudo-page size for formats without real
// pagination (.md/.txt/.docx). A citation "page" is therefore a
// stable 45-line window, not a rendered page; PDFs use real pages.
const LinesPerPage = 45

// SupportedExt lists the file types the indexer extracts text from.
// .docx is a zip of XML (stdlib archive/zip + encoding/xml); .pdf
// extraction is best-effort (see extract.go) — scanned-image PDFs
// and PDFs with custom font encodings yield no text and are reported
// in the scan stats instead of silently indexing nothing.
var SupportedExt = map[string]bool{
	".md": true, ".txt": true, ".docx": true, ".pdf": true,
}

// Node is one entry of a document's section tree: a title, the page
// range it covers, an extractive summary, and its children.
type Node struct {
	ID        string  `json:"node_id"`
	Title     string  `json:"title"`
	StartPage int     `json:"start_page"`
	EndPage   int     `json:"end_page"`
	Summary   string  `json:"summary"`
	Children  []*Node `json:"children,omitempty"`
}

// Section is the full text of one tree node, with its citation.
type Section struct {
	NodeID    string `json:"node_id"`
	Title     string `json:"title"`
	StartPage int    `json:"start_page"`
	EndPage   int    `json:"end_page"`
	Text      string `json:"text"`
}

// Document is one indexed file: its tree, its sections (flattened
// for search), and the fingerprint that drives incremental updates.
type Document struct {
	ID          string
	SourceID    string
	RelPath     string // slash-separated path inside the folder
	Title       string
	Level       store.DataLevel
	Pages       int
	Tree        *Node
	sections    []*Section // pre-order, root first
	Fingerprint string
	Size        int64
	ModTime     time.Time
}

// CatalogEntry is one row of the Wiki home: a document and its
// top-level section titles, so an agent can pick a document before
// reading any tree.
type CatalogEntry struct {
	DocID    string   `json:"doc_id"`
	Title    string   `json:"title"`
	Source   string   `json:"source"`
	RelPath  string   `json:"rel_path"`
	Level    string   `json:"level"`
	Pages    int      `json:"pages"`
	Sections []string `json:"sections"`
}

// Hit is one search result, at section granularity so the citation
// can name the file, the section and the page range.
type Hit struct {
	DocID   string `json:"doc_id"`
	Title   string `json:"title"`
	Section string `json:"section,omitempty"`
	Pages   string `json:"pages,omitempty"` // "3" or "3-5"
	Level   string `json:"level"`
	Score   int    `json:"score"`
	Excerpt string `json:"excerpt"`
	Source  string `json:"source"` // "kb" (store docs) or "folder"
	Version int    `json:"version,omitempty"`
}

// ScanStats reports what one rescan did. Errors counts files that
// could not be extracted at all (they are not indexed).
type ScanStats struct {
	Scanned   int `json:"scanned"`
	Added     int `json:"added"`
	Updated   int `json:"updated"`
	Removed   int `json:"removed"`
	Unchanged int `json:"unchanged"`
	Errors    int `json:"errors"`
}

// SourceStatus is the admin-facing state of one folder source.
type SourceStatus struct {
	Source   *store.KBFolderSource `json:"source"`
	Docs     int                   `json:"docs"`
	LastScan *time.Time            `json:"last_scan,omitempty"`
	LastErr  string                `json:"last_error,omitempty"`
	Last     ScanStats             `json:"last_scan_stats"`
}

// Registry holds the in-memory folder index. Sources are persisted
// in the store; the index is rebuilt by Rescan (at startup and on
// admin request) and is never written to disk.
type Registry struct {
	st *store.Store
	mu sync.RWMutex
	// docs by document ID.
	docs map[string]*Document
	// status by source ID.
	status map[string]*SourceStatus
}

func New(st *store.Store) *Registry {
	return &Registry{st: st, docs: map[string]*Document{}, status: map[string]*SourceStatus{}}
}

// Sources returns copies of the registered folder sources.
func (r *Registry) Sources() []*store.KBFolderSource {
	var out []*store.KBFolderSource
	r.st.Read(func(st *store.State) {
		for _, s := range st.KBFolderSources {
			cp := *s
			out = append(out, &cp)
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Status returns the admin-facing status of every source.
func (r *Registry) Status() []*SourceStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*SourceStatus
	for _, src := range r.Sources() {
		if st, ok := r.status[src.ID]; ok {
			cp := *st
			out = append(out, &cp)
			continue
		}
		out = append(out, &SourceStatus{Source: src})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source.Name < out[j].Source.Name })
	return out
}

// Source returns a copy of one registered source.
func (r *Registry) Source(id string) (*store.KBFolderSource, bool) {
	var found *store.KBFolderSource
	r.st.Read(func(st *store.State) {
		if s, ok := st.KBFolderSources[id]; ok {
			cp := *s
			found = &cp
		}
	})
	return found, found != nil
}

// DocCount returns how many documents of one source are indexed.
func (r *Registry) DocCount(sourceID string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, d := range r.docs {
		if d.SourceID == sourceID {
			n++
		}
	}
	return n
}

// DropSource removes a source's documents from the index (used when
// the admin deletes the source registration).
func (r *Registry) DropSource(sourceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, d := range r.docs {
		if d.SourceID == sourceID {
			delete(r.docs, id)
		}
	}
	delete(r.status, sourceID)
}

// RescanAll re-indexes every enabled source. A source that fails
// (missing folder, unreadable) keeps its old index and records the
// error; one bad folder never blocks the others.
func (r *Registry) RescanAll() map[string]ScanStats {
	out := map[string]ScanStats{}
	for _, src := range r.Sources() {
		if !src.Enabled {
			continue
		}
		stats, _ := r.Rescan(src)
		out[src.ID] = stats
	}
	return out
}

// Rescan walks one folder and updates the index incrementally:
// unchanged files (same size + modtime) are not read at all,
// content-identical files keep their parsed document, changed files
// are re-extracted, vanished files are removed.
func (r *Registry) Rescan(src *store.KBFolderSource) (ScanStats, error) {
	var stats ScanStats
	info, err := os.Stat(src.Path)
	if err != nil {
		r.recordError(src, err)
		return stats, fmt.Errorf("folder %q: %w", src.Path, err)
	}
	if !info.IsDir() {
		err := fmt.Errorf("folder %q is not a directory", src.Path)
		r.recordError(src, err)
		return stats, err
	}
	seen := map[string]bool{}
	walkErr := filepath.WalkDir(src.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			stats.Errors++
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && path != src.Path {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if !SupportedExt[ext] {
			return nil
		}
		rel, err := filepath.Rel(src.Path, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		seen[rel] = true
		stats.Scanned++
		r.indexFile(src, path, rel, d, &stats)
		return nil
	})
	if walkErr != nil {
		r.recordError(src, walkErr)
		return stats, walkErr
	}
	// Remove documents whose files vanished.
	r.mu.Lock()
	for id, d := range r.docs {
		if d.SourceID == src.ID && !seen[d.RelPath] {
			delete(r.docs, id)
			stats.Removed++
		}
	}
	r.mu.Unlock()

	now := time.Now()
	r.mu.Lock()
	st := r.status[src.ID]
	if st == nil {
		st = &SourceStatus{Source: src}
		r.status[src.ID] = st
	}
	st.LastScan = &now
	st.LastErr = ""
	st.Last = stats
	st.Docs = r.docCountLocked(src.ID)
	r.mu.Unlock()
	return stats, nil
}

func (r *Registry) recordError(src *store.KBFolderSource, err error) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.status[src.ID]
	if st == nil {
		st = &SourceStatus{Source: src}
		r.status[src.ID] = st
	}
	st.LastScan = &now
	st.LastErr = err.Error()
}

func (r *Registry) docCountLocked(sourceID string) int {
	n := 0
	for _, d := range r.docs {
		if d.SourceID == sourceID {
			n++
		}
	}
	return n
}

// indexFile indexes one file, honouring the incremental fast paths.
// It never removes the old document on extraction failure: a file
// that became unreadable keeps serving its last good index.
func (r *Registry) indexFile(src *store.KBFolderSource, path, rel string, d fs.DirEntry, stats *ScanStats) {
	id := DocID(src.ID, rel)
	info, err := d.Info()
	if err != nil {
		stats.Errors++
		return
	}
	r.mu.RLock()
	old, exists := r.docs[id]
	r.mu.RUnlock()
	if exists && old.Size == info.Size() && old.ModTime.Equal(info.ModTime()) {
		stats.Unchanged++
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		stats.Errors++
		return
	}
	sum := sha256.Sum256(b)
	fp := hex.EncodeToString(sum[:])
	if exists && old.Fingerprint == fp {
		// Content identical, only the timestamp moved: keep the
		// parsed document, refresh the fast-path markers.
		r.mu.Lock()
		old.Size, old.ModTime = info.Size(), info.ModTime()
		r.mu.Unlock()
		stats.Unchanged++
		return
	}
	doc, err := parseDocument(src, rel, b)
	if err != nil {
		stats.Errors++
		return
	}
	doc.Fingerprint = fp
	doc.Size = info.Size()
	doc.ModTime = info.ModTime()
	r.mu.Lock()
	r.docs[id] = doc
	r.mu.Unlock()
	if exists {
		stats.Updated++
	} else {
		stats.Added++
	}
}

// DocID is the stable ID of a folder document: folder:<source>:<relpath>.
func DocID(sourceID, rel string) string {
	return "folder:" + sourceID + ":" + rel
}

// document returns a copy of one indexed document, or false.
func (r *Registry) document(id string) (*Document, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.docs[id]
	return d, ok
}

// Catalog lists the documents visible at maxLevel: the Wiki home
// (layer 1 of progressive disclosure).
func (r *Registry) Catalog(maxLevel store.DataLevel) []CatalogEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []CatalogEntry
	for _, d := range r.docs {
		if d.Level > maxLevel {
			continue
		}
		var sections []string
		for _, ch := range d.Tree.Children {
			sections = append(sections, ch.Title)
		}
		srcName := ""
		r.st.Read(func(st *store.State) {
			if s, ok := st.KBFolderSources[d.SourceID]; ok {
				srcName = s.Name
			}
		})
		out = append(out, CatalogEntry{DocID: d.ID, Title: d.Title, Source: srcName,
			RelPath: d.RelPath, Level: d.Level.String(), Pages: d.Pages, Sections: sections})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].Title < out[j].Title
	})
	return out
}

// Structure returns a document's section tree (layer 2: titles,
// page ranges and summaries only — no full text). A document above
// the caller's level is indistinguishable from an unknown one.
func (r *Registry) Structure(docID string, maxLevel store.DataLevel) (*Node, bool) {
	d, ok := r.document(docID)
	if !ok || d.Level > maxLevel {
		return nil, false
	}
	return cloneNode(d.Tree), true
}

// Section returns the full text of one tree node (layer 3), with
// its citation. nodeID "n0" is the document root (whole document).
func (r *Registry) Section(docID, nodeID string, maxLevel store.DataLevel) (*Section, string, bool) {
	d, ok := r.document(docID)
	if !ok || d.Level > maxLevel {
		return nil, "", false
	}
	for _, s := range d.sections {
		if s.NodeID == nodeID {
			cp := *s
			return &cp, d.RelPath, true
		}
	}
	return nil, "", false
}

// Search scores sections by keyword frequency across the document
// title, the section title and the section text (same shape as the
// store-KB search: title matches weigh more). Only sections of
// documents at or below maxLevel are visible.
func (r *Registry) Search(query string, maxLevel store.DataLevel, limit int) []Hit {
	terms := splitTerms(query)
	if len(terms) == 0 || limit <= 0 {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var hits []Hit
	for _, d := range r.docs {
		if d.Level > maxLevel {
			continue
		}
		lowerTitle := strings.ToLower(d.Title)
		for _, s := range d.sections {
			// The root section carries the whole document text; when
			// the document has finer sections, searching it as well
			// would return every match twice. A single-section
			// document (no headings, one page) keeps its root.
			if s.NodeID == "n0" && len(d.sections) > 1 {
				continue
			}
			score := 0
			lowerSec := strings.ToLower(s.Title)
			lowerText := strings.ToLower(s.Text)
			for _, t := range terms {
				score += 2 * strings.Count(lowerTitle, t)
				score += 3 * strings.Count(lowerSec, t)
				score += strings.Count(lowerText, t)
			}
			if score == 0 {
				continue
			}
			pages := fmt.Sprintf("%d", s.StartPage)
			if s.EndPage > s.StartPage {
				pages = fmt.Sprintf("%d-%d", s.StartPage, s.EndPage)
			}
			hits = append(hits, Hit{
				DocID: d.ID, Title: d.Title, Section: s.Title, Pages: pages,
				Level: d.Level.String(), Score: score,
				Excerpt: excerpt(s.Text, terms), Source: "folder",
			})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].DocID < hits[j].DocID
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

func cloneNode(n *Node) *Node {
	if n == nil {
		return nil
	}
	cp := *n
	cp.Children = nil
	for _, ch := range n.Children {
		cp.Children = append(cp.Children, cloneNode(ch))
	}
	return &cp
}

func splitTerms(q string) []string {
	fields := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return r == ' ' || r == ',' || r == '，' || r == '、' || r == '\t' || r == '\n'
	})
	var out []string
	for _, f := range fields {
		if len(f) >= 2 {
			out = append(out, f)
		}
	}
	if len(fields) > 1 {
		out = append(out, strings.ToLower(strings.TrimSpace(q)))
	}
	return out
}

func excerpt(content string, terms []string) string {
	lower := strings.ToLower(content)
	best := -1
	for _, t := range terms {
		if i := strings.Index(lower, t); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	runes := []rune(content)
	if best < 0 {
		if len(runes) > 160 {
			return string(runes[:160]) + "…"
		}
		return content
	}
	runeIdx := len([]rune(content[:best]))
	start := runeIdx - 40
	if start < 0 {
		start = 0
	}
	end := runeIdx + 120
	if end > len(runes) {
		end = len(runes)
	}
	return "…" + string(runes[start:end]) + "…"
}

// summarize returns the extractive summary of a section: its first
// sentence, or the first 160 runes when the text has no sentence
// boundary. No model is involved (see the package doc).
func summarize(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	runes := []rune(text)
	for i, r := range runes {
		if r == '。' || r == '！' || r == '？' || r == '.' || r == '!' || r == '?' {
			if i+1 >= 40 { // don't end on a one-word fragment
				return string(runes[:i+1])
			}
		}
	}
	if len(runes) > 160 {
		return string(runes[:160]) + "…"
	}
	return text
}
