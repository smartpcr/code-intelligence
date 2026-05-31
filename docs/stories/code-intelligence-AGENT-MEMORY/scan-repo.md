# Scan-Repo: CLI Code-Intelligence + Graph Diagrams

> Working plan for the `scan-repo` capability on top of the
> agent-memory service. This doc owns the **goals**, the
> **current-state assessment**, the **research / feasible plan**,
> the **data contracts** for the two diagram families, and the
> **acceptance criteria**. It is grounded in the code as of
> commit `6c93d86` (branch `main`, 2026-05-30).
>
> Companion authoritative specs (do not contradict; docs win per
> repo READMEs):
> - `docs/stories/code-intelligence-AGENT-MEMORY/architecture.md`
> - `docs/stories/code-intelligence-AGENT-MEMORY/tech-spec.md`
> - `docs/stories/code-intelligence-AST-PARSER-FOR-ADDIT/{tech-spec,architecture}.md`

---

## 1. Goals

A developer should be able to:

1. **Scan many external repositories** from a command line — by
   local path or by `git URL @ sha` — with a single binary, and
   without standing up the full production stack (Postgres +
   Qdrant + OTel) just to look at a repo.
2. **Gain code understanding / intelligence** of each scanned
   repo: the structural graph (repo → package → file → class →
   method → block) plus the static relationships (`contains`,
   `imports`, `extends`, `implements`, `static_calls`, `reads`,
   `writes`, `overrides`) extracted by the existing AST
   dispatcher.
3. **Cover all supported languages**: TypeScript/JavaScript,
   Python (v1 set) **plus** C, C++, C#, Go, Rust, PowerShell
   (the AST-PARSER-FOR-ADDIT set).
4. **Generate two diagram families** from the graph:
   - **Top-down**: module/component structure and the
     interactions between components (containment + imports).
   - **Left-right**: call chains (caller → callee) rooted at a
     chosen symbol, walked both directions.
5. **Render both diagrams in a React UI** using the
   `@neo4j-nvl` package family.

Non-goal for this story: cross-file call/inheritance resolution
(inherited A4 "same-file resolution only" constraint stays),
runtime/dynamic call edges beyond what the span ingestor already
produces, and production-grade auth on the new UI.

---

## 2. Current-State Assessment (grounded)

### 2.1 AST parsing — effectively complete, CGO-gated

The polyglot dispatcher lives at
`services/agent-memory/internal/repoindexer/ast/`. All six
additional languages are **already implemented and registered**:

| Language | Parser file | Registration |
| --- | --- | --- |
| C | `parser_treesitter_c.go` | `parsers_cgo.go` |
| C++ | `parser_treesitter_cpp.go` | `parsers_cgo.go` |
| C# | `parser_treesitter_csharp.go` | `parsers_cgo.go` |
| Go | `parser_treesitter_go.go` | `parsers_cgo.go` |
| Rust | `parser_treesitter_rust.go` | `parsers_cgo.go` |
| PowerShell | `parser_powershell.go` | `parsers_cgo.go` **and** `parsers_nocgo.go` |
| TS/JS, Python | `parser_typescript.go`, `parser_python.go` (+ tree-sitter variants) | existing v1 |

`services/agent-memory/internal/repoindexer/ast/parsers_cgo.go`
registers `NewTreeSitterCParser … NewPowerShellParser`;
`parsers_nocgo.go` registers **only** PowerShell. The five
tree-sitter parsers are **CGO-only by design** (tech-spec C7 /
§4.3): a CGO=0 build silently skips `.c/.cpp/.cs/.go/.rs` with
`ast.dispatch.skip{reason:"no_parser"}`.

**Implication for "scan many external repos in any language":**
the CLI **must be built with `CGO_ENABLED=1`** and a working C
toolchain, or it degrades to PowerShell-only on the additional
set. This is the single biggest portability constraint and is
called out again in §7.

Grammar dependency: `github.com/smacker/go-tree-sitter
v0.0.0-20240827094217-dd81d9e9be82` (already in `go.mod`).

### 2.2 Repo indexer — a queue worker, not a CLI

`cmd/repoindexer/main.go` is a **long-running worker**:

- Requires `AGENT_MEMORY_PG_URL` **and** `AGENT_MEMORY_QDRANT_URL`
  (both REQUIRED; it `os.Exit(3)` on a bad ping).
- Requires an embedder; only a zero-vector **stub** exists today
  (`AGENT_MEMORY_ALLOW_STUB_EMBEDDER=true`).
- It does not take a repo argument — it polls `ingest_jobs`
  (`SELECT … FOR UPDATE SKIP LOCKED`) and processes whatever the
  mgmt-api enqueued.

Enqueueing is via `internal/mgmtapi` HTTP:
`POST /v1/repos` (register) → `POST /v1/repos/{id}/ingest`
(full) / `/ingest_delta`.

The actual scanning pipeline is reusable and well-factored:

- `repoindexer.GitMaterializer.Materialize(ctx, repoURL, sha)`
  does `git init / remote add / fetch --depth=1 / checkout
  FETCH_HEAD` into a temp dir and returns a `Workspace` with a
  stable, slash-normalized `Walk` (skips `.git`, `node_modules`,
  `vendor`, `target`, `bin`, `obj`, `__pycache__`, …).
- `ast.NewDispatcher(gw, opts…)` implements `repoindexer.ASTEmitter`
  (`EmitFile`) — it is the thing that turns one file into
  Class/Method/Block nodes + static edges.
- The embedding publisher hook is **optional**
  (`ast.WithEmbeddingPublisher(...)`); without it the dispatcher
  still writes the structural graph.

**Implication:** we can drive the existing dispatcher +
materializer from a new CLI **without** the queue, and make the
embedder/Qdrant optional. The only hard coupling to remove is the
writer target (today always Postgres `graphwriter`).

### 2.3 Graph model & reader — exactly the diagram primitives

Tables `node` / `edge` (migrations `0001` enums, `0003` tables),
append-only with tombstone retirement (`0004`).

- **Node kinds:** `repo, package, file, class, method, block`.
- **Edge kinds:** `contains, imports, static_calls, observed_calls,
  extends, implements, reads, writes, renamed_to, overrides`.
- Hierarchy via `contains` and `node.parent_node_id`
  (repo→package→file→class→method→block).

`internal/graphreader/reader.go` exposes everything the diagram
builder needs:

- `ListNodes(repoID, kinds, ListNodesFilter{ParentNodeID,…})` —
  walk the containment tree level by level.
- `ListEdgesFrom(srcNodeID, kinds, …)` — callees / outbound.
- `ListEdgesTo(dstNodeID, kinds, …)` — callers / inbound.
- `GetNode` / `GetEdge`, retirement-aware, `MaxListLimit = 10_000`.

So: **top-down** = `contains` + `imports`; **left-right** =
`static_calls` (+ `observed_calls`) walked via `ListEdgesFrom/To`.

### 2.4 Frontend — greenfield

`services/agent-memory/web/` contains only `.gitkeep`. No React
app, no `@neo4j-nvl`, no graph-rendering code anywhere in the
repo. This is a clean build.

### 2.5 Gap summary

| Goal | Status | Gap |
| --- | --- | --- |
| All additional languages parse | **Done** (CGO) | Verify CGO build + a parse smoke test per language; document the CGO=0 degradation |
| CLI to scan external repos | **Missing** | New binary; decouple writer from Postgres; make embedder/Qdrant optional; add a local-path materializer |
| Code intelligence persisted | Partial | Need a storage option that does not require full infra |
| Diagram generation (2 families) | **Missing** | New graph→diagram projector + export format / HTTP endpoint |
| React + neo4j-nvl UI | **Missing** | New web app |

---

## 3. Research & Design Decisions

### 3.1 How should the CLI store the graph?

The dispatcher writes through a narrow interface (effectively
`InsertNode` + `InsertEdge`; see `ast.NewDispatcher` /
`nodeEdgeWriter`). Three storage strategies are feasible:

| Option | Pros | Cons | Verdict |
| --- | --- | --- | --- |
| **A. Postgres (reuse `graphwriter`)** | Production-identical; reuses `graphreader`; delta/retirement already work | Heavy for "scan many repos quickly" — needs PG running, migrations, roles | Keep as the **production / `--store=postgres`** path |
| **B. Embedded SQLite store** | Single file per repo; no services; `graphreader`-shaped queries portable | New writer + reader impl; must mirror node/edge schema & fingerprint rules | **Recommended default** (`--store=sqlite`) |
| **C. In-memory + JSON/NDJSON export** | Zero deps; trivial; perfect feeding the UI directly | No incremental/delta; whole-repo only; large repos held in RAM | Ship as `--store=memory --export graph.json` for quick one-shots |

**Decision:** introduce a small `graphsink` abstraction that the
dispatcher writes to, with three backends (Postgres adapter wraps
the existing `graphwriter`; SQLite; in-memory). Default to
SQLite for the CLI; allow `--store=postgres` to opt into the
production store. This is the key feasibility lever — it removes
the Qdrant/PG hard requirement for the "understand a repo"
use case while preserving one code path through the dispatcher.

> Fingerprint/canonical-signature rules (G2) must be identical
> across backends so a repo scanned to SQLite and later to
> Postgres yields the same node identities. Reuse
> `pkg/fingerprint` and the dispatcher's signature helpers
> verbatim — backends only differ in the persistence call.

### 3.2 How should the CLI get the source tree?

`GitMaterializer` already handles `git URL @ sha`. For "scan an
external repo I already have checked out," add a
`LocalDirMaterializer` implementing the same `Materializer` /
`Workspace` interface (`internal/repoindexer/materialize.go`)
that walks an existing directory with the same exclude-dir set.
This is a small, well-bounded addition (mirror `gitWorkspace`'s
`Walk`, skip the git fetch).

For the `repoURL` / canonical-signature inputs the dispatcher
needs (signatures embed the repo URL), the CLI synthesizes a
stable repo identity for local scans (e.g.
`file://<abs-or-normalized-path>` and `sha = "local"` or the
working-tree `git rev-parse HEAD` when available).

### 3.3 Embeddings: optional

Diagrams need only the structural graph + static edges.
Embeddings (Qdrant) power semantic recall, not diagrams. The CLI
constructs the dispatcher **without** `WithEmbeddingPublisher`,
so Qdrant and the embedder are not required for scan/diagram.
A `--with-embeddings` flag can opt back into the publisher for
users who also run the recall stack.

### 3.4 Diagram projection: server-side, two query shapes

Build the diagram JSON in Go (a `diagram` package) reading from
whichever `graphsink`/reader backend is active, so the same
projection serves the CLI (`codeintel diagram …`) and an HTTP
endpoint the UI calls. Two builders:

1. **Module/component (top-down).** Choose a granularity
   (`package` for components, `file`/`class` for detail). Nodes =
   nodes of that kind under the repo; edges =
   (a) `contains` for the hierarchy and (b) `imports`
   aggregated/rolled up to the chosen granularity for
   cross-component interactions. Roll-up: an `imports` edge
   between two files becomes a component edge between their owning
   packages, deduped with a `weight` = count.
2. **Call chain (left-right).** Given a seed node (method/function
   resolved by `canonical_signature` or id), BFS:
   - downstream (callees) via `ListEdgesFrom(kinds=["static_calls",
     "observed_calls"])`,
   - upstream (callers) via `ListEdgesTo(...)`,
   - bounded by `--depth` and a node cap; annotate each edge with
     its kind (static vs observed) for styling.

Both builders emit the **same diagram JSON contract** (§5) so the
UI has one renderer with two layout presets.

### 3.5 UI: neo4j-nvl

`@neo4j-nvl/react` (`InteractiveNvlWrapper`) + `@neo4j-nvl/base`
render `nodes: {id, caption, color, size}` and
`relationships: {id, from, to, caption, color}`. NVL supports a
**hierarchical** layout (good for both top-down module trees and
left-right call chains via layout direction) and a force-directed
layout (good for dense import graphs). The UI fetches the diagram
JSON, maps it to NVL's node/relationship shape, and toggles
layout per diagram family. This is a thin mapping layer — the
intelligence is server-side.

---

## 4. Feasible Implementation Plan (phased)

Each phase is independently shippable and verifiable.

### Phase 0 — Verify parser coverage (no new code, gates the rest)
- Build the ast package with `CGO_ENABLED=1` and the C toolchain.
- Add/confirm a per-language parse smoke test (one fixture each:
  class/type, method, free function, import, same-file call). The
  AST-PARSER-FOR-ADDIT story already ships these
  (`parser_treesitter_*_test.go`, `parser_powershell_test.go`) —
  run them and record the support matrix in
  `.claude/context/tests.md`.
- Confirm CGO=0 behavior (only PowerShell registered) and
  document the degradation.

### Phase 1 — `graphsink` storage abstraction
- Define `graphsink` interface (InsertNode/InsertEdge + flush/
  close) under `internal/` and adapt the existing `graphwriter`
  to it (Postgres backend).
- Implement the **SQLite** backend (mirror `node`/`edge` columns;
  reuse `pkg/fingerprint`).
- Implement the **in-memory + JSON export** backend.
- Wire the dispatcher to accept any `graphsink` (it already takes
  a writer; this generalizes the type).

### Phase 2 — `LocalDirMaterializer`
- New `Materializer` for an on-disk directory, same `Walk` /
  exclude-dir semantics as `gitWorkspace`.
- Synthesize repo identity for local scans.

### Phase 3 — `codeintel` CLI binary (`cmd/codeintel`)
- `codeintel scan <path|git-url> [--sha] [--store sqlite|postgres|memory]
  [--out graph.db|graph.json] [--lang-hints …] [--with-embeddings]`
  drives Materializer → dispatcher → graphsink synchronously
  (no queue), prints a summary (files parsed, nodes/edges by
  kind, skipped-by-`no_parser` counts).
- `codeintel scan-many <manifest>` iterates a list of repos
  (path or url@sha) for the "many external repos" goal.
- `codeintel diagram module --store … [--granularity package|file|class]
  --out module.json`
- `codeintel diagram calls --store … --seed <signature|node-id>
  --depth N [--direction both|callers|callees] --out calls.json`
- Use a real CLI framework only if already vendored; otherwise a
  small flag-based `main` matches the repo's "env/flags, no cobra"
  convention. (Check `go.mod`; prefer stdlib `flag`.)

### Phase 4 — diagram projector (`internal/diagram`)
- `BuildModuleDiagram(reader, repoID, granularity)` →
  containment + rolled-up imports.
- `BuildCallChain(reader, seed, depth, direction)` → BFS over
  `static_calls`/`observed_calls`.
- Both return the §5 contract. Unit-test against a fixed fixture
  graph with golden JSON.

### Phase 5 — read/serve endpoint (optional but recommended for UI)
- Either: `codeintel serve --store … --addr :8088` exposing
  `GET /api/repos`, `GET /api/diagram/module`,
  `GET /api/diagram/calls?seed=…&depth=…`; or
- Add the same handlers to `mgmt-api` behind the existing router
  (`internal/mgmtapi`) for the Postgres-backed deployment.
- CORS for local UI dev.

### Phase 6 — React UI (`services/agent-memory/web/`)
- Vite + React + TypeScript app; deps `@neo4j-nvl/react`,
  `@neo4j-nvl/base`.
- Repo picker → diagram-type toggle (Module / Call chain) →
  NVL canvas. Module = hierarchical top-down (or force-directed
  for dense import graphs); Call chain = hierarchical left-right.
- Node click in call-chain view re-seeds the BFS (calls the
  endpoint with the clicked node as `seed`), giving interactive
  navigation.
- Inspector panel showing `attrs_json` (language, modifiers,
  params, line range) for the selected node.

### Phase 7 — docs & glue
- `services/agent-memory/web/README.md` (run/build), update
  `.claude/context/{usage,tests}.md`, and a short "scan a repo
  in 60 seconds" quickstart.

---

## 5. Diagram Data Contract (server → UI)

One JSON shape for both families; the `kind` field selects the
default layout in the UI.

```jsonc
{
  "diagram": "module" | "callchain",
  "repo": { "id": "…", "url": "file://…", "sha": "…" },
  "generatedAt": "2026-05-30T…Z",
  "layoutHint": "hierarchical-top-down" | "hierarchical-left-right" | "force",
  "nodes": [
    {
      "id": "node-uuid-or-synthetic",
      "label": "GreeterImpl.greet",     // caption
      "kind": "package|file|class|method|block",
      "language": "go",
      "group": "owning-package-or-file", // for clustering/color
      "attrs": { "modifiers": ["pub"], "startLine": 12, "endLine": 40 }
    }
  ],
  "edges": [
    {
      "id": "edge-uuid-or-synthetic",
      "from": "node-id",
      "to": "node-id",
      "kind": "contains|imports|static_calls|observed_calls|extends|implements|overrides|reads|writes",
      "weight": 1,                       // rolled-up count for module imports
      "label": "imports"
    }
  ],
  "truncated": false,                     // true if a cap/limit clipped results
  "stats": { "nodeCount": 0, "edgeCount": 0, "cappedAt": 10000 }
}
```

Notes:
- `truncated`/`cappedAt` make the `graphreader.MaxListLimit`
  (10 000) clamp **visible** instead of silently lossy.
- Synthetic ids are used for the in-memory/JSON backend and for
  rolled-up module nodes; they must be stable across runs.

### 5.1 neo4j-nvl mapping (UI)

```ts
const nvlNodes = diagram.nodes.map(n => ({
  id: n.id,
  caption: n.label,
  color: colorByGroupOrLanguage(n),
  size: n.kind === "package" ? 40 : 20,
}));
const nvlRels = diagram.edges.map(e => ({
  id: e.id, from: e.from, to: e.to,
  caption: e.label,
  width: 1 + Math.log2(e.weight || 1),
  color: e.kind === "observed_calls" ? "#888" : undefined,
}));
// <InteractiveNvlWrapper nodes={nvlNodes} rels={nvlRels}
//    nvlOptions={{ layout: diagram.layoutHint.startsWith("hierarchical")
//      ? "hierarchical" : "forceDirected" }} />
```

---

## 6. Architecture (target)

```
            codeintel CLI                         React UI (web/)
   scan | scan-many | diagram | serve        @neo4j-nvl/react canvas
        |                    |                        |  fetch JSON
        v                    v                        v
  Materializer        diagram projector  <----  serve endpoint / static JSON
  (Git | LocalDir)    (internal/diagram)              ^
        |                    ^                         |
        v                    | reads                   |
   ast.Dispatcher  --writes--> graphsink (Postgres | SQLite | memory)
   (EmitFile)                       ^
        |                           |
        +-- tree-sitter parsers (CGO): TS/JS, Py, C, C++, C#, Go, Rust
        +-- PowerShell subprocess parser (pwsh)
```

Everything left of `graphsink` already exists; the new work is
the `graphsink` generalization, `LocalDirMaterializer`, the CLI,
the `diagram` projector, the serve endpoint, and the UI.

---

## 7. Risks & Constraints

| ID | Risk | Mitigation |
| --- | --- | --- |
| R1 | **CGO toolchain.** Tree-sitter parsers need `CGO_ENABLED=1` + a C compiler; a stock Windows build yields PowerShell-only coverage. | Ship build instructions (mingw/gcc), fail loudly in the CLI summary listing which extensions were skipped with `no_parser`; provide prebuilt CGO binaries in CI. |
| R2 | **PowerShell needs `pwsh` on PATH.** Parser returns `ErrParserUnavailable` otherwise. | CLI summary reports `pwsh_not_available` skips; document the `pwsh` dependency. |
| R3 | **Same-file resolution only (A4).** Cross-file calls/inheritance are dropped; call chains may look sparse across files. | Set expectations in UI ("same-file edges; cross-file resolver is future work"); keep `*_raw` attrs visible in the inspector. |
| R4 | **`MaxListLimit` clamp (10 000)** silently truncates large repos. | Surface `truncated`/`cappedAt` in the contract (§5); add pagination later. |
| R5 | **Fingerprint drift across backends** would split node identity. | Single source of truth: `pkg/fingerprint` + dispatcher signature helpers; backend-parity golden test. |
| R6 | **Large monorepos** blow memory in `--store=memory`. | Default CLI to SQLite; memory mode documented as one-shot/small. |
| R7 | **Append-only/tombstone invariants** (G5) must hold for the Postgres path. | Postgres backend keeps using `graphwriter`/retirement unchanged; SQLite/memory are scan-snapshot stores (full re-scan replaces). |

---

## 8. Acceptance Criteria (how we know it works)

### 8.1 Parser coverage
- [ ] `CGO_ENABLED=1` build of the ast package succeeds; the test
      suite for all eight languages passes
      (`parser_treesitter_{c,cpp,csharp,go,rust}_test.go`,
      `parser_powershell_test.go`, plus existing TS/Py).
- [ ] A scan of a fixture repo containing one file per language
      emits ≥1 `class`/type node and ≥1 `method` node for each
      language, and ≥1 `static_calls` edge for a language whose
      fixture has a same-file call.
- [ ] CGO=0 build runs and reports `no_parser` skips for
      `.c/.cpp/.cs/.go/.rs` without crashing.

### 8.2 CLI scanning
- [ ] `codeintel scan <local-path>` and
      `codeintel scan <git-url> --sha <sha>` both complete and
      print a summary: files walked, files parsed, nodes by kind,
      edges by kind, skipped (`no_parser`, `pwsh_not_available`).
- [ ] `codeintel scan-many <manifest>` processes ≥3 external
      repos in one invocation and writes one store per repo (or a
      multi-repo store), with a per-repo result line.
- [ ] Scanning with `--store=sqlite` requires **no** Postgres and
      **no** Qdrant; `--store=postgres` writes the identical
      node/edge set (parity test on a fixture: same counts, same
      canonical signatures).
- [ ] Re-scanning an unchanged repo is idempotent (same node/edge
      identities; no duplicates).

### 8.3 Diagram generation
- [ ] `codeintel diagram module` produces JSON matching §5 with
      `diagram="module"`, containment edges forming a tree under
      the repo node, and rolled-up `imports` edges between
      components (with `weight`).
- [ ] `codeintel diagram calls --seed <sig> --depth 2` produces
      JSON with `diagram="callchain"`, the seed present, callee
      nodes reachable via `static_calls`, and callers when
      `--direction both`.
- [ ] Both outputs set `truncated`/`stats` correctly; a repo
      exceeding the cap reports `truncated=true`.
- [ ] Golden-JSON unit tests for both builders pass against a
      fixed fixture graph.

### 8.4 React + neo4j-nvl UI
- [ ] The web app builds (`npm run build`) and runs
      (`npm run dev`) from `services/agent-memory/web/`.
- [ ] It renders the **module** diagram top-down and the
      **call-chain** diagram left-right using `@neo4j-nvl/react`,
      with nodes colored by language/group and edges labeled by
      kind.
- [ ] Clicking a node in the call-chain view re-seeds the BFS and
      re-renders (interactive navigation).
- [ ] The inspector panel shows the selected node's `attrs`
      (language, modifiers, line range).
- [ ] A documented end-to-end path works:
      `scan → serve/export → open UI → see both diagrams` for at
      least one real external repo per language family.

### 8.5 Docs
- [ ] `web/README.md` + a "scan in 60 seconds" quickstart exist.
- [ ] `.claude/context/tests.md` records the language support
      matrix and the CGO / `pwsh` caveats.

---

## 9. Open Questions (resolve before/within implementation)

1. **Storage default** — confirm SQLite as the CLI default vs.
   in-memory+JSON. (Recommendation: SQLite.)
2. **Serve vs. static export** — does the UI talk to a `codeintel
   serve` endpoint, read static JSON files, or both? (Recommend
   both: static for offline, serve for interactive re-seeding.)
3. **CLI framework** — stdlib `flag` (matches repo convention) or
   a vendored cobra-style lib? (Recommend `flag`.)
4. **Multi-repo store layout** — one SQLite file per repo vs. one
   file with a `repo_id` column. (Recommend per-repo file for the
   "many repos" workflow; multi-repo store optional.)
5. **Module granularity default** — `package` (components) vs.
   `file`. (Recommend `package` with drill-down to `file`/`class`.)
