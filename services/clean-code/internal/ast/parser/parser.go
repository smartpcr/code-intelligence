package parser

import (
	"context"
	"errors"
)

// ParserVersion is the version tag embedded in every emitted
// `AstFile.parser_version`. Bumped any time a per-language
// parser's output shape changes in a way that would invalidate
// a cached `MetricSample.metric_version` (tech-spec Sec 9.14
// grammar-version drift). Stage 2.1 iter 3 ships
// `v1-tree-sitter-2026.05` -- the bump from
// `v1-structural-2026.05` reflects the move from a lexer-only
// front end to the real tree-sitter parser fleet (`cgo` path)
// + AstEdge emission. Subsequent stages MUST bump this
// whenever they change scope-discovery rules.
const ParserVersion = "v1-tree-sitter-2026.05"

// ErrUnsupportedLanguage is returned by `Registry.For` and
// `Registry.Detect` when the requested language tag is not
// pinned in the v1 language coverage (tech-spec Sec 8.6 lines
// 1005-1016). Adding a fifth v1 language requires editing both
// `SupportedLanguages` AND the `Register*` guards in
// `registry.go` -- the guard is not a config knob.
var ErrUnsupportedLanguage = errors.New("clean-code/parser: unsupported language (v1 pin: go, python, typescript, java)")

// ErrEmptyContent is returned by `Parser.Parse` when the input
// byte slice is empty. Parsing zero bytes is always a producer
// bug (the Metric Ingestor MUST short-circuit empty files
// before dispatching to the parser fleet) so we surface the
// error rather than emitting a degraded `AstFile`.
var ErrEmptyContent = errors.New("clean-code/parser: empty content")

// Parser is the contract every per-language adapter implements.
// The brief (Stage 2.1, implementation-plan.md line 166) pins
// the exact signature: `Parse(ctx, path, bytes) (*AstFile, error)`.
//
// Contract.
//   - `path` is repo-relative and serves both as the source
//     location (returned verbatim in `AstFile.path`, normalised
//     to forward-slash) and as the language sniff input when
//     callers route via the registry.
//   - `content` is the file bytes; the parser MUST NOT mutate
//     them. `Parser.Parse(ctx, path, nil)` returns
//     `ErrEmptyContent`.
//   - The returned `*AstFile` has at least one `AstScope`
//     (`SCOPE_KIND_FILE`) and a populated `language` tag
//     matching the parser's Language().
//   - The parser may emit a `degraded_reason` while still
//     returning a non-nil `AstFile`; callers MUST surface the
//     reason but should not treat a degraded parse as a fatal
//     error.
type Parser interface {
	// Language returns the canonical language tag for this
	// adapter (`"go"`, `"python"`, `"typescript"`, `"java"`).
	// MUST be one of `SupportedLanguages`.
	Language() string
	// Parse turns `(path, content)` into a canonical
	// `*AstFile`. See the interface doc-comment for the
	// invariants every implementation MUST honour.
	Parse(ctx context.Context, path string, content []byte) (*AstFile, error)
}

// AstFile is an alias for the generated proto type so callers
// can `import .../parser` and never type the long `astv1`
// import path themselves. The shape is the proto-generated
// struct; this alias is purely an ergonomics layer.
//
// (Defined in `ast_alias.go` so the parser package can be read
//  by Go developers without bouncing through `internal/ast/v1`
//  in a separate IDE tab.)
