// -----------------------------------------------------------------------
// <copyright file="snippet.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package suggest

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrInvalidLineRange is returned by ExtractSnippet when the
// caller supplies a non-positive startLine, a non-positive
// endLine, or an endLine that precedes startLine.
var ErrInvalidLineRange = errors.New("suggest: invalid line range")

// ErrInvalidMaxLines is returned by ExtractSnippet when the
// caller supplies a non-positive maxLines cap.
var ErrInvalidMaxLines = errors.New("suggest: maxLines must be positive")

// ExtractSnippet reads the raw bytes of absFilePath from disk
// and returns the lines in the inclusive 1-based range
// [startLine, endLine]. The returned snippet preserves the
// developer's original whitespace, comments, and formatting --
// it is NOT taken from the parser's normalised in-memory
// representation. This is the C12 / R4 mitigation: the AI coder
// downstream must see the file as it actually lives on disk so
// that any patch it synthesises lines up against real bytes.
//
// When the requested range spans more than maxLines, the
// snippet is truncated to the first maxLines of the range and
// the final retained line is followed by a sentinel of the form
//
//	... [truncated N lines]
//
// where N is the count of suppressed lines. In that case the
// returned truncated flag is true. When the requested range
// fits inside the cap, truncated is false and the snippet
// contains exactly (endLine - startLine + 1) lines (or fewer
// when the file ends before endLine).
//
// If endLine exceeds the file length, the snippet ends at the
// file's last line and truncated reflects only whether the
// (clamped) range itself exceeded maxLines; running off the end
// of the file is not on its own treated as truncation, because
// the caller asked for whatever lives at those line numbers and
// the file simply ended sooner.
//
// Trailing newlines from intermediate lines are preserved; the
// returned string never has a leading newline. The sentinel,
// when emitted, sits on its own line at the end of the string
// (with a trailing newline) so that pretty-printers downstream
// render it cleanly.
//
// Errors:
//   - ErrInvalidLineRange when startLine <= 0, endLine <= 0,
//     or endLine < startLine.
//   - ErrInvalidMaxLines when maxLines <= 0.
//   - The underlying I/O error (wrapped) when the file cannot
//     be opened or read.
func ExtractSnippet(absFilePath string, startLine, endLine int, maxLines int) (snippet string, truncated bool, err error) {
	if startLine <= 0 || endLine <= 0 || endLine < startLine {
		return "", false, fmt.Errorf("%w: startLine=%d endLine=%d", ErrInvalidLineRange, startLine, endLine)
	}
	if maxLines <= 0 {
		return "", false, fmt.Errorf("%w: maxLines=%d", ErrInvalidMaxLines, maxLines)
	}

	f, err := os.Open(absFilePath)
	if err != nil {
		return "", false, fmt.Errorf("suggest: open %s: %w", absFilePath, err)
	}
	defer f.Close()

	// The requested range may span more than maxLines. We keep
	// exactly maxLines lines and count how many additional lines
	// in the requested range were suppressed so the sentinel can
	// report the suppressed-line count.
	requested := endLine - startLine + 1
	keepCap := requested
	if keepCap > maxLines {
		keepCap = maxLines
	}

	var b strings.Builder
	scanner := bufio.NewScanner(f)
	// Allow long single lines (default bufio limit is 64 KiB).
	// Source files routinely contain generated lines longer than
	// the default; cap at 1 MiB which matches the walker's
	// per-file size guidance.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lineNo := 0
	kept := 0
	lastLineInFile := 0
	for scanner.Scan() {
		lineNo++
		lastLineInFile = lineNo
		if lineNo < startLine {
			continue
		}
		if lineNo > endLine {
			break
		}
		if kept >= keepCap {
			// Keep advancing through the requested range so the
			// suppressed-line count is accurate even if the file
			// ends mid-range.
			continue
		}
		b.WriteString(scanner.Text())
		b.WriteByte('\n')
		kept++
	}
	if err := scanner.Err(); err != nil {
		return "", false, fmt.Errorf("suggest: read %s: %w", absFilePath, err)
	}

	// Determine truncation. We only consider the *requested*
	// range -- running off the end of the file shortens the
	// snippet but is not on its own truncation, because the
	// caller asked for lines that don't exist.
	effectiveEnd := endLine
	if lastLineInFile < effectiveEnd {
		effectiveEnd = lastLineInFile
	}
	available := effectiveEnd - startLine + 1
	if available < 0 {
		available = 0
	}
	if available > kept {
		suppressed := available - kept
		fmt.Fprintf(&b, "... [truncated %d lines]\n", suppressed)
		truncated = true
	}

	return b.String(), truncated, nil
}
