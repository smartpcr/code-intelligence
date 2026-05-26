# Iter notes — Stage 4.1 iter 3 (per-verb durable idempotency key + Finalize same-terminal contract + interlock tests)

## Prior feedback resolution

Iter 2 evaluator (score 84, iterate) raised four items. All four are ADDRESSED this iter.

1. **ADDRESSED — Open-questions hard gate.** The two iter-2 open questions (`Sticky-failed retry-window`, `Running-status race surface`) are explicitly DEFERRED below in the `Decisions deferred this iter` section. They are NOT re-surfaced as open questions in this iter.
2. **ADDRESSED — Per-verb durable uniqueness.** Migration 0009 rewritten end-to-end: drops the iter-2 `scan_run_payload_hash_kind_uniq` index, adds a `verb text` column + CHECK constraint pinning `verb IS NULL ⇔ payload_hash IS NULL`, creates the new partial unique index `scan_run_payload_hash_verb_uniq` on `(verb, payload_hash) WHERE payload_hash IS NOT NULL`. `PGExternalScanRunStore` adds closed-set verb validation + a verb→kind matrix check. The in-memory `webhook.ScanRunRepository` keys on `(verb, payload_hash)`. New test `TestInMemoryScanRunRepository_OpenExternal_DifferentVerbs_SamePayload_GetIndependentRuns` pins the invariant that two verbs sharing the same kind AND payload_hash receive INDEPENDENT scan_run_ids.
3. **ADDRESSED — Config interlock + mount wiring tests.** `internal/config/config_test.go` gains 5 new tests (`TestExternalIngestWebhook_AllThreeVarsSet_AcceptsAndRoundTrips`, `_EnableWithoutHMACSecret_Rejected`, `_EnableWithoutSigningKeyID_Rejected`, `_SigningKeyIDWithoutEnable_Rejected`, `_UnsetByDefault`). `cmd/clean-code-metric-ingestor/main_test.go` gains 6 new `mountIngestRouter` tests including `_Enabled_MountsRouterAtCanonicalPath` which asserts a POST to `/v1/ingest/churn` without a valid signature returns 401 NOT 404 (proves Router is mounted AND the HMAC verifier sits in front of the DB roundtrip).
4. **ADDRESSED — `PGScanRunRepository.Finalize` same-terminal contract.** The adapter is rewritten to call the new `LookupExternalScanRunStatusByID` on `ErrConcurrentFinalize`, return nil when the existing terminal status matches the requested one, and wrap-error when they mismatch or the row is missing. Three new adapter-layer tests (`_ConcurrentSameTerminal_ReturnsNil`, `_ConcurrentDifferentTerminal_ReturnsError`, `_ConcurrentRowMissing_ReturnsError`) plus two in-memory tests (`_Finalize_SameTerminal_ReturnsNil`, `_Finalize_DifferentTerminal_ReturnsError`) pin all branches.

## Files touched this iter

### REWRITTEN
- `services/clean-code/migrations/0009_scan_run_payload_hash_unique.up.sql` — new shape: DROP iter-2 index → ALTER TABLE ADD COLUMN verb text → defensive backfill `verb = '__legacy_' || kind` for any iter-2-applied dev rows → ADD CONSTRAINT scan_run_verb_payload_hash_consistent → CREATE UNIQUE INDEX CONCURRENTLY scan_run_payload_hash_verb_uniq on `(verb, payload_hash) WHERE payload_hash IS NOT NULL`. Still requires `psql -f` (autocommit).
- `services/clean-code/migrations/0009_scan_run_payload_hash_unique.down.sql` — DROP INDEX → DROP CONSTRAINT → DROP COLUMN verb, in that order.
- `services/clean-code/internal/ingest/webhook/pg_scan_run_repository.go` — `PGScanRunOpener` interface widened with `LookupExternalScanRunStatusByID`. `Finalize` rewritten to SELECT-on-ErrConcurrentFinalize and honour the same-terminal-double-finalize interface contract.

### MAJOR UPDATES
- `services/clean-code/internal/metric_ingestor/pg_external_scan_run_store.go` — added `Verb` field on `OpenExternalScanRunRequest`; added `canonicalExternalVerbs` closed-set map + `canonicalVerbToKind` matrix map + `ErrExternalScanRunUnsupportedVerb` sentinel; `Validate()` rejects bad verb / verb-kind mismatch BEFORE any DB roundtrip; INSERT SQL writes the `verb` column with `ON CONFLICT (verb, payload_hash)`; `lookupByPayloadHash` switched to filter on verb; new `LookupExternalScanRunStatusByID` method.
- `services/clean-code/internal/metric_ingestor/pg_external_scan_run_store_test.go` — 4 existing tests pass `Verb: "churn"`; 4 new tests: `_BadVerb_NoDBRoundTrip`, `_VerbKindMismatch_NoDBRoundTrip`, `_LookupExternalScanRunStatusByID_HappyPath`, `_NotFound`. All 11 tests pass.
- `services/clean-code/internal/ingest/webhook/scan_run_repository.go` — `ScanRunRepositoryRequest.Verb` field doc updated to declare it as the idempotency dimension; `scanRunMemKey` field renamed `kind → verb`; in-memory store keys on `(verb, payload_hash)`; interface doc updated to declare the same-terminal-double-finalize contract.
- `services/clean-code/internal/ingest/webhook/scan_run_repository_test.go` — comments updated `(kind, payload_hash) → (verb, payload_hash)`; new test `_DifferentVerbs_SamePayload_GetIndependentRuns` (closes iter-2 item #2 at the in-memory layer); new same-terminal / different-terminal Finalize tests.
- `services/clean-code/internal/ingest/webhook/pg_scan_run_repository_test.go` — `fakePGScanRunOpener` extended with `LookupExternalScanRunStatusByID` method + `lookupStatus`/`lookupFound`/`lookupErr`/`lookupCalls`/`lastLookupID` capturing fields; existing `_OpenExternal_TranslatesShapes` asserts `Verb: "churn"`; 3 new tests for the three Finalize branches.
- `services/clean-code/internal/config/config_test.go` — added `EnvEnableExternalIngestWebhook` + `EnvWebhookSigningKeyID` to `clearCleanCodeEnv`; 5 new external-ingest interlock tests.
- `services/clean-code/cmd/clean-code-metric-ingestor/main_test.go` — 6 new `mountIngestRouter` tests.
- `services/clean-code/CHANGELOG.md` — iter-3 entry prepended, iter-2 entry footnote-marked as superseded.
- `services/clean-code/docs/runbook.md` — appended `### Iter 3` section: rewritten migration shape, per-verb closed set + verb-kind matrix table, Finalize same-terminal contract, composition root tests.
- `services/clean-code/docs/rollout.md` — prepended `## Stage 4.1 iter 3` pre-roll checklist with the new index name (`scan_run_payload_hash_verb_uniq`) + the new dedup precheck SQL keyed on `(verb, payload_hash)` + per-verb verification script.

## Decisions made this iter

- **`(verb, payload_hash)` is the right uniqueness key, not `(kind, payload_hash)`.** The verbs `coverage` / `test_balance` are both `kind = external_single`; `churn` / `defects` are both `kind = external_per_row`. Keying on kind collapsed two distinct per-pipeline tracks onto one row. The verb column is the natural per-pipeline distinction. Adding a column (rather than reusing `kind`) preserves the existing kind enum's CHECK constraint semantics and keeps the foundation-tier rows (`atomic` / `aggregated_repo`) unaffected.
- **Verb-kind consistency at `Validate()`.** A new map `canonicalVerbToKind` pins the matrix (`coverage,test_balance → external_single`; `churn,defects → external_per_row`) and `OpenExternalScanRunRequest.Validate()` rejects a mismatch BEFORE any DB roundtrip. This closes a wiring-bug surface where a caller could silently write a verb row under the wrong kind enum.
- **Defensive `__legacy_` backfill in the up migration.** Any dev DB that applied iter-2's `(kind, payload_hash)` shape has rows with `payload_hash NOT NULL` and `verb IS NULL`, which would violate the new CHECK constraint. The backfill `UPDATE scan_run SET verb = '__legacy_' || kind WHERE payload_hash IS NOT NULL AND verb IS NULL` makes the migration idempotent against those rows. Production has zero such rows (Stage 4.1 not yet deployed).
- **`PGScanRunOpener` widened from 2 → 3 methods.** Adding `LookupExternalScanRunStatusByID` to the interface (rather than passing the underlying `*sql.DB` through to the adapter) keeps the adapter's tests at unit cost. The `fakePGScanRunOpener` in `pg_scan_run_repository_test.go` was extended in lockstep so the test file still avoids sqlmock.
- **Finalize same-terminal contract — SELECT on the ErrConcurrentFinalize path, not on every Finalize.** The common case (single replica, no race) hits zero extra roundtrips. Only the race surfaces an extra SELECT, which is the correct cost trade.
- **`Setenv("", "")` style for clearing.** `internal/config/config_test.go` adds the two new env vars to `clearCleanCodeEnv` so a stale value from a prior subtest cannot leak in. The `TestExternalIngestWebhook_UnsetByDefault` test pins this.

## Dead ends tried this iter

- Initial attempt at `TestPGScanRunRepository_Finalize_ConcurrentSameTerminal_ReturnsNil` tried to use sqlmock to simulate the UPDATE-zero-rows-affected case. That works for the PG store unit test (already in place), but at the ADAPTER layer the easier seam is the `PGScanRunOpener` interface fake — the adapter's job is to translate the store's `ErrConcurrentFinalize` into the interface contract, so the adapter test only needs to assert behaviour given the sentinel, not the underlying SQL. Pivoted to extending `fakePGScanRunOpener` with the new method.

## Decisions deferred this iter

The following two questions from iter-2 are explicitly DEFERRED to future stages; they are NOT open for operator decision in this iter:

- **Sticky-failed retry-window** — DEFERRED to Stage 4.6 or later (an operator-feedback-driven stage). Current iter-3 behaviour: failed `scan_run` rows are not recycled; publisher MUST change the canonical body (e.g. bump a request-nonce field) to retry. This matches GitHub webhook conventions and preserves the audit chain. Rationale for deferring: no current operator pressure for a retry-window; the simple "publisher mutates body" path is the canonical fix and the audit chain wins over the convenience.
- **Running-status race surface** — DEFERRED to a future Router stage (no specific stage assigned). Current iter-3 behaviour: a sibling-replica's `running` row replays as `replayed=true` with the running `scan_run_id`; the publisher polls `GET /v1/scan_runs/{id}` for the terminal verdict. Rationale for deferring: the polling pattern is established by the existing scan_run lifecycle; a `409 + Retry-After` would require coordinating with the publisher's retry library, which is a larger contract change than this stage's scope.

## What's still left

- All four iter-2 evaluator items addressed.
- Full suite is green except the pre-existing `TestMgmtWriter_RetractSample_UnknownSampleSentinelFromDispatcherReturns404` in `internal/management/` (out of scope; pre-exists on baseline branch).
- Per-verb mounts for `coverage` / `test_balance` / `defects` land in Stages 4.2 / 4.3 / 4.5 — they will register new `VerbHandler`s implementing `SHABinding` + `ExtractMetadata` + `Verb()` against the same Router seam, using the new per-verb `(verb, payload_hash)` durable key from this iter.
