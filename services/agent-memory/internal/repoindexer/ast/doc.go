// Package ast implements the Stage 3.2 polyglot AST dispatcher
// (per `docs/stories/code-intelligence-AGENT-MEMORY/
// implementation-plan.md` §3.2). Stage 3.1's Repo Indexer
// worker calls `ASTEmitter.EmitFile` once per File Node it
// ensures; this package's `Dispatcher` plugs into that hook,
// chooses a language-specific parser by file extension, parses
// the file, and writes the resulting Class / Method / Block
// Nodes and their static Edges through `graphwriter.Writer`.
//
// # Responsibilities
//
//   - `Dispatcher` -- picks a `LanguageParser` by file extension
//     (overridable per-repo via `LanguageHints`). Drives the
//     two-pass insert protocol: pass 1 inserts every declared
//     Class / Method / Block Node so the local-symbol table is
//     fully populated; pass 2 resolves and inserts the static
//     Edges (`contains`, `extends`, `implements`,
//     `static_calls`) whose dst Node id is known after pass 1.
//     Without the two-pass split, a method that calls a sibling
//     declared later in the same file would emit a `static_calls`
//     edge with a missing dst -- a G2 violation.
//
//   - `LanguageParser` -- the per-language hook. Stage 3.2 ships
//     two implementations per supported language:
//
//   - Tree-sitter parsers (`parser_treesitter.go`, build-
//     tagged `//go:build cgo`). These are the CANONICAL
//     parser core mandated by the implementation plan
//     §3.2 lines 425-427. They consume the upstream
//     grammars vendored in `github.com/smacker/go-tree-
//     sitter/{typescript/typescript,python}` and produce
//     the same `ParseResult` shape as the scanner-backed
//     fallback. Selected when the binary is built with
//     CGO enabled (`CGO_ENABLED=1`, the production
//     default).
//
//   - Lightweight stdlib-only scanners (`parser_typescript.go`
//     and `parser_python.go`). These remain available as
//     a portable fallback for environments without a
//     working C toolchain -- most notably the `make test`
//     path on stock Windows toolchains that defaults to
//     CGO=0. The dispatcher contract is identical, so
//     downstream consumers see no behavioural difference
//     at runtime.
//
//     Selection happens in `parsers_cgo.go` / `parsers_nocgo.go`
//     via the `defaultParsers()` factory. Adding a third
//     language (Go, Java, Rust, etc.) requires adding both
//     implementations and extending both factories.
//
//   - Whitespace normalisation -- `NormalizeSignature` strips
//     comments, collapses whitespace runs to a single space, and
//     removes whitespace adjacent to ASCII punctuation common in
//     type signatures (`,()[]{}<>:;`). Every canonical signature
//     the dispatcher writes is run through this normaliser
//     before fingerprinting; this is the §9.7 / §9.9 risk
//     mitigation -- a formatter-only commit produces a
//     byte-identical canonical signature and therefore a
//     byte-identical Node fingerprint.
//
//   - Method-to-Block subdivision -- `SubdivideMethod` counts
//     normalised logical lines (non-blank, non-comment-only
//     lines after whitespace normalisation). A method whose
//     count exceeds the §8.2 threshold (80) is decomposed into
//     blocks; the v1 contract is the minimal pair
//     {entry, exit}, matching the acceptance scenario
//     "81 normalised logical lines -> 2 Block nodes". Per-
//     control-structure subdivision (one Block per branch /
//     loop / try) is a documented future enhancement and is
//     NOT emitted in v1 so the acceptance count stays exact.
//
// # Canonical signature scheme
//
// Every signature is prefixed with the Repo URL and a kind
// discriminator (consistent with `internal/repoindexer/canonical.go`
// `CanonicalFileSig` / `CanonicalPackageSig`):
//
//	repo:   <url>
//	pkg:    <url>::pkg::<dir>
//	file:   <url>::file::<relPath>
//	class:  <url>::class::<relPath>#<qualifiedName>
//	method: <url>::method::<relPath>#<qualifiedName>(<normalisedParams>)
//	block:  <methodSig>#block_<ordinal>_<kind>
//
// The `<relPath>` embed is load-bearing -- without it, two
// files in the same repo that declare a class `Foo` with a
// method `bar()` would collapse to the same fingerprint and
// silently merge in the graph (G2 violation). The fingerprint
// pre-image also includes the Repo UUID, so cross-repo
// collisions are independently impossible.
//
// # v1 edge scope
//
// Stage 3.2 emits these edges:
//   - `contains`     (file->class, class->method, file->method, method->block)
//   - `extends`      (class->class, when target is in the same file)
//   - `implements`   (class->class, when target is in the same file)
//   - `static_calls` (method->method, when callee is in the same file;
//     receiver-qualified `this.foo()` / `self.foo()`
//     resolve unambiguously, bare-name callees drop on
//     collision)
//   - `imports`      (file->package, materialising a synthetic
//     external-package Node per distinct non-relative
//     module specifier; relative imports `./foo` are
//     dropped pending the cross-file resolver story)
//   - `reads`        (method->class on the enclosing class, with the
//     touched member names recorded in the edge's
//     `attrs_json["members"]`)
//   - `writes`       (method->class on the enclosing class, mirrors
//     `reads`; LHS-of-assignment classifies as a write)
//
// Cross-file references to `extends` / `implements` / `static_calls`
// targets that do not resolve against the local symbol table are
// DROPPED rather than minting placeholder Nodes -- the future
// cross-file resolver story owns the global symbol table that
// would let us re-emit them with real dst ids.
//
// # Per-event language hints
//
// `EmitFileEvent.LanguageHints` (populated by Stage 3.1's
// worker from each repo's `repo.language_hints[]` column)
// takes precedence over the dispatcher-global `WithLanguageHints`
// option whenever the file extension does not map to a
// registered parser. Per-event hints are the canonical wiring
// path; the dispatcher-global option exists as a test
// convenience and a legacy-caller safety net.
//
// # Test-only seam
//
// `Dispatcher` writes through a small `nodeEdgeWriter`
// interface that `*graphwriter.Writer` satisfies. Unit tests
// inject a `*fakeNodeEdgeWriter` that captures calls without
// touching PostgreSQL. Production code passes
// `*graphwriter.Writer` directly via `NewDispatcher`.
package ast
