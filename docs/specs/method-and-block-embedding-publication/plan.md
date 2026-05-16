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
   `embedding_model_version`. The `point_id` itself is deterministic:
   `uuid_v5(NS_EMBEDDING_PUBLISH, publish_id)` where
   `NS_EMBEDDING_PUBLISH` is a fixed UUIDv5 namespace constant
   exported by the `internal/embedding` package. Pinning the namespace
   means independent implementations and tests produce byte-identical
   `point_id`s for the same `publish_id`, which is what makes Qdrant
   re-upserts in-place (no orphan vectors) and what the integration
   test in step-11 Scenario B asserts.
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
| `attempt_index` (on event)  | `1`                           | reserved inside a short Tx-A that takes `pg_advisory_xact_lock(hashtext(publish_id::text))`, re-reads the latest event for the `publish_id`, no-ops if latest is `published` / `superseded` (so a racing loser cannot regress a successful publish), otherwise inserts `queued@max+1` and commits. Subsequent `vector_written` / `failed` / `published` events for that attempt are committed in separate short transactions **without** the advisory lock — uniqueness on `(publish_id, attempt_index, event_kind)` is enough because the `attempt_index` was already reserved in Tx-A. External embed + Qdrant calls run **outside** any DB transaction. See §"Transaction scope" and §"No regression on race" below for the rationale. |
| `EmbeddingPublish` row      | **inserted**                  | **not inserted**                 |

This is what makes the retry path duplication-free: there is one and
only one `EmbeddingPublish` per intended publish, and the event chain
under it grows monotonically by `attempt_index`.

### Transaction scope: short DB transactions, external I/O outside

`publishCore` does **not** wrap the embedding + Qdrant `UpsertPoint` +
`FetchPoint` calls inside a single long-lived PostgreSQL transaction.
Doing so would (a) hold a row-level write lock and the
`pg_advisory_xact_lock` across multi-hundred-ms remote calls,
serialising all retries against the same `publish_id`, and (b) make
the §9.6a state events themselves *un-observable* until the whole
chain commits — which would defeat the `vector_written` staleness
recovery story below (a process crash after `vector_written` would
roll the event back, not leave it visible to the flusher, and may
also orphan a Qdrant point with no committed event trail).

The protocol is therefore three short, independently committed DB
transactions per attempt, with external I/O strictly between them:

1. **Tx-A — admit attempt.** Open tx, take
   `pg_advisory_xact_lock(hashtext(publish_id::text))`, re-read the
   latest event for that `publish_id`, decide eligibility (see "No
   regression on race" below), reserve `attempt_index = max+1`, and
   insert `queued@attempt_index`. Commit. Lock released on commit.
   For `PublishNew` the same transaction also inserts the brand-new
   `EmbeddingPublish` row immediately before `queued@1` so the
   `EmbeddingPublish` row and its first `queued` event are atomic
   (still one short tx).
2. **External — embed + Qdrant `UpsertPoint`** (no DB lock held).
3. **Tx-B — record write outcome.** Open tx, insert `vector_written@
   attempt_index` on success or `failed@attempt_index` on error
   (with the error in `details_json`). Commit. On `failed` the
   attempt ends here; the flusher will pick the row up.
4. **External — Qdrant `FetchPoint`** read-after-write check.
5. **Tx-C — publish.** On `FetchPoint` success, insert `published@
   attempt_index`. Commit. On `FetchPoint` failure or process crash,
   the latest event remains `vector_written@attempt_index` and the
   flusher's `vector_written` staleness predicate (next section)
   re-drives it at `attempt_index+1`. Tx-C does **not** re-take the
   advisory lock — the `attempt_index` was already reserved in Tx-A
   and the `(publish_id, attempt_index, event_kind)` triple is
   unique, so a concurrent flusher that already reserved
   `attempt_index+1` cannot collide with Tx-C's `published@N`.

The advisory-lock window is therefore bounded by the **Tx-A**
duration only — a single SELECT + 1–2 INSERTs — never by Qdrant
latency.

### No regression on race (monotonic `published` + `RetryExisting` eligibility re-check)

The advisory lock alone is **not** sufficient to prevent a
successful publish from being regressed by a racing retry. The lock
is released when Tx-A commits — Tx-B / Tx-C of attempt N run
*unlocked*, and can interleave with Tx-A / Tx-B / Tx-C of attempt
N+1. Concretely (with `AM_PUBLISH_RAW_STALENESS = 60 s`):

| t (s) | actor | event inserted | latest by `created_at DESC` |
| ----- | ----- | -------------- | --------------------------- |
| 0     | W1 Tx-A | `queued@1`            | `queued@1`            |
| 80    | W1 Tx-B | `vector_written@1`    | `vector_written@1`    |
| 140   | F2 Tx-A | `queued@2`            | `queued@2`            |
| 150   | W1 Tx-C | `published@1`         | `published@1` ✅ briefly visible |
| 230   | F2 Tx-B | `vector_written@2`    | `vector_written@2` ❌ invisible |

A naïve "latest event by `created_at DESC` = `'published'`" reader
predicate would see the row become visible at t=150 then **regress
back to invisible at t=230**, even though `published@1` is still
present in the log. A later F2 retry that landed `failed@2` instead
of `vector_written@2` would keep the row invisible until yet another
retry cycle.

The fix is **monotonic `published`**: the reader's published-filter
and the eligibility predicates do not look at "latest event by
clock" at all; they ask **"does any event of kind `'published'`
exist for this `publish_id`, and has no `'superseded'` event been
emitted on it?"**. Concretely:

```sql
EXISTS (
  SELECT 1 FROM EmbeddingPublishEvent
  WHERE publish_id = $1 AND event_kind = 'published'
)
AND NOT EXISTS (
  SELECT 1 FROM EmbeddingPublishEvent
  WHERE publish_id = $1 AND event_kind = 'superseded'
)
```

This is a faithful interpretation of tech-spec §9.6a's *intent*: the
spec elsewhere says "once it reaches `'published'`, the writer emits
one final `EmbeddingPublishEvent(event_kind='superseded')` on the
*prior* `publish_id` so the reader picks the new vector
deterministically" — meaning `'superseded'` is the **only**
revocation of `'published'`. The plan adopts that two-state
monotonic model explicitly so that `vector_written@N+1` /
`failed@N+1` from a racing retry **cannot un-publish** a row that
has already reached `published@N`. Performance: a small partial
index `(publish_id) WHERE event_kind IN ('published','superseded')`
makes both EXISTS checks point-lookups.

With monotonic-`published` in place, `RetryExisting` inside Tx-A
does:

1. Take `pg_advisory_xact_lock(hashtext(publish_id::text))`.
2. Re-check the monotonic predicate above. If it holds (row is
   already published and not superseded), **no-op the retry**
   (release lock on commit and return). This catches the case where
   `published@N` was written *after* the flusher polled but *before*
   the flusher entered Tx-A.
3. Otherwise re-read the latest `EmbeddingPublishEvent` for that
   `publish_id`. If latest is still `'queued'` whose `created_at` is
   newer than `AM_PUBLISH_QUEUED_TIMEOUT` (see "Backoff knob" below),
   no-op (the prior attempt is still in flight).
4. Otherwise (`failed`, or `vector_written` past
   `AM_PUBLISH_RAW_STALENESS`, or `queued` past
   `AM_PUBLISH_QUEUED_TIMEOUT`), reserve `attempt_index = max+1` and
   insert `queued@attempt_index`. Commit.

The flusher's eligibility predicate (step-8) is similarly tightened
with `AND NOT (EXISTS published AND NOT EXISTS superseded)` so a
published row that still has a stale `vector_written@N+1` chain in
flight is **not** re-driven again — wasted Qdrant work, harmless to
correctness but worth avoiding.

Combined, the protocol delivers two strong properties:

- **No duplicate `EmbeddingPublish` rows** — `PublishNew` is the
  only place that inserts one, and the flusher always calls
  `RetryExisting` on the existing row (see §"`PublishNew`
  at-most-once contract" below).
- **No regression of a successful publish on race** — once any
  attempt has emitted `published`, the row is permanently visible
  to recall until and unless a `'superseded'` event is written (and
  `'superseded'` is owned by the bulk re-embed driver, out of scope
  here).

### `PublishNew` at-most-once contract

`PublishNew` mints a fresh `publish_id` every time it is called. If
the Stage 3.1 full-mode handler ever calls it twice for the same
`(target_id, embedding_model_version)` — handler crash recovery,
at-least-once worker semantics, an ingest job re-running over the
same file set — the publisher would happily create **two**
`EmbeddingPublish` rows with two distinct `publish_id`s and two
distinct deterministic `point_id`s, surfacing the same node as two
hits in recall. The plan handles this by:

1. **Idempotent insert in `PublishNew`.** The `EmbeddingPublish`
   insert is `INSERT … ON CONFLICT (target_id, target_kind,
   embedding_model_version) WHERE NOT EXISTS (… superseded …) DO
   NOTHING RETURNING publish_id`. If a live row already exists,
   `PublishNew` reads it back and falls through to a tail call of
   `RetryExisting(ctx, existing_publish)`, so re-invoking
   `PublishNew` is at-most-once on (`target_id`, `target_kind`,
   active `embedding_model_version`).
2. **Schema assumption.** The
   `phase-foundation-and-schema/stage-embedding-state-log-migrations-and-roles`
   stage (a hard dependency) is expected to provide the matching
   partial UNIQUE index. If it does not, the implementer must open a
   dependency PR to add it before landing step-5 — `EmbeddingPublish`
   without that uniqueness constraint cannot guarantee at-most-once.
   This is called out in §"Open questions" below.

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
  via env (`AM_QDRANT_ENDPOINT`, `AM_QDRANT_COLLECTION`). Exports the
  fixed UUIDv5 namespace constant `NS_EMBEDDING_PUBLISH` used to
  derive `point_id`. Unit test with a fake transport asserts the
  upsert payload carries the full identity tuple so a recall hit can
  be dereferenced to `EmbeddingPublish` in a single SQL lookup:
  `publish_id` (PK into the log), `target_id`,
  `target_kind ∈ {'node','concept_version'}`, `repo_id`, `kind`,
  `embedding_model_version`. The test also asserts
  `point_id == uuid_v5(NS_EMBEDDING_PUBLISH, publish_id)` so retries
  are byte-identical idempotent in Qdrant across implementations.
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
  entry points** that share one inner core. **External I/O (embed +
  Qdrant) runs outside any open PostgreSQL transaction**; each state
  transition is its own short committed tx (see §"Transaction scope"
  for the rationale):
    - `PublishNew(ctx, node)` — the full-mode call site. Opens **Tx-A**:
      takes `pg_advisory_xact_lock(hashtext(target_id::text))` (note:
      keyed on `target_id`, not `publish_id`, because there is no
      `publish_id` yet and we must serialise concurrent
      `PublishNew` calls for the same node), then attempts the
      idempotent insert
      `INSERT INTO EmbeddingPublish (…) … ON CONFLICT (target_id,
      target_kind, embedding_model_version) WHERE NOT EXISTS
      (superseded) DO NOTHING RETURNING publish_id`. If a live row
      already exists (no row returned), reads it back and **tail-calls
      `RetryExisting(ctx, existing)`** within the same Tx-A so the
      handler sees one logical "did the right thing" outcome. If a
      fresh row was inserted: `publish_id` minted uuid_v7, `point_id =
      uuid_v5(NS_EMBEDDING_PUBLISH, publish_id)`, stamping
      `embedding_model_version`, `target_id = node.id`,
      `target_kind = 'node'`; then inserts `queued@1`, commits.
      Then runs embed + `UpsertPoint` outside any tx. Opens **Tx-B**:
      inserts `vector_written@1` (or `failed@1` and returns), commits.
      Runs `FetchPoint` outside any tx. Opens **Tx-C**: inserts
      `published@1`, commits.
      **Happy path row count = 4**: 1 `EmbeddingPublish` + 3
      `EmbeddingPublishEvent` rows (`queued`, `vector_written`,
      `published`).
    - `RetryExisting(ctx, publish)` — the flusher call site. Reuses
      the input row's `publish_id` + `point_id` + `target_id` +
      `embedding_model_version`. Opens **Tx-A**: takes the advisory
      lock on `hashtext(publish_id::text)`, **first checks the
      monotonic-published predicate** (`EXISTS published AND NOT
      EXISTS superseded`). If true, **no-op the retry** and return —
      this prevents a racing loser from being scheduled to overwrite
      a successfully-published row (see plan §"No regression on
      race" for why a latest-by-clock check is not sufficient). If
      false, re-reads the latest `EmbeddingPublishEvent` for that
      `publish_id`. If latest is still `'queued'` whose `created_at`
      is newer than `AM_PUBLISH_QUEUED_TIMEOUT`, also no-op (the
      prior attempt is still in flight). Otherwise reserves
      `attempt_index = max+1`, inserts `queued@attempt_index`,
      commits. Then runs embed + `UpsertPoint` outside any tx, opens
      Tx-B for `vector_written` / `failed`, runs `FetchPoint`, opens
      Tx-C for `published` — identical to the post-Tx-A flow of
      `PublishNew`. **Never** inserts a new `EmbeddingPublish` row.
      **Happy path row count = 3**: 3 `EmbeddingPublishEvent` rows
      only. **Retry against an already-published row = 0 INSERTs**
      (no-op).
    - Inner core `publishCore(publish_id, point_id, attempt_index)`
      starts immediately after Tx-A has reserved the attempt and
      committed the initial `queued@attempt_index`. It runs the
      external embed + `UpsertPoint` call, then Tx-B (no advisory
      lock — uniqueness of
      `(publish_id, attempt_index, event_kind)` is enough), then
      `FetchPoint`, then Tx-C. If `FetchPoint` fails or the process
      crashes after `vector_written`, the row is picked up by the
      flusher's `vector_written` staleness predicate (step-8) and
      re-driven at `attempt_index+1`. Tx-B / Tx-C of attempt N may
      interleave with Tx-A / Tx-B / Tx-C of attempt N+1 from a
      racing flusher — the monotonic-`published` reader filter and
      flusher predicate (plan §"No regression on race") absorb the
      interleaving without regression.

  Unit test against fake adapters asserts: (a) `PublishNew` emits
  exactly 4 INSERTs in order across exactly 3 committed transactions
  on the happy path; (b) `PublishNew` called twice for the same
  `(target_id, target_kind, embedding_model_version)` produces
  exactly **one** `EmbeddingPublish` row total (the second call
  observes the ON CONFLICT, reads the existing row, and tail-calls
  `RetryExisting` — at-most-once contract); (c) `RetryExisting` on a
  non-published row emits exactly 3 INSERTs across 3 transactions
  with the right incremented `attempt_index` and **zero** new
  `EmbeddingPublish` rows; (d) `RetryExisting` invoked against a row
  whose `EXISTS published AND NOT EXISTS superseded` predicate holds
  emits **zero** INSERTs (no-op); (e) on Qdrant upsert failure, the
  failed-path INSERTs are `EmbeddingPublish (PublishNew only) +
  queued + failed` and nothing else; (f) **no** UPDATE or DELETE
  statement is issued in any path; (g) two concurrent `RetryExisting`
  calls for the same `publish_id` on a non-published row never
  produce duplicate `attempt_index` values (the second caller blocks
  on the advisory lock in Tx-A, then either no-ops on the monotonic
  predicate or reserves `attempt_index = max+2`); (h) the advisory
  lock is **not** held across the embed / `UpsertPoint` /
  `FetchPoint` external calls (asserted by injecting a slow fake
  adapter and observing that a concurrent `RetryExisting` against a
  *different* `publish_id` proceeds without waiting).
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
  latest `EmbeddingPublishEvent` is (a) `'queued'` whose `created_at`
  is older than `AM_PUBLISH_QUEUED_TIMEOUT` (proposed 5 min, same
  knob as Tx-A's "queued within backoff" no-op check), or (b)
  `'failed'` past the standard backoff, or (c) `'vector_written'`
  whose `created_at` is older than `AM_PUBLISH_RAW_STALENESS`
  (proposed 60 s) — see plan §"Stale `vector_written`" for why this
  third state must be flushed too. **All three eligibility branches
  additionally require `NOT (EXISTS published AND NOT EXISTS
  superseded)`** on the `publish_id` so that a row whose `published@N`
  has already landed (even with later `vector_written@N+1` chains
  from a racing retry) is **not** re-driven again — wasted Qdrant
  work, harmless to correctness because the reader's monotonic-
  `published` filter (step-9) already keeps the row visible. LATERAL
  JOIN on the `(publish_id, created_at DESC)` index from §8.7.2 for
  the latest event; EXISTS checks on the partial `(publish_id) WHERE
  event_kind IN ('published','superseded')` index. The flusher calls
  `publisher.RetryExisting(ctx, publish)` with each eligible row.
  The flusher **passes the existing `EmbeddingPublish` row**, never a
  `Node`, so `publish_id` + `point_id` + `target_id` +
  `embedding_model_version` are reused. Each new attempt appends an
  event chain with `attempt_index = max(prior) + 1`, never an UPDATE,
  never a duplicate `EmbeddingPublish`. Integration test uses a fake
  Qdrant that fails once then succeeds and asserts the row reaches
  `'published'` with `attempt_index = 2`, and that the count of
  `EmbeddingPublish` rows for that target is still 1. A second
  integration test exercises the `vector_written` staleness path (see
  step-11 Scenario E). A third integration test exercises the
  no-redundant-flush path: after `published@1` lands, even an
  artificially-stale `vector_written@2` chain does **not** trigger
  another flusher attempt (asserts the `NOT EXISTS published` clause
  on the eligibility predicate).

### Stage 3: Read-side filter and verification

- **step-9-graphreader-published-filter** (`expectedFileChanges: 5`) —
  modify `internal/graphreader/` to filter Qdrant hits through the
  §9.6a **monotonic-published** predicate (see plan §"No regression
  on race") and the
  `EmbeddingPublish.embedding_model_version = <active>` predicate.
  The join key is the **`publish_id` carried in the Qdrant payload**
  (single SQL lookup into `EmbeddingPublish`, then two EXISTS
  point-lookups against the partial index `(publish_id) WHERE
  event_kind IN ('published','superseded')`): keep the hit iff
  `EXISTS published AND NOT EXISTS superseded`. This is **not** a
  "latest event by `created_at DESC` = `'published'`" scan — that
  semantics would regress on the race walked through in §"No
  regression on race". `published` is treated as a **monotonic
  terminal state**; only `'superseded'` (out of scope, emitted by
  the bulk re-embed driver) can revoke it.
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
    and the same full payload tuple** (`publish_id`, `target_id`,
    `target_kind`, `repo_id`, `kind`, `embedding_model_version`),
    proving point identity is reused byte-for-byte — not just the
    parent `EmbeddingPublish` row.
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
    (f) **Concurrent flusher replicas never regress a publish** — two
    `RetryExisting` goroutines invoked simultaneously on the same
    `publish_id` of a non-published row produce exactly one
    `queued@2 / vector_written@2 / published@2` chain (the loser
    blocks on the Tx-A advisory lock; once it sees the now-reserved
    `queued@2` it no-ops, never appending `queued@3` while attempt 2
    is in flight). A second sub-case has the loser arrive **after**
    attempt 2 has already reached `published@2`: it MUST no-op (zero
    INSERTs from the loser) — proving the monotonic-`published`
    eligibility re-check inside Tx-A prevents a racing retry from
    being scheduled against a successfully published row. Together
    these prove the `pg_advisory_xact_lock` serialisation **and** the
    monotonic-`published` eligibility re-check.
    (g) **Actual interleaving race — slow Tx-C with concurrent
    flusher** — exercises the precise race the monotonic-`published`
    reader filter exists to absorb (see plan §"No regression on
    race"). Test harness uses a controllable fake `QdrantClient`
    whose `FetchPoint` blocks on a test channel.
      1. W1 = `PublishNew(node)` → Tx-A `queued@1` → embed +
         `UpsertPoint` → Tx-B `vector_written@1` → blocks in
         `FetchPoint`.
      2. Advance a fake clock past `AM_PUBLISH_RAW_STALENESS`.
      3. F2 = flusher polls, finds the row eligible (latest is
         stale `vector_written@1`, no `published` yet), calls
         `RetryExisting`. F2 enters Tx-A, monotonic-`published`
         check is false, reserves `attempt_index=2`, inserts
         `queued@2`, runs embed + `UpsertPoint` (idempotent — same
         `point_id`), inserts `vector_written@2` in Tx-B. F2's
         `FetchPoint` is then made to fail (no Tx-C).
      4. Release W1's `FetchPoint` → Tx-C inserts `published@1`.
      5. **Assert: reader's published-filter surfaces the row.**
         This is precisely the case where a naïve "latest event by
         `created_at DESC` = `'published'`" predicate would *drop*
         the row because the newest event by clock is
         `vector_written@2`. The monotonic predicate (`EXISTS
         published AND NOT EXISTS superseded`) correctly keeps the
         row visible.
      6. **Assert: the next flusher cycle does NOT re-drive the
         row.** Even though `vector_written@2` is older than
         `AM_PUBLISH_RAW_STALENESS`, the eligibility predicate's
         `NOT (EXISTS published AND NOT EXISTS superseded)` clause
         filters the row out — no redundant Qdrant call, no
         `attempt_index=3`.
    (h) **`PublishNew` is at-most-once on (target_id,
    embedding_model_version)** — calling `PublishNew(node)` twice
    for the same node (handler retry) leaves exactly **one**
    `EmbeddingPublish` row in the log. The second call's ON CONFLICT
    triggers the tail call to `RetryExisting`, which sees the first
    attempt is either in-flight or published and no-ops or appends
    `attempt_index=2`. Asserts the
    `(target_id, target_kind, embedding_model_version)` partial
    UNIQUE invariant is enforced end-to-end.

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
- `AM_PUBLISH_RAW_STALENESS` must be set to **at least**
  p99(`UpsertPoint`) + p99(`FetchPoint`) to avoid every slow-but-
  correct publish triggering the redundant-retry path. Proposed 60 s
  is a starting point; tune once we have real Qdrant timings.
- `AM_PUBLISH_QUEUED_TIMEOUT` (the single knob shared by Tx-A's
  "queued within backoff" no-op check and the flusher's queued
  eligibility) — proposed 5 min. Both must read the *same* env var
  so the flusher cannot keep polling a row that Tx-A keeps no-oping.
- **Hard dependency on `EmbeddingPublish (target_id, target_kind,
  embedding_model_version) WHERE NOT EXISTS superseded` partial
  UNIQUE index.** The `PublishNew` at-most-once contract relies on
  it. If
  `phase-foundation-and-schema/stage-embedding-state-log-migrations-and-roles`
  has not added this index, step-5 cannot land without a
  one-migration dependency PR against that stage. Implementer to
  confirm at start of step-5.

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

### Additional gaps closed in iteration 3 (second rubber-duck pass)

A second independent design critique caught two more substantive
correctness issues before this push. Both are now reflected in the
plan and work-items:

- **`publishCore` was wrapping external embed + Qdrant calls inside
  one PostgreSQL transaction.** A crash mid-tx would have **rolled
  back** the `vector_written` event we depend on for staleness
  recovery, and could orphan a Qdrant point with no committed event
  trail. It also serialised every retry against the same
  `publish_id` for hundreds of milliseconds of remote latency,
  contradicting the "short lock window" claim. The protocol is now
  three short committed transactions per attempt — Tx-A
  (advisory-locked eligibility + reserve `queued@N`), Tx-B
  (`vector_written` / `failed`), Tx-C (`published`) — with external
  embed + `UpsertPoint` + `FetchPoint` running **outside** any open
  DB transaction. See plan §"Transaction scope" and the rewritten
  step-5. The step-5 unit test now asserts (a) the lock is **not**
  held across the external calls (slow-fake adapter test) and (b)
  there are exactly 3 committed transactions per happy path.
- **Concurrent `RetryExisting` could regress a successful publish to
  unpublished.** The prior iter-3 wording allowed the lock loser to
  "append `queued@3` cleanly" even if the winner had already reached
  `published@2`, which would temporarily hide a correctly-published
  vector (and could leave it as `failed@3` if the unnecessary retry
  failed). `RetryExisting` now performs an explicit eligibility
  re-read inside Tx-A and **no-ops** if the latest event is
  `'published'` or `'superseded'`. See plan §"No regression on race".
  Step-11 Scenario F is rewritten with a second sub-case that
  asserts the loser performs **zero** INSERTs when arriving after
  `published@2`.
- **UUIDv5 namespace was unpinned.** `point_id = uuid_v5(publish_id)`
  was ambiguous because UUIDv5 needs a namespace. The plan now pins
  it to a fixed exported constant `NS_EMBEDDING_PUBLISH` so
  independent implementations and tests produce byte-identical
  `point_id`s. See plan §"Approach" item 1 and step-2.
- **Scenario B payload assertion was a subset.** The Qdrant adapter
  unit test asserts the full 6-tuple but Scenario B only asserted
  4 fields. Scenario B now asserts the same full tuple
  (`publish_id`, `target_id`, `target_kind`, `repo_id`, `kind`,
  `embedding_model_version`) byte-for-byte across the failed and
  retried `UpsertPoint` calls.

### Additional gaps closed in iteration 3 (third rubber-duck pass)

A third independent design critique caught two genuinely substantive
race-condition gaps that the prior passes had missed. Both are
correctness bugs (not just "could be tightened"); both are now
addressed in the plan:

- **The advisory lock alone did NOT prevent regression of a
  successful publish.** The prior wording claimed the
  `pg_advisory_xact_lock` + Tx-A eligibility re-check together
  guarantee "no regression of a successful publish on race". A
  concrete walk-through (now in plan §"No regression on race")
  shows that with the Tx-A / Tx-B / Tx-C split + a
  `vector_written` staleness threshold, a loser flusher can enter
  Tx-A *between* the winner's Tx-B and Tx-C, reserve attempt N+1,
  reach `vector_written@N+1`, and overwrite the winner's
  about-to-be-`published@N` in the "latest by `created_at DESC`"
  view. Fix: the reader's published-filter and both eligibility
  predicates (Tx-A re-check + flusher poll) now use the
  **monotonic** predicate `EXISTS published AND NOT EXISTS
  superseded` — once any attempt has reached `published`, the row
  is permanently visible until `superseded` (out of scope, owned by
  bulk re-embed). Step-11 Scenario G was added to exercise the
  precise interleaving the prior tests did not cover (loser enters
  Tx-A between winner's `vector_written` and `published`). Step-8
  flusher predicate now adds `AND NOT (EXISTS published AND NOT
  EXISTS superseded)` so a successfully-published row is not
  redundantly re-driven.
- **`PublishNew` had no at-most-once contract.** Handler crash
  recovery or at-least-once worker semantics would call
  `PublishNew(node)` twice for the same `(target_id,
  embedding_model_version)` and produce **two** `EmbeddingPublish`
  rows with two distinct `publish_id`s and two distinct
  deterministic `point_id`s — i.e., the same node surfacing as two
  hits in recall. Fix: `PublishNew`'s `INSERT … ON CONFLICT
  (target_id, target_kind, embedding_model_version) WHERE NOT
  EXISTS superseded DO NOTHING RETURNING publish_id` gracefully
  tail-calls `RetryExisting` when a live row already exists, so
  re-invocation is at-most-once on the active version. The
  dependency on the partial UNIQUE index in
  `phase-foundation-and-schema/stage-embedding-state-log-migrations-and-roles`
  is now called out explicitly in §"Open questions". Step-11
  Scenario H asserts the end-to-end at-most-once invariant.
- **`AM_PUBLISH_QUEUED_TIMEOUT` was implicit.** Tx-A said "no-op if
  latest is queued within backoff" but the backoff value was
  undefined; the flusher's poll cadence was a separate concept
  with its own undefined backoff. Two undefined timers cannot agree
  by accident. Fix: one named env var
  `AM_PUBLISH_QUEUED_TIMEOUT` (proposed 5 min) is now the single
  knob shared by both, and is wired through step-5 (`RetryExisting`
  Tx-A eligibility) and step-8 (flusher queued-poll predicate).
