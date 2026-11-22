// Package knowpack implements the truth checks for curated
// knowledge packages (backlog F22) against the live folder index.
//
// The store package only validates a version's shape (see
// store.ValidatePackageVersion); this package answers the questions
// that need the index:
//
//   - Do the recipe's sources exist, and does a node-scoped source
//     name a real section?
//   - Is each citation backed by a real section of a source in the
//     recipe, and what are its file / chapter / pages? The
//     back-reference is filled in from the section tree here, never
//     from author text — a citation cannot point at a section that
//     does not exist (the same structural guarantee F19 uses for
//     semantic entries).
//   - What is the version's level? The highest level of its sources
//     and of the packages it references.
//   - Are the references published and downward-only (a package may
//     only reuse the synthesis of a package at or below its own
//     level, so a low-level package can never embed high-level
//     content)?
//
// The curation shape (recipe + synthesis + approval checkpoints +
// change receipt) is borrowed from Liner (linersh, liner.sh, MIT);
// Liner is a personal single-user local tool with no multi-user,
// classification or audit layer, so the governance checks here are
// written from scratch and no Liner code is copied.
package knowpack

import (
	"fmt"
	"sort"

	"github.com/myjnathan/agentgate/internal/folderkb"
	"github.com/myjnathan/agentgate/internal/store"
)

// ResolveVersion checks one version's recipe against the folder index
// and the published packages, fills in every citation's
// back-reference from the section tree, and returns the version's
// level. The caller passes the package's own ID so a self-reference
// can be rejected, and the store's packages so references can be
// resolved. An error names the problem (missing document, unknown
// section, unpublished or too-high reference) without including
// document text.
func ResolveVersion(selfID string, sources []store.PackageSource, references []string, citations []store.PackageCitation, snaps []folderkb.DocSnapshot, pkgs map[string]*store.KnowledgePackage) (store.DataLevel, []store.PackageCitation, error) {
	byID := map[string]folderkb.DocSnapshot{}
	for _, s := range snaps {
		byID[s.ID] = s
	}
	level := store.LevelPublic
	for _, src := range sources {
		snap, ok := byID[src.DocID]
		if !ok {
			return 0, nil, fmt.Errorf("source document %q is not in the folder index", src.DocID)
		}
		if src.NodeID != "" {
			if findNode(snap.Tree, src.NodeID) == nil {
				return 0, nil, fmt.Errorf("source document %q has no section %q", src.DocID, src.NodeID)
			}
		}
		if snap.Level > level {
			level = snap.Level
		}
	}
	for _, ref := range references {
		if ref == selfID {
			return 0, nil, fmt.Errorf("a package cannot reference itself")
		}
		other, ok := pkgs[ref]
		if !ok {
			return 0, nil, fmt.Errorf("referenced package %q does not exist", ref)
		}
		pub := other.Published()
		if pub == nil {
			return 0, nil, fmt.Errorf("referenced package %q is not published", other.Name)
		}
		refLevel, ok := store.ParseDataLevel(pub.Level)
		if !ok {
			return 0, nil, fmt.Errorf("referenced package %q has an invalid level %q", other.Name, pub.Level)
		}
		if refLevel > level {
			// Downward-only references: the referenced package's
			// level caps this version's level only if it is at or
			// below the source level; a higher referenced level is
			// a rejection, not a silent upgrade — a low-level
			// package must never embed high-level content.
			return 0, nil, fmt.Errorf("referenced package %q is %s, above this package's source level %s", other.Name, refLevel, level)
		}
	}
	// Citations must be backed by a source of this recipe: the cited
	// document must be among the sources, and when the source is
	// node-scoped the cited section must be that section or one of
	// its descendants.
	filled := make([]store.PackageCitation, 0, len(citations))
	for _, c := range citations {
		var src *store.PackageSource
		for i := range sources {
			if sources[i].DocID == c.DocID {
				src = &sources[i]
				break
			}
		}
		if src == nil {
			return 0, nil, fmt.Errorf("citation for %q is not among the package sources", c.DocID)
		}
		snap := byID[c.DocID]
		node := findNode(snap.Tree, c.NodeID)
		if node == nil {
			return 0, nil, fmt.Errorf("citation for %q names unknown section %q", c.DocID, c.NodeID)
		}
		if src.NodeID != "" {
			scope := findNode(snap.Tree, src.NodeID)
			if scope == nil || !isDescendantOrSelf(scope, node) {
				return 0, nil, fmt.Errorf("citation section %q is outside source section %q of %q", c.NodeID, src.NodeID, c.DocID)
			}
		}
		pages := fmt.Sprintf("%d", node.StartPage)
		if node.EndPage > node.StartPage {
			pages = fmt.Sprintf("%d-%d", node.StartPage, node.EndPage)
		}
		filled = append(filled, store.PackageCitation{
			DocID: c.DocID, NodeID: c.NodeID,
			File: snap.RelPath, Chapter: node.Title, Pages: pages,
		})
	}
	return level, filled, nil
}

// CheckReferences re-validates a published version's references
// against the current store: every referenced package must still be
// published, and its level must still be at or below the version's
// own level. It returns the names of the references that would have
// to be withheld at read time (a referenced package withdrawn or
// republished higher since this package was published). Sources are
// not re-checked here — a vanished source document is reported by
// CheckSources instead.
func CheckReferences(v *store.PackageVersion, pkgs map[string]*store.KnowledgePackage) []string {
	var withheld []string
	ownLevel, _ := store.ParseDataLevel(v.Level)
	for _, ref := range v.References {
		other, ok := pkgs[ref]
		if !ok {
			withheld = append(withheld, ref)
			continue
		}
		pub := other.Published()
		if pub == nil {
			withheld = append(withheld, other.Name)
			continue
		}
		refLevel, ok := store.ParseDataLevel(pub.Level)
		if !ok || refLevel > ownLevel {
			withheld = append(withheld, other.Name)
		}
	}
	return withheld
}

// CheckSources reports the source documents of a version that are no
// longer in the folder index (the file was deleted or its folder
// unregistered). The package stays readable — its citations were
// filled in when the version was created — but the admin list flags
// it stale so the curator can revise the recipe.
func CheckSources(v *store.PackageVersion, snaps []folderkb.DocSnapshot) []string {
	byID := map[string]bool{}
	for _, s := range snaps {
		byID[s.ID] = true
	}
	var missing []string
	seen := map[string]bool{}
	for _, src := range v.Sources {
		if seen[src.DocID] || byID[src.DocID] {
			seen[src.DocID] = true
			continue
		}
		seen[src.DocID] = true
		missing = append(missing, src.DocID)
	}
	sort.Strings(missing)
	return missing
}

// PublishedSynthesis returns the published synthesis of a referenced
// package for inclusion in a kb_pack_get response: name, level and
// the synthesis text plus its citations. It returns false when the
// package does not exist, is not published, or its level is above
// maxLevel — the caller withholds the reference instead of serving
// it.
func PublishedSynthesis(id string, pkgs map[string]*store.KnowledgePackage, maxLevel store.DataLevel) (name, level, content string, citations []store.PackageCitation, ok bool) {
	p, exists := pkgs[id]
	if !exists {
		return "", "", "", nil, false
	}
	pub := p.Published()
	if pub == nil {
		return "", "", "", nil, false
	}
	lvl, good := store.ParseDataLevel(pub.Level)
	if !good || lvl > maxLevel {
		return "", "", "", nil, false
	}
	return p.Name, pub.Level, pub.Content, pub.Citations, true
}

func findNode(n *folderkb.Node, id string) *folderkb.Node {
	if n == nil {
		return nil
	}
	if n.ID == id {
		return n
	}
	for _, ch := range n.Children {
		if found := findNode(ch, id); found != nil {
			return found
		}
	}
	return nil
}

// isDescendantOrSelf reports whether node is scope itself or sits
// inside scope's subtree.
func isDescendantOrSelf(scope, node *folderkb.Node) bool {
	if scope == node {
		return true
	}
	for _, ch := range scope.Children {
		if isDescendantOrSelf(ch, node) {
			return true
		}
	}
	return false
}
