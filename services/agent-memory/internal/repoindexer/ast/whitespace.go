package ast

import (
	"strings"
	"unicode"
)

// NormalizeSignature returns the canonical-signature form of s.
//
// The normaliser is the §9.7 / §9.9 risk mitigation: every
// canonical-signature input the Stage 3.2 dispatcher feeds into
// `fingerprint.NodeFingerprint` is run through this function
// first, so a formatter-only commit (different indentation,
// inserted spaces around `,` or `()`, swapped tabs<->spaces,
// added `//` line comments) produces a byte-identical
// fingerprint and therefore a stable Node identity.
//
// Steps, in order:
//
//  1. Strip line comments (`//...`, `#...` to end of line) and
//     block comments (`/* ... */`). Strings are NOT scanned
//     specially -- canonical signatures the dispatcher passes
//     in are name+parameter tokens, not arbitrary source.
//  2. Collapse runs of any Unicode whitespace (`unicode.IsSpace`,
//     which covers `\t`, `\n`, `\r`, NBSP, etc.) to a single
//     ASCII space.
//  3. Remove the single space when it sits directly adjacent to
//     one of the canonical punctuation marks
//     (`,`, `(`, `)`, `[`, `]`, `{`, `}`, `<`, `>`, `:`, `;`).
//     This is the step that collapses `Map<K, V>` /
//     `Map < K , V >` / `Map<K,V>` to the same normalised
//     string.
//  4. Trim leading / trailing whitespace.
//
// The normaliser is deterministic and pure -- the same input
// always produces the same output. It is intentionally NOT a
// full lexer; it deliberately accepts inputs that are already
// approximately-normalised (e.g. parser-extracted parameter
// lists) and turns them into a strictly-normalised form. Feed
// it raw method bodies and the comment-stripping is still
// useful but the output is a single long line that is not a
// faithful canonical signature -- callers must extract the
// name+param tokens themselves and only run this normaliser
// over those tokens.
func NormalizeSignature(s string) string {
	if s == "" {
		return ""
	}
	stripped := stripComments(s)
	collapsed := collapseWhitespace(stripped)
	pruned := stripWhitespaceAroundPunctuation(collapsed)
	return strings.TrimSpace(pruned)
}

// canonicalPunctuation is the closed set of ASCII punctuation
// marks whose adjacency to whitespace is collapsed. The set
// covers the standard signature delimiters across the v1
// language set (TypeScript / JavaScript / Python) plus the
// generic type-parameter brackets (`<`, `>`) and namespace
// separators (`:`). Extending this set requires opening a new
// story to revisit canonical-signature stability -- adding a
// new mark changes every existing fingerprint.
const canonicalPunctuation = ",()[]{}<>:;"

// stripComments removes `//...\n`, `#...\n`, and `/* ... */`
// runs from s. The walker is byte-oriented (not rune-oriented)
// because every recognised opener is ASCII; multibyte runes
// inside comment bodies are preserved verbatim until the
// comment terminator. String-literal awareness is intentionally
// out of scope -- callers must not feed raw source through this
// function (see NormalizeSignature's doc comment).
func stripComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		// Block comment: scan until "*/".
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '*' {
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				// Unterminated /* -- drop the rest.
				return b.String()
			}
			// Replace the entire run with a single space so
			// adjacent tokens stay separated.
			b.WriteByte(' ')
			i += 2 + end + 2
			continue
		}
		// Line comments: `//` (TS/JS) and `#` (Python).
		if (i+1 < len(s) && s[i] == '/' && s[i+1] == '/') || s[i] == '#' {
			nl := strings.IndexByte(s[i:], '\n')
			if nl < 0 {
				return b.String()
			}
			// Preserve the newline so line counts elsewhere
			// (e.g. block subdivision) remain accurate.
			b.WriteByte('\n')
			i += nl + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// collapseWhitespace replaces every run of Unicode whitespace
// with a single ASCII space. Operates on runes so multibyte
// whitespace (NBSP `\u00A0`, ideographic space `\u3000`, etc.)
// is collapsed correctly.
func collapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

// stripWhitespaceAroundPunctuation removes single ASCII spaces
// that sit directly before or after one of the
// canonicalPunctuation marks. Assumes its input has already
// passed through collapseWhitespace, i.e. there is at most one
// space between any two non-space runes.
func stripWhitespaceAroundPunctuation(s string) string {
	if s == "" {
		return ""
	}
	bs := []byte(s)
	var b strings.Builder
	b.Grow(len(bs))
	for i := 0; i < len(bs); i++ {
		c := bs[i]
		// Drop a space that precedes a punctuation mark.
		if c == ' ' && i+1 < len(bs) &&
			strings.IndexByte(canonicalPunctuation, bs[i+1]) >= 0 {
			continue
		}
		// Drop a space that follows a punctuation mark.
		if c == ' ' && i > 0 &&
			strings.IndexByte(canonicalPunctuation, bs[i-1]) >= 0 {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// CountLogicalLines returns the number of normalised logical
// lines in body. A logical line is a line that, after comment
// stripping and whitespace trimming, contains at least one
// non-whitespace byte. Blank lines and comment-only lines do
// not count.
//
// This is the input to `SubdivideMethod`'s threshold check
// (§8.2 default: 80). The counter operates after the comment
// strip so a formatter-only commit (added blank lines, added
// `//` comments) does NOT change the count and therefore does
// NOT churn the Block subdivision.
func CountLogicalLines(body string) int {
	if body == "" {
		return 0
	}
	stripped := stripComments(body)
	count := 0
	for _, line := range strings.Split(stripped, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}
