# Iter notes — Stage 4.1 iter 2 (durable scan_run idempotency + Router mount)

## Files touched this iter

### NEW
- `services/clean-code/migrations/0009_scan_run_payload_hash_unique.up.sql` — partial unique index `scan_run_payload_hash_kind_uniq` on `(kind, payload_hash) WHERE payload_hash IS NOT NULL`, `CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS`.
- `services/clean-code/migrations/0009_scan_run_payload_hash_unique.down.sql` — `DROP INDEX CONCURRENTLY IF EXISTS`.
- `services/clean-code/internal/metric_ingestor/pg_external_scan_run_store.go` — `PGExternalScanRunStore` with `OpenExternalScanRun` (INSERT ON CONFLICT DO NOTHING RETURNING → SELECT on conflict), `LookupExternalScanRunByPayloadHash`, `FinalizeExternalScanRun` (rowsAffected=0 → `ErrConcurrentFinalize`). Sentinels validate kind closed-set + payload_hash length.
- `services/clean-code/internal/metric_ingestor/pg_external_scan_run_store_test.go` — 7 sqlmock tests.
- `services/clean-code/internal/ingest/webhook/scan_run_repository.go` — `ScanRunRepository` interface + concurrent-safe `InMemoryScanRunRepository` (dev fallback / repo replay test scaffold). Status constants `succeeded|failed`. Closed-set guards in Open/Finalize.
- `services/clean-code/internal/ingest/webhook/scan_run_repository_test.go` — 10 unit tests including a concurrent-claim collapse case.
- `services/clean-code/internal/ingest/webhook/pg_scan_run_repository.go` — production adapter from `metric_ingestor.PGExternalScanRunStore` onto `webhook.ScanRunRepository`. Lives in webhook package so Router never imports metric_ingestor.
- `services/clean-code/internal/ingest/webhook/pg_scan_run_repository_test.go` — 5 tests: shape translation, AlreadyExisted propagation, status mapping, unknown-status guard, nil-store panic.

### UPDATED
- `services/clean-code/internal/ingest/webhook/router.go` — added `scanRunRepo` + `now` fields; `ServeHTTP` now calls `ExtractMetadata` → `ScanRunRepository.OpenExternal` BEFORE dispatch; on `AlreadyExisted=true` emits durable replay envelope; on fresh open runs handler then `Finalize(succeeded|failed)` then commits in-process cache. New helpers `emitDurableReplay` + `commitInMemoryReplay`. Startup asserts `SHABinding()` matches kind.
- `services/clean-code/internal/ingest/webhook/verb_handler.go` — interface gained `SHABinding() string` + `ExtractMetadata(ctx, body) (VerbPayloadMetadata, error)`. Added `VerbPayloadMetadata` struct + `canonicalSHABindingForKind` helper. Removed duplicate decls from pre-compaction edit.
- `services/clean-code/internal/ingest/webhook/churn_verb.go` — `ChurnVerbHandler` now implements `SHABinding() → "per_row"` and `ExtractMetadata` (returns `{RepoID, SHA:""}`).
- `services/clean-code/internal/ingest/webhook/router_test.go` — split helper into `newRouterStack` + `newRouterStackWithDurable`; new tests `TestRouter_DurableReplay_AcrossSimulatedRestart`, `TestRouter_VerbFailure_FinalizesScanRunAsFailed`, `failingFakeVerbHandler` fixture; `nil scan_run_repo` panic subtest added.
- `services/clean-code/internal/config/config.go` — added `EnvEnableExternalIngestWebhook`, `EnvWebhookSigningKeyID` consts; `EnableExternalIngestWebhook` + `WebhookSigningKeyID` config fields; loader hooks; validation interlock (the three webhook vars are all-or-none).
- `services/clean-code/cmd/clean-code-metric-ingestor/main.go` — new `mountIngestRouter` helper builds `PGExternalScanRunStore → PGScanRunRepository → InMemoryIdempotencyStore → StaticSecretResolver → ChurnVerbHandler → NewRouter` and mounts at `webhook.RouterPath`; called from `main()` after `mountMgmtRoutes`.
- `services/clean-code/CHANGELOG.md` — iter-2 entry prepended documenting all three evaluator items.
- `services/clean-code/docs/runbook.md` — appended iter-2 section: migration 0009, two-tier idempotency table, replay semantics across restart/replica, sticky-failed-replay publisher contract, running-status race, observability lines.
- `services/clean-code/docs/rollout.md` — prepended iter-2 pre-roll checklist: apply migration 0009 via `psql -f` (autocommit), duplicate-row precheck, env-var setup, smoke test, rollback steps.

## Decisions made this iter

- **Two-seam architecture** (key design call). Did NOT collapse the in-process `IdempotencyStore` into the durable layer. In-process is the fast same-process replay cache (single round-trip), durable is the cross-replica / cross-restart authority. Router consults both. Rationale: same-process collapse is the common case (CI publisher retry), and a DB round-trip for every such retry would 5-10x the p99 of a no-op replay.
- **`ExtractMetadata` split out from `Handle`**. Router MUST extract `(RepoID, SHA)` BEFORE opening the durable row (the row's columns require them). A body-shape error here surfaces as 400/422 WITHOUT burning a scan_run row. Both methods decode the body — that's a deliberate cost trade: coupling per-verb body shapes through the Router would be much worse.
- **`canonicalSHABindingForKind` helper + startup assertion**. Mirrors migration 0001's `scan_run_sha_binding_consistent` CHECK at registration time so a registry-misconfiguration panics at startup, not on the first request.
- **Sticky-failed replay**. Failed `scan_run` rows are NOT recycled. A retry with the same canonical body returns the failed `scan_run_id` with `replayed=true`. Publisher MUST change the body (e.g. bump a request nonce). Matches GitHub webhook conventions; preserves audit chain.
- **`CREATE UNIQUE INDEX CONCURRENTLY` (not `BEGIN`-wrapped)**. The migration runner must NOT wrap 0009 in a transaction. Documented in rollout.md as a hard operator constraint with the explicit `psql -f` invocation.
- **`PGScanRunOpener` narrow interface** in the adapter. Captured the two methods the adapter uses so the unit tests in `pg_scan_run_repository_test.go` substitute a fake without a sqlmock DB — keeps that test file at unit-cost.

## Dead ends tried this iter

- Initial attempt at `TestRouter_VerbFailure_FinalizesScanRunAsFailed` used the real `ChurnVerbHandler` with a malformed payload to drive the failure. That short-circuits inside `ExtractMetadata` (DisallowUnknownFields fires before `Handle`), so the durable row never opens — wrong code path. Switched to a `failingFakeVerbHandler` that passes ExtractMetadata but returns a stable error from `Handle`, then verified the row is durably `failed` by re-OpenExternal returning `AlreadyExisted=true, ExistingStatus="failed"`.

## Open questions surfaced this iter

- **Sticky-failed retry-window**. Should there be an operator-controlled retry-window (e.g. after 24h, recycle a failed row via `ON CONFLICT DO UPDATE`)? Current design says NO (publisher contract). Worth raising in the next phase brief.
- **Running-status race surface**. Current behaviour returns the sibling-replica's running `scan_run_id` with `replayed=true`. Publisher polls `/v1/scan_runs/{id}` for the verdict. Should the Router instead 409 + Retry-After? Defer.

## What's still left

- All three iter-1 evaluator items addressed; full suite green except the pre-existing `TestMgmtWriter_RetractSample_UnknownSampleSentinelFromDispatcherReturns404` in `internal/management/` (out of scope; pre-exists on baseline branch).
- Per-verb mounts for `coverage` / `test_balance` / `defects` land in Stages 4.2 / 4.3 / 4.5 — they just need to register new `VerbHandler`s implementing `SHABinding` + `ExtractMetadata` against the same Router seam.
