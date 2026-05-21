---
title: "clean code"
storyId: "code-intelligence:CLEAN-CODE"
---

> Livedoc: tick each `- [ ]` as work lands. Anchors (e.g. `phase-foundation-and-schema/stage-project-scaffold-and-ci-baseline`) are derived from the heading TITLE only -- no phase or stage numbers in slugs. Architecture and tech-spec section references throughout point at the sibling docs in this same story folder; do NOT re-derive their content here.

# Phase 1: Foundation and schema

## Dependencies
- _none -- start phase_

## Stage 1.1: Project scaffold and CI baseline

### Implementation Steps
- [ ] Create `services/clean-code/` Go module mirroring `services/agent-memory/` layout: `cmd/clean-coded/`, `internal/`, `pkg/`, `proto/`, `migrations/`, `deploy/local/`, `web/`, plus root `Makefile`, `go.mod`, `README.md`.
- [ ] Wire `Makefile` targets `build`, `test`, `lint`, `proto`, `migrate-up`, `migrate-down`, `docker-build`, `compose-up`, `compose-down` matching the agent-memory precedent so CI can reuse the same recipe shape.
- [ ] Add `.github/workflows/clean-code-ci.yml` running `make lint test` on PR plus build of the `clean-coded` container, mirroring `agent-memory-ci.yml`.
- [ ] Add `services/clean-code/deploy/local/docker-compose.yml` with PostgreSQL 16 (per tech-spec C9 / Sec 8.1) and a placeholder `clean-coded` service; expose Prometheus scrape port and OTel collector endpoint.
- [ ] Add `services/clean-code/internal/config/` loader that reads env + file config, exposing the operator pins listed in architecture Sec 1.6 (`ast-mode-default`, `external-metric-coverage-format`, `gate-degraded-policy`, `policy-signing-required`, `refactor-effort-source`) as typed fields with defaults from tech-spec Sec 8.
- [ ] Add `services/clean-code/internal/logging/` slog wrapper enforcing structured JSON logs and request-id propagation (architecture Sec 8 telemetry invariant).
- [ ] Add `services/clean-code/internal/version/` exporting `Version`, `Commit`, `BuildTime` plus a `/healthz` and `/readyz` HTTP handler stub used by all surfaces.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: scaffold-builds-clean -- Given a fresh checkout, When `make build lint test` runs in `services/clean-code/`, Then it exits 0 with no missing-target errors and produces the `clean-coded` binary.
- [ ] Scenario: ci-workflow-triggers -- Given a PR touching `services/clean-code/**`, When GitHub Actions evaluates the workflow file, Then `.github/workflows/clean-code-ci.yml` runs `make lint test` and the container build job and both succeed on the empty scaffold.
- [ ] Scenario: config-honours-pins -- Given a config file that omits the five operator pins, When the loader initialises, Then it returns defaults matching architecture Sec 1.6 (`embedded` AST mode, `Cobertura XML`, `warn`, `v1 required`, `ML model from historical commits`).

## Stage 1.2: Catalog and Lifecycle schema migrations

### Implementation Steps
- [ ] Add `migrations/0001_catalog_lifecycle.up.sql` creating tables `repo`, `commit` (with `scan_status` column per architecture Sec 5.2), `scope_binding`, `scan_run` in schema `catalog`, with explicit primary keys and foreign keys matching architecture Sec 5.
- [ ] Add matching `migrations/0001_catalog_lifecycle.down.sql` that drops the schema cleanly so local-dev resets are deterministic.
- [ ] Encode the `scan_status` enum (`pending`, `scanning`, `complete`, `failed`, `superseded`) and the `Commit.scan_status` writer-ownership comment as a column COMMENT pointing at architecture Sec 1.5.1.
- [ ] Add table COMMENTs on each catalog/lifecycle table naming the owning sub-store and the writers permitted by G1.
- [ ] Add indexes required by Repo Indexer hot reads: `(repo_id, sha)` unique on `commit`, `(repo_id, default_branch_head)` on `repo`, `(commit_id, scope_kind)` on `scope_binding`.
- [ ] Add a `go test` migration round-trip helper under `internal/storage/migrate_test.go` asserting `up` then `down` returns an empty schema (no leftover tables).

### Dependencies
- phase-foundation-and-schema/stage-project-scaffold-and-ci-baseline

### Test Scenarios
- [ ] Scenario: catalog-up-down -- Given an empty PostgreSQL 16 instance, When `make migrate-up` then `make migrate-down` runs, Then both succeed and `\dt catalog.*` is empty after the second run.
- [ ] Scenario: scan-status-enum-rejects-invalid -- Given the `commit` table after `migrate-up`, When an INSERT supplies `scan_status='garbage'`, Then PostgreSQL rejects it with a check / enum constraint violation.
- [ ] Scenario: scope-binding-uniqueness -- Given a committed scope binding for `(commit_id='c1', scope_kind='function', scope_path='pkg.Foo')`, When a second INSERT with the same triple runs, Then it fails with a unique constraint violation.

## Stage 1.3: Measurement schema and active pointer DDL

### Implementation Steps
- [ ] Add `migrations/0002_measurement.up.sql` creating immutable `metric_sample` table per architecture Sec 5.3 and tech-spec Sec 8.7, with no UPDATE/DELETE grants in the same migration.
- [ ] Create the side-relation `metric_sample_active` with PRIMARY KEY on `(repo_id, sha, scope_id, metric_kind, metric_version)` exactly as tech-spec C1 / Sec 8.7 pins; columns reference `metric_sample(id)` via FK.
- [ ] Create `metric_retraction` table (architecture Sec 5.3.3) capturing the audit pair `(retracted_sample_id, replacement_sample_id, reason, retracted_at, retracted_by)`.
- [ ] Encode the retract-then-reinsert transactional helper as a stored procedure `measurement.swap_active(...)` performing the active-row ON CONFLICT swap in a single statement; document the call contract in a SQL comment.
- [ ] Add indexes: `(scope_id, metric_kind, observed_at DESC)` on `metric_sample` for time-series lookups; `(repo_id, sha, metric_kind)` on `metric_sample_active` for gate reads.
- [ ] Add a regression test `internal/storage/measurement_active_test.go` asserting that two concurrent `swap_active` calls under SERIALIZABLE isolation produce exactly one winner and one serialization-failure retry (covering C1 invariant).

### Dependencies
- phase-foundation-and-schema/stage-catalog-and-lifecycle-schema-migrations

### Test Scenarios
- [ ] Scenario: metric-sample-immutable -- Given a row inserted into `metric_sample`, When any role attempts UPDATE or DELETE, Then PostgreSQL denies the operation due to absent grants.
- [ ] Scenario: active-pointer-pk-enforces-uniqueness -- Given an active row for `(repo_id=r1, sha=s1, scope_id=sc1, metric_kind=cyclomatic, metric_version=v1)`, When a second INSERT bypasses the swap procedure, Then it fails on the primary-key constraint.
- [ ] Scenario: swap-active-is-atomic -- Given an existing active row, When `swap_active` runs against a new sample id, Then the active row references the new sample and a retraction row pairs the previous and new sample ids inside the same transaction.

## Stage 1.4: Policy and Audit and Refactor schema migrations

### Implementation Steps
- [ ] Add `migrations/0003_policy.up.sql` creating `rule_pack`, `rule_pack_revision` (with `signature_bytes`, `signing_key_id`, `published_at`, `activated_at`), and `policy_override` per architecture Sec 5.5; default deny INSERT/UPDATE to all roles except `policy_steward`.
- [ ] Add `migrations/0004_audit.up.sql` creating `audit_wal_frame` (architecture Sec 7.1), `audit_event`, and `audit_anchor` tables; constrain `audit_wal_frame.lsn` to a monotonic SEQUENCE.
- [ ] Add `migrations/0005_refactor.up.sql` creating `refactor_plan`, `refactor_task`, `effort_estimate` per architecture Sec 5.7; record `effort_model_version` per row to support the ML pin (operator pin `refactor-effort-source`).
- [ ] Add table COMMENTs naming the owning sub-store (Policy, Audit, Refactor) and the single writer permitted per G1.
- [ ] Wire the corresponding `*.down.sql` files so `make migrate-down` strips everything cleanly.
- [ ] Add a smoke test that inserts a minimal row into each new table using the appropriate writer role and reads it back via the corresponding reader role.

### Dependencies
- phase-foundation-and-schema/stage-measurement-schema-and-active-pointer-ddl

### Test Scenarios
- [ ] Scenario: rule-pack-signature-required -- Given a `rule_pack_revision` insert with `signature_bytes IS NULL`, When the migration's CHECK constraint evaluates, Then it rejects the row (per operator pin `policy-signing-required=v1 required`).
- [ ] Scenario: audit-wal-lsn-monotonic -- Given the WAL sequence at value N, When two concurrent writers each request `nextval`, Then both receive distinct values and neither receives an LSN <= N.
- [ ] Scenario: refactor-plan-stores-effort-model-version -- Given an INSERT into `refactor_plan` omitting `effort_model_version`, When the migration's NOT NULL constraint evaluates, Then the insert fails.

## Stage 1.5: PostgreSQL role grants and schema isolation

### Implementation Steps
- [ ] Add `migrations/0006_roles.up.sql` creating roles `metric_ingestor`, `cross_repo_aggregator`, `policy_steward`, `evaluator`, `solid_batch`, `wal_reconciler`, `refactor_planner`, `management`, plus read-only roles `mgmt_reader`, `insights_reader`.
- [ ] Issue per-table GRANTs encoding architecture G1 sub-store ACL: Catalog/Lifecycle writers = `management` + `metric_ingestor` (split on `Commit.scan_status` per architecture Sec 1.5.1); Measurement writer = `metric_ingestor`; Measurement `pack='system'` rows writable ONLY by `cross_repo_aggregator`; Policy writer = `policy_steward`; Audit writers = `evaluator` + `solid_batch` + `wal_reconciler` (append-only); Refactor writer = `refactor_planner`.
- [ ] Implement the `pack='system'` carve-out using a row-level security policy on `metric_sample` and `metric_sample_active` that rejects `INSERT` from `metric_ingestor` when `pack='system'` and rejects non-`system` packs from `cross_repo_aggregator`.
- [ ] Add REVOKE of UPDATE/DELETE on `metric_sample`, `audit_wal_frame`, `audit_event` from ALL roles (architecture invariant: immutability).
- [ ] Document each role and grant in a `docs/stories/code-intelligence-CLEAN-CODE/runbook-roles.md` reference (created here, expanded in Stage 10.5).
- [ ] Add an `internal/storage/grants_test.go` connecting as each role and asserting permitted vs denied writes for one canonical row per sub-store.

### Dependencies
- phase-foundation-and-schema/stage-policy-and-audit-and-refactor-schema-migrations

### Test Scenarios
- [ ] Scenario: metric-ingestor-denied-system-pack -- Given role `metric_ingestor`, When it attempts INSERT into `metric_sample` with `pack='system'`, Then RLS rejects the row.
- [ ] Scenario: cross-repo-aggregator-denied-non-system-pack -- Given role `cross_repo_aggregator`, When it attempts INSERT into `metric_sample` with `pack='base.go'`, Then RLS rejects the row.
- [ ] Scenario: audit-tables-append-only -- Given role `evaluator` after appending an audit event, When it attempts UPDATE or DELETE on `audit_event`, Then PostgreSQL denies the operation.

# Phase 2: AST Adapter and foundation tier compute

## Dependencies
- phase-foundation-and-schema

## Stage 2.1: Tree sitter parser fleet and canonical AST proto

### Implementation Steps
- [ ] Add `proto/clean_code/v1/ast.proto` defining `CanonicalAst`, `ScopeNode`, `EdgeRef`, `Location` per architecture Sec 3.2 / Sec 9 public-contract; generate Go bindings under `pkg/genproto/`.
- [ ] Vendor tree-sitter grammars for the four v1 languages (Go, Python, TypeScript, Java -- tech-spec Sec 8.6 / locked decision 21) under `internal/ast/grammars/` with a manifest fixing grammar SHAs.
- [ ] Implement `internal/ast/parser/` exposing `Parse(ctx, lang, source) (*CanonicalAst, error)` that normalises whitespace and comments and emits canonical node IDs derived from a stable visit order.
- [ ] Add `internal/ast/normalize/` mapping language-specific node types into the canonical `ScopeNode.Kind` enum so downstream recipes do not need per-language branches.
- [ ] Add a per-language golden corpus under `internal/ast/testdata/<lang>/` with at least 10 minimal source fixtures and snapshot files of the canonical AST.
- [ ] Add `internal/ast/parser_test.go` asserting deterministic output (parse twice, byte-equal proto serialisation) across the corpus.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: parser-deterministic -- Given any fixture in the corpus, When `Parse` runs twice on the same bytes, Then the serialised `CanonicalAst` bytes are identical.
- [ ] Scenario: four-languages-supported -- Given fixtures for Go, Python, TypeScript, and Java, When the parser fleet boots, Then `Parse` succeeds for each and `CanonicalAst.language` matches the fixture language.
- [ ] Scenario: unsupported-language-rejected -- Given a fixture with `lang="rust"`, When `Parse` is invoked, Then it returns a typed `ErrUnsupportedLanguage` and does not panic.

## Stage 2.2: Scope identity derivation and ScopeBinding writer

### Implementation Steps
- [ ] Implement `internal/scope/identity.go` deriving `scope_id` per architecture Sec 5.2.4 (stable hash over `(repo_id, lang, normalised_path, scope_kind, qualified_name)`).
- [ ] Implement `internal/scope/binding_writer.go` that batches `ScopeBinding` rows per commit and writes them under a single transaction so partial writes never expose half-bound scopes.
- [ ] Wire the writer to refuse to commit when `Commit.scan_status != 'scanning'` so the writer-ownership split in architecture Sec 1.5.1 is enforced in code.
- [ ] Add an `internal/scope/identity_test.go` covering rename, move, and re-export edge cases to confirm identity stability for the cases architecture Sec 5.2.4 calls out.
- [ ] Add metric counters `scope_binding_rows_written_total` and `scope_binding_collisions_total` exported via Prometheus.

### Dependencies
- phase-ast-adapter-and-foundation-tier-compute/stage-tree-sitter-parser-fleet-and-canonical-ast-proto

### Test Scenarios
- [ ] Scenario: scope-id-stable-under-rename -- Given a function moved from `pkg/a/foo.go` to `pkg/a/bar.go` with the same `qualified_name`, When identity is computed for both commits, Then `scope_id` is identical.
- [ ] Scenario: scope-id-changes-on-rename -- Given a function renamed from `Foo` to `Bar` within the same file, When identity is computed for both commits, Then `scope_id` differs.
- [ ] Scenario: writer-refuses-out-of-state-commit -- Given a `Commit` with `scan_status='complete'`, When the binding writer attempts to insert rows for it, Then the transaction aborts with a typed `ErrCommitNotScanning` and no rows persist.

## Stage 2.3: Per language base pack recipes

### Implementation Steps
- [ ] Define a `Recipe` interface in `internal/compute/recipe.go` with `Kind() string`, `Version() string`, `Compute(ctx, *ScopeView) ([]MetricSample, error)` per architecture Sec 3.4.
- [ ] Implement foundation metrics from architecture Sec 1.4: `cyclomatic_complexity`, `cognitive_complexity`, `lines_of_code`, `function_length`, `parameter_count`, `nesting_depth`.
- [ ] Wire each recipe to emit a `metric_version` string that bumps on any algorithm change so the active pointer (PK includes `metric_version`) treats it as a new metric kind.
- [ ] Add per-language golden CSVs under `internal/compute/testdata/base/` with expected values for the corpus from Stage 2.1.
- [ ] Add `internal/compute/base_test.go` running each recipe against the corpus and asserting golden equality; fail loudly on any divergence.
- [ ] Register the recipes under pack name `base.<lang>` so the SOLID and cycle/duplication packs in later stages can be enabled independently per architecture Sec 1.6 pack model.

### Dependencies
- phase-ast-adapter-and-foundation-tier-compute/stage-scope-identity-derivation-and-scopebinding-writer

### Test Scenarios
- [ ] Scenario: base-metrics-golden -- Given the per-language corpus, When the foundation recipes run, Then computed values match the golden CSVs byte-for-byte.
- [ ] Scenario: metric-version-bump-invalidates-active -- Given an active row at `metric_version=v1`, When a recipe is upgraded to `v2` and runs, Then the new sample lands at a distinct active-pointer key and the v1 row remains in `metric_sample` (immutable history).

## Stage 2.4: SOLID pack foundation tier recipes

### Implementation Steps
- [ ] Implement SRP heuristic `srp_methods_per_class` and `srp_responsibility_clusters` per architecture Sec 1.4 SOLID metric catalogue.
- [ ] Implement OCP heuristic `ocp_modification_count_in_window` consuming the materialiser from Stage 2.6.
- [ ] Implement LSP heuristic `lsp_subtype_contract_violations` (signature widening, exception broadening) using the canonical AST.
- [ ] Implement ISP heuristic `isp_interface_method_count` plus `isp_unused_method_ratio` from cross-package edge data.
- [ ] Implement DIP heuristic `dip_concrete_dependency_ratio` walking edges from a scope to types in other packs.
- [ ] Register the recipes under pack name `solid.<lang>` and wire the same golden / version conventions used in Stage 2.3.
- [ ] Add per-language SOLID fixtures under `internal/compute/testdata/solid/` including at least one positive and one negative case per principle.

### Dependencies
- phase-ast-adapter-and-foundation-tier-compute/stage-per-language-base-pack-recipes

### Test Scenarios
- [ ] Scenario: solid-positive-and-negative-cases -- Given each principle's positive and negative fixture, When the SOLID recipes run, Then the negative fixture exceeds the recipe's emitted threshold marker and the positive does not.
- [ ] Scenario: dip-concrete-ratio-bounded -- Given a scope with zero outbound edges, When `dip_concrete_dependency_ratio` runs, Then it emits `value=0.0` and `confidence='low'` rather than dividing by zero.

## Stage 2.5: Cycle and duplication recipes

### Implementation Steps
- [ ] Implement `dependency_cycles` recipe building a SCC graph over `ScopeBinding` edges per pack, emitting one sample per detected cycle with `cycle_size` and `members` JSON payload.
- [ ] Implement `clone_detector` using a token-shingle MinHash over normalised AST nodes; emit `clone_pair` samples with `similarity` score.
- [ ] Cap cycle and clone work at the per-commit budget defined in tech-spec Sec 8 (parameter pin for compute budgets) so a pathological repo never blocks the ingest path.
- [ ] Add fixtures under `internal/compute/testdata/cycles/` with a 3-node cycle, a 5-node cycle, and a near-clone pair; assert the recipes detect them.
- [ ] Wire the recipes under pack names `cycle.<lang>` and `duplication.<lang>`.

### Dependencies
- phase-ast-adapter-and-foundation-tier-compute/stage-solid-pack-foundation-tier-recipes

### Test Scenarios
- [ ] Scenario: cycle-detector-finds-known-cycles -- Given the 3- and 5-node cycle fixtures, When the recipe runs, Then it emits exactly two samples with the expected `cycle_size` values.
- [ ] Scenario: clone-detector-respects-threshold -- Given a clone pair with measured similarity 0.92 and a near-clone at 0.55, When the recipe runs with threshold 0.85, Then it emits a sample for the 0.92 pair and not for the 0.55 pair.
- [ ] Scenario: compute-budget-honoured -- Given a synthetic repo whose cycle search exceeds the configured budget, When the recipe runs, Then it returns a partial result with `degraded_reason='samples_pending'` rather than running unbounded.

## Stage 2.6: Modification count in window materialiser

### Implementation Steps
- [ ] Implement `internal/compute/window/materialiser.go` that consumes commit history and emits `modification_count_in_window` per scope for windows {30d, 90d, 180d} (architecture Sec 1.4).
- [ ] Persist a rolling cache in `measurement.modification_window_cache` so re-runs are O(delta) per architecture Sec 5.3 cache discussion.
- [ ] Surface the cache write path through `metric_ingestor` only -- never via `cross_repo_aggregator` -- to honour G1.
- [ ] Add a `materialise_test.go` covering window-boundary, force-rebuild, and clock-skew scenarios.

### Dependencies
- phase-ast-adapter-and-foundation-tier-compute/stage-scope-identity-derivation-and-scopebinding-writer

### Test Scenarios
- [ ] Scenario: window-boundary -- Given commits at days {-31, -30, -29}, When the 30d materialiser runs as of today, Then the count includes the day -30 and day -29 commits and excludes day -31.
- [ ] Scenario: cache-delta-only -- Given the cache populated through commit N, When commit N+1 lands, Then the materialiser reads at most one new commit row and writes one new cache row.

# Phase 3: Repo Indexer and Metric Ingestor

## Dependencies
- phase-foundation-and-schema
- phase-ast-adapter-and-foundation-tier-compute

## Stage 3.1: Repo Indexer and Commit lifecycle

### Implementation Steps
- [ ] Implement `internal/indexer/` per architecture Sec 3.3 with verbs `Repo.Register`, `Repo.Refresh`, `Commit.Mark` driving the `Commit.scan_status` field through `pending` -> `scanning` -> `complete|failed`.
- [ ] Wire git fetch via `go-git` against authenticated GitHub / GitLab providers, recording commit SHAs and parent SHAs into `commit`.
- [ ] Honour the writer-ownership split: `management` writes `pending`; `metric_ingestor` writes `scanning`, `complete`, `failed`; `cross_repo_aggregator` writes `superseded` (architecture Sec 1.5.1).
- [ ] Emit OTel spans `indexer.refresh`, `indexer.mark` with attributes `repo_id`, `sha`, `prev_status`, `next_status`.
- [ ] Add `internal/indexer/state_machine_test.go` exercising every legal transition and rejecting every illegal one.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: legal-transitions-accepted -- Given `Commit.scan_status='pending'`, When `Commit.Mark(state='scanning')` runs as `metric_ingestor`, Then the row updates and an audit event records the transition.
- [ ] Scenario: illegal-transition-rejected -- Given `Commit.scan_status='complete'`, When any role calls `Commit.Mark(state='scanning')`, Then it returns a typed `ErrIllegalTransition` and the row is unchanged.
- [ ] Scenario: wrong-writer-denied -- Given role `evaluator`, When it attempts `Commit.Mark(state='complete')`, Then the call fails with `ErrWriterDenied` and PostgreSQL grants block the underlying UPDATE.

## Stage 3.2: Metric Ingestor and ScanRun state machine

### Implementation Steps
- [ ] Implement `internal/ingestor/` per architecture Sec 3.1 driving the `ScanRun` lifecycle (`queued` -> `running` -> `succeeded|failed|orphaned`).
- [ ] Pull canonical ASTs from the embedded AST adapter (operator pin `ast-mode-default=embedded`) and dispatch enabled recipes via the pack registry.
- [ ] Persist `MetricSample` rows in a write batch sized per tech-spec Sec 8 parameter pins; never spawn writes outside the ScanRun's transaction scope until the active-pointer swap (Stage 3.3).
- [ ] Surface a per-pack `enabled` flag from `repo` so an operator can disable a noisy pack without redeploying.
- [ ] Add `internal/ingestor/run_test.go` covering pack enable/disable and partial-pack failure isolation.

### Dependencies
- phase-repo-indexer-and-metric-ingestor/stage-repo-indexer-and-commit-lifecycle

### Test Scenarios
- [ ] Scenario: pack-disable-skips-recipes -- Given pack `cycle.go` disabled for repo R, When the ingestor runs against R, Then no `cycle_*` samples are written and the ScanRun still completes.
- [ ] Scenario: failed-recipe-does-not-poison-run -- Given a recipe that panics for one scope, When the ingestor processes the commit, Then the offending scope is recorded with `metric_status='error'` and the remaining scopes still produce valid samples.

## Stage 3.3: Active pointer transactional writer

### Implementation Steps
- [ ] Implement `internal/storage/active.go` wrapping the `measurement.swap_active` procedure (Stage 1.3) and running each swap under SERIALIZABLE isolation.
- [ ] Couple every `MetricSample` write to a swap so the row is either active or has a `MetricRetraction` audit pair -- never neither.
- [ ] Add a backoff/retry loop for serialization-failure (`40001`) up to the limit set in tech-spec Sec 8 retry pin.
- [ ] Add `internal/storage/active_concurrency_test.go` running two competing swaps and asserting exactly one wins per try, no double-active rows ever observed.

### Dependencies
- phase-repo-indexer-and-metric-ingestor/stage-metric-ingestor-and-scanrun-state-machine

### Test Scenarios
- [ ] Scenario: two-writers-no-double-active -- Given two concurrent swaps for the same `(repo_id, sha, scope_id, metric_kind, metric_version)`, When both commit, Then exactly one active row exists and the loser retried successfully.
- [ ] Scenario: retract-pair-always-present -- Given an active row replaced N times, When the audit table is queried, Then it contains exactly N `MetricRetraction` rows pairing previous and replacement sample ids.

## Stage 3.4: MetricRetraction and rescan flow

### Implementation Steps
- [ ] Implement `Metric.Rescan(repo, sha, pack)` verb in the Management surface stub so an operator can trigger a forced re-ingest per architecture Sec 6.
- [ ] Wire the rescan path through Stage 3.3's active writer so retracted rows always pair with replacement rows.
- [ ] Surface a `rescan_reason` enum (`recipe_upgrade`, `data_corrupted`, `manual_review`) and require it on every rescan call.
- [ ] Add `internal/ingestor/rescan_test.go` covering each rescan reason and asserting the retraction rows carry the right reason.

### Dependencies
- phase-repo-indexer-and-metric-ingestor/stage-active-pointer-transactional-writer

### Test Scenarios
- [ ] Scenario: rescan-replaces-active -- Given an active sample at v1, When `Metric.Rescan(reason='recipe_upgrade')` runs after recipe upgrade to v2, Then the new sample is active and a retraction row records the swap with `reason='recipe_upgrade'`.
- [ ] Scenario: rescan-without-reason-rejected -- Given a call to `Metric.Rescan` omitting `rescan_reason`, When the request reaches the handler, Then it returns `ErrInvalidArgument` and no DB writes occur.

## Stage 3.5: Orphan ScanRun sweep loop

### Implementation Steps
- [ ] Implement `internal/ingestor/sweeper.go` that periodically scans `scan_run` for rows in `running` state past the timeout pinned in tech-spec Sec 8, and transitions them to `orphaned` while emitting an audit event.
- [ ] Add a sweep cadence config knob (default per tech-spec) and surface a Prometheus gauge `scan_run_orphaned_total`.
- [ ] Guarantee the sweeper never marks a `running` run as `orphaned` if its owning worker has sent a heartbeat within the heartbeat window.
- [ ] Add `internal/ingestor/sweeper_test.go` with a fake clock covering heartbeat-just-in-time, heartbeat-stale, and heartbeat-absent cases.

### Dependencies
- phase-repo-indexer-and-metric-ingestor/stage-metric-ingestor-and-scanrun-state-machine

### Test Scenarios
- [ ] Scenario: stale-run-orphaned -- Given a `running` ScanRun with no heartbeat for 2x the timeout, When the sweeper runs, Then the row is transitioned to `orphaned` and an audit event is emitted.
- [ ] Scenario: heartbeat-protects-live-run -- Given a `running` ScanRun with a heartbeat 1s old, When the sweeper runs, Then the row remains `running` and no audit event is emitted.

# Phase 4: External Metric Ingest Webhook

## Dependencies
- phase-repo-indexer-and-metric-ingestor

## Stage 4.1: Webhook transport and HMAC verification

### Implementation Steps
- [ ] Implement `internal/webhook/server.go` exposing `POST /v1/metrics/external/<verb>` per architecture Sec 3.12 and tech-spec C12 HMAC requirement.
- [ ] Verify `X-Forge-Signature` as HMAC-SHA256 of the request body using a per-source secret stored in `external_metric_source.secret_hash`.
- [ ] Reject requests with skewed timestamps (>5 minutes) per tech-spec Sec 8 webhook clock pin to prevent replay.
- [ ] Add a request body size limit and a per-source rate limit; return `429` on breach with `Retry-After`.
- [ ] Add `internal/webhook/server_test.go` covering valid signature, bad signature, missing signature, stale timestamp, and oversized body cases.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: bad-signature-rejected -- Given a request with an HMAC computed using a wrong secret, When it hits the webhook, Then the server returns `401` and no rows are written.
- [ ] Scenario: replay-window-rejected -- Given a request with a valid HMAC but timestamp 10 minutes old, When it hits the webhook, Then the server returns `400` with a `replay_window_exceeded` reason code.

## Stage 4.2: Cobertura coverage ingest verb

### Implementation Steps
- [ ] Implement `internal/webhook/coverage/cobertura.go` parsing Cobertura XML per operator pin `external-metric-coverage-format=Cobertura XML`.
- [ ] Map Cobertura packages and classes onto canonical `scope_id` values via the Stage 2.2 identity function; record unmappable rows in `external_metric_unmapped` for operator review.
- [ ] Persist coverage samples as `metric_kind='coverage_line'` and `metric_kind='coverage_branch'` per architecture Sec 1.4.
- [ ] Reject any payload not declared as Cobertura with `ErrUnsupportedCoverageFormat` rather than guessing.
- [ ] Add `internal/webhook/coverage/cobertura_test.go` with fixtures from the four v1 languages plus one malformed payload.

### Dependencies
- phase-external-metric-ingest-webhook/stage-webhook-transport-and-hmac-verification

### Test Scenarios
- [ ] Scenario: cobertura-mapped-rows-persist -- Given a valid Cobertura payload for a known repo+sha, When the webhook processes it, Then line and branch coverage samples are written under the correct `scope_id` and active rows are present.
- [ ] Scenario: non-cobertura-rejected -- Given a payload identified as `lcov` or `jacoco` XML, When the webhook processes it, Then it returns `400` with `unsupported_coverage_format` and writes zero rows.

## Stage 4.3: Test balance ingest verb

### Implementation Steps
- [ ] Implement `Metric.External.TestBalance` verb accepting per-scope unit/integration/e2e counts per architecture Sec 3.12.
- [ ] Persist as `metric_kind='test_balance_unit'`, `test_balance_integration`, `test_balance_e2e`; one sample per kind per scope.
- [ ] Validate that the sum of declared counts matches a top-level total field so silent under-reporting is rejected.
- [ ] Add `internal/webhook/balance/balance_test.go` covering canonical, sum-mismatch, and negative-count cases.

### Dependencies
- phase-external-metric-ingest-webhook/stage-webhook-transport-and-hmac-verification

### Test Scenarios
- [ ] Scenario: balance-counts-persist -- Given counts {unit=120, integration=30, e2e=5, total=155}, When the verb runs, Then three samples are written with the expected values.
- [ ] Scenario: sum-mismatch-rejected -- Given counts {unit=120, integration=30, e2e=5, total=200}, When the verb runs, Then it returns `400 sum_mismatch` and writes zero rows.

## Stage 4.4: Churn ingest pipeline

### Implementation Steps
- [ ] Implement `Metric.External.Churn` consuming per-file additions/deletions and mapping to canonical scopes via aggregation rules in architecture Sec 3.12.
- [ ] Emit metric kinds `churn_lines_added_window` and `churn_lines_deleted_window` for windows {30d, 90d}.
- [ ] Honour the writer-ownership rule (`metric_ingestor`) -- never let the webhook handler bypass into the cross-repo writer.
- [ ] Add tests covering empty payloads, file-not-mapped, and very-large-batch cases.

### Dependencies
- phase-external-metric-ingest-webhook/stage-webhook-transport-and-hmac-verification

### Test Scenarios
- [ ] Scenario: churn-window-aggregation -- Given churn events spanning 45 days, When the 30d kind is computed, Then it sums only events <=30 days old and the 90d kind sums all 45 days of events.
- [ ] Scenario: unmapped-files-recorded -- Given a churn payload referencing files not present in any `ScopeBinding`, When the verb runs, Then the unmapped rows are written to `external_metric_unmapped` and a 207-style summary indicates partial success.

## Stage 4.5: Defects ingest store only

### Implementation Steps
- [ ] Implement `Metric.External.Defects` per architecture Sec 3.12 storing defect rates per scope without driving any gate behaviour (store-only).
- [ ] Persist as `metric_kind='defect_density'` with `confidence='low'` since the source signal is operator-supplied.
- [ ] Document explicitly in code and in the runbook that defect metrics are NOT consumed by `eval.gate` so operators do not assume they affect CI.
- [ ] Add tests covering happy-path persistence and the explicit no-op for the gate path.

### Dependencies
- phase-external-metric-ingest-webhook/stage-webhook-transport-and-hmac-verification

### Test Scenarios
- [ ] Scenario: defects-persist-with-low-confidence -- Given a defects payload, When the verb runs, Then samples are written with `confidence='low'` and active rows are present.
- [ ] Scenario: defects-do-not-affect-gate -- Given a repo with a high defect density sample but no other rule failures, When `eval.gate` evaluates the commit, Then it returns `pass` (defects are store-only).

# Phase 5: Policy Steward and SOLID Rule Engine

## Dependencies
- phase-foundation-and-schema

## Stage 5.1: Policy Steward signing key store

### Implementation Steps
- [ ] Implement `internal/policy/keys.go` storing Ed25519 keypairs per architecture Sec 3.11 / tech-spec C13.
- [ ] Persist private keys encrypted-at-rest via the KMS provider abstraction in `internal/crypto/`; public keys persisted in cleartext in `policy.signing_key`.
- [ ] Expose a key-rotation verb that issues a new keypair and marks the old key as `rotating` (still valid for verification, no longer used for signing).
- [ ] Add `internal/policy/keys_test.go` covering generation, rotation, and refusal to sign with a revoked key.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: revoked-key-cannot-sign -- Given a key with `status='revoked'`, When the steward attempts to sign, Then it returns `ErrKeyRevoked` and no signature is produced.
- [ ] Scenario: rotating-key-verifies-still -- Given a `rotating` key, When an existing signed RulePack is verified, Then verification succeeds.

## Stage 5.2: Policy publish activate and rulepack verbs

### Implementation Steps
- [ ] Implement `Policy.Publish(rule_pack_revision)` signing the canonical bytes with the active Ed25519 key per operator pin `policy-signing-required=v1 required`.
- [ ] Implement `Policy.Activate(revision_id)` transitioning a published revision to active, with at most one active revision per pack at any time.
- [ ] Reject `Policy.Activate` if the signature does not verify against any non-revoked key.
- [ ] Add audit events `policy.publish` and `policy.activate` carrying `revision_id`, `signing_key_id`, and the operator principal.
- [ ] Add `internal/policy/lifecycle_test.go` covering publish-without-signature, activate-with-bad-signature, and concurrent-activate cases.

### Dependencies
- phase-policy-steward-and-solid-rule-engine/stage-policy-steward-signing-key-store

### Test Scenarios
- [ ] Scenario: publish-requires-signature -- Given an unsigned RulePack revision, When `Policy.Publish` runs without a key, Then it returns `ErrSignatureRequired`.
- [ ] Scenario: activate-rejects-bad-signature -- Given a revision with a tampered `signature_bytes`, When `Policy.Activate` runs, Then it returns `ErrSignatureInvalid` and no row updates.

## Stage 5.3: Override append only mute lifecycle

### Implementation Steps
- [ ] Implement `policy_override` writer enforcing append-only semantics per architecture Sec 5.5: mutes are never updated or deleted, only superseded by a new row.
- [ ] Verbs: `Policy.Override.Add(scope, rule_id, expires_at, reason)`, `Policy.Override.Lift(override_id, reason)` (writes a successor row referencing the lifted override).
- [ ] Reject `Override.Add` with `expires_at` > max-mute-age pinned in tech-spec Sec 8.
- [ ] Surface a daily `aged_mute_report` projection counting overrides nearing expiry (consumed by Stage 10.2 insights report).
- [ ] Add `internal/policy/override_test.go` covering add, lift, and the rejection of UPDATE/DELETE on rows.

### Dependencies
- phase-policy-steward-and-solid-rule-engine/stage-policy-publish-activate-and-rulepack-verbs

### Test Scenarios
- [ ] Scenario: override-append-only -- Given an override row, When any role attempts UPDATE or DELETE, Then PostgreSQL grants reject the operation.
- [ ] Scenario: max-mute-age-enforced -- Given `Override.Add` with `expires_at` 2 years out and the pin set to 90 days, When the verb runs, Then it returns `ErrMaxMuteAgeExceeded` and no row is written.

## Stage 5.4: Predicate DSL evaluator

### Implementation Steps
- [ ] Implement the predicate DSL per architecture Sec 3.6: comparison ops, logical ops, percentile lookups, and a fixed set of metric kind operands.
- [ ] Wire a parser, AST, and stateless evaluator under `internal/policy/predicate/`.
- [ ] Disallow free-form code execution -- evaluator MUST refuse any operand not declared in the rule pack's `allowed_metrics` list (architecture invariant: no escape hatches in policy code).
- [ ] Add fuzz tests under `internal/policy/predicate/predicate_fuzz_test.go` to confirm the parser refuses garbage and never panics.

### Dependencies
- phase-policy-steward-and-solid-rule-engine/stage-policy-publish-activate-and-rulepack-verbs

### Test Scenarios
- [ ] Scenario: predicate-evaluates-known-operands -- Given predicate `cyclomatic_complexity > p95.system`, When evaluated against a scope where `cyclomatic_complexity=12` and `p95.system=10`, Then the result is `true`.
- [ ] Scenario: unknown-operand-rejected -- Given a predicate referencing `mystery_metric`, When parsed, Then the parser returns `ErrUnknownOperand` and the rule pack fails validation.

## Stage 5.5: SOLID rule packs

### Implementation Steps
- [ ] Author shipping rule packs `solid.go.v1`, `solid.python.v1`, `solid.typescript.v1`, `solid.java.v1` encoding the principles from the story description as predicate DSL rules.
- [ ] Each pack ships with default thresholds keyed to system-tier percentiles (architecture Sec 1.6 default policy stance).
- [ ] Each rule carries a stable `rule_id`, a `severity` (`error`, `warn`, `info`), and a human-readable `remediation_hint`.
- [ ] Add `internal/policy/packs/solid_test.go` validating each pack parses, signs, and evaluates against the SOLID corpus from Stage 2.4.

### Dependencies
- phase-policy-steward-and-solid-rule-engine/stage-predicate-dsl-evaluator

### Test Scenarios
- [ ] Scenario: shipping-packs-parse-and-sign -- Given the four shipping SOLID packs, When they pass through `Policy.Publish` and `Policy.Activate`, Then all four become active without errors.
- [ ] Scenario: solid-corpus-triggers-expected-rules -- Given the per-principle negative fixtures from Stage 2.4, When `solid.<lang>.v1` evaluates, Then the corresponding `rule_id` fires for each fixture and the positive fixtures do not fire.

## Stage 5.6: Decoupled functional areas rule pack

### Implementation Steps
- [ ] Author `decoupling.v1` rule pack encoding the "decoupled functional areas" standard from the story description: forbid edges from `pack=infrastructure` to `pack=domain`, cap inbound-edge fanout per pack, flag cross-pack circular dependencies.
- [ ] Wire pack-level metadata into `ScopeBinding` (architecture Sec 5.2.4) so the rule pack has the pack assignments it needs.
- [ ] Add fixtures under `internal/policy/packs/decoupling/testdata/` with a clean architecture and a violating one; assert the rule pack fires only on the violating one.

### Dependencies
- phase-policy-steward-and-solid-rule-engine/stage-solid-rule-packs

### Test Scenarios
- [ ] Scenario: decoupling-fires-on-bad-edge -- Given a fixture where `infrastructure.db.go` calls `domain.user.go`, When `decoupling.v1` evaluates, Then a rule with `rule_id='decoupling.forbidden_edge'` fires with the offending edge in `evidence`.
- [ ] Scenario: clean-architecture-passes -- Given the clean-architecture fixture, When `decoupling.v1` evaluates, Then no rule fires.

## Stage 5.7: SOLID Rule Engine batch worker

### Implementation Steps
- [ ] Implement `internal/policy/engine/batch.go` per architecture Sec 3.6 evaluating all active rule packs against all `metric_sample_active` rows on a cadence pinned in tech-spec Sec 8.
- [ ] Persist results into `policy.rule_finding` with `(commit_id, scope_id, rule_id)` PK so re-runs are idempotent.
- [ ] Honour the writer split: only `solid_batch` may write `rule_finding`; `evaluator` reads but never writes.
- [ ] Add OTel span `policy.batch.evaluate` with attributes `pack`, `rules_evaluated`, `findings_written`, `duration_ms`.
- [ ] Add `internal/policy/engine/batch_test.go` covering happy-path, idempotency on re-run, and pack-disabled cases.

### Dependencies
- phase-policy-steward-and-solid-rule-engine/stage-decoupled-functional-areas-rule-pack

### Test Scenarios
- [ ] Scenario: batch-idempotent -- Given a finished batch run that wrote N findings, When the same batch re-runs against unchanged data, Then it writes 0 new findings and updates 0 rows.
- [ ] Scenario: writer-isolation -- Given role `evaluator`, When it attempts INSERT into `rule_finding`, Then PostgreSQL grants reject the call.

# Phase 6: Evaluator Surface and Management Surface

## Dependencies
- phase-repo-indexer-and-metric-ingestor
- phase-policy-steward-and-solid-rule-engine

## Stage 6.1: Evaluator gate verb and degraded handling

### Implementation Steps
- [ ] Implement `internal/evaluator/gate.go` exposing `eval.gate(repo, sha)` per architecture Sec 3.7, returning `{verdict: pass|fail|gated, findings, degraded_reason?}`.
- [ ] Treat any of the closed `degraded_reason` set (`xrepo_edges_unavailable`, `samples_pending`, `policy_signature_invalid`, `percentile_stale` -- tech-spec C21) as `warn` by default per operator pin `gate-degraded-policy=warn`.
- [ ] Surface a per-repo override on `gate-degraded-policy` (`warn|block`) without expanding the closed `degraded_reason` set.
- [ ] Emit audit events `evaluator.gate` carrying the verdict, every finding's `rule_id`, and any `degraded_reason`.
- [ ] Add `internal/evaluator/gate_test.go` covering pass, fail, gated, each degraded reason, and the per-repo override flip.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: gate-degraded-warns-by-default -- Given a repo with `degraded_reason='samples_pending'` and the operator pin at default, When `eval.gate` runs, Then verdict is `pass` with a `warn` annotation containing the degraded reason.
- [ ] Scenario: gate-blocks-when-override-set -- Given the same repo with the per-repo override set to `block`, When `eval.gate` runs, Then verdict is `gated` with the same `degraded_reason`.
- [ ] Scenario: unknown-degraded-reason-rejected -- Given a code path that tries to emit `degraded_reason='disk_full'`, When the gate response is constructed, Then it panics in tests (and returns `internal_error` in prod) because the value is not in the closed set.

## Stage 6.2: Management write verbs and repo onboarding

### Implementation Steps
- [ ] Implement Management verbs per architecture Sec 3.13 / Sec 6: `Repo.Register`, `Repo.SetPackEnabled`, `Repo.SetGateOverride`, `Repo.Decommission`.
- [ ] Wire each verb through the `management` role so RLS and grants enforce the writer split.
- [ ] Emit audit events `management.<verb>` carrying actor, target, before/after JSON-diffs.
- [ ] Add `internal/management/write_test.go` covering happy-path, unauthorised-actor, and idempotent-replay.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: register-emits-pending-commit -- Given `Repo.Register(url='https://github.com/x/y')`, When the verb completes, Then a `repo` row exists, a `commit` row at the default branch head exists with `scan_status='pending'`, and an audit event is recorded.
- [ ] Scenario: unauthorised-actor-rejected -- Given a request signed by a principal lacking the `management` role, When `Repo.Register` runs, Then it returns `ErrUnauthorised` and no rows are written.

## Stage 6.3: Management read verbs and insights projections

### Implementation Steps
- [ ] Implement read verbs `mgmt.read.repo`, `mgmt.read.commit`, `mgmt.read.findings`, `mgmt.read.metric_history` backed by the `mgmt_reader` and `insights_reader` roles per architecture Sec 3.8.
- [ ] Build materialised views for the insights surface: `insights.repo_summary`, `insights.scope_hotspots`, `insights.aged_mutes` (consumed by Stage 10.2).
- [ ] Refresh views on a cadence pinned in tech-spec Sec 8; expose a manual refresh verb gated on the `management` role.
- [ ] Add `internal/management/read_test.go` covering each verb's happy path and the role-restriction enforced by PostgreSQL grants.

### Dependencies
- phase-evaluator-surface-and-management-surface/stage-management-write-verbs-and-repo-onboarding

### Test Scenarios
- [ ] Scenario: read-verb-roles-enforced -- Given role `evaluator`, When it calls `mgmt.read.findings`, Then PostgreSQL grants deny the underlying SELECT and the verb returns `ErrUnauthorised`.
- [ ] Scenario: insights-views-refresh -- Given the materialised views are stale, When `mgmt.refresh_insights` runs, Then `pg_stat_user_tables.last_analyze` advances and the views return current rows.

## Stage 6.4: HTTP JSON gateway and OIDC auth

### Implementation Steps
- [ ] Implement `cmd/clean-coded/main.go` wiring an HTTP/JSON gateway over the verb registry per architecture Sec 9 public-contract.
- [ ] Enforce OIDC bearer-token auth using a JWKS URL from config; map subject claims to internal roles via `role_mapping` config.
- [ ] Apply per-route rate limits sized per tech-spec Sec 8 rate-limit pins.
- [ ] Add request-id propagation, OTel trace propagation, and structured access logs (architecture Sec 8).
- [ ] Add `cmd/clean-coded/main_test.go` (integration) booting the server against a test PostgreSQL and exercising one verb per surface end-to-end.

### Dependencies
- phase-evaluator-surface-and-management-surface/stage-evaluator-gate-verb-and-degraded-handling
- phase-evaluator-surface-and-management-surface/stage-management-read-verbs-and-insights-projections

### Test Scenarios
- [ ] Scenario: jwt-required -- Given a request without an Authorization header, When it hits any /v1/* route, Then the server returns `401`.
- [ ] Scenario: role-mapping-honoured -- Given a JWT whose `roles` claim maps to `evaluator`, When the bearer calls `POST /v1/eval/gate`, Then the request succeeds; when the same bearer calls `POST /v1/management/repo/register`, Then it returns `403`.

# Phase 7: Cross Repo Aggregator

## Dependencies
- phase-repo-indexer-and-metric-ingestor
- phase-external-metric-ingest-webhook

## Stage 7.1: Aggregator cadence loop and snapshot writers

### Implementation Steps
- [ ] Implement `internal/aggregator/loop.go` running on the 15-minute cadence pinned in architecture Sec 1.6 / tech-spec Sec 8.
- [ ] Snapshot per-system percentiles for every active metric kind into `metric_sample` rows with `pack='system'`, written via the `cross_repo_aggregator` role only.
- [ ] Honour the RLS carve-out from Stage 1.5 so `metric_ingestor` cannot write `pack='system'` rows even by mistake.
- [ ] Add a Prometheus gauge `aggregator_cycle_latency_seconds` and an OTel span `aggregator.cycle`.
- [ ] Add `internal/aggregator/loop_test.go` with a fake clock covering on-cadence, missed-cadence, and overlapping-cycle cases.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: cadence-15-minutes -- Given the loop started at T=0, When the fake clock advances 45 minutes, Then exactly 3 cycles ran.
- [ ] Scenario: overlapping-cycle-prevented -- Given a cycle that runs longer than the cadence, When the next tick fires, Then the second cycle is skipped and a `aggregator_overlap_skipped_total` counter increments.

## Stage 7.2: System tier metric composer

### Implementation Steps
- [ ] Implement composers per architecture Sec 3.10 emitting `p50.system`, `p90.system`, `p95.system`, `p99.system` for every metric kind that opts in via the pack registry.
- [ ] Persist composer outputs to `metric_sample` with `pack='system'`, `scope_id` set to the synthetic `system` scope, and `metric_version` reflecting the composer version.
- [ ] Surface a debugging verb `mgmt.read.system_percentiles(metric_kind, asof)` returning the most recent system-tier values.
- [ ] Add `internal/aggregator/composer_test.go` covering an empty-input run (no rows persisted), a 1-repo run, and a 100-repo run.

### Dependencies
- phase-cross-repo-aggregator/stage-aggregator-cadence-loop-and-snapshot-writers

### Test Scenarios
- [ ] Scenario: composer-emits-four-percentiles -- Given 100 repos with samples for `cyclomatic_complexity`, When the composer runs, Then 4 system-tier samples are written for that kind.
- [ ] Scenario: empty-input-no-writes -- Given a metric kind with zero samples across all repos, When the composer runs, Then zero `pack='system'` rows are written.

## Stage 7.3: Percentile freshness and degraded stamping

### Implementation Steps
- [ ] Implement a freshness check stamping `degraded_reason='percentile_stale'` on `eval.gate` responses when the latest system-tier sample is older than the freshness threshold (tech-spec C21 / Sec 8).
- [ ] Implement `degraded_reason='xrepo_edges_unavailable'` for the case when the aggregator has failed for >2 cadences.
- [ ] Surface freshness as a Prometheus gauge `system_percentile_age_seconds` per metric kind.
- [ ] Add `internal/aggregator/freshness_test.go` covering fresh, stale, and aggregator-down cases.

### Dependencies
- phase-cross-repo-aggregator/stage-system-tier-metric-composer

### Test Scenarios
- [ ] Scenario: stale-percentile-warns-gate -- Given the latest `pack='system'` row is 6h old and the threshold is 1h, When `eval.gate` runs, Then the response includes `degraded_reason='percentile_stale'`.
- [ ] Scenario: aggregator-down-marks-xrepo-edges -- Given two consecutive failed cadences, When `eval.gate` runs against any repo, Then the response includes `degraded_reason='xrepo_edges_unavailable'`.

### Cross-Stage Dependencies
- phase-evaluator-surface-and-management-surface/stage-evaluator-gate-verb-and-degraded-handling

# Phase 8: Refactor Planner

## Dependencies
- phase-policy-steward-and-solid-rule-engine
- phase-cross-repo-aggregator

## Stage 8.1: Composite hotspot scoring

### Implementation Steps
- [ ] Implement `internal/refactor/score.go` per architecture Sec 3.9 combining `cyclomatic_complexity`, `cognitive_complexity`, `modification_count_in_window`, `coverage_line` deficit, and SOLID rule findings into a composite `hotspot_score`.
- [ ] Weight each input per the table in architecture Sec 3.9; weights configurable but capped to a closed set so operators cannot invent new metric kinds.
- [ ] Persist hotspot scores into `refactor.hotspot_score` keyed by `(repo_id, sha, scope_id)`.
- [ ] Add `internal/refactor/score_test.go` covering the worked example from architecture Sec 3.9 plus edge cases (missing inputs treated as 0 with `confidence='low'`).

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: composite-matches-worked-example -- Given the architecture Sec 3.9 worked example inputs, When `score` runs, Then the output matches the expected composite to 2 decimals.
- [ ] Scenario: missing-inputs-lower-confidence -- Given a scope with no churn data, When `score` runs, Then the output carries `confidence='low'` rather than excluding the scope.

## Stage 8.2: Refactor plan and task generation

### Implementation Steps
- [ ] Implement `internal/refactor/plan.go` materialising a `RefactorPlan` per architecture Sec 3.9 with N highest-scoring scopes, grouped by pack and by call-graph proximity.
- [ ] Generate per-scope `RefactorTask` rows carrying `rule_id`, `remediation_hint` from the rule pack, and an `effort_estimate` slot to be filled by Stage 8.3.
- [ ] Persist into `refactor.refactor_plan` and `refactor.refactor_task` via the `refactor_planner` role.
- [ ] Add a verb `refactor.plan.get(repo, sha)` returning the most recent plan for a commit.
- [ ] Add `internal/refactor/plan_test.go` covering plan generation, grouping, and the verb's read path.

### Dependencies
- phase-refactor-planner/stage-composite-hotspot-scoring

### Test Scenarios
- [ ] Scenario: top-n-respected -- Given a configured `N=20`, When `plan` runs against a repo with 1000 candidate scopes, Then exactly 20 `RefactorTask` rows are produced.
- [ ] Scenario: tasks-cite-remediation -- Given each generated task, When inspected, Then its `remediation_hint` matches the corresponding rule pack's hint and `rule_id` is non-empty.

## Stage 8.3: ML effort model loader and version pinning

### Implementation Steps
- [ ] Implement `internal/refactor/effort/ml.go` per operator pin `refactor-effort-source=ML model from historical commits`: load a versioned model artefact from object storage, key inputs match architecture Sec 3.9's feature list.
- [ ] Refuse to start if no model artefact is configured (no silent fallback to heuristics) -- operators expect ML by pin.
- [ ] Stamp `effort_model_version` on every `effort_estimate` row so historical plans remain interpretable across model upgrades.
- [ ] Add a model-hash check in `/readyz` so an unexpected artefact hash fails readiness rather than silently using the wrong model.
- [ ] Add `internal/refactor/effort/ml_test.go` covering load, predict on the worked example from architecture Sec 3.9, version stamping, and the no-model startup refusal.

### Dependencies
- phase-refactor-planner/stage-refactor-plan-and-task-generation

### Test Scenarios
- [ ] Scenario: no-model-fails-startup -- Given the config has no model artefact URI, When `clean-coded` boots, Then it exits non-zero with `ErrEffortModelMissing` rather than starting with a heuristic.
- [ ] Scenario: model-version-stamped -- Given a successful plan generation, When `effort_estimate` rows are inspected, Then `effort_model_version` matches the loaded artefact's manifest version.

# Phase 9: Audit WAL and reliability hardening

## Dependencies
- phase-evaluator-surface-and-management-surface

## Stage 9.1: Audit WAL frame writer

### Implementation Steps
- [ ] Implement `internal/audit/wal/writer.go` per architecture Sec 7.1: every state-changing verb appends a frame to `audit_wal_frame` BEFORE its target table write commits.
- [ ] Use a single `audit_wal_lsn` SEQUENCE for monotonic ordering; frames carry `verb`, `actor`, `request_id`, `payload_json`, `target_table`, `target_pk`.
- [ ] Couple frame write and target write in a single transaction so a frame without a target write is structurally impossible.
- [ ] Add `internal/audit/wal/writer_test.go` covering writer happy path, abort cases, and ordering across concurrent writers.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: frame-precedes-target -- Given a successful verb, When the audit WAL and target table are queried, Then the frame's LSN is <= the target write's commit timestamp and both exist.
- [ ] Scenario: aborted-verb-no-frame -- Given a verb that aborts mid-transaction, When the WAL is queried, Then no frame for that request_id exists.

## Stage 9.2: Audit WAL Reconciler replay only

### Implementation Steps
- [ ] Implement `internal/audit/reconciler/` per architecture Sec 7.10 that periodically replays WAL frames into the user-facing `audit_event` projections.
- [ ] The reconciler is replay-only: it never mutates source-of-truth tables; if a projection diverges from the WAL it logs and surfaces a metric, then halts replay until an operator acks.
- [ ] Track replay cursor in `audit.reconciler_cursor` with a single row keyed by reconciler name.
- [ ] Add `internal/audit/reconciler/reconciler_test.go` covering happy replay, divergence-halt, and resume-after-ack.

### Dependencies
- phase-audit-wal-and-reliability-hardening/stage-audit-wal-frame-writer

### Test Scenarios
- [ ] Scenario: replay-advances-cursor -- Given 100 WAL frames, When the reconciler runs, Then `reconciler_cursor.last_lsn` advances to the highest replayed LSN.
- [ ] Scenario: divergence-halts-replay -- Given a `audit_event` row that does not match the WAL frame at the same LSN, When the reconciler reaches it, Then it stops advancing the cursor and emits `audit_reconciler_diverged_total{name="default"}`.

## Stage 9.3: AST subprocess isolation and mode flip safety

### Implementation Steps
- [ ] Implement an optional `subprocess` AST mode per architecture Sec 3.2 fallback path, behind feature flag `ast_mode='subprocess'`.
- [ ] Default remains `embedded` per operator pin `ast-mode-default=embedded`; flipping the mode requires an operator action recorded in audit.
- [ ] Wire a process-supervision wrapper that bounds memory/CPU per subprocess and recycles after N parses.
- [ ] Validate that switching modes produces byte-identical `CanonicalAst` outputs across the entire corpus from Stage 2.1 (Forge invariant: mode flip must be observation-equivalent).
- [ ] Add `internal/ast/mode_flip_test.go` running the equivalence check.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: mode-flip-observation-equivalent -- Given the corpus from Stage 2.1, When parsed in `embedded` and again in `subprocess` mode, Then the serialised `CanonicalAst` bytes are identical for every fixture.
- [ ] Scenario: subprocess-recycle -- Given the subprocess parses N+1 files where N is the recycle threshold, When the (N+1)th parse runs, Then a new subprocess is spawned and the old one is reaped.

## Stage 9.4: OTel telemetry across all surfaces

### Implementation Steps
- [ ] Add OTel auto-instrumentation across HTTP, PostgreSQL, and outbound HTTP per architecture Sec 8.
- [ ] Add hand-rolled spans on every verb (`indexer.*`, `ingestor.*`, `policy.*`, `evaluator.*`, `management.*`, `aggregator.*`, `refactor.*`, `webhook.*`).
- [ ] Add a closed-set `error.code` attribute mirroring the closed `degraded_reason` set so dashboards can pivot on a small enum.
- [ ] Publish a Grafana dashboard JSON under `deploy/local/grafana/clean-code.json` consuming the metrics defined in earlier stages.
- [ ] Add `internal/telemetry/otel_test.go` asserting every verb emits a span and that error spans carry an `error.code` attribute from the closed set.

### Dependencies
- phase-audit-wal-and-reliability-hardening/stage-audit-wal-frame-writer

### Test Scenarios
- [ ] Scenario: every-verb-emits-span -- Given the verb registry, When the test fixture invokes each verb, Then OTel records exactly one span per invocation with the verb name as the span name.
- [ ] Scenario: error-spans-use-closed-set -- Given a verb that errors with `samples_pending`, When the span is inspected, Then `error.code='samples_pending'` and no free-form error strings appear on the span.

# Phase 10: Linked mode integration and rollout

## Dependencies
- phase-refactor-planner
- phase-audit-wal-and-reliability-hardening

## Stage 10.1: Optional agent memory linked mode adapter

### Implementation Steps
- [ ] Implement `internal/linked/agent_memory.go` per architecture Sec 3.13 optional linked mode: when configured, push selected `mgmt.read.*` projections into the agent-memory store as facts.
- [ ] Make the adapter strictly read-from-clean-code, write-to-agent-memory; never reach back into agent-memory's source tables.
- [ ] Reuse the existing agent-memory HTTP/JSON contract from `services/agent-memory/proto/`; no schema changes there.
- [ ] Gate the adapter behind config flag `linked_mode='agent_memory'`; default is unset (no linked mode).
- [ ] Add `internal/linked/agent_memory_test.go` covering the configured-on path, configured-off path, and the agent-memory-unreachable retry path.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: linked-mode-off-no-traffic -- Given `linked_mode` unset, When clean-coded runs for an hour, Then zero requests are made to the agent-memory endpoint.
- [ ] Scenario: linked-mode-on-pushes-facts -- Given `linked_mode='agent_memory'` and a fresh `mgmt.read.repo_summary` row, When the adapter cycle runs, Then a corresponding fact appears in agent-memory and a Prometheus counter `linked_mode_facts_pushed_total` increments.

## Stage 10.2: Aged mute insights report

### Implementation Steps
- [ ] Implement a daily report consuming `insights.aged_mutes` (Stage 6.3) per architecture Sec 3.8 insights surface.
- [ ] Surface report via `mgmt.read.aged_mutes_report` returning per-repo overrides nearing expiry, with severity bands keyed to the pin from tech-spec Sec 8.
- [ ] Emit a daily Prometheus counter `aged_mutes_report_generated_total` plus an OTel span `insights.aged_mutes.run`.
- [ ] Add `internal/insights/aged_mutes_test.go` covering empty data, near-expiry, and already-expired cases.

### Dependencies
- phase-linked-mode-integration-and-rollout/stage-optional-agent-memory-linked-mode-adapter

### Test Scenarios
- [ ] Scenario: report-flags-near-expiry -- Given overrides expiring in 5 days and the warn band at 7 days, When the report runs, Then those overrides appear in the report with `severity='warn'`.
- [ ] Scenario: already-expired-excluded -- Given an override with `expires_at` 1 day in the past, When the report runs, Then it is excluded (the gate already treats it as inactive).

## Stage 10.3: Load and conformance tests

### Implementation Steps
- [ ] Author a `tests/load/` harness using `vegeta` against a docker-compose stack: 1000 repos, 100 active rule packs, 50 RPS to `eval.gate` for one hour.
- [ ] Author a conformance test suite under `tests/conformance/` walking every public verb in architecture Sec 9, asserting request/response shapes match the proto contracts.
- [ ] Wire both suites into the CI workflow as a nightly job (not blocking PRs but blocking release tags).
- [ ] Record load-test SLOs into `docs/stories/code-intelligence-CLEAN-CODE/runbook-roles.md` (extending the Stage 1.5 stub).

### Dependencies
- phase-linked-mode-integration-and-rollout/stage-optional-agent-memory-linked-mode-adapter

### Test Scenarios
- [ ] Scenario: load-test-meets-slo -- Given the 1000-repo / 50-RPS profile, When the load test runs, Then p95 `eval.gate` latency is <500ms and error rate is <0.1%.
- [ ] Scenario: conformance-covers-every-verb -- Given the verb list extracted from `proto/clean_code/v1/*.proto`, When the conformance suite runs, Then it exercises every verb at least once and asserts the response matches the proto descriptor.

## Stage 10.4: Cross repo end to end happy path

### Implementation Steps
- [ ] Author `tests/e2e/cross_repo_happy_path_test.go` registering 3 repos in 3 languages, ingesting commits, evaluating gates, generating refactor plans, and asserting cross-repo percentiles reflect all 3 inputs.
- [ ] Boot the full docker-compose stack (PostgreSQL, clean-coded, agent-memory placeholder, OTel collector) for the test.
- [ ] Assert the audit WAL records every state-changing verb and the reconciler projection matches.
- [ ] Wire into CI as a release-gating job.

### Dependencies
- phase-linked-mode-integration-and-rollout/stage-load-and-conformance-tests

### Test Scenarios
- [ ] Scenario: happy-path-end-to-end -- Given 3 fresh repos onboarded via `Repo.Register`, When commits ingest and the aggregator cycle runs once, Then `eval.gate` returns `pass` for each repo's HEAD and `mgmt.read.system_percentiles` reflects samples from all 3 repos.
- [ ] Scenario: audit-trail-matches -- Given the e2e run, When the WAL frames and `audit_event` projections are compared, Then every state-changing verb has matching rows in both and the reconciler cursor has advanced past the last frame.

### Cross-Stage Dependencies
- phase-cross-repo-aggregator/stage-system-tier-metric-composer
- phase-refactor-planner/stage-refactor-plan-and-task-generation
- phase-audit-wal-and-reliability-hardening/stage-audit-wal-reconciler-replay-only

## Stage 10.5: Rollout playbook and operator runbooks

### Implementation Steps
- [ ] Expand `docs/stories/code-intelligence-CLEAN-CODE/runbook-roles.md` with role grants, common queries, and incident playbooks for each closed `degraded_reason`.
- [ ] Author `docs/stories/code-intelligence-CLEAN-CODE/runbook-rollout.md` covering greenfield rollout, repo onboarding, pack enablement, and the operator pin change procedure (architecture Sec 1.6).
- [ ] Author `docs/stories/code-intelligence-CLEAN-CODE/runbook-incident.md` covering AST mode flip, key rotation, WAL divergence, and aggregator outage.
- [ ] Wire `make docs` to fail if any runbook contains non-ASCII characters (mirrors the plan-doc rule).
- [ ] Add a CI step that grep-validates every runbook references at least one anchor from `architecture.md` or `tech-spec.md` so runbooks do not drift from the design.

### Dependencies
- phase-linked-mode-integration-and-rollout/stage-cross-repo-end-to-end-happy-path

### Test Scenarios
- [ ] Scenario: runbooks-ascii-clean -- Given the three runbook files, When `grep -Pn '[^\x00-\x7E]'` runs against each, Then it produces zero matches.
- [ ] Scenario: runbooks-anchored-to-sibling-docs -- Given each runbook file, When the CI doc-validator runs, Then each file contains at least one reference of the form `architecture.md#` or `tech-spec.md#`.
