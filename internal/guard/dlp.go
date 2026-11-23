package guard

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/myjnathan/agentgate/internal/normalize"
	"github.com/myjnathan/agentgate/internal/store"
)

// Category of a detection.
type Category string

const (
	CatCredential Category = "credential" // API keys, passwords, tokens
	CatPII        Category = "pii"        // ID card, phone, email, bank card
	CatCustom     Category = "custom"     // enterprise-defined words (project codes, client names)
	CatBanned     Category = "banned"     // sensitive/banned words: always blocked
)

// Finding is one detected sensitive span.
type Finding struct {
	Category Category `json:"category"`
	Kind     string   `json:"kind"` // e.g. cn_id_card, email, api_key, custom_word
	Value    string   `json:"-"`    // the matched text (never logged in plaintext)
	Start    int      `json:"start"`
	End      int      `json:"end"`
	// Level is the data level this finding implies.
	Level store.DataLevel `json:"level"`
}

// Detector finds sensitive spans in text.
type Detector struct {
	banned     *AhoCorasick
	custom     *AhoCorasick
	bannedNorm *AhoCorasick // banned words, normalized for keyword matching (F29)
	customNorm *AhoCorasick // custom words, normalized for keyword matching (F29)
	regexps    []struct {
		re    *regexp.Regexp
		kind  string
		cat   Category
		level store.DataLevel
	}
}

var builtinPatterns = []struct {
	pat   string
	kind  string
	cat   Category
	level store.DataLevel
}{
	{`\b(?:sk|rk|pk)-[A-Za-z0-9_\-]{16,}\b`, "api_key", CatCredential, store.LevelConfidential},
	{`\bAKIA[0-9A-Z]{16}\b`, "aws_access_key", CatCredential, store.LevelConfidential},
	{`\bgh[pousr]_[A-Za-z0-9]{30,}\b`, "github_token", CatCredential, store.LevelConfidential},
	// NB: \b is ASCII-only in Go regexp and CJK characters are
	// non-word characters, so `\b密码` could never match at a string
	// start or after CJK text. The boundary is therefore required
	// only for the ASCII alternatives (F29 fix).
	{`(?i)(?:\b(?:password|passwd|pwd)|密码)\s*[:=]\s*\S{4,}`, "password_assignment", CatCredential, store.LevelConfidential},
	{`\b[1-9]\d{5}(?:18|19|20)\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])\d{3}[\dXx]\b`, "cn_id_card", CatPII, store.LevelConfidential},
	{`\b1[3-9]\d{9}\b`, "cn_mobile", CatPII, store.LevelInternal},
	{`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`, "email", CatPII, store.LevelInternal},
	{`\b(?:\d[ -]?){15,18}\d\b`, "bank_card_like", CatPII, store.LevelConfidential},
}

// NewDetector builds a detector from banned and custom word lists.
func NewDetector(bannedWords, customWords []string) *Detector {
	d := &Detector{
		banned:     NewAhoCorasick(bannedWords),
		custom:     NewAhoCorasick(customWords),
		bannedNorm: NewAhoCorasick(normalizeWords(bannedWords)),
		customNorm: NewAhoCorasick(normalizeWords(customWords)),
	}
	for _, bp := range builtinPatterns {
		d.regexps = append(d.regexps, struct {
			re    *regexp.Regexp
			kind  string
			cat   Category
			level store.DataLevel
		}{regexp.MustCompile(bp.pat), bp.kind, bp.cat, bp.level})
	}
	return d
}

// Scan returns all findings in text, sorted by position.
//
// The original text is scanned first, exactly as before; then the two
// normalized views (F29) are scanned and any extra findings are mapped
// back to the original spans and merged in. Normalization can only add
// findings, never remove one: a hit the original pass already found is
// kept as-is, and a normalized hit overlapping it is dropped.
func (d *Detector) Scan(text string) []Finding {
	var out []Finding
	for _, w := range d.banned.Find(text) {
		if i := strings.Index(text, w); i >= 0 {
			out = append(out, Finding{Category: CatBanned, Kind: "banned_word", Value: w, Start: i, End: i + len(w), Level: store.LevelSecret})
		}
	}
	for _, w := range d.custom.Find(text) {
		idx := 0
		for {
			i := strings.Index(text[idx:], w)
			if i < 0 {
				break
			}
			s := idx + i
			out = append(out, Finding{Category: CatCustom, Kind: "custom_word", Value: w, Start: s, End: s + len(w), Level: store.LevelConfidential})
			idx = s + len(w)
		}
	}
	for _, rp := range d.regexps {
		for _, loc := range rp.re.FindAllStringIndex(text, -1) {
			out = append(out, Finding{Category: rp.cat, Kind: rp.kind, Value: text[loc[0]:loc[1]], Start: loc[0], End: loc[1], Level: rp.level})
		}
	}
	out = append(out, d.scanNormalized(text, out)...)
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// normalizeWords folds a word list the same way the keyword view folds
// text, so list entries and deformed input meet in the middle.
// Entries that fold to nothing (e.g. a word made only of separators)
// can never match and are dropped.
func normalizeWords(words []string) []string {
	var out []string
	for _, w := range words {
		if n := normalize.Keywords(w).Text; n != "" {
			out = append(out, n)
		}
	}
	return out
}

// scanNormalized re-runs detection on the normalized views of text and
// returns the extra findings, mapped back to spans in the original
// text. Word lists run on the keyword view (separators stripped);
// regexes run on both views — the keyword view catches values split by
// separators ("138-0000-0000"), the pattern view catches values whose
// punctuation is part of the pattern ("ｓｋ-…" folds to "sk-…",
// "ａ＠ｂ．ｃｏｍ" to "a@b.com"). Value is always the original span,
// so redaction replaces (and restore returns) exactly what the user
// typed, separators included.
func (d *Detector) scanNormalized(text string, existing []Finding) []Finding {
	var extra []Finding
	// add keeps a candidate only when it overlaps no finding already
	// collected: the same hit in the original text, or an earlier
	// normalized hit covering the same span. Overlap would corrupt
	// Redact (it replaces spans right-to-left) and double-count.
	add := func(f Finding) {
		for _, e := range existing {
			if f.Start < e.End && e.Start < f.End {
				return
			}
		}
		for _, e := range extra {
			if f.Start < e.End && e.Start < f.End {
				return
			}
		}
		extra = append(extra, f)
	}
	wordHits := func(view normalize.Normalized, ac *AhoCorasick, cat Category, kind string, lvl store.DataLevel) {
		for _, w := range ac.Find(view.Text) {
			for _, span := range allOccurrences(view.Text, w) {
				s, e := view.MapByteRange(span[0], span[1])
				add(Finding{Category: cat, Kind: kind, Value: text[s:e], Start: s, End: e, Level: lvl})
			}
		}
	}
	regexHits := func(view normalize.Normalized) {
		for _, rp := range d.regexps {
			for _, loc := range rp.re.FindAllStringIndex(view.Text, -1) {
				s, e := view.MapByteRange(loc[0], loc[1])
				add(Finding{Category: rp.cat, Kind: rp.kind, Value: text[s:e], Start: s, End: e, Level: rp.level})
			}
		}
	}
	if kw := normalize.Keywords(text); kw.Text != "" {
		wordHits(kw, d.bannedNorm, CatBanned, "banned_word", store.LevelSecret)
		wordHits(kw, d.customNorm, CatCustom, "custom_word", store.LevelConfidential)
		regexHits(kw)
	}
	if pt := normalize.Patterns(text); pt.Text != "" {
		regexHits(pt)
	}
	return extra
}

// allOccurrences returns every [start, end) byte span of word in text.
func allOccurrences(text, word string) [][2]int {
	var spans [][2]int
	idx := 0
	for {
		i := strings.Index(text[idx:], word)
		if i < 0 {
			break
		}
		s := idx + i
		spans = append(spans, [2]int{s, s + len(word)})
		idx = s + len(word)
	}
	return spans
}

// Classify returns the highest data level implied by findings.
func Classify(findings []Finding) store.DataLevel {
	lvl := store.LevelPublic
	for _, f := range findings {
		if f.Level > lvl {
			lvl = f.Level
		}
	}
	return lvl
}

// Redact replaces each finding with a reversible placeholder and returns
// the redacted text plus the mapping (placeholder -> original). The
// mapping must never leave the trust boundary that created it
// (endpoint sidecar or the enterprise gateway).
func Redact(text string, findings []Finding) (string, map[string]string) {
	r := NewRedactor()
	out := r.Redact(text, findings)
	return out, r.Mapping()
}

// Redactor keeps placeholder numbering stable across many texts of one
// request, so the same value always maps to the same placeholder —
// whether it appears in message content or in a tool_calls argument.
// Numbering per Redact call would collide when mappings are merged
// (each call restarts at _1), so a request is redacted through a single
// Redactor.
type Redactor struct {
	seen map[string]string // original value -> placeholder
	used map[string]string // placeholder -> original value
}

// NewRedactor returns an empty Redactor.
func NewRedactor() *Redactor {
	return &Redactor{seen: map[string]string{}, used: map[string]string{}}
}

// Redact replaces findings in text with placeholders, reusing the
// placeholder of any value seen before in this Redactor.
func (r *Redactor) Redact(text string, findings []Finding) string {
	// Drop overlapping spans (keep earliest/longest); banned words are
	// blocked upstream, never redacted through.
	var spans []Finding
	for _, f := range findings {
		if f.Category == CatBanned {
			continue
		}
		if _, ok := r.seen[f.Value]; !ok {
			r.seen[f.Value] = fmt.Sprintf("<%s_%d>", strings.ToUpper(string(f.Category)), len(r.seen)+1)
		}
		spans = append(spans, f)
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].Start > spans[j].Start })
	out := text
	for _, f := range spans {
		ph := r.seen[f.Value]
		out = out[:f.Start] + ph + out[f.End:]
		r.used[ph] = f.Value
	}
	return out
}

// Mapping returns the placeholder -> original map accumulated so far.
func (r *Redactor) Mapping() map[string]string {
	out := make(map[string]string, len(r.used))
	for k, v := range r.used {
		out[k] = v
	}
	return out
}

// MaxPlaceholderLength returns the longest byte length a placeholder
// from mapping can occupy on the wire, counting the JSON-escaped form
// (\u003c…\u003e) upstream encoders produce. Streaming restore must
// hold back at least this many trailing bytes so a placeholder split
// across SSE chunks is never emitted half-restored.
func MaxPlaceholderLength(mapping map[string]string) int {
	n := 0
	for k := range mapping {
		if len(k) > n {
			n = len(k)
		}
		esc := strings.NewReplacer("<", `\u003c`, ">", `\u003e`, "&", `\u0026`).Replace(k)
		if len(esc) > n {
			n = len(esc)
		}
	}
	return n
}

// Restore reverses Redact on a model response.
func Restore(text string, mapping map[string]string) string {
	// Longest placeholders first to avoid partial replacement.
	keys := make([]string, 0, len(mapping))
	for k := range mapping {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		text = strings.ReplaceAll(text, k, mapping[k])
		// Upstream JSON encoders may HTML-escape < and > in the
		// placeholder (\u003cPII_1\u003e); restore that form too.
		esc := strings.NewReplacer("<", `\u003c`, ">", `\u003e`, "&", `\u0026`).Replace(k)
		if esc != k {
			text = strings.ReplaceAll(text, esc, mapping[k])
		}
	}
	return text
}
