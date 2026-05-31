# `tools/buildtaglint` — custom `go vet` analyzers

This directory ships the two custom `go/analysis` analyzers
required by [`tech-spec.md` §8.10](../../../../docs/stories/code-intelligence-REFACTOR-GUIDE/tech-spec.md)
(rule `no-production-sql-import` and rule `no-production-build-tag-bypass`).

The package layout is the standard "thin `cmd/` over a
reusable analyzer library" shape:

```
tools/buildtaglint/
├── main.go              -- multichecker.Main entry point
├── README.md            -- this file
└── lint/
    ├── buildtag.go      -- BuildTagAnalyzer (nobuildtagbypass)
    ├── sqlimport.go     -- SQLImportAnalyzer (nocliproductionsqlimport)
    ├── *_test.go        -- analysistest-based unit tests
    └── testdata/        -- per-scenario fixture packages
```

## What the rules enforce

### `nobuildtagbypass` → `no-production-build-tag-bypass`

Anchor: [`tech-spec.md` §8.10](../../../../docs/stories/code-intelligence-REFACTOR-GUIDE/tech-spec.md), constraint C2.

For every non-test Go file under `internal/cli/devpolicy/`,
the analyzer walks every composite literal that resolves
(via `pass.TypesInfo`) to the
`<...>/internal/policy/steward.PolicyVersion` named type and
reports a diagnostic when the literal omits or sets
`Signature` to `nil` **and** the file does not carry a
`//go:build` constraint that excludes the `prod` tag.

Type resolution (not selector-name matching) means an
aliased import (`stewardv1 "<...>/policy/steward"`) is
still caught — fixing the gap that the prior
syntactic-only implementation had.

### `nocliproductionsqlimport` → `no-production-sql-import`

Anchor: [`tech-spec.md` §8.10](../../../../docs/stories/code-intelligence-REFACTOR-GUIDE/tech-spec.md), constraint C2.

For every analyzed file, the analyzer walks the import list
and reports a diagnostic when an import path:

- equals `database/sql`, or
- ends in `_sql_store`, or
- ends in `/sql_store`.

The analyzer walks `*ast.File.Imports` directly so **blank
imports** (`import _ "database/sql"`) and other
side-effect-only imports are flagged — fixing the gap that
the symbol-based `forbidigo` rule has.

Scope is enforced by the **caller**: `make lint-cli` only
passes `./cmd/cleanc/... ./internal/cli/...` to `go vet`.
This keeps the analyzer reusable from future scopes
without recompilation.

## How to run

```bash
# From services/clean-code/:
make lint-cli
```

`make lint-cli` is automatically invoked by `make lint` so
the standard developer workflow picks up both rules. CI
runs `make lint-cli` as a dedicated step in
`.github/workflows/clean-code-ci.yml` so PRs that smuggle
either a forbidden SQL import or an unconstrained bypass
file fail before code review.

Under the hood, `make lint-cli`:

1. Builds the multichecker binary at `bin/buildtaglint`.
2. Invokes `go vet -vettool=bin/buildtaglint ./cmd/cleanc/... ./internal/cli/...`.
3. Invokes `golangci-lint run --disable-all --enable forbidigo`
   on the same package set so the forbidigo rule
   (which catches symbol-use violations under the same
   `no-production-sql-import` name) runs end-to-end too.

## How to test

```bash
# Fixture-based + unit tests, in <1 minute:
cd services/clean-code
go test ./tools/buildtaglint/lint/... -count=1
```

The fixture set under `lint/testdata/src/` covers:

- `example.com/internal/cli/devpolicy/buildtag_bad_missing_tag` — unsigned literal, no build tag → diagnostic.
- `example.com/internal/cli/devpolicy/buildtag_bad_aliased`     — unsigned literal via `stewardv1 "..."` alias → diagnostic.
- `example.com/internal/cli/devpolicy/buildtag_good_not_prod`   — unsigned literal with `//go:build !prod` → no diagnostic.
- `example.com/internal/cli/devpolicy/buildtag_good_signed`     — signed literal, no build tag → no diagnostic.
- `sqlimport_bad_stdlib`   — imports `database/sql` directly → diagnostic.
- `sqlimport_bad_blank`    — `import _ "database/sql"` → diagnostic (the gap forbidigo misses).
- `sqlimport_bad_sqlstore` — imports a fixture `*_sql_store` package → diagnostic.
- `sqlimport_good`         — imports only safe stdlib packages → no diagnostic.

The "positive `//go:build prod` constraint" case is
covered by `TestBuildExprExcludesProd("prod")` (a unit
test) rather than a fixture, because analysistest builds
with the default tag set and cannot load a `//go:build prod`
file at all.

## Why a custom analyzer (vs. golangci-lint alone)

The `forbidigo` linter matches identifier USES, which
means a blank or side-effect-only import slips past it.
This is documented as the gap that motivated the custom
analyzer (see iter 1 evaluator feedback item #2).

`depguard` is import-path based and would technically
solve the SQL-import requirement on its own, but the
build-tag-bypass rule has no off-the-shelf equivalent and
the brief explicitly requires a `go vet` analyzer at the
spec-pinned path `tools/buildtaglint/main.go`. Co-locating
both analyzers in one multichecker binary keeps the
operator surface to a single tool to install, document,
and version.

## Extending the analyzers

To add a new rule:

1. Drop a new `<rulename>.go` file alongside `buildtag.go`
   and `sqlimport.go` exporting `<Rule>Analyzer *analysis.Analyzer`.
2. Add it to the `multichecker.Main(...)` call in `main.go`.
3. Add fixture packages under `lint/testdata/src/<scenario>/`.
4. Add a `TestXFixtures` driving the analyzer over them.
5. If the rule needs a narrower scope than the caller's
   package set, gate inside the analyzer (e.g. by file
   path, mirroring `isDevpolicyFile` in `buildtag.go`).

Reference: [`tech-spec.md` §8.10](../../../../docs/stories/code-intelligence-REFACTOR-GUIDE/tech-spec.md) is the source of truth for which rules
this tool enforces.
