# Repo Scanner -- Tech Spec

> Story: `code-intelligence:REPO-SCANNER` -- Points 21
> Iteration scope (this file, iter 1): **problem framing only** --
> the problem statement, in/out-of-scope items, explicit non-goals,
> hard constraints, and identified risks. Field-level data shapes,
> CLI flag catalogues, HTTP request/response schemas, env-var
> matrices, log-event taxonomies, and rotation policies are deferred
> to later iterations of this same file (the forward-looking section
> anchors S2-S10 in S8 below mark the slots the architecture and
> sibling docs already cite).
>
> Sibling artifacts (parallel; do not duplicate their content):
>
> - `architecture.md` -- component map, data flows, public types,
>   sequence diagrams, pinned defaults table.
> - `implementation-plan.md` -- stage order, branch list, test
>   plan, parity-test catalogue.
> - `e2e-scenarios.md` -- input/output walk-throughs and golden
>   assertions for the four primary scenarios (A scan-local,
>   B scan-many, C module diagram, D call-chain diagram).
>
> Anchoring rule: every Go package, type, table, migration, env
> var, and shell command this document names already exists at the
> path quoted unless the section explicitly flags it **NEW**.

---

## 1. Problem Statement

### 1.1 What the user cannot do today

A developer trying to understand an unfamiliar repository on the
agent-memory stack today must:

1. Stand up the **full production back end**:
   `cmd/repoindexer/main.go` requires `AGENT_MEMORY_PG_URL` AND
   `AGENT_MEMORY_QDRANT_URL` and `os.Exit(3)`s on a bad ping at
   `services/agent-memory/cmd/repoindexer/main.go` startup
   (env catalogue lines 22-39 of the same file). No Postgres +
   no Qdrant = no scan.
2. **Run an embedder** -- the dispatcher's
   `WithEmbeddingPublisher` option is wired in production via
   `cmd/repoindexer/main.go`; the only fallback is the
   zero-vector stub that requires
   `AGENT_MEMORY_ALLOW_STUB_EMBEDDER=true`. Either way, an
   embedder must be configured before a worker starts.
3. **Enqueue a job through `mgmt-api`** -- the worker is a
   long-running queue consumer (`POST /v1/repos` register,
   `POST /v1/repos/{id}/ingest` enqueue, then poll
   `ingest_jobs` via `SELECT ... FOR UPDATE SKIP LOCKED`).
   There is no `--repo <url>` flag on the worker; it takes
   whatever the mgmt-api enqueued.
4. **Hope CGO is on in the binary they pulled** -- the polyglot
   parser set under
   `services/agent-memory/internal/repoindexer/ast/` registers
   the five tree-sitter parsers (C, C++, C#, Go, Rust) only when
   built with `CGO_ENABLED=1` (`parsers_cgo.go`); the CGO=0
   build (`parsers_nocgo.go`) registers PowerShell alone.
5. **Read the structural graph by hand** in Postgres -- there
   is no rendering surface in the repo. The frontend directory
   `services/agent-memory/web/` contains only a `.gitkeep`.

Concretely: the story description (Section 2 "Current-State
Assessment") inventories the exact gap; this doc adopts the
same five-item gap framing as the problem statement.

### 1.2 What the user MUST be able to do after this story

The story description's Section 1 ("Goals") is the source of
truth. Restated as a contract:

1. **One binary, many repos.** A single `codeintel` binary
   accepts either a local path OR a `git URL @ sha`, scans it,
   and persists the structural graph -- without first standing
   up Postgres, Qdrant, or an embedder.
2. **Code intelligence preserved.** The persisted graph carries
   the full structural ancestry (`repo -> package -> file ->
   class -> method -> block`) and the static relationships the
   existing AST dispatcher already produces (`contains`,
   `imports`, `extends`, `implements`, `static_calls`, `reads`,
   `writes`, `overrides`).
3. **Full polyglot coverage.** All eight languages parse:
   TypeScript / JavaScript, Python (the v1 pair) PLUS C, C++,
   C#, Go, Rust, PowerShell (the AST-PARSER-FOR-ADDIT closure).
4. **Two diagram families, one envelope.** A top-down
   `module` diagram (containment + rolled-up `imports`) and a
   left-right `callchain` diagram (BFS over
   `static_calls` / `observed_calls`) -- both returned as the
   same JSON envelope so a single UI parser handles both.
5. **React + neo4j-nvl renderer.** The web app under
   `services/agent-memory/web/` (greenfield today) renders the
   two diagram families using `@neo4j-nvl/react` +
   `@neo4j-nvl/base`, with interactive re-seeding of the
   call-chain view.

The success bar for each item is enumerated in `architecture.md`
Section 8 ("Acceptance Criteria"); this doc does NOT redefine
those criteria.

### 1.3 Why "scanner" and not "queue worker v2"

The queue worker (`cmd/repoindexer`) is the right shape for
**continuous, multi-tenant production ingestion** -- back-pressure
via `ingest_jobs`, atomic claim via `FOR UPDATE SKIP LOCKED`,
G5 append-only + tombstone via `internal/retirement`, durable
write via `internal/graphwriter`. None of those properties is
load-bearing for a developer who wants to look at one repo on
their laptop. The story carves out a **second front door** for
the same dispatcher: synchronous, single-binary, snapshot store
by default. The dispatcher itself, the parsers, the
fingerprint helpers, and the materialization layer are reused
unchanged in body (see `architecture.md` S3.5).

---

## 2. In-Scope (this story)

The five capability groups below correspond 1:1 to the story
description's Section 1 goals. Each entry names the seam the
implementation adds, the existing package it builds on, and the
sibling doc that owns the deeper detail.

### 2.1 `codeintel` CLI binary

- **Path**: `services/agent-memory/cmd/codeintel/` (**NEW**).
- **Subcommands**: `scan`, `scan-many`, `diagram module`,
  `diagram calls`, `serve` (the five-subcommand surface the
  pinned-default S9.3 in `architecture.md` justifies switching
  from stdlib `flag` to `github.com/spf13/cobra`).
- **Drives**: `repoindexer.Materializer` -> NEW
  `repoindexer.AncestryWriter` -> `ast.Dispatcher.EmitFile` ->
  NEW `graphsink.Sink`. No queue, no
  `FOR UPDATE SKIP LOCKED`, no `ingest_jobs` row.
- **Per-flag semantics, default values, and config precedence**
  are catalogued in S2 (this file, future iteration).

### 2.2 `graphsink` storage abstraction

- **Path**: `services/agent-memory/internal/graphsink/`
  (**NEW**), with three sub-packages
  `graphsink/postgres`, `graphsink/sqlite`, `graphsink/memory`.
- **Interface**: a strict superset of the existing
  `ast.NodeEdgeWriter`
  (`services/agent-memory/internal/repoindexer/ast/dispatcher.go`
  line 42) that adds `EnsureRepo`, `EnsureCommit`, `Flush`,
  `Close`. `*graphwriter.Writer` satisfies the
  `Sink` extension natively via the Postgres adapter (which
  forwards 1:1 to the existing writer, plus a new
  `EnsureRepoWithID` variant catalogued in `architecture.md`
  S3.4 for the deterministic-`RepoID` parity story).
- **Backend matrix**: Postgres (production-identical, opt-in
  via `--store=postgres`), SQLite (default for CLI), memory +
  JSON export (one-shot, CI smoke). The pinned default is
  `architecture.md` S9.4 (one `.db` file per repo).
- **Field-level schema, CHECK-constraint catalogue for the
  SQLite mirror of the `node_kind` / `edge_kind` ENUMs, and the
  memory-backend JSON-export key ordering** belong to S5 (this
  file, future iteration).

### 2.3 `LocalDirMaterializer`

- **Path**: extension of
  `services/agent-memory/internal/repoindexer/materialize.go`
  (**NEW** type in the existing file).
- **Why**: today only `GitMaterializer` (production) and
  `InMemoryMaterializer` (test-only) satisfy
  `repoindexer.Materializer`. The CLI must scan an already-checked-out
  directory without a `git fetch`.
- **Reuses**: `defaultExcludeDirs` (`.git`, `.hg`, `.svn`,
  `node_modules`, `vendor`, `target`, `bin`, `obj`,
  `__pycache__`, `.venv`, `.tox` -- see `materialize.go`
  lines 137-141) and the same `WalkFile`-yielding
  `Workspace.Walk` contract.
- **Identity synthesis** for the synthesised `repo.url` /
  `repo.sha` (local path -> `file://...`, mtime-tree hash for
  non-git checkouts per pinned default `architecture.md`
  S9.1) is catalogued at S3 (this file, future iteration).

### 2.4 `diagram` projector + serve endpoint

- **Path**: `services/agent-memory/internal/diagram/`
  (**NEW**); HTTP surface under `cmd/codeintel serve`
  (**NEW**) and/or `internal/mgmtapi` handlers (**NEW**).
- **Builders**:
  `BuildModuleDiagram(reader, repoID, granularity)` and
  `BuildCallChain(reader, seed, depth, direction)`.
- **Read surface**: a new `graphsink.Reader` sub-interface
  (catalogued in `architecture.md` S5.4) whose four
  signature-identical methods (`ListNodes`, `ListEdgesFrom`,
  `ListEdgesTo`, `GetNode`) mirror
  `*graphreader.Reader` (`services/agent-memory/internal/graphreader/reader.go`
  -- `MaxListLimit = 10_000` at line 55 is the inherited
  truncation policy). The fifth method `LookupBySignature`
  is a convenience for `--seed <sig>` resolution.
- **Routes, query parameters, status codes, CORS preflight**
  belong to S6 (this file, future iteration).

### 2.5 React + neo4j-nvl web app

- **Path**: `services/agent-memory/web/` (**NEW**; only a
  `.gitkeep` exists today). Vite + React + TypeScript.
- **Top-level deps**: `@neo4j-nvl/react`, `@neo4j-nvl/base`,
  plus the standard `react`, `react-dom`, `typescript`, `vite`.
- **Components**: `<RepoPicker>`, `<DiagramSwitch>`,
  `<NvlCanvas>`, `<Inspector>`, `<CallChainNav>` (see
  `architecture.md` S3.8 for responsibilities).
- **Color/size policy, layout-direction mapping, truncation
  badge copy** belong to S7 (this file, future iteration).

---

## 3. Out-of-Scope and Non-Goals

Two distinct lists. **Out-of-scope** items are valuable, related,
and likely to land in a follow-up story; **non-goals** are
intentional refusals that the success bar of this story does NOT
move.

### 3.1 Out-of-scope (deferred to follow-up workstreams)

| ID | Item | Why deferred | Tracking pointer |
| --- | --- | --- | --- |
| O1 | Cross-file call / inheritance resolution beyond the current AGENT-MEMORY A4 "same-file resolution only" constraint. | Resolver is a separate workstream (the AGENT-MEMORY story tracks it). This story renders what the dispatcher already emits; it does NOT extend the resolver. | A4 in `docs/stories/code-intelligence-AGENT-MEMORY/architecture.md`. |
| O2 | Runtime / dynamic call edges beyond what `cmd/span-ingestor` already produces. | The dispatcher does not observe runtime; the call-chain diagram traverses both `static_calls` and `observed_calls` but does NOT generate new `observed_calls` edges from scratch. | Span Ingestor story. |
| O3 | Production-grade auth, CSRF, rate-limit on the web UI or the serve endpoint. | The serve endpoint is a **developer tool** intended for `localhost`. Wider exposure is a separate hardening workstream. | `architecture.md` S7.4. |
| O4 | Cursor-based pagination of diagram payloads above `MaxListLimit = 10_000`. | The truncated badge (`truncated`, `stats.cappedAt` in the envelope) is the v1 contract; per-page navigation belongs to a follow-up UX story. | `architecture.md` S7.3. |
| O5 | Multi-repo SQLite layout (single `.db` with a `repo_id` column). | One file per repo per pinned default `architecture.md` S9.4. The single-file layout would add a `repo_id` predicate to every read path the Postgres backend does not need. | `architecture.md` S9.4 (rejected alternative cell). |
| O6 | OTel spans (`codeintel.scan`) for the CLI path. | The CLI is sync and single-process; tracing is not load-bearing. The queue worker continues to emit `repoindexer.process_job` spans unchanged. | `architecture.md` S5.8 / S7.5. |
| O7 | Cross-machine binary distribution / release pipeline. | Out of scope for the planning artifact; the build instructions in `implementation-plan.md` cover local builds only. | `implementation-plan.md` S2 (future). |
| O8 | A `--store=postgres` "delta + retirement" code path driven from the CLI. | The CLI does NOT trigger the delta handler; operators who want delta semantics in production continue to enqueue an `ingest_jobs` row. The CLI's Postgres path is "another mode that inserts the same Nodes and Edges", not a queue substitute (see `architecture.md` S6.5). | `architecture.md` S6.5. |
| O9 | A `serve` mode that points at Postgres alongside SQLite simultaneously, multiplexing across backends. | Each `serve` invocation owns one backend (`--store=...`). Multi-backend serve is a future enhancement. | `architecture.md` S3.7. |

### 3.2 Non-goals (intentional refusals)

Lifted verbatim from the story description's Section 1, "Non-goal
for this story":

| ID | Non-goal | Anchoring text from the story description |
| --- | --- | --- |
| NG1 | Cross-file call / inheritance resolution that breaks the inherited A4 "same-file resolution only" constraint. | "the inherited A4 'same-file resolution only' constraint stays" |
| NG2 | Runtime / dynamic call edges beyond what the span ingestor already produces. | "runtime/dynamic call edges beyond what the span ingestor already produces" |
| NG3 | Production-grade auth on the new UI. | "production-grade auth on the new UI" |

NG1 and O1 are intentionally different framings of the same
constraint: NG1 is the contract refusal (do not add a resolver
to this story); O1 is the trackable follow-up promise (a resolver
is welcome in a future story). The same applies to NG2 / O2 and
NG3 / O3.

---

## 4. Hard Constraints

These are non-negotiable. Each constraint cites the file / line /
migration that enforces it; if a future change in this story
appears to violate one, the change is wrong.

### 4.1 C1 -- CGO is required for full polyglot coverage

- **Enforcement**: `services/agent-memory/internal/repoindexer/ast/parsers_cgo.go`
  (`//go:build cgo`) registers
  `NewTreeSitterCParser`, `NewTreeSitterCppParser`,
  `NewTreeSitterCSharpParser`, `NewTreeSitterGoParser`,
  `NewTreeSitterRustParser`, `NewPowerShellParser`;
  `parsers_nocgo.go` (`//go:build !cgo`) registers
  `NewPowerShellParser` alone.
- **What this means**: a stock Windows build without
  `CGO_ENABLED=1` and without a C toolchain (mingw / gcc /
  clang) is a **PowerShell-only build**. `.c`, `.cpp`, `.cs`,
  `.go`, `.rs` files are skipped with
  `ast.dispatch.skip{reason:"no_parser"}`.
- **Constraint on this story**: the CLI MUST NOT silently
  degrade. The scan summary (catalogued at S10 of this file,
  future iteration) prints every `no_parser` skip aggregated by
  extension so the operator notices the gap. Operator-visible
  build instructions (in `implementation-plan.md` S2.1, future)
  carry the CGO + toolchain prerequisites.

### 4.2 C2 -- PowerShell parser depends on `pwsh` on PATH

- **Enforcement**:
  `services/agent-memory/internal/repoindexer/ast/parser_powershell.go`
  returns `ErrParserUnavailable` with the embedded reason slug
  `pwsh_not_available` when the subprocess fails to start.
- **Constraint on this story**: the dispatcher converts the
  parser-unavailable signal into a standard
  `ast.dispatch.skip{reason:"pwsh_not_available"}` event; the
  CLI summary aggregates and reports it. The exit code stays
  zero in coverage-degraded scans (see C7).

### 4.3 C3 -- Same-file resolution only (AGENT-MEMORY A4)

- **Enforcement**: `docs/stories/code-intelligence-AGENT-MEMORY/architecture.md`
  G6/A4. The dispatcher emits a `static_calls` edge ONLY when
  the parser can resolve the callee within the same file's AST
  (it falls back to `attrs.calls_raw` for unresolved calls).
- **Constraint on this story**: the call-chain BFS will look
  sparse across files in many languages -- this is by design,
  not a bug. UI copy (S7 of this file, future iteration) sets
  this expectation; the Inspector exposes `attrs.calls_raw` so
  the user can see what the parser saw.

### 4.4 C4 -- Fingerprint identity is the single source of truth

- **Enforcement**: `services/agent-memory/pkg/fingerprint`
  (`fingerprint.NodeFingerprint` lines 195-259,
  `fingerprint.EdgeFingerprint` lines 261-319). G2 dedupe key
  is `(repo_id, fingerprint)` on both `node` and `edge`.
- **Constraint on this story**: all three `graphsink` backends
  MUST compute every NodeFingerprint / EdgeFingerprint by
  calling into `pkg/fingerprint` -- never by re-implementing
  the SHA pre-image. The `RepoID` that enters every pre-image
  is itself a pure function of the repo URL via the **NEW**
  helper `fingerprint.RepoIDFromURL(URL)` (catalogued in
  `architecture.md` S3.4). A backend-parity golden test
  (catalogued in `implementation-plan.md` S6) compares
  `(repo_id, fingerprint)` tuples between a SQLite scan and a
  Postgres scan of the same fixture into a clean schema;
  drift = fail.

### 4.5 C5 -- Postgres backend MUST NOT bypass `graphwriter`

- **Enforcement**: G5 append-only + tombstone invariants live
  in `services/agent-memory/internal/graphwriter` and the
  `agent_memory_app` role grants in
  `services/agent-memory/migrations/0016_roles_grants.sql`
  (which prohibit UPDATE on `node` / `edge`).
- **Constraint on this story**: the Postgres `graphsink`
  adapter (`internal/graphsink/postgres/`) is a thin forwarder
  to `*graphwriter.Writer` -- it MUST NOT issue direct SQL,
  MUST surface `WriteContractViolation` (SQLSTATE 42501) to
  the user, and MUST preserve the `auditDefer` structured-log
  shape (`graphwriter.<op>` / `graphwriter.<op>.failed`) per
  insert. Direct SQL inside the adapter is a lint violation.

### 4.6 C6 -- `MaxListLimit = 10_000` clamps every list read

- **Enforcement**: `MaxListLimit` constant in
  `services/agent-memory/internal/graphreader/reader.go`
  line 55; `effectiveLimit` clamps any user request above the
  cap (lines 67-68).
- **Constraint on this story**: the diagram projector inherits
  the clamp on each `ListNodes` / `ListEdgesFrom` /
  `ListEdgesTo` call AND applies a **diagram-level cap**
  (default same value) on `len(nodes) + len(edges)`. When
  either trips, the envelope MUST set `truncated = true` and
  populate `stats.cappedAt`. Cursor-based pagination of
  diagram payloads above the cap is O4 (deferred).

### 4.7 C7 -- Coverage degradation MUST be loud, never silent

- **Enforcement**: the dispatcher already emits
  `ast.dispatch.skip` events for every `no_parser` /
  `pwsh_not_available` file. The CLI MUST aggregate and print
  them.
- **Constraint on this story**: the exit code of a
  coverage-degraded scan is `0` -- the operator decides whether
  to install missing toolchains. Non-zero exit codes are
  reserved for fatal errors (config error, IO error, write
  contract violation, manifest parse error). The exact exit
  code matrix is catalogued at S4.3 of this file (future
  iteration).

### 4.8 C8 -- Embeddings stay opt-in

- **Enforcement**: today the dispatcher's
  `WithEmbeddingPublisher` option is the sole hook into
  `internal/embedding`. The `cmd/repoindexer/main.go` worker
  unconditionally wires it because production needs recall;
  the CLI MUST NOT.
- **Constraint on this story**: the default CLI invocation
  constructs the dispatcher **without** `WithEmbeddingPublisher`
  so `AGENT_MEMORY_QDRANT_URL` and the embedder env vars are
  not required. The opt-in is `--with-embeddings`, which the
  user combines with the relevant env vars and (in CGO=0 mode)
  the stub-embedder allow-flag.

### 4.9 C9 -- The CLI MUST NOT shortcut the pre-walk ancestry

- **Enforcement**: the queue worker's `runFull` method
  (`services/agent-memory/internal/repoindexer/worker.go`
  lines 1084-1219) currently owns a load-bearing pre-`EmitFile`
  sequence: `EnsureRepo` -> `EnsureCommit` -> `InsertNode(kind=repo)`
  -> per-file `package` Node on cache-miss -> `file` Node ->
  two `contains` Edges. Without these five Nodes plus the
  edges, the module diagram has no hierarchy to walk.
- **Constraint on this story**: the CLI MUST drive the
  identical sequence via the **NEW** `AncestryWriter` type
  factored from `worker.runFull` (catalogued in
  `architecture.md` S3.4). The canonical-signature helpers
  (`canonicalRepoSig`, `canonicalPackageDir`,
  `canonicalPackageSig`, `canonicalFileSig`, currently
  unexported in `worker.go` lines 1222-1259) are promoted to
  exported form so the CLI and the worker hash to byte-identical
  Node fingerprints. The refactor is part of the story.

### 4.10 C10 -- SQLite + memory backends are scan SNAPSHOTS, not delta stores

- **Enforcement**: G5 append-only / tombstone semantics belong
  to Postgres alone (per the AGENT-MEMORY architecture). The
  CLI's SQLite and memory backends do NOT create
  `node_retirement` / `edge_retirement` tables (migration
  `0004` is Postgres-only).
- **Constraint on this story**: re-scanning a repo on the
  SQLite backend opens a fresh `.db` file (the prior file is
  rotated aside per the policy at S8.2 of this file, future
  iteration). Re-scanning on the memory backend is a fresh
  in-process scan with a fresh JSON export. Delta in place is
  intentionally NOT supported on the snapshot backends; users
  who want delta continue to use `--store=postgres` against
  the production schema.

---

## 5. Identified Risks

The story description's Section 7 ("Risks & Constraints") is the
seed list. This file restates each, attaches the mitigation owner
(this story vs. follow-up), names the constraint that backs it
(C1-C10 above), and adds R8-R11 surfaced by reading the
codebase under the same problem framing.

| ID | Risk | Constraint backing | Mitigation in this story | Owned-by-follow-up tail |
| --- | --- | --- | --- | --- |
| R1 | CGO toolchain absent on the build host -> CGO=0 build registers PowerShell-only -> a "scan everything" command quietly drops `.c/.cpp/.cs/.go/.rs`. | C1, C7 | CLI summary aggregates `no_parser` skips by extension and prints them at the end of every scan (S10 future). Build instructions in `implementation-plan.md` S2.1 (future) flag the CGO + toolchain prereq. | Prebuilt CGO release binaries (O7 deferred). |
| R2 | `pwsh` missing from PATH -> PowerShell parser returns `ErrParserUnavailable` -> `.ps1/.psm1` files silently skipped. | C2, C7 | CLI summary aggregates `pwsh_not_available` skips and reports them. Documented in `implementation-plan.md` (future). | Bundling `pwsh` is out of scope. |
| R3 | Same-file resolution only (A4) -> call-chain diagrams look sparse / disconnected across files. | C3 | UI copy sets the expectation (S7 future). Inspector exposes `attrs.calls_raw` so the user can see what the parser saw before resolver fallback. | Cross-file resolver is O1 (deferred). |
| R4 | `MaxListLimit = 10_000` truncates large repos silently if the projector forgets to honour the cap. | C6 | Diagram envelope carries `truncated` + `stats.cappedAt`; the UI renders a badge. Backend-parity golden test exercises an over-cap fixture (`implementation-plan.md` S6). | Cursor-based pagination is O4 (deferred). |
| R5 | Backends diverge on fingerprint computation -> a repo scanned to SQLite and replayed to Postgres splits identity. | C4 | All three backends route through `pkg/fingerprint`; the `RepoID` itself is derived deterministically from URL via the **NEW** `fingerprint.RepoIDFromURL` helper (`architecture.md` S3.4). Backend-parity golden test compares `(repo_id, fingerprint)` tuples between SQLite and Postgres on a fresh schema (`implementation-plan.md` S6). | Migrating mgmt-api / queue worker to also call `RepoIDFromURL` so legacy Postgres repos converge (documented parity gap; out of scope here per O8). |
| R6 | `--store=memory` blows RAM on a multi-GB monorepo. | C10 | CLI default is `--store=sqlite` so the memory mode is opt-in. Memory mode is documented as "one-shot / small repos / CI smoke" in `implementation-plan.md` S4 (future). | None -- this is a documented capability bound. |
| R7 | Postgres backend bypasses the `graphwriter` audit log / append-only contract. | C5 | The Postgres backend is a thin adapter that delegates to `*graphwriter.Writer`; direct SQL in the adapter package is a CI lint violation. The interface boundary (`architecture.md` S3.2.2) is the enforcement seam. | None -- this is a permanent contract. |
| R8 | The CLI's serve endpoint is exposed beyond `localhost` (a developer flips the bind address) and leaks the structural graph of internal code. | C8-adjacent (UI/auth out of scope per NG3) | Default bind is `:8088`; documentation in `implementation-plan.md` (future) calls out the "developer tool, localhost only" trust boundary. The endpoint is read-only -- no write surface to abuse. | Auth / CORS hardening is O3 (deferred). |
| R9 | Pinned cobra dependency drifts past a security advisory unnoticed. | None directly -- ecosystem hygiene | The Go module version pin lives in `services/agent-memory/go.mod`; the existing Dependabot / `make lint` posture in this repo applies to the new dep without bespoke wiring. | None -- inherits the org's existing dependency-update flow. |
| R10 | A non-git local scan's synthesised `sha` (mtime-tree hash, pinned default `architecture.md` S9.1) is not stable across filesystems that touch mtimes during checkout (CI cache restores, robocopy). | C9-adjacent (repo identity) | Operator override via `--sha <value>` is the escape hatch; documented in S3 of this file (future). The git case is unaffected (uses `git rev-parse HEAD`). | None -- the override is the contract. |
| R11 | The SQLite backend's snapshot semantic (C10) surprises a user who expected delta on re-scan and sees their old `.db` rotated away. | C10 | Rotation policy is catalogued at S8.2 (future) -- the prior file is moved aside with a timestamp suffix, not deleted; CLI prints the rotation path. | Delta-on-SQLite is out of scope; users wanting delta use `--store=postgres`. |
| R12 | The `RepoInput.RepoID` extension required by C4 / R5 breaks existing mgmt-api / queue-worker callers that do not set the new field. | C4 | The new field is **optional** (zero-valued falls through to the legacy `gen_random_uuid()` path in `EnsureRepo`); only the new `EnsureRepoWithID` variant supplies it. Existing call sites compile unchanged. Documented in `architecture.md` S3.4. | A follow-up story migrates mgmt-api / queue worker to set the field so legacy repos converge on the deterministic-ID identity (O8-adjacent). |

Risks R1-R7 are inherited from the story description's
Section 7; R8-R12 are surfaced by this iteration of the tech-spec
after reading the relevant source files. None of R8-R12 changes
the success bar of the story.

---

## 6. Acceptance Criteria (pointer)

The acceptance criteria for this story are catalogued in
`architecture.md` Section 8 ("Acceptance Criteria") and broken
down by capability group:

- 8.1 Parser coverage -- backed by C1, C2, C7.
- 8.2 CLI scanning -- backed by C5, C8, C9, C10.
- 8.3 Diagram generation -- backed by C3, C4, C6.
- 8.4 React + neo4j-nvl UI -- backed by NG3 (no auth) and the
  diagram envelope from `architecture.md` S4.4.
- 8.5 Docs.

This document does NOT re-state the criteria; it MUST stay
consistent with the framing the architecture owns. If a
constraint here would prevent a criterion there from being met,
the constraint wins and the criterion is the bug.

---

## 7. Open Questions Resolved by Pinned Defaults

The architecture's S9 ("Pinned Defaults") table pins five
operator-answered decisions on 2026-05-30. This tech spec
inherits all five without renegotiation:

| Pinned default | Where the architecture pins it | Constraint this tech-spec carries |
| --- | --- | --- |
| Local-scan `sha` is a deterministic mtime-tree hash; operator may override via `--sha`. | `architecture.md` S9.1, S4.3 | R10, S3 (future). |
| UI transport supports both static-JSON and `codeintel serve`. | `architecture.md` S9.2, S3.7 | Out-of-scope item O3 (auth) does not change; serve is opt-in. |
| CLI framework is `github.com/spf13/cobra`. | `architecture.md` S9.3, S3.1 | R9 (dep update flow). |
| Multi-repo SQLite layout is one `.db` per repo. | `architecture.md` S9.4, S3.2.3, S6.2 | Out-of-scope item O5; S5 (future) per-file schema. |
| Module-diagram default granularity is `package`, with drill-down to `file` / `class`. | `architecture.md` S9.5, S3.6, S4.4 | S6 (future) `granularity` query parameter. |

This file MUST NOT contradict any of those five.

---

## 8. Forward-looking section anchors (future iterations of this file)

The architecture document and the two other sibling docs already
cite the following sections of `tech-spec.md` as ownership
targets. This iteration intentionally does NOT fill them in --
they are listed here so a future iteration of this file knows
which slots to expand, and so that a reader following an
architecture cross-reference sees the slot exists.

| Section | Topic | Earliest citation |
| --- | --- | --- |
| S2 | CLI flag catalogue, env-var precedence, exit codes, `--store=...` resolution rules. | `architecture.md` S3.1 |
| S2.0 | cobra module path + version pin. | `architecture.md` S3.1 |
| S3 | `LocalDirMaterializer` identity synthesis rules, Walk error semantics, mtime-tree hash byte format. | `architecture.md` S3.3, S4.3 |
| S3.4 | mtime-tree hash exclude-set + serialization. | `architecture.md` S4.3 |
| S4.3 | Exit-code matrix (config error, IO error, write-contract, manifest parse). | `architecture.md` S6.2 |
| S5 | `graphsink` interface field-level shapes; SQLite column-level mapping; memory-backend JSON-export key ordering. | `architecture.md` S3.2 |
| S5.2 | SQLite mirror of `node_kind` / `edge_kind` ENUMs as CHECK constraints. | `architecture.md` S3.2.3 |
| S5.3 | Memory-backend JSON-export schema (key ordering, optional fields, null handling). | `architecture.md` S3.2.4 |
| S6 | HTTP request/response shapes for `GET /api/repos`, `GET /api/diagram/module`, `GET /api/diagram/calls`. | `architecture.md` S3.7, S4.4, S5.6 |
| S6.1 | Verb / path / query parameters. | `architecture.md` S5.6 |
| S6.2 | Exhaustive diagram envelope JSON schema + error envelope. | `architecture.md` S4.4 |
| S6.3 | CORS origin pin (`http://localhost:5173` for Vite dev). | `architecture.md` S3.7, R8 mitigation in this file. |
| S7 | UI color / size palette, layout-direction mapping, truncation-badge copy, sparseness-expectation copy for call chains. | `architecture.md` S3.8, R3 in this file. |
| S8 | Operational concerns -- log shape, rotation, file naming. | `architecture.md` S3.2.3, R11 in this file. |
| S8.2 | SQLite `.db` rotation policy on re-scan. | `architecture.md` S3.2.3, S7.2 |
| S9 | Log-event taxonomy for `graphsink.sqlite.<op>` / `graphsink.memory.<op>`. | `architecture.md` S5.8 |
| S10 | Scan-summary format (counts, skipped by reason, by extension). | `architecture.md` S7.1, C7 in this file. |

Each row here is a forward pointer, NOT a placeholder that
should be silently expanded by a different artifact. A future
iteration of this same file (`tech-spec.md`) owns expansion.

---

## 9. Glossary of Anchored Source References

A quick lookup table the rest of this file -- and any future
iteration of it -- can cite without re-deriving the path. Every
entry is grounded in the repo at the commit this iteration was
written against (`HEAD` of branch
`story/code-intelligence-REPO-SCANNER/plan-tech-spec`).

| Name | Path | Purpose |
| --- | --- | --- |
| `cmd/repoindexer/main.go` | `services/agent-memory/cmd/repoindexer/main.go` | The existing queue-worker binary the CLI is NOT replacing. Env-var catalogue at lines 22-39. |
| `ast.Dispatcher` | `services/agent-memory/internal/repoindexer/ast/dispatcher.go` (`NewDispatcher` line 147, `EmitFile` line 220) | The polyglot dispatcher reused unchanged in body. |
| `ast.NodeEdgeWriter` | `services/agent-memory/internal/repoindexer/ast/dispatcher.go` line 42 | The narrow writer interface the dispatcher targets; `graphsink.Sink` is a strict superset. |
| `parsers_cgo.go` / `parsers_nocgo.go` | `services/agent-memory/internal/repoindexer/ast/` | C1 / C7 enforcement -- CGO-gated parser registration. |
| `parser_powershell.go` | `services/agent-memory/internal/repoindexer/ast/` | C2 enforcement -- `ErrParserUnavailable` path. |
| `Materializer` / `Workspace` | `services/agent-memory/internal/repoindexer/materialize.go` lines 23-49 | The materializer interface and workspace handle; `defaultExcludeDirs` line 137. |
| `graphwriter.Writer` | `services/agent-memory/internal/graphwriter/writer.go` (`EnsureRepo` line 281, `EnsureCommit` line 359, `RepoInput` line 250) | The production Postgres writer; the Postgres `graphsink` adapter forwards to it. |
| `graphreader.Reader` | `services/agent-memory/internal/graphreader/reader.go` (`MaxListLimit` line 55, `ListNodes` line 360, `ListEdgesFrom` line 258, `ListEdgesTo` line 289) | The Postgres reader the `graphsink.Reader` Postgres adapter wraps. |
| `pkg/fingerprint` | `services/agent-memory/pkg/fingerprint/` (`NodeFingerprint` lines 195-259, `EdgeFingerprint` lines 261-319) | C4 enforcement -- canonical SHA pre-image. |
| Migration `0001_enums.sql` | `services/agent-memory/migrations/0001_enums.sql` | `node_kind` / `edge_kind` ENUM source of truth. |
| Migration `0003_node_edge.sql` | `services/agent-memory/migrations/0003_node_edge.sql` | Node / Edge column shapes the SQLite backend mirrors. |
| Migration `0016_roles_grants.sql` | `services/agent-memory/migrations/0016_roles_grants.sql` | C5 enforcement -- `agent_memory_app` role grants. |
| `internal/embedding` | `services/agent-memory/internal/embedding/` | C8 -- the publisher the CLI does NOT wire unless `--with-embeddings`. |
| `services/agent-memory/web/.gitkeep` | as named | The greenfield frontend slot. |

---

## Iteration history

- 2026-05-30: iter 1 -- problem framing only. Sections 1-7
  authored; Section 8 catalogues the forward-looking section
  anchors the architecture already cites so future iterations
  know where to expand. Section 9 captures the source references
  this iteration verified.
