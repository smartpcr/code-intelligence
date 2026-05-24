package parser

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// fileScopeID returns the deterministic intra-file ID assigned
// to the file-level scope. Stage 2.1 parsers use a synthetic
// `local:0` prefix; Stage 2.2 rewrites these to durable
// `scope_id` UUIDs.
const fileScopeID = "local:0"

// localID formats an intra-file ordinal as `local:<n>`. Using
// a fixed prefix means a `grep -F "local:"` lands every
// placeholder scope ID in the generated AST.
func localID(ordinal int) string {
	return "local:" + strconv.Itoa(ordinal)
}

// sha256Hex returns the lowercase hex SHA-256 of the input.
// Used to populate `AstFile.content_sha256`.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// normalisePath turns Windows backslashes into forward slashes
// so `AstFile.path` is portable across platforms. Callers MUST
// hand the parser a repo-relative path; the parser does not
// strip drive letters or absolute prefixes.
func normalisePath(p string) string {
	return strings.ReplaceAll(p, `\`, "/")
}

// fileName returns the basename of a forward-slash normalised
// path. `path/to/foo.go` -> `foo.go`.
func fileName(p string) string {
	p = normalisePath(p)
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// scopeBuilder accumulates scopes / symbols / edges while a
// per-language parser walks the source. All per-language
// parsers use this builder so the resulting `AstFile` follows
// the same ordering convention (source order, intra-file
// `local:<n>` IDs) regardless of which language emitted it.
type scopeBuilder struct {
	scopes  []*AstScope
	symbols []*AstSymbol
	edges   []*AstEdge
	// nextOrdinal is the running counter feeding `localID`.
	// Starts at 1 because `local:0` is reserved for the file
	// scope (added in `newScopeBuilder`).
	nextOrdinal int
	// fileScope is the synthetic file-level scope produced by
	// `newScopeBuilder`. Per-language parsers attach top-level
	// declarations under this scope (their `parent_scope_id`
	// points to `fileScope.scope_id`).
	fileScope *AstScope
}

// newScopeBuilder seeds the builder with a file-level scope
// covering `[0, len(content))`. The file scope is always the
// first entry in `scopes` and has `scope_id = "local:0"`.
func newScopeBuilder(filePath string, lineCount, byteCount int) *scopeBuilder {
	file := &AstScope{
		ScopeId:       fileScopeID,
		ScopeKind:     ScopeKindFile,
		Name:          fileName(filePath),
		QualifiedName: fileName(filePath),
		Range: &AstRange{
			StartByte: 0,
			EndByte:   uint32(byteCount),
			StartLine: 1,
			EndLine:   uint32(lineCount),
			StartCol:  1,
			EndCol:    1,
		},
	}
	return &scopeBuilder{
		scopes:      []*AstScope{file},
		nextOrdinal: 1,
		fileScope:   file,
	}
}

// addScope appends a scope and returns the assigned `scope_id`
// (a `local:<n>` placeholder). The caller is responsible for
// populating `parent_scope_id` -- the builder cannot infer
// nesting on its own.
func (b *scopeBuilder) addScope(s *AstScope) string {
	id := localID(b.nextOrdinal)
	b.nextOrdinal++
	s.ScopeId = id
	b.scopes = append(b.scopes, s)
	return id
}

// addSymbol appends a symbol. Symbol IDs share the same ordinal
// space as scope IDs so a `grep -F "local:7"` lands the unique
// entry whether it's a scope or a symbol.
func (b *scopeBuilder) addSymbol(s *AstSymbol) string {
	id := localID(b.nextOrdinal)
	b.nextOrdinal++
	s.SymbolId = id
	b.symbols = append(b.symbols, s)
	return id
}

// build finalises and returns the canonical `*AstFile`.
func (b *scopeBuilder) build(language, path string, content []byte) *AstFile {
	return &AstFile{
		Language:      language,
		Path:          normalisePath(path),
		ContentSha256: sha256Hex(content),
		ParserVersion: ParserVersion,
		Scopes:        b.scopes,
		Symbols:       b.symbols,
		Edges:         b.edges,
	}
}

// addEdge appends an edge, assigning a `local:<n>` ordinal as
// the edge id so Stage 2.2 can rewrite to durable UUIDs while
// still finding the edge via grep.
func (b *scopeBuilder) addEdge(e *AstEdge) string {
	id := localID(b.nextOrdinal)
	b.nextOrdinal++
	e.EdgeId = id
	b.edges = append(b.edges, e)
	return id
}

// scopeRef builds an AstRef pointing at a scope by `scope_id`.
func scopeRef(scopeID string) *AstRef {
	return &AstRef{Kind: RefKindScope, Id: scopeID}
}

// externalScopeRef builds an AstRef pointing at an out-of-file
// scope by its fully-qualified name. Stage 2.2 rewrites these
// to durable scope IDs at link time; until then the `qualified:`
// prefix marks the entry as unresolved without losing the
// referent's name.
func externalScopeRef(qualifiedName string) *AstRef {
	return &AstRef{Kind: RefKindScope, Id: "qualified:" + qualifiedName}
}

// joinQualified joins two qualified-name components with a dot,
// handling empty parents (e.g. file-scope children). Used by
// per-language parsers to assemble `AstScope.qualified_name`.
func joinQualified(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}

// lineCounts returns (line count, byte count) for `content`.
// Empty input is one line (matching POSIX behaviour for empty
// files counted with `wc -l` + 1).
func lineCounts(content []byte) (int, int) {
	if len(content) == 0 {
		return 1, 0
	}
	lines := 1
	for _, b := range content {
		if b == '\n' {
			lines++
		}
	}
	return lines, len(content)
}

// splitTopLevelCommas splits `s` on commas that appear at the
// top angle/paren/brace nesting level. Used to break a method
// parameter list into individual parameter slots without
// re-implementing a Go expression parser.
func splitTopLevelCommas(s string) []string {
	var (
		parts   []string
		current strings.Builder
		angle   int
		paren   int
		brace   int
		bracket int
	)
	for _, r := range s {
		switch r {
		case '<':
			angle++
		case '>':
			if angle > 0 {
				angle--
			}
		case '(':
			paren++
		case ')':
			if paren > 0 {
				paren--
			}
		case '{':
			brace++
		case '}':
			if brace > 0 {
				brace--
			}
		case '[':
			bracket++
		case ']':
			if bracket > 0 {
				bracket--
			}
		case ',':
			if angle == 0 && paren == 0 && brace == 0 && bracket == 0 {
				parts = append(parts, strings.TrimSpace(current.String()))
				current.Reset()
				continue
			}
		}
		current.WriteRune(r)
	}
	if current.Len() > 0 {
		parts = append(parts, strings.TrimSpace(current.String()))
	}
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// validateContent is the common entry-point guard every parser
// runs against `(path, content)` before doing real work.
func validateContent(path string, content []byte) error {
	if len(content) == 0 {
		return fmt.Errorf("%w (path=%q)", ErrEmptyContent, path)
	}
	return nil
}

// looksLikeABC returns true when the comma-separated `bases`
// string contains a canonical Python ABC marker (`ABC`,
// `abc.ABC`, `Protocol`, `typing.Protocol`, or any name
// referencing `ABCMeta`). Shared by the lexer and tree-sitter
// Python adapters so both classify ABCs as `ScopeKindInterface`.
func looksLikeABC(bases string) bool {
	for _, b := range splitTopLevelCommas(bases) {
		switch strings.TrimSpace(b) {
		case "ABC", "abc.ABC", "Protocol", "typing.Protocol":
			return true
		}
		if strings.Contains(b, "ABCMeta") {
			return true
		}
	}
	return false
}

// boolStr returns "true" / "false". Shared by per-language
// adapters that need to stamp a string attribute from a Go
// boolean.
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
