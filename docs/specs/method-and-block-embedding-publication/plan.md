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
the cross-store invariant is observable from the read side as the
**monotonic predicate** "there exists at least one
`EmbeddingPublishEvent` of kind `'published'` for this `publish_id`
**and** no `EmbeddingPublishEvent` of kind `'superseded'` has been
written for it." See §"No regression on race" for why a naïve "latest
event by `created_at DESC` = `'published'`" reader contract would
regress under interleaved retries; the monotonic predicate is the only
reader contract the writer protocol below actually upholds. This stage
owns the **writer** that drives that protocol and the **reader filter**
that consults it.

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
whose **monotonic predicate** (`EXISTS published AND NOT EXISTS
superseded`, see §"No regression on race") does not hold — **or** whose
`embedding_model_version` ≠ the active version — incrementing
`recall_filter_unpublished_total` (e2e-scenarios.md L460) per filtered
hit. **This is intentionally not a "latest event by `created_at DESC` =
`'published'`" predicate**; that semantics would regress a successful
publish under the race walked through in §"No regression on race".
When the Qdrant cosine query itself fails (not the join), the
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
| `point_id`                  | `uuid_v5(NS_EMBEDDING_PUBLISH, publish_id)` | reused from input row            |
| `target_id`, `target_kind`  | from the just-committed Node  | reused from input row            |
| `embedding_model_version`   | from `EmbeddingModel.Version()` at insert time | **always reused as-is.** If it no longer matches the active version, the reader's published-filter silently drops the hit (§9.6). The model-upgrade supersede flow — *new* `EmbeddingPublish` at the active version plus a `superseded` event on the prior `publish_id` — is **owned by the bulk re-embed driver and is out of scope here**. The flusher in this stage **never** mints a new `EmbeddingPublish`. |
| `attempt_index` (on event)  | `1`                           | reserved inside a short Tx-A that takes `pg_advisory_xact_lock(LockKeyForRetry(publish_id))` (64-bit `hashtextextended` per §"`PublishNew` at-most-once contract" / step-4), re-reads the latest event for the `publish_id`, no-ops if latest is `published` / `superseded` (so a racing loser cannot regress a successful publish), otherwise inserts `queued@max+1` and commits. Subsequent `vector_written` / `failed` / `published` events for that attempt are committed in separate short transactions **without** the advisory lock. These follow-on inserts are safe **not** because the DB enforces uniqueness on `(publish_id, attempt_index, event_kind)` — migration 0015 deliberately does **not** add such a constraint (its PK is `(event_id, created_at)` with `event_id = gen_random_uuid()`) — but because the single in-process goroutine that committed Tx-A is the **sole owner of that `attempt_index`** and no other writer can know about it. See §"Why no DB uniqueness on `(publish_id, attempt_index, event_kind)` is required" below for the full code-level ownership argument. External embed + Qdrant calls run **outside** any DB transaction. See §"Transaction scope" and §"No regression on race" below for the rationale. |
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

1. **Tx-A — admit attempt.** Open tx, take the advisory lock
   (`LockKeyForRetry(publish_id)` for `RetryExisting`;
   `LockKeyForPublishNew(targetKind, targetID, version)` for
   `PublishNew` — see §"`PublishNew` at-most-once contract" for why
   the two lock-key namespaces are distinct), re-read the latest
   event for that `publish_id`, decide eligibility (see "No
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
   by this same goroutine, and the publisher's code-level ownership
   of that `attempt_index` (no other writer reserved it) is what
   prevents collisions. The DB itself does **not** enforce
   uniqueness on `(publish_id, attempt_index, event_kind)`; see
   §"Why no DB uniqueness on `(publish_id, attempt_index, event_kind)`
   is required" below.

The advisory-lock window is therefore bounded by the **Tx-A**
duration only — a single SELECT + 1–2 INSERTs — never by Qdrant
latency.

### Why no DB uniqueness on `(publish_id, attempt_index, event_kind)` is required

Iteration 4 of this plan asserted that Tx-B / Tx-C could run
without the advisory lock because the DB would reject a duplicate
`(publish_id, attempt_index, event_kind)` row. That claim was
**wrong**: `services/agent-memory/migrations/0015_embedding_publish.sql`
declares `embedding_publish_event` with `PRIMARY KEY
(event_id, created_at)` where `event_id` defaults to
`gen_random_uuid()`, plus a non-negative `attempt_index` CHECK and
a `(publish_id, created_at DESC)` index. There is **no** unique
index on `(publish_id, attempt_index, event_kind)` and **none is
being added by this stage**. (Adding one is possible but would
have to include the `created_at` partition key per PostgreSQL's
rule, which means it would not enforce global uniqueness anyway —
two `vector_written@N` rows at different `created_at` would both
succeed. So a partial unique index is not a sound mitigation
either.)

The correctness argument is therefore **code-level attempt
ownership**, not a DB constraint:

1. **Reservation is single-writer.** Inside Tx-A, exactly one
   goroutine holds `pg_advisory_xact_lock(LockKeyForRetry(
   publish_id))` (or `LockKeyForPublishNew(...)` for the very
   first attempt). It reads `max(attempt_index)` for that
   `publish_id`, computes `attempt_index = max+1`, INSERTs
   `queued@max+1`, and commits. No other Tx-A can read the same
   max because they are blocked on the lock; once Tx-A commits
   they read a higher max and reserve `max+2`. So at any instant
   **at most one goroutine in the cluster believes it owns
   `(publish_id, attempt_index)`**.
2. **Tx-B / Tx-C are linear continuations of that one goroutine.**
   The publisher inserts `vector_written@attempt_index` (or
   `failed@attempt_index`) in Tx-B and, on success,
   `published@attempt_index` in Tx-C. There is **no other writer
   that knows the value of `attempt_index` it owns** — the value
   lives only in the goroutine's stack frame between Tx-A commit
   and Tx-C commit. A concurrent flusher / publisher that grabs
   the lock after Tx-A commits will read the new max
   (`= attempt_index`) and reserve `attempt_index+1`, never
   `attempt_index`.
3. **Process crash between Tx-A and Tx-C is safe.** If the
   process owning `attempt_index = N` crashes after `Tx-B`
   inserts `vector_written@N` and before Tx-C inserts
   `published@N`, the value `N` is permanently abandoned. A
   future flusher reads `max(attempt_index) = N` and reserves
   `N+1`. The crashed-process state for `attempt_index = N`
   simply never gets a `published@N` row, which is exactly the
   stale-`vector_written` case the flusher's predicate (step-8)
   recovers via `attempt_index+1`. Two attempts may share a
   `publish_id`, but **never** the same `(publish_id,
   attempt_index)` tuple, so each attempt's event chain is
   distinct.
4. **What this argument does NOT defend against.** A bug inside
   `publisher.go` that calls `InsertPublishEvent` twice for the
   same `(publish_id, attempt_index, event_kind)` from the same
   goroutine, or two goroutines somehow holding the same
   reservation, would not be caught by the DB. Step-5's unit
   tests (assertion (f) "no UPDATE or DELETE" + assertion (g)
   "two concurrent `RetryExisting` calls never produce duplicate
   `attempt_index`") and step-11 Scenario F (concurrent flushers)
   are the test-level safety net. If operations want a
   belt-and-braces DB constraint, the right place to add one is
   a **separate migration** in
   `phase-foundation-and-schema/stage-embedding-state-log-migrations-and-roles`
   adding `UNIQUE (publish_id, attempt_index, event_kind, created_at)`
   on `embedding_publish_event` — explicitly out of scope here
   (the partition-key requirement and the resulting weakened
   semantics need their own design pass), but the writer protocol
   would not change at all if that migration later landed.

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

1. Take `pg_advisory_xact_lock(LockKeyForRetry(publish_id))` (64-bit
   `hashtextextended('publish_retry:' || publish_id::text, 0)`).
2. **Terminal-state guards (in order).**
   - If `EXISTS (… event_kind = 'superseded' …)` for this
     `publish_id`, **no-op the retry** — `'superseded'` is a terminal
     dead-letter state and the bulk re-embed driver owns any further
     work for this target. Without this guard, a flusher race could
     append a fresh `queued / vector_written / published` chain
     *after* a `'superseded'` event landed, leaving the log in a
     contradictory state. (Reader correctness still survives because
     the published-filter also checks `NOT EXISTS superseded`, but
     the log integrity does not — adopted from the rubber-duck
     review.)
   - Else, re-check the monotonic predicate above. If it holds (row
     is already published and not superseded), **no-op the retry**
     (release lock on commit and return). This catches the case
     where `published@N` was written *after* the flusher polled but
     *before* the flusher entered Tx-A.
3. Otherwise re-read the latest `EmbeddingPublishEvent` for that
   `publish_id`. If latest is still `'queued'` whose `created_at` is
   newer than `AM_PUBLISH_QUEUED_TIMEOUT` (see "Backoff knob" below),
   no-op (the prior attempt is still in flight).
4. Otherwise (`failed`, or `vector_written` past
   `AM_PUBLISH_RAW_STALENESS`, or `queued` past
   `AM_PUBLISH_QUEUED_TIMEOUT`), reserve `attempt_index = max+1` and
   insert `queued@attempt_index`. Commit.

The flusher's eligibility predicate (step-8) is similarly tightened
with `AND NOT EXISTS (… 'superseded' …) AND NOT (EXISTS published AND
NOT EXISTS superseded)` so neither a superseded row nor a published
row is re-driven — wasted Qdrant work and (for the superseded case)
contradictory log entries, both worth avoiding.

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

### `PublishNew` at-most-once contract — advisory lock + SELECT, no schema dependency

`PublishNew` mints a fresh `publish_id` every time it is called. If
the Stage 3.1 full-mode handler ever calls it twice for the same
target (handler crash recovery, at-least-once worker semantics, an
ingest job re-running over the same file set), naïve code would
create **two** `EmbeddingPublish` rows with two distinct `publish_id`s
and two distinct deterministic `point_id`s, surfacing the same node
as two hits in recall.

**Why a partial UNIQUE index cannot enforce this against migration 0015.**
The natural shape `UNIQUE (target_id, target_kind, embedding_model_version)
WHERE NOT EXISTS (… superseded …)` is **not implementable** against the
shipped schema, for three independent reasons:

1. `services/agent-memory/migrations/0015_embedding_publish.sql`
   defines the target discriminator as two nullable columns —
   `node_id` and `concept_version_id` (with a CHECK enforcing
   exactly-one) — not as a `(target_id, target_kind)` pair. There is
   no `target_id` column to index.
2. `embedding_publish` is `PARTITION BY RANGE (created_at)`. PostgreSQL
   requires every UNIQUE index on a partitioned table to **include the
   partition key**. A unique index over `(node_id, embedding_model_version,
   created_at)` does not enforce global uniqueness — two inserts at
   different timestamps would both succeed.
3. PostgreSQL partial-index predicates **cannot reference another
   table**. `WHERE NOT EXISTS (SELECT 1 FROM embedding_publish_event …
   AND event_kind = 'superseded')` is rejected by the planner because
   `'superseded'` is an `event_kind` value in the sibling table
   `embedding_publish_event`, not a column on `embedding_publish`.

The plan therefore enforces at-most-once **in the publisher, not in
the schema**, using a `pg_advisory_xact_lock` keyed on the target +
model-version tuple. This requires no migration change and is fully
compatible with the append-only contract:

1. **Tx-A discriminated SELECT under an advisory lock.** Inside the
   short Tx-A that admits the attempt, before any INSERT, the publisher
   takes
   `pg_advisory_xact_lock(LockKeyForPublishNew(targetKind, targetID, version))`.
   The lock-key helper is exported from the publisher package and uses
   PostgreSQL `hashtextextended('publish_new:' || targetKind || ':' ||
   targetID::text || ':' || version, 0)::bigint` so the key is 64-bit
   (32-bit `hashtext` collisions cause spurious blocking under bulk
   ingest, never wrong correctness, but the 64-bit variant is the
   safer default — see rubber-duck finding 3). The lock is released
   automatically when Tx-A commits.
2. **Inside the lock, SELECT for any LIVE (non-superseded) publish for
   this target.** Uses `LIMIT 2` (not `LIMIT 1`) so the implementation
   can detect — and refuse to silently proceed past — an invariant
   violation where two non-superseded rows already exist for the same
   target (rubber-duck finding 4). The SELECT is:

   ```sql
   SELECT ep.publish_id, ep.qdrant_point_id
     FROM embedding_publish ep
    WHERE ep.node_id = $1                                            -- or concept_version_id = $1
      AND ep.embedding_model_version = $2
      AND NOT EXISTS (
        SELECT 1 FROM embedding_publish_event ev
         WHERE ev.publish_id = ep.publish_id
           AND ev.event_kind = 'superseded'
      )
    ORDER BY ep.created_at DESC
    LIMIT 2;
   ```

   The two existing partial indexes
   (`embedding_publish_node_id_idx` and
   `embedding_publish_concept_version_id_idx`, both `WHERE … IS NOT
   NULL`) back the node lookup; the EXISTS check rides the
   `(publish_id, created_at DESC)` index from §8.7.2 (point lookup,
   ride-along on the optional partial
   `(publish_id) WHERE event_kind IN ('published','superseded')` index
   if step-9 lands it as a perf addition). Note: the partitioned
   parent has no `(node_id, embedding_model_version)` composite index,
   so PostgreSQL may probe every monthly partition for the node_id
   index. That is fast at v1 partition counts but should be re-measured
   once the rolling window has accumulated >12 months — step-5's
   implementation note calls this out as an observability ask, not a
   schema change here.
3. **Branch A (live row found, `count == 1`):** Tx-A commits with no
   inserts. The publisher facade then dispatches a **fresh call** to
   `RetryExisting(ctx, existing_publish)`, which opens its **own**
   independent Tx-A keyed on
   `pg_advisory_xact_lock(LockKeyForRetry(publish_id))`. **No nested
   lock holding** — PublishNew's lock is fully released before
   RetryExisting acquires its lock. This eliminates the lock-ordering
   ambiguity flagged by iteration 3's evaluator. The two lock-key
   namespaces (`publish_new:…` and `publish_retry:…`) cannot collide.
4. **Branch B (no live row found, `count == 0`):** Tx-A mints
   `publish_id = uuid_v7()` and `point_id =
   uuid_v5(NS_EMBEDDING_PUBLISH, publish_id)`, INSERTs the new
   `EmbeddingPublish` row, INSERTs `queued@1`, commits. The lock is
   released on commit, then the inner core runs external embed +
   `UpsertPoint` + Tx-B + `FetchPoint` + Tx-C as described in
   §"Transaction scope" above.
5. **Branch C (`count == 2`, invariant violated):** the publisher
   logs an `embedding_publish_invariant_violation_total{reason=
   "duplicate_live_publish"}` counter, refuses the attempt, and
   surfaces an error to the handler so an operator alert fires.
   This is defence-in-depth — Branch C should never trigger if all
   writers cooperate via the advisory lock and migration 0015 is
   unchanged — but a manual SQL repair or a future bug must not be
   able to silently double-publish.

**Why the advisory lock is sufficient** (and why the schema needs no
change):

- `pg_advisory_xact_lock` is **cluster-scoped**, not session-scoped.
  Two concurrent `PublishNew` calls from **different** application
  processes (or different `agent-memory` HA replicas) connecting to
  the same PostgreSQL cluster will serialise on the same key, so
  Branch B → Branch A transitions are visible across processes.
- The lock is held for **only Tx-A's duration** (a SELECT + at-most-2
  INSERTs, sub-millisecond). It is **never held across embed or
  Qdrant calls** — those run after Tx-A commits.
- Between Tx-A's SELECT and INSERT, **no other writer can slip in for
  the same target** because the lock is held for the whole transaction.
- The design contract that **all writers go through `PublishNew`** is
  honoured by every call site in this stage. Out-of-scope callers
  that may also insert into `embedding_publish` (the bulk re-embed
  driver — see §"Out of scope") MUST take the same
  `LockKeyForPublishNew(targetKind, targetID, version)` lock and must
  append the prior publish's `superseded` event **in the same Tx-A**
  as their new `EmbeddingPublish` insert; this contract is called out
  in §"Out of scope" so the future stage carries it forward.

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
because `point_id = uuid_v5(NS_EMBEDDING_PUBLISH, publish_id)` is
deterministic, so Qdrant treats the second upsert as an in-place
update of the same point — no orphan vectors, no duplicate hits. This
keeps the publisher's public surface to exactly `PublishNew` +
`RetryExisting` (no third "resume-from-vector_written" method).

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
  Both helpers map the conceptual `(target_kind, target_id)` tuple
  onto the schema's two-column discriminator (`node_id` /
  `concept_version_id`, exactly-one CHECK per migration 0015) at the
  SQL boundary. Also exports two lock-key helpers used by step-5:
  `LockKeyForPublishNew(targetKind, targetID, version) int64`
  (`hashtextextended('publish_new:' || targetKind || ':' ||
  targetID::text || ':' || version, 0)`) and `LockKeyForRetry(publishID)
  int64` (`hashtextextended('publish_retry:' || publishID::text, 0)`).
  Centralising key construction (rubber-duck finding 6) prevents
  implementation drift across call sites; unit tests assert byte-
  identical key generation for `node` vs `concept_version` and for
  matching/non-matching versions. Unit test also asserts the prepared
  SQL has no `UPDATE`/`DELETE` keyword (defence-in-depth against
  accidental mutation).

### Stage 2: §9.6a write protocol + wiring

- **step-5-publisher-state-machine** (`expectedFileChanges: 6`) —
  implement `publisher.go` realising §9.6a steps 2–5 behind **two
  entry points** that share one inner core. **External I/O (embed +
  Qdrant) runs outside any open PostgreSQL transaction**; each state
  transition is its own short committed tx (see §"Transaction scope"
  for the rationale):
    - `PublishNew(ctx, node)` — the full-mode call site. Opens **Tx-A**
      and takes
      `pg_advisory_xact_lock(LockKeyForPublishNew('node', node.id,
      activeVersion))` (the helper exported from step-4 produces a
      64-bit `hashtextextended` key — see §"`PublishNew` at-most-once
      contract" for why this is the only at-most-once mechanism the
      shipped schema supports; the prior plan's `ON CONFLICT` over a
      partial UNIQUE index was **withdrawn** because PostgreSQL cannot
      create such an index on a `created_at`-partitioned parent with a
      cross-table NOT EXISTS predicate). With the lock held, Tx-A runs
      the **`LIMIT 2` live-row SELECT** (see §"`PublishNew`
      at-most-once contract" for the exact statement):
        - **Branch A (1 row found)** — Tx-A commits with **zero
          inserts** (lock released on commit). The publisher facade
          then dispatches a **fresh, independent** call to
          `RetryExisting(ctx, existing_publish)`. The fresh call opens
          its own Tx-A keyed on `LockKeyForRetry(existing.publish_id)`.
          **No nested lock holding** — PublishNew's `publish_new:…` lock
          is fully released before RetryExisting's `publish_retry:…`
          lock is acquired, and the two lock-key namespaces cannot
          collide. This is the explicit commit / lock-ordering fix to
          iteration 3's "tail-call within Tx-A" ambiguity.
        - **Branch B (0 rows found)** — Tx-A mints `publish_id =
          uuid_v7()`, derives `point_id = uuid_v5(NS_EMBEDDING_PUBLISH,
          publish_id)`, INSERTs the new `EmbeddingPublish` row
          (populating either `node_id` or `concept_version_id` per the
          schema's exactly-one CHECK) with the active
          `embedding_model_version` stamped, INSERTs `queued@1`, and
          commits. The lock is released on commit. Then the inner core
          runs embed + `UpsertPoint` outside any tx; Tx-B inserts
          `vector_written@1` (or `failed@1` and returns); `FetchPoint`
          outside any tx; Tx-C inserts `published@1`.
        - **Branch C (2 rows found — invariant violation)** — Tx-A
          rolls back, increments
          `embedding_publish_invariant_violation_total{reason=
          "duplicate_live_publish"}`, and returns an error to the
          handler so an operator alert fires (rubber-duck finding 4).
      **Branch B happy path row count = 4**: 1 `EmbeddingPublish` + 3
      `EmbeddingPublishEvent` rows (`queued`, `vector_written`,
      `published`). **Branch A** invokes RetryExisting which has its
      own row count.
    - `RetryExisting(ctx, publish)` — the flusher call site (also
      called by PublishNew Branch A above as a fresh call after Tx-A
      commits). Reuses the input row's `publish_id` + `point_id` +
      `node_id` (or `concept_version_id`) + `embedding_model_version`.
      Opens **Tx-A**: takes the advisory lock on
      `LockKeyForRetry(publish_id)`. **First** checks `EXISTS (…
      event_kind = 'superseded' …)` for this `publish_id` — if true,
      no-ops (terminal dead-letter, rubber-duck finding 1). **Second**
      checks the monotonic-published predicate (`EXISTS published AND
      NOT EXISTS superseded`). If true, **no-op the retry** and
      return — this prevents a racing loser from being scheduled to
      overwrite a successfully-published row (see plan §"No regression
      on race" for why a latest-by-clock check is not sufficient). If
      neither terminal guard fires, re-reads the latest
      `EmbeddingPublishEvent` for that `publish_id`. If latest is still
      `'queued'` whose `created_at` is newer than
      `AM_PUBLISH_QUEUED_TIMEOUT`, also no-op (the prior attempt is
      still in flight). Otherwise reserves `attempt_index = max+1`,
      inserts `queued@attempt_index`, commits. Then runs embed +
      `UpsertPoint` outside any tx, opens Tx-B for `vector_written` /
      `failed`, runs `FetchPoint`, opens Tx-C for `published` —
      identical to the post-Tx-A flow of `PublishNew`. **Never**
      inserts a new `EmbeddingPublish` row.
      **Happy path row count = 3**: 3 `EmbeddingPublishEvent` rows
      only. **Retry against an already-published or already-superseded
      row = 0 INSERTs** (no-op).
    - Inner core `publishCore(publish_id, point_id, attempt_index)`
      starts immediately after Tx-A has reserved the attempt and
      committed the initial `queued@attempt_index`. It runs the
      external embed + `UpsertPoint` call, then Tx-B (no advisory
      lock — the goroutine that committed Tx-A is the sole owner
      of `attempt_index`, so no other writer can produce a
      colliding `vector_written@N` / `failed@N` row; see plan
      §"Why no DB uniqueness on `(publish_id, attempt_index,
      event_kind)` is required" for the full code-level argument),
      then `FetchPoint`, then Tx-C. If `FetchPoint` fails or the
      process crashes after `vector_written`, the row is picked up
      by the flusher's `vector_written` staleness predicate
      (step-8) and re-driven at `attempt_index+1`. Tx-B / Tx-C of
      attempt N may interleave with Tx-A / Tx-B / Tx-C of attempt
      N+1 from a racing flusher — the monotonic-`published` reader
      filter and flusher predicate (plan §"No regression on race")
      absorb the interleaving without regression.

  Implementation note: the SELECT in PublishNew's Tx-A rides the
  partial `embedding_publish_node_id_idx` (or `…_concept_version_id_idx`)
  defined in migration 0015; there is no composite
  `(node_id, embedding_model_version)` index on the partitioned parent,
  so PostgreSQL may probe every monthly partition for the node-id
  seek. At v1 partition counts this is fast, but step-10 must emit an
  `embedding_publish_tx_a_seconds` histogram so an operator can spot
  degradation once the rolling partition window crosses ~12 months
  (rubber-duck finding 5).

  Unit test against fake adapters asserts: (a) `PublishNew` Branch B
  emits exactly 4 INSERTs in order across exactly 3 committed
  transactions on the happy path; (b) `PublishNew` called twice for
  the same `(node_id, embedding_model_version)` produces exactly
  **one** `EmbeddingPublish` row total (the second call's Tx-A SELECT
  finds the row, Tx-A commits with zero inserts, and the publisher
  facade then dispatches a fresh `RetryExisting` call — at-most-once
  contract enforced via advisory lock + SELECT, **not** via a partial
  UNIQUE index); (c) `RetryExisting` on a non-published row emits
  exactly 3 INSERTs across 3 transactions with the right incremented
  `attempt_index` and **zero** new `EmbeddingPublish` rows;
  (d) `RetryExisting` invoked against a row whose `EXISTS published
  AND NOT EXISTS superseded` predicate holds emits **zero** INSERTs
  (no-op); (d′) `RetryExisting` invoked against a row whose `EXISTS
  superseded` predicate holds also emits **zero** INSERTs (terminal
  guard, rubber-duck finding 1); (e) on Qdrant upsert failure, the
  failed-path INSERTs are `EmbeddingPublish (PublishNew Branch B
  only) + queued + failed` and nothing else; (f) **no** UPDATE or
  DELETE statement is issued in any path; (g) two concurrent
  `RetryExisting` calls for the same `publish_id` on a non-published
  row never produce duplicate `attempt_index` values (the second
  caller blocks on the advisory lock in Tx-A, then either no-ops on
  the monotonic predicate or reserves `attempt_index = max+2`);
  (g′) two concurrent `PublishNew` calls for the same
  `(node_id, version)` produce exactly one `EmbeddingPublish` row —
  the loser observes the winner's row in its post-lock SELECT and
  takes Branch A (zero inserts); (h) the advisory lock is **not**
  held across the embed / `UpsertPoint` / `FetchPoint` external
  calls (asserted by injecting a slow fake adapter and observing
  that a concurrent `RetryExisting` against a *different*
  `publish_id` proceeds without waiting); (i) Branch C path: a test
  that pre-inserts two non-superseded `EmbeddingPublish` rows
  directly for the same target observes that the next `PublishNew`
  call refuses the attempt and increments
  `embedding_publish_invariant_violation_total`.
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
  additionally require BOTH (i) `NOT EXISTS (… event_kind =
  'superseded' …)`** on the `publish_id` (terminal dead-letter — a
  superseded row must never be re-driven, otherwise the flusher
  would append a fresh `queued / vector_written / published` chain
  *after* the supersede event, leaving the log in a contradictory
  state; rubber-duck finding 1) **AND (ii) `NOT (EXISTS published
  AND NOT EXISTS superseded)`** on the `publish_id` (a row that
  already reached `published` must not be re-driven — wasted
  Qdrant work; harmless to correctness because the reader's
  monotonic-`published` filter (step-9) already keeps the row
  visible). These two clauses are deliberately spelled out
  separately even though `(ii)` implies the published-side of
  `(i)` whenever a `published` event exists: spelling them
  separately makes the "no INSERTs after a `superseded` event" log
  invariant **explicit at the flusher layer**, so an
  implementation that copies the predicate cannot accidentally
  drop the superseded guard while preserving the published one.
  LATERAL JOIN on the `(publish_id, created_at DESC)` index from
  §8.7.2 for the latest event; EXISTS checks ride the partial
  `(publish_id) WHERE event_kind IN ('published','superseded')`
  index. The flusher calls `publisher.RetryExisting(ctx, publish)`
  with each eligible row. The flusher **passes the existing
  `EmbeddingPublish` row** (never a `Node`), so `publish_id` +
  `point_id` + `target_id` + `embedding_model_version` are reused.
  Each new attempt appends an event chain with
  `attempt_index = max(prior) + 1`, never an UPDATE, never a
  duplicate `EmbeddingPublish`. **Integration tests (four
  cases — matched 1:1 with the YAML):**
    1. Fake Qdrant fails once then succeeds; row reaches
       `'published'` with `attempt_index = 2`, and the count of
       `EmbeddingPublish` rows for that target is still 1.
    2. `UpsertPoint` succeeds but `FetchPoint` errors once; after
       the staleness window the flusher re-drives the row to
       `published@2` via the `vector_written` predicate
       (step-11 Scenario E).
    3. **No-redundant-flush after `published`.** After `published@1`
       lands, an artificially-stale `vector_written@2` chain (from
       a concurrent race that lost) does **not** trigger another
       flusher attempt — asserts the `NOT (EXISTS published AND
       NOT EXISTS superseded)` clause filters the row out.
    4. **No-reflush after `superseded`.** Pre-insert a
       `superseded` event on a `publish_id` whose latest
       pre-supersede event was `'failed@1'` past the standard
       backoff (the row would be eligible under the latest-event
       check alone). Assert the flusher's `NOT EXISTS superseded`
       clause filters the row out and no `RetryExisting` call is
       issued. This proves clause `(i)` independently of
       clause `(ii)`, and pairs with the inside-Tx-A safety net
       exercised by step-11 Scenario I for direct/administrative
       callers that bypass the flusher.

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
  `deploy/local/docker-compose.yml` stack (PostgreSQL + Qdrant).
  **Nine** test cases mirror the work-item scenarios and lock in the
  identity, liveness, concurrency, at-most-once, and supersede-
  guard contracts:
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
    (h) **`PublishNew` is at-most-once on (node_id,
    embedding_model_version)** — calling `PublishNew(node)` twice
    for the same node (handler retry) leaves exactly **one** live
    `EmbeddingPublish` row in the log. The second call's Tx-A SELECT
    inside the `LockKeyForPublishNew` advisory lock observes the
    winner's committed row, commits Tx-A with zero inserts (Branch
    A), and the publisher facade then dispatches a fresh
    `RetryExisting(existing)` which sees the first attempt is
    either in-flight (no-op) or already published (no-op) or
    failed/stale (appends `attempt_index=2`). The test additionally
    pre-inserts two non-superseded `EmbeddingPublish` rows directly
    for one target and asserts that a subsequent `PublishNew` call
    (i) refuses the attempt, (ii) increments
    `embedding_publish_invariant_violation_total{reason=
    "duplicate_live_publish"}`, (iii) emits zero further INSERTs
    (Branch C — the `LIMIT 2` defence-in-depth contract from §
    "`PublishNew` at-most-once contract"). This asserts the
    end-to-end at-most-once invariant **without** requiring a
    partial UNIQUE index — which is unimplementable against
    migration 0015 (see §"`PublishNew` at-most-once contract" for
    why).
    (i) **`RetryExisting` terminal-superseded guard (direct-call
    safety net).** Pre-insert a `superseded` event on a `publish_id`
    whose latest pre-supersede event was `failed@1` (no `published`
    ever landed). **Invoke `RetryExisting(ctx, publish)` directly**
    — bypassing the flusher entirely, to simulate an
    administrative/manual retry or any future call site that does
    not go through the flusher's eligibility predicate. Assert that
    `RetryExisting`'s **first** Tx-A guard (`EXISTS superseded`)
    fires and produces **zero** INSERTs — proving the rubber-duck
    finding 1 fix at the publisher layer. The flusher's own
    eligibility predicate (step-8 integration test 4) already
    filters this row out *before* invoking `RetryExisting`, so the
    two tests are complementary, not duplicative: step-8 test 4
    proves the flusher never asks `RetryExisting` to retry a
    superseded row, and this Scenario I proves `RetryExisting`
    itself refuses the work even when asked directly. There is no
    "legacy predicate" path — both layers refuse the row; this
    test exists purely for the inside-Tx-A safety net contract on
    direct callers.

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
  here. **Forward contract that the future stage MUST honour** (so
  the at-most-once invariant survives across writers, rubber-duck
  finding 2): (i) the driver MUST take
  `pg_advisory_xact_lock(LockKeyForPublishNew(targetKind, targetID,
  newVersion))` before inserting the new `EmbeddingPublish`; (ii) the
  `superseded` event on the prior `publish_id` and the insert of the
  new `EmbeddingPublish` row MUST be committed in the **same Tx-A**
  (atomic transition) so readers never observe either two live rows
  or zero live rows for the target. If those rules are violated, the
  PublishNew advisory lock alone cannot prevent duplicates. Calling
  this out here so the future stage can lift the contract from this
  plan verbatim.
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
- **No schema dependency for at-most-once.** Iteration 3 named a
  partial UNIQUE index in
  `phase-foundation-and-schema/stage-embedding-state-log-migrations-and-roles`
  as a hard prerequisite for `PublishNew`. That dependency is
  **withdrawn**: such an index is not implementable against the
  shipped `services/agent-memory/migrations/0015_embedding_publish.sql`
  (two-column discriminator, `created_at`-partitioned parent,
  no cross-table NOT EXISTS predicates in partial indexes — see
  §"`PublishNew` at-most-once contract" for the full rationale).
  At-most-once is enforced by `pg_advisory_xact_lock + SELECT`
  inside Tx-A, with `LIMIT 2` defence-in-depth and an
  `embedding_publish_invariant_violation_total` counter. **No
  migration change is required from
  `phase-foundation-and-schema/stage-embedding-state-log-migrations-and-roles`.**
  An optional, additive performance index that step-9 may request —
  partial `(publish_id) WHERE event_kind IN ('published',
  'superseded')` on `embedding_publish_event` — would make the
  reader's two EXISTS lookups point-probes; it is not required for
  correctness and is called out as a perf nicety, not a blocker.
- Should the publisher set a per-transaction `statement_timeout` on
  Tx-A (e.g. 2 s) so a broken connection cannot stall the advisory
  lock indefinitely? Rubber-duck finding 7 — likely yes, with the
  exact value tuned during operational shakeout. Not a blocker.

## Prior feedback resolution (iteration 5)

Direct response to iteration 4's `## Still needs improvement` list,
one item per evaluator finding. Iteration 4's resolution section is
preserved below for traceability but the active list (the one the
evaluator should grade against) is **this** §"Prior feedback
resolution (iteration 5)" subsection immediately below.

### Prior feedback resolution

1. **ADDRESSED — false `(publish_id, attempt_index, event_kind)`
   uniqueness claim removed; code-level attempt ownership argument
   added.** The evaluator correctly noted that migration 0015's
   `embedding_publish_event` table has PK `(event_id, created_at)`
   with `event_id = gen_random_uuid()` plus only the nonneg
   `attempt_index` CHECK and the `(publish_id, created_at DESC)`
   index — there is no DB-level uniqueness on
   `(publish_id, attempt_index, event_kind)`, so the iter-4 claim
   that Tx-B / Tx-C are safe "because uniqueness on that triple is
   enough" was wrong. Three callsites were rewritten to drop the
   schema-uniqueness claim and replace it with a code-level
   attempt-ownership argument: (a) the §"Identity contract"
   `attempt_index` table row, (b) the §"Transaction scope" Tx-C
   bullet, and (c) the step-5 inner-core `publishCore` bullet.
   A new dedicated subsection §"Why no DB uniqueness on
   `(publish_id, attempt_index, event_kind)` is required" was
   added between §"Transaction scope" and §"No regression on
   race" giving the full four-point argument: (i) Tx-A's advisory
   lock makes reservation single-writer, (ii) Tx-B / Tx-C are
   linear continuations of the same goroutine, (iii) crash safety
   leaves the abandoned `attempt_index` permanently orphaned and
   the flusher picks up `attempt_index+1`, (iv) what the argument
   does NOT defend against (in-process bugs — covered by step-5
   unit tests and step-11 Scenario F). Explicit forward note:
   adding a DB unique index later is possible but would have to
   include `created_at` (partition key rule), which weakens its
   semantics — that's why it's not being added here, and adding
   one later would require zero protocol changes.
2. **ADDRESSED — `plan.md` step-8 now mirrors `work-items.yaml`
   step-background-flusher's two explicit eligibility clauses
   plus the superseded-no-reflush test.** The flusher eligibility
   predicate is now spelled out as BOTH (i) `NOT EXISTS (…
   superseded …)` AND (ii) `NOT (EXISTS published AND NOT
   EXISTS superseded)`, with the design rationale for spelling
   them as two clauses rather than one (so an implementation
   cannot accidentally drop the superseded guard while preserving
   the published one). The integration-test list is expanded from
   3 to 4 cases, with the new test 4 (no-reflush after
   `superseded`) pre-inserting a `superseded` event on a
   `failed@1`-past-backoff `publish_id` and asserting the
   flusher's `NOT EXISTS superseded` clause filters the row out
   before any `RetryExisting` call is issued. This matches
   `work-items.yaml` lines 259-293 verbatim in intent.
3. **ADDRESSED — Scenario I rewritten to remove the
   "legacy predicate" contradiction.** The prior wording said
   "the flusher polls and decides this row is eligible by the
   legacy `latest = failed past backoff` predicate; the test
   asserts that RetryExisting's first Tx-A guard fires" — which
   internally contradicted step-8's eligibility predicate that
   already filters superseded rows. Scenario I now explicitly
   states the test **invokes `RetryExisting(ctx, publish)`
   directly** (bypassing the flusher) to simulate an
   administrative/manual retry or any future call site that does
   not go through the flusher's predicate, and adds that
   Scenario I and step-8 test 4 are **complementary** (step-8
   proves the flusher never asks RetryExisting to retry a
   superseded row; Scenario I proves RetryExisting itself
   refuses the work even when asked directly). The word
   "legacy" is removed entirely.

## Prior feedback resolution (iteration 4)

Direct response to iteration 3's `## Still needs improvement` list,
one item per evaluator finding, plus a small set of additional
rubber-duck blind-spots fixed before push. Iteration 3's four prior
`## Additional gaps closed …` subsections are intentionally elided
here — the evaluator flagged them as "overly repetitive"; their
content is preserved inline in the §"Approach", §"No regression on
race", §"Transaction scope", and §"`PublishNew` at-most-once
contract" sections where it actually informs the implementation.

### Prior feedback resolution

1. **ADDRESSED — at-most-once redesigned for the actual schema.**
   The evaluator correctly observed that `INSERT … ON CONFLICT
   (target_id, target_kind, embedding_model_version) WHERE NOT
   EXISTS superseded` is not implementable against migration 0015 on
   three independent grounds (column shape, partition-key forced
   into UNIQUE, no cross-table predicates in partial indexes). The
   plan now enforces at-most-once **in the publisher** via
   `pg_advisory_xact_lock(LockKeyForPublishNew(targetKind, targetID,
   version)) + SELECT … LIMIT 2 + INSERT` inside Tx-A — fully
   implementable against the shipped schema with no migration
   change. See §"`PublishNew` at-most-once contract" (rewritten
   end-to-end) and the rewritten step-5 PublishNew bullet. The
   schema-dependency open-question bullet is rewritten to state the
   dependency is **withdrawn**.
2. **ADDRESSED — stale "latest event = published" reader contract
   purged.** The Problem section and the Approach section's
   read-side description now both use the monotonic predicate
   (`EXISTS published AND NOT EXISTS superseded`), with explicit
   forward references to §"No regression on race" so a reader
   following the plan top-down never sees a contradictory
   contract. All eligibility predicates (RetryExisting Tx-A, flusher
   step-8, reader filter step-9) use the same monotonic semantics.
3. **ADDRESSED — `uuid_v5(publish_id)` → `uuid_v5(NS_EMBEDDING_PUBLISH,
   publish_id)` everywhere.** Every remaining bare `uuid_v5(publish_id)`
   in the §"Identity contract" table, §"Stale `vector_written`" section,
   and the prior-feedback churn was rewritten to use the namespaced
   form. The NS_EMBEDDING_PUBLISH constant is exported from the
   embedding package (step-2 owns the constant; step-4 / step-5
   reference it).
4. **ADDRESSED — explicit commit/lock ordering for PublishNew →
   RetryExisting dispatch.** PublishNew's Tx-A **always commits
   before** any dispatch to RetryExisting. Branch A (live row found):
   Tx-A commits with zero inserts, advisory lock released, then the
   publisher facade makes a **fresh** RetryExisting call which opens
   its own Tx-A keyed on a different lock namespace
   (`publish_retry:…`). **No nested lock holding, no lock-namespace
   collision** — see the rewritten step-5 PublishNew bullet and
   §"`PublishNew` at-most-once contract" Branch A.

### Additional design fixes (rubber-duck pass before iter-4 push)

- **`superseded` is now a terminal guard in RetryExisting.** Prior
  iter-3 only no-oped on `EXISTS published AND NOT EXISTS
  superseded`. A flusher arriving after a model-upgrade `superseded`
  event (but before any `published` ever landed on that publish_id)
  would have appended a fresh `queued / vector_written / published`
  chain *after* the supersede. Log correctness now matches reader
  correctness: RetryExisting checks `EXISTS superseded` first as a
  terminal dead-letter guard. See plan §"No regression on race"
  step 2 and step-5 unit-test assertion (d′).
- **Lock-key helper centralised + 64-bit hash.** Iter-3 used
  ad-hoc `hashtext(publish_id::text)` (32-bit). Step-4 now exports
  `LockKeyForPublishNew` and `LockKeyForRetry` using
  `hashtextextended(…, 0)` for a 64-bit key, with unit tests
  asserting byte-identical key generation across call sites.
- **`LIMIT 2` defence-in-depth for the at-most-once SELECT.** A
  pre-existing duplicate live row (manual repair, future bug, or a
  non-cooperating writer) would have been silently masked by `LIMIT
  1`. PublishNew now selects up to two rows and refuses the attempt
  with `embedding_publish_invariant_violation_total{reason=
  "duplicate_live_publish"}` if two are returned.
- **Tx-A latency observable.** Step-10 emits an
  `embedding_publish_tx_a_seconds` histogram so operators can spot
  the partition-probe regression flagged in step-5's implementation
  note once the rolling partition window crosses ~12 months.
- **Bulk re-embed driver contract spelled out in §"Out of scope".**
  Any future writer that creates a publish for an existing
  `(target, model)` tuple MUST take the same
  `LockKeyForPublishNew(…)` advisory lock, and the supersede event
  on the prior `publish_id` MUST be appended **in the same Tx-A** as
  the new `EmbeddingPublish` insert. Without that contract, the
  bulk driver can race PublishNew and produce two live rows.
