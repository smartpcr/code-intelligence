package parser

import (
	"context"
	"testing"
)

// TestParserVersion_PinnedToTreeSitter pins the exact
// `ParserVersion` constant Stage 2.1 iter 3 ships. The string
// MUST change any time a parser's output shape changes in a way
// that invalidates a `MetricSample.metric_version` cache row
// (tech-spec Sec 9.14 grammar-version drift). Iter 3's bump
// from `v1-structural-2026.05` -> `v1-tree-sitter-2026.05`
// reflects the move from a lexer-only fleet to real
// tree-sitter parsers + AstEdge emission.
func TestParserVersion_PinnedToTreeSitter(t *testing.T) {
	t.Parallel()
	const want = "v1-tree-sitter-2026.05"
	if ParserVersion != want {
		t.Fatalf("ParserVersion = %q; want %q (Stage 2.1 iter 3)", ParserVersion, want)
	}
}

// TestParserVersion_StampedOnEveryLanguage walks the registry's
// pinned languages and asserts the per-language adapter writes
// `ParserVersion` to `AstFile.parser_version` on every parse.
// Caching downstream (recipe layer -> MetricSample) keys on
// this string; an adapter that forgets to stamp it would
// silently re-use a stale cache entry.
func TestParserVersion_StampedOnEveryLanguage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		lang string
		path string
		sub  []string
	}{
		{"go", LanguageGo, "sample.go", []string{"go", "sample.go"}},
		{"python", LanguagePython, "sample.py", []string{"python", "sample.py"}},
		{"typescript", LanguageTypeScript, "sample.ts", []string{"typescript", "sample.ts"}},
		{"java", LanguageJava, "Sample.java", []string{"java", "Sample.java"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := DefaultRegistry()
			p, err := r.For(tc.lang)
			if err != nil {
				t.Fatalf("DefaultRegistry().For(%q): %v", tc.lang, err)
			}
			content := readFixture(t, tc.sub...)
			out, err := p.Parse(context.Background(), tc.path, content)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := out.GetParserVersion(); got != ParserVersion {
				t.Errorf("AstFile.parser_version = %q; want %q", got, ParserVersion)
			}
		})
	}
}
