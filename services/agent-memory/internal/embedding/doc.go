// Package embedding implements the Stage 3.3 EmbeddingIndex
// writer per implementation-plan.md §3.3 and tech-spec.md
// §9.6a.  It wraps a configurable embedding-model client and a
// Qdrant upsert call into the strict §9.6a append-only write
// protocol:
//
//  1. The architecture-owned Node row (Method or Block) is
//     already committed by the AST emitter's Stage 3.2
//     transaction; this publisher does NOT insert Node rows.
//  2. Insert an `embedding_publish` row whose `node_id`
//     foreign-keys to the Node, carrying
//     `embedding_model_version` (risk §9.6) and the planned
//     `qdrant_point_id`.
//  3. Insert `embedding_publish_event(queued)`.
//  4. Call the embedder for the vector body.
//  5. Upsert the vector to Qdrant with the same `point_id`.
//     On success insert `embedding_publish_event(vector_written)`;
//     on failure insert `embedding_publish_event(failed)` and
//     surface the error so a background flusher can retry.
//  6. Read-after-write confirm via a Qdrant point fetch.
//  7. Insert the final `embedding_publish_event(published)`.
//
// Failure mode discipline
// -----------------------
// Every transition the publisher attempts is durably recorded
// in PostgreSQL.  Embedder / Qdrant failures are wrapped in
// `ErrAttemptFailed` so callers (the Stage 3.2 dispatcher) can
// distinguish "transient failure was recorded as `failed`,
// safe to continue ingest" from "PostgreSQL insert blew up,
// abort the job".  This matches the tech-spec §9.6a residual:
// "A long Qdrant outage during a heavy delta ingest leaves a
// backlog of `queued` / `failed` `EmbeddingPublish` rows;
// recall degrades smoothly because the GraphReader filter
// excludes them".
//
// Boundaries
// ----------
//   - The package depends only on `database/sql` (via lib/pq)
//     and on small `Embedder` / `Qdrant` interfaces so unit
//     tests can drive the §9.6a state machine without standing
//     up a real model service or a real Qdrant container.
//   - It does NOT import `internal/repoindexer/ast` (the
//     ast→embedding dependency direction is the only correct
//     one). The one-file adapter in `astadapter.go` lets the
//     Stage 3.2 dispatcher inject a `*Publisher` via the
//     `ast.NodeEmbeddingPublisher` interface without leaking
//     embedding's types up into the ast package.
//   - It does NOT own Concept (`concept_version_id`) publishes;
//     the Concept Promoter (Stage 6.2) owns that path. The
//     `embedding_publish.exactly_one_target_chk` constraint
//     makes both shapes addressable in the same table while
//     this publisher stays focused on the Method/Block side.
//
// Production wiring
// -----------------
// The Stage 3.1 full-mode handler (`repoindexer.Worker`) calls
// the publisher transitively through the Stage 3.2 AST
// dispatcher's `WithEmbeddingPublisher` option.  The
// composition pattern (proved end-to-end in
// `wiring_integration_test.go` and matching the production
// wiring in `cmd/repoindexer/main.go`) is:
//
//	embedder := openai.NewEmbedder(cfg.OpenAI)         // or stub in dev
//	qdrant   := embedding.NewHTTPQdrant(cfg.QdrantURL) // single-arg; collection
//	                                                   // is per-publish via
//	                                                   // CollectionFor(kind).  Wrap
//	                                                   // the inner http.Client with
//	                                                   // a header-injecting
//	                                                   // RoundTripper to add an API key.
//	pub      := embedding.NewPublisher(db, embedder, qdrant)
//	disp     := ast.NewDispatcher(writer,
//	                ast.WithEmbeddingPublisher(embedding.AsASTPublisher(pub)))
//	worker   := repoindexer.NewWorker(db, writer, repoindexer.WorkerOptions{
//	                Materializer: mat,
//	                Emitter:      disp,
//	                Publisher:    eventPublisher,
//	            })
//	// Background §9.6a retry scanner.  Uses the queued-event
//	// snapshot resolver so retries don't need to re-materialize
//	// source bytes from the AST emitter.
//	flusher  := embedding.NewFlusher(db, pub,
//	                embedding.NewPublishEventContentResolver(db))
//	go flusher.Run(ctx, 30*time.Second)
//
// With this wiring every Method and Block the dispatcher emits
// triggers a §9.6a publish through the same write protocol the
// integration tests assert.  The dispatcher's two-bucket error
// policy (recorded-failure ⇒ warn-and-continue, all other
// errors ⇒ propagate) keeps a flaky Qdrant from killing the
// whole ingest while still surfacing structural bugs.
package embedding
