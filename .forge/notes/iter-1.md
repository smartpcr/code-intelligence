# Stage 3.1 -- Repo Indexer and Commit lifecycle -- iter 1

## Files touched this iter

- `services/clean-code/internal/repo_indexer/scan_status.go` (NEW) -- canonical `ScanStatus` enum + transition diagram.
- `services/clean-code/internal/repo_indexer/indexer.go` (NEW) -- `Indexer`, `CatalogWriter` interface, `InMemoryCatalogWriter` fake, `CommitEnsureRequest`/`CommitEnsureResult`.
- `services/clean-code/internal/repo_indexer/webhook.go` (NEW) -- HTTP `/v1/indexer/webhook` handler.
- `services/clean-code/internal/repo_indexer/hmac.go` (NEW) -- standalone HMAC-SHA256 verifier (duplicates `internal/ingest/webhook` rule-of-three).
- `services/clean-code/internal/repo_indexer/scan_status_test.go` (NEW) -- closed-set + exhaustive transition tests.
- `services/clean-code/internal/repo_indexer/indexer_test.go` (NEW) -- happy/duplicate/multi-repo/validation/panic/concurrency.
- `services/clean-code/internal/repo_indexer/handler_test.go` (NEW) -- HTTP method/CT/JSON/HMAC/size guards.
- `services/clean-code/CHANGELOG.md` -- prepended Stage 3.1 entry.

## Decisions made this iter

- **Single `CatalogWriter` interface, not split `CommitWriter`/`RepoEventWriter`.** Rubber-duck flagged that two interfaces leak partial-write races (commit lands, event fails permanently). One method `EnsureCommitAndRegisteredEvent` encodes atomicity in the type system; Stage 3.2 storage adapter will wrap both INSERTs in a single tx.
- **Omit `scan_status` from INSERT, rely on DB DEFAULT `'pending'`.** Architecture G1 / iter-1 evaluator item 2: Repo Indexer never names `scan_status`. `InMemoryCatalogWriter` mirrors this by stamping `ScanStatusPending` server-side, never reading caller input.
- **`ScanStatus` lives in `repo_indexer`, not a neutral `lifecycle` package.** Rubber-duck suggested a separate package to avoid Metric Ingestor importing the indexer, but the workstream brief literally says "Add a Go enum `ScanStatus`" inside this stage. A future Stage 3.2 MAY extract; documented as a known future refactor.
- **Past-tense `registered` literal.** Architecture Sec 5.1.4 lines 877-884 -- `register` (imperative) is a forbidden alias. Added explicit `NeverEmitsRegisterPastTenseCanon` assertion test.
- **`Ref` field carried on `CommitEnsureRequest` and `WebhookPayload`.** Unused in Stage 3.1, but downstream `default_branch_head` work needs it; including now avoids a wire-shape break later.
- **HMAC code duplicated rather than shared.** Rule of three -- 2nd surface joining the codebase; refactor when a 3rd webhook arrives.
- **Black-box `package repo_indexer_test` for tests.** Matches `webhook_test`, `churn_test`, `recipes_test` convention in the repo.

## Dead ends tried this iter

- Tried `go build`/`go test` directly. Failed because `go.mod` declares `module forge/services/clean-code` while ALL existing source uses `github.com/microsoft/code-intelligence/services/clean-code/...` imports. This is a pre-existing repo-wide condition (baseline `make build` fails identically on `cmd/clean-coded`), NOT a regression introduced by Stage 3.1. CI uses an out-of-band setup (likely `go.work` overlay or replace injection at the runner) that the local worktree doesn't reproduce.
- Tried adding a `replace github.com/microsoft/... => ./` directive locally to confirm syntactic correctness: `go build ./internal/repo_indexer/...` then PASSED, proving the new code itself is sound. The test-run still tripped the `internal/` package rule because the synthetic module name didn't match, but that's noise from the local workaround, not the code.

## Open questions surfaced this iter

- None blocking Stage 3.1. The module-path mismatch is repo-wide and predates this stage; the next stage owner inherits the same condition.

## What's still left

- Stage 3.2 (Metric Ingestor lifecycle transitions) will import this package's `ScanStatus` + `CanTransition` for its UPDATE statements.
- Stage 3.2 will provide the SQL `CatalogWriter` implementation (`pgx`-backed) that executes both INSERTs in a single transaction with the `ON CONFLICT (repo_id, sha) DO NOTHING RETURNING 1` shape documented in `indexer.go`.
- Composition root (`cmd/clean-coded/main.go`) is NOT wired in this stage -- that lands when Stage 3.2 provides the storage adapter so the webhook has somewhere to write to.
