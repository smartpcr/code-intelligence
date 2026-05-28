// Package wal implements the per-process Audit Write-Ahead Log
// (architecture Sec 7.1 / tech-spec Sec 4.13).
//
// # Scope
//
// The WAL is scoped EXCLUSIVELY to the Audit sub-store. Only writes
// to the three audit tables route through it:
//
//   - `clean_code.evaluation_run`
//   - `clean_code.evaluation_verdict`
//   - `clean_code.finding`
//
// Catalog, Measurement, Policy, and Refactor writes do NOT touch
// this package. Conformance test `test/conformance/wal_scope_test.go`
// asserts the import-graph contract -- the only packages that may
// import `internal/audit/wal` are `internal/evaluator` (for the
// gate's degraded short-circuit writer) and `internal/rule_engine`
// (for the engine's happy-path writer + the post-scan batch
// refresh worker).
//
// # Frame format
//
// Each WAL frame is a serialised [AuditFrame] appended as one
// newline-delimited JSON record to a per-partition file under
// `data/wal/audit/`. The partition key is the UTC date of the
// frame's `written_at` timestamp (`YYYY-MM-DD.wal`). Within a
// partition file, frames appear in the order they were appended.
// The reconciler (Stage 9.2) scans partitions in filename order
// and replays missing rows by `(table, row_pk)` lookup.
//
// # Atomicity contract
//
// The writer follows the architecture's "WAL fsync before SQL
// commit" ordering. Four states are possible and ALL are
// resolved safely:
//
//  1. Caller opens a PostgreSQL `*sql.Tx` and runs its `INSERT`
//     statements against the three audit tables.
//  2. Caller stages an [AuditFrame] per inserted row via
//     [TxBatch.Stage] -- this is in-memory only, no disk I/O yet.
//  3. Caller calls [TxBatch.Commit] which appends all staged
//     frames to the partition file and `fsync`s.
//  4. If [TxBatch.Commit] returns an error, the caller MUST
//     rollback the SQL transaction. The frame bytes MAY still
//     be readable on disk (a `write(2)` that the kernel
//     accepted is visible to concurrent readers even when the
//     subsequent `fsync(2)` fails). The Stage 9.2 reconciler
//     reconciles this state idempotently: it sees the
//     "speculative" frame, observes that `(table, row_pk)` is
//     absent from PostgreSQL, and replays the INSERT (the
//     SQL rollback in step 1 means the row does not yet exist,
//     so the replay creates it cleanly).
//  5. If [TxBatch.Commit] returns nil and the subsequent
//     `tx.Commit()` fails, the frame remains on disk and the
//     reconciler replays the missing row on the next service
//     start. This is the intentional ahead-of-DB durability
//     window.
//
// Anywhere the closure returns an error before step 3, the
// staged frames are discarded by [TxBatch.Cancel] (a `defer`
// guard on the call site). The reconciler never sees them.
//
// The writer DOES NOT attempt a post-fsync truncate-back of
// the partition file: a second writer (sibling process,
// restart of this process) can have appended its own frames
// past the failure point, and truncating to the pre-write
// size would wipe them. The reconciler is the authority for
// "frame on disk but SQL row absent" reconciliation; see
// [appendAndSync] in `writer.go` for the in-code rationale.
//
// # Signature
//
// Each frame carries a signature over a canonical-bytes
// projection of every field except `signature` itself:
//
//	"audit-wal-v1\n" + canonical JSON
//	  ({frame_id, table, op, row_pk, row_json,
//	    written_at, signing_key_id})
//
// The domain prefix `"audit-wal-v1\n"` provides domain separation
// from other signatures the policy/keys.Manager produces. The
// reconciler verifies the signature against the recorded
// `signing_key_id` using its own lower-level key resolver --
// historical frames must verify even after key rotation, so the
// reconciler intentionally does NOT use the publish-time
// "currently active" Verify path.
//
// # Concurrency
//
// [Writer] is safe for concurrent use across goroutines: a
// per-writer mutex serialises every partition-file append so
// frames never interleave. [TxBatch] is NOT safe for concurrent
// use -- each batch belongs to exactly one transaction and is
// produced/consumed by one goroutine.
//
// Multi-process WAL sharing is OUT OF SCOPE in v1. The Stage 9.1
// brief targets per-process WAL files and the Stage 9.2
// reconciler reads only its own process's partition tree. A v2
// multi-replica deployment can shard the partition directory
// per replica id without changing the frame format.
package wal
