# Agent Memory — Tech Spec

> Story: `code-intelligence:AGENT-MEMORY` · 21 points
> Sibling planning artifacts: `architecture.md`, `implementation-plan.md`, `e2e-scenarios.md`
> This file owns: **problem statement, in-/out-of-scope items,
> non-goals, hard constraints, identified risks, and the
> numeric/policy parameter pins this story needs.** Component-level
> contracts and data models are owned by `architecture.md`; rollout
> sequencing is owned by `implementation-plan.md`; numbered
> end-to-end scenarios are owned by `e2e-scenarios.md`. Where this
> document and `architecture.md` describe the same concept,
> `architecture.md` is the source of truth for the component/data
> contract and this file is the source of truth for scope and
> parameter pins.

---

## 1. Document Scope

This document is a **sibling planning artifact** to `architecture.md`,
`implementation-plan.md`, and `e2e-scenarios.md`. It is the source of
truth for the brief assigned to this file: **problem statement,
in-/out-of-scope items, non-goals, hard constraints, identified
risks, and the numeric/policy parameter pins** the Agent Memory
subsystem needs.

It is **not** an upstream layer that `architecture.md` derives from.
`architecture.md` is the merged source of truth for components,
interfaces, and data models, and it explicitly defers a fixed set of
numeric and vendor decisions to this file (see `architecture.md` §10:
storage engine, exact OTel-span attribute mapping, and SLO numbers).
Those deferred decisions are pinned in §8 of this document.
`architecture.md` §10 also lists "schema DDL" as living in this
file; this tech spec narrows that claim — **engine-level and
policy-level storage decisions are pinned here (§8.1), but the
table-by-table DDL (CREATE TABLE statements, indices, constraints)
is owned by `implementation-plan.md`** because DDL is build-order
material, not framing material. The data model itself is normative
in `architecture.md` §5; this tech spec does not redefine it.

This document is normative for:

- The **problem statement** the subsystem must solve (§2).
- The **strategy survey** that narrows the design from the
  Awesome-GraphMemory body of work down to a single candidate family,
  and the rationale for the choice (§3).
- **Scope boundaries** (§4 in scope · §5 out of scope · §6 non-goals).
- **Hard constraints** — operator-authored or directly implied by the
  story — that must be respected verbatim across all sibling docs
  (§7).
- **Numeric and policy parameter pins** that `architecture.md` §10
  defers to this file: storage engine, vector index, retention
  windows, threshold defaults, SLO targets, reranker class/cadence,
  transport, authN, OTel attribute mapping (§8). Each pin is a
  **locked decision** for v1; §10 lists the locks for single-glance
  reference.
- **Identified risks and their mitigations** (§9).

This document is **not** the place to specify component interfaces
(see `architecture.md` §6), data-model field tables (see
`architecture.md` §5), or build-order milestones (see
`implementation-plan.md`).

Whenever this file uses the shorthand **`Gn`**, it refers to the
guiding principle of that index in `architecture.md` §1.3 (G1
read/write separation, G2 identity-by-fingerprint, G3 append-only
EpisodicLog, G4 Concepts append-only / confidence derived, G5
structural append-only / tombstone retirement, G6 cross-repo
Concepts and repo-scoped Nodes/Edges, G7 operator-correction
auto-promotes).

---

## 2. Problem Statement

### 2.1 What the operator asked for

The story description is short and precise. Paraphrased:

> *Based on links from*
> [`DEEP-PolyU/Awesome-GraphMemory`](https://github.com/DEEP-PolyU/Awesome-GraphMemory),
> *research the best strategy to implement agent memory, where we
> build graph/expertise on top-down repo structure and call-chain
> context. This will be used to construct context where the agent
> can learn and get smarter.*

Four concrete commitments are embedded in that sentence:

1. **Graph-shaped memory.** The substrate is a graph, not a flat
   vector store, not a transcript log. This rules out a
   "stuff-everything-into-RAG" design.
2. **Top-down repo structure as the structural axis.** The
   structural graph is anchored on the repository tree
   (`Repo → Package → File → Class → Method → Block` per
   `architecture.md` §3.7). The structural axis is built by static
   AST walk, not by inference.
3. **Call-chain context as the dynamic axis.** Edges captured by
   actual runs (the call graph that *happened*) augment the static
   call graph (the call graph that *could* happen). Both are
   first-class.
4. **Learn-and-get-smarter loop.** Every concrete reasoning run is
   recorded, consolidated into reusable patterns (Concepts), and the
   retrieval ranking is improved by the resulting signal. Operator
   corrections feed the same loop.

### 2.2 Why this is a real problem

A coding agent without memory exhibits three observable failure
patterns the operator already sees in production:

- **Re-learning the same repo on every task.** With a flat retrieval
  layer the agent re-derives the package/class/method hierarchy on
  every prompt, paying token cost and accuracy cost each time.
- **Static-only call reasoning.** Without runtime observation the
  agent cannot tell a hot path from a dead branch; structural
  similarity alone produces irrelevant suggestions on large
  codebases.
- **No accumulation.** A correct fix made yesterday is invisible
  today. The agent cannot improve from corrections because nothing
  durable links the correction to the recall context that produced
  the wrong answer.

The Agent Memory subsystem exists to remove all three failure modes
on a single substrate.

### 2.3 What "construct context" means in this story

"Construct context" is the act of producing a `RecallContext`
envelope — a ranked, durable, replayable bundle of nodes, edges, and
concepts — that the agent consumes as part of its prompt assembly.
The contract is fixed by `architecture.md` §6.1.1 (`agent.recall`)
and §4.2 (recall-only flow); this tech spec does not redefine it.

### 2.4 What "learn and get smarter" means in this story

The learning loop is the chain:

```
recall  →  reason  →  observe  →  consolidate  →  re-rank  →  recall'
```

Specifically (cross-referenced to `architecture.md`):

- `recall` produces a `RecallContext` and a durable `context_id`
  (`architecture.md` §4.2).
- `observe` writes a single Episode plus N Observation rows that
  pin which `RecallContext` elements were used
  (`architecture.md` §4.3, §5.3.1, §5.3.3).
- `consolidate` converts Episodes into Concepts + ConceptVersions
  via the Consolidator and Concept Promoter (`architecture.md`
  §3.4, §3.5, §7.7, §7.8).
- `re-rank` retrains the reranker offline from labelled Episodes,
  including the synthetic positives produced by operator
  corrections (G7; `architecture.md` §3.6, §4.4).
- The next `recall` uses the new reranker model version, the new
  Concept embeddings, and the updated structural graph.

The "smarter" half of "learn and get smarter" is observable through
two metrics this tech spec commits to (§8.3): rank-of-correct-node
at fixed *k*, and Concept-hit fraction at fixed *k*.

---

## 3. Strategy Survey and Selected Approach

The Awesome-GraphMemory catalogue in the linked repository is the
operator-authored starting point. This section narrows that body of
work down to the family that matches the four commitments in §2.1.

### 3.1 Families considered

| Family | Representative idea | Fit for this story |
| --- | --- | --- |
| **Flat episodic logs** (chat transcripts, vector-only) | Append every conversation chunk; retrieve top-K by similarity. | **Rejected** — fails commitment 1 (graph) and commitment 2 (top-down structure). No expertise accumulates beyond raw recall. |
| **Pure knowledge graph (KG) over code** | LSP-extracted symbols + relations only; static. | **Rejected** — fails commitment 3 (call-chain dynamics) and commitment 4 (no learning surface attached). |
| **GraphRAG over docs** (e.g. summarised-community + entity-graph) | Build entity graph from text, then RAG over communities. | **Partial** — community summarisation pattern is reusable for the Concept layer, but it is doc-centric, not code-centric, and has no notion of a runtime call edge. |
| **Episodic-then-semantic memory** (Generative Agents-style, MemGPT-style) | Episodes get summarised into long-term semantic memory. | **Partial** — the Episode → Concept consolidation idea is reused (see G4). Lacks code-graph anchoring. |
| **Hybrid graph memory** (structural + episodic + conceptual on one substrate) | Structural code graph as backbone; episodic events as a parallel layer; conceptual layer derived from episodes; reranker trained on episode labels. | **Selected** — meets all four commitments and matches the operator's "graph/expertise on top-down repo structure and call-chain context" phrasing exactly. |

### 3.2 Selected approach (one-paragraph statement)

**Hybrid graph memory anchored on a static, top-down code graph,
extended by an observed call-chain layer, recorded by an
append-only episodic layer, and consolidated into a global,
cross-repo Concept layer with a reranker trained on the same
episode log.** This is the design realised by the components in
`architecture.md` §2 and the data model in `architecture.md` §5;
this tech spec is the source of truth for the *strategy* commitment
and the parameter slots, not for the components or schemas.

### 3.3 What stays orthogonal across iterations

The strategy fixes three things that no later iteration of any plan
document is allowed to negotiate without re-opening this section:

1. **Three-layer memory.** Structural (G2 fingerprinted Nodes/Edges)
   + Episodic (`architecture.md` §5.3) + Conceptual
   (`architecture.md` §5.5). Reducing to two layers (e.g. dropping
   Concepts) is a different story.
2. **Static + dynamic call graph on the same Edge table** with
   `kind ∈ {static_calls, observed_calls}` and a `TraceObservation`
   child for the dynamic provenance (`architecture.md` §5.2.2,
   §5.2.3). Splitting them into separate stores is a different
   story.
3. **Reranker trained from labelled Episodes** including synthetic
   positives produced by G7 (`architecture.md` §3.6, §4.4). Swapping
   the supervision source (e.g. to LLM-judged labels only) is a
   different story.

Everything else — vendor choice, transport, embedding
model — is in the parameter-slot list in §8 and can be pinned per
iteration.

---

## 4. In Scope (this story)

The following items are committed deliverables of `AGENT-MEMORY`. A
release that omits any of them does not satisfy the story.

1. **Top-down structural graph** at method granularity with
   block-level subdivision for oversized methods, per
   `architecture.md` §3.7. Includes static edges:
   `contains`, `imports`, `static_calls`, `extends`, `implements`,
   `reads`, `writes`, `renamed_to` (`architecture.md` §5.2.2).
2. **Call-chain dynamic layer** with `observed_calls` edges fed by
   the OTel Collector (`architecture.md` §3.3, §5.2.2, §5.2.3).
3. **Append-only EpisodicLog** with Observation rows tying each
   Episode to the recall snapshot that produced it (G3;
   `architecture.md` §5.3, §5.4).
4. **Global, cross-repo ConceptStore** with append-only Concept,
   versioned ConceptVersion, and per-support-row ConceptSupport
   (G4, G6; `architecture.md` §5.5).
5. **Consolidator + Concept Promoter** workers
   (`architecture.md` §3.4, §3.5, §7.7, §7.8).
6. **Operator correction loop** that auto-promotes corrections to
   synthetic positive Episodes (G7; `architecture.md` §4.4, §7.3).
7. **Reranker training pipeline** that consumes Episodes as
   labelled supervision (`architecture.md` §3.6).
8. **Agent Surface** with `recall`, `observe`, `expand`,
   `summarize` (`architecture.md` §6.1).
9. **Management Surface** with `register`, `ingest`,
   `ingest_delta`, `ingest_spans`, `feedback`, `snapshot`,
   `read.*` (`architecture.md` §6.2).
10. **Webhook Receiver** + Repo Indexer (full/delta/manual modes)
    (`architecture.md` §3.1, §3.2).
11. **Append-only `RecallContextLog`** so any past recall can be
    replayed (`architecture.md` §5.4.1).
12. **Append-only tombstone tables** (`NodeRetirement`,
    `EdgeRetirement`) implementing G5
    (`architecture.md` §5.2.4).
13. **Degraded-mode behaviour** for every Agent and Management
    verb (`architecture.md` §6.3, §7.5, §7.6, §8.2).
14. **Read-only operator inspector contract** — the read endpoints
    in `architecture.md` §6.2.3. The UI build itself is out of
    scope (§5 item 1).

### 4.1 Single-tenant v1

V1 runs as a **single-tenant** service. There is one global
namespace for Concepts (G6) and no per-tenant ACL on Repos,
Episodes, or Concepts.

### 4.2 OTel-only dynamic-trace input

The only supported runtime-trace source for v1 is the configured
**OpenTelemetry Collector**. Any other source (py-spy, perf,
language-specific profilers) must be normalised upstream to OTel
before reaching this service (`architecture.md` §3.3).

---

## 5. Out of Scope (this story)

Items the operator may eventually want but that v1 does not ship.
Each line is paired to the architecture / story decision that put
it on this list. Numeric / vendor parameter pins are **not** on
this list — they are *in* scope of this document and pinned in §8
(see also §10 Locked Decisions).

1. **Operator UI implementation.** The read contract is in scope
   (§4 item 14); the actual web/desktop UI build, theming, and
   accessibility audit are out of scope (`architecture.md` §1.2
   row "UI design for the operator console").
2. **Per-tenant Concept isolation.** Single-tenant in v1 (G6;
   `architecture.md` §1.2 row "Per-tenant Concept isolation").
3. **Episode purge / GDPR delete tooling.** EpisodicLog is
   append-only forever per operator decision (G3;
   `architecture.md` §1.2 row "Episode purge / GDPR delete
   tooling").
4. **PII anonymisation inside trace payloads.** Out of scope;
   callers are responsible for OTel hygiene before ingest
   (`architecture.md` §1.2 row "Anonymising third-party PII inside
   trace payloads").
5. **Whole-line / statement-level static analysis.** The base
   structural granularity is `method`; `block` is the smallest
   *optional* subdivision, used only for methods that exceed the
   size threshold per §7.7 / C20 and `architecture.md` §3.7. Going
   finer than `block` (per-statement or per-line nodes) is out of
   scope (`architecture.md` §1.2 row
   "Whole-line/statement-level static analysis").
6. **Vendor-specific profilers as first-class sources** (py-spy,
   perf, etc.). Must be normalised to OTel upstream
   (`architecture.md` §1.2 row "py-spy / perf / vendor-specific
   profilers").
7. **Online learning / live weight updates of the reranker.**
   Reranker training is offline only; the *model class* is pinned
   in §8.4 (cross-encoder, BERT-class), the training-data shape is
   fixed by `architecture.md` §3.6.

---

## 6. Non-goals

Items that look adjacent to the story but are explicitly **not**
goals — i.e. work the team should refuse to take on inside this
story.

1. **Replacing the agent's reasoning model.** Agent Memory is the
   memory substrate. Prompt assembly, tool-use orchestration, and
   model selection live in the calling agent
   (`architecture.md` §3.10).
2. **Replacing the existing build / CI system.** Agent Memory
   *consumes* commits via webhook (`architecture.md` §3.1) and via
   `mgmt.ingest*` calls. It does not run builds, run tests, or
   produce artifacts itself.
3. **Replacing the OTel Collector.** OTel collection,
   sampling, and transport are upstream concerns
   (`architecture.md` §3.3, §4.2 above).
4. **Source-control hosting.** The Webhook Receiver verifies and
   consumes events from a configured git host; the service does
   not host repos.
5. **Code generation / code mutation.** Agent Memory is a recall +
   learning layer. Producing patches is the calling agent's
   responsibility.
6. **A general-purpose KG for non-code artifacts.** Tickets,
   wiki pages, design docs, etc. are not in the Node `kind` enum
   (`architecture.md` §5.2.1) and there is no plan to add them in
   v1.
7. **Online learning of the reranker.** Reranker training is
   offline, model-version-published only
   (`architecture.md` §3.6). Live weight updates are non-goal.
8. **Stream-level realtime alerting.** The Span Ingestor aggregates
   into edges; it does not raise alerts on hot-path anomalies.

---

## 7. Hard Constraints

These are non-negotiable. Each constraint cites the operator
decision (or directly-implied story commitment) that backs it.
Any sibling plan doc that contradicts a constraint here is out of
compliance and must be revised.

### 7.1 Identity and integrity

| # | Constraint | Source |
| --- | --- | --- |
| C1 | Every Node and Edge identity is a deterministic 32-byte fingerprint (G2). Two ingests of the same commit produce identical fingerprints. | `architecture.md` §1.3 G2, §5.2.1, §5.2.2 |
| C2 | Node and Edge rows are immutable post-insert; retirement is recorded by append-only tombstone rows in `NodeRetirement` / `EdgeRetirement` with `retired_at_sha = parent(new_HEAD)` (G5). | `architecture.md` §1.3 G5, §5.2.4 |
| C3 | Renames produce a *new* fingerprint and a stored `renamed_to` Edge; the inverse is a derived view, **not** a separately-stored `renamed_from` Edge kind. | `architecture.md` §1.3 G2, §5.2.2 |
| C4 | The Node `kind` enum is closed at `{repo, package, file, class, method, block}`. Concepts are not Nodes; there is no `concept_attaches` Edge kind. Code↔Concept links are carried exclusively by `ConceptSupport` rows. | `architecture.md` §5.2.1, §5.2.2, §5.5.3 |

### 7.2 Append-only event surfaces

| # | Constraint | Source |
| --- | --- | --- |
| C5 | `EpisodicLog` and `EpisodeUpdate` are append-only **forever**. No rotation, no rewrite, no GDPR-delete tooling in v1. | `architecture.md` §1.3 G3, §1.2, §8.1 |
| C6 | The original Episode row is never mutated. Status transitions on an Episode are recorded as new `EpisodeUpdate` rows. | `architecture.md` §1.3 G3, §4.4 |
| C7 | `RecallContextLog` is append-only forever and stores only ids (not payloads), so any past recall is replayable. | `architecture.md` §5.4.1, §8.1 |
| C8 | `TraceObservationLog` is append-only inside a configurable retention window (§8.1 carries the default). The `TraceObservation` aggregate row is **always** preserved — never pruned. | `architecture.md` §5.2.3, §5.6, §8.1 |

### 7.3 Concept layer

| # | Constraint | Source |
| --- | --- | --- |
| C9 | `Concept` rows are immutable. Confidence, support count, and embedding live on `ConceptVersion` (G4). | `architecture.md` §1.3 G4, §5.5.1, §5.5.2 |
| C10 | Concepts live in a **global** namespace from day one (no `repo_id` on Concept). Repo dimension is preserved per-support-row via `ConceptSupport.repo_id`. | `architecture.md` §1.3 G6, §3.8, §5.5.3 |
| C11 | The "current embedding" for a Concept is defined as the `embedding_vec` of the most-recent `ConceptVersion` with non-null `embedding_vec`, ordered by `version_index` desc. This is the single canonical rule shared by `EmbeddingIndex` writes and reads. | `architecture.md` §5.5.1, §5.5.2 |
| C12 | The **Concept Promoter** is the sole writer of Concept entries to the `EmbeddingIndex`. The Consolidator never writes EmbeddingIndex entries; it only emits `Concept` / `ConceptVersion` / `ConceptSupport` rows. | `architecture.md` §3.5, §7.7, §7.8 |

### 7.4 Read/write separation

| # | Constraint | Source |
| --- | --- | --- |
| C13 | Reads (`recall`, `expand`, `summarize`, every `mgmt.read.*`) never mutate the structural graph, episodic layer, or concept layer (G1). The single allowed write on a read path is the `RecallContextLog` append. | `architecture.md` §1.3 G1, §4.2, §4.5 |
| C14 | Writes (`observe`, `ingest*`, `feedback`) never block on read latency. The Span Ingestor and Webhook Receiver paths are independently scalable. | `architecture.md` §1.3 G1, §3.1, §3.3 |

### 7.5 Operator correction

| # | Constraint | Source |
| --- | --- | --- |
| C15 | `mgmt.feedback(outcome=human_corrected, …)` requires `corrected_action`. Other outcomes must omit it. `agent.observe` rejects `outcome=human_corrected`. | `architecture.md` §6.2.2 |
| C16 | A `human_corrected` correction **auto-produces** a synthetic positive Episode whose `context_id` is copied from the parent Episode, whose `action` is the operator's `corrected_action`, and whose `outcome` is `success` (G7). Provenance carries **both** `synthesized_from_parent_episode_id` and `synthesized_from_feedback_episode_id`. | `architecture.md` §1.3 G7, §3.4, §4.4, §5.3.1, §7.3 |
| C17 | Synthetic positive Episodes also get mirror Observation rows attached to the same recall elements as the parent Episode, so the positive signal lands on the same context. | `architecture.md` §3.4, §4.4 step 4, §7.3 step 4 |

### 7.6 Dynamic-trace input

| # | Constraint | Source |
| --- | --- | --- |
| C18 | Only OpenTelemetry spans (via the configured OTel Collector) are accepted by the Span Ingestor in v1. Any other source must be normalised to OTel upstream. | `architecture.md` §3.3, §1.2 row "OpenTelemetry spans" |
| C19 | The Span Ingestor never rewrites an Edge row. It only updates `TraceObservation` aggregate counters and appends `TraceObservationLog` rows; new `observed_calls` Edges are appended (with G2 fingerprints) when a call pair has not been seen before. | `architecture.md` §3.3, §5.2.3, §8.3 |

### 7.7 Granularity

| # | Constraint | Source |
| --- | --- | --- |
| C20 | Smallest structural Node is `method`. `block`-level subdivision is required for any method whose static body exceeds the size threshold (§8.2 carries the default). Block parentage uses the generic `parent_node_id` column — there is no separate `parent_method_id`. | `architecture.md` §3.7 |
| C21 | Call edges and Observation rows can target either a `method` Node or a `block` Node. If a Block is retired but its parent Method survives, the Block is tombstoned and references in older Episodes still resolve via the historical `Block.fingerprint`. | `architecture.md` §3.7, §5.2.4 |

### 7.8 Degraded mode

| # | Constraint | Source |
| --- | --- | --- |
| C22 | Every Agent and Management verb has a normative degraded-response shape. Allowed `degraded_reason` values form a fixed closed set: `episodic_log_unavailable`, `graph_store_unavailable`, `embedding_index_unavailable`, `reranker_model_stale`, `span_ingestor_backpressure`, `consolidator_backpressure`. | `architecture.md` §6.3, §7.5, §7.6, §8.2 |
| C23 | A `degraded_recall_context` Observation row is written by the server only — callers may not pass `role='degraded_recall_context'` on `agent.observe`. | `architecture.md` §5.3.3, §6.1.2 |
| C24 | An `agent.observe` call never fails because the Consolidator is backpressured; the Episode is queued and `degraded_reason` is set. | `architecture.md` §8.3 |

---

## 8. Parameter Pins (numeric / policy decisions locked by this tech spec)

`architecture.md` §10 defers a fixed set of numeric and vendor
decisions to this file. **This section pins each one as a locked
decision for v1.** A locked decision is normative; an operator
override would require a new story (or a follow-up that explicitly
reopens the relevant subsection of this document). The single-glance
roll-up of every lock is in §10.

> **Iter-3 operator pin update.** The operator has pinned the
> decisions in §8.1, §8.2, §8.4, and §8.5 verbatim (most as the
> proposed defaults). The notable substantive change is the
> **vector index → Qdrant** (a separate service, not pgvector on
> the same PostgreSQL instance); see §8.1, §9.6, and the §7.8
> degraded-mode contract for the implications. Two pins were
> answered non-decisively: **`slo-targets` ("don't know")** — the
> §8.3 numbers stand as the v1 contract but are explicitly
> **provisional, subject to load-test calibration** in the first
> release cycle (see §8.3 note); and **`otel-mapping`
> ("unresolved")** — the §8.6 mapping stands as pinned with its
> drop-and-count fallback but is **pending field-validation** as
> the first real OTel-instrumented repo onboards (see §8.6 note).

Each entry below lists: (a) the **locked value** for v1, (b) the
upper/lower bounds the slot may take *without* violating §7
(useful when a follow-up story considers an override), and (c) the
**override route** — the story / approval needed to change the lock.

### 8.1 Storage and indexing

| Slot | Locked value (v1) | Bounds | Override route |
| --- | --- | --- | --- |
| Primary durable store for Nodes / Edges / Episodes / Concepts | **PostgreSQL 16+** (single physical store, schema split by namespace). | Any ACID engine that supports composite UNIQUE indexes on `(repo_id, fingerprint)` and append-only INSERT-only workloads. KV-only stores rejected because of the `EpisodeUpdate ← Episode` join used by `mgmt.read.episodes`. | New story; revisit §8.1. |
| Vector index | **Qdrant** (separate service; per operator pin in iter 3). Repo Indexer writes Method/Block vectors; Concept Promoter writes Concept vectors. The EmbeddingIndex is therefore **not transactionally consistent** with the PostgreSQL store — see §7.8 / C22 (`embedding_index_unavailable` is a closed `degraded_reason`) and risk §9.6 for the staleness / outage handling. C12's sole-writer rule is unaffected: Repo Indexer and Concept Promoter remain the only writers. | Any vector index with k-NN cosine ≥ 1k-vector throughput, online insert, payload-filtering by `repo_id` / `kind`, and snapshot/restore. Single-process pgvector remains a viable lower-bound if separation cost is too high. | New story; revisit §8.1. |
| `TraceObservationLog` retention window | **30 days** rolling. | Lower bound: 7 days (debugging hot-path regressions across a sprint). Upper bound: unbounded if storage budget allows. Aggregate `TraceObservation` row is preserved regardless (C8). | New story; revisit §8.1. |
| EpisodicLog physical retention | **Forever** (C5). Sized at §8.3 throughput × planning horizon; partition by `created_at` month. | Lower bound: forever is non-negotiable. Upper bound: N/A. | Locked by operator decision; not overridable inside this story line. |

### 8.2 Granularity thresholds

| Slot | Locked value (v1) | Bounds | Override route |
| --- | --- | --- | --- |
| Method-to-Block split threshold | **80 logical lines** (matches `architecture.md` §3.7 default). A method exceeding this is decomposed into Blocks per §3.7. | Lower bound: 40 (very fine-grained, larger graph, higher embed cost). Upper bound: 200 (coarser, fewer Blocks, weaker localisation in Episodes). | New story; revisit §8.2. |
| Block kinds | **Closed set `{entry, branch, loop_body, exception, exit}`** per `architecture.md` §3.7. | The set is closed; extension requires reopening §3 of this doc. | Reopen §3 of this doc. |
| Embedded entry kinds (Node entries) | **`method`, `block`** — written at ingest time by the Repo Indexer (per `architecture.md` §3.5 EmbeddingIndex writer set). `file` / `class` Nodes are not embedded in v1. | Bounds: embedding `file` / `class` is a deferred optimisation; it cannot precede method/block coverage. | New story; revisit §8.2. |
| Embedded entry kinds (Concept entries) | **`concept`** — written at promotion time by the Concept Promoter (per `architecture.md` §3.5, §7.8). Concepts are **not Nodes** (C4); they are a separate entry class on the EmbeddingIndex. | Bounds: Concepts only become EmbeddingIndex entries after they cross the §7.8 promotion threshold (`confidence ≥ 0.7`, `support_count ≥ 5`). | New story; revisit §8.2. |

### 8.3 SLO targets and throughput envelopes

These are the contract numbers `architecture.md` §3.10 defers ("a
request/response round-trip of single-digit seconds for recall and
observe at p95 under nominal load"). They are normative for v1.

| Slot | Locked p95 target | Locked p99 target | Nominal-load envelope |
| --- | --- | --- | --- |
| `agent.recall` round-trip | **≤ 1.5 s** | ≤ 4 s | 50 RPS sustained, 200 RPS burst (1 min) |
| `agent.observe` round-trip | **≤ 400 ms** | ≤ 1.5 s | 50 RPS sustained, 200 RPS burst |
| `agent.expand` round-trip (depth ≤ 3) | **≤ 1.5 s** | ≤ 4 s | 20 RPS sustained |
| `agent.summarize` round-trip | **≤ 4 s** | ≤ 10 s | 5 RPS sustained |
| `mgmt.ingest_spans` batch (≤ 1k spans) | **≤ 2 s** | ≤ 5 s | 50 batches/min sustained |
| Webhook → first `Node` row visible | **≤ 30 s** for delta on ≤ 100 changed files | ≤ 2 min | 1 push / 5 s sustained per repo |
| Full ingest on 200 k LOC repo | **≤ 30 min** wall-clock | — | 4 Repo Indexer workers |

Two **learning-quality** SLO targets that fall out of §2.4:

| Slot | Locked value (v1) | Notes |
| --- | --- | --- |
| Rank-of-correct-node @ k=20 (median over labelled positive Episodes, last 7 days) | **≤ 5** | Measured by joining `Observation` rows on positive-outcome Episodes against the originating `RecallContextLog.node_ids` order. |
| Concept-hit fraction @ k=20 on `agent.recall` (last 7 days) | **≥ 25 %** | Measured by the share of `RecallContextLog.concept_ids` that lead to at least one positive-outcome Episode within the next 24 h. |

Override route for any of the §8.3 numbers: new story; revisit §8.3.

> **Provisional flag (iter-3 operator answer `slo-targets`).** The
> operator answered "don't know" on the §8.3 numbers. The values
> above stand as the v1 contract — they are what downstream load
> tests and the `reranker_model_stale` degraded flag will be
> measured against — but they are explicitly **provisional**: the
> first release cycle calibrates them against real traffic, and a
> follow-up iteration of this section pins the post-calibration
> values without re-opening the rest of §8.

### 8.4 Reranker and training cadence

| Slot | Locked value (v1) | Bounds | Override route |
| --- | --- | --- | --- |
| Reranker model class | **Cross-encoder, BERT-class, ≤ 200M params** | Lower bound: any pairwise-scoring model with ≤ 50 ms / 100-candidate latency. Upper bound: anything that satisfies p95 SLO in §8.3. Online learning is explicitly out (§6 non-goal 7). | New story; revisit §8.4. |
| Training cadence | **Nightly**, plus on-demand if labelled-Episode count grows by ≥ 5 % since the last run. | Lower bound: hourly (cost). Upper bound: weekly (signal staleness). | New story; revisit §8.4. |
| Training data window | **Trailing 90 days** of Episodes + all synthetic positives ever. | Lower bound: 30 days. Upper bound: all-time (cost). | New story; revisit §8.4. |
| Model registry | Same PostgreSQL instance, `reranker_model` table with `version`, `artifact_uri`, `trained_at`, `metrics_json`. GraphReader reads the latest published version on every request. | — | Locked. |

### 8.5 Transport and authentication

| Slot | Locked value (v1) | Bounds | Override route |
| --- | --- | --- | --- |
| Transport | **gRPC over HTTP/2** for Agent Surface, **REST + JSON** for Management Surface. | Bounds: either may be served on the other protocol if both `RecallResponse` and `ExpandResponse` envelopes carry identical fields. | New story; revisit §8.5. |
| AuthN | **mTLS** for Agent Surface (caller is a service), **OIDC bearer token** for Management Surface (caller is a human via the UI). | Bounds: any mutual-cryptographic scheme on Agent; any human-IdP scheme on Management. | New story; revisit §8.5. |
| AuthZ | **Single-tenant v1**: all callers have access to all repos. Per-tenant ACL is out of scope (§5 item 2). | — | Out of scope for v1; covered by a future multi-tenant story. |

### 8.6 OTel attribute mapping

The Span Ingestor (`architecture.md` §3.3) resolves spans to Method
or Block Nodes via OTel semantic-convention attributes. This tech
spec pins the mapping:

| Goal | OTel attribute(s) | Behaviour on miss |
| --- | --- | --- |
| Resolve to a Method Node | `code.namespace` + `code.function` | If either is missing, fall back to `code.filepath` + `code.lineno` and resolve to the enclosing Method via the structural graph. If still unresolved, drop the span and increment a `span_unresolved_total` counter. Do **not** create a synthetic Node. |
| Resolve to a Block Node | After the Method is resolved, use `code.lineno` against the Block boundaries recorded on Method ingest. | If no Block matches, attach the observation to the Method Node. |
| Caller side of an `observed_calls` Edge | OTel `parent_span_id`, recursively resolved through the same mapping above. | If the parent span is missing (root span), drop the edge contribution but record the latency on the destination Method's solo aggregate. |
| Trace correlation id | `trace_id` (OTel-native). | — |

The mapping is closed for v1. Override route: reopen §8.6 of this
doc.

> **Provisional flag (iter-3 operator answer `otel-mapping`).** The
> operator answered "unresolved" on §8.6. The mapping above stands
> as pinned (the drop-and-count fallback is normative; no
> synthetic Node is created on resolution miss). It is explicitly
> **pending field-validation** as the first real OTel-instrumented
> repo onboards — the `span_unresolved_total` counter from risk
> §9.1 / §9.11 is the calibration signal, and a follow-up
> iteration of §8.6 pins refinements (e.g. additional fallback
> attributes for language runtimes with weaker OTel auto-instr)
> without re-opening the rest of §8.

---

## 9. Identified Risks

Each risk lists: **trigger** → **impact** → **mitigation** →
**residual** (what remains after the mitigation is applied).

### 9.1 Graph drift between AST parse and runtime

- **Trigger.** The static parser identifies a method at one
  signature; the OTel span reports `code.function` at a slightly
  different signature (e.g. overloaded methods, generic erasure,
  lambdas inlined).
- **Impact.** `observed_calls` edges land on the wrong Method Node,
  or land on no Node at all, so call-chain expansion (§4.5 of
  `architecture.md`) is incomplete.
- **Mitigation.** Pin the OTel mapping in §8.6 with explicit
  fallback. Counter `span_unresolved_total` is published per repo;
  if it climbs above 1 % the Repo Indexer is re-run in `manual`
  mode against the current HEAD.
- **Residual.** Generics + reflection-heavy code may keep a small
  unresolved tail. Acceptable in v1.

### 9.2 EpisodicLog forever-retention storage blow-up

- **Trigger.** C5 (append-only forever) crossed with §8.3 throughput
  (50 RPS sustained on `observe`).
- **Impact.** Disk growth ~150 GB / month per RPS at average row
  size; without partitioning the table becomes unqueryable.
- **Mitigation.** Monthly `created_at` partitioning of `Episode`,
  `EpisodeUpdate`, and `Observation`. `mgmt.read.episodes` always
  carries a `since` filter (`architecture.md` §6.2.3) so the
  partition pruner can engage.
- **Residual.** Cold-partition queries (e.g. operator looking up a
  2-year-old run) will be slow. Acceptable.

### 9.3 Cross-repo Concept collision

- **Trigger.** G6 puts Concepts in a global namespace. Two repos
  with semantically different "retry-with-backoff" patterns could
  share a fingerprint if the canonical name + feature signature
  collide.
- **Impact.** A Concept aggregates `ConceptSupport` rows from both
  repos, polluting cross-repo recall.
- **Mitigation.** Concept fingerprint includes the
  observed-feature-signature, not just the human-readable name
  (`architecture.md` §5.5.1). The Concept Promoter only flips
  `promoted=true` when the latest `ConceptVersion` satisfies
  `confidence ≥ 0.7` and `support_count ≥ 5`
  (`architecture.md` §7.8). Cross-repo recall therefore only
  surfaces Concepts whose support is both deep (≥ 5) and well-
  calibrated (≥ 0.7).
- **Residual.** A novel cross-repo pattern with thin support may
  still mis-cluster on early ingests; the reranker absorbs the
  signal once corrections accumulate (G7).

### 9.4 Operator-correction poisoning

- **Trigger.** A bad-actor operator (or a well-meaning one with a
  flawed mental model) submits `mgmt.feedback(
  outcome=human_corrected, corrected_action=…)` on many Episodes,
  auto-promoting synthetic positives (C16).
- **Impact.** Reranker training set is skewed; future recall pulls
  toward the bad correction.
- **Mitigation.** The synthetic positive carries
  `synthesized_from_feedback_episode_id` (C16, §5.3.1); the
  Reranker Trainer (`architecture.md` §3.6) can apply a per-operator
  rate cap and require ≥ 2 distinct operators on any correction
  whose induced rank change exceeds a threshold. Per-operator cap
  is a §8.4 follow-up.
- **Residual.** A single trusted operator with bad judgement can
  still tilt the model. Accepted in single-tenant v1 (§4.1).

### 9.5 Cold-start (no Episodes yet)

- **Trigger.** A freshly registered repo has zero Episodes, so the
  reranker has no labelled supervision and the Concept layer is
  empty.
- **Impact.** First-week recall is purely embedding-similarity over
  Method/Block Nodes, with no Concept-hit @ k signal.
- **Mitigation.** The reranker has a "structural prior" fallback
  built into its v0 weights — pure cosine + structural-distance
  scoring. Concept-hit @ k SLO (§8.3) is explicitly measured over
  the trailing 7 days, so cold repos do not show up as red.
- **Residual.** Early-week recall quality on a new repo is worse
  than on a long-running one. Accepted.

### 9.6 Embedding model drift / re-embed cost

- **Trigger.** The embedding model is upgraded; existing Method,
  Block, and Concept vectors are no longer in the same vector
  space.
- **Impact.** Recall mixes incompatible vectors; rank collapses.
- **Mitigation.** Embedding model version is stored on every Node
  row (`attrs_json.embedding_model_version`) and on every
  ConceptVersion row. GraphReader's `EmbeddingIndex` queries are
  pinned to the currently-active version; an upgrade requires a
  bulk re-embed driven by the Repo Indexer (Method/Block) and the
  Concept Promoter (Concept). `mgmt.snapshot` (`architecture.md`
  §6.2.1) is the operator's lever to force this.
- **Residual.** Re-embed of a 200 k LOC repo costs ~30 min wall
  clock (§8.3); during that window recall serves degraded
  (C22, `embedding_index_unavailable`).

### 9.6a Cross-store staleness between PostgreSQL and Qdrant

- **Trigger.** §8.1 pins Qdrant as a **separate service** from the
  PostgreSQL store. Writes to Node / ConceptVersion rows in
  PostgreSQL and writes to the EmbeddingIndex in Qdrant are
  therefore **not in a single transaction**; a crash between the
  two can leave a Node row with no vector, or a vector with no
  row.
- **Impact.** Recall returns vectors that dereference to missing
  Node ids (or, on the inverse path, a Node with no vector that is
  invisible to recall until re-embedded).
- **Mitigation.** Each writer (Repo Indexer for Method/Block,
  Concept Promoter for Concept) follows a fixed two-phase order:
  (1) write the PostgreSQL row first with a sentinel
  `embedding_state='pending'`; (2) write the Qdrant vector with
  the row's primary key as the Qdrant point id; (3) update the
  PostgreSQL row's `embedding_state='ready'`. GraphReader filters
  recall hits whose `embedding_state != 'ready'`. On crash
  recovery, both writers re-scan rows where `embedding_state IN
  ('pending', 'ready')` against the current Qdrant snapshot and
  reconcile. Qdrant outages surface through C22 as
  `embedding_index_unavailable`.
- **Residual.** A long Qdrant outage during a heavy delta ingest
  leaves a backlog of `pending` rows; recall degrades smoothly
  because those rows are filtered out. Re-embedding throughput is
  bounded by §8.3 ingest envelope.

### 9.7 Tombstone churn from automated refactors

- **Trigger.** A bulk-rename commit (e.g. linter-driven package
  rename) retires thousands of Nodes/Edges in one push, producing
  thousands of `NodeRetirement` / `EdgeRetirement` rows in a single
  `delta-ingest` job.
- **Impact.** Spike in tombstone table size; "current" queries that
  anti-join the tombstone tables get slower.
- **Mitigation.** Tombstone tables have a unique index on
  `(node_id)` / `(edge_id)` (`architecture.md` §5.2.4); the
  anti-join is keyed and remains O(log N). For pure renames the
  Repo Indexer also writes a `renamed_to` Edge (C3) so the new
  Node inherits relevance signal.
- **Residual.** A formatter-only commit with no semantic change can
  still produce churn if `canonical_signature` is sensitive to
  whitespace. The parser must normalise whitespace before
  fingerprinting (Repo Indexer responsibility per
  `architecture.md` §3.2).

### 9.8 Synthetic-positive double-count

- **Trigger.** The Consolidator (§7.7 step 4) emits one synthetic
  positive per parent Episode that was newly marked
  `human_corrected`. A bug in the Consolidator restart logic
  re-emits the same synthetic positive.
- **Impact.** Reranker training set contains duplicate positive
  pairs; bias toward the corrected action.
- **Mitigation.** `synthesized_from_feedback_episode_id` is unique
  per synthetic positive (C16). A unique index on
  `(kind='synthetic_positive', synthesized_from_feedback_episode_id)`
  enforces single-emission across restarts.
- **Residual.** None expected once the unique index is in place.

### 9.9 Block-boundary instability across formatters

- **Trigger.** A code formatter rewrites whitespace inside a method
  body; logical-line counts shift; Blocks fingerprinted on the old
  body do not match the new one.
- **Impact.** All Blocks under that Method are retired and
  re-introduced with new fingerprints; older Episodes lose direct
  references.
- **Mitigation.** Older Episodes still resolve via the historical
  `Block.fingerprint` (`architecture.md` §3.7, C21). Block boundary
  detection is performed on a *normalised* AST node, not on raw
  source — Repo Indexer responsibility per `architecture.md` §3.2.
- **Residual.** Recall on the retired Blocks shows them as
  tombstoned; the reranker degrades smoothly to the parent Method.

### 9.10 Reranker model staleness

- **Trigger.** Nightly training run (§8.4 default) skipped for
  several days due to compute outage; reranker degrades.
- **Impact.** Recall quality declines; corrections accumulate
  unused.
- **Mitigation.** `reranker_model_stale` is one of the closed
  `degraded_reason` values (C22). GraphReader publishes a
  `last_trained_at` metric; if it exceeds 7 days, recall responses
  carry `degraded=true, degraded_reason='reranker_model_stale'`.
- **Residual.** None — degraded mode is contract-fixed (§7.8).

### 9.11 OTel attribute coverage gaps

- **Trigger.** The OTel auto-instrumentation for the language in
  question does not emit `code.function` (older Python agents are a
  known case).
- **Impact.** Span Ingestor cannot resolve spans to Method Nodes;
  `observed_calls` Edges are not written.
- **Mitigation.** Fallback to `code.filepath` + `code.lineno`
  (§8.6). If both are missing, the span is dropped and counted in
  `span_unresolved_total`. Operator dashboard surfaces this
  counter per repo.
- **Residual.** Repos with unfixable OTel agents will have no
  dynamic layer until the agent is upgraded. Static layer is
  unaffected.

### 9.12 Webhook signature spoofing

- **Trigger.** A malicious caller crafts a webhook request with a
  forged signature header.
- **Impact.** A fake `RepoEvent` could enqueue a delta-ingest job
  against an unreal SHA; the Repo Indexer would no-op
  (`from_sha`/`to_sha` not found in the git host) but the noise
  would obscure real events.
- **Mitigation.** Webhook Receiver verifies HMAC signature against
  the per-repo secret (configured at `mgmt.register` time).
  Unverified requests are rejected with 401; no `RepoEvent` row is
  written.
- **Residual.** A compromised webhook secret bypasses this. Secret
  rotation procedure deferred to operations runbook.

### 9.13 Stale RecallContext replay

- **Trigger.** An operator opens an Episode whose `context_id`
  points to a `RecallContextLog` row from 6 months ago; the Nodes
  referenced have since been retired (G5 tombstones).
- **Impact.** The UI cannot fully de-reference the context.
- **Mitigation.** `mgmt.read.context` (`architecture.md` §6.2.3)
  returns the tombstone status per id; the UI shows retired Nodes
  with a "retired at SHA …" badge. The Episode and Observation rows
  remain fully readable.
- **Residual.** Visualisation is partial; the audit trail is intact.

---

## 10. Locked Decisions (single-glance roll-up)

This section is the single-glance roll-up of every parameter pin made
elsewhere in this document. **All entries are locked decisions for
v1.** Each row gives the pinned value and the section where the
rationale, bounds, and override route are documented. There are no
open questions blocking this story; an operator override of any
locked decision requires a new story that reopens the cited section.

| Decision | Locked value (v1) | Defined in | Override route |
| --- | --- | --- | --- |
| Primary durable store | PostgreSQL 16+ (schema split by namespace) | §8.1 | New story; revisit §8.1 |
| Vector index | Qdrant (separate service) | §8.1 | New story; revisit §8.1 |
| `TraceObservationLog` retention window | 30 days rolling | §8.1 | New story; revisit §8.1 |
| `EpisodicLog` physical retention | Forever (append-only, partition by month) | §8.1, C5 | Not overridable inside this story line |
| Method-to-Block split threshold | 80 logical lines | §8.2 | New story; revisit §8.2 |
| Block kinds | Closed set `{entry, branch, loop_body, exception, exit}` | §8.2 | Reopen §3 of this doc |
| Embedded entry kinds — Node entries (v1) | `method`, `block` (written at ingest by Repo Indexer) | §8.2 | New story; revisit §8.2 |
| Embedded entry kinds — Concept entries (v1) | `concept` (written at promotion by Concept Promoter; Concepts are not Nodes per C4) | §8.2, §7.8 | New story; revisit §8.2 |
| `agent.recall` p95 / p99 | ≤ 1.5 s / ≤ 4 s @ 50 RPS sustained | §8.3 | New story; revisit §8.3 |
| `agent.observe` p95 / p99 | ≤ 400 ms / ≤ 1.5 s @ 50 RPS sustained | §8.3 | New story; revisit §8.3 |
| `agent.expand` (depth ≤ 3) p95 / p99 | ≤ 1.5 s / ≤ 4 s @ 20 RPS sustained | §8.3 | New story; revisit §8.3 |
| `agent.summarize` p95 / p99 | ≤ 4 s / ≤ 10 s @ 5 RPS sustained | §8.3 | New story; revisit §8.3 |
| `mgmt.ingest_spans` batch (≤ 1k spans) p95 / p99 | ≤ 2 s / ≤ 5 s @ 50 batches/min | §8.3 | New story; revisit §8.3 |
| Webhook → first Node row visible | ≤ 30 s (delta ≤ 100 files) / ≤ 2 min p99 | §8.3 | New story; revisit §8.3 |
| Full ingest, 200 k LOC repo | ≤ 30 min wall-clock @ 4 workers | §8.3 | New story; revisit §8.3 |
| Rank-of-correct-node @ k=20 (median, 7-day) | ≤ 5 | §8.3 | New story; revisit §8.3 |
| Concept-hit fraction @ k=20 (7-day) | ≥ 25 % | §8.3 | New story; revisit §8.3 |
| Reranker model class | Cross-encoder, BERT-class, ≤ 200M params | §8.4 | New story; revisit §8.4 |
| Reranker training cadence | Nightly + on-demand on ≥ 5 % labelled-Episode growth | §8.4 | New story; revisit §8.4 |
| Reranker training data window | Trailing 90 days + all-time synthetic positives | §8.4 | New story; revisit §8.4 |
| Reranker model registry | Same PostgreSQL instance, `reranker_model` table | §8.4 | Locked |
| Agent Surface transport | gRPC over HTTP/2 | §8.5 | New story; revisit §8.5 |
| Management Surface transport | REST + JSON | §8.5 | New story; revisit §8.5 |
| Agent Surface authN | mTLS | §8.5 | New story; revisit §8.5 |
| Management Surface authN | OIDC bearer token | §8.5 | New story; revisit §8.5 |
| AuthZ scope (v1) | Single-tenant, no per-repo ACL | §8.5, §4.1 | Future multi-tenant story |
| OTel span → Method resolution | `code.namespace` + `code.function`, fallback `code.filepath` + `code.lineno`, then drop and count | §8.6 | Reopen §8.6 of this doc |
| OTel span → Block resolution | `code.lineno` against ingested Block boundaries; fallback to parent Method | §8.6 | Reopen §8.6 of this doc |
| Caller side of `observed_calls` edge | OTel `parent_span_id`, recursively resolved; root → solo aggregate | §8.6 | Reopen §8.6 of this doc |
| Trace correlation id | OTel-native `trace_id` | §8.6 | Locked |

---

## 11. Cross-references

- Component map, data model, public-interface contracts, and
  end-to-end flows: `architecture.md`.
- Workstream sequencing, milestones, and rollout: `implementation-plan.md`.
- Numbered end-to-end test scenarios: `e2e-scenarios.md`.
- Awesome-GraphMemory survey (operator-supplied starting point):
  <https://github.com/DEEP-PolyU/Awesome-GraphMemory>.
