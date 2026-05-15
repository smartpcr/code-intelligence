# Agent Memory — End-to-End Scenarios

> Story: `code-intelligence:AGENT-MEMORY` · 21 points
> Sibling planning artifacts: `architecture.md`, `tech-spec.md`, `implementation-plan.md`
> This file owns: **Gherkin-style feature scenarios** that QA executes against
> the Agent Memory subsystem — happy paths plus the edge cases the contract
> commits to.

---

## 0. How to read this document

### 0.1 Scope

This document is the **executable acceptance surface** for the Agent Memory
subsystem. Every Feature in §1-§17 corresponds to one or more wire contracts
or sequence flows that the sibling docs declare normative. Specifically:

- The two HTTPS surfaces and their verbs are defined in `architecture.md`
  §6.1 (Agent Surface) and §6.2 (Management Surface).
- The eight numbered end-to-end flows are defined in `architecture.md` §4
  and re-stated in wire terms in §7.
- The 24 hard constraints (C1-C24) are pinned in `tech-spec.md` §7.
- The 13 identified risks (§9.1-§9.13) drive the bulk of the edge-case
  scenarios in §11-§17 below.
- The closed `degraded_reason` set (`episodic_log_unavailable`,
  `graph_store_unavailable`, `embedding_index_unavailable`,
  `reranker_model_stale`, `span_ingestor_backpressure`,
  `consolidator_backpressure`) is from `architecture.md` §8.2 and
  `tech-spec.md` C22.

This file does **not** redefine schemas, verb signatures, or component
contracts. Each scenario cites the section of `architecture.md` or
`tech-spec.md` it exercises so that a contract change in those docs is
detectable by a corresponding scenario edit here.

### 0.2 Notation conventions

- **Feature / Background / Scenario / Scenario Outline / Examples** — standard
  Gherkin. Scenario Outlines that fan out across a parameter table use
  `<placeholder>` tokens that bind to the `Examples:` rows immediately below.
- **AND-steps** are written `And` (not `*`) to keep the file diffable.
- **Tags** (`@happy`, `@edge`, `@degraded`, `@security`, `@perf`, `@invariant`)
  appear immediately above the Scenario and pick out test categories. CI
  runs `@happy + @edge + @invariant` on every commit; `@degraded`, `@perf`,
  and `@security` run on the nightly cadence.
- **Verbs are written in dotted form** (`agent.recall`, `mgmt.ingest_spans`)
  because the transport (gRPC for Agent Surface, REST for Management Surface
  per `tech-spec.md` §8.5) is irrelevant at this layer of test description.
- **SHA strings** in Examples are deterministic short prefixes (e.g. `sha_A`,
  `sha_B`); the test harness expands them to concrete 40-char hex during
  fixture loading.
- **Constraint references** (`[C2]`, `[G5]`, `[§4.4]`) point back to the
  sibling docs and must remain valid after any future plan edit.

### 0.3 Test-substrate assumptions (Background for every feature)

These are imposed by `tech-spec.md` §8.1 / §8.5 and `implementation-plan.md`
Stage 1.1. Every Feature inherits them implicitly; only Features that
deviate (e.g. degraded-mode features that down a dependency) restate them.

```gherkin
Background:
  Given the Agent Memory service is running on its single deployable
  And PostgreSQL 16 is reachable with extensions `pgcrypto` and `pg_partman` installed
  And Qdrant is reachable on its admin port and reports `/healthz` ok
  And the OTel Collector is reachable and forwards spans to the Span Ingestor
  And a `reranker_model` row exists with `version='v0-cold-start'` (per tech-spec §8.4 / risk §9.5)
  And the active embedding model version is `e5-code-v1`
  And the Agent Surface caller presents a valid mTLS client certificate
  And the Management Surface caller presents a valid OIDC bearer token
```

---

## 1. Feature: Cold registration of a repo

> Anchors: `architecture.md` §4.1, §7.1; `mgmt.register`, `mgmt.ingest`
> (`architecture.md` §6.2.1); `tech-spec.md` C1, C2, C13, C14.

```gherkin
Feature: Cold registration of a new repository
  As an operator onboarding a new code repository
  I want to register the repo and have its top-down structure indexed
  So that the agent can recall structural context on the next agent.recall call
```

```gherkin
@happy
Scenario: Register a repo and observe full ingest completion
  Given no `Repo` row exists for url `https://git.example/acme/svc`
  When the operator invokes `mgmt.register(repo_url='https://git.example/acme/svc', default_branch='main')`
  Then the response carries a new `repo_id`
  And a `Repo` row exists with `current_head_sha = <HEAD of main>`
  And a `RepoEvent(kind='register', from_sha=NULL, to_sha=<HEAD>)` is written [arch §5.6]
  And an `ingest_jobs` row exists with `mode='full'`, `to_sha=<HEAD>`, `status='pending'` [impl Stage 1.2]
  When the Repo Indexer worker claims the job
  Then the job transitions through `claimed → running → done`
  And a `Commit` row exists for `<HEAD>` with `index_status='indexed'`
  And `Node` rows exist for at least one entry per kind in `{repo, package, file, class, method}` [arch §3.7]
  And every `Node` row has a 32-byte fingerprint computed per G2 [tech-spec C1]
  And `mgmt.read.repos()` returns the repo with `status='indexed'`
```

```gherkin
@edge
Scenario: Re-registering the same URL is rejected idempotently
  Given a `Repo` row already exists for url `https://git.example/acme/svc`
  When the operator invokes `mgmt.register(repo_url='https://git.example/acme/svc', default_branch='main')` again
  Then the response references the existing `repo_id`
  And no second `Repo` row is created
  And no additional `ingest_jobs` row is enqueued
```

```gherkin
@edge @invariant
Scenario: Two independent ingests of the same SHA produce identical fingerprints
  Given a repo is registered and indexed at `sha_A`
  When the operator invokes `mgmt.ingest(repo_id, sha=sha_A)` again
  Then the Repo Indexer treats every recomputed (kind, canonical_signature) tuple as a fingerprint match
  And no new `Node` or `Edge` rows are inserted for unchanged entities [C1]
  And the second job ends in `status='done'` with zero new rows reported
```

```gherkin
@edge
Scenario: Cold registration on a 200 k LOC repo completes within the §8.3 envelope
  Given a fresh `Repo` for a 200 k-LOC monorepo and 4 Repo Indexer workers
  When the operator invokes `mgmt.ingest(repo_id)`
  Then the Repo Indexer reports `status='done'` within 30 minutes wall-clock [tech-spec §8.3]
  And `RepoEvent` shows exactly one `register` row and exactly one `repo.registered` event was published
```

```gherkin
@edge @invariant
Scenario: Method exceeding the 80-logical-line threshold is decomposed into Blocks
  Given a fixture file contains Method `pkg.Foo#big()` whose static body is 120 logical lines
  And another Method `pkg.Foo#small()` whose static body is 40 logical lines
  When the operator invokes `mgmt.ingest(repo_id)` and the Repo Indexer completes
  Then exactly one `Node(kind='method', canonical_signature='pkg.Foo#big()')` is written for `big`
  And at least one `Node(kind='block')` is written whose `parent_node_id` is that Method [C20, arch §3.7]
  And every emitted Block Node's `block_kind` is in the closed set `{entry, branch, loop_body, exception, exit}` [tech-spec §8.2]
  And no `Block` Node is written for `pkg.Foo#small()` (the split is conditional on the 80-logical-line threshold) [tech-spec §8.2]
  And each emitted Block Node has an `EmbeddingPublish` row that reaches `event_kind='published'` [tech-spec §9.6a]
```

```gherkin
@edge
Scenario: Ingest at an unknown SHA fails fast without polluting the graph
  Given a repo is registered at HEAD `sha_A`
  When the operator invokes `mgmt.ingest(repo_id, sha='deadbeef00deadbeef00deadbeef00deadbeef00')`
  Then the Repo Indexer claims the job, fails to materialise the tree, and writes `status='failed'`
  And `attempt_index` increases on retry up to the worker's bounded retry policy
  And no `Node`, `Edge`, or `Commit` rows are inserted for `deadbeef…`
  And `mgmt.read.repos()` continues to report `current_head_sha=sha_A`
```

---

## 2. Feature: Delta re-index on git push

> Anchors: `architecture.md` §4.6, §7.4; `tech-spec.md` C2, C3, C19;
> risk §9.7 (tombstone churn), risk §9.12 (webhook spoofing).

```gherkin
Feature: Delta re-index on git push
  As the platform
  I want every push webhook to produce an incremental graph update
  So that the agent's structural recall stays within seconds of HEAD
```

```gherkin
Background:
  Given a repo is indexed at `sha_A`
  And the repo's webhook secret is configured at register time per risk §9.12
```

```gherkin
@happy
Scenario: Push webhook re-indexes only changed files
  Given the operator pushes a commit `sha_B` whose parent is `sha_A` and which changes 3 files
  When the git host POSTs the push webhook with a valid HMAC signature
  Then the Webhook Receiver responds 202 within 100 ms
  And a `RepoEvent(kind='push', from_sha=sha_A, to_sha=sha_B)` row is written
  And an `ingest_jobs` row with `mode='delta', from_sha=sha_A, to_sha=sha_B` is enqueued
  When the Repo Indexer claims the delta job
  Then only files in the diff are re-parsed
  And new `Node`/`Edge` rows are appended with `from_sha=sha_B` [C1]
  And no row from `sha_A` is mutated [C2]
  And the first new `Node` row is visible within 30 seconds of webhook receipt [tech-spec §8.3]
```

```gherkin
@happy @invariant
Scenario: A removed Method produces a NodeRetirement, not a row rewrite
  Given a `Node(kind='method', canonical_signature='pkg.Foo#removed()')` exists at `sha_A`
  When push `sha_B` removes that method
  And the delta-ingest job runs to completion
  Then no column on the original `Node` row is updated [C2]
  And a `NodeRetirement` row exists with `node_id=<that method>`, `retired_at_sha=sha_A` [arch §5.2.4 / G5]
  And `mgmt.read.graph_node(<that method>, sha=sha_B)` reports the node as tombstoned
  And `mgmt.read.graph_node(<that method>, sha=sha_A)` still returns the method card
```

```gherkin
@happy @invariant
Scenario: A renamed Method produces a new fingerprint and one renamed_to Edge
  Given a `Method` node `pkg.Foo#old()` exists at `sha_A`
  When push `sha_B` renames it to `pkg.Foo#new()` with no body change beyond the signature
  Then a new `Node(canonical_signature='pkg.Foo#new()', from_sha=sha_B)` is inserted with a fresh fingerprint
  And an `Edge(kind='renamed_to', src=<old node_id>, dst=<new node_id>)` is inserted exactly once [C3]
  And no `Edge(kind='renamed_from')` row exists — the inverse is a derived view [C3]
  And `NodeRetirement(node_id=<old>, retired_at_sha=sha_A)` is written [C2 / G5]
```

```gherkin
@edge
Scenario Outline: Webhook signature validation
  When the git host POSTs a push event with `signature=<sig>`
  Then the Webhook Receiver responds `<status>` and `<event_written>` RepoEvent row is written
  Examples:
    | sig                                 | status | event_written     |
    | <valid HMAC over body>              | 202    | exactly one       |
    | <missing X-Hub-Signature-256 header>| 401    | no                |
    | <HMAC of a different body>          | 401    | no                |
    | <empty string>                      | 401    | no                |
```

```gherkin
@edge
Scenario: Bulk-rename push produces bounded tombstone churn
  Given a push `sha_B` renames 5 000 symbols in one commit (risk §9.7 scenario)
  When the delta-ingest job completes
  Then 5 000 `NodeRetirement` rows are written, each indexed UNIQUE on `(node_id)` [arch §5.2.4]
  And 5 000 paired `renamed_to` Edge rows are written so the new Nodes inherit relevance signal [C3]
  And `mgmt.read.graph_node` anti-join queries on "current" nodes remain O(log N) and return within the §8.3 p95 envelope
```

```gherkin
@edge
Scenario: Formatter-only push must not retire Nodes
  Given a push `sha_B` only reformats whitespace and produces no semantic change
  When the delta-ingest job runs
  Then the parser normalises whitespace before fingerprinting [risk §9.7 residual]
  And `canonical_signature` for every affected Method is unchanged
  And zero `NodeRetirement` rows are written for that push
```

```gherkin
@edge
Scenario: Delta on a push with an unknown from_sha is no-op
  Given a forged or out-of-order push declares `from_sha='cafecafe…'` that is not in `Commit`
  When the Repo Indexer claims the job
  Then it logs a `delta-precondition-failed` error
  And `ingest_jobs.status='failed'` is recorded
  And no `Node`/`Edge`/`NodeRetirement`/`EdgeRetirement` rows are written
```

---

## 3. Feature: OpenTelemetry span ingestion

> Anchors: `architecture.md` §3.3, §5.2.3; `tech-spec.md` §8.6, C18, C19;
> risks §9.1, §9.11.

```gherkin
Feature: OTel span ingestion into the dynamic call layer
  As the platform
  I want OTel spans to enrich observed_calls edges and TraceObservation aggregates
  So that the agent can rank by actually-observed hot paths
```

```gherkin
Background:
  Given a repo is indexed at `sha_HEAD`
  And the OTel Collector is configured to forward spans to `mgmt.ingest_spans`
```

```gherkin
@happy
Scenario: First-seen call pair produces a new observed_calls Edge
  Given no `Edge(kind='observed_calls', src=<MethodA>, dst=<MethodB>)` exists
  When a batch of 100 spans arrives via `mgmt.ingest_spans` whose parent/child resolution lands on `(MethodA → MethodB)`
  Then a new `Edge(kind='observed_calls')` is appended with a fresh G2 fingerprint [C19]
  And a `TraceObservation(edge_id=<new>, observation_count=100, p50_latency_ms=…, p95_latency_ms=…)` row is created
  And 100 rows are appended to `TraceObservationLog` (one per span) [arch §5.6]
  And no `Node` or `Edge` row from prior ingests is mutated [C2, C19]
```

```gherkin
@happy
Scenario: Repeated call pair updates aggregates without touching the Edge row
  Given an `Edge(kind='observed_calls', edge_id=E1)` and a `TraceObservation` row already exist
  When a batch of 50 additional spans arrives mapping to the same edge
  Then `TraceObservation.observation_count` is incremented to 150
  And `p50_latency_ms` / `p95_latency_ms` are recomputed
  And `latest_span_ref` is updated to the last `(trace_id, span_id)` in the batch
  And the `Edge(edge_id=E1)` row is not updated [C19]
  And 50 new rows are appended to `TraceObservationLog`
```

```gherkin
@edge
Scenario: Span resolution falls back to code.filepath + code.lineno
  Given a span has no `code.function` attribute (risk §9.11 trigger)
  And the span carries `code.filepath='svc/handler.go'` and `code.lineno=142`
  When `mgmt.ingest_spans` processes the batch
  Then the Span Ingestor resolves the span to the enclosing Method via the structural graph [tech-spec §8.6]
  And the span's observation is attached to that Method (no synthetic Node is created)
  And `span_unresolved_total` is **not** incremented
```

```gherkin
@edge
Scenario: Unresolvable spans are dropped and counted
  Given a span has neither `code.function` nor `code.filepath`
  When `mgmt.ingest_spans` processes the batch
  Then the span is dropped (no Edge/Observation row produced)
  And the `span_unresolved_total` counter for that repo is incremented by 1 [tech-spec §8.6 / risk §9.11]
  And the batch response still reports HTTP 200 / OK with `dropped_count` in the body
```

```gherkin
@edge
Scenario: Root span has no parent_span_id
  Given a root span has no `parent_span_id` (tech-spec §8.6 row 3)
  When the Span Ingestor processes it
  Then no `observed_calls` Edge is written (no caller pair to anchor) [tech-spec §8.6]
  And latency is recorded on the destination Method's solo aggregate (no caller side)
```

```gherkin
@edge @degraded
Scenario: Backpressure on the Span Ingestor returns a degraded ingest response
  Given the Span Ingestor's internal queue is at high-water mark
  When `mgmt.ingest_spans(batch)` is called
  Then the verb returns HTTP 200 with `degraded=true, degraded_reason='span_ingestor_backpressure'` [C22]
  And the batch is queued (no spans are dropped silently)
  And the verb completes within the §8.3 envelope (≤ 2 s p95 for ≤ 1k-span batch)
```

```gherkin
@edge @invariant
Scenario: Non-OTel input is rejected
  When a caller posts a non-OTel payload to `mgmt.ingest_spans` (e.g. a py-spy capture, a perf record)
  Then the verb returns a validation error and writes zero rows [C18]
```

```gherkin
@edge
Scenario: TraceObservationLog retention pruning preserves aggregates
  Given a `TraceObservation` row was last updated 40 days ago
  And the retention window is 30 days [tech-spec §8.1]
  When the retention pruner runs
  Then matching `TraceObservationLog` rows older than 30 days are deleted
  And the `TraceObservation` row is **not** deleted [C8]
  And after pruning the aggregate counters can no longer be recomputed from log rows [arch §5.2.3 mutability note]
  And `mgmt.read.trace_observation(edge_id)` still returns the surviving aggregate
```

---

## 4. Feature: Agent recall (read path)

> Anchors: `architecture.md` §4.2, §6.1.1, §6.3, §7.5, §7.6; `tech-spec.md`
> C13, C22, §8.3; risks §9.5, §9.6a.

```gherkin
Feature: agent.recall returns a ranked, replayable RecallContext
  As an agent reasoning loop
  I want a ranked bundle of nodes, edges, and concepts plus a durable context_id
  So that I can ground my next action and replay the same context later
```

```gherkin
Background:
  Given a repo `R1` is registered and indexed
  And at least 50 Method/Block Nodes have been embedded by the Repo Indexer
  And at least 1 Concept is in state `promoted=true` (per arch §7.8)
```

```gherkin
@happy
Scenario: Recall returns mixed Nodes + Concepts in the same ranking
  When the agent invokes `agent.recall(repo_id=R1, query='retry with backoff', k=20)`
  Then the response carries a fresh `context_id`
  And `nodes[]`, `edges[]`, `concepts[]` are all populated
  And the union of `nodes[]` + `concepts[]` is exactly `k=20` ranked entries [arch §4.2 step 2]
  And `reranker_model_version` matches the latest published model row [tech-spec §8.4]
  And `degraded=false`
  And one `RecallContextLog` row exists with `verb='recall'`, `served_under_degraded=false`, `context_id=<as returned>`
```

```gherkin
@happy @invariant
Scenario: Recall is read-only beyond its single RecallContextLog append
  Given a snapshot of the Node / Edge / Episode / Concept tables before the call
  When `agent.recall(R1, …)` is invoked
  Then no row is inserted into, updated in, or deleted from `Node`, `Edge`, `Episode`,
       `EpisodeUpdate`, `Observation`, `Concept`, `ConceptVersion`, or `ConceptSupport` [C13 / G1]
  And exactly one new row is appended to `RecallContextLog`
```

```gherkin
@happy
Scenario: Recall surfaces both static_calls and observed_calls edges
  Given the candidate set after expansion includes call edges of both kinds
  When `agent.recall(R1, …)` is invoked
  Then `edges[]` contains at least one `EdgeCard` of kind `static_calls`
  And at least one of kind `observed_calls`
  And each `EdgeCard` of kind `observed_calls` carries the latest `TraceObservation` aggregate fields [arch §6.1.1]
```

```gherkin
@edge @degraded
Scenario: Graph store outage returns a degraded recall from snapshot
  Given the Hybrid Graph Store reader is unavailable [arch §7.6]
  When `agent.recall(R1, query, k=20)` is invoked
  Then the response carries `degraded=true, degraded_reason='graph_store_unavailable'` [C22]
  And `nodes[]`, `edges[]`, `concepts[]` are served from the most recent valid snapshot
  And `reranker_model_version` reflects that snapshot's version
  And exactly one `RecallContextLog` row is written with `served_under_degraded=true` [arch §5.4.1]
```

```gherkin
@edge @degraded
Scenario: Embedding index outage degrades to structural-prior fallback
  Given Qdrant is unreachable [risk §9.6a / C22]
  When `agent.recall(R1, query, k=20)` is invoked
  Then the response carries `degraded=true, degraded_reason='embedding_index_unavailable'`
  And `nodes[]` is populated from cosine + structural-distance fallback weights [risk §9.5]
  And `concepts[]` may be empty (Concepts live in the unreachable EmbeddingIndex)
```

```gherkin
@edge @degraded
Scenario: Stale reranker is surfaced as a degraded reason
  Given the latest `reranker_model.trained_at` is more than 7 days old [risk §9.10]
  When `agent.recall(R1, query, k=20)` is invoked
  Then `degraded=true, degraded_reason='reranker_model_stale'` is set
  And `nodes[]` is still ranked (using the stale model) and `context_id` is returned
```

```gherkin
@edge
Scenario: Recall on a freshly registered repo with zero Episodes (cold start)
  Given repo `R_cold` has just finished its full ingest and has zero Episodes
  When `agent.recall(R_cold, query, k=20)` is invoked
  Then the response is `degraded=false`
  And `concepts[]` is empty (no Concepts have been consolidated yet) [risk §9.5]
  And `nodes[]` is populated via the structural-prior v0 reranker weights [tech-spec §8.4 / risk §9.5]
```

```gherkin
@edge @invariant
Scenario: Recall hits whose latest EmbeddingPublishEvent is not 'published' are filtered
  Given a Method Node `N1` has a pending `EmbeddingPublish` with latest event `event_kind='queued'`
  When `agent.recall` would otherwise return `N1` via vector similarity
  Then `N1` is filtered out of the response candidate set [tech-spec §9.6a read protocol]
  And the `recall_filter_unpublished_total` counter is incremented
  And the filtered hit is replaced from the next-best candidate so `len(nodes)+len(concepts) == k`
```

```gherkin
@perf
Scenario: Recall p95 latency under nominal load
  Given the system is at 50 RPS sustained on `agent.recall`
  When the harness samples 10 000 round-trips
  Then the p95 round-trip is ≤ 1.5 s and p99 is ≤ 4 s [tech-spec §8.3]
```

---

## 5. Feature: Agent observe (write path)

> Anchors: `architecture.md` §4.3, §6.1.2, §7.2, §7.5; `tech-spec.md`
> C5, C6, C14, C22-C24; risk §9.2.

```gherkin
Feature: agent.observe records an Episode plus Observations against a recall context
  As an agent reasoning loop
  I want to record the action I took, its outcome, and which recall elements I used
  So that the learning loop can consolidate and re-rank
```

```gherkin
Background:
  Given the agent just received `context_id=CTX1` from a successful `agent.recall`
  And `CTX1` references `nodes=[N1, N2]`, `edges=[E1]`, `concepts=[C1]`
```

```gherkin
@happy
Scenario: Successful observe appends exactly one Episode plus N Observation rows
  When the agent invokes
    `agent.observe(repo_id=R1, session_id=S1, trace_id=T1, action={…}, outcome='success',
                   context_id=CTX1,
                   observation_refs=[{role:'node_hit', node_id:N1, weight:0.4},
                                     {role:'edge_hit', edge_id:E1, weight:0.2},
                                     {role:'concept_hit', concept_id:C1, weight:0.4}])`
  Then a single `Episode` row is appended with `kind='agent'`, `outcome='success'`, `context_id=CTX1` [arch §5.3.1]
  And three `Observation` rows are appended (one per ref) with the corresponding `role` and weights [arch §5.3.3]
  And no row is mutated [C5, C6]
  And the response returns `{episode_id, episode_group_id, degraded:false}` [arch §6.1.2]
```

```gherkin
@happy @invariant
Scenario: Observation CHECK enforces exactly-one-target per row
  When `agent.observe` is called with a malformed ref `{role:'node_hit', node_id:N1, concept_id:C1}`
  Then the writer rejects the call with a validation error before insert [arch §5.3.3 CHECK]
  And no `Episode` and no `Observation` row is written
```

```gherkin
@edge @invariant
Scenario: Caller cannot inject a degraded_recall_context Observation
  When the agent supplies `observation_refs=[{role:'degraded_recall_context', node_id:N1}]`
  Then the verb returns a validation error [C23, arch §6.1.2]
  And no `Episode`/`Observation` row is written
```

```gherkin
@edge @invariant
Scenario: agent.observe rejects outcome=human_corrected
  When the agent invokes `agent.observe(…, outcome='human_corrected', corrected_action={…})`
  Then the verb returns a validation error [C15, §6.2.2]
  And no row is written
```

```gherkin
@edge @degraded
Scenario: EpisodicLog outage degrades but does not fail the observe
  Given the EpisodicLog write path is unavailable [arch §7.5]
  When `agent.observe(…)` is invoked
  Then the writer buffers the Episode + Observations into the local WAL
  And the response is `{episode_id:<eventual final id>, degraded:true,
                       degraded_reason:'episodic_log_unavailable'}` [C22]
  And once the EpisodicLog recovers, the WAL drains and the `Episode`/`Observation` rows appear with the same `episode_id` [arch §7.5]
```

```gherkin
@edge @degraded
Scenario: Consolidator backpressure surfaces but never fails the observe
  Given the Consolidator queue is at high-water mark
  When `agent.observe(…)` is invoked
  Then the call succeeds with `degraded_reason='consolidator_backpressure'` [C22, C24]
  And the Episode + Observation rows are written normally
  And the Episode is picked up by the Consolidator when the queue drains
```

```gherkin
@edge
Scenario: Server auto-writes degraded_recall_context Observation when context was degraded
  Given `CTX1` is a `RecallContextLog` row with `served_under_degraded=true`
  When the agent invokes `agent.observe(…, context_id=CTX1, observation_refs=[…])`
  Then in addition to the caller's Observations, the writer appends exactly one extra
       `Observation(role='degraded_recall_context', degraded_recall_context_id=CTX1)` [arch §6.1.2]
  And no other path is allowed to write a `degraded_recall_context` Observation [C23]
```

```gherkin
@edge
Scenario Outline: outcome enum coverage
  When `agent.observe(…, outcome='<outcome>')` is invoked with valid refs
  Then the Episode row is appended with `outcome='<outcome>'`
  And `mgmt.read.episodes(outcome_in=['<outcome>'])` returns it
  Examples:
    | outcome   |
    | success   |
    | failure   |
    | refused   |
    | degraded  |
```

```gherkin
@perf
Scenario: Observe p95 latency under nominal load
  Given the system is at 50 RPS sustained on `agent.observe`
  When the harness samples 10 000 round-trips
  Then the p95 round-trip is ≤ 400 ms and p99 is ≤ 1.5 s [tech-spec §8.3]
```

---

## 6. Feature: Call-chain expansion (agent.expand)

> Anchors: `architecture.md` §4.5, §6.1.3; `tech-spec.md` §8.3.

```gherkin
Feature: agent.expand walks the static + dynamic call graph
  As an agent reasoning loop
  I want to walk the call chain from a known Node along callers or callees
  So that I can rank suggestions by hot path, not just static structure
```

```gherkin
@happy
Scenario: Expand callees from a Method Node, depth 2
  Given a Method Node `N_root` has 3 callees at depth 1 and 7 transitive callees at depth 2
  When the agent invokes `agent.expand(node_id=N_root, direction='callees', depth=2)`
  Then `nodes[]` contains exactly 10 reached Nodes plus `N_root` [arch §6.1.3]
  And `edges[]` contains both `static_calls` and `observed_calls` edges [arch §4.5]
  And every `observed_calls` `EdgeCard` carries the latest `TraceObservation` aggregate
  And one new `RecallContextLog(verb='expand', context_id=<returned>)` row is appended [arch §4.5]
  And no other graph mutation occurs [C13]
```

```gherkin
@happy
Scenario: Expand callers from a Block Node
  Given a Block Node `B1` has 2 distinct Method callers
  When the agent invokes `agent.expand(node_id=B1, direction='callers', depth=1)`
  Then `nodes[]` contains exactly 2 Method Nodes (the callers)
  And `edges[]` contains 2 edges (`call_edge_hit`-eligible) with `dst=B1`
```

```gherkin
@edge @degraded
Scenario: Expand under graph_store_unavailable serves cached snapshot
  Given the Hybrid Graph Store reader is unavailable
  When `agent.expand(node_id=N_root, direction='callees', depth=3)` is invoked
  Then the response is `degraded=true, degraded_reason='graph_store_unavailable'` [C22, arch §6.3]
  And `nodes[]`/`edges[]` are served from the cached snapshot
  And `context_id` references a `RecallContextLog(served_under_degraded=true, verb='expand')` row
```

```gherkin
@edge
Scenario: Expand on a retired Node still returns its historical neighbours
  Given Method Node `N_old` was retired at `sha_A` (tombstoned per G5)
  When `agent.expand(node_id=N_old, direction='callees', depth=1)` is invoked
  Then `nodes[]` is populated using the historical edges at the SHA when `N_old` was current [arch §3.7, C21]
  And each card carries a `retired=true` flag for entities that no longer exist at HEAD
```

```gherkin
@edge @invariant
Scenario: Expand respects the depth bound
  When the agent invokes `agent.expand(node_id=N_root, direction='callees', depth=5)`
  Then the walk does not exceed 5 hops
  And the response latency stays within the §8.3 envelope (≤ 1.5 s p95 at depth ≤ 3; degrade gracefully beyond)
```

---

## 7. Feature: Summarize

> Anchors: `architecture.md` §4.2 dependency, §6.1.4, §6.3.

```gherkin
Feature: agent.summarize produces a Markdown synopsis with citations
  As an agent reasoning loop
  I want a short Markdown summary of a Node or Concept with citations
  So that I can inject a digest into a prompt without re-walking the graph
```

```gherkin
@happy
Scenario: Summarize a Method Node returns Markdown plus citations
  When the agent invokes `agent.summarize(node_id=N1, max_tokens=512)`
  Then `summary_md` is non-empty and ≤ 512 tokens
  And `citations[]` references at least one of `N1`, an incident edge, or a related Concept [arch §6.1.4]
  And `target_kind='node'`, `target_id=N1`, `degraded=false`
  And one `RecallContextLog(verb='summarize')` row is appended
```

```gherkin
@happy
Scenario: Summarize a promoted Concept
  Given Concept `C1` has at least one ConceptVersion with `promoted=true`
  When the agent invokes `agent.summarize(concept_id=C1, max_tokens=256)`
  Then `target_kind='concept'`, `target_id=C1`
  And `citations[]` references supporting Nodes / Episodes (`ConceptSupport` rows)
```

```gherkin
@edge @degraded
Scenario: Summarize under graph outage returns a cached summary
  Given the graph store reader is unavailable [arch §6.3]
  When `agent.summarize(node_id=N1, max_tokens=256)` is invoked
  Then `degraded=true, degraded_reason='graph_store_unavailable'`
  And `summary_md` is a cached prior summary or a banner string
  And `citations[]` may be empty [arch §6.3 row 4]
```

---

## 8. Feature: Operator correction (human_corrected → synthetic positive)

> Anchors: `architecture.md` §4.4, §7.3, §1.3 G7; `tech-spec.md`
> C15, C16, C17; risks §9.4, §9.8.

```gherkin
Feature: Operator correction auto-produces a synthetic positive Episode
  As an operator reviewing a failure
  I want my correction to feed the reranker without rewriting history
  So that future recall pulls toward the corrected action
```

```gherkin
Background:
  Given a parent Episode `EP_parent` exists with `kind='agent', outcome='failure', context_id=CTX_p`
  And `EP_parent` has Observations `{O1(node_hit, N1), O2(edge_hit, E1), O3(concept_hit, C1)}`
```

```gherkin
@happy @invariant
Scenario: Submitting a correction writes the full 3-Episode chain
  When the operator invokes
    `mgmt.feedback(parent_episode_id=EP_parent, outcome='human_corrected',
                   corrected_action={…}, note='re-route through the retry helper')`
  Then a new Episode `EP_fb` is appended with:
        `kind='feedback'`,
        `parent_episode_id=EP_parent`,
        `context_id=NULL` [arch §4.4 step 2],
        `corrected_action=<as supplied>`
  And an `EpisodeUpdate(episode_id=EP_parent, new_outcome='human_corrected', actor='operator')` row is appended [arch §4.4 step 3]
  And `EP_parent` row itself is not mutated [C6, G3]
  When the next Consolidator tick fires
  Then exactly one synthetic positive Episode `EP_syn` is appended with:
        `kind='synthetic_positive'`,
        `context_id=CTX_p` (copied from EP_parent) [G7, C16],
        `action={…corrected_action…}`,
        `outcome='success'`,
        `synthesized_from_parent_episode_id=EP_parent`,
        `synthesized_from_feedback_episode_id=EP_fb` [arch §5.3.1, C16]
  And three mirror Observation rows attached to `EP_syn` reference `{N1, E1, C1}` [C17, arch §7.3 step 4]
  And `parent_episode_id` is **not** set on `EP_syn` [arch §5.3.1]
```

```gherkin
@edge @invariant
Scenario: corrected_action is mandatory on human_corrected and forbidden elsewhere
  When the operator invokes `mgmt.feedback(parent_episode_id=EP_parent, outcome='human_corrected')` with no `corrected_action`
  Then the verb returns a validation error [C15, arch §6.2.2]
  When the operator invokes `mgmt.feedback(parent_episode_id=EP_parent, outcome='success', corrected_action={…})`
  Then the verb returns a validation error (corrected_action must be omitted) [C15]
```

```gherkin
@edge @invariant
Scenario: Restart-safe single emission of synthetic positive
  Given a `human_corrected` correction is in flight and the service crashes between writing EpisodeUpdate and the next Consolidator tick
  When the service restarts and the Consolidator runs
  Then exactly one synthetic positive Episode is produced [arch §8.3, risk §9.8]
  And the partial unique index on `(kind='synthetic_positive', synthesized_from_feedback_episode_id)` rejects any second attempt [risk §9.8, impl Stage 1.3 migration 0013]
```

```gherkin
@edge
Scenario: Acknowledgement (non-correcting feedback) writes EpisodeUpdate only
  When the operator invokes `mgmt.feedback(parent_episode_id=EP_parent, outcome='success', note='looks right after re-read')`
  Then a feedback Episode is appended with `kind='feedback', context_id=NULL, corrected_action=NULL`
  And an `EpisodeUpdate(new_outcome='success', actor='operator')` row is appended
  And no synthetic positive Episode is produced [G7 only fires on human_corrected]
```

```gherkin
@edge
Scenario: Multiple operators correcting the same parent each produce one synthetic positive
  Given two different operators each submit `mgmt.feedback(EP_parent, outcome='human_corrected', …)`
  When both Consolidator ticks complete
  Then exactly one synthetic positive Episode exists per distinct `feedback_episode_id` (i.e. two synthetic positives total)
  And the partial unique index allows both because their `synthesized_from_feedback_episode_id` differ [risk §9.8]
  And the per-operator rate cap (tech-spec §9.4 mitigation) is enforced by the Reranker Trainer downstream
```

---

## 9. Feature: Consolidation tick

> Anchors: `architecture.md` §4.7, §7.7; `tech-spec.md` C9-C12; risks §9.3, §9.8.

```gherkin
Feature: Consolidator emits Concepts, ConceptVersions, ConceptSupport rows
  As the platform
  I want Episodes to be periodically distilled into reusable Concepts
  So that future recall surfaces cross-episode patterns alongside Nodes
```

```gherkin
@happy
Scenario: First-time Concept emission when support crosses the threshold
  Given 5 Episodes within the trailing window share the same observation-set signature for a candidate pattern
  And no `Concept` row with that fingerprint exists [arch §7.7 step 3]
  When the Consolidator tick fires
  Then exactly one new `Concept` row is appended [G4 immutability]
  And one `ConceptVersion(version_index=1, confidence∈[0,1], support_count=5, producer='consolidator', producer_run_id=<this run>, embedding_vec=NULL, promoted=false)` is appended [arch §5.5.2]
  And 5 `ConceptSupport(polarity='positive', repo_id=…, episode_id=…)` rows are appended [arch §5.5.3]
  And a `ConsolidatorRun(status='success', episode_high_water_mark=<new>)` row is written [arch §5.6]
```

```gherkin
@happy
Scenario: Subsequent ticks emit new ConceptVersions, not Concept rewrites
  Given a Concept `C1` exists with ConceptVersion `v1` (support_count=5)
  When 3 more positive Episodes match the same signature and the Consolidator runs
  Then no new `Concept` row is created [G4]
  And a new `ConceptVersion(concept_id=C1, version_index=2, support_count=8, embedding_vec=NULL, promoted=false)` is appended
  And 3 new `ConceptSupport` rows are appended (one per new Episode)
  And the `Concept.created_at` field is unchanged (row is immutable) [G4, C9]
```

```gherkin
@edge @invariant
Scenario: Consolidator never writes EmbeddingIndex entries
  When the Consolidator tick completes
  Then zero Qdrant upserts are issued by the Consolidator [C12]
  And every `ConceptVersion` produced by it has `producer='consolidator'` and `embedding_vec=NULL`
```

```gherkin
@edge
Scenario: Negative Episodes accumulate as negative ConceptSupport
  Given the parent Episode of a synthetic-positive chain is now in `outcome='human_corrected'` and 4 sibling negative Episodes share the same signature
  When the Consolidator runs
  Then 4 `ConceptSupport(polarity='negative')` rows are appended
  And `ConceptVersion.negative_count` reflects them
  And `confidence_band` is derived at write time per the §8 thresholds
```

```gherkin
@edge @invariant
Scenario: Cross-repo Concept fingerprint collisions do not duplicate Concepts
  Given two repos `R1` and `R2` each produce 5 Episodes whose canonical-name + feature-signature hash to the same Concept fingerprint
  When the Consolidator runs against both
  Then exactly one `Concept` row exists (global namespace per G6, C10) [risk §9.3]
  And `ConceptSupport` rows exist for both `R1` and `R2` (carrying the supporting `repo_id`)
```

---

## 10. Feature: Concept promotion and EmbeddingIndex publish

> Anchors: `architecture.md` §7.8, §3.5; `tech-spec.md` C11, C12, §9.6a;
> risk §9.6 (re-embed), §9.6a (cross-store staleness).

```gherkin
Feature: Concept Promoter publishes Concepts to the EmbeddingIndex
  As the platform
  I want only well-supported Concepts to become first-class recall candidates
  So that cold or thin patterns do not pollute cross-repo recall
```

```gherkin
@happy @invariant
Scenario: Promotion threshold and the embedding-publish protocol
  Given Concept `C1`'s latest `ConceptVersion` has `confidence=0.72, support_count=6, promoted=false`
  When the Concept Promoter runs
  Then the Promoter inserts a new `ConceptVersion(producer='promoter', producer_run_id=<this PromoterRun>, embedding_vec=<new vector>, promoted=true)` [arch §7.8 step 3, C9]
  And one `EmbeddingPublish(target=<that ConceptVersion>, point_id=<new>, embedding_model_version='e5-code-v1')` row is appended [tech-spec §9.6a]
  And `EmbeddingPublishEvent(event_kind='queued')` is appended
  And the Promoter upserts the vector into Qdrant under `<new point_id>`
  And `EmbeddingPublishEvent(event_kind='vector_written')` is appended
  And after the read-after-write check `EmbeddingPublishEvent(event_kind='published')` is appended
  And `C1` becomes a first-class candidate in subsequent `agent.recall` calls [arch §4.2 step 2]
  And the prior `Concept` row is not mutated [G4]
```

```gherkin
@edge
Scenario: Sub-threshold Concept stays un-promoted
  Given Concept `C2`'s latest `ConceptVersion` has `confidence=0.65, support_count=4`
  When the Concept Promoter runs
  Then no new `ConceptVersion(promoted=true)` is written for `C2`
  And no `EmbeddingPublish` row is appended for `C2`
  And `C2` is still excluded from vector recall (no current embedding per the §5.5.1 rule)
```

```gherkin
@edge @invariant
Scenario: Re-embedding the same Concept supersedes the prior publish
  Given Concept `C1` has an active `EmbeddingPublish(EP1, model='e5-code-v1', latest event='published')`
  When the active embedding model is upgraded to `e5-code-v2` and the Promoter re-embeds `C1`
  Then a new `EmbeddingPublish(EP2, model='e5-code-v2', point_id=<new>)` row is appended
  And the protocol drives EP2 to `event_kind='published'`
  And exactly one `EmbeddingPublishEvent(publish_id=EP1, event_kind='superseded')` is appended on the prior row [tech-spec §9.6a, risk §9.6]
  And no `EmbeddingPublish` row is updated [G5-style append-only on the index-state tables]
  And the GraphReader resolves `C1` recall hits through EP2 thereafter
```

```gherkin
@edge @degraded
Scenario: Qdrant outage during publish leaves the EmbeddingPublish in 'queued'/'failed' but never mutates rows
  Given Qdrant is unreachable mid-promotion
  When the Promoter attempts the upsert
  Then `EmbeddingPublishEvent(event_kind='failed', attempt_index=1)` is appended
  And the prior `EmbeddingPublish` row is not updated [tech-spec §9.6a]
  And on the next retry a new `EmbeddingPublishEvent(event_kind='queued', attempt_index=2)` is appended
  And `agent.recall` reports `degraded_reason='embedding_index_unavailable'` for affected hits [C22]
```

---

## 11. Feature: Cross-repo Concept queries

> Anchors: `architecture.md` §8.4, §3.8; `tech-spec.md` C10.

```gherkin
Feature: Cross-repo Concept inspection
  As an operator
  I want to see which repos support a given Concept
  So that I can audit cross-repo learning signals
```

```gherkin
@happy
Scenario: List concepts with latest version joined
  Given Concept `C1` has two ConceptVersions (v1, v2 promoted=true) and supports in `R1` and `R2`
  When the operator invokes `mgmt.read.concepts(filter={promoted:true})`
  Then `C1` is in the response
  And the latest joined version is `v2` [arch §6.2.3]
  And `Concept.repo_id` is not present in the result (no such column per G6, C10)
```

```gherkin
@happy
Scenario: Filter cross-repo support rows by repo
  When the operator invokes `mgmt.read.concept_supports(concept_id=C1, repo_id=R1)`
  Then only `ConceptSupport` rows whose `repo_id=R1` are returned [arch §6.2.3 / §8.4]
  And every row carries the `concept_version_id` that pinned its contribution
```

---

## 12. Feature: Operator inspection (mgmt.read.* read path)

> Anchors: `architecture.md` §4.8, §6.2.3; risk §9.13 (stale RecallContext).

```gherkin
Feature: Operator inspects a degraded run and its provenance
  As an operator triaging a failure
  I want to see the Episode, its Observations, and the recall context it grounded on
  So that I can decide whether to acknowledge or correct
```

```gherkin
@happy
Scenario: Drill from episode list into RecallContext
  When the operator invokes `mgmt.read.episodes(repo_id=R1, outcome_in=['failure','degraded'], since='7d')`
  Then the response lists Episodes plus their `current_status` joined from `EpisodeUpdate` [arch §6.2.3]
  When the operator invokes `mgmt.read.context(context_id=<picked>)`
  Then the response returns the full `RecallContextLog` row plus dereferenced Node/Edge/Concept cards
  And entries whose Nodes are tombstoned carry a "retired at SHA …" badge [risk §9.13]
```

```gherkin
@happy
Scenario: Observation list for an Episode
  When the operator invokes `mgmt.read.observations(episode_id=EP1)`
  Then the response returns every Observation row attached to `EP1` (one per recall element used) [arch §5.3.3]
  And rows whose `role='degraded_recall_context'` carry their `degraded_recall_context_id`
```

```gherkin
@edge
Scenario: Trace observation tail paging
  When the operator invokes `mgmt.read.trace_observation(edge_id=E1)`
  Then the response returns the current `TraceObservation` aggregate
  And a paged tail of `TraceObservationLog` rows (most recent first) bounded by the retention window [tech-spec §8.1]
```

```gherkin
@edge @degraded
Scenario Outline: Every read verb carries the degraded envelope
  Given the Hybrid Graph Store reader is `<state>`
  When the operator invokes `<verb>` with valid args
  Then the response carries top-level `degraded=<deg>` and `degraded_reason=<reason>` [arch §6.3 last row]
  Examples:
    | state         | verb                              | deg   | reason                        |
    | available     | mgmt.read.repos()                 | false | (omitted)                     |
    | unavailable   | mgmt.read.repos()                 | true  | graph_store_unavailable       |
    | unavailable   | mgmt.read.episodes(since='1d')    | true  | graph_store_unavailable       |
    | unavailable   | mgmt.read.context(context_id=CTX) | true  | graph_store_unavailable       |
```

---

## 13. Feature: Degraded-mode contract surface

> Anchors: `architecture.md` §6.3, §7.5, §7.6, §8.2; `tech-spec.md` C22-C24.

```gherkin
Feature: Every verb has a normative degraded-mode response
  As a calling agent or operator
  I want each verb to return a verb-specific degraded shape
  So that I can react without inferring envelope semantics
```

```gherkin
@degraded @invariant
Scenario Outline: degraded_reason is restricted to the closed set
  Given a fault `<fault>` is induced on the named dependency
  When `<verb>` is invoked with valid args
  Then the response carries `degraded=true, degraded_reason='<reason>'`
  And `<reason>` is in the closed set
        {episodic_log_unavailable, graph_store_unavailable, embedding_index_unavailable,
         reranker_model_stale, span_ingestor_backpressure, consolidator_backpressure} [arch §8.2, C22]
  Examples:
    | fault                                | verb                  | reason                          |
    | kill EpisodicLog writer              | agent.observe         | episodic_log_unavailable        |
    | block PostgreSQL reader              | agent.recall          | graph_store_unavailable         |
    | block Qdrant reader                  | agent.recall          | embedding_index_unavailable     |
    | reranker_model.trained_at > 7 days   | agent.recall          | reranker_model_stale            |
    | Span Ingestor queue at high water    | mgmt.ingest_spans     | span_ingestor_backpressure      |
    | Consolidator queue at high water     | agent.observe         | consolidator_backpressure       |
```

```gherkin
@degraded @invariant
Scenario: Unknown degraded_reason values are forbidden
  When any verb attempts to return `degraded_reason='qdrant_partition_split'` (not in the closed set)
  Then the service-level contract test fails (the value would not pass schema validation downstream) [C22]
```

```gherkin
@degraded
Scenario: Recall under degraded mode still writes a RecallContextLog row
  Given the Hybrid Graph Store reader is unavailable
  When `agent.recall` returns `degraded=true`
  Then exactly one `RecallContextLog(served_under_degraded=true)` row is appended [arch §5.4.1]
  And a later `agent.observe(context_id=<that>)` triggers the server-side auto-write of
       one `Observation(role='degraded_recall_context')` row [C23, arch §6.1.2]
```

---

## 14. Feature: Identity, immutability, and tombstones (audit invariants)

> Anchors: `architecture.md` §1.3 (G2, G3, G4, G5, G6, G7), §5.2.1-§5.2.4,
> §5.3.1, §5.5.1-§5.5.2; `tech-spec.md` C1-C12.

```gherkin
Feature: Append-only invariants across the whole stack
  As a compliance reviewer
  I want every append-only contract to hold under stress
  So that the audit trail is intact
```

```gherkin
@invariant
Scenario: No UPDATE on append-only tables under nominal load
  Given the system is at sustained nominal load on all verbs
  When PostgreSQL audit logs are inspected for one hour
  Then zero `UPDATE` statements are observed on:
       `Node`, `Edge`, `NodeRetirement`, `EdgeRetirement`,
       `Episode`, `EpisodeUpdate`, `Observation`,
       `RecallContextLog`, `TraceObservationLog`,
       `Concept`, `ConceptVersion`, `ConceptSupport`,
       `Commit`, `EmbeddingPublish`, `EmbeddingPublishEvent` [C2, C5, C6, C7, C9, tech-spec §8.7.4]
  And the only UPDATE-grantable targets observed are:
       `TraceObservation`, `Repo`, `ConsolidatorRun`, `PromoterRun`, `RepoEvent`,
       `reranker_model`, `ingest_jobs` [tech-spec §8.7.4 + impl Stage 1.4]
```

```gherkin
@invariant
Scenario: agent_memory_app role cannot UPDATE or DELETE append-only tables
  Given the `agent_memory_app` role is bound to the service connection
  When the role attempts `UPDATE Episode SET outcome='success' WHERE episode_id=<any>`
  Then PostgreSQL rejects the statement with insufficient privilege [impl Stage 1.4]
```

```gherkin
@invariant
Scenario: Tombstone uniqueness prevents double-retirement
  Given `NodeRetirement(node_id=N1, retired_at_sha=sha_A)` already exists
  When a buggy delta job attempts to insert a second `NodeRetirement(node_id=N1, …)`
  Then the unique index on `(node_id)` rejects the insert [arch §5.2.4]
  And the writer surfaces the conflict as `retirement-already-recorded`, not as a row update
```

```gherkin
@invariant
Scenario: Fingerprint determinism across re-ingest
  Given a repo is fully re-ingested at the same SHA
  Then every recomputed Node fingerprint equals the previously stored one byte-for-byte [C1]
  And every recomputed Edge fingerprint equals the previously stored one [C1]
```

```gherkin
@invariant
Scenario: Fingerprint CHECK rejects non-32-byte values
  When a malformed insert sets `Node.fingerprint='\x00\x01'` (2 bytes)
  Then the row-level CHECK rejects it with an `octet_length` violation [impl Stage 1.2]
```

```gherkin
@invariant
Scenario: Closed enum on RepoEvent.kind
  When the operator path attempts to enqueue `RepoEvent(kind='rebase')` (not in the closed set)
  Then the ENUM rejects the insert [arch §5.6, impl Stage 1.2 migration 0006]
  And the only accepted values are `{push, merge, register, manual}`
```

```gherkin
@invariant
Scenario: Concept has no repo_id column
  When the operator runs `\d+ concept` on PostgreSQL
  Then no `repo_id` column exists on `Concept` [C10, G6]
  And `ConceptSupport.repo_id` is the only cross-repo dimension
```

```gherkin
@invariant
Scenario: Node kind enum is closed and there is no concept_attaches Edge kind
  When the operator runs `\d+ node_kind` on PostgreSQL
  Then the ENUM members are exactly `{repo, package, file, class, method, block}` [C4, arch §5.2.1]
  When an INSERT attempts `Node(kind='concept', …)`
  Then the insert is rejected as an ENUM violation
  When the operator runs `\d+ edge_kind` on PostgreSQL
  Then the ENUM members are exactly `{contains, imports, static_calls, observed_calls, extends, implements, reads, writes, renamed_to}` [C4, arch §5.2.2]
  And no `concept_attaches` value is in the ENUM (Concept↔code links are carried by `ConceptSupport` rows, not Edges) [C4, arch §5.5.3]
  When an INSERT attempts `Edge(kind='concept_attaches', …)`
  Then the insert is rejected as an ENUM violation
  And no `renamed_from` value is in the ENUM (the inverse is the derived view `SELECT … WHERE kind='renamed_to' AND dst_node_id=?`) [C3, arch §5.2.2]
```

---

## 15. Feature: Validation rules (rejection scenarios)

> Anchors: `architecture.md` §6.2.2; `tech-spec.md` C15, C23, §8.6.

```gherkin
Feature: Verb-level validation rules
  As a client
  I want invalid inputs to be rejected before any state is written
  So that the append-only logs are never polluted by half-states
```

```gherkin
@invariant
Scenario Outline: mgmt.feedback corrected_action coupling
  When `mgmt.feedback(parent_episode_id=EP1, outcome='<outcome>', corrected_action=<ca>)` is invoked
  Then the response status is `<status>`
  Examples:
    | outcome          | ca       | status                            |
    | human_corrected  | {…}      | 200 (feedback Episode appended)   |
    | human_corrected  | null     | 4xx (corrected_action required)   |
    | success          | null     | 200 (acknowledgement)             |
    | success          | {…}      | 4xx (corrected_action forbidden)  |
    | failure          | {…}      | 4xx (corrected_action forbidden)  |
```

```gherkin
@invariant
Scenario Outline: agent.observe role coverage
  When `agent.observe(…, observation_refs=[{role:'<role>', <id_field>:<id>}])` is invoked
  Then the response status is `<status>`
  Examples:
    | role                       | id_field      | status                                  |
    | node_hit                   | node_id       | 200                                     |
    | edge_hit                   | edge_id       | 200                                     |
    | call_edge_hit              | edge_id       | 200                                     |
    | concept_hit                | concept_id    | 200                                     |
    | degraded_recall_context    | node_id       | 4xx (server-only role) [C23]            |
    | unknown_role               | node_id       | 4xx (closed enum)                       |
```

```gherkin
@invariant
Scenario: mgmt.ingest_spans schema validation
  When a span payload is missing `trace_id`
  Then `mgmt.ingest_spans` returns a 4xx for the offending span
  And other spans in the batch are still ingested (batch-level partial success per arch §6.2.2)
```

```gherkin
@invariant
Scenario: Span lacking code.function AND code.filepath is dropped, not synthesised
  Given a span has neither attribute (risk §9.11 worst case)
  When `mgmt.ingest_spans` processes it
  Then the span is dropped, `span_unresolved_total` is incremented, and **no synthetic Node is created** [tech-spec §8.6]
```

---

## 16. Feature: Cold start and learning-quality observability

> Anchors: `architecture.md` §2.4 (in tech-spec, the §2.4 mirror); `tech-spec.md`
> §8.3 learning-quality SLOs; risk §9.5.

```gherkin
Feature: Learning-quality SLOs are observable from day one
  As an operator
  I want rank-of-correct-node and concept-hit fraction visible on the dashboard
  So that I can verify the "learn and get smarter" commitment holds
```

```gherkin
@happy
Scenario: Rank-of-correct-node @ k=20 is computed from RecallContextLog + Observation
  Given 100 positive-outcome Episodes in the trailing 7-day window each carry a `node_hit` Observation
  When the metric pipeline computes rank-of-correct-node @ k=20
  Then for each Episode the rank is derived by joining `Observation.node_id` to the position of that node in `RecallContextLog.node_ids` [tech-spec §8.3]
  And the **median** rank over the window is exported as `rank_of_correct_node_p50`
  And the target threshold is `≤ 5`
```

```gherkin
@happy
Scenario: Concept-hit fraction @ k=20 over trailing 7 days
  Given the trailing 7 days of `agent.recall` responses
  When the metric pipeline computes concept-hit fraction at k=20
  Then it equals `(concept ids that led to ≥1 positive-outcome Episode within 24h) / (total recall concept ids)` [tech-spec §8.3]
  And the v1 target threshold is `≥ 25 %`
```

```gherkin
@edge
Scenario: Cold repo does not regress the dashboard
  Given repo `R_cold` was registered today and has zero Episodes
  When the metric pipeline runs
  Then `R_cold` does not contribute to the trailing-7-days denominators (no Episodes yet) [risk §9.5]
  And the dashboard does not show `R_cold` in red on the learning-quality SLOs
```

---

## 17. Feature: Security and operational guardrails

> Anchors: `tech-spec.md` §8.5 (transport / authN), risks §9.12 (webhook spoofing).

```gherkin
Feature: Authentication and authorisation guardrails
  As an operator
  I want unauthenticated callers to be rejected before any state is read or written
  So that the audit trail is not contaminated
```

```gherkin
@security
Scenario: Agent Surface rejects calls without mTLS
  When a caller invokes `agent.recall(…)` over plain TLS (no client cert)
  Then the connection is rejected at the TLS handshake [tech-spec §8.5]
  And no `RecallContextLog` row is appended
```

```gherkin
@security
Scenario: Management Surface rejects expired OIDC bearer tokens
  When the operator invokes `mgmt.read.episodes(…)` with an expired token
  Then the verb returns 401 [tech-spec §8.5]
  And no read query is forwarded to PostgreSQL
```

```gherkin
@security
Scenario: Webhook with a forged signature is rejected
  When the git host POSTs a webhook with an HMAC computed over a tampered body
  Then the Webhook Receiver returns 401 and writes no `RepoEvent` row [risk §9.12]
```

```gherkin
@security
Scenario: ingest_jobs UPDATE is constrained to mode/status flips, not row replacement
  Given the `agent_memory_app` role attempts `UPDATE ingest_jobs SET repo_id=<other>` [impl Stage 1.4]
  Then the role's grants permit the UPDATE syntactically
  But the service-side writer never issues such a statement; the only UPDATEs observed in audit logs are
       `status` transitions and `attempt_index` increments
```

---

## 18. Feature: Reranker training cycle and freshness

> Anchors: `architecture.md` §3.6, §4.2 step 3, §7.2; `tech-spec.md` §8.4
> (cross-encoder model class, nightly + on-demand cadence, trailing 90-day
> training window, `reranker_model` registry); risks §9.4 (operator-correction
> poisoning), §9.10 (reranker staleness).

```gherkin
Feature: Reranker training cycle and freshness
  As the platform
  I want labelled Episodes (including synthetic positives from operator
  corrections) to drive a periodic retraining of the recall reranker
  So that recall quality improves as supervision accumulates
```

```gherkin
Background:
  Given a `reranker_model` row exists with `version='v_prev'`, `trained_at=<2 days ago>`
  And the labelled-Episode count at the last training run was 1 000
  And at least one synthetic-positive Episode (from a prior §8 correction) exists in the trailing 90 days
```

```gherkin
@happy @invariant
Scenario: Nightly training cycle publishes a new reranker_model row
  Given the trailing 90-day Episode window contains at least one new positive and one new negative
       since `trained_at` of `v_prev` [tech-spec §8.4]
  When the nightly Reranker Trainer cycle fires
  Then exactly one new `reranker_model` row is written with `version='v_next'`,
       `trained_at≈now`, `metrics_json` non-null, `artifact_uri` resolvable [tech-spec §8.4]
  And the prior `reranker_model(version='v_prev')` row is preserved (the
       registry is UPDATE-grantable per tech-spec §8.7.4 but the writer
       publishes a new row for lifecycle rather than rewriting v_prev)
  And the next `agent.recall` returns `reranker_model_version='v_next'` in its response [arch §6.1.1]
  And the next `RecallContextLog` row carries `reranker_model_version='v_next'` [arch §5.4.1]
```

```gherkin
@happy
Scenario: Synthetic positives are included in the training window
  Given the trailing 90-day labelled-Episode window includes 100 negative Episodes
       and 20 synthetic positives (each carrying `synthesized_from_feedback_episode_id` per C16)
  When the Reranker Trainer cycle fires
  Then the Trainer's run log reports 100 negative pairs and 20 positive pairs
       (synthetic positives count as positives per arch §3.6, tech-spec §8.4 "+ all synthetic positives ever")
  And the published `reranker_model.metrics_json` includes a `positive_count=20` field
```

```gherkin
@edge
Scenario: On-demand retraining fires when labelled-Episode count grows ≥ 5 %
  Given the labelled-Episode count at the last training run was 1 000
  When the labelled-Episode count first crosses 1 050 (≥ 5 % growth)
  Then a Reranker Trainer cycle is enqueued outside the nightly schedule [tech-spec §8.4]
  And on completion a new `reranker_model` row is published per the §18 happy path
  And subsequent recalls reference the new `reranker_model_version`
```

```gherkin
@edge
Scenario: Training window excludes Episodes older than the trailing 90 days
  Given an Episode `EP_old` with `outcome='success', created_at=<100 days ago>`
  And a synthetic-positive Episode `EP_syn_old` with `created_at=<100 days ago>` (still in scope per tech-spec §8.4 "all synthetic positives ever")
  When the Reranker Trainer cycle fires
  Then `EP_old` is NOT in the training set (trailing 90-day window)
  And `EP_syn_old` IS in the training set (synthetic positives are kept all-time)
```

```gherkin
@edge @degraded
Scenario: Reranker staleness propagates to agent.recall (cross-link to §4)
  Given the latest `reranker_model.trained_at` is `<8 days ago>` [risk §9.10]
  When `agent.recall(R1, query, k=20)` is invoked
  Then the response carries `degraded=true, degraded_reason='reranker_model_stale'` [C22, §4 cross-link]
  And `nodes[]`/`concepts[]` are still returned using the stale model (staleness is a quality signal, not an outage)
  And the dashboard's `last_trained_at` metric matches `trained_at` of the latest `reranker_model` row [tech-spec §8.4 / risk §9.10]
```

```gherkin
@edge
Scenario: Reranker model class is the cross-encoder BERT-class family
  Given the latest `reranker_model.metrics_json` carries a `model_class` field
  Then the value is `cross_encoder_bert` and the parameter count `params` is ≤ 200 M [tech-spec §8.4]
  And no `online_learning_step` metric is emitted (online learning is explicitly out per tech-spec §6 non-goal 7)
```

```gherkin
@edge
Scenario: Bad-actor operator correction is bounded by the rate cap downstream
  Given a single operator submits `mgmt.feedback(parent_episode_id=<E_i>, outcome='human_corrected', corrected_action={…})`
       on 50 distinct Episodes within one hour [risk §9.4 trigger]
  When the next Reranker Trainer cycle fires
  Then the Trainer applies the per-operator rate cap defined in tech-spec §9.4 mitigation
  And the bad-actor's induced rank change for any single recall scenario does not exceed the §9.4 threshold
       without ≥ 2 distinct operators having submitted a concurring correction
  And the dashboard surfaces an `operator_correction_rate` counter per operator
```

---

## 19. Cross-references

This document does **not** redefine schemas or signatures. Each Feature cites
the section it exercises. The mapping below is the contract surface this file
asserts QA must cover end-to-end:

| Feature (this doc) | Exercises | Hard constraints |
| --- | --- | --- |
| §1 Cold registration | arch §4.1, §7.1, §6.2.1 | C1, C2, C13, C14 |
| §2 Delta re-index | arch §4.6, §7.4, §5.2.4 | C1, C2, C3, C19 |
| §3 OTel span ingestion | arch §3.3, §5.2.3, tech-spec §8.6 | C8, C18, C19, C22 |
| §4 Agent recall | arch §4.2, §6.1.1, §6.3, §7.5, §7.6 | C7, C13, C22 |
| §5 Agent observe | arch §4.3, §6.1.2, §7.2 | C5, C6, C14, C22, C23, C24 |
| §6 Call-chain expansion | arch §4.5, §6.1.3 | C13, C21, C22 |
| §7 Summarize | arch §6.1.4, §6.3 | C13, C22 |
| §8 Operator correction | arch §4.4, §7.3, §1.3 G7 | C6, C15, C16, C17 |
| §9 Consolidation tick | arch §4.7, §7.7 | C9, C10, C12 |
| §10 Concept promotion | arch §7.8, tech-spec §9.6a | C9, C11, C12, C22 |
| §11 Cross-repo Concepts | arch §3.8, §8.4 | C10 |
| §12 Operator inspection | arch §4.8, §6.2.3 | C7, C13 |
| §13 Degraded contracts | arch §6.3, §7.5, §7.6, §8.2 | C22, C23, C24 |
| §14 Append-only invariants | arch §1.3 G2-G7, §5.2.1-§5.2.4, §5.3.1, §5.5.1 | C1-C12 |
| §15 Validation rules | arch §6.2.2 | C15, C18, C23 |
| §16 Learning-quality SLOs | tech-spec §8.3 | (provisional pins) |
| §17 Security guardrails | tech-spec §8.5, risk §9.12 | — |
| §18 Reranker training | arch §3.6, tech-spec §8.4 | C22 (cross-link to §13) |

---

## 20. Open considerations for the next iteration

These are not new constraints — they are notes for the next iteration of any
sibling plan doc that might tighten a scenario.

- **Per-repo HMAC rotation runbook.** Risk §9.12 defers secret rotation
  procedure to operations. When that runbook lands, §17 should gain a
  scenario that verifies `mgmt.register` accepts a rotated secret and the
  Webhook Receiver atomically switches at the rotation epoch.
- **Per-operator rate cap on `mgmt.feedback`.** Tech-spec §9.4 mitigation
  mentions a per-operator cap. Until that cap is pinned (§8.4 follow-up),
  §8 has no scenario for "operator X's 11th correction in 60 s is rejected
  / queued". Add when pinned.
- **Numeric calibration of §8.3 SLOs.** The §16 thresholds (rank ≤ 5,
  hit-fraction ≥ 25 %) are explicitly provisional in `tech-spec.md` §8.3.
  Re-anchor §16 to the post-calibration values when iter-N pins them.
