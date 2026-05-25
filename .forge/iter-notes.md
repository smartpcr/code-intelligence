# Stage 3.1 -- Repo Indexer and Commit lifecycle -- iter 3

## Files touched this iter

- `services/clean-code/cmd/clean-coded/main.go` -- `db` open
  extracted from inside the KMS branch into a new
  unconditional `if cfg.PostgresURL != ""` block placed
  BEFORE the KMS section. KMS branch reduced to `if db !=
  nil && cfg.KMSProvider == keys.KMSProviderLocal { bc.DB =
  db }`. Indexer wiring block now calls
  `repo_indexer.NewRescanHandlerWithHMAC(idx,
  []byte(cfg.WebhookHMACSecret), log)`.
- `services/clean-code/internal/repo_indexer/rescan.go` --
  `RescanHandler` gains `hmacSecret []byte` field, new
  `NewRescanHandlerWithHMAC` constructor (panics on nil
  indexer or empty secret + copies the secret slice), HMAC
  verification block inserted between body-size and
  Content-Type checks. Old `NewRescanHandler` retained
  (HMAC-disabled) and re-documented as test-only.
- `services/clean-code/internal/repo_indexer/rescan_test.go`
  -- added 5 HMAC tests: positive signed, missing header,
  tampered signature, panic-on-nil-indexer,
  panic-on-empty-secret.
- `services/clean-code/internal/repo_indexer/pg_writer_sql_test.go`
  (NEW) -- 9 go-sqlmock-backed SQL behaviour tests for
  `EnsureCommitAndRegisteredEvent`.
- `services/clean-code/cmd/clean-coded/routes_test.go` --
  upgraded `IndexerRescanMounted_RoundtripWritesCommit` to
  exercise HMAC end-to-end; added
  `IndexerRescanMounted_RejectsUnsigned` (401 + writer
  untouched).
- `services/clean-code/go.mod` / `go.sum` -- added
  `github.com/DATA-DOG/go-sqlmock v1.5.2` as a direct test
  dependency.
- `services/clean-code/internal/metric_ingestor/foundation_dispatch_test.go`
  -- (a) `*trackingRecipe` gained `Pack() recipes.Pack`
  (the `recipes.Recipe` interface added Pack in stage 3.0
  and this test helper had drifted behind), (b)
  recipe-count assertion bumped from 3 -> 6 to match the
  live `DefaultRegistry`. Three `trackingRecipe{}`
  literals updated to populate `pack: recipes.PackBase`.
- `services/clean-code/CHANGELOG.md` -- iter-3 entries
  prepended under the Stage 3.1 section.

## Decisions made this iter

- **`db` lives ABOVE the KMS branch.** The iter-2 evaluator
  flagged that `CLEAN_CODE_PG_URL` was silently ignored
  unless `CLEAN_CODE_KMS_PROVIDER=local`. Fix is purely
  structural: extract the `sql.Open` + ping into an
  unconditional block. The KMS branch still threads `db`
  into `bc.DB` for the local KMS pathway, but the indexer
  block (the one that builds `PGCatalogWriter`) now reaches
  PG persistence whenever `cfg.PostgresURL` is set,
  regardless of KMS state.
- **Rescan HMAC parity with the webhook.** iter-2 closed
  with an open question -- "rescan HMAC vs separate
  operator-credential surface?". The architecture (Sec 8.5
  "one external-ingest secret") and the evaluator's
  feedback both point at HMAC parity. iter-3 picks HMAC and
  resolves the open question in-code: rescan now requires
  `X-Hub-Signature-256` matching the SAME secret the
  webhook uses. The two-constructor pattern
  (`NewRescanHandler` test-only,
  `NewRescanHandlerWithHMAC` production) mirrors the
  webhook's existing shape, so the rule-of-three for HMAC
  duplication still hasn't tripped (both surfaces share the
  `hmac.go` helpers).
- **go-sqlmock for SQL behaviour tests, NOT a live PG
  container.** The PG-backed writer's assertions are about
  prepared-statement SHAPE (`scan_status` omitted,
  `NULLIF($3, '')` present, `ON CONFLICT ... DO NOTHING
  RETURNING 1`, canonical `registered` literal), not about
  PG semantics. sqlmock proves the writer issues the right
  statements in the right order; integration tests against
  live PG belong in Stage 3.2 when the Metric Ingestor
  needs the same harness.
- **metric_ingestor fixes land in iter-3 even though
  `foundation_dispatch_test.go` is not in my workstream's
  target list.** Two reasons: (a) the iter-2 evaluator
  hard-gated on the open question that called these out --
  carrying it to iter-4 would trip
  `stalled-no-convergence`; (b) both fixes are mechanical
  (3 lines + 1 literal swap). Leaving the package
  unbuildable would also block any future stage from
  exercising the registry-driven dispatcher.

## Dead ends tried this iter

- Initially considered a separate operator-credential
  surface for rescan (Bearer + audience). Reverted to HMAC
  parity after re-reading architecture Sec 8.5; deferring
  Bearer would have introduced a NEW open question.
- Initially used `sqlmock.QueryMatcherEqual` (the default)
  -- the dynamic schema interpolation via
  `pq.QuoteIdentifier` produces SQL strings that aren't
  byte-stable. Switched to
  `sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp)`
  so the matcher accepts patterns instead of literal
  strings.

## Open questions surfaced this iter

NONE. iter-2's two open questions were both resolved
in-code this iter (rescan HMAC parity adopted;
metric_ingestor pre-existing failures fixed).

## What's still left

- Stage 3.2 (Metric Ingestor lifecycle transitions) imports
  `ScanStatus` + `CanTransition` for its UPDATE statements
  and exercises the SQL `PGCatalogWriter` against live PG.
- Stage 3.2 will own the `pending -> scanning -> scanned`
  and `pending -> scanning -> failed` UPDATE paths; the
  Repo Indexer in this stage is INTENTIONALLY a write-only
  surface for `scan_status='pending'` (via DB DEFAULT).
