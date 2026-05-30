package walk

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

// TestPrepareGitignoreLine_PreservesEscapedTrailingSpace
// pins the fix for the gitignore-spec edge case where a
// pattern ends in an escaped trailing space (`foo\ `). Git
// treats that as a literal trailing-space match; a naive
// `strings.TrimRight(line, " \t\r")` corrupts the pattern
// into the malformed `foo\` that matches nothing. The
// walker's [prepareGitignoreLine] strips ONLY BOM and CR,
// deferring all trailing-space semantics to
// `gitignore.ParsePattern` which already honours the escape.
//
// This is a pure-function test of [prepareGitignoreLine] so
// every assertion is cross-platform; the assertions are
// byte-exact on what the helper hands to ParsePattern.
func TestPrepareGitignoreLine_PreservesEscapedTrailingSpace(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		raw      string
		wantLine string
		wantKeep bool
	}{
		{
			name:     "escaped_trailing_space_preserved",
			raw:      `foo\ `, // f, o, o, \, SPACE
			wantLine: `foo\ `,
			wantKeep: true,
		},
		{
			name:     "escaped_trailing_space_with_cr_preserved",
			raw:      "qux\\ \r", // q, u, x, \, SPACE, CR -- CR must be stripped, escape preserved
			wantLine: `qux\ `,
			wantKeep: true,
		},
		{
			name:     "unescaped_trailing_spaces_preserved_for_gogit",
			raw:      "bar  ", // b, a, r, SPACE, SPACE -- we DO NOT trim; go-git will
			wantLine: "bar  ",
			wantKeep: true,
		},
		{
			name:     "blank_line_skipped",
			raw:      "",
			wantLine: "",
			wantKeep: false,
		},
		{
			name:     "pure_space_line_skipped",
			raw:      "   ",
			wantLine: "",
			wantKeep: false,
		},
		{
			name:     "cr_only_line_skipped",
			raw:      "\r",
			wantLine: "",
			wantKeep: false,
		},
		{
			name:     "comment_skipped",
			raw:      "# this is a comment",
			wantLine: "",
			wantKeep: false,
		},
		{
			name:     "leading_whitespace_disqualifies_comment",
			raw:      "   # not a comment",
			wantLine: "   # not a comment",
			wantKeep: true,
		},
		{
			name:     "bom_stripped_from_first_line",
			raw:      "\uFEFFsecret.go",
			wantLine: "secret.go",
			wantKeep: true,
		},
		{
			name:     "ordinary_pattern_passthrough",
			raw:      "secret.go",
			wantLine: "secret.go",
			wantKeep: true,
		},
		{
			name:     "crlf_strip_on_ordinary_pattern",
			raw:      "secret.go\r",
			wantLine: "secret.go",
			wantKeep: true,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotLine, gotKeep := prepareGitignoreLine(tc.raw)
			if gotKeep != tc.wantKeep {
				t.Errorf("keep = %v; want %v (line = %q)", gotKeep, tc.wantKeep, gotLine)
			}
			if gotLine != tc.wantLine {
				t.Errorf("line = %q; want %q", gotLine, tc.wantLine)
			}
		})
	}
}

// TestParseGitignoreFile_EscapedTrailingSpaceEndToEnd
// asserts the integrated parser (file -> patterns ->
// matcher) honours the escaped trailing-space contract.
//
// `filepath.Match` (which go-git uses internally) disables
// backslash escaping on Windows ("On Windows, escaping is
// disabled. Instead, '\\' is treated as path separator." --
// pkg.go.dev/path/filepath#Match), so the end-to-end
// match-via-go-git half of the test only runs on POSIX. The
// PURE LINE-PREP fix is verified cross-platform by
// [TestPrepareGitignoreLine_PreservesEscapedTrailingSpace]
// above.
func TestParseGitignoreFile_EscapedTrailingSpaceEndToEnd(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("filepath.Match disables backslash-escape on Windows; the line-prep half of the fix is covered by TestPrepareGitignoreLine_PreservesEscapedTrailingSpace")
	}
	dir := t.TempDir()
	// Two patterns:
	//   `foo\ ` -- escaped trailing space, must match `foo `.
	//   `bar  ` -- unescaped trailing spaces, must match `bar`.
	content := "foo\\ \nbar  \n"
	ignorePath := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(ignorePath, []byte(content), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	patterns := parseGitignoreFile(ignorePath, nil)
	if len(patterns) != 2 {
		t.Fatalf("got %d patterns, want 2", len(patterns))
	}
	// `foo\ ` MUST match `"foo "` (literal trailing space).
	if got := patterns[0].Match([]string{"foo "}, false); got != gitignore.Exclude {
		t.Errorf(`pattern[0] (foo\ ).Match(["foo "]) = %v; want Exclude`, got)
	}
	// `foo\ ` MUST NOT match `"foo"` (no trailing space).
	if got := patterns[0].Match([]string{"foo"}, false); got == gitignore.Exclude {
		t.Errorf(`pattern[0] (foo\ ).Match(["foo"]) = Exclude; want NoMatch`)
	}
	// `bar  ` (trailing unescaped spaces) MUST match `"bar"`.
	if got := patterns[1].Match([]string{"bar"}, false); got != gitignore.Exclude {
		t.Errorf(`pattern[1] (bar  ).Match(["bar"]) = %v; want Exclude`, got)
	}
}

// TestParseGitignoreFile_MissingFile asserts a non-existent
// .gitignore returns an empty slice rather than crashing.
func TestParseGitignoreFile_MissingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	patterns := parseGitignoreFile(filepath.Join(dir, "does-not-exist"), nil)
	if patterns != nil {
		t.Fatalf("want nil, got %v", patterns)
	}
}


