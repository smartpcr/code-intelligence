# Agent Memory — Architecture

> Story: `code-intelligence:AGENT-MEMORY` · Points 21
> Companion docs: `tech-spec.md`, `implementation-plan.md`, `e2e-scenarios.md`
> (each owns its own scope; this file owns the component/data/interface contracts).

## 1. Purpose and Scope

This document defines the architecture of **Agent Memory**, a hybrid
graph-memory subsystem that gives a coding agent (a) a top-down map of one or
more code repositories, (b) a call-chain reasoning surface on top of that map,
and (c) a learning loop that converts every concrete agent run into reusable
expertise.

### 1.1 Greenfield anchoring

The worktree on branch
`story/code-intelligence-AGENT-MEMORY/plan-architecture` contains only
`README.md`, `.gitignore`, and `docs/`. There is no `src/` tree, no existing
service, and no prior schema. **Every component, table, and interface named in
this document is introduced by this story.** No module name in this draft refers
to pre-existing code; the implementation plan in `implementation-plan.md` is
free to choose the language/package layout when the tech spec is locked.

### 1.2 In scope / out of scope

| In scope | Out of scope |
| --- | --- |
| Hybrid graph store (structural + episodic + conceptual) | Choice of vector-DB vendor (deferred to `tech-spec.md`) |
| Public interfaces for an agent reasoning loop | UI design for the operator console (only the read contract is in scope) |
| Append-only event log with consolidation | Anonymising third-party PII inside trace payloads |
| Method-level + basic-block-level code nodes | Whole-line/statement-level static analysis |
| OpenTelemetry spans as the canonical dynamic-trace source | py-spy / perf / vendor-specific profilers (can be normalised to OTel upstream) |
| Global, cross-repo Concept space | Per-tenant Concept isolation (single tenant in v1) |
| Append-only EpisodicLog (no rotation) | Episode purge / GDPR delete tooling (separate workstream) |

### 1.3 Guiding principles

These are the contract bedrocks every later section must respect. Where you see
`Gn` referenced elsewhere in this file, it is one of these.

- **G1 — Read/write separation.** Reads (recall, expand, summarize) never
  mutate the graph. Writes (observe, ingest, feedback) never block on read
  latency.
- **G2 — Identity by fingerprint.** Every Node and every Edge is keyed by a
  deterministic 32-byte `fingerprint` whose pre-image always includes the SHA
  of first appearance. The pre-image differs by entity type because an Edge
  has no canonical_signature of its own — its identity is the ordered pair of
  its endpoints' fingerprints:
  - **Node fingerprint**: `sha256(repo_id ‖ kind ‖ canonical_signature ‖ first_seen_sha)`, where `canonical_signature` is the language-stable identifier (e.g. `pkg.Foo#bar(int)`) and `first_seen_sha` is materialised in the data model as the `from_sha` column (§5.2.1).
  - **Edge fingerprint**: `sha256(repo_id ‖ kind ‖ src_fingerprint ‖ dst_fingerprint ‖ first_seen_sha)`, where `first_seen_sha` is materialised as the `from_sha` column (§5.2.2).

  Two ingests of the same commit always produce the same fingerprints; a
  renamed or moved member produces a *new* fingerprint linked to the old one
  by a `renamed_to`/`renamed_from` Edge.
- **G3 — Append-only event log.** The `EpisodicLog` is a strict append-only
  table. Per operator decision, retention is **forever** (no rotation, no
  rewrite). Status transitions on an Episode are recorded as new
  `EpisodeUpdate` rows that reference the original Episode by id, not by
  mutating it.
- **G4 — Concepts are append-only, confidence is derived.** A `Concept` row is
  immutable once written. The Consolidator emits new `ConceptVersion` rows that
  carry the current confidence band and support count. The "current" confidence
  for a Concept is always the most recent ConceptVersion row — Concept never
  stores a writable `confidence` column.
- **G5 — Structural edges are append-only with retirement, not mutation.**
  When a commit retires an edge or node, the writer sets the retiring entity's
  `to_sha` to the parent commit of the new HEAD; the row itself is never
  rewritten. Dynamic-call edges aggregate provenance into a per-edge
  `TraceObservation` child table (mutable count), not into the edge row
  itself (see §5.2.3).
- **G6 — Cross-repo Concepts, repo-scoped Nodes/Edges.** Per operator decision,
  Concepts live in a global namespace from day one. Structural Nodes and Edges
  remain scoped to `repo_id`. Cross-repo Concepts attach to per-repo Nodes via
  `ConceptSupport` rows that carry the supporting `repo_id`.
- **G7 — Operator correction auto-promotes.** Per operator decision, when an
  operator submits a correction (`mgmt.feedback` with a `corrected_action`),
  the Consolidator immediately writes a synthetic *positive* Episode in
  addition to the negative original. The synthetic positive Episode reuses the
  parent Episode's `context_id` so the positive signal attaches to the same
  recall snapshot that produced the wrong answer (§4.8, §7.6).

---

## 2. Component Map

```
                         ┌─────────────────────────────────────┐
                         │              Operator UI            │
                         │  (read-only inspector + corrections)│
                         └──────────────┬──────────────────────┘
                                        │  Management Surface (HTTPS)
                                        ▼
┌────────────────────────────────────────────────────────────────────────┐
│                        Agent Memory Service                            │
│                                                                        │
│   ┌──────────────┐   ┌──────────────────┐   ┌──────────────────────┐   │
│   │ Agent Surface│   │  Management      │   │  Background Workers  │   │
│   │  (recall,    │   │  Surface         │   │  • Repo Indexer      │   │
│   │   observe,   │   │  (register,      │   │  • Span Ingestor     │   │
│   │   expand,    │   │   ingest,        │   │  • Consolidator      │   │
│   │   summarize) │   │   ingest_delta,  │   │  • Concept Promoter  │   │
│   └──────┬───────┘   │   ingest_spans,  │   │  • Reranker Trainer  │   │
│          │           │   feedback,      │   └──────────┬───────────┘   │
│          │           │   snapshot,      │              │               │
│          │           │   read_*)        │              │               │
│          │           └─────────┬────────┘              │               │
│          │                     │                       │               │
│          ▼                     ▼                       ▼               │
│   ┌──────────────────────────────────────────────────────────────┐    │
│   │              Hybrid Graph Store (read/write)                 │    │
│   │  • Structural Graph (Repo→Pkg→File→Class→Method→Block)       │    │
│   │  • Call/Data/Concept Edges                                   │    │
│   │  • EpisodicLog + Observations                                │    │
│   │  • ConceptStore (global) + ConceptVersion + ConceptSupport   │    │
│   │  • Embedding index (semantic recall)                         │    │
│   └──────────────────────────────────────────────────────────────┘    │
└────────────────────────────────────────────────────────────────────────┘
        ▲                              ▲                        ▲
        │ static AST + git diff        │ OTel span batches      │ git webhooks
   ┌────┴─────────┐               ┌────┴────────┐         ┌─────┴─────────┐
   │ Repo Indexer │               │  OTel       │         │ Webhook       │
   │ (workers)    │               │  Collector  │         │ Receiver      │
   └──────────────┘               └─────────────┘         └───────────────┘
```

The Agent Memory Service is a single deployable that exposes two HTTPS
surfaces and hosts the background workers in-process or as sidecars. The Hybrid
Graph Store sits behind both surfaces; it is the single source of truth for
nodes, edges, episodes, observations, and concepts.

---

## 3. Components and Responsibilities

### 3.1 Webhook Receiver

Accepts authenticated push and merge events from any configured git host.
Writes a `RepoEvent` row and enqueues a delta-ingest job. **Never** mutates the
graph directly — it is purely an event source for the Repo Indexer.

### 3.2 Repo Indexer

Stateless worker pool that consumes ingest jobs. For each job it:

1. Materialises the commit tree at the requested SHA.
2. Walks each file with a language-aware AST parser to emit Repo / Package /
   File / Class / Method / Block nodes (§3.7).
3. Emits static structural edges (`contains`, `imports`, `static_calls`,
   `extends`, `implements`, `reads`, `writes`).
4. Computes fingerprints per G2 and writes Nodes/Edges through the Hybrid
   Graph Store writer (§3.5).
5. Closes (retires) any node or edge no longer present in the new commit by
   setting `to_sha` to the parent SHA of the new HEAD (G5).

The Repo Indexer runs in three modes: `full` (cold registration), `delta` (push
hook), `manual` (operator-triggered re-index).

### 3.3 Span Ingestor

Consumes batches of **OpenTelemetry spans** from the configured OTel
Collector. Per operator decision, OTel spans are the canonical dynamic-trace
source for v1; any other source (py-spy, perf, language-specific profilers)
must be normalised upstream to OTel before reaching this service. The Span
Ingestor:

1. Resolves each span's `code.function` and `code.namespace` attributes to a
   Method node (or the enclosing Method node, if the span is a sub-method
   block).
2. Aggregates per-edge call counts and latencies into the `TraceObservation`
   child table attached to each `observed_calls` Edge (§5.2.3).
3. Creates new `observed_calls` Edges (with fresh fingerprints per G2) when
   a call pair has never been seen before.

### 3.4 Consolidator

Periodically (every N new Episodes, or every K minutes) scans recent Episodes
and emits:

- New `ConceptVersion` rows for any Concept whose supporting Episode set has
  grown or changed status (G4). Concept confidence is computed from this version
  row, never written in place on the parent Concept.
- New `ConceptSupport` rows that attach individual Concepts to specific
  Nodes/Episodes/repos.
- A synthetic *positive* Episode for every Episode that has just been labelled
  `human_corrected` by the operator (G7). The synthetic Episode reuses the
  *original* (parent) Episode's `context_id`, stores the operator's
  `corrected_action` as its `action` field, and sets its `outcome` to
  `success`. Provenance is captured by **two distinct fields**:
  `synthesized_from_parent_episode_id` points to the original failing Episode,
  and `synthesized_from_feedback_episode_id` points to the operator's
  `feedback` Episode. Both are required on every `synthetic_positive` row.

### 3.5 Hybrid Graph Store (writer + reader)

The single durable store. Internally split into:

- **GraphWriter** — transactional writer used by Repo Indexer, Span Ingestor,
  Consolidator, and the observe/feedback verbs of the public surfaces.
- **GraphReader** — read-only API used by recall, expand, summarize, and all
  Management read endpoints (§6.2).
- **EmbeddingIndex** — vector index over Method nodes, Block nodes, and
  Concepts. Vectors are written by Repo Indexer and Consolidator; queried by
  GraphReader during recall.

### 3.6 Reranker Trainer

Offline worker that periodically re-trains the recall reranker using the
`Episode + Observation` history as supervision. Negative Episodes (outcomes
`failure`, `degraded`, or pre-correction `human_corrected`) become negative
training pairs; positive Episodes (including the synthetic positives from G7)
become positive pairs. The trained model is published to GraphReader through
versioned model artifacts; no online graph mutation is involved.

### 3.7 Code-level Member Granularity

Per operator decision, the smallest code-level Node is **method-level**, with
**basic-block-level** subdivision for any method whose static body exceeds a
size threshold (default 80 logical lines; tunable in `tech-spec.md`). Blocks
are stored as `Block` nodes with `parent_method_id` and a `block_kind`
discriminator (`entry`, `branch`, `loop_body`, `exception`, `exit`). Call edges
and observation rows can target either a Method or a Block; if a Block is
retired in a future commit but its parent Method survives, the Block is closed
(G5) and references in older Episodes still resolve via the historical
`Block.fingerprint`.

### 3.8 Cross-repo Concept Store

Per operator decision, Concepts live in a **global** namespace. A Concept row
contains no `repo_id`. Cross-repo recall, comparison, and aggregation are
first-class queries. The repo dimension is preserved per-support-row via
`ConceptSupport.repo_id` so the UI can answer "which repos exhibit this
concept?".

### 3.9 Operator UI

Read-only inspector plus two write paths:

1. `mgmt.feedback` — submit an outcome label or correction.
2. `mgmt.register` / `mgmt.ingest*` — onboarding actions for operators.

All read paths go through the Management Surface (§6.2). The Operator UI never
calls the Agent Surface.

### 3.10 Agent Reasoning Loop (caller)

External to this service. Calls only the Agent Surface (`recall`, `observe`,
`expand`, `summarize`). The contract guarantees a request/response round-trip
of single-digit seconds for recall and observe at p95 under nominal load
(detailed SLOs deferred to `tech-spec.md`).

---

## 4. Component Interactions and End-to-End Flows

This section narrates the principal flows in prose plus a numbered step list.
For the full request/response shapes see §6.

### 4.1 Cold registration of a repo

1. Operator calls `mgmt.register(repo_url, default_branch)` on the Management
   Surface.
2. Management Surface writes a `Repo` row and enqueues a `full-ingest` job
   keyed on the HEAD SHA.
3. Repo Indexer materialises the tree, parses every file, emits Nodes
   (Repo → Package → File → Class → Method → Block per §3.7) with fingerprints
   per G2, and emits static edges.
4. Repo Indexer publishes a `repo.registered` event with the indexed SHA.
5. Operator UI's repo list now resolves through `mgmt.read.repos` (§6.2).

### 4.2 Recall-only (read path used by the agent reasoning loop)

1. Agent calls `agent.recall(repo_id, query, k, filters?)`.
2. GraphReader resolves the query against the EmbeddingIndex to get top-K
   seed Nodes.
3. GraphReader expands the seed set by structural neighbors (1-2 hops) and
   ranks via the latest reranker model.
4. GraphReader assembles a `RecallContext` envelope containing
   `nodes[]`, `edges[]`, `concepts[]`, plus a durable `context_id` written
   to the `RecallContextLog` (§5.4.1) so a later `observe` can refer to
   exactly the same recall snapshot. **No graph mutation other than the
   RecallContextLog append.**
5. Agent receives the `RecallContext` and a `context_id`.

### 4.3 Reason and observe

1. Agent reasons using the `RecallContext`, takes an `action` (e.g. produces a
   patch suggestion, files a comment, refuses).
2. The action is evaluated by whatever harness the caller uses; the harness
   returns an `outcome` in `{success, failure, refused, degraded}` and an
   optional `signal` blob with metric/trace ids.
3. Agent calls `agent.observe(repo_id, session_id, trace_id, action, outcome,
   signal?, context_id?, observation_refs?)`. `context_id` ties the Episode
   back to the recall snapshot; `observation_refs[]` enumerates which
   `RecallContext` entries (nodes, edges, or concepts) the agent actually
   used.
4. Writer appends one new `Episode` row plus N `Observation` rows
   (one per element in `observation_refs[]`); the Consolidator picks up the
   new Episode on its next tick.

### 4.4 Operator correction (human_corrected)

1. Operator opens an Episode in the UI and submits `mgmt.feedback(
   parent_episode_id, outcome=human_corrected, corrected_action,
   note?)` (corrected_action is REQUIRED here per §6.2.2).
2. Writer appends a *new* Episode row of kind `feedback`. This feedback
   Episode's `context_id` is **NULL** (the operator did not perform a new
   recall — they made a judgement from the UI). Its `parent_episode_id`
   points at the original Episode.
3. Writer appends an `EpisodeUpdate` row that marks the original Episode's
   status as `human_corrected` (immutability of the original row is
   preserved per G3 — the status is read by joining to `EpisodeUpdate`).
4. Consolidator (G7) immediately emits a **synthetic positive Episode**.
   This synthetic Episode is *not* the feedback Episode — it is a third
   Episode whose `context_id` is **copied from the parent (original)
   Episode**, whose `action` is `corrected_action`, whose `outcome` is
   `success`. Its provenance is captured by **two** fields:
   `synthesized_from_parent_episode_id` = the original failing Episode, and
   `synthesized_from_feedback_episode_id` = the operator's feedback Episode.
   The synthetic Episode also gets new Observation rows that mirror the
   parent Episode's Observation rows so the positive signal attaches to the
   same recall elements.
5. The next Reranker Trainer cycle uses both the negative parent and the
   synthetic positive in its training pairs.

### 4.5 Call-chain expansion (dynamic mode)

1. Agent calls `agent.expand(node_id, direction=callers|callees, depth)`.
2. GraphReader walks `static_calls` and `observed_calls` edges up to `depth`.
3. Result includes, per edge, the latest `TraceObservation` aggregate
   (count, p50, p95 latency, last-seen `trace_id`) so the agent can rank by
   *actually observed* hot paths, not just static structure.
4. No write occurs. Identical to §4.2 wrt RecallContextLog — `expand` writes
   one RecallContextLog row keyed by the new `context_id` so a subsequent
   `observe` can pin to this expansion.

### 4.6 Delta re-index on git push

1. Webhook Receiver verifies signature, writes `RepoEvent(kind=push,
   from_sha, to_sha)`, enqueues a `delta-ingest` job.
2. Repo Indexer diffs the two SHAs, re-parses changed files only, emits new
   Nodes/Edges (G2 fingerprints from the new SHA), and retires any Node/Edge
   no longer present by setting `to_sha = parent(new_HEAD)` (G5).
3. EmbeddingIndex is updated for any Node whose canonical signature changed.
4. A `repo.delta_ingested` event is published.

### 4.7 Episodic learning (consolidation)

1. Consolidator wakes every N Episodes (or every K minutes).
2. It groups Episodes by `(repo_id, signature_hash_of_observation_set)` and
   computes support, confidence-band, and concept-name candidates.
3. For each group whose support crossed a threshold, it appends a new
   `Concept` row (only if no fingerprint match exists) and always a new
   `ConceptVersion` row (carries the current confidence and support count
   — G4). It attaches `ConceptSupport` rows to the contributing Nodes /
   Episodes / repos.
4. The Concept Promoter runs after each Consolidator cycle to promote any
   Concept whose latest ConceptVersion crosses a publishable threshold into
   the EmbeddingIndex so it becomes a first-class recall result alongside
   Nodes.

### 4.8 Operator inspecting a degraded run

1. Operator opens "Recent Episodes" in the UI.
2. UI calls `mgmt.read.episodes(repo_id?, since?, outcome_in?)`.
3. UI shows the Episode plus its Observations, its `degraded` flag (if any),
   and lets the operator drill into the `RecallContextLog` snapshot via
   `mgmt.read.context(context_id)`.
4. Operator either acknowledges the run, or files `mgmt.feedback` per §4.4.

---

## 5. Data Model

This section lists every persistent entity, its fields, and the relationships
between them. Field types are normative; storage engine (SQL, KV+index, mixed)
is deferred to `tech-spec.md`.

### 5.1 Top-level entities (overview)

| Entity | Purpose | Mutability |
| --- | --- | --- |
| `Repo` | Registered code repository | Mutable settings only |
| `Commit` | Snapshot of a repo at a SHA | Immutable |
| `Node` | Structural code element (Repo→Block) | Append-only with `to_sha` retirement |
| `Edge` | Relation between two Nodes | Append-only with `to_sha` retirement |
| `TraceObservation` | Per-`observed_calls`-edge aggregate | Mutable counters; provenance is append-only `TraceObservationLog` rows |
| `Episode` | One agent reasoning attempt | Immutable row (status read via `EpisodeUpdate`) |
| `EpisodeUpdate` | Status change on an Episode | Append-only |
| `Observation` | Which RecallContext element was used in an Episode | Immutable |
| `RecallContextLog` | Durable snapshot of a recall/expand response | Immutable |
| `Concept` | Learned cross-repo pattern | Immutable |
| `ConceptVersion` | Current confidence band for a Concept | Append-only |
| `ConceptSupport` | Attaches a Concept to specific Nodes / repos | Append-only |

### 5.2 Structural Graph

#### 5.2.1 Node

| Field | Type | Notes |
| --- | --- | --- |
| `node_id` | uuid | Primary key, generated server-side. |
| `fingerprint` | bytes(32) | **G2**: `sha256(repo_id ‖ kind ‖ canonical_signature ‖ from_sha)`. Unique within `(repo_id, fingerprint)`. |
| `repo_id` | uuid | FK → `Repo`. |
| `kind` | enum | `repo`, `package`, `file`, `class`, `method`, `block`. |
| `canonical_signature` | text | Stable name (e.g. `pkg.Foo#bar(int)#block_3`). |
| `parent_node_id` | uuid? | FK → `Node` (containment hierarchy). |
| `from_sha` | text | First SHA at which this exact fingerprint appeared. |
| `to_sha` | text? | Null = current; non-null = retired at the listed parent SHA per **G5**. |
| `embedding_vec` | vector? | Optional; written by Repo Indexer for Method/Block nodes. |
| `attrs_json` | json | Language-specific attributes (visibility, return type, etc.). |

#### 5.2.2 Edge

| Field | Type | Notes |
| --- | --- | --- |
| `edge_id` | uuid | Primary key. |
| `fingerprint` | bytes(32) | **G2**: `sha256(repo_id ‖ kind ‖ src_fingerprint ‖ dst_fingerprint ‖ from_sha)`. Unique within `(repo_id, fingerprint)`. |
| `repo_id` | uuid | FK → `Repo` (Edges remain repo-scoped per **G6**). |
| `kind` | enum | `contains`, `imports`, `static_calls`, `observed_calls`, `extends`, `implements`, `reads`, `writes`, `renamed_to`. **There is no `concept_attaches` edge kind** — Concepts are *not* graph Nodes (the Node `kind` enum is closed at `repo`/`package`/`file`/`class`/`method`/`block`), so links from code Nodes to Concepts are carried exclusively by `ConceptSupport` rows (§5.5.3), not by Edges. |
| `src_node_id` | uuid | FK → `Node`. |
| `dst_node_id` | uuid | FK → `Node`. |
| `from_sha` | text | First SHA at which this edge appeared. |
| `to_sha` | text? | Null = current; non-null = retired per **G5**. |
| `attrs_json` | json | Edge-kind-specific attributes (e.g. argument-count, branch label). |

#### 5.2.3 TraceObservation (child of Edge, only for `observed_calls`)

| Field | Type | Notes |
| --- | --- | --- |
| `edge_id` | uuid | FK → `Edge` (Edge row is append-only per **G5**). |
| `observation_count` | int | **Mutable aggregate** — incremented on each new span batch. |
| `p50_latency_ms` | float | Mutable aggregate. |
| `p95_latency_ms` | float | Mutable aggregate. |
| `latest_span_ref` | text | Mutable; last `(trace_id, span_id)` that touched this edge. |
| `last_observed_at` | timestamp | Mutable. |

> **Mutability note.** Per **G5**, the parent `Edge` row stays append-only.
> The mutable counters live in `TraceObservation`, which is conceptually a
> *materialised view* over the append-only `TraceObservationLog` (one row per
> ingested span, never updated). The rebuild guarantee is narrowed to the
> configured `TraceObservationLog` retention window (§8.1): if a
> `TraceObservation` row is lost, it can be rebuilt deterministically *only*
> from log rows still inside the retention window. Aggregates older than the
> window are authoritative on the `TraceObservation` row alone — they cannot
> be recomputed from the log because the contributing log rows have been
> pruned. The Edge row's own `attrs_json` is **not** updated by dynamic
> ingest.

### 5.3 Episodic Layer

#### 5.3.1 Episode

| Field | Type | Notes |
| --- | --- | --- |
| `episode_id` | uuid | Primary key. |
| `episode_group_id` | uuid | Stable across retries of the same logical task. |
| `repo_id` | uuid | FK → `Repo`. |
| `session_id` | text | Agent-side session identifier. |
| `trace_id` | text | Caller-side correlation id. |
| `kind` | enum | `agent`, `feedback`, `synthetic_positive`. |
| `parent_episode_id` | uuid? | Set on `feedback` rows (points to the original failing Episode). Not set on `synthetic_positive` rows — those use the two `synthesized_from_*` fields below. |
| `synthesized_from_parent_episode_id` | uuid? | Set on `synthetic_positive` rows only. Points to the original failing Episode (the row that was labelled `human_corrected`). |
| `synthesized_from_feedback_episode_id` | uuid? | Set on `synthetic_positive` rows only. Points to the `feedback` Episode (the row the operator wrote when submitting `mgmt.feedback`). |
| `context_id` | uuid? | FK → `RecallContextLog`. NULL is legal **only** for `feedback` Episodes (operator did not run a new recall — see §4.4 step 2). For `synthetic_positive` Episodes this field is **copied from the parent Episode's `context_id`** per **G7**. |
| `action` | json | The proposed/chosen action. |
| `outcome` | enum | `success`, `failure`, `refused`, `degraded`, `human_corrected`. Note that on the *original* Episode this column is always the initial value; subsequent transitions are reflected via `EpisodeUpdate`. |
| `corrected_action` | json? | Required when `outcome=human_corrected` per §6.2.2; otherwise null. |
| `signal_json` | json? | Caller-supplied metrics/trace ids. |
| `degraded` | bool | True iff the producing call was served under a degraded mode (§7.5). |
| `degraded_reason` | text? | Set iff `degraded=true`. |
| `created_at` | timestamp | Append time. |

#### 5.3.2 EpisodeUpdate

| Field | Type | Notes |
| --- | --- | --- |
| `update_id` | uuid | Primary key. |
| `episode_id` | uuid | FK → `Episode`. |
| `new_outcome` | enum | Same enum as `Episode.outcome`. |
| `note` | text? | Operator-supplied free text. |
| `actor` | enum | `operator`, `consolidator`, `system`. |
| `created_at` | timestamp | Append-only. |

#### 5.3.3 Observation

| Field | Type | Notes |
| --- | --- | --- |
| `observation_id` | uuid | Primary key. |
| `episode_id` | uuid | FK → `Episode`. |
| `role` | enum | `node_hit`, `edge_hit`, `call_edge_hit`, `concept_hit`, `degraded_recall_context`. The Observation table therefore covers nodes, edges (both static and `observed_calls`), concepts, and degraded contexts — exactly the four kinds of element a `RecallContext` can return. |
| `node_id` | uuid? | Set iff `role=node_hit`. |
| `edge_id` | uuid? | Set iff `role in (edge_hit, call_edge_hit)`. |
| `concept_id` | uuid? | Set iff `role=concept_hit`. |
| `degraded_recall_context_id` | uuid? | Set iff `role=degraded_recall_context`. Lets `mgmt.read.episodes` distinguish "fell back to stale graph" from "used live graph". |
| `weight` | float | Caller-supplied "how much did this element contribute to the action". |
| `created_at` | timestamp | Append-only. |

> **Exactly one** of `node_id`, `edge_id`, `concept_id`,
> `degraded_recall_context_id` is non-null per row; this is enforced by a
> CHECK constraint at write time.

### 5.4 Recall Context

#### 5.4.1 RecallContextLog

| Field | Type | Notes |
| --- | --- | --- |
| `context_id` | uuid | Primary key. |
| `repo_id` | uuid | FK → `Repo`. |
| `verb` | enum | `recall`, `expand`, `summarize`. |
| `query_json` | json | Inputs to the originating verb. |
| `node_ids` | uuid[] | Ordered list of node ids returned. |
| `edge_ids` | uuid[] | Ordered list of edge ids returned. |
| `concept_ids` | uuid[] | Ordered list of concept ids returned. |
| `reranker_model_version` | text | Pinned for reproducibility. |
| `served_under_degraded` | bool | True iff served from cached snapshot during a graph outage (§7.5). |
| `created_at` | timestamp | Append-only. |

### 5.5 Concept Layer

#### 5.5.1 Concept (G4 — append-only)

| Field | Type | Notes |
| --- | --- | --- |
| `concept_id` | uuid | Primary key. |
| `fingerprint` | bytes(32) | Deterministic over the canonical concept name + observed-feature-signature. Cross-repo per **G6** — no `repo_id` here. |
| `name` | text | Human-readable label (e.g. "double-checked-locking"). |
| `description_md` | text | Markdown description. |
| `embedding_vec` | vector | Written by the Concept Promoter. |
| `created_at` | timestamp | Append-only. |

> **Concept has no `confidence` column.** Per **G4**, confidence is always
> the most recent `ConceptVersion` row for a given `concept_id`. Treat
> `SELECT … FROM ConceptVersion WHERE concept_id=? ORDER BY version_index DESC
> LIMIT 1` as the canonical "current" lookup. The Consolidator never updates
> Concept in place; it always inserts a new ConceptVersion row.

#### 5.5.2 ConceptVersion (G4)

| Field | Type | Notes |
| --- | --- | --- |
| `concept_version_id` | uuid | Primary key. |
| `concept_id` | uuid | FK → `Concept`. |
| `version_index` | int | Monotonic per `concept_id`. |
| `confidence` | float | In `[0,1]`. This is the canonical confidence at this version. |
| `confidence_band` | enum | `low`, `medium`, `high`. Derived from `confidence` at write time. |
| `support_count` | int | Number of supporting positive Episodes. |
| `negative_count` | int | Number of supporting negative Episodes. |
| `consolidator_run_id` | uuid | Pointer to the run that produced this version. |
| `promoted` | bool | Set true by the Concept Promoter when the Concept first crosses the publishable threshold (§7.8). Subsequent versions inherit/refresh this flag. |
| `created_at` | timestamp | Append-only. |

#### 5.5.3 ConceptSupport

| Field | Type | Notes |
| --- | --- | --- |
| `support_id` | uuid | Primary key. |
| `concept_id` | uuid | FK → `Concept`. |
| `concept_version_id` | uuid | FK → `ConceptVersion`. Pins which version this support contributed to. |
| `repo_id` | uuid | FK → `Repo`. Cross-repo grouping per **G6**. |
| `node_id` | uuid? | Optional — set when the support is anchored to a specific Node. |
| `episode_id` | uuid? | Optional — set when the support is anchored to a specific Episode. |
| `polarity` | enum | `positive`, `negative`. |
| `created_at` | timestamp | Append-only. |

### 5.6 Misc

| Entity | Notes |
| --- | --- |
| `Repo` | `repo_id`, `url`, `default_branch`, `current_head_sha`, `language_hints[]`, `created_at`. |
| `Commit` | `repo_id`, `sha`, `parent_sha`, `committed_at`, `index_status`. |
| `RepoEvent` | `event_id`, `repo_id`, `kind in (push|merge|register|manual)`, `from_sha?`, `to_sha`, `received_at`. |
| `ConsolidatorRun` | `run_id`, `started_at`, `finished_at`, `episode_high_water_mark`, `status`. |
| `TraceObservationLog` | `span_log_id`, `edge_id`, `trace_id`, `span_id`, `started_at`, `duration_ms`. Immutable. Source of truth for `TraceObservation` aggregates. |

---

## 6. Public Interfaces

Two HTTPS surfaces. Authentication and transport (REST vs gRPC) are deferred
to `tech-spec.md`; the *contracts* below are normative.

### 6.1 Agent Surface

Four verbs. The Agent Surface is the **only** surface the agent reasoning loop
calls.

#### 6.1.1 `agent.recall(repo_id, query, k, filters?) → RecallResponse`

```
RecallResponse {
  context_id:   uuid           # durable; ref it in observe
  nodes:        [NodeCard...]  # ranked
  edges:        [EdgeCard...]  # ranked; includes both static_calls and observed_calls
  concepts:     [ConceptCard...]
  reranker_model_version: text
  degraded:     bool           # true iff served under degraded mode
  degraded_reason: text?       # set iff degraded
}
```

#### 6.1.2 `agent.observe(repo_id, session_id, trace_id, action, outcome, signal?, context_id?, observation_refs?) → ObserveResponse`

- `outcome ∈ {success, failure, refused, degraded}` for direct agent observations.
- `outcome = human_corrected` is **not** accepted on `agent.observe` — only on
  `mgmt.feedback` (§6.2.2). This keeps the §6.2.2 `corrected_action`
  requirement scoped to the operator path.
- `observation_refs[]` is an array of `{role, node_id?, edge_id?, concept_id?}`
  rows that map onto Observation rows (§5.3.3). `role` must be one of
  `node_hit`, `edge_hit`, `call_edge_hit`, `concept_hit`; `edge_hit` and
  `call_edge_hit` reference entries from `RecallResponse.edges`. The caller
  **never** supplies a `degraded_recall_context` ref — that role is reserved
  for the server. When `context_id` points to a `RecallContextLog` row whose
  `served_under_degraded=true`, the writer automatically appends one extra
  Observation row with `role='degraded_recall_context'` and
  `degraded_recall_context_id=context_id` (this is the only path that writes
  that role). Callers that pass a `role='degraded_recall_context'` entry are
  rejected with a validation error.

```
ObserveResponse {
  episode_id:        uuid
  episode_group_id:  uuid
  degraded:          bool
  degraded_reason:   text?
}
```

#### 6.1.3 `agent.expand(node_id, direction, depth) → ExpandResponse`

```
ExpandResponse {
  context_id:   uuid
  root_node_id: uuid
  edges:        [EdgeCard...]   # call-chain hops
  nodes:        [NodeCard...]   # reached nodes
  degraded:     bool
  degraded_reason: text?
}
```

#### 6.1.4 `agent.summarize(node_id | concept_id, max_tokens) → SummarizeResponse`

```
SummarizeResponse {
  context_id:    uuid           # the underlying recall snapshot
  target_kind:   enum           # 'node' | 'concept'
  target_id:     uuid
  summary_md:    text
  citations:     [{node_id?, edge_id?, concept_id?, snippet?}]
  degraded:      bool
  degraded_reason: text?
}
```

### 6.2 Management Surface

#### 6.2.1 Writes

| Verb | Purpose |
| --- | --- |
| `mgmt.register(repo_url, default_branch)` | Onboard a new repo. |
| `mgmt.ingest(repo_id, sha?)` | Full ingest at a SHA (default: HEAD). |
| `mgmt.ingest_delta(repo_id, from_sha, to_sha)` | Delta ingest between two SHAs. Idempotent. |
| `mgmt.ingest_spans(batch[])` | Canonical OTel span batch (§3.3, **G3**). No `outcome`/`corrected_action` semantics — those belong to `feedback`. |
| `mgmt.feedback(parent_episode_id, outcome, corrected_action?, note?)` | Operator correction or acknowledgement. |
| `mgmt.snapshot(repo_id)` | Force an embedding/index snapshot. |

#### 6.2.2 Validation rules

- `mgmt.feedback`:
  - `outcome=human_corrected` ⇒ `corrected_action` **REQUIRED**.
  - Other outcomes: `corrected_action` must be omitted.
- `agent.observe`:
  - Rejects `outcome=human_corrected` (operator-only).
- `mgmt.ingest_spans`:
  - Validates each span against the OTel schema. **No** `outcome`/
    `corrected_action` validation — this verb has no such fields.

#### 6.2.3 Reads

The UI **only** uses these:

| Verb | Returns |
| --- | --- |
| `mgmt.read.repos(filter?)` | List of `Repo` rows + their current ingest status. |
| `mgmt.read.commits(repo_id, since?)` | Commit history with index status. |
| `mgmt.read.episodes(repo_id?, since?, outcome_in?, kind_in?)` | Episodes with their `EpisodeUpdate` joined as `current_status`. |
| `mgmt.read.observations(episode_id)` | Observation rows for an Episode. |
| `mgmt.read.context(context_id)` | Full `RecallContextLog` row plus dereferenced Node/Edge/Concept cards. |
| `mgmt.read.concepts(filter?)` | Concepts with their latest `ConceptVersion` joined (§5.5.2). |
| `mgmt.read.concept_supports(concept_id, repo_id?)` | Cross-repo support rows for a concept (per **G6**). |
| `mgmt.read.graph_node(node_id, sha?)` | Node card + immediate neighbors at the requested SHA (default: current). |
| `mgmt.read.trace_observation(edge_id)` | `TraceObservation` aggregate plus a paged tail of `TraceObservationLog` rows. |

### 6.3 Degraded-mode response shapes

Every verb listed above has a normative degraded response (§7.5). The shape is
**verb-specific** — there is no shared envelope. The matrix:

| Verb | Degraded envelope |
| --- | --- |
| `agent.recall` | `RecallResponse` with `degraded=true`, `nodes/edges/concepts` served from the most recent valid snapshot, `reranker_model_version` set to that snapshot's version, `context_id` references a **degraded** `RecallContextLog` row (`served_under_degraded=true`). |
| `agent.observe` | `ObserveResponse` with `degraded=true` and `degraded_reason` set. The Episode is still appended; if the EpisodicLog itself is degraded the writer buffers and replies `degraded=true` with the *eventually-assigned* `episode_id` once the buffer flushes. |
| `agent.expand` | `ExpandResponse` with `degraded=true`. Edges/nodes served from snapshot. |
| `agent.summarize` | `SummarizeResponse` with `degraded=true`. `summary_md` may be a cached prior summary plus a banner string; `citations[]` may be empty. |
| `mgmt.*` reads | Each read verb returns its normal shape plus a top-level `degraded: bool` field and a `degraded_reason: text?` field. |

---

## 7. Sequence Flows (normative)

Each flow lists the wire calls in the order they cross component boundaries.
Internal worker steps are noted but not numbered as wire calls.

### 7.1 Cold registration

```
Operator UI  → mgmt.register(repo_url, default_branch)
              ← {repo_id}
Operator UI  → mgmt.ingest(repo_id)            # optional; otherwise auto-triggered
              ← {ingest_job_id}
(Repo Indexer runs: write Repo/Commit/Node/Edge rows with G2 fingerprints)
Operator UI  → mgmt.read.repos()               # poll for ready
              ← {repos: [{repo_id, status: 'indexed'}]}
```

### 7.2 Steady-state agent loop

```
Agent → agent.recall(repo_id, query, k)
      ← {context_id, nodes, edges, concepts, ...}
Agent (reason locally, produce action)
Agent → agent.observe(repo_id, session_id, trace_id, action, outcome,
                      signal?, context_id, observation_refs)
      ← {episode_id, episode_group_id, degraded:false}
(Consolidator picks up the new Episode on next tick.)
```

### 7.3 Operator correction (the §4.4 flow, in wire terms)

```
Operator UI → mgmt.read.episodes(repo_id, outcome_in=[failure])
            ← {episodes: [...]}
Operator UI → mgmt.read.context(context_id_of_chosen_episode)
            ← {...}                           # shows what the agent saw
Operator UI → mgmt.feedback(parent_episode_id, outcome='human_corrected',
                            corrected_action={...}, note?)
            ← {feedback_episode_id}
(Writer:
   1. Append feedback Episode with context_id=NULL,
      parent_episode_id=<parent>.
   2. Append EpisodeUpdate(episode_id=<parent>,
                           new_outcome='human_corrected').
 Consolidator (next tick):
   3. Append synthetic_positive Episode with
      context_id := parent.context_id,                ← G7
      action     := corrected_action,
      outcome    := 'success',
      synthesized_from_parent_episode_id   := <parent>,
      synthesized_from_feedback_episode_id := feedback_episode_id.
   4. Append Observation rows that mirror the parent's Observation rows,
      attached to the synthetic positive Episode.)
```

> The parent Episode row itself is **not** rewritten (G3). All status changes
> on the parent are visible through `EpisodeUpdate`.

### 7.4 Delta ingest after push

```
git host → POST /webhook (push)
Webhook Receiver → write RepoEvent + enqueue delta job
                 → respond 202
(Repo Indexer:
   - diff(from_sha, to_sha)
   - for each changed file: re-parse, emit new Node/Edge rows with fresh
     fingerprints per G2
   - retire stale Nodes/Edges by setting to_sha = parent(to_sha) per G5
   - update EmbeddingIndex for changed Method/Block nodes)
```

### 7.5 EpisodicLog outage (write path)

```
Agent → agent.observe(...)
        (Writer detects EpisodicLog unavailable;
         buffers the Episode + Observations to a local WAL;
         returns immediately.)
      ← {episode_id: <wal-assigned>, degraded:true,
         degraded_reason:'episodic_log_unavailable'}
(Background flusher drains the WAL when the EpisodicLog recovers; the
 episode_id returned at request time is the final id.)
```

### 7.6 Hybrid Graph Store outage (read path)

When the Hybrid Graph Store is unavailable for reads, **each affected verb
falls back to its own degraded shape** per §6.3. There is no shared envelope.
Specifically:

- `agent.recall` returns a `RecallResponse` with `degraded=true` and a
  `RecallContextLog` row written with `served_under_degraded=true`. The
  agent's later `observe` records an Observation row with
  `role='degraded_recall_context'` and `degraded_recall_context_id` set to
  this context (§5.3.3).
- `agent.expand` returns an `ExpandResponse` with `degraded=true`; the
  walked edges come from the cached snapshot.
- `agent.summarize` returns a `SummarizeResponse` with `degraded=true` and
  a possibly-stale `summary_md`.
- All `mgmt.read.*` verbs return their normal shape with a top-level
  `degraded=true` / `degraded_reason` pair.

### 7.7 Consolidation tick

```
(Consolidator wakes every N Episodes or every K minutes.)
1. Read Episodes since last high-water mark.
2. Group by (repo_id, observation_signature_hash).
3. For each group whose support crossed threshold:
     - If fingerprint not seen: append Concept row.
     - Always: append ConceptVersion (carries the current confidence band
       and support count — G4).
     - Append ConceptSupport rows (positive or negative).
4. For each parent Episode newly marked human_corrected (via EpisodeUpdate):
     - Append synthetic_positive Episode per §7.3 step 3, copying
       parent.context_id.
     - Append mirror Observation rows per §7.3 step 4.
5. Persist a ConsolidatorRun row with the new high-water mark.
```

### 7.8 Concept promotion

```
(Concept Promoter runs after each ConsolidatorRun.)
1. SELECT concepts whose latest ConceptVersion crossed the publishable
   confidence threshold (e.g. confidence ≥ 0.7 and support_count ≥ 5).
2. Compute embedding for each promoted Concept (description + canonical
   feature signature) and write to EmbeddingIndex.
3. Mark the Concept as "promoted" by appending a `ConceptVersion` row whose
   `promoted=true` flag is set (this row carries the same confidence/support
   counts as the prior version but flips the flag; the flag is read by
   `mgmt.read.concepts` and by GraphReader during recall). No graph Edge is
   written — Concepts are not Nodes (§5.2.1), and links from code Nodes to
   Concepts are carried by `ConceptSupport` rows (§5.5.3), which the
   Consolidator has already written in §7.7 step 3.
```

> Promotion does **not** rewrite the Concept row, and it does **not** create
> a synthetic graph Node or Edge. The publishable signal is derived from
> `ConceptVersion` per **G4**; an un-promoted Concept simply has no
> `ConceptVersion` row with `promoted=true` yet.

---

## 8. Operational concerns

### 8.1 Retention

- `EpisodicLog` and `EpisodeUpdate`: **append-only forever** per operator
  decision (no rotation). Capacity sizing is the responsibility of
  `tech-spec.md`.
- `RecallContextLog`: append-only forever (cheap because it stores ids, not
  payloads).
- `TraceObservationLog`: append-only with a configurable retention window
  (default in `tech-spec.md`). Inside the window, `TraceObservation`
  aggregates can be rebuilt deterministically from the log (§5.2.3). Once a
  log row falls outside the window the Span Ingestor prunes it; from that
  point on the aggregated `TraceObservation` row is authoritative and cannot
  be recomputed. The `TraceObservation` row itself is **always** preserved —
  it is never pruned by retention.
- Structural `Node` and `Edge`: append-only with retirement (G5). Retired
  rows are kept forever so historic Episodes resolve their node/edge ids.

### 8.2 Degraded-mode flag conventions

Every response that carries degraded semantics uses the same two fields:
`degraded: bool` and `degraded_reason: text?`. The set of allowed reasons is
fixed and closed:

`episodic_log_unavailable`, `graph_store_unavailable`,
`embedding_index_unavailable`, `reranker_model_stale`,
`span_ingestor_backpressure`, `consolidator_backpressure`.

### 8.3 Reliability invariants

- An `agent.observe` call **never** fails because the Consolidator is
  backpressured; the Episode is queued and `degraded_reason` set.
- A `mgmt.ingest_spans` call **never** rewrites an Edge row; it only updates
  `TraceObservation` aggregate counters and appends `TraceObservationLog`
  rows (G5).
- A `mgmt.feedback` call with `outcome=human_corrected` **always** results
  in one feedback Episode + one EpisodeUpdate row + (on the next
  Consolidator tick) one synthetic positive Episode. Operators can rely on
  this even after a service restart, because the synthetic positive is
  produced by the Consolidator reading EpisodeUpdate rows, not by an
  in-memory hand-off.

### 8.4 Cross-repo Concept queries (G6)

The Operator UI's "Concepts across repos" view is served entirely by
`mgmt.read.concepts` joined to `mgmt.read.concept_supports(concept_id,
repo_id=?)`. Because Concepts have no `repo_id` field, this query is a
straight scan with a `support.repo_id` filter; no Concept duplication across
repos is needed.

### 8.5 Method vs basic-block resolution rules

When an `observe` or `expand` references a Node:

- If the target Method has been split into Blocks (per §3.7), the caller
  may pass either the Method fingerprint or a Block fingerprint.
- A Concept anchored at the Method level matches Episodes regardless of
  which Block was hit; a Concept anchored at the Block level only matches
  Episodes that recorded an Observation at that exact Block.

---

## 9. Public-contract summary (single-glance table)

| Surface | Verb | Reads | Writes |
| --- | --- | --- | --- |
| Agent | `recall` | Graph, EmbeddingIndex | `RecallContextLog` (append) |
| Agent | `observe` | — | `Episode` (append) + `Observation[]` (append) |
| Agent | `expand` | Graph | `RecallContextLog` (append) |
| Agent | `summarize` | Graph + RecallContextLog | `RecallContextLog` (append; may reference existing context) |
| Management | `register` | Repo | `Repo` |
| Management | `ingest` | — | `Commit`, `Node`, `Edge`, `EmbeddingIndex` |
| Management | `ingest_delta` | — | `Commit`, `Node`, `Edge`, `EmbeddingIndex` |
| Management | `ingest_spans` | Graph (lookup) | `TraceObservation`, `TraceObservationLog`, occasionally new `Edge` |
| Management | `feedback` | Episode | `Episode` (feedback kind), `EpisodeUpdate`; *Consolidator later writes* `Episode` (synthetic_positive) + `Observation[]` |
| Management | `snapshot` | — | `EmbeddingIndex` |
| Management | `read.*` | All read-only entities | — |

---

## 10. Cross-references

- Storage engine choice, schema DDL, exact OTel-span attribute mapping, and
  SLO numbers live in `tech-spec.md`.
- Workstream sequencing, milestones, and rollout plan live in
  `implementation-plan.md`.
- Concrete numbered end-to-end test scenarios (cold registration, steady-state,
  correction loop, span ingest, degraded mode, cross-repo concept query) live
  in `e2e-scenarios.md`.
