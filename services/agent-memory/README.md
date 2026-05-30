# agent-memory

Hybrid graph-memory service for the `code-intelligence:AGENT-MEMORY`
story. Implements a top-down repo / call-chain graph plus episodic
memory layer that downstream agents can recall, observe, and
consolidate against.

This subtree is the **service root**. Every binary, library,
migration, proto, and deploy artifact for the service lives under
this directory and nowhere else.

## Sibling design docs

Authoritative specs for the service live one tree up under
[`docs/stories/code-intelligence-AGENT-MEMORY/`](../../docs/stories/code-intelligence-AGENT-MEMORY/):

- [`architecture.md`](../../docs/stories/code-intelligence-AGENT-MEMORY/architecture.md)
  — system-level architecture (§5 data model, §3.3 ingest pipeline,
  §8.5 transport, §8.6 observability).
- [`tech-spec.md`](../../docs/stories/code-intelligence-AGENT-MEMORY/tech-spec.md)
  — concrete PostgreSQL 16+ DDL (§8.7), role grants (§8.7.4),
  partitioning plan (§8.7.3), and Qdrant collection layout.
- [`implementation-plan.md`](../../docs/stories/code-intelligence-AGENT-MEMORY/implementation-plan.md)
  — phased stage / step / scenario plan that this scaffold realises.
- [`e2e-scenarios.md`](../../docs/stories/code-intelligence-AGENT-MEMORY/e2e-scenarios.md)
  — end-to-end Given/When/Then scenarios that gate the service.

If those docs and this scaffold disagree, the docs win — open a PR
that fixes the code, not one that rewrites the spec.

## Layout

```
services/agent-memory/
├── cmd/              # main packages, one per binary (agent-api, repo-indexer, …)
├── internal/         # service-private libraries (graphwriter, graphreader, …)
├── migrations/       # ordered SQL migrations + the migrate runner test
├── pkg/              # reusable, importable libraries (fingerprint, …)
├── proto/            # protobuf / gRPC service definitions (§8.5 Agent transport)
├── web/              # static assets / mgmt UI bundles, if any
└── deploy/
    └── local/        # docker compose stack for local dev + CI integration tests
```

## Local development

```
cd services/agent-memory
make build      # go build ./...
make test       # go test -count=1 ./... (portable; no -race)
make test-race  # go test -race -count=1 ./... (CGO; Linux CI only)
make test-nocgo # go test -count=1 ./internal/repoindexer/ast/... with CGO_ENABLED=0
make test-cgo   # go test -count=1 ./internal/repoindexer/ast/... with CGO_ENABLED=1
make lint       # golangci-lint run ./...
```

`make test` is the portable target invoked from any developer
laptop. `make test-race` is the same suite with the race detector
on and is what CI runs on Linux runners where CGO is available.

## Building with CGO

The AST parser dispatcher under
[`internal/repoindexer/ast/`](internal/repoindexer/ast/) uses
build tags to split per-language parser registration into two
files (REPO-SCANNER impl-plan Stage 1.1):

| File                | Build tag        | Parsers registered |
| ------------------- | ---------------- | ------------------ |
| `parsers_cgo.go`    | `//go:build cgo` | C, C++, C#, Go, Rust, PowerShell |
| `parsers_nocgo.go`  | `//go:build !cgo`| PowerShell only |

The five tree-sitter-backed parsers
(`parser_treesitter_{c,cpp,csharp,go,rust}.go` and
`parser_treesitter.go` for TypeScript / Python) link against the
[`github.com/smacker/go-tree-sitter`](https://github.com/smacker/go-tree-sitter)
grammars, which are compiled C code. Building or testing those
files therefore requires `CGO_ENABLED=1` **and a C compiler on
PATH**. With `CGO_ENABLED=0`, the dispatcher silently registers
only the PowerShell parser and emits
`ast.dispatch.skip{reason="no_parser"}` for every `.c` / `.cpp` /
`.cs` / `.go` / `.rs` input it sees.

### Required C toolchain

| Host OS         | Compiler   | Recommended install path |
| --------------- | ---------- | ------------------------ |
| Linux           | `gcc`      | `apt-get install -y build-essential` (Debian/Ubuntu) or the distro equivalent (`dnf groupinstall "Development Tools"`, `pacman -S base-devel`, …). Most container base images already ship `gcc`. |
| macOS           | `clang`    | `xcode-select --install` installs the Command Line Tools, which puts `clang` on PATH and is the path used by Homebrew Go builds. |
| Windows         | `gcc.exe`  | A MinGW-W64 distribution (recommended: [MSYS2 `mingw-w64-x86_64-gcc`](https://www.msys2.org/), or the TDM-GCC bundle, or the MinGW-W64 13.x toolchain that ships under `C:\vcpkg\downloads\tools\perl\<ver>\c\bin\` on a vcpkg-managed Strawberry Perl install). After install, ensure the `bin\` directory containing `gcc.exe` is on `PATH` and set `CC=gcc` so cgo picks it up. |

CI mirrors this matrix: the Linux job assumes `gcc` from
`ubuntu-latest`'s default image; if a future runner image
rotation removes it, the
[`.github/workflows/agent-memory-ci.yml`](../../.github/workflows/agent-memory-ci.yml)
workflow's `make test-cgo` step will fail with cgo's
`C compiler "gcc" not found` error.

### Validating the CGO / non-CGO split

Two narrow Makefile targets exercise *only* the parser
dispatcher subtree (`./internal/repoindexer/ast/...`), printing
the active toolchain (`go env CGO_ENABLED CC CXX`) before each
run so the CI log records the build mode:

```
# Exercise the !cgo registration (PowerShell-only fleet).
# Works on any host -- no C compiler required.
make test-nocgo

# Exercise the cgo registration (tree-sitter fleet).
# Requires a C compiler on PATH per the matrix above.
make test-cgo
```

Both targets are wired into the agent-memory CI job so every PR
runs the CGO=1 and CGO=0 build modes back-to-back; a regression
in either parser-registration file therefore fails CI before it
can land on `feature/scanner`. The broader Go suite continues to
run under `make test` / `make test-race` (which default to
whatever CGO mode the runner provides) so this pair of targets
only adds the explicit build-tag split coverage, not redundant
load on the full test matrix.

## Local dependency stack

The local dependency stack (PostgreSQL 16 + `pgcrypto` + `pg_partman`,
Qdrant, OTel Collector) is started with:

```
cd services/agent-memory/deploy/local
docker compose up -d
docker compose ps    # wait for "running (healthy)" on all three rows
```

CI runs the same `docker compose` + `make test` sequence on every PR
opened against this story's workstream branches; see
[`.github/workflows/agent-memory-ci.yml`](../../.github/workflows/agent-memory-ci.yml).
