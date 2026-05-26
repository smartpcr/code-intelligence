# Iter notes — Stage 4.1 Webhook transport + HMAC verification

## Files touched this iter

- `services/clean-code/internal/ingest/webhook/router_test.go` — NEW. Comprehensive `httptest`-driven Router tests covering: happy path, invalid signature 401, missing/malformed signature 401, unknown/malformed/missing signing_key_id 401, tampered body 401, replay-returns-cached-scan-run (with `replayed=true` + same scan_run_id), distinct payloads get distinct scan_run_ids, 405/404/415/413/400 paths, HMAC-before-content-type ordering invariant, verb-failure-aborts-claim, concurrent retries collapse to single execution, structured log redaction of secrets, NewRouter panic-on-misconfig, payload_hash equals sha256(body), no tenant_id field on envelope.
- `services/clean-code/internal/ingest/webhook/hmac_test.go` — NEW. Unit tests for SignHMAC/VerifyHMAC: known digest vector, round-trip, empty-secret, missing/malformed header branches, signature mismatch, body tamper, uppercase-hex (pinning current accept behaviour).
- `services/clean-code/internal/ingest/webhook/churn_verb_test.go` — NEW. Identity (verb/media-type/scan_run_kind), honours-Router-supplied-scan_run_id, ClassifyError closed-set mapping, unknown-error defers to Router (returns 0/""), bad JSON + unknown field rejection, nil ingestor panic.
- `services/clean-code/internal/ingest/webhook/verb_handler_test.go` — NEW. ValidateVerbToken table test covering happy and 9 rejection shapes.
- `services/clean-code/internal/ingest/webhook/testhelpers_test.go` — NEW. `safeBuffer` + `newTestLogger` helper used by `TestRouter_LoggerEmitsHMACFailures`.
- `services/clean-code/CHANGELOG.md` — Prepended Stage 4.1 entry documenting Router, HMAC, idempotency seams.
- `services/clean-code/docs/runbook.md` — Appended Stage 4.1 section: wire shape, order-of-operations, success envelope, operator rotation.
- `services/clean-code/docs/rollout.md` — Prepended Stage 4.1 pre-roll checklist + key rotation procedure.

## Decisions made this iter

- **Did NOT touch the existing untracked Router/idempotency/secret_resolver/verb_handler/churn_verb source files** — they were already implemented in prior work (just not yet committed). The implementation is complete and the build is green. My contribution was the test surface required by Stage 4.1's brief ("Add `internal/ingest/webhook/handler_test.go` covering valid signature, invalid signature, replay returns cached scan_run.") plus the operator docs.
- **Replay scenario test** explicitly checks `writer.Records()` count is unchanged between first and second POST and `Replayed=true` on the second response — the brief's "no new scan_run row is appended" invariant.
- **Tampered-body test** swaps `req.Body` and `req.ContentLength` to a mutated buffer while keeping the signature header pointing at the original, proving HMAC verification runs over the actual read bytes.
- Did NOT modify `cmd/clean-code-metric-ingestor/main.go` to mount the Router — Stage 4.1's brief scopes to `internal/ingest/webhook/`. Mounting in a binary is a downstream-stage concern (probably the first verb stage to land, 4.2 coverage or 4.4 churn-via-Router).

## Dead ends tried this iter

- First `TestRouter_MalformedPath_Returns404` used `"/v1/ingest/foo bar"` — `httptest.NewRequest` panics on a space in the URL (`malformed HTTP version`). Replaced with `"/v1/ingest/foo-bar"` (hyphen is rejected by `ValidateVerbToken` so it still proves the 404 branch).

## Open questions surfaced this iter

- None. The implementation was already complete; this iter was about adding the test surface and docs that the brief explicitly asks for.

## What's still left

- The pre-existing failure `TestMgmtWriter_RetractSample_UnknownSampleSentinelFromDispatcherReturns404` in `internal/management/` is NOT in scope for Stage 4.1 (no edits to that package; failure exists on the baseline branch too). The dispatcher's classifier was tightened to require typed sentinels but the test still uses a plain `fmt.Errorf` string — operator follow-up.
- Mounting the Router in `cmd/clean-code-metric-ingestor/main.go` will land with the first verb-routing stage that genuinely needs it (Stage 4.2 coverage or 4.4 churn-via-Router).
