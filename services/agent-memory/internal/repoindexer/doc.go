// Package repoindexer hosts the Stage 3.1 Repo Indexer worker
// scaffold (per `docs/stories/code-intelligence-AGENT-MEMORY/
// implementation-plan.md` §3.1).
//
// Responsibilities encapsulated by this package:
//
//   - `Worker` -- a polling loop that consumes `ingest_jobs` rows
//     (mode ∈ {full, delta, manual}) using `SELECT ... FOR UPDATE
//     SKIP LOCKED`, drives the row through the
//     pending → claimed → running → done|failed state machine, and
//     dispatches to the mode-specific handler. Stage 3.1 ships only
//     the `full` mode handler; `delta` and `manual` are wired as
//     `errModeNotImplemented` placeholders so the dispatcher
//     compiles and Stages 3.4/3.5 can fill them in without touching
//     the polling loop.
//
//   - `Materializer` / `Workspace` -- shallow-clones the configured
//     git host URL at the requested SHA into a temp dir and exposes
//     a deterministic tree-walker. A real `GitMaterializer` shells
//     out to the `git` binary (`init` + `fetch --depth=1 <sha>` +
//     `checkout FETCH_HEAD`); tests inject an `InMemoryMaterializer`
//     that synthesises a fake workspace without touching disk so
//     the small-fixture and idempotent-re-ingest scenarios run
//     without git installed.
//
//   - The full-mode handler -- walks every source file, derives the
//     Repo → Package → File ancestry, writes Nodes (kind ∈ {repo,
//     package, file}) and `contains` Edges through `graphwriter`,
//     and delegates per-file Class/Method/Block emission to the
//     pluggable `ASTEmitter` (Stage 3.2's surface). The default
//     `NoopASTEmitter` records nothing -- Stage 3.2 will swap in
//     the tree-sitter-backed dispatcher.
//
//   - `EventPublisher` -- thin wrapper over PostgreSQL
//     `pg_notify(channel, payload)` on the `agent_memory_events`
//     channel. The full-mode handler publishes a `repo.registered`
//     event the FIRST time a `done` ingest_jobs row appears for a
//     given (repo_id, mode='full', from_sha, to_sha) tuple AND a
//     `repo.full_ingested` event on every successful full ingest.
//     Using "first done row" as the predicate (instead of
//     `EnsureCommit.Inserted`) means a transient post-EnsureCommit
//     failure that succeeds on retry STILL fires `repo.registered`
//     -- the prior implementation suppressed the event because the
//     commit row already existed and EnsureCommit returned
//     Inserted=false on the second attempt. Downstream stages
//     (Embedding Publisher, Concept Promoter) `LISTEN` on the same
//     channel. The two events are delivered ATOMICALLY with the
//     `ingest_jobs.status='done'` UPDATE via a single transaction
//     that issues both `pg_notify(...)` calls and the status
//     UPDATE -- PostgreSQL queues NOTIFY payloads in the issuing
//     tx and only delivers them on commit, so subscribers can
//     never see "done with no event" or "event for a job that
//     never reached done". A `nil` Publisher is rejected at
//     `NewWorker` construction time. Transient publish failures
//     re-queue the row to `pending` (capped by
//     `WorkerOptions.MaxAttempts`) so a flaky NOTIFY backend
//     does not turn into terminal-failed jobs operators must
//     re-enqueue by hand.
//
//   - `Pool` -- spawns `cfg.Workers` (default 4 per tech-spec §8.3
//     "200 k LOC ≤ 30 min") instances of `Worker.Run`, each polling
//     the queue independently. `SELECT ... FOR UPDATE SKIP LOCKED`
//     is the load-bearing claim-exclusivity primitive; the pool
//     wrapper itself is intentionally trivial.
//
// What this package does NOT do
// -----------------------------
//
//   - It does not parse ASTs. The Stage 3.2 dispatcher
//     (tree-sitter, language grammars, Block subdivision) plugs
//     into `ASTEmitter`. Stage 3.1 keeps the integration boundary
//     clean: the worker calls `Emitter.EmitFile(...)` once per
//     File Node and otherwise has no knowledge of language
//     semantics.
//
//   - It does not write embeddings. Stage 3.3 wires
//     `internal/embedding/publisher.go` into the AST emitter's
//     Method/Block insert callback; the worker's only job is to
//     produce the File-level ancestry the publisher needs.
//
//   - It does not implement `delta` or `manual` mode handlers.
//     Those are Stages 3.4 / 3.5 and ship as
//     `errModeNotImplemented` placeholders here so the dispatcher
//     surface is in place.
package repoindexer
