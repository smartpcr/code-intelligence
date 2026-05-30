# AST Parser Coverage and Degradation Matrix

> **Story:** `code-intelligence:REPO-SCANNER`,
> phase `parser-coverage-verification`,
> stage `degradation-matrix-in-docs` (implementation-plan Stage 1.3).
>
> Authoritative anchors this file is grounded in:
> [`architecture.md` S7](../../../../../docs/stories/code-intelligence-REPO-SCANNER/architecture.md)
> ("Degraded language coverage MUST be loud, not silent") and
> [`tech-spec.md` C1 / C2 / C7](../../../../../docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/tech-spec.md)
> (parser surface contract, canonical-signature stability, and the
> CGO build-tag duality).

This document is the single source of truth for **which file
extensions the dispatcher parses on which build**, and which
languages **degrade silently or skip-with-reason** when the
toolchain is missing. It cross-references the build-tagged
registration files
[`parsers_cgo.go`](./parsers_cgo.go) and
[`parsers_nocgo.go`](./parsers_nocgo.go) so a reader can verify
each row against the actual roster `defaultParsers()` returns.

## TL;DR

- **CGO=1 build** (`CGO_ENABLED=1`, C compiler on PATH) registers
  the five tree-sitter parsers (C, C++, C#, Go, Rust) plus the
  `pwsh` subprocess PowerShell parser. Eight extensions parse end
  to end via `defaultParsers()` when `.ts` / `.tsx` / `.js` /
  `.jsx` / `.mjs` / `.cjs` / `.py` / `.pyi` are layered on via
  `WithParsers` (see "TS/JS and Python registration" below).
- **CGO=0 build** (stock `make test` on Windows, the portable
  smoke-test gate, any host without a C toolchain) registers
  **only** the PowerShell parser; every `.c` / `.h` / `.cpp` /
  `.cxx` / `.c++` / `.hpp` / `.hh` / `.hxx` / `.h++` / `.cs` /
  `.go` / `.rs` file is **skipped** with
  `ast.dispatch.skip{reason="no_parser"}` and the worker drains
  the next file in the queue (architecture S7 `loud, not silent`
  guarantee).
- **`pwsh` not on PATH** (any build): the PowerShell parser
  returns `ErrParserUnavailable`; the dispatcher logs
  `ast.dispatch.skip{reason="pwsh_not_available"}` at Info level
  and the worker drains the next file.

The skip reasons surface in the CLI summary (Stage 8+ "scan-many"
output) so the operator notices coverage gaps before reading the
graph — silent degradation is a story-level non-goal (architecture
S7).

## Build-tag roster (what `defaultParsers()` returns)

| Build constraint | `defaultParsers()` source | Registered parsers (order matters for overlapping extensions) |
| --- | --- | --- |
| `//go:build cgo` | [`parsers_cgo.go`](./parsers_cgo.go) | `NewTreeSitterCParser`, `NewTreeSitterCppParser`, `NewTreeSitterCSharpParser`, `NewTreeSitterGoParser`, `NewTreeSitterRustParser`, `NewPowerShellParser` |
| `//go:build !cgo` | [`parsers_nocgo.go`](./parsers_nocgo.go) | `NewPowerShellParser` (only) |

Ordering note: in [`parsers_cgo.go`](./parsers_cgo.go) the C
parser is listed BEFORE the C++ parser. Both claim `.h`; the
dispatcher's `extMap` is overwrite-on-last-wins, so the C++
parser intentionally owns `.h` headers. This is guarded by
`TestDefaultParsers_CBeforeCpp` and the inline comment in
`parsers_cgo.go::defaultParsers`.

## Per-language coverage matrix

The eight v1 languages, in the order the polyglot smoke test
([`parsers_polyglot_smoke_test.go`](./parsers_polyglot_smoke_test.go))
drives them. The **Skip reason** column records the exact `reason`
slug the dispatcher emits on `ast.dispatch.skip` when the parser
is unavailable on the current build/host.

| Language | Extensions | Source parser file (this dir) | CGO=1 status | CGO=0 status | Runtime dependency | Skip reason (when unavailable) |
| --- | --- | --- | --- | --- | --- | --- |
| TypeScript / JavaScript | `.ts .tsx .js .jsx .mjs .cjs` | [`parser_treesitter.go`](./parser_treesitter.go) (`NewTreeSitterTypeScriptParser`) + [`parser_typescript.go`](./parser_typescript.go) (`NewTypeScriptParser`, scanner fallback) | parses (tree-sitter) when registered via `WithParsers`; **not** in `defaultParsers()` -- see "TS/JS and Python registration" below | parses (stdlib scanner) when registered via `WithParsers`; **not** in `defaultParsers()` | none (CGO toolchain only for the tree-sitter variant) | `no_parser` if neither variant is registered |
| Python | `.py .pyi` | [`parser_treesitter.go`](./parser_treesitter.go) (`NewTreeSitterPythonParser`) + [`parser_python.go`](./parser_python.go) (`NewPythonParser`, scanner fallback) | parses (tree-sitter) when registered via `WithParsers`; **not** in `defaultParsers()` -- see "TS/JS and Python registration" below | parses (stdlib scanner) when registered via `WithParsers`; **not** in `defaultParsers()` | none (CGO toolchain only for the tree-sitter variant) | `no_parser` if neither variant is registered |
| C | `.c .h` (note: `.h` is owned by C++ when both register; see ordering note above) | [`parser_treesitter_c.go`](./parser_treesitter_c.go) (`NewTreeSitterCParser`) | parses (tree-sitter) via `defaultParsers()` | **skipped** -- not in `defaultParsers()`; `.c` files fall through the dispatcher with no Nodes | none (CGO toolchain only) | `no_parser` |
| C++ | `.cc .cpp .cxx .c++ .hpp .hh .hxx .h++` (and `.h` via the C-before-C++ ordering) | [`parser_treesitter_cpp.go`](./parser_treesitter_cpp.go) (`NewTreeSitterCppParser`) | parses (tree-sitter) via `defaultParsers()` | **skipped** -- not in `defaultParsers()`; `.cpp` / `.h` / etc. fall through with no Nodes | none (CGO toolchain only) | `no_parser` |
| C# | `.cs` | [`parser_treesitter_csharp.go`](./parser_treesitter_csharp.go) (`NewTreeSitterCSharpParser`) | parses (tree-sitter) via `defaultParsers()` | **skipped** -- not in `defaultParsers()`; `.cs` files fall through with no Nodes | none (CGO toolchain only) | `no_parser` |
| Go | `.go` | [`parser_treesitter_go.go`](./parser_treesitter_go.go) (`NewTreeSitterGoParser`) | parses (tree-sitter) via `defaultParsers()` | **skipped** -- not in `defaultParsers()`; `.go` source files fall through with no Nodes | none (CGO toolchain only) | `no_parser` |
| Rust | `.rs` | [`parser_treesitter_rust.go`](./parser_treesitter_rust.go) (`NewTreeSitterRustParser`) | parses (tree-sitter) via `defaultParsers()` | **skipped** -- not in `defaultParsers()`; `.rs` files fall through with no Nodes (asserted by `TestDefaultParsers_NoCGOOmitsRust` in [`parsers_nocgo_rust_test.go`](./parsers_nocgo_rust_test.go)) | none (CGO toolchain only) | `no_parser` |
| PowerShell | `.ps1 .psm1 .psd1` | [`parser_powershell.go`](./parser_powershell.go) (`NewPowerShellParser`) | parses (subprocess) via `defaultParsers()` **iff** `pwsh` on PATH | parses (subprocess) via `defaultParsers()` **iff** `pwsh` on PATH (only language in either roster that works under CGO=0) | `pwsh` (PowerShell 7+) on PATH | `pwsh_not_available` when `pwsh` is absent; emitted by [`dispatcher.go`](./dispatcher.go) `EmitFile`'s `errors.Is(err, ErrParserUnavailable)` branch |

### TS/JS and Python registration

The TypeScript/JavaScript and Python parsers ship in this
package but are **NOT** appended to `defaultParsers()` in either
[`parsers_cgo.go`](./parsers_cgo.go) or
[`parsers_nocgo.go`](./parsers_nocgo.go). Callers that need
`.ts` / `.py` routing layer them on explicitly:

```go
d := ast.NewDispatcher(writer, ast.WithParsers(
    append(ast.DefaultParsers(),
        ast.NewTreeSitterTypeScriptParser(),
        ast.NewTreeSitterPythonParser(),
    )...,
))
```

The polyglot smoke gate
([`parsers_polyglot_smoke_test.go`](./parsers_polyglot_smoke_test.go))
demonstrates this pattern via its `polyglotParserSet()` helper.
The scanner-only fallbacks `NewTypeScriptParser()` and
`NewPythonParser()` are the CGO=0 substitutes -- swap them in
when CGO is unavailable.

## Skip-reason semantics

| Trigger | Dispatcher behaviour | Log event | Worker continues |
| --- | --- | --- | --- |
| Extension has no entry in `extMap` (no registered parser claims it) | Returns empty `EmitResult` with nil error | `ast.dispatch.skip{reason="no_parser"}` (Info) | yes |
| Parser returns `ErrParserUnavailable` (runtime dependency missing -- e.g. PowerShell without `pwsh` on PATH) | Returns empty `EmitResult` with nil error | `ast.dispatch.skip{reason="pwsh_not_available"}` (Info; the slug is extracted by `dispatcher.go::parseUnavailableReason` from the sentinel-wrapped message) | yes |
| Parser returns any other error (genuine grammar rejection, subprocess crash, `context.DeadlineExceeded`, etc.) | Returns empty `EmitResult` with nil error | `ast.parse.error` (Warn) via `safeParse` | yes |
| Parser panics | Recovered by `safeParse` | `ast.parse.error` (Warn) with `panic=true` | yes |

The C6 invariant (`tech-spec.md` C6) is the contract: **parse
errors are file-local**. No parser failure aborts the worker;
all four rows above keep draining the next file.

## Build / verification commands

Stock `make test` (portable, CGO=0) -- only PowerShell parses; the
C/C++/C#/Go/Rust extensions all hit the `no_parser` branch:

```powershell
Set-Location services\agent-memory
make test-nocgo
go test -count=1 .\internal\repoindexer\ast
```

CGO=1 path (production, Linux CI, Windows with a `gcc` / `clang`
on PATH) -- the full tree-sitter roster registers:

```powershell
$env:CGO_ENABLED='1'
Set-Location services\agent-memory
make test-cgo
go test -tags cgo -count=1 -run TestPolyglotParseSmoke -v .\internal\repoindexer\ast
```

`pwsh` smoke (any build) -- run a `.ps1` file end to end; the
PowerShell parser short-circuits with the sentinel branch when
`pwsh` is absent:

```powershell
$env:CGO_ENABLED='1'  # or '0' -- PowerShell registers in both modes
go test -tags cgo -count=1 -run TestPowerShellFixture .\internal\repoindexer\ast
```

## See also

- [`.claude/context/tests.md`](../../../../../.claude/context/tests.md)
  -- AST parser language support matrix, polyglot coverage matrix,
  and CGO + `pwsh` validation caveats. Points at this file for
  the canonical degradation rows.
- [`parsers_cgo.go`](./parsers_cgo.go) -- CGO=1 `defaultParsers()`
  (C, C++, C#, Go, Rust, PowerShell).
- [`parsers_nocgo.go`](./parsers_nocgo.go) -- CGO=0
  `defaultParsers()` (PowerShell only).
- [`parsers_polyglot_smoke_test.go`](./parsers_polyglot_smoke_test.go)
  -- per-language `>=1` class + `>=1` method + `>=1` `static_calls`
  smoke gate that exercises every row in the matrix above
  (CGO=1; PowerShell row skips when `pwsh` is absent).
- [`docs/stories/code-intelligence-REPO-SCANNER/architecture.md`](../../../../../docs/stories/code-intelligence-REPO-SCANNER/architecture.md)
  S7 -- "Degraded language coverage MUST be loud, not silent."
- [`docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/tech-spec.md`](../../../../../docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/tech-spec.md)
  C1 (parser surface), C2 (canonical-signature stability),
  C7 (build-tag duality).
