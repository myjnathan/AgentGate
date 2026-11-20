package guard

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

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
	banned  *AhoCorasick
	custom  *AhoCorasick
	regexps []struct {
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
	{`(?i)\b(?:password|passwd|pwd|密码)\s*[:=]\s*\S{4,}`, "password_assignment", CatCredential, store.LevelConfidential},
	{`\b[1-9]\d{5}(?:18|19|20)\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])\d{3}[\dXx]\b`, "cn_id_card", CatPII, store.LevelConfidential},
	{`\b1[3-9]\d{9}\b`, "cn_mobile", CatPII, store.LevelInternal},
	{`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`, "email", CatPII, store.LevelInternal},
	{`\b(?:\d[ -]?){15,18}\d\b`, "bank_card_like", CatPII, store.LevelConfidential},
}

// NewDetector builds a detector from banned and custom word lists.
func NewDetector(bannedWords, customWords []string) *Detector {
	d := &Detector{
		banned: NewAhoCorasick(bannedWords),
		custom: NewAhoCorasick(customWords),
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
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
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
	// Deduplicate identical values; drop overlapping spans (keep earliest/longest).
	seen := map[string]string{}
	var spans []Finding
	for _, f := range findings {
		if f.Category == CatBanned {
			continue // banned words are blocked upstream, never redacted through
		}
		if _, ok := seen[f.Value]; !ok {
			seen[f.Value] = fmt.Sprintf("<%s_%d>", strings.ToUpper(string(f.Category)), len(seen)+1)
		}
		spans = append(spans, f)
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].Start > spans[j].Start })
	out := text
	used := map[string]string{}
	for _, f := range spans {
		ph := seen[f.Value]
		out = out[:f.Start] + ph + out[f.End:]
		used[ph] = f.Value
	}
	return out, used
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
