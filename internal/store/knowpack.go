// Curated knowledge packages (backlog F22): the curation layer over
// the folder knowledge base (F18) and its semantic index (F19). An
// administrator or department expert curates several folder documents
// around one concrete work problem into a "package": a source list
// (the recipe) + a synthesis (purpose, evidence usage, boundaries
// and conclusions) + the scope it may serve. After review and
// publication, agents read the package through MCP (catalog →
// synthesis → source drill-down) instead of searching the raw folder
// index — the value is controlled wording, accumulated experience
// and traceable responsibility (library vs. case files).
//
// The shape is borrowed from Liner (linersh, liner.sh, MIT): a tape
// file (tape.yaml) is the source recipe, a Mixtape is the compiled
// corpus (synthesis.md + sources/ + MIXTAPE.md), and the curation
// skill stops for human approval on the synthesis, the source list
// and the instructions, with a change receipt when a package is
// maintained. Only the shape and the curation flow are borrowed —
// Liner is a personal single-user local tool with no multi-user,
// classification or audit layer, so the governance layer here (level
// gate, downward-only references, review pipeline, audit receipts)
// is written from scratch, and no Liner code is copied.
//
// Governance rules, enforced by internal/knowpack and the API:
//
//   - A package's level is the highest level of its sources and of
//     the packages it references. It is computed when a version is
//     created and stored on the version, so serving never needs the
//     folder index.
//   - A package may reference another published package's synthesis,
//     but only downward: the referenced package's level must be at or
//     below this package's level, so a low-level package can never
//     embed high-level content. References are re-checked at publish
//     time and at MCP read time (a reference whose package has since
//     been withdrawn or republished higher is withheld, not served).
//   - Every synthesis version carries at least one citation, and each
//     citation is resolved against the folder index tree at creation:
//     file, chapter and pages are filled in from the tree, never from
//     author text — a citation cannot point at a section that does
//     not exist (the same structural guarantee F19 uses for entries).
//   - Versions are append-only like prompt templates (F8): an update
//     only creates a new version, a separate publish step marks one
//     version as the production version, and agents only ever read
//     the published version. Every create / version / review /
//     publish / delete is an audit event (the change receipt).
package store

import (
	"fmt"
	"strings"
	"time"
)

// MaxPackageContentBytes caps one synthesis version: packages are
// admin-authored text, not documents (same bound as prompt templates).
const MaxPackageContentBytes = 32 * 1024

// MaxPackageCitations caps the citations of one synthesis version.
const MaxPackageCitations = 64

// MaxPackageSources caps the sources of one recipe.
const MaxPackageSources = 64

// MaxPackageReferences caps the packages one package may reference.
const MaxPackageReferences = 16

// MaxChangeNoteRunes caps the change note (the maintenance receipt).
const MaxChangeNoteRunes = 500

// PackageSource is one entry of a package's recipe: a folder-indexed
// document (whole document when NodeID is empty) or a single section
// of it (NodeID set: the section and its subtree).
type PackageSource struct {
	DocID  string `json:"doc_id"` // folder:<source>:<relpath>
	NodeID string `json:"node_id,omitempty"`
}

// PackageCitation is one back-reference of a synthesis: the document
// and section a conclusion is drawn from. DocID/NodeID are what the
// author supplies; File/Chapter/Pages are filled in from the folder
// index tree at version creation and never taken from author text.
type PackageCitation struct {
	DocID   string `json:"doc_id"`
	NodeID  string `json:"node_id"`
	File    string `json:"file"`
	Chapter string `json:"chapter"`
	Pages   string `json:"pages"` // "3" or "3-5"
}

// PackageVersion is one immutable revision of a knowledge package:
// the recipe (sources + references), the synthesis text, and the
// citations that back it. Editing a package always creates a new
// version and never rewrites an old one, so a published version can
// always be rolled back to.
type PackageVersion struct {
	Version    int               `json:"version"`
	Content    string            `json:"content"` // synthesis: purpose / evidence usage / boundaries + conclusions
	Citations  []PackageCitation `json:"citations"`
	Sources    []PackageSource   `json:"sources"`
	References []string          `json:"references,omitempty"` // other package IDs (published synthesis reuse)
	ChangeNote string            `json:"change_note,omitempty"`
	Level      string            `json:"level"` // computed at creation: highest source/reference level
	CreatedAt  time.Time         `json:"created_at"`
	CreatedBy  string            `json:"created_by"`
}

// KnowledgePackage is a curated knowledge package in the capability
// hub. Status is draft | in_review | published | rejected. Scope
// limits the departments it is served to ("*" = all). The level of a
// package is the level of its published version (or, before anything
// is published, of its current version).
type KnowledgePackage struct {
	ID               string           `json:"id"`
	Name             string           `json:"name"` // snake_case, unique
	Description      string           `json:"description"`
	Scope            string           `json:"scope"` // comma-separated departments, or "*"
	Status           string           `json:"status"`
	CurrentVersion   int              `json:"current_version"`
	PublishedVersion int              `json:"published_version"` // 0 = nothing published yet
	Versions         []PackageVersion `json:"versions"`
	AuthorID         string           `json:"author_id"`
	CreatedAt        time.Time        `json:"created_at"`
	UpdatedAt        time.Time        `json:"updated_at"`
}

// Version returns the version record for n, or nil.
func (p *KnowledgePackage) Version(n int) *PackageVersion {
	for i := range p.Versions {
		if p.Versions[i].Version == n {
			return &p.Versions[i]
		}
	}
	return nil
}

// Published returns the currently published version, or nil when
// nothing has been published yet — or when the package was rejected
// after publishing: a rejection withdraws the package from serving,
// whatever version was published before.
func (p *KnowledgePackage) Published() *PackageVersion {
	if p.Status != "published" || p.PublishedVersion == 0 {
		return nil
	}
	return p.Version(p.PublishedVersion)
}

// Level returns the package's effective level: the published
// version's level when published, otherwise the current version's.
func (p *KnowledgePackage) Level() string {
	if v := p.Published(); v != nil {
		return v.Level
	}
	if v := p.Version(p.CurrentVersion); v != nil {
		return v.Level
	}
	return ""
}

// InScope reports whether a caller from department dept may see this
// package ("*" matches every department).
func (p *KnowledgePackage) InScope(dept string) bool {
	if p.Scope == "" || p.Scope == "*" {
		return true
	}
	for _, d := range strings.Split(p.Scope, ",") {
		if strings.TrimSpace(d) == dept {
			return true
		}
	}
	return false
}

// ValidatePackageVersion checks a new version's shape before any
// index lookup: content bounds, at least one source and one citation,
// no duplicate sources/citations/references, and citation fields
// present. Whether the sources and citations actually exist in the
// folder index, and whether the references are published and
// downward-only, is checked by internal/knowpack against the live
// index — this function is the shape gate, that one is the truth gate.
func ValidatePackageVersion(content string, citations []PackageCitation, sources []PackageSource, references []string, changeNote string) error {
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("content required")
	}
	if len(content) > MaxPackageContentBytes {
		return fmt.Errorf("content exceeds %d bytes", MaxPackageContentBytes)
	}
	if len([]rune(changeNote)) > MaxChangeNoteRunes {
		return fmt.Errorf("change_note exceeds %d runes", MaxChangeNoteRunes)
	}
	if len(sources) == 0 {
		return fmt.Errorf("at least one source is required")
	}
	if len(sources) > MaxPackageSources {
		return fmt.Errorf("at most %d sources", MaxPackageSources)
	}
	seenSrc := map[PackageSource]bool{}
	for _, s := range sources {
		if s.DocID == "" {
			return fmt.Errorf("source doc_id is required")
		}
		if seenSrc[s] {
			return fmt.Errorf("duplicate source %q (node %q)", s.DocID, s.NodeID)
		}
		seenSrc[s] = true
	}
	if len(references) > MaxPackageReferences {
		return fmt.Errorf("at most %d references", MaxPackageReferences)
	}
	seenRef := map[string]bool{}
	for _, r := range references {
		if r == "" {
			return fmt.Errorf("reference package id must not be empty")
		}
		if seenRef[r] {
			return fmt.Errorf("duplicate reference %q", r)
		}
		seenRef[r] = true
	}
	if len(citations) == 0 {
		return fmt.Errorf("at least one citation is required: every synthesis must carry a source back-reference (file + section + pages)")
	}
	if len(citations) > MaxPackageCitations {
		return fmt.Errorf("at most %d citations", MaxPackageCitations)
	}
	seenCit := map[string]bool{}
	for _, c := range citations {
		if c.DocID == "" || c.NodeID == "" {
			return fmt.Errorf("citation requires doc_id and node_id")
		}
		key := c.DocID + "\x00" + c.NodeID
		if seenCit[key] {
			return fmt.Errorf("duplicate citation for %q node %q", c.DocID, c.NodeID)
		}
		seenCit[key] = true
	}
	return nil
}
