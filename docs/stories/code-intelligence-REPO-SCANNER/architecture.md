# Repo Scanner -- Architecture

> Story: `code-intelligence:REPO-SCANNER` -- Points 21
> This document is the **architecture-only** deliverable for the story. The
> sibling docs `tech-spec.md`, `implementation-plan.md`, and `e2e-scenarios.md`
> are downstream artifacts written in **later** iterations of this workstream;
> when this file delegates a field-level shape, env-var matrix, stage order,
> or scenario walk-through to one of them, the reference is forward-looking
> (future ownership) rather than a citation of co-shipping content.
>
> Anchoring rule: every Go package, type, table, and migration named in this
> document already exists at the path quoted unless explicitly marked **NEW**.
> The story is mostly a glue-and-extend effort on top of the AGENT-MEMORY
> stack; this file calls out the seams it introduces.

---

## 1. Purpose and Scope

The Repo Scanner workstream takes the polyglot AST dispatcher and the
structural graph the AGENT-MEMORY service already produces and ships them as
a **standalone developer tool** that a single engineer can run against many
external repositories without first standing up Postgres, Qdrant, and OTel.

Concretely the story delivers:

1. A `codeintel` CLI (**NEW**, `services/agent-memory/cmd/codeintel/`) that
   scans a repository by local path or by `git URL @ sha`, persists the
   resulting structural graph to a chosen backend, and prints a
   per-language summary.
2. A `graphsink` storage abstraction (**NEW**,
   `services/agent-memory/internal/graphsink/`) with three backends --
   Postgres (wraps the existing `graphwriter.Writer`), SQLite (default
   for CLI), and in-memory + JSON export.
3. A `LocalDirMaterializer` (**NEW**, added to
   `services/agent-memory/internal/repoindexer/materialize.go`) so the
   CLI can scan an on-disk checkout without a `git fetch`.
4. A `diagram` projector (**NEW**,
   `services/agent-memory/internal/diagram/`) that turns the graph into
   two JSON diagram families -- top-down **module/component** and
   left-right **call chain**.
5. A read/serve HTTP surface (**NEW**, `codeintel serve` and/or
   handlers on `internal/mgmtapi`) that backs the UI.
6. A React + `@neo4j-nvl` web app (**NEW**,
   `services/agent-memory/web/`) that renders both diagram families and
   supports interactive re-seeding of the call-chain view.

The story description's Section 1 ("Goals") is the source of truth for
scope. The non-goals from that section are inherited verbatim:
cross-file call/inheritance resolution stays bounded by the AGENT-MEMORY
A4 same-file-only rule, dynamic-call edges beyond what the Span Ingestor
already produces are out of scope, and the UI ships without
production-grade auth.

### 1.1 Languages in scope

The supported set is the AST-PARSER-FOR-ADDIT closure plus the v1 pair:
TypeScript/JavaScript, Python, C, C++, C#, Go, Rust, and PowerShell.
All eight parsers already live under
`services/agent-memory/internal/repoindexer/ast/`; this story does NOT
add a new grammar.

### 1.2 Sibling-doc seams

This doc owns *what the components are*, *what data flows between them*,
and *what the public types look like*. It deliberately does not own:

- Field-level JSON schemas, env-var catalogue, CLI flag matrix, or
  configuration precedence -- those belong to **tech-spec.md** (future
  ownership, downstream iteration).
- Stage-by-stage implementation order, branch list, or test-coverage
  targets -- those belong to **implementation-plan.md** (future
  ownership).
- Scenario walk-throughs with inputs/outputs and golden assertions --
  those belong to **e2e-scenarios.md** (future ownership).

Cross-references use the form `tech-spec.md S4` / `implementation-plan.md S3` and are
written as forward pointers; when the sibling doc is generated in a
later iteration of this workstream it will inherit the same section
labels.

---

## 2. Guiding Principles

These are the contract bedrocks the rest of the doc respects. Where you
see `S<n>` referenced later it is one of these.

- **S1 -- Inherit, never fork the graph model.** Repo Scanner reuses the
  `node_kind` / `edge_kind` ENUMs from
  `services/agent-memory/migrations/0001_enums.sql` and the row shape
  from `0003_node_edge.sql`. Backends differ only in *where* rows are
  written, never in their fields or identity.
- **S2 -- Identity is fingerprint, no matter the backend.** The G2 rule
  from AGENT-MEMORY's architecture (`pkg/fingerprint`) is the single
  source of truth. The Postgres, SQLite, and in-memory backends all
  call `fingerprint.NodeFingerprint` / `fingerprint.EdgeFingerprint`
  through the same helper so a repo scanned to SQLite and later replayed
  into Postgres yields byte-identical node/edge identities.
- **S3 -- Diagrams are derived, not stored.** The two diagram families
  are read-time projections over `node` + `edge`. They never sit in
  their own table; truncation and stale-graph hazards therefore live in
  one place (the projector).
- **S4 -- Scanning is decoupled from embeddings.** The AST dispatcher's
  `WithEmbeddingPublisher` option is the only hook into the Qdrant /
  embedder stack. The CLI default omits it so Qdrant is not on the
  critical path of "understand a repo" -- it stays opt-in via
  `--with-embeddings`.
- **S5 -- Append-only / tombstone semantics belong to Postgres alone.**
  The CLI's SQLite and in-memory backends model a *scan snapshot* --
  re-scan replaces. Postgres retains the AGENT-MEMORY G5 invariant
  (append + tombstone via `NodeRetirement` / `EdgeRetirement`); the
  CLI's `--store=postgres` path keeps using `graphwriter.Writer`
  unchanged so retirement / rename flow is not duplicated in the new
  backends.
- **S6 -- Truncation MUST be visible.** Every diagram payload carries
  `truncated` + `stats.cappedAt`; the UI MUST render the badge when
  set. `graphreader.MaxListLimit = 10_000` (see
  `services/agent-memory/internal/graphreader/reader.go`) is the
  clamp -- the projector inherits it.
- **S7 -- Degraded language coverage MUST be loud, not silent.** A
  CGO=0 build registers only the PowerShell parser
  (`services/agent-memory/internal/repoindexer/ast/parsers_nocgo.go`);
  a `pwsh`-less host returns `ErrParserUnavailable`. The CLI summary
  surfaces every `no_parser` and `pwsh_not_available` skip with file
  counts so the operator notices coverage gaps before reading the
  graph.

---

## 3. Component Map

Logical components, grouped by trust boundary. Existing components keep
their package path; new components are tagged **NEW**.

```
+----------------------------------------------------------------+
|                       Developer workstation                    |
|                                                                |
|  +----------------+        +------------------------------+    |
|  | codeintel CLI  |        | React + @neo4j-nvl UI        |    |
|  | (NEW)          |        | services/agent-memory/web    |    |
|  | cmd/codeintel  |        | (NEW)                        |    |
|  +-------+--------+        +--------------+---------------+    |
|          |                                | fetch JSON         |
|          v                                v                    |
|  +-------+--------+        +--------------+---------------+    |
|  | scan pipeline  |        | serve endpoint               |    |
|  | (reused, S4)   |        | (NEW; cmd/codeintel serve or |    |
|  +-------+--------+        |  internal/mgmtapi handlers)  |    |
|          |                 +--------------+---------------+    |
|          v                                |                    |
|  +-------+----------------------+         |                    |
|  | graphsink (NEW)              |<--------+                    |
|  +-+-----------+-----------+----+                              |
|    | postgres  | sqlite    | memory + JSON export              |
|    v           v           v                                   |
| +--+-----+ +---+----+ +----+----+                              |
| | graph- | | sqlite | | inmem   |                              |
| | writer | | store  | | store   |                              |
| | (xst)  | | (NEW)  | | (NEW)   |                              |
| +---+----+ +---+----+ +----+----+                              |
|     |          |           |                                   |
|     v          v           v                                   |
| +---+--------------------------+                               |
| | graphreader-shaped read API  |                               |
| | (existing pgx Reader; NEW    |                               |
| | sqlite + memory adapters)    |                               |
| +---+--------------------------+                               |
|     |                                                          |
|     v                                                          |
| +---+--------------------------+                               |
| | diagram projector (NEW)      |                               |
| | internal/diagram             |                               |
| +------------------------------+                               |
+----------------------------------------------------------------+
```

The blocks left of `graphsink` are reused verbatim from AGENT-MEMORY.
The new surface area is everything that touches `graphsink`, plus the
diagram projector, the serve endpoint, and the UI.

### 3.1 codeintel CLI -- `services/agent-memory/cmd/codeintel/` (**NEW**)

Single binary with subcommands. Owns the scan/projection control plane;
holds no persistent state of its own (the chosen `graphsink` backend
does).

| Subcommand | Responsibility |
| --- | --- |
| `scan <path-or-url> [--sha]` | Materialize one repo, drive the AST dispatcher, persist to the chosen backend, print a summary. |
| `scan-many <manifest>` | Iterate a manifest (one repo per line, path or `url@sha`), reuse the same dispatcher / backend per entry. |
| `diagram module` | Read the persisted graph, build the top-down module/component diagram JSON. |
| `diagram calls --seed <sig-or-id>` | Read the graph, build the left-right call-chain diagram JSON. |
| `serve --addr :8088` | Same persisted graph, expose the diagram endpoints over HTTP so the UI can call them. |

Non-responsibilities (delegated to existing packages):

- File materialization -> `internal/repoindexer.Materializer`.
- AST parsing + Node/Edge production -> `internal/repoindexer/ast.Dispatcher`.
- Fingerprinting -> `pkg/fingerprint`.
- Postgres DML -> `internal/graphwriter.Writer`.

The CLI parses flags with a vendored cobra-style command framework
(`github.com/spf13/cobra`, a new top-level dependency added in this
story; see **tech-spec.md S2.0** for the exact module path and
version pin). **Decision (pinned, S9.3, operator answer 2026-05-30):**
cobra was chosen over stdlib `flag` because the CLI has four
distinct subcommands (`scan`, `scan-many`, `diagram module`,
`diagram calls`, plus `serve`) and a future plugin/sub-sub-command
surface; cobra's per-command help, completion, and persistent-flag
machinery pay back the dependency. Configuration precedence and
per-flag semantics are catalogued in **tech-spec.md S2**.

### 3.2 graphsink -- `services/agent-memory/internal/graphsink/` (**NEW**)

The narrow writer abstraction the dispatcher is rewired to target.
Today `ast.NewDispatcher` requires a `NodeEdgeWriter` (see
`services/agent-memory/internal/repoindexer/ast/dispatcher.go`):

```go
type NodeEdgeWriter interface {
    InsertNode(ctx context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error)
    InsertEdge(ctx context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error)
}
```

`*graphwriter.Writer` already satisfies this interface natively. We add
a thin `graphsink.Sink` super-interface so the three backends can plug
in without forcing the dispatcher to import them:

```go
package graphsink

type Sink interface {
    EnsureRepo(ctx context.Context, in graphwriter.RepoInput) (graphwriter.RepoRecord, error)
    EnsureCommit(ctx context.Context, in graphwriter.CommitInput) (graphwriter.CommitRecord, error)
    InsertNode(ctx context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error)
    InsertEdge(ctx context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error)
    Flush(ctx context.Context) error
    Close() error
}
```

The `RepoInput` / `RepoRecord` / `CommitInput` / `CommitRecord` types
are reused verbatim from `internal/graphwriter` -- there is no new DTO
introduced at this seam. Field-level shapes belong to **tech-spec.md
S5**.

#### 3.2.1 Backend matrix

| Backend | Package (NEW unless noted) | Default? | Use case |
| --- | --- | --- | --- |
| Postgres | `internal/graphsink/postgres` (wraps existing `*graphwriter.Writer`) | No | `--store=postgres`; production-identical; reuses migrations 0001-0004 and `graphreader`. |
| SQLite | `internal/graphsink/sqlite` | **Yes** | `--store=sqlite`; single `.db` file per repo; no external services; cross-machine portable. |
| Memory + JSON | `internal/graphsink/memory` | No | `--store=memory --export graph.json`; one-shot scans, tiny repos, CI smoke tests. |

All three call into the same `pkg/fingerprint` helpers, so a Node
inserted into SQLite and later replayed into Postgres produces the same
`(repo_id, fingerprint)` tuple. The backend-parity test catalogued in
**implementation-plan.md S6** asserts this on a fixture repo.

#### 3.2.2 Postgres backend invariants

The Postgres backend is a thin adapter -- it forwards `InsertNode` /
`InsertEdge` to `*graphwriter.Writer` 1:1. It MUST preserve:

- G5 append-only / tombstone semantics (the `agent_memory_app` role
  cannot UPDATE `node` / `edge`; see `0016_roles_grants.sql`).
- `WriteContractViolation` surfacing -- if the CLI ever connects with
  the wrong role, the typed error must propagate to the user.
- `auditDefer` structured logging -- one log record per insert, per
  the writer convention in `internal/graphwriter/writer.go`.

The CLI MUST NOT bypass these via direct SQL.

#### 3.2.3 SQLite backend shape

Tables mirror migrations `0003_node_edge.sql` and `0002_repo_commit.sql`
field-for-field, with PostgreSQL `uuid` mapped to TEXT, `bytea` to
BLOB, `jsonb` to TEXT (validated to be a JSON object on insert), and
`timestamptz` to INTEGER (unix millis). Tombstone tables
(`node_retirement` / `edge_retirement`, migration `0004`) are NOT
created -- the SQLite store is a scan snapshot per S5, so re-scanning a
repo opens a fresh database file (the prior file is moved aside if
present, see **tech-spec.md S8** for the rotation policy).

Column-level mappings -- including the `kind`-enum `CHECK` constraints
that replace PostgreSQL's `node_kind` / `edge_kind` ENUMs -- live in
**tech-spec.md S5.2**.

#### 3.2.4 Memory backend shape

Two append-only slices (`nodes []NodeRecord`, `edges []EdgeRecord`)
plus a `map[fingerprint.Sum]string` for the idempotent re-emit path.
`Flush` is a no-op; `Close` is what triggers the optional JSON export
(when the CLI passed `--export <file>`). The exporter writes a single
JSON object with shape:

```jsonc
{
  "repo": {"id": "...", "url": "...", "sha": "..."},
  "nodes": [/* graphwriter.NodeRecord plus the original NodeInput */],
  "edges": [/* graphwriter.EdgeRecord plus the original EdgeInput */]
}
```

This is the rehydration format `codeintel diagram` consumes when
pointed at an export file rather than a live backend. The exact JSON
schema (key ordering, optional fields, null handling) is catalogued in
**tech-spec.md S5.3**.

### 3.3 LocalDirMaterializer -- extension of `internal/repoindexer/materialize.go` (**NEW**)

A third implementation of the existing `Materializer` interface (the
one `GitMaterializer` and `InMemoryMaterializer` already satisfy).
Walks an on-disk directory using the same `defaultExcludeDirs` set
already defined in `materialize.go`. Synthesizes a stable repo URL of
the form `file://<abs-or-normalized-path>` and resolves the SHA via
`git rev-parse HEAD` when the directory is a git repo, falling back to
the sentinel `"local"` otherwise.

The new type's exact identity-synthesis rules and Walk error semantics
are catalogued in **tech-spec.md S3**.

### 3.4 AncestryWriter -- factored from `worker.go` (**NEW**)

The Stage 3.1 worker's `runFull` method
(`services/agent-memory/internal/repoindexer/worker.go` lines
1084-1219) owns a load-bearing pre-`EmitFile` sequence that the CLI
MUST reproduce verbatim:

1. `graphwriter.EnsureRepo` -- upsert the `repo` row by URL.
2. `graphwriter.EnsureCommit` -- append the `repo_commit` row for the
   scanned SHA.
3. `InsertNode` of kind `repo` whose `canonical_signature` is
   `canonicalRepoSig(repoURL)` (just the URL, today). The returned
   `RepoNodeID` is the parent of every `package` Node.
4. For each file from `Workspace.Walk`:
   - Compute `dir := canonicalPackageDir(file.RelPath)` (the
     forward-slash directory path, "" for the repo root).
   - Look up `dir` in a per-scan `map[string]nodeID` cache. On miss,
     `InsertNode` of kind `package` (parent=`RepoNodeID`,
     `canonical_signature = canonicalPackageSig(repoURL, dir)`),
     then `InsertEdge` of kind `contains` from repo to package.
   - `InsertNode` of kind `file` (parent=`packageNodeID`,
     `canonical_signature = canonicalFileSig(repoURL, relPath)`),
     then `InsertEdge` of kind `contains` from package to file.
   - Build an `EmitFileEvent` populating BOTH `FileNodeID` and
     `RepoNodeID` (the dispatcher needs the latter to parent the
     synthetic external-package Nodes it mints for non-relative
     imports -- see the doc comment on `EmitFileEvent.RepoNodeID`
     in `internal/repoindexer/ast.go`).

Today the four helpers (`canonicalRepoSig`, `canonicalPackageDir`,
`canonicalPackageSig`, `canonicalFileSig`) are unexported functions
in `worker.go` lines 1222-1259. This story extracts the sequence and
the helpers into a new in-package type so the CLI cannot drift from
the worker's identity calculus:

```go
package repoindexer

// AncestryWriter owns the repo -> package -> file InsertNode +
// contains-edge sequence that both the queue worker (worker.go
// runFull) and the codeintel CLI MUST drive identically. Construct
// one per scan; not safe for concurrent use because the package
// cache is single-writer.
//
// A first-time CLI scan does NOT know the repo's fingerprint.RepoID
// before EnsureRepoAndCommit runs -- repo.repo_id is allocated by
// the Postgres schema's `DEFAULT gen_random_uuid()` on first INSERT
// (migrations/0002_repo_commit.sql line 32). For the SQLite/memory
// backends the AncestryWriter derives a deterministic RepoID from
// the URL on the first call so SQLite-scanned graphs can be replayed
// into Postgres without losing fingerprint parity (S2). Either way
// the RepoID is the OUTPUT of EnsureRepoAndCommit, never an input.
type AncestryWriter struct {
    Sink    graphsink.Sink   // any sink: postgres, sqlite, memory
    RepoURL string
    SHA     string
    // The per-scan dedupe cache. Empty at construction; the CLI
    // and the worker MUST share one across the file walk.
    packages map[string]string // canonicalPackageDir -> package node id
    // ancestry is populated by EnsureRepoAndCommit and is what
    // EnsureFile reads to obtain RepoID + RepoNodeID. Calling
    // EnsureFile before EnsureRepoAndCommit returns an error.
    ancestry RepoAncestry
}

// NewAncestryWriter returns a writer scoped to one (repoURL, sha)
// scan. The RepoID is not yet known and is intentionally not an
// argument.
func NewAncestryWriter(sink graphsink.Sink, repoURL, sha string) *AncestryWriter

// EnsureRepoAndCommit performs Sink.EnsureRepo (which allocates the
// RepoID), Sink.EnsureCommit, and InsertNode(kind=repo) in that
// order, caches the resulting RepoAncestry on the writer, and
// returns it. The CLI captures the returned RepoID for use in
// EmitFileEvent.RepoID and for the diagram projector's repo
// scoping.
func (a *AncestryWriter) EnsureRepoAndCommit(ctx context.Context, defaultBranch string, hints []string) (RepoAncestry, error)

// EnsureFile performs the per-file package-on-miss + file
// InsertNode + contains-edge pair. It reads RepoID + RepoNodeID
// from a.ancestry and returns the parent/child node ids the CLI
// passes into EmitFileEvent.
func (a *AncestryWriter) EnsureFile(ctx context.Context, file WalkFile) (FileAncestry, error)

type RepoAncestry struct {
    RepoID         fingerprint.RepoID // assigned by Sink.EnsureRepo (Postgres) or derived from RepoURL (SQLite/memory)
    RepoUUID       string             // textual UUID form, == graphwriter.RepoRecord.RepoID
    RepoNodeID     string             // assigned by Sink.InsertNode(kind=repo)
    CommitID       string             // assigned by Sink.EnsureCommit
    CommitInserted bool               // true on first scan of this SHA
}
type FileAncestry struct {
    FileNodeID     string
    PackageNodeID  string
    PackageDir     string
    NewlyInserted  bool // file Node freshly inserted (vs idempotent re-hit)
}
```

The Postgres backend's `Sink.EnsureRepo` delegates straight to
`*graphwriter.Writer.EnsureRepo` (writer.go:281-328) so the assigned
`RepoID` matches the queue worker's. The SQLite and memory backends
synthesize `RepoID = fingerprint.RepoIDFromURL(repoURL)` (a new
deterministic helper to be added in `pkg/fingerprint`, computed as
`uuid.NewSHA1(namespaceRepoURL, repoURL)`) so the same URL always
hashes to the same `fingerprint.RepoID` and a SQLite-scanned graph
replayed into Postgres preserves Node/Edge fingerprint parity (S2,
R5). The mgmt-api / queue worker path remains the canonical Postgres
RepoID allocator -- the deterministic helper is a fallback used only
when there is no Postgres row to read from.

The canonical-signature helpers (`canonicalRepoSig`,
`canonicalPackageDir`, `canonicalPackageSig`, `canonicalFileSig`)
are promoted to exported `CanonicalRepoSig` / `CanonicalPackageDir` /
`CanonicalPackageSig` / `CanonicalFileSig` in the same package, with
their bodies unchanged so the existing worker and the new CLI hash
to byte-identical Node fingerprints. The change is a refactor of
existing logic, not new identity rules.

After this refactor:

- `worker.runFull` calls `EnsureRepoAndCommit` once and
  `EnsureFile` per `WalkFile`, then proceeds to its existing
  `EmitFile` invocation.
- The CLI calls the same two methods in the same order, then runs
  `EmitFile` synchronously without a queue.

Concrete reuse is documented in S5.2 (interface) and S6.1 (sequence).

### 3.5 ast.Dispatcher (existing, generalised)

The Stage 3.2 dispatcher under
`services/agent-memory/internal/repoindexer/ast/` is reused unchanged
in body. The only generalisation is the writer parameter: today the
constructor takes a `NodeEdgeWriter` (which `*graphwriter.Writer`
satisfies natively); after this story it accepts any `graphsink.Sink`
via the same parameter (the existing `NodeEdgeWriter` interface
remains; `graphsink.Sink` is a strict superset, so the dispatcher's
signature does not change).

Parser registration, the two-pass insert protocol, the
`ErrParserUnavailable` skip path, the per-language LangMeta merge, and
the `ast.dispatch.skip` / `ast.parse.error` log events all stay
identical. The CLI receives the same `EmitResult.TouchedNodes` slice
the queue worker does, even though the CLI ignores it (snapshot scans
have no retire-set to compute -- see S5).

### 3.6 diagram projector -- `services/agent-memory/internal/diagram/` (**NEW**)

Pure read-side. Takes a `graphsink.Reader` (the read counterpart of
`graphsink.Sink`; for Postgres it wraps `*graphreader.Reader`, for
SQLite a new `sqlite.Reader`, for memory a slice scan) and produces
the diagram contract from S5.4.

Two builders:

- `BuildModuleDiagram(ctx, reader, repoID, granularity)` -- containment
  hierarchy + rolled-up `imports` edges between components of the
  chosen granularity (`package`, `file`, or `class`). Roll-up logic:
  given an `imports` edge from file `A` to file `B`, the projector
  emits a synthetic edge between `A.parent_package` and
  `B.parent_package` with `weight = count` and dedupe-by-(src,dst).
- `BuildCallChain(ctx, reader, seed, depth, direction)` -- bounded BFS
  over `static_calls` (and, when present, `observed_calls`); walks
  callees via `ListEdgesFrom`, callers via `ListEdgesTo`, and tags
  each edge with its kind for UI styling.

Both functions clamp `nodeCount + edgeCount` to a configurable cap
(default `MaxListLimit = 10_000`) and set `truncated = true` when the
cap is hit. The contract shape is in S5.4.

### 3.7 serve endpoint -- `codeintel serve` and/or `internal/mgmtapi` handlers (**NEW**)

Two deployment shapes, both backed by the same `diagram` package:

- **Standalone**: `codeintel serve --store sqlite --db ./repo.db --addr :8088`
  spins up an HTTP server with three routes (`GET /api/repos`,
  `GET /api/diagram/module`, `GET /api/diagram/calls`) plus a CORS
  preflight for `http://localhost:5173` (the Vite dev origin).
- **Integrated**: the same routes are mounted under
  `internal/mgmtapi.Handler` so the production deployment (which
  already runs `mgmt-api` against Postgres) exposes the diagrams
  alongside the existing `mgmt.read.*` surface.

Request/response field shapes, status codes, and CORS specifics are in
**tech-spec.md S6**.

### 3.8 React + neo4j-nvl UI -- `services/agent-memory/web/` (**NEW**)

Vite + React + TypeScript single-page app. Today the directory is just
a `.gitkeep`. New top-level deps:

- `@neo4j-nvl/react` -- `InteractiveNvlWrapper` is the canvas component.
- `@neo4j-nvl/base` -- types + layout options.
- `react` / `react-dom` / `typescript` / `vite` -- standard SPA stack.

UI components and their responsibilities:

| Component | Responsibility |
| --- | --- |
| `<RepoPicker>` | Lists repos (one per scanned backend) via `GET /api/repos`. |
| `<DiagramSwitch>` | Toggles between Module and Call Chain modes; persists the choice in URL hash. |
| `<NvlCanvas>` | Maps the diagram JSON to `@neo4j-nvl/react`'s `nodes` / `relationships` shape and applies the layout preset from `layoutHint`. |
| `<Inspector>` | Shows the selected node's `attrs` (language, modifiers, line range). |
| `<CallChainNav>` | On node click in call-chain mode, re-issues `GET /api/diagram/calls?seed=<id>` and re-renders. |

The contract bridge -- mapping `diagram.nodes[i]` to NVL's
`{id, caption, color, size}` and `diagram.edges[i]` to NVL's
`{id, from, to, caption, width, color}` -- is documented in S5.4.1.
Color / size policy lives in **tech-spec.md S7**.

---

## 4. Data Model

The graph schema is inherited from AGENT-MEMORY. This story adds NO
new node kinds, NO new edge kinds, and NO new migrations against the
production Postgres database. The new persistent shape is the SQLite
schema (S5 below), which mirrors the existing tables.

### 4.1 Node entity (existing)

Source of truth: `services/agent-memory/migrations/0003_node_edge.sql`,
read shape `graphreader.Node` in
`services/agent-memory/internal/graphreader/reader.go`.

| Field | Type | Notes |
| --- | --- | --- |
| `node_id` | UUID | Surrogate primary key, generated server-side. |
| `repo_id` | UUID | FK to `repo`; every Node belongs to exactly one repo (G6). |
| `fingerprint` | bytea(32) | `sha256(repo_id_16B \|\| kind \|\| 0x00 \|\| canonical_signature \|\| 0x00 \|\| from_sha)` per `pkg/fingerprint.NodeFingerprint` (`fingerprint.go` lines 195-259). `repo_id_16B` is the raw 16-byte RFC 4122 layout; `0x00` is the framing NUL byte that disambiguates the kind / canonical_signature / from_sha boundaries (without it the tuples `(method, "pkg.Foo()a", "bc")` and `(method, "pkg.Foo()", "abc")` would hash-collide). The helper rejects any field containing an embedded NUL (`ErrEmbeddedNUL`). G2 dedupe key with `repo_id` (UNIQUE index `node_repo_fingerprint_uidx`). |
| `kind` | node_kind ENUM | Closed set: `repo`, `package`, `file`, `class`, `method`, `block`. |
| `canonical_signature` | text | Language-stable identifier (e.g. `<repoURL>::method::<relPath>#Foo.bar(int,string)`). |
| `parent_node_id` | UUID nullable | NULL only for the `repo` kind. Drives the `repo -> package -> file -> class -> method -> block` walk. |
| `from_sha` | text | First-seen SHA. Together with the other components forms the fingerprint pre-image. |
| `attrs_json` | jsonb | Language-specific bag: `language`, `decl_kind`, `start_line`, `end_line`, `params_raw`, `modifiers`, `extends_raw`, `calls_raw`, plus per-language LangMeta keys (see AGENT-MEMORY architecture S4.4.3). |

The CLI never invents new node kinds or attribute keys; it just routes
the dispatcher's output to whatever backend the user chose.

### 4.2 Edge entity (existing)

| Field | Type | Notes |
| --- | --- | --- |
| `edge_id` | UUID | Surrogate primary key. |
| `repo_id` | UUID | FK to `repo`; cross-repo edges are rejected at write time. |
| `fingerprint` | bytea(32) | `sha256(repo_id_16B \|\| kind \|\| 0x00 \|\| src_fingerprint_32B \|\| dst_fingerprint_32B \|\| from_sha)` per `pkg/fingerprint.EdgeFingerprint` (`fingerprint.go` lines 261-319). Only `kind` needs a trailing NUL because src/dst are fixed 32-byte values and `from_sha` trails (its boundary is fixed by dst's known length). Edge identity is keyed by endpoint *fingerprints*, not endpoint UUIDs, so the edge remains stable across re-ingests of the same commit when surrogate `node_id`s change. |
| `kind` | edge_kind ENUM | Closed set: `contains`, `imports`, `static_calls`, `observed_calls`, `extends`, `implements`, `reads`, `writes`, `renamed_to`, `overrides` (added by migration `0022_edge_kind_overrides.sql`). |
| `src_node_id` | UUID | FK to `node`. |
| `dst_node_id` | UUID | FK to `node`. |
| `from_sha` | text | First-seen SHA. |
| `attrs_json` | jsonb | Edge-kind-specific bag (e.g. `imports.symbols`, `imports.is_type_only`, `static_calls.line`). |

For the two diagram families the projector consumes:

- **Module diagram**: `contains` (hierarchy) + `imports` (component
  interactions).
- **Call-chain diagram**: `static_calls` and `observed_calls` only;
  `extends` / `implements` / `overrides` / `reads` / `writes` are
  surfaced in the Inspector but do NOT influence the BFS.

### 4.3 Repo and RepoCommit entities (existing)

`repo` and `repo_commit` tables from migration `0002`. The CLI calls
`graphsink.EnsureRepo` / `graphsink.EnsureCommit` once per scan; the
Postgres backend forwards to `graphwriter.EnsureRepo` /
`EnsureCommit`, the SQLite backend writes the same fields to the local
file, and the memory backend retains them on the in-process struct.

For local-path scans the synthesized identity is:

| Field | Value |
| --- | --- |
| `url` | `file://<abs-path>` (lower-cased drive letters on Windows, forward slashes). |
| `sha` | Output of `git rev-parse HEAD` when the directory is a git checkout; otherwise a **deterministic hex hash of the directory's mtime tree** (see S9.1). The mtime hash is a sentinel that re-derives stably as long as no file is touched, but changes the moment any tracked file's mtime moves -- this makes `(repo_id, sha)` a reasonable change-detection key for re-scans of non-git directories. |
| `default_branch` | Empty string. |
| `language_hints` | Empty slice (the CLI does not surface a hint UI in v1). |

**Decision (pinned, S9.1, operator answer 2026-05-30):** the
synthesized `sha` for non-git local scans is a deterministic hex
hash of the directory's mtime tree, computed as
`sha256( for each f in Workspace.Walk in stable order: f.RelPath + 0x00 + f.ModTime.UTC().Unix() + 0x00 + f.Size + 0x00 )[:16]`
expressed as a 32-char lowercase hex string (exact byte format to
be specified in **tech-spec.md S3.4**). The operator may override
it with `--sha <value>` when they want a stable identity across
re-scans that ignores mtime drift. The literal string `local` was
considered and rejected because every non-git scan would collide
under the same `(repo_id, sha)` key, breaking re-scan dedupe whenever
the directory's contents actually changed.

### 4.4 Diagram contract (NEW)

The single JSON envelope both diagram families return. Identical key
order across families so the UI ships one parser.

```jsonc
{
  "diagram": "module" | "callchain",
  "repo":    {"id": "<uuid>", "url": "<repo-url>", "sha": "<sha>"},
  "generatedAt": "<RFC3339Nano UTC>",
  "layoutHint": "hierarchical-top-down"
              | "hierarchical-left-right"
              | "force",
  "nodes": [
    {
      "id": "<node-uuid-or-synthetic>",
      "label": "<display caption>",
      "kind": "package" | "file" | "class" | "method" | "block",
      "language": "<go|python|typescript|rust|c|cpp|csharp|powershell>",
      "group": "<owning-package-or-file id>",
      "attrs": {/* mirror of node.attrs_json relevant subset */}
    }
  ],
  "edges": [
    {
      "id": "<edge-uuid-or-synthetic>",
      "from": "<node-id>",
      "to":   "<node-id>",
      "kind": "contains" | "imports" | "static_calls" | "observed_calls"
            | "extends"  | "implements" | "overrides"
            | "reads"    | "writes",
      "weight": 1,
      "label": "<edge.kind verbatim>"
    }
  ],
  "truncated": false,
  "stats": {
    "nodeCount": 0,
    "edgeCount": 0,
    "cappedAt":  10000,
    "skipped":   {"no_parser": 0, "pwsh_not_available": 0}
  }
}
```

Synthetic ids are used in two cases:

1. The memory + JSON backend assigns deterministic ids by hashing
   `(repo_id, fingerprint)` -- it has no surrogate UUID column.
2. Module-diagram roll-up nodes that aggregate multiple files into
   their owning package use `pkg:<canonical_signature>` form.

Per S6 the `truncated` flag MUST be honoured by the UI (which renders
a "results clipped at 10 000" badge).

The exhaustive JSON schema (per-field type, optionality, error
envelope) lives in **tech-spec.md S6.2**.

#### 4.4.1 UI mapping (neo4j-nvl)

The bridge from the diagram envelope to `@neo4j-nvl/react`'s
`nodes` / `relationships` shape:

| Diagram field | NVL field | Mapping |
| --- | --- | --- |
| `nodes[].id` | `nodes[].id` | identity |
| `nodes[].label` | `nodes[].caption` | identity |
| `nodes[].kind` + `nodes[].language` | `nodes[].color` | palette in tech-spec S7 |
| `nodes[].kind` | `nodes[].size` | package=40, file=30, class=22, method=18, block=14 |
| `edges[].id` | `relationships[].id` | identity |
| `edges[].from` / `.to` | `relationships[].from` / `.to` | identity |
| `edges[].label` | `relationships[].caption` | identity |
| `edges[].weight` | `relationships[].width` | `1 + log2(weight)` (per story description S5.1) |
| `edges[].kind` == `observed_calls` | `relationships[].color` | dashed grey to mark "dynamic" |
| `layoutHint` | `nvlOptions.layout` | hierarchical or force-directed |

`layoutHint` values map to NVL layouts: `hierarchical-top-down` ->
hierarchical with `direction=down`; `hierarchical-left-right` ->
hierarchical with `direction=right`; `force` -> force-directed.

---

## 5. Interfaces Between Components

This section is the seam catalogue. Each subsection names the producer,
the consumer, the package path, and the contract shape. Field-level
JSON or struct schemas are in tech-spec.md; this is purely about
*which* surface each pair shares.

### 5.1 CLI -> Materializer

- **Producer**: `cmd/codeintel` (NEW).
- **Consumer**: `internal/repoindexer.Materializer`.
- **Surface**:
  ```go
  type Materializer interface {
      Materialize(ctx context.Context, repoURL, sha string) (Workspace, error)
  }
  ```
- **Implementations the CLI selects between**:
  - `*repoindexer.GitMaterializer` for `git URL @ sha` scans.
  - `*repoindexer.LocalDirMaterializer` (NEW) for local-path scans.
  - `*repoindexer.InMemoryMaterializer` for unit tests only.

### 5.2 Materializer -> AncestryWriter -> Dispatcher

- **Producer**: any `Workspace` (returned by Materialize).
- **Consumer**: `repoindexer.AncestryWriter` (S3.4), then
  `internal/repoindexer/ast.Dispatcher.EmitFile`.
- **Surface**:
  - `Workspace.Walk(WalkFn)` yields one `WalkFile` per source file
    (forward-slash relpaths, exclude-dir set already configured).
  - Before the walk starts, the CLI constructs an
    `AncestryWriter` with `NewAncestryWriter(sink, repoURL, sha)`
    and calls
    `AncestryWriter.EnsureRepoAndCommit(ctx, defaultBranch, hints)`
    which performs `Sink.EnsureRepo` (allocates `RepoID`) +
    `Sink.EnsureCommit` + the `repo`-kind `InsertNode` and
    **returns** a `RepoAncestry` whose `RepoID` and `RepoNodeID`
    the CLI captures. The CLI does NOT pre-allocate a `RepoID`.
  - Per `WalkFile`, the CLI calls
    `AncestryWriter.EnsureFile(ctx, file)` (which reads the cached
    ancestry off the writer) -- this (a) ensures the per-directory
    `package` Node and the `repo -> package` `contains` Edge on
    first sight of that directory and (b) inserts the `file` Node
    and the `package -> file` `contains` Edge. It returns both
    `FileNodeID` and `PackageNodeID`.
  - The CLI then builds the `EmitFileEvent`
    (`internal/repoindexer/ast.go`) populating `RepoID` (from the
    returned `RepoAncestry.RepoID`), `RepoURL`, `SHA`,
    `FileNodeID`, **`RepoNodeID`**, `RelPath`, `AbsPath`,
    `LanguageHints`, and `Open` (the field set is identical to the
    one `worker.runFull` populates at lines 1199-1208).

The five Nodes the CLI is responsible for ensuring before the
dispatcher runs are exactly those `worker.runFull` ensures today:

| Step | Kind | Parent | Canonical signature helper |
| --- | --- | --- | --- |
| 1 | `repo` | (none, NULL) | `CanonicalRepoSig(repoURL)` |
| 2 | `package` | `RepoNodeID` | `CanonicalPackageSig(repoURL, dir)` where `dir = CanonicalPackageDir(relPath)` |
| 3 | `file` | `PackageNodeID` | `CanonicalFileSig(repoURL, relPath)` |
| 4 | `contains` Edge | n/a | repo -> package (one per unique dir) |
| 5 | `contains` Edge | n/a | package -> file (one per WalkFile) |

The dispatcher then emits the `class`, `method`, and `block` Nodes
plus the static edges (`imports`, `static_calls`, `extends`,
`implements`, `overrides`, `reads`, `writes`) per its existing
two-pass contract.

This sequence is load-bearing: without the `repo` / `package` /
`file` Nodes and the corresponding `contains` Edges, the module
diagram (S6.3) has no hierarchy to walk and the containment
acceptance criterion in **e2e-scenarios.md** cannot be met. The CLI
MUST NOT shortcut it.

### 5.3 Dispatcher -> graphsink

- **Producer**: `internal/repoindexer/ast.Dispatcher`.
- **Consumer**: any `graphsink.Sink`.
- **Surface**: the existing `NodeEdgeWriter` interface (S3.2). The
  dispatcher never calls `Sink.EnsureRepo` / `Sink.EnsureCommit`; the
  CLI calls those once before the first `EmitFile`.

The `EmitResult.TouchedNodes` return path is preserved -- the CLI
ignores it (snapshot scans have nothing to retire) but the Postgres
backend's downstream consumers (delta handler, retirement adapter)
continue to receive it intact when the same dispatcher runs under the
queue worker.

### 5.4 graphsink -> diagram projector

- **Producer**: `internal/graphsink` (any backend).
- **Consumer**: `internal/diagram` (NEW).
- **Surface**: a read sub-interface (NEW) that mirrors the relevant
  subset of `*graphreader.Reader`:
  ```go
  package graphsink

  type Reader interface {
      ListNodes(ctx context.Context, repoID fingerprint.RepoID, kinds []string, f graphreader.ListNodesFilter, opts graphreader.ReaderOptions) ([]graphreader.Node, error)
      ListEdgesFrom(ctx context.Context, srcNodeID string, kinds []string, opts graphreader.ReaderOptions) ([]graphreader.Edge, error)
      ListEdgesTo(ctx context.Context, dstNodeID string, kinds []string, opts graphreader.ReaderOptions) ([]graphreader.Edge, error)
      GetNode(ctx context.Context, nodeID string, opts graphreader.ReaderOptions) (graphreader.Node, error)
      LookupBySignature(ctx context.Context, repoID fingerprint.RepoID, kind, sig string) (graphreader.Node, error)
  }
  ```
  The first four methods are signature-identical to the existing
  `*graphreader.Reader`. `LookupBySignature` is the new convenience
  the call-chain BFS uses to resolve a `--seed <sig>` flag without
  the caller knowing the node id; it is satisfied by
  `ListNodes` with `Kinds=[kind]` and
  `ListNodesFilter.CanonicalSignature=sig`.

### 5.5 diagram projector -> serve endpoint

- **Producer**: `internal/diagram` (NEW).
- **Consumer**: `cmd/codeintel serve` and/or `internal/mgmtapi`
  handlers (NEW).
- **Surface**: in-process function calls. The HTTP layer marshals the
  returned struct with `encoding/json` and writes it as the response
  body.

### 5.6 serve endpoint -> React UI

- **Producer**: `codeintel serve` (or `mgmt-api`).
- **Consumer**: `services/agent-memory/web/` (NEW).
- **Surface**: three JSON-over-HTTP endpoints (see S3.7 and
  **tech-spec.md S6.1** for verb/path/query parameters):
  - `GET /api/repos` -> repo list (id, url, sha, generatedAt).
  - `GET /api/diagram/module?repo=<id>&granularity=<package|file|class>`.
  - `GET /api/diagram/calls?repo=<id>&seed=<sig-or-id>&depth=<n>&direction=<both|callers|callees>`.
- All responses share the diagram envelope from S4.4.

### 5.7 CLI -> embedding pipeline (opt-in)

- **Producer**: `cmd/codeintel scan --with-embeddings`.
- **Consumer**: `internal/embedding.Publisher`
  (`embedding.AsASTPublisher`).
- **Surface**: the existing
  `ast.WithEmbeddingPublisher(embedding.AsASTPublisher(publisher))`
  option on the dispatcher (see
  `services/agent-memory/cmd/repoindexer/main.go` for the production
  wiring). When `--with-embeddings` is absent the CLI does NOT
  construct the publisher and the Qdrant / embedder env vars are not
  required.

### 5.8 Logging / telemetry boundaries

- The dispatcher's `ast.dispatch.skip` and `ast.parse.error` log
  events are unchanged. The CLI's summary aggregates them by
  `reason` and prints to stdout.
- The Postgres backend's `graphwriter.<op>` / `graphwriter.<op>.failed`
  records continue to fire under `--store=postgres`. SQLite and memory
  backends emit a parallel `graphsink.sqlite.<op>` /
  `graphsink.memory.<op>` shape catalogued in **tech-spec.md S9** so
  log consumers can tell which backend produced a record.
- OTel spans (`repoindexer.process_job`) are NOT emitted by the CLI by
  default -- there is no queue. A future enhancement may add a
  `codeintel.scan` span; it is out of scope for this story.

---

## 6. End-to-End Sequence Flows

The four primary scenarios. Detailed step-by-step walkthroughs (with
inputs/outputs and golden assertions) live in **e2e-scenarios.md**;
this section captures the component interactions only.

### 6.1 Scenario A -- Scan one local repository to SQLite

User runs `codeintel scan /path/to/repo --store sqlite --out repo.db`.

```
User       CLI               LocalDirMaterializer   AncestryWriter        ast.Dispatcher    graphsink.sqlite.Sink   pkg/fingerprint
 |           |                       |                    |                    |                    |                    |
 |--scan---->|                       |                    |                    |                    |                    |
 |           |--Open("repo.db")------------------------------------------------------------------>  |                    |
 |           |--mtime-tree hash for /path/to/repo (S9.1) ----------------------> sha := <hex>        |                    |
 |           |--Materialize(file://...,sha)-------------->|                    |                    |                    |
 |           |<--Workspace-----------|                    |                    |                    |                    |
 |           |--NewAncestryWriter(sink, URL, sha) ---------------------------->                                           |
 |           |                                                                                                            |
 |           |== 1. Pre-walk ancestry (AncestryWriter.EnsureRepoAndCommit) =====================|                       |
 |           |  // RepoID is NOT supplied by the CLI; the sink allocates it.                                              |
 |           |--EnsureRepoAndCommit(ctx, defaultBranch, hints) --------------->|                                          |
 |           |                                                                  |--Sink.EnsureRepo(RepoInput{URL,...})-->|
 |           |                                                                  |<-- RepoRecord{RepoID, RepoUUID, ...}---|
 |           |                                                                  |--Sink.EnsureCommit(RepoID, SHA, ...)-->|
 |           |                                                                  |<-- CommitID ---------------------------|
 |           |                                                                  |--Sink.InsertNode(kind=repo,             |
 |           |                                                                  |   sig=CanonicalRepoSig(URL),            |
 |           |                                                                  |   parent=NULL,                          |
 |           |                                                                  |   repoID=RepoID, from_sha=SHA)--------->|--NodeFingerprint------>|
 |           |                                                                  |<-- RepoNodeID --------------------------|
 |           |<--RepoAncestry{RepoID, RepoUUID, RepoNodeID, CommitID, CommitInserted}                                     |
 |           |                                                                                                            |
 |           |== 2. Per-WalkFile (AncestryWriter.EnsureFile) ===================================|                       |
 |           |--Walk(WalkFn)-------->|                                                                                    |
 |           |<--WalkFile{RelPath,Reader}--                                                                               |
 |           |--EnsureFile(ctx, WalkFile) ------------------------------------>|                                          |
 |           |                                                                  |  dir := CanonicalPackageDir(RelPath)    |
 |           |                                                                  |  if dir not in pkg-cache:               |
 |           |                                                                  |    InsertNode(kind=package,             |
 |           |                                                                  |      sig=CanonicalPackageSig(URL,dir),  |
 |           |                                                                  |      parent=RepoNodeID,                 |
 |           |                                                                  |      repoID=RepoID)------------------>  |--NodeFingerprint------>|
 |           |                                                                  |<-- PackageNodeID                        |
 |           |                                                                  |    InsertEdge(kind=contains,            |
 |           |                                                                  |      src=RepoNodeID,                    |
 |           |                                                                  |      dst=PackageNodeID,                 |
 |           |                                                                  |      repoID=RepoID)------------------>  |--EdgeFingerprint------>|
 |           |                                                                  |    cache[dir] = PackageNodeID           |
 |           |                                                                  |  InsertNode(kind=file,                  |
 |           |                                                                  |    sig=CanonicalFileSig(URL,RelPath),   |
 |           |                                                                  |    parent=PackageNodeID,                |
 |           |                                                                  |    repoID=RepoID)-------------------->  |--NodeFingerprint------>|
 |           |                                                                  |<-- FileNodeID                           |
 |           |                                                                  |  InsertEdge(kind=contains,              |
 |           |                                                                  |    src=PackageNodeID, dst=FileNodeID,   |
 |           |                                                                  |    repoID=RepoID)-------------------->  |--EdgeFingerprint------>|
 |           |<--FileAncestry{FileNodeID, PackageNodeID, ...}-----------------|                                          |
 |           |                                                                                                            |
 |           |== 3. Dispatcher emits class/method/block + static edges ========================|                       |
 |           |  EmitFile(EmitFileEvent{                                                                                   |
 |           |    RepoID,           // == ancestry.RepoID                                                                 |
 |           |    RepoURL, SHA,                                                                                           |
 |           |    FileNodeID, RepoNodeID,                                                                                 |
 |           |    RelPath, AbsPath, LanguageHints, Open})----------------------->|                                       |
 |           |                                                                   |--Parse(src)--> (per-language Parser)  |
 |           |                                                                   |  Pass 0: imports                      |
 |           |                                                                   |    InsertNode(kind=package,           |
 |           |                                                                   |      parent=RepoNodeID)-------------->|--NodeFingerprint------>|
 |           |                                                                   |    InsertEdge(kind=imports)---------->|--EdgeFingerprint------>|
 |           |                                                                   |  Pass 1a: classes                     |
 |           |                                                                   |    InsertNode(kind=class,             |
 |           |                                                                   |      parent=FileNodeID)-------------->|                       |
 |           |                                                                   |  Pass 1b: methods                     |
 |           |                                                                   |    InsertNode(kind=method,            |
 |           |                                                                   |      parent=class|file)-------------->|                       |
 |           |                                                                   |  Pass 2a/2b/2d: extends/implements/   |
 |           |                                                                   |    static_calls/overrides Edges------>|                       |
 |           |<--EmitResult.TouchedNodes (ignored by CLI)-----------------------|                                       |
 |           |                                                                                                            |
 |           |--Flush + Close-------------------------------------------------------------------> |                       |
 |<--summary-|                                                                                                            |
```

Key invariants:
- The CLI does NOT pre-allocate `fingerprint.RepoID`. It is the
  RETURN value of `EnsureRepoAndCommit`, populated either by
  Postgres's `gen_random_uuid()` default or by the SQLite/memory
  sink's `fingerprint.RepoIDFromURL` helper. Both shapes preserve
  the S2 backend-parity rule: scanning the same URL twice (even
  across backends) yields the same `RepoID` for the SQLite/memory
  case, and the existing `EnsureRepo` URL-upsert idempotence holds
  for the Postgres case.
- The pre-walk ancestry (`EnsureRepoAndCommit`) MUST run before any
  `EmitFile`; `EmitFile`'s `RepoNodeID` field is only meaningful
  after the `repo` Node is inserted.
- The package cache is per-scan and single-writer. The CLI's loop is
  sequential -- no queue, no `FOR UPDATE SKIP LOCKED`. Parallelism is
  a future enhancement (per-package fan-out would need to serialise
  the cache).
- `graphsink.sqlite.Sink.Close` is what commits the final SQLite
  transaction; the CLI MUST defer it.
- The CLI does NOT call `ast.WithEmbeddingPublisher` -- per S4 the
  embedding hook is opt-in.

### 6.2 Scenario B -- Scan many remote repositories

User runs `codeintel scan-many manifest.txt --store sqlite --out-dir ./scans`.

```
CLI                         Loop driver       GitMaterializer   ast.Dispatcher    graphsink.sqlite.Sink (per repo)
 |--- read manifest ------->|                                                                  |
 |                          |--- for each (url, sha):                                          |
 |                          |     Open("./scans/<slug>.db") --->----------------------------->|
 |                          |     Materialize(url, sha) -------> (clone --depth=1)            |
 |                          |     run Scenario A inner loop                                   |
 |                          |     Close the per-repo Sink ----------------------------------->|
 |                          |     append result line (counts, skipped reasons)                |
 |<--- aggregated summary --|                                                                  |
```

Per-repo failure isolation: a single repo whose `git fetch` fails (or
whose graph write errors) is recorded in the summary with a status of
`failed: <reason>` and the loop continues. The exit code policy is in
**tech-spec.md S4.3**.

**Decision (pinned, S9.4):** one `.db` file per repo. A single-file
multi-repo layout (`repo_id` column) was considered and is deferred
to a future workstream because it would force every read path to add
a `repo_id` predicate that the Postgres backend does not need, and
the diagram projector is already per-repo. Operators who want
co-located storage can keep the files under a shared directory.

### 6.3 Scenario C -- Build the module diagram

User runs `codeintel diagram module --store sqlite --db repo.db --granularity package --out module.json`.

```
User       CLI         graphsink.sqlite.Reader      diagram.BuildModuleDiagram          (filesystem)
 |          |                                                                                  |
 |---->-----|--- Open("repo.db") ------------------------>|                                    |
 |          |--- ListNodes(kinds=[repo]) -------->         |                                    |
 |          |--- ListNodes(kinds=[package],                                                    |
 |          |              ParentNodeID=repoID) ---->      |                                    |
 |          |--- ListNodes(kinds=[file]/[class]            |                                    |
 |          |              for each package, if            |                                    |
 |          |              granularity != "package") -->    |                                    |
 |          |--- ListEdgesFrom(kinds=[imports]) for                                             |
 |          |              each file Node ---------->      |                                    |
 |          |                                                                                  |
 |          |--- BuildModuleDiagram -------------------------------------------->|              |
 |          |    roll up file->file imports into pkg->pkg edges with weight       |              |
 |          |    emit contains edges from the hierarchy walk                      |              |
 |          |    clamp at MaxListLimit, set truncated/stats                       |              |
 |          |<--- diagram envelope ----------------------------------------------|              |
 |          |--- Write JSON to module.json --------------------------------------------------->|
 |<---------|                                                                                  |
```

The hierarchy walk uses `ListNodes` with `ListNodesFilter.ParentNodeID`
to step level-by-level -- the same pattern the existing AGENT-MEMORY
reader expects (see the doc comment on
`graphreader.ListNodesFilter.ParentNodeID`).

### 6.4 Scenario D -- Build and re-seed a call chain

User opens the React UI, picks a repo, switches to "Call Chain" mode,
selects a method, clicks an adjacent node to re-seed.

```
Browser (web/)         serve endpoint            diagram.BuildCallChain        graphsink.Reader
   |                          |                                                       |
   |--- GET /api/repos ------>|                                                       |
   |<-- repo list ------------|                                                       |
   |                          |                                                       |
   |--- user picks repo + mode=callchain + seed=<sig>                                 |
   |                                                                                  |
   |--- GET /api/diagram/calls?repo=<id>&seed=<sig>&depth=2&direction=both --------->|
   |                          |--- LookupBySignature(seed) ----------------------->   |
   |                          |--- BuildCallChain                                     |
   |                          |     queue := [seed]; depth := 0                       |
   |                          |     while depth < 2:                                  |
   |                          |       for n in current frontier:                      |
   |                          |         if direction in (both, callees):              |
   |                          |           ListEdgesFrom(n, kinds=[static_calls,       |
   |                          |                                    observed_calls])   |
   |                          |         if direction in (both, callers):              |
   |                          |           ListEdgesTo(...)                            |
   |                          |       advance frontier; depth++                       |
   |                          |     clamp, set truncated                              |
   |<---- diagram envelope ---|                                                       |
   |                          |                                                       |
   |--- render with NVL (hierarchical-left-right)                                     |
   |                                                                                  |
   |--- user clicks node X                                                            |
   |--- GET /api/diagram/calls?repo=<id>&seed=<X-id>&depth=2&direction=both --------->|
   |          (the loop above repeats with X as the new seed)                         |
```

Interactive re-seeding is the only stateful UI behaviour. Everything
else is request/response -- the UI keeps no graph state across
toggles, which matches the React-with-fetch convention used elsewhere
in the org.

### 6.5 Scenario E -- Scan with `--store=postgres` (production-identical)

Same as Scenario A except the CLI selects the Postgres backend and
takes the same `AGENT_MEMORY_PG_URL` env var the
`cmd/repoindexer/main.go` worker uses. Because the backend forwards
1:1 to `*graphwriter.Writer`, the resulting `node` / `edge` rows are
byte-identical to what the queue worker would produce -- the
backend-parity test in **implementation-plan.md S6** asserts equal `(repo_id,
fingerprint)` tuples across a `--store=sqlite` scan and a
`--store=postgres` scan of the same fixture.

This is the only path that exercises the AGENT-MEMORY G5 tombstone /
retirement invariants. The CLI does NOT trigger the delta handler; if
the operator wants delta semantics in production they continue to
enqueue an `ingest_jobs` row and let the worker process it. The CLI's
Postgres path is "another mode that inserts the same Nodes and Edges",
not a queue substitute.

---

## 7. Cross-Cutting Concerns

### 7.1 CGO and PowerShell preconditions

Parser coverage is determined at build time by the `parsers_cgo.go` /
`parsers_nocgo.go` pair. The CLI MUST surface coverage at runtime:

- On every scan, the dispatcher emits `ast.dispatch.skip` with
  `reason="no_parser"` for files whose extension has no registered
  parser. The CLI summary aggregates these by extension and prints
  them.
- The PowerShell parser returns `ErrParserUnavailable` (with the
  embedded slug `reason=pwsh_not_available`) when `pwsh` is missing
  from PATH; the dispatcher converts this into the same skip event,
  and the CLI summary aggregates it.
- The CLI exit code is 0 on a coverage-degraded scan -- the operator
  decides whether to install missing toolchains. Non-zero exit is
  reserved for fatal errors (config, IO, write contract).

The exact summary format is in **tech-spec.md S10**.

### 7.2 Idempotency and re-scan semantics

- **Postgres backend**: identical to the queue worker. Re-scanning the
  same `(repo_id, sha)` produces zero new Nodes / Edges (the
  `(repo_id, fingerprint)` UNIQUE index drives idempotence).
- **SQLite backend**: re-scan opens a fresh DB file (the previous one
  is rotated aside per **tech-spec.md S8.2**). There is no notion of
  in-place delta on SQLite; this is the S5 snapshot-store rule.
- **Memory backend**: every invocation is a fresh scan. JSON export is
  always whole-graph.

### 7.3 Truncation

`MaxListLimit = 10_000` (defined in
`services/agent-memory/internal/graphreader/reader.go`) applies to
every list-style read in the projector. The projector also enforces a
**diagram-level cap** (default same value) on `len(nodes) +
len(edges)`; when either cap is hit, `truncated = true` and
`stats.cappedAt` is set so the UI's badge fires.

Cursor-based pagination on diagrams is out of scope for this story --
the truncated badge is the contract.

### 7.4 Security and trust boundaries

- The CLI is a **developer tool**, not a network-exposed service. It
  reads source files in the local filesystem or fetches them from a
  user-supplied git URL with the user's own credentials.
- The serve endpoint listens on the user-chosen address (default
  `:8088`). It exposes read-only diagram endpoints; no auth, no
  CSRF, no rate limit. The intended deployment is `localhost`. Wider
  exposure is out of scope for this story.
- When `--store=postgres` is used, the CLI MUST connect with a role
  that has the same grants as the queue worker (typically
  `agent_memory_app`). `WriteContractViolation` (SQLSTATE 42501)
  surfaces to the user verbatim if the role is wrong.
- The UI bundles no auth library. It assumes the serve endpoint and
  the dev origin share the same trust boundary.

### 7.5 Observability

- CLI stdout: per-scan summary (files walked, files parsed, nodes by
  kind, edges by kind, skipped by reason).
- Structured logs: the dispatcher's events flow through `slog.Default`
  as today; the CLI sets a `text` handler by default and a `json`
  handler when invoked with `--log=json`. The graphsink backends emit
  one record per insert under op names listed in S5.8.
- OTel: not wired by the CLI. The serve endpoint inherits whatever
  the host process provides (none by default).

---

## 8. Risks and Mitigations

| ID | Risk | Mitigation |
| --- | --- | --- |
| R1 | A CGO=0 build silently degrades to PowerShell-only coverage. | S7 / S7.1 -- the CLI summary reports `no_parser` per extension, and the docs warn about CGO + `pwsh` prerequisites (**implementation-plan.md S2.1**). |
| R2 | Backends diverge on fingerprint computation and split node identity. | S2 -- all three backends route through `pkg/fingerprint`; backend-parity golden test in **implementation-plan.md S6** catches drift. |
| R3 | SQLite store collides with a future Postgres re-replay. | S5 -- SQLite is a snapshot store, never replayed in place; replay is "rescan against Postgres" which uses the same fingerprint pre-image. |
| R4 | Truncation goes unnoticed on large repos. | S6 + S7.3 -- the diagram envelope carries `truncated` + `stats.cappedAt`; the UI renders a badge. |
| R5 | The dispatcher's same-file-only resolution makes call chains look sparse across files. | Inherit the AGENT-MEMORY A4 contract; UI Inspector exposes `attrs.calls_raw` so consumers see the parser's pre-resolver view. Tech-spec S7 covers the UI copy. |
| R6 | The memory backend blows RAM on large monorepos. | Default CLI to SQLite; memory mode is documented as one-shot/small (**implementation-plan.md S4**). |
| R7 | Postgres backend bypasses `graphwriter` audit logging. | The Postgres backend is a thin adapter that delegates to `*graphwriter.Writer` -- it MUST NOT issue direct SQL. The interface boundary (S3.2.2) is the lint rule. |
| R8 | UI fetches against a serve endpoint that has CORS misconfigured. | **tech-spec.md S6.3** pins the dev origin (`http://localhost:5173`) on the serve endpoint; **implementation-plan.md S5** exercises the path with a Vite dev server. |

---

## 9. Pinned Defaults (Architect Decisions)

The following decisions were owner-pinnable in iteration 1 and are
pinned here so the architecture is implementable without further
operator input. Each pinned default is referenced from the section
that depends on it; downstream artifacts (`tech-spec.md`,
`implementation-plan.md`, `e2e-scenarios.md`) inherit these without
further negotiation.

| ID | Decision | Pinned default | Anchor | Rejected alternative |
| --- | --- | --- | --- | --- |
| S9.1 | Local-scan `sha` sentinel | Deterministic hex hash of the directory's mtime tree (see S4.3 for the exact construction); operator may override via `--sha`. | S4.3 | Literal string `local` -- collides for every non-git scan, breaking `(repo_id, sha)` re-scan dedupe whenever directory contents change. |
| S9.2 | UI transport | Both supported. `codeintel serve` for live navigation; static JSON for embedding in docs / handing off offline. The diagram envelope (S4.4) is the only contract. | S3.7, S6.3 | "Serve only" would block the static-export use case the story description calls out. |
| S9.3 | CLI framework | Vendored cobra-style library (`github.com/spf13/cobra`, added in this story). Per-command help, completions, and persistent flags justify the dependency given five subcommands (`scan`, `scan-many`, `diagram module`, `diagram calls`, `serve`). | S3.1 | Stdlib `flag` -- matches `cmd/qdrant-bootstrap/main.go` but the subcommand fan-out turns into hand-rolled dispatching. |
| S9.4 | Multi-repo SQLite layout | One `.db` file per repo. | S3.2.3, S6.2 | One file with a `repo_id` column -- adds a predicate every read path needs and complicates the projector for no immediate benefit. |
| S9.5 | Module-diagram default granularity | `package`, with drill-down to `file` and `class` via the `--granularity` flag. | S3.6, S4.4, S6.3 | `file` as the default -- buries the package-level summary the story description (Goal 4) asks for. |

All five decisions in this table were pinned by the operator on
2026-05-30 (iter 3); rows S9.1 and S9.3 invert the iter-2 architect's
recommendation, S9.2 / S9.4 / S9.5 confirm it. Future workstreams MAY
revisit any of these (each cell names its rejected alternative so the
trade-off is recoverable), but no downstream artifact in this story
(`tech-spec.md`, `implementation-plan.md`, `e2e-scenarios.md`) is
allowed to assume a different default.

---

## Iteration history

- 2026-05-30: initial draft -- component map, data model, interface
  catalogue, four primary scenarios, risks, and five open questions.
- 2026-05-31: iter 2 -- added S3.4 AncestryWriter component
  (factored from `worker.runFull`); rewrote S5.2 and S6.1 to surface
  the repo / package / file Node + `contains` Edge ancestry the queue
  worker performs today; rewrote S4.1 and S4.2 fingerprint pre-images
  to match `pkg/fingerprint`'s NUL framing; reworded the header /
  S1.2 to "downstream artifact" language; pinned all five iter-1
  open questions as defaults in S9; section S9 retitled to "Pinned
  Defaults".
- 2026-05-30 (iter 3): repaired the impossible CLI-facing surface on
  `AncestryWriter.EnsureRepoAndCommit` -- the method no longer accepts
  `repoID fingerprint.RepoID` as input because a first-time CLI scan
  cannot know it before `Sink.EnsureRepo` allocates the row; the
  RepoID is now an OUTPUT carried in the returned `RepoAncestry`, and
  `EnsureFile` reads it from the writer's cached ancestry. SQLite /
  memory backends derive a deterministic `fingerprint.RepoID` via
  `RepoIDFromURL(URL)` (a new helper) so backend-parity holds without
  a Postgres trip. Operator-pinned defaults applied: S9.1 SHA sentinel
  flipped from literal `local` to a deterministic mtime-tree hash;
  S9.3 CLI framework flipped from stdlib `flag` to vendored
  `github.com/spf13/cobra`. Stray sibling-doc name `acceptance.md`
  replaced with `e2e-scenarios.md` everywhere; the four `impl-plan`
  shorthand references in the risks table and S3.2.1 / S6.5 expanded
  to the canonical `implementation-plan.md` per the sequential
  plan-doc set (architecture -> tech-spec -> implementation-plan ->
  e2e-scenarios).
