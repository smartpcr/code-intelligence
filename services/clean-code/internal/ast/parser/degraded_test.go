//go:build cgo

package parser

import (
	"context"
	"testing"
)

// TestTreeSitter_DegradedReasonOnParseError addresses iter-2
// evaluator finding #5: tree-sitter adapters MUST inspect
// `root.HasError()` and stamp `AstFile.degraded_reason` when
// the CST contains ERROR / MISSING nodes. We feed each
// per-language adapter intentionally-broken source and assert
// the adapter (a) still returns a non-nil `AstFile` with a
// file scope (graceful degradation) and (b) sets
// `degraded_reason="tree_sitter_parse_error"`.
func TestTreeSitter_DegradedReasonOnParseError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		p    Parser
		path string
		// Source intentionally crafted to produce
		// tree-sitter ERROR nodes (unmatched braces, missing
		// closers, dangling tokens).
		content []byte
	}{
		{
			name:    "go",
			p:       &goParser{},
			path:    "broken.go",
			content: []byte("package broken\n\nfunc Foo( {\n  return\n"),
		},
		{
			name:    "python",
			p:       &pythonParser{},
			path:    "broken.py",
			content: []byte("def foo(:\n    return 1\nclass Bar(\n"),
		},
		{
			name:    "typescript",
			p:       &tsParser{},
			path:    "broken.ts",
			content: []byte("class Foo {\n  bar(x:): void {\n    return\n  "),
		},
		{
			name:    "java",
			p:       &javaParser{},
			path:    "Broken.java",
			content: []byte("public class Broken {\n  public void foo( {\n"),
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := tc.p.Parse(context.Background(), tc.path, tc.content)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if out == nil {
				t.Fatal("Parse returned nil AstFile on broken input; want degraded AstFile")
			}
			if len(out.GetScopes()) == 0 {
				t.Fatal("AstFile.scopes empty; want at least the file scope even on a degraded parse")
			}
			if got := out.GetDegradedReason(); got != "tree_sitter_parse_error" {
				t.Errorf("AstFile.degraded_reason = %q; want %q (tree-sitter ERROR nodes present)", got, "tree_sitter_parse_error")
			}
		})
	}
}

// TestTreeSitter_NoDegradedReasonOnCleanParse asserts the
// adapters do NOT spuriously stamp `degraded_reason` when the
// source parses cleanly. The empty string is the success
// signal downstream consumers (recipe layer, MetricSample
// emitter) rely on.
func TestTreeSitter_NoDegradedReasonOnCleanParse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		p    Parser
		path string
		sub  []string
	}{
		{"go", &goParser{}, "sample.go", []string{"go", "sample.go"}},
		{"python", &pythonParser{}, "sample.py", []string{"python", "sample.py"}},
		{"typescript", &tsParser{}, "sample.ts", []string{"typescript", "sample.ts"}},
		{"java", &javaParser{}, "Sample.java", []string{"java", "Sample.java"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			content := readFixture(t, tc.sub...)
			out, err := tc.p.Parse(context.Background(), tc.path, content)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := out.GetDegradedReason(); got != "" {
				t.Errorf("AstFile.degraded_reason = %q on a clean fixture; want empty", got)
			}
		})
	}
}
