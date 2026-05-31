// -----------------------------------------------------------------------
// <copyright file="snippet_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package suggest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

// countLines counts the number of "lines" in s, where a line is
// either terminated by '\n' or is a non-empty tail without a
// terminator. This mirrors what `wc -l` + "last unterminated
// line" semantics report.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

func TestExtractSnippet_FullRange_LF(t *testing.T) {
	p := writeFile(t, "a.txt", "line1\nline2\nline3\nline4\nline5\n")
	got, trunc, err := ExtractSnippet(p, 2, 4, 100)
	if err != nil {
		t.Fatal(err)
	}
	if trunc {
		t.Fatalf("unexpected truncated=true")
	}
	if got != "line2\nline3\nline4\n" {
		t.Fatalf("got %q", got)
	}
}

// Scenario "snippet capped" from implementation-plan Sec 4.1:
// a 500-line scope with maxLines=200 must return EXACTLY 200
// lines, truncated=true, and the last line is
// "... [truncated 300 lines]".
func TestExtractSnippet_500LineScope_Cap200(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= 500; i++ {
		fmt.Fprintf(&sb, "line%03d\n", i)
	}
	p := writeFile(t, "big.txt", sb.String())

	got, trunc, err := ExtractSnippet(p, 1, 500, 200)
	if err != nil {
		t.Fatal(err)
	}
	if !trunc {
		t.Fatalf("expected truncated=true")
	}
	if n := countLines(got); n != 200 {
		t.Fatalf("expected exactly 200 lines, got %d\n--snippet tail--\n%s", n, tail(got, 5))
	}
	lines := splitLinesPreserveLast(got)
	last := lines[len(lines)-1]
	want := "... [truncated 300 lines]"
	if last != want {
		t.Fatalf("last line = %q, want %q", last, want)
	}
	// First 199 must be the verbatim first 199 source lines.
	for i := 0; i < 199; i++ {
		expect := fmt.Sprintf("line%03d", i+1)
		if lines[i] != expect {
			t.Fatalf("line %d = %q, want %q", i+1, lines[i], expect)
		}
	}
}

// Scenario "snippet not truncated for small scope": a 50-line
// scope with maxLines=200 returns exactly 50 lines, no sentinel.
func TestExtractSnippet_50LineScope_NoCap(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&sb, "l%02d\n", i)
	}
	p := writeFile(t, "small.txt", sb.String())

	got, trunc, err := ExtractSnippet(p, 1, 50, 200)
	if err != nil {
		t.Fatal(err)
	}
	if trunc {
		t.Fatalf("unexpected truncated=true: %q", got)
	}
	if n := countLines(got); n != 50 {
		t.Fatalf("expected 50 lines, got %d", n)
	}
	if strings.Contains(got, "truncated") {
		t.Fatalf("snippet must not contain sentinel; got %q", got)
	}
}

// C12 / R4: raw bytes preserved. CRLF line endings must round-trip.
func TestExtractSnippet_PreservesCRLF(t *testing.T) {
	raw := "alpha\r\nbeta\r\ngamma\r\n"
	p := writeFile(t, "crlf.txt", raw)
	got, _, err := ExtractSnippet(p, 1, 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got != raw {
		t.Fatalf("CRLF not preserved.\n got=%q\nwant=%q", got, raw)
	}
}

// C12 / R4: tabs, multi-byte UTF-8, and odd whitespace round-trip.
func TestExtractSnippet_PreservesRawBytes(t *testing.T) {
	// Tab + multi-byte UTF-8 (per implementation-plan scenario "raw bytes").
	raw := "\t日本語 // コメント\n  spaced\t/* c */  \n\n\tindented\n"
	p := writeFile(t, "u.go", raw)
	got, _, err := ExtractSnippet(p, 1, 4, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got != raw {
		t.Fatalf("raw bytes mutated.\n got=%q\nwant=%q", got, raw)
	}
}

// File whose final line lacks a trailing newline: that final
// line must come through verbatim (no trailing '\n' added).
func TestExtractSnippet_LastLineNoTrailingNewline(t *testing.T) {
	raw := "alpha\nbeta\ngamma" // no trailing \n
	p := writeFile(t, "tail.txt", raw)
	got, trunc, err := ExtractSnippet(p, 1, 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	if trunc {
		t.Fatalf("unexpected truncated=true")
	}
	if got != raw {
		t.Fatalf("got %q want %q", got, raw)
	}
	if strings.HasSuffix(got, "\n") {
		t.Fatalf("ExtractSnippet must not synthesize a trailing newline; got %q", got)
	}
}

// File whose final line lacks a trailing newline AND truncation
// fires: the sentinel must start on its own line.
func TestExtractSnippet_TruncationWithUnterminatedTail(t *testing.T) {
	// 5-line file, last line unterminated; cap to 3 -> keep 2
	// source lines + sentinel. Suppressed = 3.
	raw := "one\ntwo\nthree\nfour\nfive"
	p := writeFile(t, "mix.txt", raw)
	got, trunc, err := ExtractSnippet(p, 1, 5, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !trunc {
		t.Fatalf("expected truncated=true")
	}
	if n := countLines(got); n != 3 {
		t.Fatalf("expected 3 lines, got %d (%q)", n, got)
	}
	lines := splitLinesPreserveLast(got)
	// 5 available, maxLines=3 -> suppressed = 5 - 3 = 2.
	if lines[0] != "one" || lines[1] != "two" || lines[2] != "... [truncated 2 lines]" {
		t.Fatalf("unexpected snippet: %q", got)
	}
}

func TestExtractSnippet_EndBeyondEOFIsNotTruncation(t *testing.T) {
	p := writeFile(t, "a.txt", "line1\nline2\nline3\nline4\nline5\n")
	got, trunc, err := ExtractSnippet(p, 4, 10, 100)
	if err != nil {
		t.Fatal(err)
	}
	if trunc {
		t.Fatalf("EOF shorter than range must not flag truncation; got %q", got)
	}
	if got != "line4\nline5\n" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractSnippet_MaxLinesOne(t *testing.T) {
	p := writeFile(t, "a.txt", "x\ny\nz\n")
	got, trunc, err := ExtractSnippet(p, 1, 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !trunc {
		t.Fatalf("expected truncated=true")
	}
	// 3 available, maxLines=1 -> suppressed = 3 - 1 = 2.
	if got != "... [truncated 2 lines]" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractSnippet_InvalidRange(t *testing.T) {
	p := writeFile(t, "a.txt", "x\n")
	cases := [][3]int{{0, 1, 10}, {1, 0, 10}, {5, 2, 10}, {-1, 1, 10}}
	for _, c := range cases {
		_, _, err := ExtractSnippet(p, c[0], c[1], c[2])
		if !errors.Is(err, ErrInvalidLineRange) {
			t.Fatalf("case %v: expected ErrInvalidLineRange, got %v", c, err)
		}
	}
}

func TestExtractSnippet_InvalidMaxLines(t *testing.T) {
	p := writeFile(t, "a.txt", "x\n")
	for _, m := range []int{0, -1, -100} {
		_, _, err := ExtractSnippet(p, 1, 1, m)
		if !errors.Is(err, ErrInvalidMaxLines) {
			t.Fatalf("maxLines=%d: expected ErrInvalidMaxLines, got %v", m, err)
		}
	}
}

func TestExtractSnippet_MissingFile(t *testing.T) {
	_, _, err := ExtractSnippet(filepath.Join(t.TempDir(), "nope.txt"), 1, 2, 10)
	if err == nil {
		t.Fatal("expected open error")
	}
}

func TestPromptFormatVersionConstant(t *testing.T) {
	if PromptFormatVersion != "v1.2026.05" {
		t.Fatalf("PromptFormatVersion drifted: %q", PromptFormatVersion)
	}
}

func TestDefaultSnippetMaxLinesConstant(t *testing.T) {
	if DefaultSnippetMaxLines != 200 {
		t.Fatalf("DefaultSnippetMaxLines must be 200 per tech-spec.md Sec 8.2, got %d", DefaultSnippetMaxLines)
	}
}

// --- helpers -------------------------------------------------------------

// splitLinesPreserveLast splits on '\n' but does not produce
// a trailing empty element when the input ends with '\n'.
// For "a\nb\n" returns ["a","b"]; for "a\nb" returns ["a","b"].
func splitLinesPreserveLast(s string) []string {
	if s == "" {
		return nil
	}
	trimmed := strings.TrimSuffix(s, "\n")
	// strip any sole \r left from CRLF on the last kept line.
	parts := strings.Split(trimmed, "\n")
	for i, p := range parts {
		parts[i] = strings.TrimSuffix(p, "\r")
	}
	return parts
}

func tail(s string, n int) string {
	parts := splitLinesPreserveLast(s)
	if len(parts) <= n {
		return strings.Join(parts, "\n")
	}
	return strings.Join(parts[len(parts)-n:], "\n")
}
