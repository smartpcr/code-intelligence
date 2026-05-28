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
| Rust       | `.rs`                    | yes                 | no              | v1 emits `ClassDecl` for `struct_item` / `enum_item` / `trait_item`; trait supertraits populate `Extends`. Enum variants are NOT emitted as methods. In-file `mod_item` recurses without propagating the module name into `QualifiedName`. CGO-only by design -- no stdlib scanner fallback exists. |

CGO is the default OFF state on stock Windows toolchains and on
the portable `make test` path. To exercise the tree-sitter
back-ends locally set `CGO_ENABLED=1` and ensure a C compiler
(gcc/clang) is on PATH. CI's `make test-race` step in
`.github/workflows/agent-memory-ci.yml` is the canonical
exerciser of the CGO=1 path.

## Current local validation caveats


During the 2026-05-27 prime run on Windows, full-suite validation was not green after merging `origin/feature/memory` and `origin/feature/clean-code` into `main`.

- `services\agent-memory`: `go test ./...` failed in baseline/deploy checks plus selected `internal/agentapi`, `internal/repoindexer/ast`, `internal/webhookreceiver`, and `pkg/fingerprint` tests.
- `services\clean-code`: `make test` referenced missing `cmd\maketest`; direct `go test ./...` reported missing `go.sum` entries and `TestNamespace_Pinned` drift.

Re-check these before assuming the branch has a clean baseline.

