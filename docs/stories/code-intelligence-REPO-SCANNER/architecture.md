# Repo Scanner -- Architecture

> Story: `code-intelligence:REPO-SCANNER` -- Points 21
> Companion docs (parallel iteration): `tech-spec.md`,
> `implementation-plan.md`, `e2e-scenarios.md`.
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
  configuration precedence -- those live in **tech-spec.md**.
- Stage-by-stage implementation order, branch list, or test-coverage
  targets -- those live in **implementation-plan.md**.
- Scenario walk-throughs with inputs/outputs and golden assertions --
  those live in **e2e-scenarios.md**.

Cross-references use the form `tech-spec.md S4` / `impl-plan S3`, never
inline duplication.

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

The CLI parses flags with stdlib `flag` to match the repo convention
(see `cmd/qdrant-bootstrap/main.go`) unless **Open Question Q3** is
resolved otherwise. Configuration precedence and per-flag semantics are
catalogued in **tech-spec.md S2**.

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
**impl-plan S6** asserts this on a fixture repo.

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

### 3.4 ast.Dispatcher (existing, generalised)

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

### 3.5 diagram projector -- `services/agent-memory/internal/diagram/` (**NEW**)

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

### 3.6 serve endpoint -- `codeintel serve` and/or `internal/mgmtapi` handlers (**NEW**)

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

### 3.7 React + neo4j-nvl UI -- `services/agent-memory/web/` (**NEW**)

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
| `fingerprint` | bytea(32) | `sha256(repo_id || kind || canonical_signature || from_sha)`; G2 dedupe key with `repo_id` (UNIQUE index `node_repo_fingerprint_uidx`). |
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
| `fingerprint` | bytea(32) | `sha256(repo_id || kind || src_fingerprint || dst_fingerprint || from_sha)`. |
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
| `sha` | Output of `git rev-parse HEAD` when the directory is a git checkout; otherwise the literal string `local` -- this is a deliberate sentinel so the canonical signatures from a non-git scan stand apart from any real commit. |
| `default_branch` | Empty string. |
| `language_hints` | Empty slice (the CLI does not surface a hint UI in v1). |

Sentinel choice for `sha` is **Open Question Q1**.

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

### 5.2 Materializer -> Dispatcher

- **Producer**: any `Workspace` (returned by Materialize).
- **Consumer**: `internal/repoindexer/ast.Dispatcher.EmitFile`.
- **Surface**:
  - `Workspace.Walk(WalkFn)` yields one `WalkFile` per source file
    (forward-slash relpaths, exclude-dir set already configured).
  - The CLI converts each `WalkFile` into an `EmitFileEvent`
    (`internal/repoindexer/ast.go`) and calls `dispatcher.EmitFile`.

The CLI does NOT need to recreate the queue worker -- it executes the
loop synchronously. The only field the queue worker uses that the CLI
must also supply is `FileNodeID`, which the CLI ensures by calling
`graphsink.InsertNode` for the file kind before each emit.

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
- **Surface**: three JSON-over-HTTP endpoints (see S3.6 and
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
User                 CLI (cmd/codeintel)         LocalDirMaterializer   ast.Dispatcher     graphsink.sqlite.Sink     pkg/fingerprint
 |                          |                            |                    |                       |                         |
 |---- scan invocation ---->|                            |                    |                       |                         |
 |                          |--- Open("repo.db") ---------------------------------------------------->|                         |
 |                          |--- Materialize("file://...", "<git-HEAD or 'local'>") ----------------->|                         |
 |                          |                            |--- Walk(WalkFn) -->|                       |                         |
 |                          |<--- ensure repo node ------|                    |                       |                         |
 |                          |--- EnsureRepo  ----------------------------------------------------->|                         |
 |                          |--- EnsureCommit ---------------------------------------------------->|                         |
 |                          |                            |                    |                       |                         |
 |                          |--- for each WalkFile :     |                    |                       |                         |
 |                          |     InsertNode(file kind) ----------------------------------------->|                         |
 |                          |     EmitFile(EmitFileEvent)|------------------->|                       |                         |
 |                          |                            |                    |--- Parse(src) ------> (per-language Parser)     |
 |                          |                            |                    |--- per pass:          |                         |
 |                          |                            |                    |    InsertNode -------->|                         |
 |                          |                            |                    |    (fingerprint via   |--- NodeFingerprint ---->|
 |                          |                            |                    |    pkg/fingerprint)   |<--- fp[32] -------------|
 |                          |                            |                    |    InsertEdge -------->|                         |
 |                          |<--- EmitResult (touched)---|--------------------|                       |                         |
 |                          |--- Flush + Close -------------------------------------------------->|                         |
 |<--- summary printed -----|                            |                    |                       |                         |
```

Key invariants:
- The CLI's loop is sequential -- no queue, no `FOR UPDATE SKIP
  LOCKED`. Parallelism is a future enhancement.
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

The one-file-per-repo layout is the default; **Open Question Q4**
discusses the alternative (a single SQLite file with a `repo_id`
column).

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
backend-parity test in **impl-plan S6** asserts equal `(repo_id,
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
| R1 | A CGO=0 build silently degrades to PowerShell-only coverage. | S7 / S7.1 -- the CLI summary reports `no_parser` per extension, and the docs warn about CGO + `pwsh` prerequisites (impl-plan S2.1). |
| R2 | Backends diverge on fingerprint computation and split node identity. | S2 -- all three backends route through `pkg/fingerprint`; backend-parity golden test in impl-plan S6 catches drift. |
| R3 | SQLite store collides with a future Postgres re-replay. | S5 -- SQLite is a snapshot store, never replayed in place; replay is "rescan against Postgres" which uses the same fingerprint pre-image. |
| R4 | Truncation goes unnoticed on large repos. | S6 + S7.3 -- the diagram envelope carries `truncated` + `stats.cappedAt`; the UI renders a badge. |
| R5 | The dispatcher's same-file-only resolution makes call chains look sparse across files. | Inherit the AGENT-MEMORY A4 contract; UI Inspector exposes `attrs.calls_raw` so consumers see the parser's pre-resolver view. Tech-spec S7 covers the UI copy. |
| R6 | The memory backend blows RAM on large monorepos. | Default CLI to SQLite; memory mode is documented as one-shot/small (impl-plan S4). |
| R7 | Postgres backend bypasses `graphwriter` audit logging. | The Postgres backend is a thin adapter that delegates to `*graphwriter.Writer` -- it MUST NOT issue direct SQL. The interface boundary (S3.2.2) is the lint rule. |
| R8 | UI fetches against a serve endpoint that has CORS misconfigured. | Tech-spec S6.3 pins the dev origin (`http://localhost:5173`) on the serve endpoint; impl-plan S5 exercises the path with a Vite dev server. |

---

## 9. Open Questions

The following decisions are still owner-pinnable; the iteration summary
emits them as structured questions for the wizard.

1. **Local-scan SHA sentinel** -- when scanning a directory that is
   not a git checkout, use `local` (current draft, S4.3) or a
   deterministic hash of the directory's mtime tree?
2. **Serve vs. static export** -- does the UI talk only to a
   `codeintel serve` endpoint, only to static JSON files, or both
   (current draft assumes "both")?
3. **CLI framework** -- stdlib `flag` (matches `cmd/qdrant-bootstrap`
   convention; current draft) or a vendored cobra-style lib?
4. **Multi-repo SQLite layout** -- one `.db` per repo (current draft,
   Section 6.2) or one file with a `repo_id` column?
5. **Module-diagram default granularity** -- `package` (current draft)
   with drill-down to file/class, or `file` directly?

---

## Iteration history

- 2026-05-30: initial draft -- component map, data model, interface
  catalogue, four primary scenarios, risks, and five open questions.
