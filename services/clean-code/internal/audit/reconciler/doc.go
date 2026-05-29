// Package reconciler is the Stage 9.2 Audit WAL Reconciler:
// a REPLAY-ONLY worker that reads signed [wal.AuditFrame]
// records from `data/wal/audit/` and re-inserts MISSING rows
// into the three Audit tables (architecture Sec 7.10 /
// tech-spec Sec 4.13 / implementation-plan Stage 9.2).
//
// # Replay-only contract
//
// The reconciler honours four invariants documented at the
// brief-level (Stage 9.2) and enforced in the implementation:
//
//  1. NEVER inserts a row whose `(table, row_pk)` already
//     exists. Implementation: every replay uses
//     `INSERT INTO <audit_table> (...) VALUES (...)
//     ON CONFLICT (<pk_column>) DO NOTHING`. PostgreSQL
//     returns `rows_affected = 0` on the conflict path
//     and `1` on the insert path; the reconciler classifies
//     the Outcome accordingly.
//  2. NEVER deletes a row. The reconciler emits no DELETE
//     statement. The `clean_code_wal_reconciler` PostgreSQL
//     role is `REVOKE`d UPDATE/DELETE on the three Audit
//     tables at the migration level (0004_roles.up.sql); the
//     reconciler code path simply cannot reach an UPDATE /
//     DELETE statement.
//  3. NEVER modifies a non-Audit table. The reconciler
//     dispatches on [wal.AuditFrame.Table] via an explicit
//     switch over [wal.TableEvaluationRun],
//     [wal.TableEvaluationVerdict], [wal.TableFinding]; any
//     other value short-circuits with [ErrUnknownTable]
//     before any SQL is issued. The `clean_code_wal_reconciler`
//     role has NO grant on the Catalog / Measurement /
//     Policy / Refactor tables, so a bug here would also
//     fail at the PostgreSQL grant layer.
//  4. PRESERVES `evaluation_run.caller`. The reconciler
//     parses the `caller` value from `row_json` and passes
//     it verbatim as the SQL INSERT bind variable. There is
//     no literal string constant for any caller value in
//     this package. A frame minted with `caller='eval_gate'`
//     replays as `caller='eval_gate'`; a frame minted with
//     `caller='batch_refresh'` replays as
//     `caller='batch_refresh'`. The reconciler does NOT
//     substitute itself as the caller (implementation-plan
//     Stage 9.2 bullet 3).
//
// # Signature verification
//
// Every frame is verified before replay via the [Verifier]
// interface. The interface is intentionally narrower than
// `policy/keys.Manager.Verify`:
//
//   - The reconciler needs HISTORICAL key resolution -- a
//     frame signed yesterday by a now-retired key MUST still
//     verify on replay. The publish-time `Manager.Verify`
//     path rejects retired keys on purpose; the composition
//     root wires a separate adapter that consults
//     `clean_code.policy_signing_keys` directly (the
//     historical-keys adapter ships alongside the production
//     binary integration in a follow-up workstream).
//   - The [Verifier] error contract distinguishes:
//
//     * [ErrSignatureInvalid] / [ErrSigningKeyUnknown] --
//       the frame is durably broken; the reconciler counts
//       it in [Stats.SkippedBadSig] and continues.
//     * Any other error -- treated as transient infrastructure
//       failure (KMS unreachable, DB outage); the reconciler
//       aborts the whole [Reconciler.Run] so the operator can
//       address the root cause before retry. The brief's
//       "replay-only" promise depends on a healthy verifier;
//       silently skipping every frame on a verifier outage
//       would erase the durability guarantee Stage 9.1 set up.
//
// # Phased replay (FK ordering)
//
// The Audit tables form a small FK graph:
//
//	evaluation_run        (pk: evaluation_run_id)
//	evaluation_verdict    FK evaluation_run_id  -> evaluation_run
//	finding               FK evaluation_run_id  -> evaluation_run
//
// The writer's per-tx batch always stages `evaluation_run`
// BEFORE `evaluation_verdict` and `finding` (see
// `rule_engine.appendEvaluationInTx`); the wal package
// preserves intra-tx order on disk. The reconciler still
// replays in two passes:
//
//  1. Every `evaluation_run` frame, in WAL order.
//  2. Every `evaluation_verdict` + `finding` frame, in
//     WAL order.
//
// The two-pass layout costs one extra slice iteration and
// makes the FK invariant resilient to a corrupted partition
// where a `finding` frame somehow appears before its
// owning `evaluation_run` frame.
//
// # Stats
//
// [Reconciler.Run] returns a [Stats] value tallying per-table
// outcomes:
//
//   - [Stats.Replayed] -- frames that produced a fresh
//     INSERT (RowsAffected = 1).
//   - [Stats.SkippedExisting] -- frames whose row already
//     existed (RowsAffected = 0 under ON CONFLICT DO NOTHING).
//   - [Stats.SkippedBadSig] -- frames whose signature did
//     not verify ([ErrSignatureInvalid] /
//     [ErrSigningKeyUnknown]).
//   - [Stats.SkippedBadShape] -- frames whose PRE-signature
//     structural validation failed (`wal.AuditFrame.Validate`
//     or `SigningPayload()`). POST-signature mismatches
//     (decode rejection, RowPK mismatch) are LOUD aborts
//     ([ErrRowPKMismatch]) -- they DO NOT count here.
//   - [Stats.Warnings] -- non-fatal sentinels surfaced by
//     `wal.ReadAll`: a trailing partial frame
//     ([wal.ErrTrailingPartialFrame]) or an oversized frame
//     ([wal.ErrFrameSizeExceeded]). The reconciler still
//     replays every complete frame that preceded the
//     warning.
package reconciler
