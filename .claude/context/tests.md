# Tests

> Last updated: 2026-05-27

## Test stack

- Go's standard `testing` package is the primary test framework.
- `go-sqlmock` is used for SQL-facing unit tests.
- Live PostgreSQL tests are used for migration/DDL/role behavior when a DSN is available.
- Clean-code E2E scenarios use Gherkin `.feature` files plus Go test files under `services\clean-code\test\e2e\code-intelligence-CLEAN-CODE`.
- Some parser paths require CGO and a C compiler because of tree-sitter.
- Race-detector tests require CGO and are intended for Linux CI.

## Common commands

### agent-memory

From `services\agent-memory`:

```powershell
make test
make test-race
go test ./...
```

Package-focused example:

```powershell
go test -count=1 ./internal/agentapi
```

### clean-code

From `services\clean-code`:

```powershell
make test
make test-nocgo
make test-cgo
make test-race
go test ./...
```

Package-focused example:

```powershell
go test -count=1 ./internal/evaluator
```

### pre-commit

From the repo root:

```powershell
pre-commit run --all-files
```

## Integration and E2E

### agent-memory local stack

CI brings up `services\agent-memory\deploy\local` and waits for Postgres, Qdrant, OTel, and partition-maintainer healthchecks. Reproduce locally:

```powershell
Set-Location services\agent-memory\deploy\local
docker compose up -d --build
docker compose ps
```

### clean-code migration integration

The clean-code CI migration job sets:

```powershell
$env:CLEAN_CODE_PG_URL = "postgres://clean_code:clean_code@localhost:5432/clean_code?sslmode=disable"
```

Then runs `make migrate-up`, checks schema state with `psql`, runs `make migrate-down`, and executes storage tests against the live DB.

### clean-code E2E workflows

The repo contains dedicated E2E workflows for:

- repo indexer and metric ingestor
- external metric ingest webhook
- policy steward and SOLID rule engine
- evaluator surface and management surface

The root `Makefile` also has `test-phase-03`, which discovers Compose ports, migrates/seeds PostgreSQL, and runs the phase-03 E2E Go test subset.

## CI expectations

`agent-memory-ci.yml` runs:

1. `make lint`
2. `make build`
3. `make test`
4. `make test-race`
5. `make proto` and fails on generated binding drift
6. pre-commit hooks
7. local stack healthchecks

`clean-code-ci.yml` runs:

1. `make lint`
2. `make build`
3. C toolchain verification
4. `make test-nocgo`
5. `make test-cgo`
6. `make test-race`
7. migration integration
8. container build

## AST parser support matrix (Stage 3.2)

The repo-indexer AST dispatcher
(`services\agent-memory\internal\repoindexer\ast`) routes each
file to a `LanguageParser` by extension. Two back-ends ship per
language: tree-sitter (`//go:build cgo`, canonical per
implementation-plan §3.2) and stdlib-only scanners
(`//go:build !cgo`, portable fallback).

| Language   | Extensions               | Tree-sitter (CGO=1) | Scanner (CGO=0) | Notes                                                                                  |
|------------|--------------------------|---------------------|-----------------|----------------------------------------------------------------------------------------|
| TypeScript | `.ts .tsx .js .jsx .mjs .cjs` | yes            | yes             | TSX/JSX routes through `tsx` grammar; non-JSX through `typescript`.                    |
| Python     | `.py .pyi`               | yes                 | yes             | Receiver-qualified `self.foo()` resolved through `walkPySelfCalls`.                    |
| Rust       | `.rs`                    | yes                 | no              | Stage 5.1 emits `ClassDecl` for `struct_item` / `enum_item` / `trait_item` (trait supertraits → `Extends`; enum variants NOT emitted as methods). `MethodDecl` covers `function_item` at file scope (free function: `EnclosingClass=""`), inherent `impl Foo { fn ... }` (`EnclosingClass="Foo"`), trait impls `impl Trait for Foo { fn ... }` (`EnclosingClass="Foo"`, `LangMeta["trait"]="Trait"`, and `Foo.Implements` gains `"Trait"`), and trait body items (`function_item` → `LangMeta["trait_default"]=true`; `function_signature_item` → required, body-less). `use_declaration` handled for single / grouped (`{A,B}`) / aliased (`as Bar`) / wildcard (`::*`). Body walk collects bare `Calls` (`scoped_identifier` callees keep the rightmost segment), `self.X()` `ReceiverCalls`, and `self.field` `MemberAccesses` (write LHS dedupes with read RHS). Non-`self` receiver calls (`x.foo()`) are NOT collected -- receiver type inference is deferred. In-file `mod_item` recurses without propagating the module name into `QualifiedName`; `mod foo;` (out-of-line) is skipped (other-file source surfaces independently). `macro_invocation` / `macro_definition` are explicit non-goals. CGO-only by design -- no stdlib scanner fallback exists; `.rs` files under CGO=0 fall through the dispatcher with no nodes (asserted by `TestDefaultParsers_NoCGOOmitsRust` in `parsers_nocgo_rust_test.go`). |

CGO is the default OFF state on stock Windows toolchains and on
the portable `make test` path. To exercise the tree-sitter
back-ends locally set `CGO_ENABLED=1` and ensure a C compiler
(gcc/clang) is on PATH. CI's `make test-race` step in
`.github/workflows/agent-memory-ci.yml` is the canonical
exerciser of the CGO=1 path.

### Rust parser CGO validation (Stage 5.1)

The Rust parser (`parser_treesitter_rust.go`) is gated on
`//go:build cgo` because it links against the smacker
tree-sitter Rust grammar. On hosts WITHOUT a C toolchain (the
default `make test` gate, the orchestrator validator that ran
this story's iter-8 evaluator):

- `parser_treesitter_rust_test.go` and
  `parser_treesitter_rust_dispatcher_test.go` are silently
  skipped.
- `parser_treesitter_rust_contract_test.go` (no `//go:build cgo`
  tag, added in iter 9) DOES run and provides a structural
  guard: it parses `parser_treesitter_rust.go` via the
  stdlib `go/parser` and asserts the documented invariants
  (`function_item` → `appendTraitDefaultMethod`,
  `function_signature_item` → `appendTraitRequiredMethod`,
  `LangMeta["trait_default"]=true` is set ONLY on the default
  branch, trait impls write `ClassDecl.Implements` not
  `LangMeta["implements"]`, `pendingImpls` dedupes via
  `appendUnique`, and the public factory
  `NewTreeSitterRustParser` exists). The audit-friendly
  mapping from each disputed invariant to the assertion that
  guards it (so a reviewer who cannot run CGO can still
  confirm the contract holds):

  | Invariant (claimed broken in iter-8) | Reality (line in `parser_treesitter_rust.go`)             | Non-CGO guard (`parser_treesitter_rust_contract_test.go`)                                       |
  |---|---|---|
  | trait `function_item` (default body) → `trait_default=true` | `handleTrait` dispatches to `appendTraitDefaultMethod`; line 496 sets `m.LangMeta["trait_default"] = true` | `TestRustParserContract_FunctionItemDispatchesToTraitDefault` + `..._TraitDefaultFlagIsSetExactly` |
  | trait `function_signature_item` (no body) → required, no flag | `handleTrait` dispatches to `appendTraitRequiredMethod`; flag is never written here | `TestRustParserContract_FunctionSignatureDispatchesToRequired`                                  |
  | trait impls populate `ClassDecl.Implements` (struct field) | `handleImpl` line 426 and `appendClass` line 909 write `c.Implements = appendUnique(...)`; no write to `LangMeta["implements"]` | `TestRustParserContract_ImplementsIsStructFieldNotLangMeta`                                     |
  | `pendingImpls` dedupes duplicate trait impls | line 431 routes through `appendUnique`; line 426 also | `TestRustParserContract_PendingImplsUsesAppendUnique` + `..._ImplementsAccumulatorAlsoUsesAppendUnique` |
  | Public surface (`Language()=="rust"`, `Extensions()==[".rs"]`, `NewTreeSitterRustParser`) | string literals and factory function are present | `TestRustParserContract_LanguageAndExtensionsStringLiterals` + `..._NewTreeSitterRustParserExists` |
  | Anchor doc-block mentions `trait_default` + `Implements` | top-of-file READ FIRST block | `TestRustParserContract_DocBlockMentionsTraitDefaultContract`                                   |
- The dispatcher's Pass 2d trait-default behaviour is
  exercised via `fakeStaticParser` in
  `dispatcher_pass2bd_test.go` (`TestDispatcher_Rust_*`), which
  is non-CGO and runs everywhere.

On hosts WITH a C toolchain on PATH:

```powershell
Set-Location services\agent-memory
make test-cgo-rust         # runs Rust-named CGO tests only
$env:CGO_ENABLED='1'; go test ./internal/repoindexer/ast/... -run Rust -count=1
```

Both commands exercise the real smacker tree-sitter Rust
grammar end-to-end against the fixtures in
`parser_treesitter_rust_test.go` (parser-level) and
`parser_treesitter_rust_dispatcher_test.go` (writer-level
edges). The orchestrator's validator is expected to run the
contract test on every iter regardless of CGO availability,
and the full CGO suite on CI's Linux runner.

## Current local validation caveats


During the 2026-05-27 prime run on Windows, full-suite validation was not green after merging `origin/feature/memory` and `origin/feature/clean-code` into `main`.

- `services\agent-memory`: `go test ./...` failed in baseline/deploy checks plus selected `internal/agentapi`, `internal/repoindexer/ast`, `internal/webhookreceiver`, and `pkg/fingerprint` tests.
- `services\clean-code`: `make test` referenced missing `cmd\maketest`; direct `go test ./...` reported missing `go.sum` entries and `TestNamespace_Pinned` drift.

Re-check these before assuming the branch has a clean baseline.

