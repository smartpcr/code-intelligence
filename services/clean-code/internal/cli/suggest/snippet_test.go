// -----------------------------------------------------------------------
// <copyright file="snippet_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package suggest

import (
	"errors"
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

const sample = "line1\nline2\nline3\nline4\nline5\n"

func TestExtractSnippet_FullRange(t *testing.T) {
	p := writeFile(t, "a.txt", sample)
	got, trunc, err := ExtractSnippet(p, 2, 4, 100)
	if err != nil {
		t.Fatal(err)
	}
	if trunc {
		t.Fatalf("unexpected truncated=true")
	}
	want := "line2\nline3\nline4\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestExtractSnippet_TruncatedByCap(t *testing.T) {
	p := writeFile(t, "a.txt", sample)
	got, trunc, err := ExtractSnippet(p, 1, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !trunc {
		t.Fatalf("expected truncated=true")
	}
	if !strings.Contains(got, "line1\nline2\n") {
		t.Fatalf("snippet missing kept lines: %q", got)
	}
	if !strings.Contains(got, "... [truncated 3 lines]") {
		t.Fatalf("snippet missing sentinel: %q", got)
	}
}

func TestExtractSnippet_EndBeyondEOFIsNotTruncation(t *testing.T) {
	p := writeFile(t, "a.txt", sample) // 5 lines
	got, trunc, err := ExtractSnippet(p, 4, 10, 100)
	if err != nil {
		t.Fatal(err)
	}
	if trunc {
		t.Fatalf("EOF shorter than range should not flag truncation; got %q", got)
	}
	if got != "line4\nline5\n" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractSnippet_PreservesRawWhitespace(t *testing.T) {
	// R4 / C12: snippet MUST come from raw disk bytes.
	raw := "  spaced\t/* comment */  \n\n\tindented\n"
	p := writeFile(t, "a.go", raw)
	got, _, err := ExtractSnippet(p, 1, 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := "  spaced\t/* comment */  \n\n\tindented\n"
	if got != want {
		t.Fatalf("snippet must preserve whitespace verbatim.\n got=%q\nwant=%q", got, want)
	}
}

func TestExtractSnippet_InvalidRange(t *testing.T) {
	p := writeFile(t, "a.txt", sample)
	cases := [][3]int{{0, 1, 10}, {1, 0, 10}, {5, 2, 10}}
	for _, c := range cases {
		_, _, err := ExtractSnippet(p, c[0], c[1], c[2])
		if !errors.Is(err, ErrInvalidLineRange) {
			t.Fatalf("case %v: expected ErrInvalidLineRange, got %v", c, err)
		}
	}
}

func TestExtractSnippet_InvalidMaxLines(t *testing.T) {
	p := writeFile(t, "a.txt", sample)
	_, _, err := ExtractSnippet(p, 1, 2, 0)
	if !errors.Is(err, ErrInvalidMaxLines) {
		t.Fatalf("expected ErrInvalidMaxLines, got %v", err)
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
