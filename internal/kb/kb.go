// Package kb implements the knowledge base: multi-round keyword search
// over original documents with citations — deliberately no vector DB
// (the approach dsh-ee validated; it keeps deployment light and answers
// verifiable). Access is level-checked: a caller only sees documents at
// or below the level their tier may access.
package kb

import (
	"sort"
	"strings"

	"github.com/myjnathan/agentgate/internal/store"
)

type Hit struct {
	DocID   string `json:"doc_id"`
	Title   string `json:"title"`
	Version int    `json:"version"`
	Level   string `json:"level"`
	Score   int    `json:"score"`
	Excerpt string `json:"excerpt"`
}

// MaxLevelForTier maps an agent tier to the highest KB level it may read.
func MaxLevelForTier(t store.Tier) store.DataLevel {
	if t == store.TierManaged {
		return store.LevelConfidential
	}
	return store.LevelInternal
}

// Search scores documents by keyword frequency across title and body.
// Multi-round: the caller (agent) is expected to issue follow-up
// searches with refined keywords and then read the originals.
func Search(st *store.State, query string, maxLevel store.DataLevel, limit int) []Hit {
	terms := splitTerms(query)
	if len(terms) == 0 || limit <= 0 {
		return nil
	}
	var hits []Hit
	for _, doc := range st.KBDocs {
		if doc.Level > maxLevel {
			continue
		}
		score := 0
		lowerTitle := strings.ToLower(doc.Title)
		lowerBody := strings.ToLower(doc.Content)
		for _, t := range terms {
			score += 3 * strings.Count(lowerTitle, t)
			score += strings.Count(lowerBody, t)
		}
		if score == 0 {
			continue
		}
		hits = append(hits, Hit{
			DocID: doc.ID, Title: doc.Title, Version: doc.Version,
			Level: doc.Level.String(), Score: score,
			Excerpt: excerpt(doc.Content, terms),
		})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// Get returns a full document if the caller's level permits.
func Get(st *store.State, id string, maxLevel store.DataLevel) (*store.KBDoc, bool) {
	doc, ok := st.KBDocs[id]
	if !ok || doc.Level > maxLevel {
		return nil, false
	}
	cp := *doc
	return &cp, true
}

func splitTerms(q string) []string {
	fields := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return r == ' ' || r == ',' || r == '，' || r == '、' || r == '\t' || r == '\n'
	})
	var out []string
	for _, f := range fields {
		if len(f) >= 2 { // skip single-char noise; CJK bigrams still pass at len 2 bytes? no—runes
			out = append(out, f)
		}
	}
	// Also keep the whole query as a phrase term for exact matches.
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
	// Convert byte index to rune index approximately by counting runes before it.
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
