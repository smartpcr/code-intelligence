# AST Parser for Additional Languages -- End-to-End Scenarios

> Story: `code-intelligence:AST-PARSER-FOR-ADDIT` -- 13 points
> Sibling planning artifacts (drafted in parallel): `architecture.md`,
> `tech-spec.md`, `implementation-plan.md`.
> This file owns: the **operator-visible Gherkin scenarios** QA executes
> against the dispatcher / per-language parser surface. The architecture
> doc owns component / data-model / sequence contracts; the tech spec
> owns extraction-rule tables and threshold values; the implementation
> plan owns file lists, step order, and developer-facing test scenarios
> per stage.
>
> Phases mirror the implementation-plan H1 sections. Each phase has a
> mandatory `### Setup` block (Type + Local + CI runner + Secrets +
> Pre-test bootstrap) directly before `### Scenarios`. Setup `Type` is
> one of the closed set `inline` / `compose` / `lab-wsfc` /
> `lab-bare-metal`. This story is unit-test heavy -- no lab phases --
> but two phases (Phase 1 migration, Phase 7 full service suite) need
> the local compose stack.
>
> All four operator-pinned decisions (`dot-h-extension-routing`,
> `powershell-grammar-strategy`, `go-receiver-pointer-fingerprint`,
> `rust-trait-overrides-edge`) drive the scenario assertions below;
> each reference is annotated with the pin id inline.

---

## 0. How to read this document

### 0.1 Scope

This file is the **executable acceptance surface** for the six new
`LanguageParser` implementations and their dispatcher wiring. Each
scenario corresponds to one or more contracts pinned in the sibling
docs:

- The locked Stage 3.2 contracts (A1 through A7) live in
  `architecture.md` Section 1.2 and `tech-spec.md` Section 3 (C1
  through C12).
- The per-language extraction-rule tables live in `tech-spec.md`
  Section 5 (one subsection per language `5.1` through `5.6`).
- The new additive struct surfaces (`LangMeta`, `ReceiverAliases`)
  and the sentinel `ErrParserUnavailable` live in `architecture.md`
  Section 4.4 / 4.5.1.
- The new dispatcher pass (Pass 2d `overrides`) and the receiver-
  qualified multimap (Pass 2b refactor) live in `architecture.md`
  Section 7.2 and `tech-spec.md` Section 8 R2 / R4.
- The one schema change (`0022_edge_kind_overrides.sql` adds
  `overrides` to the `edge_kind` enum) lives in
  `implementation-plan.md` Stage 1.2 and `tech-spec.md` Section 2.1.

### 0.1.1 Decisions committed (no longer open questions)

Two earlier ambiguities are now committed decisions in the
body of this doc and are listed here so QA knows the questions are
CLOSED:

| Decision id              | Question                                                                                                                                                                  | Committed answer (binding for this story)                                                                                                                                                                                                                                          |
| ---                      | ---                                                                                                                                                                       | ---                                                                                                                                                                                                                                                                                |
| `phase1-compose-path`    | Should Phase 1 reuse `services/agent-memory/deploy/local/docker-compose.yml`, or create a new `tests/e2e/{phase-slug}/docker-compose.yml`, or symlink to the service-local file? | CREATE NEW. Phase 1 owns a standalone postgres-only compose file at `tests/e2e/phase-1-shared-additive-surfaces/docker-compose.yml` whose `postgres` service uses `build: { context: ../../../services/agent-memory/deploy/local/postgres, dockerfile: Dockerfile }` and `image: agent-memory/postgres:16-partman` -- i.e., the SAME `FROM postgres:16` Debian image plus `pg_partman` that the service-local stack produces (the upstream image, build context, and produced tag are defined at `services/agent-memory/deploy/local/postgres/Dockerfile` and `services/agent-memory/deploy/local/docker-compose.yml` lines 22-27). It mounts the init SQL from `services/agent-memory/deploy/local/postgres/init/` to `/docker-entrypoint-initdb.d`, but does NOT `include:` the full service-local compose (avoids dragging in qdrant / otel-collector for a phase that only needs postgres). Recorded in 0.4 and in Phase 1 Setup. |
| `phase6-ci-install-pwsh` | Should this story add a `pwsh` install step to `.github/workflows/agent-memory-ci.yml`, keep the skip posture, or defer to a follow-up?                                  | KEEP SKIP POSTURE. The hosted runner does not install `pwsh` in v1; `@needs-pwsh` scenarios `t.Skip` per `implementation-plan.md` Stage 6.3 lines 417 and 428. Workflow change is the scope of a separate follow-up story (`code-intelligence:CI-INSTALL-PWSH-FOR-AGENT-MEMORY`), not part of this story. Recorded in 0.2 and in Phase 6 Setup. |

### 0.2 Notation conventions

- **Feature / Background / Scenario / Scenario Outline / Examples** --
  standard Gherkin. AND-steps use `And` (not `*`) for diff stability.
- **Tags** (`@happy`, `@edge`, `@regression`, `@cgo-on`, `@cgo-off`,
  `@needs-pwsh`, `@no-pwsh`, `@migration`, `@invariant`) appear
  immediately above each Scenario. CI runs
  `@happy + @edge + @regression + @invariant` on every PR;
  `@cgo-off` runs on the Windows portable matrix leg;
  `@needs-pwsh` is FILTERED on the existing hosted runner because
  `pwsh` is not installed there (per Phase 6 Setup), so those
  scenarios `t.Skip` and contribute zero coverage on
  `agent-memory-ci`. `@needs-pwsh` is exercised only on a developer
  workstation with `pwsh >= 7.0` on PATH (or by a follow-up story
  that adds a pwsh install step to the workflow -- see
  `Decisions committed` in 0.1.1). `@no-pwsh` covers the absent-pwsh
  sentinel skip path and always runs on every leg, hosted runner
  included.
- **Parser ids** (`c`, `cpp`, `csharp`, `go`, `rust`, `powershell`)
  match `LanguageParser.Language()` exactly.
- **Build-tag matrix** is referenced as `@cgo-on` (`CGO_ENABLED=1`)
  vs `@cgo-off` (`CGO_ENABLED=0`). The five tree-sitter parsers
  exist only under `@cgo-on`; PowerShell registers under both per
  `tech-spec.md` Section 4 / Section 5.6.
- **Connection strings** are referenced as environment variable
  names (`AGENT_MEMORY_PG_URL`, `AGENT_MEMORY_QDRANT_URL`,
  `AGENT_MEMORY_OTEL_ENDPOINT`) per the existing CI convention --
  never as literal hostnames or credentials.

### 0.3 Inherited substrate (Background for every Feature)

```gherkin
Background:
  Given the dispatcher under test is constructed via NewDispatcher with the build-tag-resolved defaultParsers() set
  And a fakeWriter implementation of nodeEdgeWriter records every InsertNode and InsertEdge call
  And the EmitFileEvent under test carries RepoURL "https://git.example/acme/lab", RelPath as listed in the Scenario, and Source bytes equal to the embedded fixture
  And the dispatcher logger is captured via slog.NewJSONHandler to a bytes.Buffer for log-key assertions
```

The substrate is overridden only by phases that need additional
process or runtime dependencies (pwsh on PATH for Phase 6, postgres +
qdrant + otel via docker compose for Phase 1 migration + Phase 7
full-service suite).

### 0.4 New e2e compose-file inventory (artifacts this story introduces)

Per the brief's compose convention (`tests/e2e/{phase-slug}/docker-compose.yml`),
this story introduces two new compose files. They are NOT alternatives
to the existing service-local stack at
`services/agent-memory/deploy/local/docker-compose.yml`; that file
remains the source of truth for full-service local development. The
e2e files below are scoped to the AST-PARSER story's QA needs and are
created as part of the Phase 1 / Phase 7 implementation:

| Compose path                                                       | Services started                       | Used by         |
| ---                                                                | ---                                    | ---             |
| `tests/e2e/phase-1-shared-additive-surfaces/docker-compose.yml`    | `postgres` (the partman-enabled image built from `services/agent-memory/deploy/local/postgres/Dockerfile`, tagged `agent-memory/postgres:16-partman`, with init SQL mounted from `services/agent-memory/deploy/local/postgres/init/`) | Phase 1 migration probe; Phase 1 dispatcher unit tests do not need postgres but inherit the same compose to keep `make` lines uniform |
| `tests/e2e/phase-7-cross-cutting-validation/docker-compose.yml`    | `postgres`, `qdrant`, `otel-collector` (overlay over the service-local Dockerfiles) | Phase 7 targeted + CGO-off + full-service validation runs |

Connection-string variables resolved by these compose files
(`AGENT_MEMORY_PG_URL`, `AGENT_MEMORY_QDRANT_URL`,
`AGENT_MEMORY_OTEL_ENDPOINT`) are read by the Go tests through the
`os.Getenv` pattern already used by `internal/recallcontext/log_integration_test.go`
and the agent-memory-ci workflow. Tests skip when the env var is
unset (the existing convention for integration tests in this repo);
the doc never inlines the underlying DSN.

---

# Phase 1: Shared additive surfaces and dispatcher edits

> Implementation-plan anchors: Stages 1.1, 1.2, 1.3, 1.4, 1.5.
> Owns: `LangMeta` / `ReceiverAliases` field additions, the
> `ErrParserUnavailable` sentinel, the `mergeLangMeta` writer helper,
> Pass 2b multimap refactor, Pass 2d overrides emission, schema
> migration `0022_edge_kind_overrides.sql`, and `normalizeHints`
> alias expansion.

### Setup

- **Type**: compose
- **Compose file**: `tests/e2e/phase-1-shared-additive-surfaces/docker-compose.yml` (NEW artifact this story introduces). Services started: `postgres` only -- the partman-enabled image built from `services/agent-memory/deploy/local/postgres/Dockerfile` (`FROM postgres:16` + `postgresql-16-partman`, tagged `agent-memory/postgres:16-partman` to match the service-local stack at `services/agent-memory/deploy/local/docker-compose.yml` line 27), with init SQL mounted from `services/agent-memory/deploy/local/postgres/init/` to `/docker-entrypoint-initdb.d`, exposing port 5432 on the runner's loopback. Qdrant and the OTel collector are NOT started; the Phase 1 scenarios only exercise the AST dispatcher (in-process) and the schema migration journal (postgres only).
- **Local**: From the repo root, run `docker compose -f tests/e2e/phase-1-shared-additive-surfaces/docker-compose.yml up -d` to start postgres. Export `AGENT_MEMORY_PG_URL` to the env-var convention used by the agent-memory test suite (the value is supplied by the compose file's documented mapping; tests read the env var, never a literal DSN). Then run `go test ./internal/repoindexer/ast -count=1 -run 'TestDispatcher|TestLangMeta|TestErrParserUnavailable|TestMergeLangMeta|TestNormalizeHints|TestPass2bMultimap|TestPass2dOverrides'` for the dispatcher unit-test slice and `go test ./migrations -count=1 -run 'TestMigrator_Up_AppliesAll|TestMigrations_0022_EdgeKindOverrides'` for the migration apply / probe test.
- **CI runner**: GitHub-hosted `ubuntu-latest` per `.github/workflows/agent-memory-ci.yml` (the existing `integration-stack` job already starts a postgres container with healthchecks). No new runner pool, labels, or hosted image required.
- **Secrets**: None. The Phase 1 unit + migration tests run against the disposable runner-local postgres started from the compose file. The agent-memory-ci workflow reads `AGENT_MEMORY_PG_URL` / `AGENT_MEMORY_QDRANT_URL` / `AGENT_MEMORY_OTEL_ENDPOINT` as workflow-scoped `env` keys; no KeyVault path and no GitHub environment-scoped secret is referenced because no production credential crosses the boundary.
- **Pre-test bootstrap**: `go mod download` from `services/agent-memory`; then the migration apply happens in-process at test setup via `migrations.New(db).Up(ctx)` (the embedded `migrations` package iterates `0001` through `0022` in lexicographic order against `$AGENT_MEMORY_PG_URL`). The migration probe test asserts the new `0022_edge_kind_overrides.sql` is reachable and that `'overrides'::edge_kind` resolves.

### Scenarios

```gherkin
Feature: Shared additive surfaces and dispatcher edits
  As a parser author about to land six new LanguageParser implementations
  I want the additive surfaces (LangMeta, ReceiverAliases, ErrParserUnavailable),
    the writer attr merge helper, the receiver-qualified multimap, the Pass 2d
    overrides pass, and the new normalizeHints aliases to land first
  So that subsequent per-language phases can drop in without re-litigating the
    dispatcher contract
```

```gherkin
@happy @regression
Scenario: LangMeta nil is a no-op for existing TS / Python parsers
  Given the TypeScript fixture from parser_typescript_test.go is loaded
  When the dispatcher EmitFile runs against "src/hello.ts" with CGO=on
  Then the fakeWriter captures the same 3 class + 5 method + 0 package nodes the pre-change baseline captured
  And the attrs_json on every captured node is byte-identical to the pre-change golden
  And no attrs_json key named "receiver", "receiver_ptr", "trait", "embeds", "module_kind", "cmdlet_verb", "is_static", "blank_import", "dot_import", "base_access", "template_params", "namespace", "partial", "base_raw", "trait_default" appears on any node
```

```gherkin
@happy
Scenario: First-class attr key wins over LangMeta on merge
  Given a stub LanguageParser registered for extension ".stub" whose Parse returns one MethodDecl with LangMeta map "language" -> "bogus"
  When the dispatcher EmitFile runs against "src/x.stub"
  Then the fakeWriter records exactly one method node
  And the captured attrs_json key "language" equals "stub" (the dispatcher's first-class value)
  And the captured attrs_json key "language" does NOT equal "bogus"
```

```gherkin
@happy
Scenario: New LangMeta keys flow through methodAttrs
  Given a stub LanguageParser returns one MethodDecl with LangMeta map "receiver"->"r", "receiver_ptr"->true
  When the dispatcher EmitFile runs
  Then the captured method attrs_json carries "receiver"="r" and "receiver_ptr"=true
  And the merge helper preserves first-class keys per architecture Section 4.4.2
```

```gherkin
@happy
Scenario: ReceiverCalls land in calls_raw
  Given a stub parser emits MethodDecl Calls=["log_global"] and ReceiverCalls=["identify"]
  When the dispatcher EmitFile runs
  Then attrs_json["calls_raw"] decodes to a string slice whose deduplicated set equals {"log_global","identify"}
```

```gherkin
@edge
Scenario: ReceiverCalls alone (no bare Calls) still emit calls_raw
  Given a stub parser emits MethodDecl Calls=nil and ReceiverCalls=["Bar"]
  When the dispatcher EmitFile runs
  Then attrs_json["calls_raw"] decodes to exactly the slice ["Bar"]
  And the methodAttrs writer does NOT gate the merge on len(Calls) > 0
```

```gherkin
@happy @regression
Scenario: ErrParserUnavailable sentinel routes to ast.dispatch.skip
  Given a stub LanguageParser whose Parse returns fmt.Errorf("test: %w (reason=stub_missing)", ErrParserUnavailable)
  And the stub is registered for extension ".stub"
  When the dispatcher EmitFile runs against "src/a.stub"
  Then the structured log buffer contains an entry with key "event"="ast.dispatch.skip" and "reason"="stub_missing"
  And no log entry with "event"="ast.parse.error" is produced
  And the dispatcher return value equals (EmitResult{}, nil)
  And the fakeWriter recorded zero InsertNode and zero InsertEdge calls
```

```gherkin
@edge
Scenario: ErrParserUnavailable without an embedded reason slug defaults
  Given a stub LanguageParser whose Parse returns fmt.Errorf("plain: %w", ErrParserUnavailable)
  And the stub is registered for extension ".plain"
  When the dispatcher EmitFile runs against "src/a.plain"
  Then the structured log carries "event"="ast.dispatch.skip" and "reason"="runtime_unavailable"
```

```gherkin
@happy @invariant
Scenario: Pass 2b multimap drops same-file collisions
  Given a stub Go parser emits two MethodDecls Foo.Bar (value receiver) and *Foo.Bar (pointer receiver) plus a sibling method Foo.Other whose ReceiverCalls=["Bar"]
  And the dispatcher's ReceiverAliases registers Foo.Bar twice (once for the value method, once for the pointer method's alias)
  When the dispatcher EmitFile runs
  Then the fakeWriter records zero edges of kind "static_calls" whose target simple name is "Bar"
  And the verbatim "Bar" persists on attrs_json["calls_raw"] of the calling method
```

```gherkin
@happy
Scenario: Pass 2b multimap resolves pointer-only receiver call
  Given a stub Go parser emits one MethodDecl *Foo.Bar with ReceiverAliases=["Foo.Bar"]
  And another MethodDecl *Foo.Other whose ReceiverCalls=["Bar"]
  When the dispatcher EmitFile runs
  Then exactly one "static_calls" edge with source *Foo.Other and target *Foo.Bar is recorded
  And the resolution path used the ReceiverAliases entry, not a bare-name match
```

```gherkin
@happy @invariant
Scenario: Pass 2d emits one overrides edge for same-file Rust trait shadow
  Given a stub parser emits a trait method Greeter.greet (LangMeta nil) and an impl method GreeterImpl.greet (LangMeta["trait"]="Greeter") in the same file
  When the dispatcher EmitFile runs after Pass 2c completes
  Then exactly one InsertEdge call with kind "overrides", source = GreeterImpl.greet node id, target = Greeter.greet node id is recorded
  And no "overrides" edge has source equal to target (self-loop check)
```

```gherkin
@edge
Scenario: Pass 2d cross-file miss is silent
  Given a stub parser emits an impl method GreeterImpl.greet with LangMeta["trait"]="Greeter"
  And no method Greeter.greet exists in the same file
  When the dispatcher EmitFile runs
  Then zero InsertEdge calls of kind "overrides" are recorded
  And attrs_json["trait"]="Greeter" persists on the impl method node for the future cross-file resolver
```

```gherkin
@migration
Scenario: Migration 0022 appends overrides to edge_kind enum
  Given postgres is reachable at $AGENT_MEMORY_PG_URL with migrations applied through 0021
  When the test harness calls migrations.New(db).Up(ctx) against $AGENT_MEMORY_PG_URL
  Then migration 0022_edge_kind_overrides.sql applies without error
  And `SELECT 'overrides'::edge_kind` returns the string "overrides"
  And `SELECT unnest(enum_range(NULL::edge_kind))` includes "overrides" along with the pre-existing values (contains, imports, static_calls, observed_calls, extends, implements, reads, writes, renamed_to)
```

```gherkin
@migration @regression
Scenario: Migration 0022 is idempotent on the runner re-pass
  Given postgres has migration 0022_edge_kind_overrides.sql applied once via migrations.New(db).Up(ctx)
  When migrations.New(db).Up(ctx) is invoked a second time
  Then 0022 is skipped by filename (no duplicate ALTER TYPE ... ADD VALUE issued)
  And `SELECT count(*) FROM pg_enum WHERE enumlabel='overrides'` returns exactly 1
```

```gherkin
@happy
Scenario Outline: normalizeHints accepts every new language alias
  Given language_hints=[<alias>]
  When normalizeHints runs
  Then the resulting set contains "<canonical>"

  Examples:
    | alias        | canonical  |
    | h            | c          |
    | cxx          | cpp        |
    | c++          | cpp        |
    | hpp          | cpp        |
    | csharp       | csharp     |
    | c#           | csharp     |
    | golang       | go         |
    | rs           | rust       |
    | ps           | powershell |
    | ps1          | powershell |
    | psm1         | powershell |
    | psd1         | powershell |
    | powershell   | powershell |
```

```gherkin
@regression
Scenario: Existing TS / Python hint aliases survive expansion
  Given language_hints=["typescript","python"]
  When normalizeHints runs
  Then the resulting set still contains "typescript" and "python"
```

---

# Phase 2: Go parser

> Implementation-plan anchors: Stages 2.1, 2.2, 2.3.
> Owns: `goTreeSitterParser` (CGO-only), registration in
> `parsers_cgo.go`, and the Go fixture acceptance test asserting
> the pinned `go-receiver-pointer-fingerprint` behaviour.

### Setup

- **Type**: inline
- **Local**: From `services/agent-memory`, run `CGO_ENABLED=1 go test ./internal/repoindexer/ast -count=1 -run 'TestGoFixture|TestParserGo|TestDispatcher_GoMultimap'`. Local toolchain must have a C compiler reachable to satisfy `github.com/smacker/go-tree-sitter/golang` (the `golang` binding is CGO).
- **CI runner**: GitHub-hosted `ubuntu-latest` per `.github/workflows/agent-memory-ci.yml`. The existing job sets `CGO_ENABLED=1` implicitly via setup-go and `gcc` is preinstalled on `ubuntu-latest`. No new runner pool required.
- **Secrets**: None. All scenarios are inline unit tests that run in-process with a fakeWriter; no external services and no credentials are involved.
- **Pre-test bootstrap**: `go mod download` (idempotent; the smacker/go-tree-sitter dependency is already declared in `services/agent-memory/go.mod` for the existing TS / Python tree-sitter parsers).

### Scenarios

```gherkin
Feature: Go AST parser (tree-sitter, CGO-only)
  As a consumer of the structural graph
  I want Go source to emit class / method / package nodes and contains /
    static_calls / imports edges identically to TS / Python
  So that "which method calls formatGreeting?" is answerable for Go repos
```

```gherkin
@happy @cgo-on
Scenario: Canonical Go fixture emits the expected node and edge set
  Given the Go fixture string containing:
    | construct                  | source                                                |
    | type with body             | type Greeter struct { prefix string }                  |
    | pointer-receiver method    | func (g *Greeter) Greet(name string) string { ... }    |
    | free function              | func formatGreeting(prefix, name string) string { ... } |
    | std-lib import             | import "fmt"                                          |
  And the method body of Greet calls formatGreeting(g.prefix, name)
  When the dispatcher EmitFile runs against "src/hello.go" under CGO=on
  Then exactly 1 class node "Greeter" with attrs_json["kind"]="struct" is emitted
  And exactly 2 method nodes (*Greeter.Greet, formatGreeting) are emitted
  And exactly 1 package node for "fmt" is emitted
  And exactly 3 "contains" edges (file->Greeter, Greeter->*Greeter.Greet, file->formatGreeting) are emitted
  And exactly 1 "static_calls" edge from *Greeter.Greet to formatGreeting is emitted
  And exactly 1 "imports" edge from the file to the fmt package is emitted
```

```gherkin
@happy @cgo-on
Scenario: Pointer-receiver method gets * prefix in QualifiedName (pinned go-receiver-pointer-fingerprint)
  Given the Go source `func (r *Foo) Bar(s string) {}`
  When goTreeSitterParser.Parse is invoked directly
  Then the returned MethodDecl has QualifiedName="*Foo.Bar"
  And EnclosingClass="Foo" (stays bare for class attachment)
  And ReceiverAliases=["Foo.Bar"]
  And LangMeta["receiver_ptr"]=true and LangMeta["receiver"]="r"
  And the canonical signature string includes the substring "#*Foo.Bar("
```

```gherkin
@happy @cgo-on
Scenario: Value-receiver method has bare QualifiedName
  Given the Go source `func (r Foo) Bar() {}`
  When goTreeSitterParser.Parse is invoked directly
  Then the returned MethodDecl has QualifiedName="Foo.Bar"
  And ReceiverAliases is nil (no alias needed -- canonical key is already the lookup key)
  And LangMeta["receiver_ptr"]=false
```

```gherkin
@edge @cgo-on
Scenario: Selector-call rightmost identifier lands in Calls
  Given a Go method body `fmt.Println("hi"); strconv.Itoa(1)`
  When the parser walks the body
  Then Calls equals ["Println","Itoa"] (after uniqueStringsInsert dedup)
  And the package qualifier ("fmt", "strconv") is NOT prefixed onto Calls entries
```

```gherkin
@edge @cgo-on
Scenario: Dot import and blank import carry the right LangMeta
  Given the Go source contains `import . "fmt"` and `import _ "image/png"`
  When the parser walks imports
  Then one Import has Module="fmt", Alias="." and LangMeta["dot_import"]=true
  And another Import has Module="image/png", Alias="_" and LangMeta["blank_import"]=true
```

```gherkin
@edge @cgo-on
Scenario: Embedded type names captured in LangMeta["embeds"]
  Given the Go struct `type Server struct { http.Server; logger Logger }`
  When the parser emits the ClassDecl
  Then attrs_json["embeds"] decodes to a slice containing "Server" (the embedded type name; package qualifier dropped per the selector-call convention)
```

```gherkin
@edge @cgo-on
Scenario: Receiver-qualified field write produces a writes edge
  Given the Go method `func (g *Greeter) SetPrefix(p string) { g.prefix = p }`
  When the dispatcher EmitFile runs against "src/setter.go"
  Then exactly one "writes" edge from *Greeter.SetPrefix to a member node with attrs_json["member_kind"]="field" and "name"="prefix" is emitted
  And no "reads" edge is emitted from SetPrefix to the prefix member (the field appears only on the LHS of `=`)
```

```gherkin
@edge @cgo-on
Scenario: `:=` short declaration does NOT count as a write
  Given a Go method body `p := computePrefix()`
  When the parser walks the body
  Then no MemberAccess is recorded for `p` (it is a fresh binding, not a member write)
```

```gherkin
@regression @cgo-on
Scenario: .go file routes to the Go parser
  Given the dispatcher constructed via defaultParsers() under CGO=on
  When selectParser("foo.go", nil) is invoked
  Then the returned parser's Language() equals "go"
```

```gherkin
@regression @cgo-off
Scenario: .go file is skipped under CGO=off
  Given the dispatcher constructed via defaultParsers() under CGO=off
  When the dispatcher EmitFile runs against "src/foo.go" with valid Go source
  Then the structured log carries "event"="ast.dispatch.skip" and "reason"="no_parser"
  And no InsertNode and no InsertEdge calls are recorded
  And the returned EmitResult equals (EmitResult{}, nil)
```

---

# Phase 3: C and C++ parsers

> Implementation-plan anchors: Stages 3.1, 3.2, 3.3, 3.4, 3.5.
> Owns: `cTreeSitterParser`, `cppTreeSitterParser`, joint
> registration, the `.h` pinning rule, and the C++
> declaration-vs-definition dedupe pass.

### Setup

- **Type**: inline
- **Local**: From `services/agent-memory`, run `CGO_ENABLED=1 go test ./internal/repoindexer/ast -count=1 -run 'TestCFixture|TestCppFixture|TestParserC|TestParserCpp|TestDispatcher_DotH'`. C / C++ tree-sitter bindings (`github.com/smacker/go-tree-sitter/c` and `.../cpp`) are CGO-only.
- **CI runner**: GitHub-hosted `ubuntu-latest` -- `gcc` is preinstalled, smacker's CGO bindings link cleanly. No new runner pool required.
- **Secrets**: None. All scenarios are in-process unit tests against the fakeWriter.
- **Pre-test bootstrap**: `go mod download` (smacker's `c` and `cpp` packages add no new top-level dependencies beyond the existing `golang` / `python` / `typescript` modules; same vendor tree).

### Scenarios

```gherkin
Feature: C and C++ AST parsers
  As a consumer of the structural graph
  I want C structs / functions / includes and C++ classes / namespaces /
    inheritance / overloads to emit the same node and edge kinds the
    existing TS / Python parsers do
  So that the graph index covers the C/C++ slice of the monorepo
```

```gherkin
@happy @cgo-on
Scenario: C fixture emits the expected node and edge set
  Given a C source string with:
    | construct        | source                                                  |
    | struct           | struct Greeter { int n; };                              |
    | free function 1  | int greet(int n) { return format_greeting(n); }         |
    | free function 2  | int format_greeting(int n) { return n + 1; }            |
    | system include   | #include <stdio.h>                                      |
    | relative include | #include "local.h"                                      |
  When the dispatcher EmitFile runs against "src/hello.c" under CGO=on
  Then exactly 1 class node "Greeter" with attrs_json["kind"]="struct" is emitted
  And exactly 2 method nodes (greet, format_greeting) are emitted
  And exactly 1 package node for "stdio.h" is emitted
  And exactly 3 "contains" edges (file->Greeter, file->greet, file->format_greeting) are emitted
  And exactly 1 "static_calls" edge from greet to format_greeting is emitted
  And exactly 1 "imports" edge from the file to the stdio.h package is emitted
  And zero "imports" edges target a package whose Module starts with "./" (the local.h include is dropped by isRelativeImport)
```

```gherkin
@edge @cgo-on
Scenario: Anonymous C struct is skipped
  Given the C source `struct { int x; } anon_inst;`
  When cTreeSitterParser.Parse is invoked directly
  Then ParseResult.Classes is empty
```

```gherkin
@edge @cgo-on
Scenario: Function prototype declarations (no body) are skipped
  Given the C source `int helper(int);` followed by `int helper(int n) { return n; }`
  When cTreeSitterParser.Parse is invoked directly
  Then ParseResult.Methods contains exactly one entry named "helper"
  And that entry has non-empty BodySource
```

```gherkin
@edge @cgo-on
Scenario: Function-pointer calls are NOT collected
  Given the C source `void run(void (*cb)(int)) { (*cb)(42); }`
  When cTreeSitterParser.Parse is invoked
  Then the MethodDecl for `run` has Calls=[] (the parenthesised callee is not a callable identifier in v1)
```

```gherkin
@happy @cgo-on
Scenario: C++ class with same-file public base and in-class method
  Given the C++ source `class Base { public: void identify() {} }; class Greeter : public Base { public: void greet() { this->identify(); log_global(); } }; void log_global() {}`
  When the dispatcher EmitFile runs against "src/hello.cpp" under CGO=on
  Then exactly 2 class nodes (Base, Greeter) are emitted
  And exactly 3 method nodes (Base.identify, Greeter.greet, log_global) are emitted
  And exactly 1 "extends" edge from Greeter to Base is emitted
  And exactly 1 "static_calls" edge from Greeter.greet to log_global is emitted
  And zero "static_calls" edges target Base.identify (the this->identify call misses methodNodeID["Greeter.identify"] per A4 same-file rule)
  And attrs_json["calls_raw"] on Greeter.greet contains the verbatim string "identify"
  And the Greeter class attrs_json["base_access"]["Base"]="public"
```

```gherkin
@invariant @cgo-on
Scenario: In-class declaration plus out-of-line definition collapse to one method (R1)
  Given the C++ source `class Foo { void bar(); }; void Foo::bar() { log_global(); } void log_global() {}`
  When the dispatcher EmitFile runs
  Then ParseResult.Methods contains exactly one entry "Foo.bar"
  And that entry's BodySource is non-empty (definition wins over declaration)
  And exactly one "static_calls" edge from Foo.bar to log_global is emitted
```

```gherkin
@edge @cgo-on
Scenario: C++ template parameters captured in LangMeta
  Given the C++ source `template<typename T, int N> class Buffer { T data[N]; };`
  When cppTreeSitterParser.Parse is invoked
  Then the Buffer ClassDecl has LangMeta["template_params"] decoding to ["T","N"]
  And the canonical signature uses the bare class name "Buffer" (NO template arguments)
```

```gherkin
@edge @cgo-on
Scenario: Namespace path joined with `.` in QualifiedName
  Given the C++ source `namespace acme { namespace lab { class Widget {}; } }`
  When cppTreeSitterParser.Parse is invoked
  Then the Widget ClassDecl has QualifiedName="acme.lab.Widget"
  And LangMeta["namespace"]="acme.lab"
```

```gherkin
@edge @cgo-on
Scenario: Operator overloads are dropped from Calls
  Given the C++ method body `*this = other;`
  When cppTreeSitterParser.Parse is invoked
  Then the enclosing MethodDecl's Calls slice does NOT contain "operator=" or "operator"
```

```gherkin
@regression @cgo-on
Scenario Outline: Extension routing for C and C++
  Given the dispatcher constructed via defaultParsers() under CGO=on
  When selectParser("<file>", nil) is invoked
  Then the returned parser's Language() equals "<lang>"

  Examples:
    | file        | lang |
    | foo.c       | c    |
    | foo.h       | c    |
    | foo.cc      | cpp  |
    | foo.cpp     | cpp  |
    | foo.cxx     | cpp  |
    | foo.c++     | cpp  |
    | foo.hpp     | cpp  |
    | foo.hh      | cpp  |
    | foo.hxx     | cpp  |
    | foo.h++     | cpp  |
```

```gherkin
@regression @cgo-on
Scenario: `.h` routes to C even when LanguageHints=["cpp"] (pinned dot-h-extension-routing)
  Given the dispatcher constructed via defaultParsers() under CGO=on
  When selectParser("widget.h", []string{"cpp"}) is invoked
  Then the returned parser's Language() equals "c"
  And no "cpp" alias in the hint list re-routes the file
```

```gherkin
@regression @cgo-off
Scenario: .c / .cpp files are skipped under CGO=off
  Given the dispatcher constructed via defaultParsers() under CGO=off
  When the dispatcher EmitFile runs against "src/foo.c" with valid C source
  And again against "src/foo.cpp" with valid C++ source
  Then both EmitFile calls log "event"="ast.dispatch.skip" with "reason"="no_parser"
  And zero Nodes or Edges are inserted
```

---

# Phase 4: C# parser

> Implementation-plan anchors: Stages 4.1, 4.2, 4.3.
> Owns: `csharpTreeSitterParser` plus the parser-time same-file
> two-pass base-list partition (R11). The dispatcher's Pass 2a
> code path stays unchanged.

### Setup

- **Type**: inline
- **Local**: From `services/agent-memory`, run `CGO_ENABLED=1 go test ./internal/repoindexer/ast -count=1 -run 'TestCSharpFixture|TestCSharp_BaseList|TestParserCSharp'`. The smacker `csharp` binding is CGO-only.
- **CI runner**: GitHub-hosted `ubuntu-latest`. No new runner pool required.
- **Secrets**: None. Inline unit tests only.
- **Pre-test bootstrap**: `go mod download`.

### Scenarios

```gherkin
Feature: C# AST parser
  As a consumer of the structural graph
  I want C# classes / interfaces / structs / records to emit the right
    extends vs implements edges even when the base list mixes both kinds
  So that "what does HelloWorld implement?" returns IGreeter without
    a cross-file resolver
```

```gherkin
@happy @cgo-on
Scenario: C# fixture emits the expected node and edge set
  Given the C# source:
    """
    using System;
    namespace Demo;
    interface IGreeter { string Greet(string name); }
    class Base { public string Identify() => "base"; }
    class HelloWorld : Base, IGreeter {
      private string prefix = "hi";
      public string Greet(string name) { return FormatGreeting(this.prefix, name); }
      private static string FormatGreeting(string prefix, string name) => prefix + name;
    }
    """
  When the dispatcher EmitFile runs against "src/hello.cs" under CGO=on
  Then exactly 3 class/interface nodes (IGreeter, Base, HelloWorld) are emitted
  And exactly 4 method nodes (IGreeter.Greet, Base.Identify, HelloWorld.Greet, HelloWorld.FormatGreeting) are emitted
  And exactly 1 package node for "System" is emitted
  And exactly 1 "extends" edge from HelloWorld to Base is emitted
  And exactly 1 "implements" edge from HelloWorld to IGreeter is emitted
  And exactly 1 "static_calls" edge from HelloWorld.Greet to HelloWorld.FormatGreeting is emitted
  And exactly 1 "imports" edge from the file to the System package is emitted
  And every emitted node's attrs_json["namespace"] equals "Demo"
```

```gherkin
@happy @invariant @cgo-on
Scenario Outline: C# base-list partition decision matrix (R11)
  Given the source contains "<source>"
  When csharpTreeSitterParser.Parse is invoked
  Then the Foo ClassDecl has Extends=<extends> and Implements=<implements>
  And LangMeta["base_raw"] equals <base_raw>

  Examples:
    | source                                                | extends     | implements    | base_raw            |
    | class Bar {} class Foo : Bar {}                       | ["Bar"]     | []            | ["Bar"]             |
    | interface IBar {} class Foo : IBar {}                 | []          | ["IBar"]      | ["IBar"]            |
    | class Bar {} interface IBaz {} class Foo : Bar, IBaz {}| ["Bar"]    | ["IBaz"]      | ["Bar","IBaz"]      |
    | interface IBaz {} interface IQux {} class Foo : IBaz, IQux {} | [] | ["IBaz","IQux"] | ["IBaz","IQux"]   |
    | class Foo : Bar {}                                    | ["Bar"]     | []            | ["Bar"]             |
    | interface IBaz {} class Foo : Bar, IBaz {}            | ["Bar"]     | ["IBaz"]      | ["Bar","IBaz"]      |
```

```gherkin
@edge @cgo-on
Scenario: Cross-file extends drops at dispatcher per C4
  Given the C# source `class Foo : ExternalBase {}` with no same-file `class ExternalBase`
  When the dispatcher EmitFile runs against "src/foo.cs"
  Then Foo's ClassDecl has Extends=["ExternalBase"]
  And zero "extends" edges are inserted (the dispatcher's Pass 2a drops the edge on classNodeID miss)
  And attrs_json["extends_raw"] on the Foo class node contains "ExternalBase"
  And attrs_json["base_raw"] on the Foo class node contains "ExternalBase"
```

```gherkin
@edge @cgo-on
Scenario: Partial class flag set on each fragment
  Given the C# source `partial class Foo { public void A() {} }`
  When csharpTreeSitterParser.Parse is invoked
  Then the Foo ClassDecl has LangMeta["partial"]=true
  And v1 emits ONE ClassDecl per fragment (no cross-file partial unification per architecture Section 2.2)
```

```gherkin
@edge @cgo-on
Scenario: Using directive variants emit the right Import
  Given the C# source contains:
    | directive                              | expected Module      | expected attrs              |
    | using System;                          | "System"             | none                        |
    | using static System.Math;              | "System.Math"        | LangMeta["is_static"]=true  |
    | using IO = System.IO;                  | "System.IO"          | Alias="IO"                  |
  When csharpTreeSitterParser.Parse is invoked
  Then one Import is emitted per row with the listed Module, Alias, and LangMeta entries
```

```gherkin
@edge @cgo-on
Scenario: this-qualified call lands in ReceiverCalls
  Given a C# method body `return this.FormatGreeting(name);`
  When csharpTreeSitterParser.Parse is invoked
  Then the enclosing MethodDecl's ReceiverCalls contains "FormatGreeting"
  And bare-name Calls does NOT contain "FormatGreeting"
```

```gherkin
@edge @cgo-on
Scenario: Property accessors are NOT emitted as methods
  Given the C# source `class Foo { public string Name { get; set; } }`
  When csharpTreeSitterParser.Parse is invoked
  Then ParseResult.Methods is empty (the accessor_declaration nodes are skipped)
  And "Name" is recorded internally for MemberAccess resolution
```

```gherkin
@regression @cgo-on
Scenario: .cs and .csx route to C#
  Given the dispatcher constructed via defaultParsers() under CGO=on
  When selectParser is called for "foo.cs" then "foo.csx"
  Then both return a parser whose Language() equals "csharp"
```

```gherkin
@regression @cgo-off
Scenario: .cs file is skipped under CGO=off
  Given the dispatcher constructed via defaultParsers() under CGO=off
  When the dispatcher EmitFile runs against "src/foo.cs"
  Then the structured log carries "event"="ast.dispatch.skip" and "reason"="no_parser"
  And no Nodes / Edges are inserted
```

---

# Phase 5: Rust parser

> Implementation-plan anchors: Stages 5.1, 5.2, 5.3.
> Owns: `rustTreeSitterParser`, registration, and the end-to-end
> Pass 2d `overrides` emission for the trait-default shadowing case
> (pinned `rust-trait-overrides-edge`).

### Setup

- **Type**: inline
- **Local**: From `services/agent-memory`, run `CGO_ENABLED=1 go test ./internal/repoindexer/ast -count=1 -run 'TestRustFixture|TestParserRust|TestDispatcher_Rust_TraitOverrides'`. The smacker `rust` binding is CGO-only.
- **CI runner**: GitHub-hosted `ubuntu-latest`. No new runner pool required.
- **Secrets**: None. Inline unit tests only; the `overrides` edge kind requires migration 0022 from Phase 1 to be applied if any persisted-graph round-trip is asserted, but the fakeWriter scenarios in this phase do not touch postgres.
- **Pre-test bootstrap**: `go mod download`. Phase 1's migration 0022 must already be applied to `$AGENT_MEMORY_PG_URL` if the persistence sub-scenario tagged `@migration` is exercised against a live postgres; the fakeWriter scenarios skip that requirement.

### Scenarios

```gherkin
Feature: Rust AST parser and Pass 2d overrides emission
  As a consumer of the structural graph
  I want Rust traits, impl blocks, and trait-default shadowing to emit
    typed extends / implements / overrides edges
  So that "which trait does GreeterImpl implement and which default does
    it shadow?" is answerable directly from the graph
```

```gherkin
@happy @cgo-on
Scenario: Rust fixture emits the expected node and edge set
  Given the Rust source:
    """
    use std::fmt::Display;
    trait Greeter {
      fn greet(&self, name: &str) -> String { String::new() }
    }
    struct GreeterImpl;
    impl Greeter for GreeterImpl {
      fn greet(&self, name: &str) -> String { format_greeting(name) }
    }
    pub fn format_greeting(name: &str) -> String { String::from(name) }
    """
  When the dispatcher EmitFile runs against "src/hello.rs" under CGO=on
  Then exactly 2 class nodes (Greeter trait kind="trait", GreeterImpl struct kind="struct") are emitted
  And exactly 3 method nodes (Greeter.greet, GreeterImpl.greet, format_greeting) are emitted
  And exactly 1 package node for "std::fmt" is emitted
  And exactly 1 "implements" edge from GreeterImpl to Greeter is emitted
  And exactly 1 "static_calls" edge from GreeterImpl.greet to format_greeting is emitted
  And exactly 1 "imports" edge from the file to the std::fmt package is emitted
  And the std::fmt package node attrs_json["symbols"] decodes to ["Display"]
```

```gherkin
@happy @invariant @cgo-on
Scenario: Pass 2d emits one overrides edge for the same-file trait default shadow
  Given the Rust fixture from the previous scenario
  When the dispatcher emit method runs Pass 2d after Pass 2c
  Then exactly one InsertEdge call with kind "overrides", source = GreeterImpl.greet node id, target = Greeter.greet node id is recorded
  And the GreeterImpl.greet method's attrs_json["trait"]="Greeter" persists for downstream consumers
  And LangMeta["trait_default"]=true is recorded on Greeter.greet
```

```gherkin
@edge @cgo-on
Scenario: Cross-file trait reference does NOT emit an overrides edge
  Given the Rust source `struct GreeterImpl; impl Greeter for GreeterImpl { fn greet(&self) -> String { String::new() } }` with NO trait declaration in the same file
  When the dispatcher EmitFile runs
  Then zero "overrides" edges are emitted
  And the GreeterImpl.greet method's attrs_json["trait"]="Greeter" still persists
  And no "ast.parse.error" log is produced (the cross-file miss is silent per A4)
```

```gherkin
@edge @cgo-on
Scenario: Supertrait clause populates Extends
  Given the Rust source `trait A {} trait B: A {}`
  When rustTreeSitterParser.Parse is invoked
  Then the B trait ClassDecl has Extends=["A"]
  And the dispatcher emits exactly 1 "extends" edge from B to A
```

```gherkin
@edge @cgo-on
Scenario: use list expands to multiple Imports, alias and glob round-trip
  Given the Rust source contains:
    | use stmt                                | expected Module          | expected Symbols           | expected Alias |
    | use std::fmt::Display;                  | "std::fmt"               | ["Display"]                | ""             |
    | use std::collections::{HashMap,HashSet};| "std::collections"       | ["HashMap","HashSet"]      | ""             |
    | use serde_json::Value as JsonValue;     | "serde_json"             | ["Value"]                  | "JsonValue"    |
    | use std::io::*;                         | "std::io"                | ["*"]                      | ""             |
  When rustTreeSitterParser.Parse is invoked
  Then one Import is emitted per source row matching the columns above
```

```gherkin
@edge @cgo-on
Scenario: self-qualified call lands in ReceiverCalls; non-self method call dropped
  Given the Rust method body `self.identify(); other.transform();`
  When rustTreeSitterParser.Parse is invoked
  Then the enclosing MethodDecl's ReceiverCalls contains "identify"
  And ReceiverCalls does NOT contain "transform" (non-self receiver is dropped in v1; the receiver type is not statically known)
  And the verbatim "transform" does NOT appear on Calls either (method-call exprs with non-self receivers are dropped, not coerced to bare names)
```

```gherkin
@edge @cgo-on
Scenario: macro invocations are NOT collected as Calls
  Given the Rust method body `println!("hi"); vec![1, 2, 3];`
  When rustTreeSitterParser.Parse is invoked
  Then the enclosing MethodDecl's Calls slice is empty
  And no Import is emitted for "println" or "vec"
```

```gherkin
@regression @cgo-on
Scenario: .rs routes to Rust
  Given the dispatcher constructed via defaultParsers() under CGO=on
  When selectParser("foo.rs", nil) is invoked
  Then the returned parser's Language() equals "rust"
```

```gherkin
@regression @cgo-off
Scenario: .rs file is skipped under CGO=off
  Given the dispatcher constructed via defaultParsers() under CGO=off
  When the dispatcher EmitFile runs against "src/foo.rs" with valid Rust source
  Then the structured log carries "event"="ast.dispatch.skip" and "reason"="no_parser"
  And no Nodes / Edges are inserted
```

---

# Phase 6: PowerShell parser

> Implementation-plan anchors: Stages 6.1, 6.2, 6.3.
> Owns: the `powershellParser` subprocess implementation (no build
> tags), the `ErrParserUnavailable` skip path when `pwsh` is absent,
> and the 10-second per-file timeout. Pinned by
> `powershell-grammar-strategy` -- subprocess to `pwsh` only,
> NO tree-sitter binding.

### Setup

- **Type**: inline
- **Local**: From `services/agent-memory`, ensure `pwsh` is on PATH (`pwsh --version` returns >= 7.0). Then run `go test ./internal/repoindexer/ast -count=1 -run 'TestPowerShellFixture|TestPowerShellParser|TestParserPowerShell'` under either `CGO_ENABLED=0` or `CGO_ENABLED=1`. When `pwsh` is absent the fixture tests call `t.Skip` and remain green; the sentinel scenario (`TestPowerShellParser_NoPwsh_ReturnsSentinel`) forces `pwshBin=""` and runs unconditionally.
- **CI runner**: GitHub-hosted `ubuntu-latest` per the existing `.github/workflows/agent-memory-ci.yml`. The workflow does NOT install `pwsh` today (no apt step for `powershell` exists in the `lint-build-test`, `pre-commit`, or `integration-stack` jobs at lines 39-258 of that workflow). Per `implementation-plan.md` Stage 6.3 lines 417, 428, the canonical CI posture for v1 is "PowerShell fixture tests `t.Skip` when `pwsh` is not on PATH", which keeps the hosted runner green without adding a pwsh install step. This story COMMITS TO that skip-posture (recorded in 0.1.1 Decisions committed); adding a pwsh install step to the workflow is explicitly OUT OF SCOPE and is the subject of follow-up story `code-intelligence:CI-INSTALL-PWSH-FOR-AGENT-MEMORY` (to be filed separately, not an open question on this story).
- **Secrets**: None. The subprocess invocation passes source bytes on stdin and consumes JSON on stdout; no credentials cross the process boundary.
- **Pre-test bootstrap**: `go mod download`. On a developer workstation with `pwsh` present, run `pwsh -NoProfile -NonInteractive -Command '$PSVersionTable.PSVersion'` to confirm runtime availability before invoking `go test`. Tests that depend on `pwsh` carry `@needs-pwsh`; tests that prove the absent-pwsh skip path carry `@no-pwsh`. On the agent-memory-ci hosted runner the `@needs-pwsh` scenarios skip and the `@no-pwsh` sentinel + dispatcher-skip scenarios provide the only Phase 6 CI coverage; this is the committed v1 posture (see 0.1.1) and is not a gap that this story will close.

### Scenarios

```gherkin
Feature: PowerShell AST parser via pwsh subprocess
  As a consumer of the structural graph
  I want PowerShell scripts to emit class / method / package nodes via
    the official System.Management.Automation.Language API
  So that classes, functions, and Import-Module statements anchor the
    graph for PS-heavy repos -- and the dispatcher logs a graceful skip
    when pwsh is unavailable instead of failing the worker
```

```gherkin
@happy @needs-pwsh
Scenario: PowerShell fixture emits the expected node and edge set
  Given pwsh is on PATH (exec.LookPath("pwsh") succeeds)
  And the PowerShell source:
    """
    Import-Module Foo
    class Greeter {
      [string] $Prefix
      [string] Format([string]$name) { return "$($this.Prefix) $name" }
      [string] Greet([string]$name) { return $this.Format($name) }
    }
    function Format-Hello { param([string]$Name) return "hi $Name" }
    """
  When the dispatcher EmitFile runs against "src/hello.ps1"
  Then exactly 1 class node "Greeter" with attrs_json["kind"]="class" is emitted
  And exactly 3 method nodes (Greeter.Format, Greeter.Greet, Format-Hello) are emitted
  And exactly 1 package node for "Foo" is emitted
  And exactly 1 "imports" edge from the file to the Foo package is emitted
  And exactly 1 "static_calls" edge from Greeter.Greet to Greeter.Format is emitted (via the $this.Format(...) receiver-qualified path)
  And the Foo Import attrs_json["module_kind"]="Import-Module" and attrs_json["cmdlet_verb"]="Import"
```

```gherkin
@edge @needs-pwsh
Scenario: using module emits a typed Import
  Given pwsh is on PATH
  And the PowerShell source contains `using module BuildHelpers`
  When parser_powershell.Parse is invoked
  Then one Import has Module="BuildHelpers" and LangMeta["module_kind"]="using_module" and LangMeta["cmdlet_verb"]="using"
```

```gherkin
@edge @needs-pwsh
Scenario: Dot-source of a relative file produces an Import that the dispatcher drops
  Given pwsh is on PATH
  And the PowerShell source `. ./helpers.ps1`
  When the dispatcher EmitFile runs against "src/load.ps1"
  Then the parser emits one Import with Module="./helpers.ps1" and LangMeta["module_kind"]="dot_source"
  And the dispatcher's isRelativeImport drops the edge (zero "imports" edges target a package whose Module starts with "./")
```

```gherkin
@happy @no-pwsh
Scenario: Sentinel return when pwsh is missing
  Given a powershellParser constructed with pwshBin=""
  When Parse is invoked against any non-empty source
  Then the returned error wraps ErrParserUnavailable (errors.Is returns true)
  And the wrapped message contains the substring "pwsh_not_available"
```

```gherkin
@happy @regression @no-pwsh
Scenario: pwsh-missing host logs skip, not error, for .ps1 files
  Given the dispatcher constructed via defaultParsers() and a powershellParser with pwshBin=""
  When the dispatcher EmitFile runs against "src/loader.ps1" with valid PS source
  Then the structured log carries "event"="ast.dispatch.skip" and "reason"="pwsh_not_available"
  And NO log entry with "event"="ast.parse.error" is produced
  And the dispatcher returns (EmitResult{}, nil)
  And the fakeWriter records zero InsertNode / InsertEdge calls
```

```gherkin
@edge @needs-pwsh
Scenario: pwsh subprocess timeout surfaces as parse.error (NOT sentinel)
  Given a powershellParser whose pwshBin points to a script that sleeps for 30 seconds
  And the per-file timeout is 10 seconds
  When Parse is invoked
  Then the returned error is non-nil
  And errors.Is(err, ErrParserUnavailable) is false (timeouts are NOT sentinel-class)
  And the dispatcher's safeParse / EmitFile logs "event"="ast.parse.error" (NOT ast.dispatch.skip)
```

```gherkin
@edge @needs-pwsh
Scenario: pwsh non-zero exit surfaces as parse.error
  Given a powershellParser whose pwshBin points to a script that exits with code 1 after printing garbage on stdout
  When Parse is invoked
  Then the returned error is non-nil
  And errors.Is(err, ErrParserUnavailable) is false
  And the dispatcher logs "event"="ast.parse.error"
```

```gherkin
@regression @needs-pwsh
Scenario Outline: PowerShell extensions route to the parser under both build tags
  Given the dispatcher constructed via defaultParsers() under <cgo>
  When selectParser("<file>", nil) is invoked
  Then the returned parser's Language() equals "powershell"

  Examples:
    | file        | cgo     |
    | mod.ps1     | CGO=on  |
    | mod.psm1    | CGO=on  |
    | mod.psd1    | CGO=on  |
    | mod.ps1     | CGO=off |
    | mod.psm1    | CGO=off |
    | mod.psd1    | CGO=off |
```

```gherkin
@edge @needs-pwsh
Scenario: $this.X(...) inside class method resolves to ReceiverCalls
  Given pwsh is on PATH
  And the PowerShell source contains the class method body `return $this.Format($name)`
  When parser_powershell.Parse is invoked
  Then the enclosing MethodDecl's ReceiverCalls contains "Format"
  And the bare-name Calls slice does NOT contain "Format" (the receiver-qualified path supersedes bare-name collection per A5)
```

```gherkin
@edge @needs-pwsh
Scenario: Top-level function emits no modifiers; class method emits static / hidden when present
  Given the PowerShell source:
    """
    function Format-Top { return 1 }
    class C { static [int] Add([int]$a, [int]$b) { return $a + $b } hidden [void] H() {} }
    """
  When parser_powershell.Parse is invoked
  Then the Format-Top MethodDecl's Modifiers slice is empty
  And the C.Add MethodDecl's Modifiers contains "static"
  And the C.H MethodDecl's Modifiers contains "hidden"
```

---

# Phase 7: Cross-cutting tests, documentation, validation

> Implementation-plan anchors: Stages 7.1, 7.2, 7.3.
> Owns: cross-language dispatcher routing tests, the
> `.claude/context/tests.md` support-matrix update, and the targeted
> + full-service validation runs that gate the PR.

### Setup

- **Type**: compose
- **Compose file**: `tests/e2e/phase-7-cross-cutting-validation/docker-compose.yml` (NEW artifact this story introduces). Services started: `postgres`, `qdrant`, and `otel-collector` (the same three the existing `services/agent-memory/deploy/local/docker-compose.yml` brings up; the Phase 7 compose file is a thin overlay that points its build context at the service-local Dockerfiles so the e2e tree owns the new file path without forking the image definitions). Ports 5432 (postgres), 6333 (qdrant), and 4317 (otel gRPC) bind to runner loopback.
- **Local**: From the repo root, run `docker compose -f tests/e2e/phase-7-cross-cutting-validation/docker-compose.yml up -d` to bring up the three services. Export `AGENT_MEMORY_PG_URL`, `AGENT_MEMORY_QDRANT_URL`, `AGENT_MEMORY_OTEL_ENDPOINT` via the env-var convention already used by `.github/workflows/agent-memory-ci.yml` (the workflow keys these in its job `env`; local developers can `source` a `.envrc` that mirrors the same keys, never inlining the literal DSNs in the doc or in shell history). Then run, in order: `CGO_ENABLED=1 go test ./internal/repoindexer/ast -count=1` (targeted), `CGO_ENABLED=0 go test ./internal/repoindexer/ast -count=1` (CGO-off portable matrix), `CGO_ENABLED=1 go test ./... -count=1` (full service suite), and `make lint`.
- **CI runner**: GitHub-hosted `ubuntu-latest` per `.github/workflows/agent-memory-ci.yml`. The existing `integration-stack` job already runs the compose stack + the lint/build/test triple. This story adds an additional matrix leg with `CGO_ENABLED=0` to regression-test the no_parser skip; the workflow file edit for that matrix leg is part of Phase 7's implementation. No new runner pool, labels, or hosted image required.
- **Secrets**: None for the in-repo validation. The runner-local postgres / qdrant / otel containers use credentials baked into the compose file's service definitions and never persist beyond the job container. The agent-memory-ci workflow does NOT consume any KeyVault path or GitHub environment-scoped secret for this story.
- **Pre-test bootstrap**: `go mod download`; then the integration-class tests apply migrations in-process via `migrations.New(db).Up(ctx)` from the embedded `migrations` package against `$AGENT_MEMORY_PG_URL` (the `0022_edge_kind_overrides.sql` file from Phase 1 ships in the embed). On a developer workstation with `pwsh` present, `pwsh --version` confirms Phase 6's runtime; on the hosted runner the PS fixture skips per Phase 6 above. The runner also primes the qdrant collection via `cmd/qdrant-bootstrap` if any persistence-touching tests in `./...` require the collection (covered by the existing integration-stack scripts).

### Scenarios

```gherkin
Feature: Cross-cutting dispatcher routing, support matrix, and validation gate
  As the agent-memory release manager
  I want every new extension to route to the correct parser, the docs to
    document the support matrix, and the targeted + full-service test
    suites to pass under both CGO matrix legs
  So that the PR ships a feature-complete additive change with no
    coverage regression on the existing TS / Python paths
```

```gherkin
@regression @cgo-on
Scenario: Every new extension routes to its parser under CGO=on
  Given the dispatcher constructed via defaultParsers() under CGO=on (and a host with pwsh on PATH)
  When selectParser is called for each of the extensions below
  Then the returned parser's Language() equals the documented id

  Examples:
    | file        | language   |
    | x.c         | c          |
    | x.h         | c          |
    | x.cc        | cpp        |
    | x.cpp       | cpp        |
    | x.cxx       | cpp        |
    | x.c++       | cpp        |
    | x.hpp       | cpp        |
    | x.hh        | cpp        |
    | x.hxx       | cpp        |
    | x.cs        | csharp     |
    | x.csx       | csharp     |
    | x.go        | go         |
    | x.rs        | rust       |
    | x.ps1       | powershell |
    | x.psm1      | powershell |
    | x.psd1      | powershell |
    | x.ts        | typescript |
    | x.tsx       | typescript |
    | x.py        | python     |
```

```gherkin
@regression
Scenario: Unknown extension returns nil parser and dispatches to skip
  Given the dispatcher constructed via defaultParsers()
  When selectParser("foo.unknown", nil) is invoked
  Then the returned LanguageParser is nil
  When the dispatcher EmitFile runs against "src/foo.unknown" with arbitrary bytes
  Then the structured log carries "event"="ast.dispatch.skip" and "reason"="no_parser"
  And the fakeWriter records zero InsertNode / InsertEdge calls
```

```gherkin
@regression @invariant
Scenario: Duplicate-extension registration is deterministic last-wins
  Given two stub parsers parserA and parserB both registered with Extensions()=[".go"] via WithParsers(parserA, parserB)
  When selectParser("x.go", nil) is invoked
  Then the returned parser is parserB (the later registration wins)
  And no panic, no error, and no log entry are produced by the registration itself
  And the dispatcher contract documented in dispatcher.go buildExtMap is preserved
```

```gherkin
@regression @cgo-on
Scenario: Multimap collision drops end-to-end on the real Go parser
  Given a Go fixture file `src/multi.go` containing both `func (r Foo) Bar() {}`, `func (r *Foo) Bar() {}`, and a third method `func (r Foo) Caller() { r.Bar() }`
  When the dispatcher EmitFile runs under CGO=on
  Then zero "static_calls" edges target either Foo.Bar or *Foo.Bar from Foo.Caller
  And attrs_json["calls_raw"] on Foo.Caller contains the verbatim string "Bar"
```

```gherkin
@regression @cgo-on
Scenario: Multimap pointer-only resolution end-to-end
  Given a Go fixture file `src/ptronly.go` with only `func (r *Foo) Bar() {}` and a sibling `func (r *Foo) Caller() { r.Bar() }`
  When the dispatcher EmitFile runs under CGO=on
  Then exactly one "static_calls" edge with source = *Foo.Caller and target = *Foo.Bar is emitted
  And the resolution route used the ReceiverAliases entry "Foo.Bar"
```

```gherkin
@regression
Scenario: First-class attr key cannot be overridden via LangMeta
  Given a stub parser populating LangMeta["language"]="bogus"
  When the dispatcher EmitFile runs against an extension registered to that stub
  Then the persisted attrs_json["language"] equals the parser's Language() return value (NOT "bogus")
  And the merge helper's first-class key wins rule (C11) is preserved across class, method, and import attrs writers
```

```gherkin
@regression
Scenario: tests.md ships an authoritative support matrix
  Given the edited .claude/context/tests.md file
  When `grep -nF "Language | CGO=on" .claude/context/tests.md` runs
  Then it matches at least one non-empty line (the header of the new support-matrix table)
  And `grep -nF "pwsh_not_available" .claude/context/tests.md` matches at least one line (the PowerShell skip documentation)
  And `grep -nF "no_parser" .claude/context/tests.md` matches at least one line (the C / C++ / C# / Go / Rust CGO=off skip documentation)
  And `grep -nF ".h" .claude/context/tests.md` matches a row documenting the unconditional `.h` -> C routing rule
```

```gherkin
@regression @cgo-on
Scenario: Targeted AST tests pass under CGO=on
  Given the local stack is up and migrations are applied
  When `CGO_ENABLED=1 go test ./internal/repoindexer/ast -count=1` runs from services/agent-memory
  Then the exit code is 0
  And the test output reports PASS for TestGoFixture_EmitsExpectedNodeAndEdgeSet, TestCFixture_EmitsExpectedNodeAndEdgeSet, TestCppFixture_EmitsExpectedNodeAndEdgeSet, TestCSharpFixture_EmitsExpectedNodeAndEdgeSet, TestRustFixture_EmitsExpectedNodeAndEdgeSet, TestDispatcher_RoutesByExtension, TestDispatcher_DotHRoutesToC_EvenWithCppHint, TestDispatcher_ErrParserUnavailable_LogsSkip, TestDispatcher_GoMultimapDropsOnReceiverCollision, TestDispatcher_GoMultimapResolvesPointerReceiverAlone, TestDispatcher_LangMetaMergePreservesFirstClassKeys
  And the pre-existing TestTypeScriptFixture_EmitsExpectedNodeAndEdgeSet and TestPythonFixture_EmitsExpectedNodeAndEdgeSet continue to PASS unchanged
```

```gherkin
@regression @cgo-off
Scenario: Targeted AST tests pass under CGO=off
  Given the local stack is up and migrations are applied
  When `CGO_ENABLED=0 go test ./internal/repoindexer/ast -count=1` runs from services/agent-memory
  Then the exit code is 0
  And the tree-sitter parser tests (TestGoFixture, TestCFixture, TestCppFixture, TestCSharpFixture, TestRustFixture) are excluded by build tags (reported as "no tests to run" or filtered out)
  And the dispatcher skip-path tests (one per .c / .cpp / .cs / .go / .rs file) confirm "event"="ast.dispatch.skip" with "reason"="no_parser"
  And the PowerShell fixture either runs (pwsh on PATH) or t.Skip's (pwsh absent)
```

```gherkin
@regression @cgo-on
Scenario: Full agent-memory service suite passes under CGO=on
  Given the local stack is up and migrations are applied (through 0022_edge_kind_overrides)
  When `CGO_ENABLED=1 go test ./... -count=1` runs from services/agent-memory
  Then the exit code is 0
  And no test in the broader service suite (graphwriter, repoindexer, agentapi, ...) regresses on the new LangMeta / ReceiverAliases fields (they default to nil for TS / Python and existing fixtures stay green)
```

```gherkin
@regression
Scenario: make lint stays clean across the additive surface
  Given the changes for Phases 1-6 are applied
  When `make lint` runs from services/agent-memory
  Then the exit code is 0
  And no golangci-lint finding cites the new parser_treesitter_{c,cpp,csharp,go,rust}.go or parser_powershell.go files
```

```gherkin
@invariant
Scenario: ASCII-clean docs and no secret literals in CI
  Given the edited tree at HEAD
  When `grep -Pn '[^\x00-\x7E]' docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/e2e-scenarios.md` runs
  Then the command produces zero matches (the e2e doc stays ASCII-only)
  And the connection-string references in scenarios use only the env-var names $AGENT_MEMORY_PG_URL, $AGENT_MEMORY_QDRANT_URL, $AGENT_MEMORY_OTEL_ENDPOINT (no literal hostnames or credentials)
```

---

## Maintenance notes (for future plan updates)

This file establishes the seven-phase scenario set mirroring
`implementation-plan.md`'s phase H1 layout. Future plan updates
should:

- Treat new operator pinnings (if any surface) as additive: append a
  scenario under the relevant phase that exercises the pinned answer,
  and cross-reference the pin id.
- Keep phase ordering aligned with the implementation plan -- a phase
  rename in `implementation-plan.md` requires a parallel rename here.
- Resist adding any phase whose Setup `Type` falls outside the closed
  set (`inline`, `compose`, `lab-wsfc`, `lab-bare-metal`). This story
  is unit-test heavy by design; no lab phase is justified.
