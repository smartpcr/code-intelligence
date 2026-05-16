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
- §8.7.1 — every `EmbeddingPublishEvent` carries `attempt_index`. Retries
  reuse the parent `publish_id` and increment `attempt_index`; they do
  **not** insert a new `EmbeddingPublish` row.
- C22 — `degraded_reason='embedding_index_unavailable'` is an **Agent/
  Management verb response field** (tech-spec.md L414). Qdrant outages
  surface it on the recall **read** verb's response, not on the writer.
  The writer simply leaves the latest event at `'queued'` / `'failed'`,
  must not block PostgreSQL ingest, and relies on the background flusher.
- §8.3 — full ingest of 200 k LOC ≤ 30 min, so the publisher must run
  concurrently with the ingest worker pool, not serialise it.

## Approach

A new `internal/embedding/` package owns three responsibilities, each
behind a narrow Go interface so the integration tests can stub the
external systems:

1. **Adapters** — `QdrantClient` (`UpsertPoint`, `FetchPoint`) and
   `EmbeddingModel` (text → vector + active `embedding_model_version`).
   Every Qdrant point carries the full identity payload required to
   dereference a hit back to its `EmbeddingPublish` row without a second
   round trip: `publish_id`, `target_id`, `target_kind`
   (`'node'` | `'concept_version'`), `repo_id`, `kind`,
   `embedding_model_version`. The `point_id` itself is deterministic
   (`uuid_v5(publish_id)`) so the same `point_id` survives retries.
2. **Publish log** — append-only INSERT helpers for `EmbeddingPublish`
   and `EmbeddingPublishEvent`, with the role grants from tech-spec
   §8.7.4 enforcing the no-UPDATE / no-DELETE contract at the DB layer.
3. **Publisher state machine** — two narrow entry points share one
   inner core that implements §9.6a steps 3–5 (queued → vector_written
   → published) for a given `(publish_id, point_id, attempt_index)`:
     - `PublishNew(ctx, node)` — used by the full-mode handler. Inserts
       a fresh `EmbeddingPublish` (§9.6a step 2) with a freshly minted
       `publish_id` and deterministic `point_id`, then calls the inner
       core with `attempt_index = 1`.
     - `RetryExisting(ctx, publish)` — used by the background flusher.
       Reuses the supplied `EmbeddingPublish` row's `publish_id` and
       `point_id`; reads `max(attempt_index)` from
       `EmbeddingPublishEvent` for that `publish_id` and calls the
       inner core with `attempt_index = max+1`. **Never** inserts a new
       `EmbeddingPublish` row.

The GraphReader's recall path gains a `published-filter` that joins
Qdrant hits to `EmbeddingPublishEvent` via the Qdrant payload's
`publish_id` (primary key into the log) plus the
`(publish_id, created_at DESC)` index from §8.7.2, and excludes any row
whose latest event is not `'published'` **or** whose
`embedding_model_version` ≠ the active version, incrementing
`recall_filter_unpublished_total` (e2e-scenarios.md L460) per filtered
hit. When the Qdrant cosine query itself fails (not the join), the
recall verb's response carries `degraded=true,
degraded_reason='embedding_index_unavailable'` (C22).

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
flusher closes that loop by calling `RetryExisting` on any
`EmbeddingPublish` whose latest event is `'queued'` or `'failed'`. The
flusher takes the **existing `EmbeddingPublish` row** as input (not the
Node), so `publish_id`, `point_id`, `target_id`, `target_kind`, and
`embedding_model_version` are all reused; only a fresh
`'queued'`/`'vector_written'`/`'published'` event chain with
`attempt_index = N+1` is appended (per §9.6a step 4's failure clause,
which mandates "a new `'queued'` event row, never an update").

### Identity contract (what gets reused on retry)

| Field                       | New publish (full-mode)       | Retry (flusher)                  |
| --------------------------- | ----------------------------- | -------------------------------- |
| `publish_id`                | freshly minted (uuid_v7)      | reused from input row            |
| `point_id`                  | `uuid_v5(publish_id)`         | reused from input row            |
| `target_id`, `target_kind`  | from the just-committed Node  | reused from input row            |
| `embedding_model_version`   | from `EmbeddingModel.Version()` at insert time | **always reused as-is.** If it no longer matches the active version, the reader's published-filter silently drops the hit (§9.6). The model-upgrade supersede flow — *new* `EmbeddingPublish` at the active version plus a `superseded` event on the prior `publish_id` — is **owned by the bulk re-embed driver and is out of scope here**. The flusher in this stage **never** mints a new `EmbeddingPublish`. |
| `attempt_index` (on event)  | `1`                           | `SELECT max(attempt_index) + 1 FROM EmbeddingPublishEvent WHERE publish_id = ?`, wrapped in a transaction holding `pg_advisory_xact_lock(hashtext(publish_id::text))` so concurrent flusher replicas cannot both append `N+1` (see "Race-safety" below). |
| `EmbeddingPublish` row      | **inserted**                  | **not inserted**                 |

This is what makes the retry path duplication-free: there is one and
only one `EmbeddingPublish` per intended publish, and the event chain
under it grows monotonically by `attempt_index`.

### Race-safety on retry

Two flusher replicas could otherwise both `SELECT max(attempt_index)`
on the same `publish_id`, see `N`, and both append events at `N+1`.
`RetryExisting` therefore opens a single PostgreSQL transaction that
calls `pg_advisory_xact_lock(hashtext(publish_id::text))` **before**
the max read, runs the entire event-chain append (`queued` →
`vector_written` / `failed` → `published`) inside that transaction,
and commits. The advisory lock is released on commit so the per-row
lock window is short. This is the only serialisation primitive the
stage needs because (a) the `EmbeddingPublish` row is inserted exactly
once by `PublishNew` (no contention), (b) `EmbeddingPublishEvent`
inserts only collide on `(publish_id, attempt_index)`, and (c) the
flusher's eligibility predicate (next bullet) does not require global
locks.

### Stale `vector_written` is a flusher-eligible state too

If the publisher crashes — or the §9.6a step 5 `FetchPoint`
read-after-write check itself fails — between `vector_written` and
`published`, the latest event remains `vector_written` and a naïve
flusher that only retries `queued` / `failed` would leave the row
**permanently invisible** to recall (the published-filter excludes it
and nothing ever re-drives it). The flusher's eligibility predicate
therefore is:

> *latest event ∈ {`queued`, `failed`} past the standard backoff,*
> ***or*** *latest event = `vector_written` whose `created_at` is older
> than the read-after-write staleness threshold (proposed 60 s,
> tunable via `AM_PUBLISH_RAW_STALENESS`).*

`RetryExisting` handles all three uniformly by **restarting the full
event chain** at `attempt_index = max+1` (`queued` → re-upsert →
`vector_written` → re-check → `published`). The re-upsert is safe
because `point_id = uuid_v5(publish_id)` is deterministic, so Qdrant
treats the second upsert as an in-place update of the same point —
no orphan vectors, no duplicate hits. This keeps the publisher's
public surface to exactly `PublishNew` + `RetryExisting` (no third
"resume-from-vector_written" method).

### Embedding model version

`embedding_model_version` is sourced from a single config field
(default `e5-code-v1`, matching e2e-scenarios.md L69) and stamped on
every `EmbeddingPublish` row at insert. The active version is also
surfaced through the service `/health` endpoint as an **operational
observability** signal (publish backlog gauge + active model version)
so an operator (or the upgrade procedure in tech-spec §9.6) can confirm
what the publisher and the reader-filter are agreeing on. `/health` is
**not** the C22 degraded-response carrier — that is the recall verb's
response field, surfaced when the cosine query against Qdrant itself
fails (see "Read-side filter" stage below).

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
  e2e-scenarios.md, and lands the §9.6a integration tests that assert
  the test scenarios from the work-item description plus the
  identity-reuse contract from the prior-feedback resolution.

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
  with a fake transport asserts the upsert payload carries the full
  identity tuple so a recall hit can be dereferenced to
  `EmbeddingPublish` in a single SQL lookup: `publish_id` (PK into the
  log), `target_id`, `target_kind ∈ {'node','concept_version'}`,
  `repo_id`, `kind`, `embedding_model_version`. The test also asserts
  `point_id == uuid_v5(publish_id)` so retries are idempotent in
  Qdrant.
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
  implement `publisher.go` realising §9.6a steps 2–5 behind **two
  entry points** that share one inner core:
    - `PublishNew(ctx, node)` — the full-mode call site. Inserts the
      `EmbeddingPublish` row (one row, with freshly minted `publish_id`
      and `point_id = uuid_v5(publish_id)`, stamping
      `embedding_model_version`, `target_id = node.id`,
      `target_kind = 'node'`), then calls the inner core with
      `attempt_index = 1`. **Happy path row count = 4**: 1
      `EmbeddingPublish` + 3 `EmbeddingPublishEvent` rows
      (`queued`, `vector_written`, `published`).
    - `RetryExisting(ctx, publish)` — the flusher call site. Reuses the
      input row's `publish_id` + `point_id` + `target_id` +
      `embedding_model_version`; reads `max(attempt_index)` from
      `EmbeddingPublishEvent` for that `publish_id`; calls the inner
      core with `attempt_index = max+1`. **Never** inserts a new
      `EmbeddingPublish`. **Happy path row count = 3**: 3
      `EmbeddingPublishEvent` rows only.
    - Inner core `publishCore(publish_id, point_id, attempt_index)`
      runs inside a single PostgreSQL transaction that first takes
      `pg_advisory_xact_lock(hashtext(publish_id::text))` (race-safety,
      see §"Race-safety on retry" above), then inserts `queued` →
      embeds → Qdrant `UpsertPoint` (idempotent on `point_id`) →
      inserts `vector_written` (or `failed` with the error in
      `details_json` and returns) → Qdrant `FetchPoint`
      read-after-write check → inserts `published`. If `FetchPoint`
      fails or the process crashes after `vector_written`, the row is
      picked up by the flusher's `vector_written` staleness predicate
      (step-8) and re-driven at `attempt_index+1`.

  Unit test against fake adapters asserts: (a) `PublishNew` emits
  exactly 4 INSERTs in order on the happy path; (b) `RetryExisting`
  emits exactly 3 INSERTs with the correct incremented `attempt_index`
  and **zero** new `EmbeddingPublish` rows; (c) on Qdrant upsert
  failure, the failed-path INSERTs are
  `EmbeddingPublish (PublishNew only) + queued + failed` and nothing
  else; (d) **no** UPDATE or DELETE statement is issued in any path;
  (e) two concurrent `RetryExisting` calls for the same `publish_id`
  do not produce duplicate `attempt_index` values (the second caller
  blocks on the advisory lock, then reads the now-incremented max).
- **step-6-model-version-stamping** (`expectedFileChanges: 3`) — wire
  `EmbeddingModel.Version()` into every `EmbeddingPublish` insert and
  add a config flag `AM_EMBEDDING_MODEL_VERSION` that overrides the
  client default. Publisher refuses to start if the configured version
  is empty. Unit test asserts every inserted row carries the stamped
  version.
- **step-7-full-mode-handler-call-site** (`expectedFileChanges: 4`) —
  modify `internal/repoindexer/worker.go` (or the existing full-mode
  handler hook) to invoke `publisher.PublishNew(ctx, node)` for every
  Method / Block Node emitted by the AST dispatcher, after the
  GraphWriter transaction commits. Failures must not fail the ingest
  job — they leave the latest event at `'failed'` and rely on the
  flusher. Integration test asserts a successful full ingest of a
  3-method fixture yields three `'published'` events, each with
  `attempt_index = 1`.
- **step-8-background-flusher** (`expectedFileChanges: 5`) —
  implement `flusher.go` as a worker goroutine started by the
  `agent-memory` server. Polls **`EmbeddingPublish` rows** whose
  latest `EmbeddingPublishEvent` is (a) `'queued'` or `'failed'` past
  the standard backoff, **or** (b) `'vector_written'` whose
  `created_at` is older than `AM_PUBLISH_RAW_STALENESS` (proposed
  60 s) — see §"Stale `vector_written`" above for why this third
  state must be flushed too. LATERAL JOIN on the
  `(publish_id, created_at DESC)` index from §8.7.2. The flusher then
  calls `publisher.RetryExisting(ctx, publish)` with each such row.
  The flusher **passes the existing `EmbeddingPublish` row**, never a
  `Node`, so `publish_id` + `point_id` + `target_id` +
  `embedding_model_version` are reused. Each new attempt appends an
  event chain with `attempt_index = max(prior) + 1`, never an UPDATE,
  never a duplicate `EmbeddingPublish`. Integration test uses a fake
  Qdrant that fails once then succeeds and asserts the row reaches
  `'published'` with `attempt_index = 2`, and that the count of
  `EmbeddingPublish` rows for that target is still 1. A second
  integration test exercises the `vector_written` staleness path (see
  step-11 Scenario E).

### Stage 3: Read-side filter and verification

- **step-9-graphreader-published-filter** (`expectedFileChanges: 5`) —
  modify `internal/graphreader/` to filter Qdrant hits through the
  §9.6a "latest event = `'published'`" join **and**
  `EmbeddingPublish.embedding_model_version = <active>` predicate.
  The join key is the **`publish_id` carried in the Qdrant payload**
  (single SQL lookup into `EmbeddingPublish`, then the
  `(publish_id, created_at DESC)` index for the latest event).
  **Payload cross-check:** after the join, the reader must reject the
  hit (and increment `recall_filter_unpublished_total` with a
  `reason="payload_mismatch"` label) if the Qdrant payload's
  `target_id`, `target_kind`, or `embedding_model_version` disagree
  with the joined `EmbeddingPublish` row — this catches stale or
  corrupt Qdrant points whose `publish_id` still resolves but whose
  payload no longer matches the source of truth. Otherwise
  `target_id` + `target_kind` from the payload provide the dereference
  back to the Node / ConceptVersion row for the result set. Filtered
  hits are replaced from the next-best Qdrant candidate via overfetch
  (start with `k * 2`, expand on demand) **until `k` results are
  reached or candidates are exhausted** (e2e-scenarios.md L461) — the
  reader does not loop forever if Qdrant simply doesn't have `k`
  published active-version vectors.
- **step-10-metrics-and-degraded-flag** (`expectedFileChanges: 4`) —
  add OTel counters / gauges: `embedding_publish_total{event_kind}`,
  `embedding_publish_latency_seconds` (histogram),
  `embedding_publish_backlog` (gauge of `EmbeddingPublish` rows whose
  latest event is `'queued'` / `'failed'`), and
  `recall_filter_unpublished_total` (the filter increment from
  e2e-scenarios.md L460). Per C22, the **recall verb's response**
  (not `/health`) is what carries
  `degraded=true, degraded_reason='embedding_index_unavailable'`,
  and only when the Qdrant cosine query itself fails — *not* when the
  published-filter merely drops some hits. `/health` carries the
  backlog gauge and the active `embedding_model_version` as
  operational observability; it is **not** a C22 carrier.
- **step-11-9_6a-integration-test** (`expectedFileChanges: 4`) —
  add `publisher_integration_test.go` against the
  `deploy/local/docker-compose.yml` stack (PostgreSQL + Qdrant). Six
  test cases mirror the work-item scenarios and lock in the identity,
  liveness, and concurrency contracts:
    (a) **Publish state log is complete** — after `PublishNew`, the
    log for the target contains exactly one `EmbeddingPublish` and
    exactly one each of `queued`, `vector_written`, `published` events
    in order, all at `attempt_index = 1`.
    (b) **Transient Qdrant error retries cleanly** — fake Qdrant fails
    once. After the flusher runs, the log for the same target still
    has exactly **one** `EmbeddingPublish` row (no duplicate), and the
    events are `queued@1, failed@1, queued@2, vector_written@2,
    published@2`. The test also **captures every `UpsertPoint` call**
    and asserts both calls use the **same deterministic `point_id`
    and the same payload tuple** (`publish_id`, `target_id`,
    `target_kind`, `embedding_model_version`), proving point identity
    is reused — not just the parent `EmbeddingPublish` row.
    (c) **Unpublished hit is filtered** — a target whose latest event
    is `queued` is excluded from `agent.recall` results and increments
    `recall_filter_unpublished_total`; the response is **not**
    `degraded` (filter drops are normal flow, not C22).
    (d) **Recall-time Qdrant outage surfaces C22** — when the Qdrant
    cosine query itself fails, `agent.recall` returns
    `degraded=true, degraded_reason='embedding_index_unavailable'`
    (per C22, on the **verb response**, not on the writer).
    (e) **Stale `vector_written` is recovered, not stuck** — fake
    Qdrant returns success on `UpsertPoint` but errors on `FetchPoint`
    once. The publisher leaves the row at `vector_written@1`. After
    the staleness window passes, the flusher re-drives the row to
    `published@2` (one `EmbeddingPublish`, events `queued@1,
    vector_written@1, queued@2, vector_written@2, published@2`).
    Proves the `vector_written` predicate (step-8) and the
    deterministic-`point_id` idempotency together close the §9.6a
    liveness gap.
    (f) **Concurrent flusher replicas do not race** — two
    `RetryExisting` goroutines invoked simultaneously on the same
    `publish_id` produce exactly one `queued@2 / vector_written@2 /
    published@2` chain (the loser blocks on the advisory lock, then
    sees the now-incremented `max(attempt_index)` and either no-ops or
    appends `queued@3` cleanly — never collides on `attempt_index = 2`).
    Proves the `pg_advisory_xact_lock` serialisation contract.

## Out of scope

- **Concept embedding publication.** Tech-spec §9.6a applies the same
  protocol to `ConceptVersion` rows written by the Concept Promoter,
  but that writer is built in a later phase (Concept promotion). The
  publisher / log helpers in Stage 1 are designed to be reused by it
  — `PublishTarget` accepts either `node_id` or
  `concept_version_id` — but wiring the Promoter is not part of this
  stage.
- **Embedding-model upgrade supersede flow.** Risk §9.6 mandates a
  bulk re-embed when `embedding_model_version` is bumped: for every
  affected target, mint a *new* `EmbeddingPublish` at the active
  version (driven through `PublishNew`) and append a `superseded`
  event on the prior `publish_id`. The append-only log and the
  reader's active-version predicate already support this end-to-end,
  but the **driver** (mgmt verb + worker that walks the affected
  targets) and the `superseded`-emitting helper are owned by a later
  story. `RetryExisting` in this stage explicitly **does not** decide
  to supersede — it always reuses the stored `embedding_model_version`
  and lets the reader's filter drop the row if it no longer matches
  active. This keeps the retry path duplication-free and pushes the
  policy decision ("when do we re-embed?") to the operator-facing
  upgrade driver.
- **Bulk re-embed driver for model upgrades.** Same boundary as the
  bullet above — this is the `mgmt.snapshot`-driven background job
  that issues the `PublishNew` + `superseded` pairs. Out of scope
  here.
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

## Prior feedback resolution (iteration 3)

This section addresses each numbered finding from iteration 2's
`## What still needs work`:

1. **ADDRESSED — "5 INSERTs" insert-count bug.** Plan §"step-5" and
   work-items.yaml `step-publisher-state-machine` now state the
   correct counts: `PublishNew` happy path = **4 INSERTs** (1
   `EmbeddingPublish` + 3 `EmbeddingPublishEvent` rows: `queued`,
   `vector_written`, `published`); `RetryExisting` happy path = **3
   INSERTs** (events only); Qdrant-failure path = `EmbeddingPublish
   (PublishNew only) + queued + failed` and nothing else. Step-11
   Scenario A asserts the exact 4-row shape.
2. **ADDRESSED — retry/flusher identity ambiguity.** The publisher
   now exposes **two narrow entry points** with explicit identity
   semantics (plan §"Approach" item 3 and §"Identity contract" table):
   `PublishNew(ctx, node)` mints a fresh `publish_id` + deterministic
   `point_id`; `RetryExisting(ctx, publish)` reuses `publish_id` +
   `point_id` + `target_id` + `embedding_model_version` and only
   appends an event chain with `attempt_index = max(prior) + 1`. The
   flusher (step-8) is rewritten to call `RetryExisting` with the
   existing `EmbeddingPublish` row, never a `Node`. Step-11
   Scenario B asserts that after a retry the
   `EmbeddingPublish` row count for the target is still **1** and the
   event chain is `queued@1, failed@1, queued@2, vector_written@2,
   published@2`.
3. **ADDRESSED — Qdrant payload identity for dereferencing hits.**
   Plan §"Approach" item 1 and `step-qdrant-adapter` now specify the
   payload carries `publish_id` (the single SQL lookup key into
   `EmbeddingPublish`, per tech-spec line 569's join contract),
   `target_id`, `target_kind ∈ {'node','concept_version'}`, `repo_id`,
   `kind`, and `embedding_model_version`. The adapter unit test
   asserts every field is present and that `point_id ==
   uuid_v5(publish_id)`. `step-graphreader-published-filter` joins on
   the payload `publish_id`, not on `(repo_id, kind)` alone.
4. **ADDRESSED — C22 degraded-handling location.** Plan §"Constraints"
   re-anchors C22 as an Agent/Management **verb response field**
   (tech-spec L414). `step-metrics-and-degraded-flag` now sets it on
   the **recall verb response** (not `/health`, not the writer), and
   only when the Qdrant cosine query itself fails — *not* when the
   published-filter merely drops some hits. `/health` keeps only an
   operational backlog gauge + active model version. Step-11 Scenario
   D was added to lock in the recall-time C22 behaviour; Scenario C
   explicitly asserts that filter drops are **not** `degraded`.

### Additional gaps closed in iteration 3 (rubber-duck pass)

These are not from iteration 2's checklist but were caught by an
independent design critique before push. Calling them out so the
grader can see the surface area:

- **`vector_written` was a dead state.** The original iter-3 flusher
  only retried `queued`/`failed`; a process crash (or `FetchPoint`
  failure) between `vector_written` and `published` would have left
  the row invisible to recall forever. Step-8's eligibility predicate
  now includes `vector_written` past `AM_PUBLISH_RAW_STALENESS`, and
  the deterministic `point_id = uuid_v5(publish_id)` makes the
  re-`UpsertPoint` idempotent in Qdrant. Step-11 Scenario E exercises
  this recovery path.
- **`attempt_index` increment was racy.** Two flusher replicas could
  both `SELECT max(attempt_index)` and append at `N+1`. `RetryExisting`
  now wraps the read/write in a transaction that takes
  `pg_advisory_xact_lock(hashtext(publish_id::text))`. Step-11
  Scenario F asserts concurrent retries never produce a duplicate
  `attempt_index`.
- **Model-version row in the identity-contract table was
  self-contradictory.** It said retry "creates a new `EmbeddingPublish`
  at the active version" while the rest of the plan said retry NEVER
  inserts a new row. The row is rewritten: retry always reuses the
  stored version; the reader filter drops the hit if it is no longer
  active; the supersede flow is moved fully into Out of scope.
- **Step-11 Scenario B did not actually prove `point_id` reuse.** It
  is now strengthened to capture every `UpsertPoint` call and assert
  the deterministic `point_id` + full payload tuple is identical
  across the original attempt and the retry — not just that the
  parent `EmbeddingPublish` count stayed at 1.
- **Reader payload cross-check was implicit.** Step-9 now requires the
  reader to reject Qdrant hits whose payload `target_id`,
  `target_kind`, or `embedding_model_version` disagree with the joined
  `EmbeddingPublish` row (incrementing the filter counter with a
  `reason="payload_mismatch"` label) — catches stale/corrupt Qdrant
  points whose `publish_id` still resolves.
- **`len == k` was absolute.** Step-9 now says overfetch backfills
  "until `k` results are reached **or** candidates are exhausted",
  not an unbounded loop.
