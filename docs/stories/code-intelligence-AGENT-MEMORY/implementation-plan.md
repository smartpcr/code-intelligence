---
title: "agent memory"
storyId: "code-intelligence:AGENT-MEMORY"
---

# Phase 1: Foundation and schema

## Dependencies
- _none -- start phase_

## Stage 1.1: Project scaffold and CI baseline

### Implementation Steps
- [ ] Create the service repository layout: top-level `services/agent-memory/`
      with `cmd/`, `internal/`, `migrations/`, `pkg/`, `proto/`, `web/`, and
      `deploy/` subtrees; commit a project-level `README.md` that points to
      the four sibling docs.
- [ ] Add the language toolchain manifest `go.mod` (the service is
      built in Go for v1: single static binary, native gRPC for the
      §8.5 Agent transport, mature OTel SDK for §3.3 / §8.6) and a
      single `make` target `make lint` that runs `golangci-lint` with
      zero findings on the empty tree.
- [ ] Add `make test` and `make build` targets that succeed on the empty
      tree; both must be invoked by CI on every PR opened against this
      story's branch.
- [ ] Add a `docker compose` file under `deploy/local/` that brings up
      PostgreSQL 16 with `pgcrypto` + `pg_partman` extensions, a Qdrant
      container, and an OTel Collector container, all on a private network
      with healthchecks (`pg_isready`, Qdrant `/healthz`, OTel `:13133`).
- [ ] Wire a CI job that runs `docker compose up -d`, waits for all three
      healthchecks, runs `make test`, and tears the stack down — this
      becomes the substrate for every later integration test.
- [ ] Add a `.editorconfig` and pre-commit hook config that enforce the
      conventions chosen in step 2 (line width, import ordering, etc.).

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: CI green on empty tree -- Given a fresh clone of the
      scaffold, When `make lint && make build && make test` runs in CI,
      Then all three targets exit 0 and CI publishes a green badge.
- [ ] Scenario: local stack healthchecks -- Given `docker compose up -d`
      from `deploy/local/`, When each container's healthcheck is polled
      for up to 60 s, Then PostgreSQL, Qdrant, and the OTel Collector all
      report healthy and `docker compose ps` shows three `running
      (healthy)` rows.
- [ ] Scenario: PostgreSQL extensions present -- Given the local stack is
      up, When `psql -c "SELECT extname FROM pg_extension"` is run, Then
      the result includes both `pgcrypto` and `pg_partman`.

## Stage 1.2: Structural schema migrations

### Implementation Steps
- [ ] Add migration `0001_enums.sql` that creates every named ENUM listed
      in tech-spec §8.7.1 (`node_kind`, `edge_kind`, `episode_kind`,
      `outcome`, `block_kind`, `concept_band`, `producer`, `polarity`,
      `actor`, `observation_role`, `repo_event_kind`, `verb`,
      `degraded_reason`) with members matching the closed sets in
      `architecture.md`.
- [ ] Add migration `0002_repo_commit.sql` that creates `Repo` and
      `Commit` tables per `architecture.md` §5.6 with the `timestamptz`,
      `uuid`, `text` typing of §8.7.1.
- [ ] Add migration `0003_node_edge.sql` that creates `Node` (no
      `embedding_vec` column per §8.7.1) and `Edge` tables with the
      `bytea(32)` fingerprint CHECK and the UNIQUE
      `(repo_id, fingerprint)` indices from §8.7.2.
- [ ] Add migration `0004_retirements.sql` that creates `NodeRetirement`
      and `EdgeRetirement` with UNIQUE `(node_id)` / `(edge_id)` indices
      per §8.7.2.
- [ ] Add migration `0005_trace_observation.sql` that creates
      `TraceObservation` (mutable counters) and the partitioned parent
      `TraceObservationLog` (weekly RANGE on `started_at` per §8.7.3)
      with the B-tree index on `(edge_id, started_at DESC)`.
- [ ] Add migration `0006_repo_event.sql` that creates `RepoEvent` per
      §5.6 with `kind` restricted to the closed upstream set
      `(push|merge|register|manual)` (architecture.md §5.6 line
      644). No other `kind` value may be inserted.
- [ ] Add migration `0006a_ingest_jobs.sql` that creates the
      durable job-queue table `ingest_jobs` used by the Repo
      Indexer (Stage 3.1) and the Management onboarding verbs
      (Stage 7.1): columns `job_id uuid PK`, `repo_id uuid FK`,
      `mode` ENUM `(full|delta|manual)`, `from_sha text?`,
      `to_sha text`, `status` ENUM
      `(pending|claimed|running|done|failed)`, `attempt_index int`,
      `claimed_by text?`, `created_at timestamptz`,
      `updated_at timestamptz`. UNIQUE on
      `(repo_id, mode, COALESCE(from_sha,''), to_sha)` for
      idempotent enqueue, plus a partial B-tree on
      `(status, created_at)` where `status='pending'` to make
      `SELECT … FOR UPDATE SKIP LOCKED` fast.
- [ ] Add a `migrations/test_migrate.go` (or equivalent) that runs every
      migration up and then down on a fresh database, verifying that
      down/up is idempotent.

### Dependencies
- phase-foundation-and-schema/stage-project-scaffold-and-ci-baseline

### Test Scenarios
- [ ] Scenario: structural schema applies cleanly -- Given an empty
      PostgreSQL 16 database, When migrations `0001`-`0006a` are
      applied, Then every expected table (including `ingest_jobs`),
      ENUM, and UNIQUE index exists per `\d+` inspection.
- [ ] Scenario: ingest_jobs accepts only valid mode/status -- Given
      the `ingest_jobs` table exists, When `INSERT INTO ingest_jobs
      (mode) VALUES ('rebuild')` is attempted, Then the insert is
      rejected as an ENUM violation (closed set is
      `full|delta|manual`).
- [ ] Scenario: fingerprint CHECK rejects wrong length -- Given the
      `Node` table exists, When `INSERT INTO node (..., fingerprint)
      VALUES (..., '\x00')` is attempted, Then the insert is rejected
      with a CHECK violation referencing `octet_length`.
- [ ] Scenario: round-trip migration -- Given the structural schema is
      applied, When `migrate down` then `migrate up` is run, Then the
      final schema matches the initial one byte-for-byte under
      `pg_dump --schema-only`.

## Stage 1.3: Episodic and concept schema migrations

### Implementation Steps
- [ ] Add migration `0007_episode.sql` that creates `Episode` partitioned
      monthly on `created_at` (§8.7.3), with all fields from
      `architecture.md` §5.3.1 including
      `synthesized_from_parent_episode_id`,
      `synthesized_from_feedback_episode_id`, `context_id`,
      `degraded`/`degraded_reason`.
- [ ] Add migration `0008_episode_update.sql` that creates
      `EpisodeUpdate` partitioned monthly on `created_at` with FK to
      `Episode.episode_id`.
- [ ] Add migration `0009_observation.sql` that creates `Observation`
      partitioned monthly with the table-level CHECK from §8.7.4 (exactly
      one of `node_id`, `edge_id`, `concept_id`,
      `degraded_recall_context_id` is non-null).
- [ ] Add migration `0010_recall_context_log.sql` that creates
      `RecallContextLog` partitioned monthly with `node_ids`,
      `edge_ids`, `concept_ids` as `uuid[]`.
- [ ] Add migration `0011_concept.sql` that creates `Concept`,
      `ConceptVersion`, and `ConceptSupport` per §5.5; UNIQUE
      `(fingerprint)` on `Concept` (no `repo_id` per G6); B-tree on
      `ConceptVersion (concept_id, version_index DESC)` per §8.7.2.
- [ ] Add migration `0012_run_tables.sql` that creates `ConsolidatorRun`,
      `PromoterRun`, and `reranker_model` per §5.6 / §8.4 (registry).
- [ ] Add migration `0013_synthetic_positive_unique.sql` that adds the
      partial UNIQUE index
      `WHERE kind='synthetic_positive'` on
      `Episode.synthesized_from_feedback_episode_id` per §8.7.2 / risk
      §9.8.
- [ ] Add migration `0014_pg_partman_setup.sql` that registers
      `Episode`, `EpisodeUpdate`, `Observation`, `RecallContextLog`,
      and `TraceObservationLog` with `pg_partman` and provisions the
      first 3 forward partitions for each.
- [ ] Extend `migrations/test_migrate.go` to assert the partial unique
      index, the Observation CHECK, and the pg_partman schedule rows.

### Dependencies
- phase-foundation-and-schema/stage-structural-schema-migrations

### Test Scenarios
- [ ] Scenario: Observation CHECK rejects multi-target -- Given the
      Observation table exists, When a row with both `node_id` and
      `concept_id` set is inserted, Then the insert fails with the
      table-level CHECK error.
- [ ] Scenario: synthetic-positive uniqueness -- Given two
      `synthetic_positive` Episode rows with the same
      `synthesized_from_feedback_episode_id`, When the second is
      inserted, Then the partial UNIQUE index rejects it.
- [ ] Scenario: monthly partitions auto-provision -- Given pg_partman
      is configured for `Episode`, When the maintenance worker runs,
      Then `pg_class` shows partition tables covering at least the
      next 3 months.

## Stage 1.4: Embedding state-log migrations and roles

### Implementation Steps
- [ ] Add migration `0015_embedding_publish.sql` that creates
      `EmbeddingPublish` (append-only) and `EmbeddingPublishEvent`
      (append-only) per tech-spec §9.6a, both monthly-partitioned on
      `created_at` per §8.7.3, with the B-tree on
      `(publish_id, created_at DESC)` per §8.7.2.
- [ ] Add migration `0016_roles_grants.sql` that creates the
      `agent_memory_app` and `agent_memory_admin` roles. Grant
      `INSERT, SELECT` (no `UPDATE`, no `DELETE`) to `agent_memory_app`
      on every table flagged append-only in §8.7.4 (`Node`, `Edge`,
      `NodeRetirement`, `EdgeRetirement`, `Episode`, `EpisodeUpdate`,
      `Observation`, `RecallContextLog`, `TraceObservationLog`,
      `Concept`, `ConceptVersion`, `ConceptSupport`, `Commit`,
      `EmbeddingPublish`, `EmbeddingPublishEvent`). Grant
      `INSERT, SELECT, UPDATE` on the UPDATE-grantable tables from
      §8.7.4 (`TraceObservation`, `Repo`, `ConsolidatorRun`,
      `PromoterRun`, `RepoEvent`, `reranker_model`). Additionally
      grant `INSERT, SELECT, UPDATE` on the durable job-queue
      table `ingest_jobs` (from migration `0006a` in Stage 1.2)
      so the Repo Indexer can `SELECT … FOR UPDATE SKIP LOCKED`,
      flip `status` from `pending → claimed → running → done /
      failed`, and bump `attempt_index` under the
      `agent_memory_app` role. The Management onboarding verbs
      (Stage 7.1) hold the same role and use it to `INSERT` new
      job rows on `mgmt.ingest` / `mgmt.ingest_delta`. The
      `ingest_jobs` table is **not** in the §8.7.4 append-only
      set — it is a working queue, not an audit log — so the
      UPDATE grant is consistent with the §8.7.4 mutability
      classification.
- [ ] Add a Qdrant collection bootstrap script that creates the
      `agent_memory_method`, `agent_memory_block`, and
      `agent_memory_concept` collections with `cosine` distance,
      payload index on `repo_id` and `kind`, and a snapshot schedule.
- [ ] Add an integration test that connects as `agent_memory_app` and
      asserts UPDATE on `Node` fails with `permission denied`.
- [ ] Add an integration test that connects as `agent_memory_app` and
      asserts UPDATE on `TraceObservation` succeeds (counter update).

### Dependencies
- phase-foundation-and-schema/stage-episodic-and-concept-schema-migrations

### Test Scenarios
- [ ] Scenario: app role cannot UPDATE Node -- Given the
      `agent_memory_app` role is logged in, When `UPDATE node SET
      attrs_json='{}' WHERE node_id=...` is issued, Then PostgreSQL
      returns `permission denied for table node`.
- [ ] Scenario: app role can UPDATE TraceObservation -- Given the
      `agent_memory_app` role is logged in, When `UPDATE
      trace_observation SET observation_count = observation_count + 1
      WHERE edge_id=...` is issued, Then the update succeeds and
      affects exactly one row.
- [ ] Scenario: Qdrant collections exist -- Given the bootstrap script
      has run, When `GET /collections` is issued against Qdrant, Then
      all three collections (`agent_memory_method`,
      `agent_memory_block`, `agent_memory_concept`) are present
      with `distance: cosine`.

# Phase 2: Hybrid Graph Store core

## Dependencies
- phase-foundation-and-schema

## Stage 2.1: GraphWriter library

### Implementation Steps
- [ ] Implement `pkg/fingerprint` exposing `NodeFingerprint(repo_id,
      kind, canonical_signature, from_sha)` and `EdgeFingerprint(repo_id,
      kind, src_fp, dst_fp, from_sha)` per G2.
- [ ] Implement `internal/graphwriter/writer.go` with
      `InsertNode`, `InsertEdge`, `InsertObservedCallsEdge` (returns
      existing edge if fingerprint already present, per §3.3 step 3).
      Each function runs inside a single PostgreSQL transaction and uses
      the `agent_memory_app` role.
- [ ] Add an `EnsureRepo` / `EnsureCommit` helper that idempotently
      writes Repo and Commit rows (Commit by `(repo_id, sha)` unique
      key).
- [ ] Wire a structured-logging middleware on every writer call so each
      insert emits `{repo_id, kind, fingerprint_hex, sha}` for audit.
- [ ] Add a unit test pack that asserts the same `(repo_id, kind,
      canonical_signature, from_sha)` tuple produces a byte-identical
      fingerprint across runs (G2 determinism).
- [ ] Add an integration test that inserts a Node twice with the same
      fingerprint and asserts the second insert is a no-op (idempotent
      ingest).

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: fingerprint determinism -- Given two calls to
      `NodeFingerprint` with identical inputs, When their byte outputs
      are compared, Then they are equal and exactly 32 bytes long.
- [ ] Scenario: idempotent Node insert -- Given a Node row already
      exists for fingerprint X, When `InsertNode` is called again with
      the same fingerprint, Then the table still contains exactly one
      row for X and no exception is raised.
- [ ] Scenario: writer denied UPDATE -- Given the GraphWriter library
      issues an internal `UPDATE node` statement (forced via a test
      hook), When the statement reaches PostgreSQL, Then it fails with
      `permission denied` and the writer surfaces a typed
      `WriteContractViolation` error.

## Stage 2.2: GraphReader library

### Implementation Steps
- [ ] Implement `internal/graphreader/reader.go` with `GetNode`,
      `GetEdge`, `ListEdgesFrom(node_id, kinds...)`, and `ListNodes(
      repo_id, kinds, filters...)`. Every read is wrapped in a
      `SELECT ... WHERE NOT EXISTS (SELECT 1 FROM node_retirement WHERE
      node_id = ...)` anti-join so retired rows are filtered out by
      default (G5).
- [ ] Add a `ReaderOptions.IncludeRetired bool` flag so historical
      Episode replay can opt-in to retired rows (per §8.5 of
      architecture).
- [ ] Add a `NeighborhoodCard` builder that resolves Node + 1-hop
      edges + their `TraceObservation` aggregate for `agent.expand` /
      `mgmt.read.graph_node` (§4.5).
- [ ] Wire a `pgxpool` connection pool with the read-only role and a
      max-conn cap chosen to satisfy the §8.3 RPS envelope.
- [ ] Add a unit test pack that asserts the anti-join filters retired
      nodes and that `IncludeRetired=true` returns them.

### Dependencies
- phase-hybrid-graph-store-core/stage-graphwriter-library

### Test Scenarios
- [ ] Scenario: retired node hidden by default -- Given a Node has a
      matching `NodeRetirement` row, When `GetNode(node_id)` is called
      with default options, Then the result is `NotFound`.
- [ ] Scenario: retired node visible with opt-in -- Given the same
      retired Node, When `GetNode(node_id, IncludeRetired=true)` is
      called, Then the Node row is returned with the retirement
      metadata attached.
- [ ] Scenario: neighborhood card resolves observed_calls -- Given a
      Method Node with one `observed_calls` edge whose
      `TraceObservation.observation_count = 42`, When
      `NeighborhoodCard(method_id)` is called, Then the returned card
      lists the edge with `observation_count = 42`.

## Stage 2.3: Tombstone retirement service

### Implementation Steps
- [ ] Implement `internal/retirement/service.go` with
      `RetireNode(node_id, retired_at_sha, superseded_by_node_id?)` and
      `RetireEdge(edge_id, retired_at_sha)`. Both functions insert one
      tombstone row inside a transaction and return an error if the
      target id is missing or already tombstoned.
- [ ] Add a `RetireMany([]id, retired_at_sha)` batch entry point that
      runs a single multi-row INSERT — used by the bulk-rename path
      per risk §9.7.
- [ ] Add a unit test that asserts a second retirement of the same id
      fails with the UNIQUE-index error from §5.2.4.
- [ ] Add a unit test that asserts `superseded_by_node_id` is set when
      a `renamed_to` Edge is also written.

### Dependencies
- phase-hybrid-graph-store-core/stage-graphwriter-library

### Test Scenarios
- [ ] Scenario: double-retirement rejected -- Given a Node has been
      retired once, When `RetireNode` is called again on the same id,
      Then the call fails with the UNIQUE-index violation surfaced as
      `AlreadyRetired`.
- [ ] Scenario: rename retirement links new node -- Given a method is
      renamed in a new commit, When the Repo Indexer calls
      `RetireNode(old_id, sha, superseded_by=new_id)` and writes a
      `renamed_to` Edge, Then `NodeRetirement.superseded_by_node_id =
      new_id` and a `renamed_to` Edge row exists pointing from old to
      new fingerprint.

## Stage 2.4: RecallContextLog append helper

### Implementation Steps
- [ ] Implement `internal/recallcontext/log.go` with
      `Append(verb, repo_id, query_json, node_ids[], edge_ids[],
      concept_ids[], reranker_model_version, served_under_degraded)`
      returning a fresh `context_id`.
- [ ] Add a `Resolve(context_id)` reader used by `mgmt.read.context`
      that joins the dereferenced Node / Edge / Concept cards through
      GraphReader (with `IncludeRetired=true` so historical contexts
      are inspectable per risk §9.13).
- [ ] Add a unit test that asserts the `node_ids[]` ordering is
      preserved end-to-end.
- [ ] Add an integration test that appends 10 000 rows and asserts
      partition pruning on a `WHERE created_at >= now() - 1 day`
      query (EXPLAIN must show partitions skipped).

### Dependencies
- phase-hybrid-graph-store-core/stage-graphreader-library

### Test Scenarios
- [ ] Scenario: ordering preserved -- Given an `Append` call with
      `node_ids=[A,B,C]`, When `Resolve(context_id)` is called, Then
      the returned ordered card list is `[A,B,C]`.
- [ ] Scenario: degraded snapshot flag -- Given an `Append` call with
      `served_under_degraded=true`, When the row is read back, Then
      `served_under_degraded=true` and `mgmt.read.context` returns
      `degraded=true` to its caller.
- [ ] Scenario: partition pruning engaged -- Given 12 monthly
      partitions of `RecallContextLog`, When a `since=now()-1d`
      filter is applied, Then `EXPLAIN` shows only the current and
      previous month's partitions scanned.

# Phase 3: Static ingestion pipeline

## Dependencies
- phase-hybrid-graph-store-core

## Stage 3.1: Repo Indexer worker scaffold (full mode)

### Implementation Steps
- [ ] Implement `internal/repoindexer/worker.go` with a polling loop
      that consumes `ingest_jobs` rows (status=`pending`,
      mode=`full|delta|manual`), claims them with a `SELECT … FOR
      UPDATE SKIP LOCKED`, and runs the appropriate handler.
- [ ] Implement a `materialize.go` helper that shallow-clones the
      configured git host URL at the requested SHA into a temp dir
      and exposes a tree-walker over the workspace.
- [ ] Implement the `full` mode handler that walks every file,
      delegates to the per-language AST parser (Stage 3.2), and
      writes Repo→Package→File ancestry through `GraphWriter`.
- [ ] Add a worker-pool config so 4 workers run in parallel against
      the §8.3 "200 k LOC in ≤ 30 min" target.
- [ ] Publish a `repo.registered` / `repo.full_ingested` event over
      PostgreSQL `LISTEN/NOTIFY` (the event-bus pin for v1 — keeps the
      service single-store; downstream workers `LISTEN` on the
      `agent_memory_events` channel) once the job completes.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: full ingest of a small fixture -- Given a 50-file
      fixture repo with three packages, When the full-mode handler
      runs against HEAD, Then the resulting `Node` rows form a
      complete `repo→package→file` ancestry with no orphan files.
- [ ] Scenario: worker claim is exclusive -- Given two workers
      polling the queue, When both observe the same `ingest_jobs`
      row, Then only one claims it (the other receives zero rows
      due to `FOR UPDATE SKIP LOCKED`).
- [ ] Scenario: idempotent re-ingest -- Given a full ingest has
      completed for SHA X, When the same job is replayed, Then no
      new `Node` rows are inserted (fingerprint idempotency from
      Stage 2.1).

## Stage 3.2: AST node and edge emitters

### Implementation Steps
- [ ] Implement `internal/repoindexer/ast/dispatcher.go` that picks
      a language-specific parser based on file extension and the
      `Repo.language_hints[]` setting.
- [ ] Implement the polyglot parser core on top of `tree-sitter`
      with grammars for the v1 language set — TypeScript / JavaScript
      and Python — that emits Class / Method nodes with canonical
      signatures and the static edges `contains`, `imports`,
      `static_calls`, `extends`, `implements`, `reads`, `writes`.
      Additional grammars (Go, Java, etc.) are added by dropping in
      a new grammar binding without re-architecting the dispatcher.
- [ ] Implement the Block subdivision pass: any Method whose
      normalised logical-line count exceeds the §8.2 threshold (80)
      is decomposed into Blocks with `block_kind ∈ {entry, branch,
      loop_body, exception, exit}` and `parent_node_id` pointing to
      the enclosing Method.
- [ ] Normalise whitespace before canonical signature computation
      per risk §9.7 so formatter-only commits do not churn
      fingerprints.
- [ ] Add a fixture-driven parser test that asserts a known
      TypeScript / JavaScript file and a known Python file (the
      v1 language set pinned in the step above) each produce the
      expected Node + Edge set. Additional-language fixtures
      (Go, Java, etc.) are explicitly out of scope for v1 and
      land in the follow-up stories that drop in their grammars.

### Dependencies
- phase-static-ingestion-pipeline/stage-repo-indexer-worker-scaffold-full-mode

### Test Scenarios
- [ ] Scenario: Method-to-Block split fires at threshold -- Given a
      method with 81 normalised logical lines, When the AST emitter
      runs, Then 2 Block nodes are emitted with `parent_node_id`
      set to the enclosing Method.
- [ ] Scenario: Method-to-Block split does not fire below threshold
      -- Given a method with 80 normalised logical lines, When the
      AST emitter runs, Then no Block nodes are emitted for it.
- [ ] Scenario: whitespace normalisation -- Given the same method
      reformatted only by adding spaces, When the canonical
      signature is computed, Then the resulting fingerprint matches
      the unformatted version's fingerprint exactly.

## Stage 3.3: Method and Block embedding publication

### Implementation Steps
- [ ] Implement `internal/embedding/publisher.go` that wraps a
      configurable embedding-model client (HTTP / SDK) and a Qdrant
      upsert call.
- [ ] Wire the §9.6a write protocol per tech-spec.md §8.7.1
      lines 818-833 — **prerequisite**: the architecture-owned
      `Node` row (Method or Block) is already committed by the
      AST emitter's Stage 3.2 transaction (Step 1 of the
      protocol); this publisher does not insert Node rows.
      Then: insert `EmbeddingPublish` row whose `node_id`
      foreign-keys to that `Node.node_id` (architecture.md
      §5.2.1 — `Node` primary key is `node_id`), then
      `EmbeddingPublishEvent(queued)`, then call the embedder,
      then Qdrant upsert, then `vector_written`, then
      read-after-write confirm, then `published`. On failure
      append `failed` and let the background retry pick up.
- [ ] Carry `embedding_model_version` on each `EmbeddingPublish`
      row per risk §9.6.
- [ ] Have the full-mode handler (Stage 3.1) call the publisher
      for every emitted Method / Block node.
- [ ] Add an integration test that asserts the §9.6a state
      transitions are recorded exactly once per publish.

### Dependencies
- phase-static-ingestion-pipeline/stage-ast-node-and-edge-emitters

### Test Scenarios
- [ ] Scenario: publish state log is complete -- Given an embedded
      Method node, When the publisher finishes, Then
      `EmbeddingPublishEvent` rows for that `publish_id` contain
      exactly one each of `queued`, `vector_written`, `published`
      in that order.
- [ ] Scenario: failed publish retries -- Given a transient Qdrant
      error during step 4, When the publisher records `failed`,
      Then the background retry produces a new `queued` event row
      for the same target (not an update to the existing row) and
      eventually reaches `published`.
- [ ] Scenario: vector excluded until published -- Given an
      `EmbeddingPublish` whose latest event is `queued`, When
      `agent.recall` runs, Then the GraphReader filter excludes
      the row and increments `recall_filter_unpublished_total`.

## Stage 3.4: Delta re-index handler

### Implementation Steps
- [ ] Implement the `delta` mode handler that takes `(repo_id,
      from_sha, to_sha)`, diffs the two SHAs against the git host
      API, and routes each changed file to the AST emitter.
- [ ] For renamed / removed files: call `RetireNode` /
      `RetireEdge` (Stage 2.3) with `retired_at_sha =
      parent(to_sha)` per G5.
- [ ] For renamed members: write a `renamed_to` Edge and pass
      `superseded_by_node_id` to `RetireNode`.
- [ ] Update the EmbeddingIndex via the publisher (Stage 3.3) for
      any Method or Block whose canonical signature changed.
- [ ] Publish a `repo.delta_ingested` event with `from_sha`,
      `to_sha`, and `affected_node_count`.
- [ ] Add an integration test that diffs two committed fixture
      trees and asserts the resulting Node/Edge/Retirement rows.

### Dependencies
- phase-static-ingestion-pipeline/stage-method-and-block-embedding-publication

### Test Scenarios
- [ ] Scenario: removed file retires Nodes -- Given a file F is
      removed in `to_sha`, When the delta handler runs, Then every
      Node whose `canonical_signature` started with F's path has a
      `NodeRetirement` row with `retired_at_sha = parent(to_sha)`.
- [ ] Scenario: rename produces renamed_to edge -- Given a method
      `pkg.Foo#bar` is renamed to `pkg.Foo#baz` in `to_sha`, When
      the delta handler runs, Then a `renamed_to` Edge points from
      the old fingerprint to the new one and the old Node is
      tombstoned with `superseded_by_node_id = new_node.id`.
- [ ] Scenario: bulk rename keyed anti-join is fast -- Given a
      delta that retires 5 000 Nodes in one push, When `GetNode`
      is called against 1 000 random current nodes, Then p95 query
      time stays under 50 ms (UNIQUE-index keyed anti-join per
      §9.7).

## Stage 3.5: Webhook Receiver

### Implementation Steps
- [ ] Implement `cmd/webhook-receiver/main.go` with an HTTPS
      endpoint that accepts push / merge events from the
      configured git host.
- [ ] Verify the per-repo HMAC signature using the secret stored
      at `mgmt.register` time (risk §9.12). Reject 401 on failure
      without writing a `RepoEvent` row.
- [ ] On verified events, write a `RepoEvent(kind, from_sha,
      to_sha)` row and enqueue a `delta-ingest` job. Respond `202
      Accepted` with the job id.
- [ ] Add an end-to-end test (via `docker compose`) that posts a
      signed push payload and asserts a `RepoEvent` row plus an
      `ingest_jobs` row appear.

### Dependencies
- phase-static-ingestion-pipeline/stage-delta-re-index-handler

### Test Scenarios
- [ ] Scenario: invalid signature rejected -- Given a webhook
      payload signed with a wrong secret, When the receiver
      processes it, Then the response is 401 and no `RepoEvent`
      row is written.
- [ ] Scenario: valid push enqueues delta job -- Given a webhook
      payload with a valid HMAC for a registered repo, When the
      receiver processes it, Then a `RepoEvent(kind=push)` row
      and an `ingest_jobs(mode=delta)` row both exist and the
      response is 202.

# Phase 4: Dynamic ingestion pipeline

## Dependencies
- phase-static-ingestion-pipeline

## Stage 4.1: OTel attribute resolver

### Implementation Steps
- [ ] Implement `internal/spaningestor/resolver.go` that
      implements the §8.6 mapping: first
      `code.namespace`+`code.function`, then
      `code.filepath`+`code.lineno` fallback, then drop and
      increment `span_unresolved_total`.
- [ ] Add Block-resolution: after the Method is found, use
      `code.lineno` against the ingested Block boundaries (which
      Stage 3.2 records on each Block's `attrs_json`).
- [ ] Add a unit-test pack covering: clean attributes, missing
      `code.function`, missing both, ambiguous overload (use
      `code.signature` if present per OTel semantic conventions),
      and Block-boundary edge cases.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: clean resolve to Method -- Given a span with
      `code.namespace=pkg` and `code.function=Foo.bar(int)`, When
      the resolver runs, Then it returns the Method Node whose
      `canonical_signature` is `pkg.Foo#bar(int)`.
- [ ] Scenario: fallback to filepath/lineno -- Given a span
      missing `code.function`, When the resolver runs, Then it
      uses `code.filepath` + `code.lineno` to locate the
      enclosing Method and returns it.
- [ ] Scenario: unresolved span counted -- Given a span with
      neither `code.function` nor `code.filepath` set, When the
      resolver runs, Then it returns `Unresolved` and
      `span_unresolved_total{repo_id=...}` is incremented by 1.

## Stage 4.2: Span Ingestor worker

### Implementation Steps
- [ ] Implement `cmd/span-ingestor/main.go` consuming OTel span
      batches from the configured Collector (gRPC OTLP).
- [ ] For each resolved span, write or update the
      `observed_calls` Edge using `GraphWriter.InsertObservedCallsEdge`
      (creates a new Edge with G2 fingerprint if absent).
- [ ] Append one `TraceObservationLog` row per span and update
      the `TraceObservation` aggregate row in the same
      transaction (counters mutation is permitted on this table
      per §8.7.4).
- [ ] Resolve the caller side via `parent_span_id` per §8.6;
      root spans contribute to a "solo aggregate" on the
      destination Method only.
- [ ] Add a backpressure flag: if the queue depth exceeds the
      §8.3 envelope, set `degraded_reason =
      span_ingestor_backpressure` on the per-repo health and
      surface it through subsequent recall responses.

### Dependencies
- phase-dynamic-ingestion-pipeline/stage-otel-attribute-resolver

### Test Scenarios
- [ ] Scenario: first observed call creates Edge -- Given a span
      pair (caller→callee) never observed before, When the span
      batch is ingested, Then a new `observed_calls` Edge exists
      with the G2 fingerprint and one `TraceObservationLog` row
      and one `TraceObservation` row.
- [ ] Scenario: repeated calls increment aggregate -- Given the
      same caller→callee pair observed 100 times, When the
      ingestor finishes, Then `TraceObservation.observation_count
      = 100` and `TraceObservationLog` has 100 rows.
- [ ] Scenario: Span Ingestor backpressure surfaces -- Given the
      Span Ingestor's input queue depth exceeds the §8.3 sustained
      envelope by 2x for 30 s, When a subsequent `agent.recall`
      is served, Then the response carries `degraded=true,
      degraded_reason='span_ingestor_backpressure'`.

## Stage 4.3: TraceObservationLog retention pruner

### Implementation Steps
- [ ] Implement a daily-cron pruner that calls `pg_partman`'s
      `drop_partition_time` to detach partitions older than the
      §8.1 30-day retention window (the §8.1 default).
- [ ] Verify the `TraceObservation` aggregate row is **never**
      pruned (only log partitions are detached) per C8.
- [ ] Emit a `trace_log_partitions_dropped_total` metric per run.
- [ ] Add an integration test that materialises a 35-day-old log
      partition and asserts the pruner detaches it.

### Dependencies
- phase-dynamic-ingestion-pipeline/stage-span-ingestor-worker

### Test Scenarios
- [ ] Scenario: 30-day window dropped -- Given a
      `TraceObservationLog` partition whose week-range ends 35
      days ago, When the pruner runs, Then that partition is
      detached and `trace_log_partitions_dropped_total += 1`.
- [ ] Scenario: aggregate row preserved -- Given an Edge whose
      `TraceObservation` row was populated 60 days ago, When the
      pruner runs, Then the `TraceObservation` row is still
      present (C8 — aggregates are never pruned).

# Phase 5: Agent Surface

## Dependencies
- phase-dynamic-ingestion-pipeline

## Stage 5.1: agent.recall verb

### Implementation Steps
- [ ] Implement the gRPC service skeleton in `proto/agent.proto`
      and `cmd/agent-api/main.go` (mTLS per §8.5).
- [ ] Implement `internal/agentapi/recall.go` step 1 — embed the
      `query` via the same embedding-model client used by the Repo
      Indexer (Stage 3.3) so query and corpus share a vector space.
- [ ] Implement `internal/agentapi/recall.go` step 2 — issue a mixed
      k-NN search against the Qdrant collections
      `agent_memory_method`, `agent_memory_block`, **and**
      `agent_memory_concept`, filtered by `repo_id` (§7.8 mixed
      seed).
- [ ] Implement `internal/agentapi/recall.go` step 3 — filter out
      Qdrant hits whose latest `EmbeddingPublishEvent` is not
      `published` (§9.6a); increment
      `recall_filter_unpublished_total` per filtered hit.
- [ ] Implement `internal/agentapi/recall.go` step 4 — expand the
      seed set by 1-2 structural hops through `GraphReader` (Stage
      2.2) and assemble the candidate set.
- [ ] Implement `internal/agentapi/recall.go` step 5 — invoke the
      reranker (v0 cold-start model from this stage's step below if
      no published `reranker_model` row exists; Phase 6.4's trained
      model otherwise).
- [ ] Implement `internal/agentapi/recall.go` step 6 — append a
      `RecallContextLog` row via `recallcontext.Append` and return
      the `RecallResponse` envelope.
- [ ] Implement the v0 cold-start reranker: pure cosine +
      structural distance fallback per risk §9.5; loaded if no
      published `reranker_model` row exists.
- [ ] Implement the `degraded_reason='graph_store_unavailable'`
      / `embedding_index_unavailable` fallback that serves from
      the most recent valid `RecallContextLog` snapshot.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: mixed seed includes Concepts -- Given a Concept
      promoted to the EmbeddingIndex with high cosine similarity
      to the query, When `agent.recall` runs, Then the
      `RecallResponse.concepts[]` is non-empty and the matching
      Concept appears in the top-k.
- [ ] Scenario: unpublished vectors filtered -- Given a Method
      vector whose latest event is `queued`, When `agent.recall`
      runs and Qdrant returns it as a hit, Then the response
      does not include that Method id and
      `recall_filter_unpublished_total` increments.
- [ ] Scenario: degraded recall returns prior snapshot -- Given
      the Qdrant client returns connection errors, When
      `agent.recall` runs, Then the response carries
      `degraded=true,
      degraded_reason='embedding_index_unavailable'`, the
      `nodes[]` come from the most recent
      `RecallContextLog.node_ids`, and a fresh
      `RecallContextLog` row is written with
      `served_under_degraded=true`.

## Stage 5.2: agent.observe verb

### Implementation Steps
- [ ] Implement `internal/agentapi/observe.go` that validates the
      input (rejects `outcome=human_corrected` per C15 with a
      typed gRPC error), validates each `observation_refs[]` role
      (rejects `degraded_recall_context` per C23), appends one
      `Episode` row and N `Observation` rows in a single
      transaction.
- [ ] When `context_id` references a `RecallContextLog` row with
      `served_under_degraded=true`, automatically append one
      extra Observation with `role='degraded_recall_context'`
      per §6.1.2.
- [ ] Implement the WAL fallback from §7.5: if the
      `Episode`/`Observation` write fails because the partition
      is unavailable, buffer to a local file-based queue and
      return `degraded=true,
      degraded_reason='episodic_log_unavailable'` with the
      eventually-assigned `episode_id`.
- [ ] Add a background flusher that drains the WAL and writes
      buffered rows in arrival order once the partition recovers.
- [ ] Add metric `observe_wal_buffer_depth`.

### Dependencies
- phase-agent-surface/stage-agent-recall-verb

### Test Scenarios
- [ ] Scenario: human_corrected rejected on observe -- Given
      `agent.observe(outcome=human_corrected, ...)`, When the
      call reaches the server, Then it is rejected with a typed
      validation error (gRPC `INVALID_ARGUMENT`) and no `Episode`
      row is written.
- [ ] Scenario: degraded context auto-stamps observation -- Given
      a `context_id` pointing to a row with
      `served_under_degraded=true`, When `agent.observe(...,
      context_id=...)` is called, Then one Observation with
      `role='degraded_recall_context'` and
      `degraded_recall_context_id=context_id` is appended in
      addition to the caller's `observation_refs[]`.
- [ ] Scenario: caller cannot forge degraded_recall_context role
      -- Given `agent.observe` is called with
      `observation_refs=[{role:'degraded_recall_context', ...}]`,
      When the server validates, Then it rejects the call with
      `INVALID_ARGUMENT` per C23.
- [ ] Scenario: WAL fallback returns episode_id -- Given the
      Episode partition is offline, When `agent.observe` is
      called, Then the response carries `degraded=true,
      degraded_reason='episodic_log_unavailable'` and an
      `episode_id`; when the partition recovers, that exact id
      appears in the `Episode` table.

## Stage 5.3: agent.expand verb

### Implementation Steps
- [ ] Implement `internal/agentapi/expand.go` that walks
      `static_calls` and `observed_calls` edges from `node_id` in
      the requested direction up to `depth`, returning each edge
      with its current `TraceObservation` aggregate.
- [ ] Enforce a hard `depth` cap (configurable; default 5) and a
      `max_nodes` cap to bound response size for the §8.3 RPS
      target.
- [ ] Append a `RecallContextLog(verb='expand')` row before
      returning so a later `observe` can pin to this expansion.
- [ ] Use the same degraded-fallback path as `agent.recall` when
      the structural graph is unavailable.

### Dependencies
- phase-agent-surface/stage-agent-recall-verb

### Test Scenarios
- [ ] Scenario: callees with hot-path ranking -- Given a Method M
      with three `observed_calls` edges (counts 1, 10, 100),
      When `agent.expand(M, direction='callees')` runs, Then the
      returned edges are ordered by `observation_count`
      descending.
- [ ] Scenario: depth cap honoured -- Given a call chain of
      depth 10, When `agent.expand(root, depth=5)` runs, Then
      the response contains nodes at depth ≤ 5 only and a
      `truncated=true` flag is set.
- [ ] Scenario: expand writes RecallContextLog -- Given an
      `agent.expand` call succeeds, When the
      `RecallContextLog` is queried for the returned
      `context_id`, Then the row exists with `verb='expand'`.

## Stage 5.4: agent.summarize verb

### Implementation Steps
- [ ] Implement `internal/agentapi/summarize.go` that takes
      either a `node_id` or a `concept_id`, fetches the
      neighborhood card, builds a prompt for the configured
      summariser, and returns `summary_md` plus a citation list.
      The summariser is reached through a **pluggable
      `Summariser` client interface** (vendor pin for v1: any
      OpenAI-API-compatible HTTPS endpoint is supported via
      config — self-hosted vLLM or external API — so deployments
      pick the vendor at deploy time without code changes).
- [ ] Append a `RecallContextLog(verb='summarize')` row keyed by
      the returned `context_id`.
- [ ] On summariser timeout, fall back to a templated summary
      built from canonical signatures plus
      `degraded=true, degraded_reason='reranker_model_stale'` if
      the latest run is older than 7 days (per risk §9.10).

### Dependencies
- phase-agent-surface/stage-agent-recall-verb

### Test Scenarios
- [ ] Scenario: summary cites resolved nodes -- Given an
      `agent.summarize(node_id=M)` call succeeds, When the
      response is inspected, Then every `citations[].node_id` /
      `edge_id` / `concept_id` references a row that exists and
      is reachable from M in the structural graph.
- [ ] Scenario: timeout returns degraded summary -- Given the
      summariser exceeds its 5 s deadline, When `agent.summarize`
      returns, Then `degraded=true` and `summary_md` is the
      templated fallback (not the partial LLM output).

# Phase 6: Learning loop

## Dependencies
- phase-agent-surface

## Stage 6.1: Consolidator worker

### Implementation Steps
- [ ] Implement `cmd/consolidator/main.go` with a polling loop
      that wakes every K minutes (§7.7) or after N new Episodes
      (configurable).
- [ ] **Open the `ConsolidatorRun` lifecycle FIRST** — at the
      start of each tick, INSERT `ConsolidatorRun(run_id=<new
      uuid>, started_at=now(), status='running')` per
      architecture.md §5.6 line 645. This row MUST exist before
      any `ConceptVersion(producer_run_id=...)` references it,
      because architecture.md §5.5.2 line 620 makes
      `ConceptVersion.producer_run_id` an FK → `ConsolidatorRun`
      when `producer='consolidator'`. `ConsolidatorRun` is
      UPDATE-grantable per tech-spec §8.7.4 (and the Stage 1.4
      grant at line 189 already includes it), so the open row
      will be finalised by UPDATE at the end of this tick.
- [ ] Read Episodes since the latest
      `ConsolidatorRun.episode_high_water_mark` (from the most
      recent `status='done'` run, not the row just opened);
      group by `(repo_id, signature_hash_of_observation_set)`.
- [ ] For each group crossing the threshold, append a `Concept`
      row (only if fingerprint not seen) and a `ConceptVersion`
      row with `producer='consolidator'`,
      `producer_run_id=<run_id from the row opened above>`, and
      the new confidence / support / negative counts (G4).
      Attach `ConceptSupport` rows per contributing
      Node/Episode/repo.
- [ ] **Finalise the `ConsolidatorRun` lifecycle** — at the end
      of the tick, UPDATE the same `ConsolidatorRun.run_id` row
      SET `finished_at=now()`,
      `episode_high_water_mark=<new mark>`, `status='done'`
      (or `'failed'` on error). The row stays in place; only
      the four mutable fields transition per §8.7.4.
- [ ] Emit metric `consolidator_episode_lag` =
      max(Episode.created_at) − high-water-mark.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: threshold creates new Concept -- Given 10
      positive Episodes sharing the same observation signature,
      When the Consolidator tick runs, Then exactly one new
      `Concept` row exists and exactly one new `ConceptVersion`
      row references it with `support_count=10`.
- [ ] Scenario: subsequent run only adds version -- Given the
      same group grows to 15 supporting Episodes, When the next
      tick runs, Then no new `Concept` row is added but a fresh
      `ConceptVersion` row exists with `support_count=15`.
- [ ] Scenario: support spans repos -- Given Episodes from two
      different repos share an observation signature, When the
      Consolidator tick runs, Then `ConceptSupport` rows for
      both `repo_id`s exist, and the `Concept` row has no
      `repo_id` column (G6).

## Stage 6.2: Concept Promoter worker

### Implementation Steps
- [ ] Implement `cmd/concept-promoter/main.go` that runs after
      each `ConsolidatorRun` finishes.
- [ ] **Open the `PromoterRun` lifecycle FIRST** — at the start
      of each promoter tick, INSERT `PromoterRun(run_id=<new
      uuid>, started_at=now(), concepts_promoted=0,
      status='running')` per architecture.md §5.6 line 646.
      This row MUST exist before any
      `ConceptVersion(producer='promoter', producer_run_id=...)`
      references it, because architecture.md §5.5.2 line 620
      makes `ConceptVersion.producer_run_id` an FK →
      `PromoterRun` when `producer='promoter'`. `PromoterRun`
      is UPDATE-grantable per tech-spec §8.7.4 (and the Stage
      1.4 grant at line 190 already includes it), so the open
      row is finalised by UPDATE at the end of this tick.
- [ ] Select Concepts whose latest `ConceptVersion` crosses the
      §7.8 threshold (`confidence ≥ 0.7` AND
      `support_count ≥ 5`).
- [ ] **Step 1 of §8.7.1 lines 818-833 write protocol** —
      append the architecture-owned `ConceptVersion` row first,
      carrying only architecture.md §5.5.2 lines 612-623 fields
      (the new `concept_version_id` UUID primary key,
      `concept_id`, `version_index`, `confidence`,
      `confidence_band`, `support_count`, `negative_count`,
      `producer='promoter'`, `producer_run_id` → the `run_id`
      of the **already-opened** `PromoterRun` row above,
      `promoted=true`, `created_at`). The `ConceptVersion` row
      does **NOT** carry an `embedding_model_version` column —
      that field lives exclusively on the `EmbeddingPublish`
      row per tech-spec.md §8.7 lines 807-809. The
      `ConceptVersion` table also has **no physical
      `embedding_vec` column** (per tech-spec.md §8.7.1 line
      569) — the vector body lives in Qdrant only. This row
      MUST exist before any `EmbeddingPublish` row can
      reference it, because
      `EmbeddingPublish.concept_version_id` is a foreign-key
      reference to `ConceptVersion.concept_version_id`
      (architecture.md §5.5.2 line 612).
- [ ] Compute the Concept embedding vector for the row just
      appended (description + canonical-feature-signature) and
      reserve a Qdrant `point_id` for it. No Qdrant write
      happens in this step — only the vector + `point_id`
      computation needed by the next step's `EmbeddingPublish`
      row.
- [ ] **Step 2 of §8.7.1 lines 818-833 write protocol** —
      insert an `EmbeddingPublish` row whose
      `concept_version_id` foreign-keys to the
      `ConceptVersion.concept_version_id` of the row appended
      above, with the reserved Qdrant `point_id` and the
      `embedding_model_version` (the model-version field lives
      on this row per tech-spec.md §8.7 lines 807-809, not on
      the `ConceptVersion` row). Immediately insert the
      matching `EmbeddingPublishEvent` with
      `event_kind='queued'` (Step 3 of the protocol).
- [ ] **Steps 4-5 of §8.7.1 lines 818-833 write protocol** —
      upsert the vector into the Qdrant
      `agent_memory_concept` collection under the reserved
      `point_id`; on success append an `EmbeddingPublishEvent`
      with `event_kind='vector_written'`, on failure append one
      with `event_kind='failed'` and exit (the next promoter
      tick produces a new `'queued'` event, never an UPDATE).
      Concept Promoter is the sole writer of Concept entries to
      the EmbeddingIndex per architecture.md C12.
- [ ] **Step 5 (read-after-write) of §8.7.1 lines 818-833 write
      protocol** — issue a confirming Qdrant fetch for the
      `point_id`; on success append a final
      `EmbeddingPublishEvent` with `event_kind='published'`.
      Recall (Stage 5.1) only surfaces this vector once that
      final `'published'` event exists.
- [ ] **Finalise the `PromoterRun` lifecycle** — at the end of
      the tick, UPDATE the same `PromoterRun.run_id` row SET
      `finished_at=now()`,
      `concepts_promoted=<count of Concepts whose chain reached
      'published' in this tick>`, `status='done'` (or
      `'failed'` on error). A Concept whose chain stalled at
      `'queued'` or `'failed'` is **not** counted in
      `concepts_promoted` until a later tick drives it to
      `'published'`.

### Dependencies
- phase-learning-loop/stage-consolidator-worker

### Test Scenarios
- [ ] Scenario: threshold flips promoted=true with §8.7.1
      ordering -- Given a Concept whose latest `ConceptVersion`
      has `confidence=0.72` and `support_count=5`, When the
      Promoter runs, Then in row-insertion order: (1) a new
      `ConceptVersion(promoted=true, producer='promoter')` row
      exists with a fresh `concept_version_id` UUID, (2) an
      `EmbeddingPublish` row whose `concept_version_id`
      foreign-keys to that
      `ConceptVersion.concept_version_id` exists with
      `created_at >=` the `ConceptVersion.created_at` AND
      carries the `embedding_model_version` value (which is
      absent from the `ConceptVersion` row), (3) the
      `EmbeddingPublishEvent` chain for that `publish_id`
      reaches `event_kind='published'`, and (4) the Concept's
      vector is fetchable from the Qdrant
      `agent_memory_concept` collection under the same
      `point_id`.
- [ ] Scenario: PromoterRun precedes ConceptVersion FK
      reference -- Given the Promoter starts a tick, When the
      `PromoterRun` row and any `ConceptVersion(producer=
      'promoter')` rows produced in that tick are inspected,
      Then the `PromoterRun.started_at` is strictly earlier
      than every referencing
      `ConceptVersion.created_at`, the `PromoterRun.run_id`
      already exists in the table at the moment each
      `ConceptVersion.producer_run_id` insert occurs, and the
      same `PromoterRun.run_id` row is later UPDATE'd with
      `status='done'` and a non-null `finished_at`.
- [ ] Scenario: ConceptVersion precedes EmbeddingPublish -- Given
      the Promoter starts a promotion, When the run is
      inspected, Then for every `EmbeddingPublish` row produced
      in that run the referenced `ConceptVersion.created_at`
      is strictly earlier than that `EmbeddingPublish.created_at`
      (enforces §8.7.1 lines 818-824 ordering: architecture-owned
      row first, then index-state log).
- [ ] Scenario: below threshold stays unpromoted -- Given a
      Concept whose latest version has `confidence=0.65`, When
      the Promoter runs, Then no new `ConceptVersion` row is
      written, no `EmbeddingPublish` row is written for that
      Concept, and the Concept has no Qdrant entry.
- [ ] Scenario: Consolidator never writes EmbeddingIndex --
      Given the Consolidator just emitted a ConceptVersion,
      When the Qdrant `agent_memory_concept` collection is
      queried, Then no new point appears until the Promoter
      runs (C12 — sole writer rule).

## Stage 6.3: Operator-correction auto-promotion

### Implementation Steps
- [ ] Extend the Consolidator from Stage 6.1: at the end of each
      tick, scan `EpisodeUpdate` rows since the last run for
      `new_outcome='human_corrected'` and produce one
      `synthetic_positive` Episode per parent Episode (G7,
      §7.7 step 4).
- [ ] Copy the parent Episode's `context_id` onto the synthetic
      positive (C16). Set `action = corrected_action`,
      `outcome = success`,
      `synthesized_from_parent_episode_id` and
      `synthesized_from_feedback_episode_id` per C16.
- [ ] Mirror the parent's Observation rows onto the synthetic
      positive (C17).
- [ ] Rely on the partial UNIQUE index from migration `0013`
      (§9.8) to prevent double-emission on restart.
- [ ] Add an end-to-end integration test that drives §7.3 from a
      `mgmt.feedback` call all the way to the synthetic positive.

### Dependencies
- phase-learning-loop/stage-consolidator-worker

### Test Scenarios
- [ ] Scenario: correction yields one synthetic positive -- Given
      `mgmt.feedback(parent_id, outcome=human_corrected,
      corrected_action={...})` was just accepted, When the next
      Consolidator tick runs, Then exactly one Episode row with
      `kind='synthetic_positive'`,
      `synthesized_from_feedback_episode_id=<feedback_id>`,
      `synthesized_from_parent_episode_id=<parent_id>` exists.
- [ ] Scenario: synthetic positive copies context -- Given the
      parent Episode has `context_id=X`, When the synthetic
      positive is emitted, Then its `context_id` is also `X` and
      its Observation rows reference the same nodes/edges/
      concepts as the parent's.
- [ ] Scenario: restart does not duplicate -- Given the
      Consolidator crashes after writing the synthetic positive
      but before finalising the open `ConsolidatorRun` row (the
      row exists with `status='running'`), When it restarts
      and reprocesses the same `EpisodeUpdate`, Then the partial
      UNIQUE index rejects the duplicate and no second synthetic
      positive row exists.

## Stage 6.4: Reranker Trainer

### Implementation Steps
- [ ] Implement `cmd/reranker-trainer/main.go` running on the
      §8.4 nightly cadence (plus on-demand on ≥ 5 % growth).
- [ ] Pull labelled training pairs: positive Episodes (including
      synthetic positives) and negative Episodes (`failure`,
      `degraded`, pre-correction `human_corrected`) from the
      trailing 90 days, joined to their `RecallContextLog`
      seeds and `Observation` rows.
- [ ] Train a cross-encoder BERT-class model (≤ 200 M params per
      §8.4) using the configured training framework; emit metrics
      `train_loss`, `eval_ndcg@k`, `rank-of-correct-node@k=20`.
- [ ] Publish a new `reranker_model` row with `version`,
      `artifact_uri`, `trained_at`, `metrics_json`. GraphReader
      reads the latest published version on every request.
- [ ] Mark recall responses `degraded_reason='reranker_model_stale'`
      when `last_trained_at` exceeds 7 days (risk §9.10).
- [ ] Implement the per-operator correction rate cap from risk
      §9.4 (sliding-window count of `mgmt.feedback` per
      `actor`).

### Dependencies
- phase-learning-loop/stage-operator-correction-auto-promotion

### Test Scenarios
- [ ] Scenario: trained model published -- Given the nightly
      run consumes ≥ 100 labelled Episodes, When it completes,
      Then a new `reranker_model` row exists and the GraphReader
      uses it on the next `agent.recall`.
- [ ] Scenario: model stale flag fires -- Given the latest
      `reranker_model.trained_at` is 8 days ago, When
      `agent.recall` runs, Then the response carries
      `degraded=true,
      degraded_reason='reranker_model_stale'`.
- [ ] Scenario: per-operator rate cap engages -- Given operator
      O submits 100 `mgmt.feedback(human_corrected)` calls in
      an hour, When the trainer assembles pairs, Then it caps
      O's contribution at the configured threshold and emits
      `trainer_capped_actor_total{actor=O}`.

# Phase 7: Management Surface

## Dependencies
- phase-learning-loop

## Stage 7.1: Onboarding write verbs

### Implementation Steps
- [ ] Implement `cmd/mgmt-api/main.go` with the REST + JSON
      service skeleton, OIDC token validation, and the
      `degraded` envelope helper.
- [ ] Implement `POST /v1/repos` (`mgmt.register`) that writes a
      `Repo` row, generates a webhook HMAC secret, and returns
      the secret + `repo_id` once to the operator.
- [ ] Implement `POST /v1/repos/{id}/ingest` (`mgmt.ingest`)
      that enqueues a `full-ingest` job at the requested SHA
      (default HEAD).
- [ ] Implement `POST /v1/repos/{id}/ingest_delta`
      (`mgmt.ingest_delta`) that is idempotent on
      `(repo_id, from_sha, to_sha)`.
- [ ] Validate inputs and return typed 4xx errors for malformed
      bodies; 5xx only on infrastructure failures.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: register issues HMAC secret once -- Given a
      fresh `POST /v1/repos`, When the response is read, Then
      the body contains `repo_id` and `webhook_secret`; a
      second `GET` on the repo never reveals the secret.
- [ ] Scenario: ingest_delta is idempotent -- Given two
      identical `POST /v1/repos/{id}/ingest_delta` calls with
      the same SHA pair, When both have completed, Then exactly
      one `RepoEvent(kind=manual)` row (the closed set on
      `RepoEvent.kind` per architecture.md §5.6 line 644 is
      `(push|merge|register|manual)`) and one `ingest_jobs`
      row exist.
- [ ] Scenario: missing OIDC token rejected -- Given a request
      without a `Authorization: Bearer …` header, When the
      Management API processes it, Then the response is 401 and
      no row is written.

## Stage 7.2: Span ingest verb

### Implementation Steps
- [ ] Implement `POST /v1/spans` (`mgmt.ingest_spans`) that
      validates each span against the OTel schema per §6.2.2 and
      forwards verified batches to the Span Ingestor input queue
      (alternative entry path to the OTel Collector — primary
      path remains the Collector).
- [ ] Reject any payload containing an `outcome` or
      `corrected_action` field with `400` and a descriptive
      error (§6.2.2 — these are not span fields).
- [ ] Emit metric `mgmt_ingest_spans_total` partitioned by
      `repo_id` and `status`.

### Dependencies
- phase-management-surface/stage-onboarding-write-verbs

### Test Scenarios
- [ ] Scenario: invalid OTel field rejected -- Given a payload
      whose first span is missing `trace_id`, When
      `POST /v1/spans` is called, Then the response is 400 with
      `validation: trace_id required` and no rows are written.
- [ ] Scenario: outcome field rejected -- Given a payload whose
      first span includes an `outcome` field, When the API
      validates, Then the response is 400 with a §6.2.2
      reference and the batch is dropped.

## Stage 7.3: Feedback verb

### Implementation Steps
- [ ] Implement `POST /v1/episodes/{parent_id}/feedback`
      (`mgmt.feedback`) that validates §6.2.2: if
      `outcome=human_corrected`, `corrected_action` is
      REQUIRED; otherwise it must be omitted.
- [ ] Append a `feedback` Episode (with `context_id=NULL` per
      §4.4 step 2 and `parent_episode_id=parent_id`).
- [ ] Append an `EpisodeUpdate` row that flips the parent's
      effective status (G3 — the parent row itself is never
      mutated).
- [ ] Return `{feedback_episode_id}`. The synthetic positive is
      produced by the Consolidator on its next tick (Stage 6.3)
      — this is not an inline operation.
- [ ] Add an end-to-end test that asserts the full §7.3 wire
      flow lands the expected three Episodes
      (`agent`/`feedback`/`synthetic_positive`).

### Dependencies
- phase-management-surface/stage-onboarding-write-verbs

### Test Scenarios
- [ ] Scenario: corrected_action required on human_corrected --
      Given a `POST /v1/episodes/{id}/feedback` body with
      `outcome=human_corrected` and no `corrected_action`,
      When the API processes it, Then the response is 400 and
      no Episode row is written.
- [ ] Scenario: corrected_action forbidden on other outcomes --
      Given a body with `outcome=failure` and
      `corrected_action={...}`, When the API processes it,
      Then the response is 400 per §6.2.2.
- [ ] Scenario: feedback yields EpisodeUpdate -- Given a
      successful `feedback` call with
      `outcome=human_corrected`, When the writer completes,
      Then exactly one `feedback` Episode and one
      `EpisodeUpdate(new_outcome=human_corrected,
      actor=operator)` row exist.

## Stage 7.4: Snapshot verb

### Implementation Steps
- [ ] Implement `POST /v1/repos/{id}/snapshot`
      (`mgmt.snapshot`) that triggers a forced re-embed of every
      Method / Block / promoted Concept in the repo using the
      currently active embedding-model version (risk §9.6).
- [ ] Job runs through the §9.6a publish protocol — appending
      new `EmbeddingPublish` rows; no row mutation.
- [ ] Emit progress metrics `snapshot_published_total` /
      `snapshot_pending_total`.

### Dependencies
- phase-management-surface/stage-onboarding-write-verbs

### Test Scenarios
- [ ] Scenario: snapshot triggers re-embed -- Given a repo with
      100 Method nodes, When `mgmt.snapshot` is called, Then
      100 new `EmbeddingPublish` rows are inserted with the
      current `embedding_model_version` and each eventually
      reaches `published`.
- [ ] Scenario: snapshot supersedes prior publish -- Given a
      Method already had a `published` EmbeddingPublish, When
      the new snapshot publishes its replacement, Then the
      prior `publish_id` has a final
      `EmbeddingPublishEvent(event_kind='superseded')` row.

## Stage 7.5: Operator read endpoints

### Implementation Steps
- [ ] Implement every `mgmt.read.*` verb from §6.2.3 as
      `GET /v1/{...}` endpoints (`repos`, `commits`,
      `episodes`, `observations`, `context`, `concepts`,
      `concept_supports`, `graph_node`, `trace_observation`).
- [ ] Always carry a top-level `degraded: bool` /
      `degraded_reason: text?` envelope (§6.3, C22) so the UI
      can render a stale-data banner.
- [ ] Pin `mgmt.read.episodes` to require a `since` query
      parameter so partition pruning engages (risk §9.2).
- [ ] Join `Episode` to its latest `EpisodeUpdate` as
      `current_status` per §6.2.3.
- [ ] Add an integration test pack covering every endpoint
      against a seeded database fixture.

### Dependencies
- phase-management-surface/stage-onboarding-write-verbs

### Test Scenarios
- [ ] Scenario: episodes since-filter required -- Given a
      `GET /v1/episodes` without `since`, When the API
      processes it, Then the response is 400 with a `since
      required` error per risk §9.2.
- [ ] Scenario: current_status reflects latest update -- Given
      an Episode with `outcome=failure` plus a later
      `EpisodeUpdate(new_outcome=human_corrected)`, When
      `GET /v1/episodes` returns it, Then `current_status =
      human_corrected` (the original `outcome` column shows
      `failure`).
- [ ] Scenario: context read tolerates retired ids -- Given a
      `RecallContextLog` row whose `node_ids[]` includes a
      tombstoned id, When `GET /v1/context/{id}` runs, Then the
      response includes the retired node with a `retired_at_sha`
      badge field and the call succeeds (risk §9.13).

# Phase 8: Reliability and operations

## Dependencies
- phase-management-surface

## Stage 8.1: Degraded-mode contract wiring

### Implementation Steps
- [ ] Audit every Agent and Management verb against the
      §6.3 / §8.2 degraded-reason matrix; assert each verb
      returns exactly one of the closed reasons
      (`episodic_log_unavailable`, `graph_store_unavailable`,
      `embedding_index_unavailable`, `reranker_model_stale`,
      `span_ingestor_backpressure`,
      `consolidator_backpressure`) when triggered.
- [ ] Add a fault-injection middleware (configurable on a test
      flag) that flips a specific subsystem to "unavailable"
      and asserts the degraded shape per §7.5 / §7.6.
- [ ] Wire the `consolidator_backpressure` flag so
      `agent.observe` queues but never fails (C24).
- [ ] Implement a per-verb `degraded` metric counter so the
      operator dashboard can graph each reason separately.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: closed degraded_reason enforced -- Given a
      fault-injection flag that returns a custom
      `degraded_reason='oops'`, When any verb handles it, Then
      the response is rewritten to a closed value or returns
      `500` (closed set is enforced server-side).
- [ ] Scenario: observe never fails on consolidator pressure --
      Given the Consolidator queue depth exceeds threshold,
      When 100 `agent.observe` calls are issued in a burst,
      Then 100 responses return 200 with `degraded=true,
      degraded_reason='consolidator_backpressure'` and 100
      Episode rows exist after the burst (C24).

## Stage 8.2: Partition rotation automation

### Implementation Steps
- [ ] Configure `pg_partman` maintenance to run every 10
      minutes via PostgreSQL's `pg_cron` extension (or the
      service-side scheduler) to provision new partitions and
      drop expired ones for `Episode`, `EpisodeUpdate`,
      `Observation`, `RecallContextLog`,
      `TraceObservationLog`, `EmbeddingPublish`,
      `EmbeddingPublishEvent`.
- [ ] Implement a `partition_provision_lag` metric (oldest
      next-day partition that is missing).
- [ ] Add an alert rule that fires when `partition_provision_lag
      > 1 day`.
- [ ] Add a chaos test that disables the scheduler for an hour
      and asserts no write fails (`pg_partman` provisions ahead
      of time).

### Dependencies
- phase-reliability-and-operations/stage-degraded-mode-contract-wiring

### Test Scenarios
- [ ] Scenario: forward partitions always present -- Given the
      scheduler has been running, When the partition catalog is
      inspected, Then partitions for the next 3 months exist on
      every monthly-partitioned table.
- [ ] Scenario: lag alert fires -- Given the scheduler is
      paused for 25 hours, When `partition_provision_lag` is
      scraped, Then the value exceeds 1 day and the alert is
      firing.

## Stage 8.3: Observability surface

### Implementation Steps
- [ ] Expose Prometheus metrics from every binary: counters
      (`recall_filter_unpublished_total`,
      `span_unresolved_total`,
      `trainer_capped_actor_total`,
      `mgmt_ingest_spans_total`,
      `snapshot_published_total`, `observe_wal_buffer_depth`,
      `consolidator_episode_lag`, etc.), histograms
      (`agent_recall_duration_seconds`,
      `agent_observe_duration_seconds`,
      `agent_expand_duration_seconds`,
      `mgmt_ingest_spans_batch_duration_seconds`), and gauges
      (`partition_provision_lag`, `reranker_last_trained_at`).
- [ ] Add OTel-trace export from every binary on its own
      operational spans so the service can be observed by the
      same Collector it ingests from.
- [ ] Ship a Grafana dashboard JSON under `deploy/dashboards/`
      with one row per §8.3 SLO and a degraded-reason
      breakdown panel.
- [ ] Ship a logbook of alert rules (Prometheus alerting
      `*.rules.yml`) tied to the §8.3 SLO numbers.

### Dependencies
- phase-reliability-and-operations/stage-degraded-mode-contract-wiring

### Test Scenarios
- [ ] Scenario: dashboard renders with seeded data -- Given
      the local stack is up and the seed script has run, When
      Grafana loads the dashboard JSON, Then every panel
      renders without "No data" and the §8.3 SLO panels show
      a numeric current value.
- [ ] Scenario: alert rule fires on synthetic SLO breach --
      Given the load generator drives `agent.recall` p95 above
      1.5 s for 5 minutes, When the alertmanager evaluates,
      Then the `recall_p95_breach` rule is `firing`.

## Stage 8.4: Load-test calibration harness

### Implementation Steps
- [ ] Implement a `k6` (or `vegeta`) script that drives the
      §8.3 nominal-load envelope: 50 RPS sustained on
      `agent.recall` / `agent.observe`, 20 RPS on
      `agent.expand`, 5 RPS on `agent.summarize`, 50
      batches/min on `mgmt.ingest_spans`.
- [ ] Run the harness against a seeded 200 k LOC fixture repo
      for 30 minutes; capture p50 / p95 / p99 per verb.
- [ ] Persist the calibration result into the repo under
      `docs/stories/code-intelligence-AGENT-MEMORY/load-test-iter1.md`
      (informational, not a contract change) so the operator
      can pin post-calibration SLO numbers via the §8.3
      override route.
- [ ] Add the two learning-quality SLO measurements
      (rank-of-correct-node @ k=20, Concept-hit fraction @
      k=20) to the harness; report them in the same artifact.

### Dependencies
- phase-reliability-and-operations/stage-observability-surface

### Test Scenarios
- [ ] Scenario: harness completes a clean run -- Given the
      local stack is up and the fixture is seeded, When the
      harness runs the 30-minute envelope, Then it exits 0,
      writes the calibration artifact, and no verb errored
      above the 1 % budget.
- [ ] Scenario: learning-quality SLOs reported -- Given the
      harness has run, When the artifact is inspected, Then
      `rank_of_correct_node_at_k20` and
      `concept_hit_fraction_at_k20` are both reported with a
      numeric value.
