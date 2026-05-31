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
	"io"
	"os"
	"strings"
)

// DefaultSnippetMaxLines is the canonical default cap for the
// suggest emitter's source-snippet extraction (tech-spec.md
// Sec 8.2, architecture.md Sec 4.6 "capped at a configurable
// max (default 200 lines)"). The downstream emitter (Stage 4.2
// JSONL writer) MUST pass this value to ExtractSnippet unless
// the operator explicitly overrides via the (currently
// reserved) `--snippet-cap-lines` flag.
const DefaultSnippetMaxLines = 200

// ErrInvalidLineRange is returned by ExtractSnippet when the
// caller supplies a non-positive startLine, a non-positive
// endLine, or an endLine that precedes startLine.
var ErrInvalidLineRange = errors.New("suggest: invalid line range")

// ErrInvalidMaxLines is returned by ExtractSnippet when the
// caller supplies a non-positive maxLines cap.
var ErrInvalidMaxLines = errors.New("suggest: maxLines must be positive")

// truncationSentinelPrefix is the literal prefix of the
// truncation sentinel; the suppressed-line count is appended
// per call. Exported only so downstream tests can match it
// without re-typing the literal.
const truncationSentinelPrefix = "... [truncated "

// ExtractSnippet reads the raw bytes of absFilePath from disk
// and returns the lines in the inclusive 1-based range
// [startLine, endLine].
//
// Raw-byte guarantee (constraint C12 / risk R4 mitigation):
// the returned snippet is assembled from the exact bytes on
// disk, NOT from any parser-normalised in-memory form. Line
// terminators are preserved verbatim, so a CRLF file produces
// a CRLF-terminated snippet, an LF file produces an
// LF-terminated snippet, and a file whose final line has no
// terminator produces a snippet whose final source line also
// has no terminator. Whitespace, tabs, comments, BOMs, and
// multi-byte UTF-8 sequences round-trip byte-for-byte.
//
// Truncation contract (implementation-plan.md Sec 4.1 Scenario
// "snippet capped"): when the requested range
// endLine - startLine + 1 exceeds maxLines, the snippet is
// truncated to EXACTLY maxLines total lines and the final line
// is the sentinel
//
//	... [truncated N lines]
//
// where N is the count of lines not present in the output
// (i.e. requested - maxLines). In
// that case the returned truncated flag is true and the snippet
// contains (maxLines - 1) verbatim source lines followed by the
// sentinel line. When maxLines == 1 and the requested range
// spans more than one line, the snippet contains only the
// sentinel.
//
// When the requested range fits inside the cap, truncated is
// false and the snippet contains the source lines verbatim --
// no sentinel is appended, and no extra line-count overhead is
// added. If the file ends before endLine, the snippet ends at
// the file's last line; running off the end of the file is NOT
// on its own treated as truncation, because the caller asked
// for whatever lives at those line numbers and the file simply
// ended sooner.
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

	// Stream the file line-by-line using bufio.Reader.ReadString,
	// which preserves the byte-exact line terminator (including
	// '\r' before '\n' on CRLF files and any trailing bytes on a
	// final unterminated line). Scanner.Text() strips terminators
	// and is therefore unsuitable for C12 / R4.
	br := bufio.NewReaderSize(f, 64*1024)

	// Collect up to maxLines verbatim source lines from the
	// requested range, but keep counting availableInRange past
	// the cap so the suppressed-line tally in the sentinel is
	// based on what was actually present on disk.
	kept := make([]string, 0, maxLines)
	lineNo := 0
	availableInRange := 0
	for {
		line, readErr := br.ReadString('\n')
		// A non-empty `line` even on io.EOF means the last line
		// had no trailing newline. We MUST keep those bytes when
		// they fall in range so the snippet round-trips a
		// no-trailing-newline file faithfully.
		if len(line) > 0 {
			lineNo++
			if lineNo >= startLine && lineNo <= endLine {
				availableInRange++
				if len(kept) < maxLines {
					kept = append(kept, line)
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return "", false, fmt.Errorf("suggest: read %s: %w", absFilePath, readErr)
		}
		if lineNo > endLine {
			break
		}
	}

	// Truncation only fires when the requested range physically
	// held more lines than the cap. Running off the end of the
	// file is not on its own truncation; only suppressing
	// actually-present lines is.
	var b strings.Builder
	if availableInRange > maxLines {
		// Replace the slot the sentinel will occupy: keep the
		// first (maxLines-1) source lines verbatim, then a
		// single sentinel line. Total emitted = maxLines.
		drop := 1
		if maxLines == 0 {
			drop = 0 // unreachable per arg validation, defensive
		}
		for i := 0; i < len(kept)-drop; i++ {
			b.WriteString(kept[i])
		}
		// Ensure the sentinel begins on a fresh line. If the
		// last kept-and-emitted source line lacked a trailing
		// newline (rare in the middle of a file but possible
		// when endLine == final unterminated line), inject one
		// so the sentinel never concatenates onto source bytes.
		s := b.String()
		if len(s) > 0 && s[len(s)-1] != '\n' {
			b.WriteByte('\n')
		}
		// Suppressed count = available source lines minus
		// maxLines. The sentinel occupies one of the M
		// displayed slots, so only (M-1) source lines
		// survive; however, the suppressed tally counts lines
		// absent from the output as a whole (N = requested -
		// maxLines), not lines absent from the source-only
		// portion, matching the implementation-plan Sec 4.1
		// scenario expectation "truncated 300 lines" for a
		// 500-line scope capped at 200.
		suppressed := availableInRange - maxLines
		fmt.Fprintf(&b, "%s%d lines]", truncationSentinelPrefix, suppressed)
		truncated = true
	} else {
		// Fits inside the cap (or the file ended early); emit
		// all kept lines verbatim with their original line
		// terminators and no sentinel.
		for _, line := range kept {
			b.WriteString(line)
		}
	}

	return b.String(), truncated, nil
}
