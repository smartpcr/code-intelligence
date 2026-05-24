//go:build !cgo

package parser

import (
	"bufio"
	"bytes"
	"strings"
)

// astRangeAt returns a synthesised `AstRange` covering one
// source location. Used by per-language `!cgo` parsers (the
// stdlib-based Go adapter and the Python / TypeScript / Java
// lexer adapters) when we have (line, col) coordinates but no
// end-byte. Tree-sitter adapters compute ranges via
// `nodeRange()` in `tree_sitter_common.go` so this helper is
// only needed on the `!cgo` build.
func astRangeAt(startByte, endByte, startLine, endLine, startCol, endCol int) *AstRange {
	if endByte < startByte {
		endByte = startByte
	}
	if endLine < startLine {
		endLine = startLine
	}
	return &AstRange{
		StartByte: uint32(startByte),
		EndByte:   uint32(endByte),
		StartLine: uint32(startLine),
		EndLine:   uint32(endLine),
		StartCol:  uint32(startCol),
		EndCol:    uint32(endCol),
	}
}

// lineCursor walks a byte slice line by line, tracking the byte
// offset of the current line's start. Used by the Python,
// TypeScript, and Java parsers to extract scopes without
// rebuilding a position table.
type lineCursor struct {
	content     []byte
	scanner     *bufio.Scanner
	byteOffset  int
	lineNumber  int
	currentLine string
}

// newLineCursor seeds a lineCursor pointing at the first line.
func newLineCursor(content []byte) *lineCursor {
	s := bufio.NewScanner(bytes.NewReader(content))
	// Lift the scanner buffer ceiling so a single long line
	// (generated TS bundle, minified JS) does not abort the
	// parse.
	s.Buffer(make([]byte, 64*1024), 4*1024*1024)
	return &lineCursor{content: content, scanner: s, lineNumber: 0}
}

// next advances to the next line. Returns false at EOF.
func (c *lineCursor) next() bool {
	if !c.scanner.Scan() {
		return false
	}
	if c.lineNumber > 0 {
		// Include the prior line's newline byte in the running
		// offset. We append +1 unconditionally; for files with
		// CRLF line endings the trailing CR is part of
		// `currentLine` already so the +1 covers exactly the
		// `\n`.
		c.byteOffset += len(c.currentLine) + 1
	}
	c.currentLine = c.scanner.Text()
	c.lineNumber++
	return true
}

// lineRange returns an `*AstRange` covering the current line.
// Useful when the per-language parser only wants to mark a
// scope's *declaration* line (we do not track block-close lines
// without a real parse tree).
func (c *lineCursor) lineRange(startCol, endCol int) *AstRange {
	return astRangeAt(c.byteOffset, c.byteOffset+len(c.currentLine),
		c.lineNumber, c.lineNumber, startCol+1, endCol+1)
}

// stripComment removes a trailing line comment marker (`//`,
// `#`) from `line`, ignoring matches inside string literals.
// Simplified -- we treat strings as `"..."` or `'...'` with `\`
// escapes; sufficient for v1 fixture coverage.
func stripComment(line, marker string) string {
	in := false
	var quote byte
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if in {
			if ch == '\\' {
				i++
				continue
			}
			if ch == quote {
				in = false
			}
			continue
		}
		switch ch {
		case '"', '\'', '`':
			in = true
			quote = ch
		default:
			if strings.HasPrefix(line[i:], marker) {
				return strings.TrimRight(line[:i], " \t")
			}
		}
	}
	return strings.TrimRight(line, " \t")
}

// extractParenList returns the substring between the FIRST `(`
// and its matching `)` in `s`, accounting for nested
// parentheses but ignoring brackets / braces. Returns
// `("", false)` if no balanced pair exists.
func extractParenList(s string) (string, bool) {
	start := strings.IndexByte(s, '(')
	if start < 0 {
		return "", false
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[start+1 : i], true
			}
		}
	}
	return "", false
}
