---
title: "Method and Block embedding publication"
slug: "method-and-block-embedding-publication"
parent_story: "code-intelligence:AGENT-MEMORY"
parent_phase: "phase-static-ingestion-pipeline"
parent_stage: "stage-method-and-block-embedding-publication"
anchors:
  - tech-spec.md §8.7.1 (Schema type mapping for `EmbeddingPublish` / `EmbeddingPublishEvent`)
  - tech-spec.md §9.6a (Cross-store staleness write + read protocol)
  - tech-spec.md §9.6 (Embedding-model upgrade)
  - implementation-plan.md Stage 3.3 (Method and Block embedding publication)
  - e2e-scenarios.md scenarios at lines 134-145, 425-433, 454-462
---

# Method and Block embedding publication — design plan

## Problem

The Repo Indexer commits Method and Block `Node` rows into PostgreSQL during
the Stage 3.1 / 3.2 full-mode ingest. The vector embedding for each row
lives in a **separate** service (Qdrant — pinned in tech-spec §8.1) so the
write across PostgreSQL and Qdrant is **not** a single transaction. Without
a published-state log the GraphReader cannot tell whether a Qdrant hit
still corresponds to a committed Node, an upcoming Node, or a stale Node
left over from a half-finished publish.

Tech-spec §9.6a pins the mitigation: two append-only operational tables
(`EmbeddingPublish` + `EmbeddingPublishEvent`) and a strict 5-step write
protocol that records every transition without ever rewriting a row, so
the cross-store invariant is observable from the read side as
"latest `EmbeddingPublishEvent` for this `publish_id` = `'published'`".
This stage owns the **writer** that drives that protocol and the
**reader filter** that consults it.

Constraints inherited from upstream docs:
- G3 / G4 / G5 — no Node, ConceptVersion, EmbeddingPublish, or
  EmbeddingPublishEvent row is ever updated. Every state transition is a
  fresh row in `EmbeddingPublishEvent`.
- §9.6 — every `EmbeddingPublish` carries `embedding_model_version`; the
  reader filters by `<active version>`.
- C22 — Qdrant outages surface as `degraded_reason='embedding_index_unavailable'`
  and must not block PostgreSQL ingest; the publisher leaves the latest
  event at `'queued'` / `'failed'` and a background flusher retries.
- §8.3 — full ingest of 200 k LOC ≤ 30 min, so the publisher must run
  concurrently with the ingest worker pool, not serialise it.

## Approach

A new `internal/embedding/` package owns three responsibilities, each
behind a narrow Go interface so the integration tests can stub the
external systems:

1. **Adapters** — `QdrantClient` (upsert + read-after-write fetch) and
   `EmbeddingModel` (text → vector + active `embedding_model_version`).
2. **Publish log** — append-only INSERT helpers for `EmbeddingPublish`
   and `EmbeddingPublishEvent`, with the role grants from tech-spec
   §8.7.4 enforcing the no-UPDATE / no-DELETE contract at the DB layer.
3. **Publisher state machine** — implements §9.6a steps 2–5 inline for
   the foreground call from the full-mode handler, and is reused by a
   background flusher goroutine that polls `EmbeddingPublish` rows whose
   latest event is `'queued'` or `'failed'` and re-runs the protocol.

The GraphReader's recall path gains a `published-filter` that joins
Qdrant hits to `EmbeddingPublishEvent` via the
`(publish_id, created_at DESC)` index from §8.7.2 and excludes any row
whose latest event is not `'published'` **or** whose
`embedding_model_version` ≠ the active version, incrementing
`recall_filter_unpublished_total` (e2e-scenarios.md L460) per filtered
hit.

### Why an append-only log instead of a transactional outbox

A 2PC across PostgreSQL and Qdrant is rejected by §8.1 (separate
services, no shared transaction). A transactional outbox table would
work but would still require mutation of an outbox row from "claimed"
to "done", which collides with the G3 / G5 immutability rule that
tech-spec §9.6a explicitly extends to `EmbeddingPublish` rows. The
append-only event-log shape is the only shape compatible with the
existing append-only role grants (§8.7.4) and is also what risk §9.6
needs for embedding-model upgrades: a new `EmbeddingPublish` row at
the new `embedding_model_version`, then a `superseded` event on the
prior `publish_id` — no row rewrite.

### Why the full-mode handler calls the publisher inline

The Stage 3.1 full-mode handler already runs its AST emit + GraphWriter
insert per file in a worker pool sized for the §8.3 budget. Issuing the
publish call inline (after the GraphWriter transaction commits) keeps
the per-file unit of work cohesive and avoids a separate queue table
for "things waiting to be embedded" — the `EmbeddingPublish` log row
itself doubles as that queue. The background flusher only handles
failures and Qdrant outages; the steady-state path never touches it.

### Why a background flusher is mandatory, not optional

If Qdrant is unreachable for the duration of a 30-minute full ingest,
the inline publisher will append `'failed'` for every Method / Block
and the ingest job will still mark `status='done'` because the
PostgreSQL writes succeeded. Without a flusher, those vectors would
never reach `'published'` — recall would silently degrade forever. The
flusher closes that loop by re-running the §9.6a protocol on any row
whose latest event is `'queued'` or `'failed'`, **without** mutating
the prior row (a fresh `'queued'` event is appended per protocol
step 4's failure clause).

### Embedding model version

`embedding_model_version` is sourced from a single config field
(default `e5-code-v1`, matching e2e-scenarios.md L69) and stamped on
every `EmbeddingPublish` row at insert. The active version is also
surfaced through the service `/health` endpoint so an operator (or
the upgrade procedure in tech-spec §9.6) can confirm what the
publisher and the reader-filter are agreeing on.

## Decomposition

The work splits cleanly into three stages — adapters & log, write
protocol & wiring, read-side & verification — driven by what changes
across files:

- **Stage 1** (Embedding publisher core) introduces the new
  `internal/embedding/` package and its outbound dependencies. No
  existing code is modified.
- **Stage 2** (§9.6a state machine + wiring) wires the state machine
  into the existing Stage 3.1 worker and adds the background flusher.
  Modifies the full-mode handler.
- **Stage 3** (Read-side filter + verification) modifies the
  GraphReader recall path, adds the OTel metrics from
  e2e-scenarios.md, and lands the §9.6a integration test that asserts
  the three test scenarios from the work-item description.

Each step is one PR. Each step's `expectedFileChanges` is honest about
the file count for that PR (≤ 20 enforced server-side).

## Phase 1: Method and Block embedding publication

### Stage 1: Embedding publisher core

- **step-1-package-scaffold** (`expectedFileChanges: 5`) — create
  `internal/embedding/` with `doc.go`, `types.go` (PublishTarget,
  PublishOutcome, EventKind enum mirroring §9.6a closed set), and the
  two interfaces `QdrantClient` and `EmbeddingModel`. No behaviour;
  unit test asserts the enum members exactly match the §9.6a closed
  set `{queued, vector_written, published, failed, superseded}`.
- **step-2-qdrant-adapter** (`expectedFileChanges: 4`) — implement
  `qdrant.go` over the Qdrant gRPC SDK exposing `UpsertPoint` and
  `FetchPoint` (the read-after-write check in §9.6a step 5). Config
  via env (`AM_QDRANT_ENDPOINT`, `AM_QDRANT_COLLECTION`). Unit test
  with a fake transport asserts payload includes `repo_id` + `kind`
  for §9.6a filtering.
- **step-3-embedding-model-client** (`expectedFileChanges: 4`) —
  implement `model_client.go` (HTTP) returning `(vector, version)`;
  `version` defaults to `e5-code-v1` (e2e-scenarios.md L69) and is
  exposed via a `Version()` accessor for downstream stamping. Unit
  test with a fake server.
- **step-4-publish-log-repo** (`expectedFileChanges: 6`) — append-only
  INSERT helpers `InsertPublish` and `InsertPublishEvent` in
  `publish_log.go` against the `EmbeddingPublish` /
  `EmbeddingPublishEvent` tables migrated in
  `phase-foundation-and-schema/stage-embedding-state-log-migrations-and-roles`.
  Unit test asserts the prepared SQL has no `UPDATE`/`DELETE` keyword
  (defence-in-depth against accidental mutation).

### Stage 2: §9.6a write protocol + wiring

- **step-5-publisher-state-machine** (`expectedFileChanges: 6`) —
  implement `publisher.go` realising §9.6a steps 2–5: insert
  `EmbeddingPublish` referencing the already-committed `Node.node_id`,
  insert `queued` event, embed, upsert to Qdrant, insert
  `vector_written` (or `failed`), read-after-write fetch, insert
  `published`. The function is reentrant — the **only** input is the
  `Node` row; it never reads from `EmbeddingPublish` to decide what to
  do (the flusher handles retries by calling it again with the same
  Node). Unit test against fake adapters asserts the 5 INSERTs land
  in the expected order and that **no UPDATE** statement is issued.
- **step-6-model-version-stamping** (`expectedFileChanges: 3`) — wire
  `EmbeddingModel.Version()` into every `EmbeddingPublish` insert and
  add a config flag `AM_EMBEDDING_MODEL_VERSION` that overrides the
  client default. Publisher refuses to start if the configured version
  is empty. Unit test asserts every inserted row carries the stamped
  version.
- **step-7-full-mode-handler-call-site** (`expectedFileChanges: 4`) —
  modify `internal/repoindexer/worker.go` (or the existing full-mode
  handler hook) to invoke the publisher for every Method / Block Node
  emitted by the AST dispatcher, after the GraphWriter transaction
  commits. Failures must not fail the ingest job — they append
  `'failed'` and rely on the flusher. Integration test asserts a
  successful full ingest of a 3-method fixture yields three
  `'published'` events.
- **step-8-background-flusher** (`expectedFileChanges: 5`) —
  implement `flusher.go` as a worker goroutine started by the
  `agent-memory` server. Polls `EmbeddingPublish` rows whose latest
  `EmbeddingPublishEvent` is `'queued'` or `'failed'` (LATERAL JOIN
  on the `(publish_id, created_at DESC)` index from §8.7.2) older
  than a backoff, then calls the publisher again. New attempts
  produce a new `'queued'` row, never an UPDATE. Integration test
  uses a fake Qdrant that fails once then succeeds and asserts the
  row reaches `'published'`.

### Stage 3: Read-side filter and verification

- **step-9-graphreader-published-filter** (`expectedFileChanges: 5`) —
  modify `internal/graphreader/` to filter Qdrant hits through the
  §9.6a "latest event = `'published'`" join **and**
  `EmbeddingPublish.embedding_model_version = <active>` predicate.
  Filtered hits are replaced from the next-best candidate so
  `len(nodes) + len(concepts) == k` (e2e-scenarios.md L461).
- **step-10-metrics-and-degraded-flag** (`expectedFileChanges: 4`) —
  add OTel counters: `embedding_publish_total{event_kind}`,
  `embedding_publish_latency_seconds` (histogram),
  `recall_filter_unpublished_total` (the filter increment from
  e2e-scenarios.md L460). On a sustained Qdrant outage the publisher
  also surfaces `degraded_reason='embedding_index_unavailable'`
  through the health endpoint per C22.
- **step-11-9_6a-integration-test** (`expectedFileChanges: 4`) —
  add `publisher_integration_test.go` against the
  `deploy/local/docker-compose.yml` stack (PostgreSQL + Qdrant). Three
  test cases mirror the work-item scenarios: (a) publish state log
  is complete — exactly one each of `queued`, `vector_written`,
  `published` in order; (b) transient Qdrant error appends `failed`
  and the flusher produces a new `queued` event (not an UPDATE) that
  eventually reaches `published`; (c) a row whose latest event is
  `queued` is excluded from `agent.recall` and increments
  `recall_filter_unpublished_total`.

## Out of scope

- **Concept embedding publication.** Tech-spec §9.6a applies the same
  protocol to `ConceptVersion` rows written by the Concept Promoter,
  but that writer is built in a later phase (Concept promotion). The
  publisher / log helpers in Stage 1 are designed to be reused by it
  — `PublishTarget` accepts either `node_id` or
  `concept_version_id` — but wiring the Promoter is not part of this
  stage.
- **Bulk re-embed driver for model upgrades.** Risk §9.6 mandates a
  `mgmt.snapshot`-driven bulk re-embed when `embedding_model_version`
  is bumped. The publisher already supports it (a new
  `EmbeddingPublish` at the new version + `superseded` event on the
  prior `publish_id`), but the operator-facing job is a later story.
- **Delta-mode re-embed.** Stage 3.4 (Delta re-index handler) calls
  the publisher for Methods / Blocks whose canonical signature
  changed. That call site is not part of this stage; it is wired in
  the Stage 3.4 PR.
- **Migrations for `EmbeddingPublish` / `EmbeddingPublishEvent`.**
  Those DDLs are already owned by
  `phase-foundation-and-schema/stage-embedding-state-log-migrations-and-roles`
  (see implementation-plan.md L177 and the
  `migrations/00xx_embedding_publish*.sql` files referenced there).
  This stage takes them as a hard dependency and does not modify them.
- **Reranker / structural-prior fallback.** Risk §9.5 covers the
  read-side fallback when the EmbeddingIndex is unavailable. The
  publisher's failure modes surface the C22 degraded flag, but the
  fallback ranking itself is owned by the GraphReader / Reranker
  stages.

## Open questions (for the implementing crew, not blockers)

- Should the flusher use PostgreSQL `LISTEN/NOTIFY` (the v1 event bus
  per Stage 3.1) to wake up on every new `'failed'` event, or stick
  to a simple poll? Poll is the safer default for v1; LISTEN can be
  added later without protocol change.
- Backoff curve for the flusher — proposed: exponential, capped at
  5 min, abandoned after 24 h with an alert. Confirm with operations
  during code review.
