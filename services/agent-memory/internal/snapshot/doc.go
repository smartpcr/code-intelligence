// Package snapshot implements the `mgmt.snapshot` write verb's
// enqueue half (implementation-plan.md Stage 7.4, architecture.md
// §6.2.1 line 740).
//
// The verb's responsibility is to FORCE re-embedding of every
// Method / Block Node and every promoted Concept attributed to
// a given repo, using the CURRENTLY active embedding-model
// version (risk §9.6). The actual embed + Qdrant upsert work
// is owned by the existing §9.6a publisher pipeline
// (`internal/embedding/publisher.go`) which the Repo Indexer
// and the Concept Promoter already drive. Snapshot's only job
// is to durably append one fresh `embedding_publish` row +
// `queued` event per target, carrying:
//
//   - the CURRENT `embedding_model_version` (so a model bump
//     is the trigger for the operator to call snapshot);
//   - the source `Content` and `SignatureOnly` flag SNAPSHOTTED
//     from the prior `published` publish's queued event (Node
//     rows do not store source bytes per G5; the prior queued
//     event's `details_json` is the only persisted body);
//   - a `supersedes_publish_id` discriminator pointing at the
//     prior publish row, so the publisher's
//     "transition-to-published" hook can append a `superseded`
//     event for the prior atomically with the new `published`
//     event (closing the brief window where recall would see
//     two `published` rows for the same Node);
//   - a `snapshot_id` discriminator so re-running `mgmt.snapshot`
//     while a prior snapshot is still in flight does not
//     enqueue a second replacement for the same Node (the
//     enumeration query filters out targets whose prior publish
//     already has a non-terminal snapshot replacement queued).
//
// What `mgmt.snapshot` is NOT
// ---------------------------
//   - It does NOT mutate any existing row. Per architecture
//     G4/G5 the publish log is append-only; snapshot writes
//     fresh rows and emits a `superseded` event for the prior
//     via the publisher hook.
//   - It does NOT re-parse source code or reach into git. The
//     content body is the one the indexer ALREADY embedded
//     (stored in `embedding_publish_event.details_json`); a
//     snapshot is a re-embed, not a re-index.
//   - It does NOT block on the embedding/Qdrant chain. The
//     verb returns `202 Accepted` once the queued rows are
//     persisted; the existing flusher/promoter loops drain
//     the queue.
//
// Cross-package contract
// ----------------------
// The `Service` type exposed by this package is the production
// implementation of the `mgmtapi.Snapshotter` interface
// (defined alongside the HTTP handler in
// `internal/mgmtapi/snapshot.go`). The interface boundary lets
// the handler unit tests use a stub Snapshotter without going
// through PostgreSQL; the binary wires the real `*Service`.
package snapshot
