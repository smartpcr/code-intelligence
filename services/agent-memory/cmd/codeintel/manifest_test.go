package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseManifestLine covers the per-line parser:
//   - path-only entries
//   - <git-url>@<sha> entries (https + scp form + with .git suffix)
//   - blank-line skipping
//   - `#` comment skipping (with and without leading whitespace)
//   - CRLF handling
//   - scp-style URL WITHOUT a SHA suffix (the embedded `@` MUST
//     NOT be treated as the sha boundary).
func TestParseManifestLine(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantOK    bool
		wantInput string
		wantSHA   string
	}{
		{
			name:      "path-only-absolute",
			raw:       "/tmp/some/repo",
			wantOK:    true,
			wantInput: "/tmp/some/repo",
		},
		{
			name:      "path-only-windows",
			raw:       `C:\code\repo`,
			wantOK:    true,
			wantInput: `C:\code\repo`,
		},
		{
			name:      "path-only-file-url",
			raw:       "file:///tmp/repo",
			wantOK:    true,
			wantInput: "file:///tmp/repo",
		},
		{
			name:      "url-at-full-sha",
			raw:       "https://github.com/owner/repo.git@deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			wantOK:    true,
			wantInput: "https://github.com/owner/repo.git",
			wantSHA:   "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		},
		{
			name:      "url-at-short-sha",
			raw:       "https://github.com/owner/repo@abcdef1",
			wantOK:    true,
			wantInput: "https://github.com/owner/repo",
			wantSHA:   "abcdef1",
		},
		{
			name:      "scp-style-without-sha",
			raw:       "git@github.com:owner/repo.git",
			wantOK:    true,
			wantInput: "git@github.com:owner/repo.git",
			wantSHA:   "",
		},
		{
			name:      "scp-style-with-sha",
			raw:       "git@github.com:owner/repo.git@cafebabecafebabecafebabecafebabecafebabe",
			wantOK:    true,
			wantInput: "git@github.com:owner/repo.git",
			wantSHA:   "cafebabecafebabecafebabecafebabecafebabe",
		},
		{
			name:      "leading-trailing-whitespace-trimmed",
			raw:       "   /tmp/repo   ",
			wantOK:    true,
			wantInput: "/tmp/repo",
		},
		{
			name:   "blank-line-skipped",
			raw:    "",
			wantOK: false,
		},
		{
			name:   "whitespace-only-line-skipped",
			raw:    "   \t  ",
			wantOK: false,
		},
		{
			name:   "comment-line-skipped",
			raw:    "# this is a comment",
			wantOK: false,
		},
		{
			name:   "indented-comment-line-skipped",
			raw:    "   # indented comment",
			wantOK: false,
		},
		{
			name:      "crlf-line-ending-stripped",
			raw:       "/tmp/repo\r",
			wantOK:    true,
			wantInput: "/tmp/repo",
		},
		{
			name:      "non-hex-suffix-not-treated-as-sha",
			raw:       "https://github.com/owner/repo@main",
			wantOK:    true,
			wantInput: "https://github.com/owner/repo@main",
			wantSHA:   "",
		},
		{
			name:      "too-short-hex-suffix-not-sha",
			raw:       "https://github.com/owner/repo@abc",
			wantOK:    true,
			wantInput: "https://github.com/owner/repo@abc",
			wantSHA:   "",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseManifestLine(c.raw)
			if ok != c.wantOK {
				t.Fatalf("ok=%v, want %v (raw=%q parsed=%+v)", ok, c.wantOK, c.raw, got)
			}
			if !ok {
				return
			}
			if got.Input != c.wantInput {
				t.Errorf("input=%q, want %q", got.Input, c.wantInput)
			}
			if got.SHA != c.wantSHA {
				t.Errorf("sha=%q, want %q", got.SHA, c.wantSHA)
			}
		})
	}
}

// TestParseManifest covers the multi-line driver: line numbers,
// ordering, blank/comment interleaving, and that the parser
// returns the expected count of entries.
func TestParseManifest(t *testing.T) {
	manifest := `# header comment
# another comment

/tmp/local-repo
   # indented comment

https://github.com/owner/repo.git@deadbeefdeadbeefdeadbeefdeadbeefdeadbeef

git@github.com:owner/scp-repo.git
file:///tmp/file-url-repo
`
	got, err := parseManifest(strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("parseManifest: %v", err)
	}
	want := []manifestEntry{
		{Input: "/tmp/local-repo", SHA: "", Line: 4},
		{Input: "https://github.com/owner/repo.git", SHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", Line: 7},
		{Input: "git@github.com:owner/scp-repo.git", SHA: "", Line: 9},
		{Input: "file:///tmp/file-url-repo", SHA: "", Line: 10},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseManifest mismatch\ngot:  %+v\nwant: %+v", got, want)
	}
}

// TestParseManifestEmptyOrCommentsOnly confirms a manifest with
// nothing but blanks and `#` comments yields zero entries (the
// scenario `manifest-comments-skipped` from impl-plan 5.3).
func TestParseManifestEmptyOrCommentsOnly(t *testing.T) {
	manifest := `# only comments

# more comments

   # indented
`
	got, err := parseManifest(strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("parseManifest: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 entries from comment/blank-only manifest, got %d: %+v", len(got), got)
	}
}

// TestParseManifestPreservesLineNumbers verifies that the
// reported line numbers match the original 1-based stream line
// even when blanks and comments precede / interleave entries.
func TestParseManifestPreservesLineNumbers(t *testing.T) {
	manifest := "\n\n# c\n/tmp/a\n\n/tmp/b\n"
	got, err := parseManifest(strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("parseManifest: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d (%+v)", len(got), got)
	}
	if got[0].Line != 4 {
		t.Errorf("entry[0].Line = %d, want 4", got[0].Line)
	}
	if got[1].Line != 6 {
		t.Errorf("entry[1].Line = %d, want 6", got[1].Line)
	}
}
