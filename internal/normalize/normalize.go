// Package normalize canonicalizes text before DLP matching so that
// deformed sensitive content still matches the word lists and
// patterns: fullwidth forms, mixed case, inserted separators and
// zero-width/invisible characters.
//
// Normalization is detection-only. It never rewrites the request, the
// forwarded text or the audit record. Every finding produced from a
// normalized view is mapped back to the exact span in the original
// text, so redaction and restore keep working on the original bytes
// (including the inserted separators, which are redacted along with
// the value they were hiding).
//
// Two views exist because keywords and regexes need different
// treatment of punctuation:
//
//   - ForKeywords strips everything but letters and digits. A word
//     list entry "XH-200" must match "XH 200", "ＸＨ－２００" or
//     "X\u200bH-200" — any inserted separator is an evasion.
//   - ForPatterns keeps punctuation (after fullwidth folding) and
//     only drops whitespace and invisible characters. A regex such as
//     the API-key pattern needs its hyphen and the email pattern needs
//     "@" and "."; fullwidth "ｓｋ-…" still folds to "sk-…".
//
// The caller runs detection on both views and takes the union with the
// original-text pass, so normalization can only add findings, never
// remove one. Homophone / pinyin / split-character recognition needs a
// pronunciation dictionary and is out of scope for this package.
package normalize

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Mode selects how aggressively text is canonicalized.
type Mode int

const (
	// ForKeywords strips everything but letters and digits; see the
	// package doc.
	ForKeywords Mode = iota
	// ForPatterns keeps punctuation; see the package doc.
	ForPatterns
)

// Normalized is a normalized view of a source string plus the map back
// to source byte offsets.
type Normalized struct {
	Text string
	// Orig maps each rune index in Text to the byte offset in the
	// source of the rune it came from.
	Orig []int
	// Ends maps each rune index in Text to the source byte offset just
	// past the rune it came from. A normalized rune range [i, j) maps
	// to the source byte range [Orig[i], Ends[j-1]) — dropped
	// characters between matched runes (inserted separators) are
	// included, dropped characters outside the match are not.
	Ends []int
}

// Keywords returns the letters-and-digits-only view of text.
func Keywords(text string) Normalized {
	return Normalize(text, ForKeywords)
}

// Patterns returns the punctuation-preserving view of text.
func Patterns(text string) Normalized {
	return Normalize(text, ForPatterns)
}

// Normalize canonicalizes text under the given mode.
func Normalize(text string, mode Mode) Normalized {
	var b strings.Builder
	b.Grow(len(text))
	var orig, ends []int
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		// Fullwidth ASCII (U+FF01–U+FF5E) folds to halfwidth by a
		// fixed offset; the ideographic space folds to a plain space
		// (and is then dropped as whitespace below).
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		} else if r == 0x3000 {
			r = ' '
		}
		// Case folding (single-rune, so the 1:1 map below holds).
		r = unicode.ToLower(r)
		if isInvisible(r) {
			i += size
			continue
		}
		switch mode {
		case ForKeywords:
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				i += size
				continue
			}
		case ForPatterns:
			if unicode.IsSpace(r) {
				i += size
				continue
			}
		}
		orig = append(orig, i)
		ends = append(ends, i+size)
		b.WriteRune(r)
		i += size
	}
	return Normalized{Text: b.String(), Orig: orig, Ends: ends}
}

// isInvisible reports whether r is a zero-width or otherwise invisible
// character: the usual evasion set (zero-width space/joiner/non-joiner,
// word joiner, byte-order mark, soft hyphen) plus the Unicode format
// category Cf, which covers the directional controls and the rest.
func isInvisible(r rune) bool {
	switch r {
	case '\u200b', // zero width space
		'\u200c', // zero width non-joiner
		'\u200d', // zero width joiner
		'\u2060', // word joiner
		'\ufeff', // zero width no-break space / byte-order mark
		'\u180e', // mongolian vowel separator
		'\u00ad': // soft hyphen
		return true
	}
	return unicode.Is(unicode.Cf, r)
}

// MapRuneRange maps a rune range [start, end) in the normalized text
// back to a byte range in the source. Out-of-range inputs are clamped;
// an empty range maps to a zero-width point at the corresponding
// source offset.
func (n Normalized) MapRuneRange(start, end int) (int, int) {
	total := len(n.Orig)
	if total == 0 {
		return 0, 0
	}
	if start < 0 {
		start = 0
	}
	if end > total {
		end = total
	}
	if start > end {
		start = end
	}
	if start == end {
		if start >= total {
			return n.Ends[total-1], n.Ends[total-1]
		}
		return n.Orig[start], n.Orig[start]
	}
	return n.Orig[start], n.Ends[end-1]
}

// MapByteRange maps a byte range [start, end) in the normalized text
// back to a byte range in the source. Offsets landing inside a rune
// are rounded outward to that rune.
func (n Normalized) MapByteRange(start, end int) (int, int) {
	if start < 0 {
		start = 0
	}
	if end > len(n.Text) {
		end = len(n.Text)
	}
	if start > end {
		start = end
	}
	if len(n.Text) == 0 {
		return 0, 0
	}
	// Rune index of the rune starting at or containing start, and of
	// the last rune starting before end.
	rs, re := 0, 0
	ri := 0
	for off := 0; off < len(n.Text); {
		_, size := utf8.DecodeRuneInString(n.Text[off:])
		if off <= start {
			rs = ri
		}
		if off < end {
			re = ri + 1
		}
		off += size
		ri++
	}
	return n.MapRuneRange(rs, re)
}
