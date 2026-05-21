---
title: "clean code"
storyId: "code-intelligence:CLEAN-CODE"
---

> Livedoc: tick each `- [ ]` as work lands. Anchors (e.g. `phase-foundation-and-schema/stage-project-scaffold-and-ci-baseline`) are derived from the heading TITLE only -- no phase or stage numbers in slugs. Every entity name, schema name, enum value, verb name, and metric kind in this plan is the canonical name pinned by sibling `architecture.md` and `tech-spec.md`; do NOT introduce synonyms here.

> Canon alignment guarantees (audit these by `grep -F` if in doubt):
> - **ONE PostgreSQL schema:** `clean_code`. Words like `catalog`, `lifecycle`, `measurement`, `policy`, `audit`, `refactor` appear ONLY as migration file-name fragments and as logical table-grouping prose -- NEVER as schema names. There is no `catalog.`, `measurement.`, `policy.`, `audit.`, or `refactor.` schema and no qualified table reference of that form anywhere below.
> - **No invented tables:** `rule_pack_revision`, `policy_override`, `audit_event`, `audit_anchor`, `effort_estimate` are NEVER created. Each one is explicitly excluded with a negative migration step and a regression-guard test scenario.
> - **No procedural verbs on Measurement:** active-row uniqueness is a PARTIAL UNIQUE INDEX, not a `swap_active` function/trigger/procedure. The literal string `swap_active` appears only in negative ("do NOT use") clauses.
> - **Canonical state enums:** `Commit.scan_status` is exactly `pending | scanning | scanned | failed`; `ScanRun.status` is exactly `running | succeeded | failed`. Values `complete`, `superseded`, `queued`, `orphaned` are NEVER used as state values (they appear only in test scenarios that ASSERT the DB rejects them).
> - **Canonical 12 foundation metric_kinds (architecture Sec 1.4.1 / tech-spec Sec 4.1):** `cyclo`, `cognitive_complexity`, `loc`, `lcom4`, `fan_in`, `fan_out`, `depth_of_inheritance`, `interface_width`, `coupling_between_objects`, `cycle_member`, `duplication_ratio`, `modification_count_in_window`. Names `cyclomatic_complexity`, `lines_of_code`, `function_length`, `parameter_count`, `nesting_depth` are NEVER emitted (they appear only in negative clauses or as DSL-rejection test inputs).
> - **Canonical 3 ingested metric_kinds:** `coverage_line_ratio`, `coverage_branch_ratio`, `pass_first_try_ratio`. Names `coverage_line`, `coverage_branch`, test-balance count metrics, churn window metrics, and `defect_density` are NEVER written as MetricSample rows. Defects in v1 write NO MetricSample (only a `scan_run` audit row); churn feeds the `modification_count_in_window` materialiser only.
> - **Canonical 7 system-tier metric_kinds:** `xrepo_dep_depth`, `arch_debt_ratio`, `velocity_trend`, `arch_fitness`, `blast_radius`, `xservice_test_reliability`, `knowledge_index`. Names `p50.system`, `p90.system`, `p95.system`, `p99.system` are NEVER metric_kinds -- percentile vectors are COLUMNS (`p50`, `p90`, `p99`, `histogram_json`, `built_at`) on the `cross_repo_percentile` snapshot table.
> - **Canonical Override lifecycle:** the `override` table has NO `expires_at` column, NO TTL, NO max-mute-age timer (tech-spec Sec 10A pins v1 to latest-row-wins via append `mute=false`). Verbs are `mgmt.override` only; names `Policy.Override.Add`, `Policy.Override.Lift` are NEVER used.
> - **Canonical verdict:** `pass | warn | block`. The string `pass|fail|gated` and the values `fail`, `gated` are NEVER returned by `eval.gate` (they appear only in negative DB CHECK assertions). Degraded conditions map to `verdict='warn'` with a separate `degraded=true` boolean and a `degraded_reason` from the closed set.
> - **Percentile freshness banner is INSIGHTS-ONLY:** `degraded_reason='percentile_stale'` is attached to `mgmt.read.cross_repo` / `mgmt.read.portfolio` envelopes; `eval.gate` REJECTS this reason at validator level. The string `eval.gate` and `percentile_stale` never co-occur as a positive emission.
> - **Audit WAL scope:** the WAL writer is referenced ONLY from `internal/evaluator/` and `internal/rule_engine/` and durably backs ONLY `evaluation_run`, `evaluation_verdict`, `finding`. Catalog, Measurement, Policy, and Refactor writes do NOT route through the WAL. There is no `audit_event` or `audit_anchor` projection table.
> - **Markdown dependencies:** `### Cross-Stage Dependencies` blocks are NOT used anywhere in this plan -- every upstream dep lives in the regular `### Dependencies` block of the stage that needs it. Stage 2.4 does NOT depend on Stage 2.6; the OCP RULE that consumes `modification_count_in_window` lives in Stage 5.5, which correctly lists `stage-modification-count-in-window-materialiser` in its `### Dependencies`.

# Phase 1: Foundation and schema

## Dependencies
- _none -- start phase_

## Stage 1.1: Project scaffold and CI baseline

### Implementation Steps
- [ ] Create `services/clean-code/` Go module mirroring `services/agent-memory/` layout: `cmd/clean-coded/`, `internal/`, `pkg/`, `proto/`, `migrations/`, `deploy/local/`, `web/`, plus root `Makefile`, `go.mod`, `README.md`.
- [ ] Wire `Makefile` targets `build`, `test`, `lint`, `proto`, `migrate-up`, `migrate-down`, `docker-build`, `compose-up`, `compose-down` matching the agent-memory precedent so CI can reuse the same recipe shape.
- [ ] Add `.github/workflows/clean-code-ci.yml` running `make lint test` on PR plus build of the `clean-coded` container, mirroring `agent-memory-ci.yml`.
- [ ] Add `services/clean-code/deploy/local/docker-compose.yml` with PostgreSQL 16 (per tech-spec C9 / Sec 8.1) and a placeholder `clean-coded` service; expose Prometheus scrape port and OTel collector endpoint.
- [ ] Add `services/clean-code/internal/config/` loader that reads env + file config, exposing the operator pins listed in architecture Sec 1.6 (`ast-mode-default`, `external-metric-coverage-format`, `gate-degraded-policy`, `policy-signing-required`, `refactor-effort-source`) as typed fields with defaults from tech-spec Sec 8.2.
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
- [ ] Add `migrations/0001_catalog_lifecycle.up.sql` creating the single PostgreSQL schema `clean_code` (per tech-spec C9 / Sec 8.1.3) and the Catalog + Lifecycle tables `repo`, `commit`, `metric_kind`, `repo_event`, `scan_run` with primary keys and foreign keys matching architecture Sec 5.1.
- [ ] Add matching `migrations/0001_catalog_lifecycle.down.sql` that drops the `clean_code` schema cleanly so local-dev resets are deterministic.
- [ ] Encode `Commit.scan_status` as the canonical enum `pending | scanning | scanned | failed` with column-level `DEFAULT 'pending'` (per architecture Sec 5.1.2) and add a column COMMENT naming the Metric Ingestor as the sole writer.
- [ ] Encode `ScanRun.status` as the canonical enum `running | succeeded | failed` (per architecture Sec 5.7) and `ScanRun.kind` as `full | delta | external_single | external_per_row | retract`; add a `sha_binding` column with enum `single | per_row` defaulting to `single`.
- [ ] Encode `RepoEvent.kind` as the canonical enum `register | retract_intent | rescan_intent | mode_change` matching architecture Sec 5.1.3.
- [ ] Add table COMMENTs on each Catalog/Lifecycle table naming the owning sub-store and the writers permitted by architecture G1.
- [ ] Add indexes required by Repo Indexer hot reads: `UNIQUE(repo_id, sha)` on `commit`, `(repo_id, default_branch_head)` on `repo`, `(repo_id, started_at DESC)` on `scan_run`.
- [ ] Add a `go test` migration round-trip helper under `internal/storage/migrate_test.go` asserting `up` then `down` returns an empty `clean_code` schema (no leftover tables).

### Dependencies
- phase-foundation-and-schema/stage-project-scaffold-and-ci-baseline

### Test Scenarios
- [ ] Scenario: catalog-up-down -- Given an empty PostgreSQL 16 instance, When `make migrate-up` then `make migrate-down` runs, Then both succeed and `\dt clean_code.*` returns zero rows after the second run.
- [ ] Scenario: scan-status-enum-rejects-invalid -- Given the `commit` table after `migrate-up`, When an INSERT supplies `scan_status='garbage'` or `scan_status='complete'`, Then PostgreSQL rejects it with an enum constraint violation (only `pending|scanning|scanned|failed` are accepted).
- [ ] Scenario: scan-status-default-pending -- Given the `commit` table, When a row is INSERTed without a `scan_status` value, Then the row materialises with `scan_status='pending'` from the column DEFAULT.
- [ ] Scenario: scan-run-status-enum -- Given the `scan_run` table, When an INSERT supplies `status='orphaned'` or `status='complete'`, Then it fails (only `running|succeeded|failed` are accepted).

## Stage 1.3: Measurement schema and active row index

### Implementation Steps
- [ ] Add `migrations/0002_measurement.up.sql` creating Measurement tables in the same `clean_code` schema: `metric_sample`, `metric_retraction`, `scope_binding`, `repo_metric_snapshot`, `cross_repo_percentile`, `portfolio_snapshot`, matching architecture Sec 5.2.
- [ ] Encode the canonical `MetricSample` columns `sample_id`, `repo_id`, `sha`, `scope_id`, `metric_kind`, `metric_version`, `pack`, `source`, `value`, `unit`, `policy_version_id`, `computed_at`, `scan_run_id` (architecture Sec 5.2.2) and store `MetricKind.metric_version` as part of the row (no separate dimension table needed).
- [ ] Encode `MetricSample.pack` as enum `base | solid | system | ingested` and `MetricSample.source` as enum `computed | derived | ingested` (architecture Sec 1.4.1 / Sec 5.2.2).
- [ ] Implement active-row uniqueness as a PARTIAL UNIQUE INDEX `metric_sample_active_uq ON metric_sample (repo_id, sha, scope_id, metric_kind, metric_version) WHERE sample_id NOT IN (SELECT sample_id FROM metric_retraction)`, replacing any procedural "swap_active" idea -- architecture Sec 5.2.2 makes this an index invariant, not a stored procedure.
- [ ] Add `MetricRetraction(sample_id PK, retracted_at, retracted_by_scan_run_id, reason)` per architecture Sec 5.2.4; document append-only invariant in a table COMMENT.
- [ ] Add `ScopeBinding(scope_id, repo_id, sha, scope_kind, canonical_signature, parent_scope_id NULLABLE, file_path, span_start, span_end)` with the canonical `scope_kind` enum `function | method | class | interface | module | package` per architecture Sec 5.2.3.
- [ ] Add `RepoMetricSnapshot(repo_id, sha, metric_kind, p50, p90, p99, sample_count, built_at)` and `CrossRepoPercentile(metric_kind, scope_kind, p50, p90, p99, histogram_json, built_at)` and `PortfolioSnapshot(metric_kind, sum_value, weighted_mean, repo_count, built_at)` per architecture Sec 5.2.5 / 5.2.6.
- [ ] Add `migrations/0002_measurement.down.sql` reversing 0002.

### Dependencies
- phase-foundation-and-schema/stage-catalog-and-lifecycle-schema-migrations

### Test Scenarios
- [ ] Scenario: active-row-quintuple-uniqueness -- Given a `metric_sample` row already present for `(repo, sha, scope, metric_kind, metric_version)` and not retracted, When a second INSERT with the same quintuple runs, Then it fails with a unique-index violation; after inserting a matching `metric_retraction(sample_id)` the second INSERT succeeds.
- [ ] Scenario: pack-source-enum-rejects-invalid -- Given the `metric_sample` table, When an INSERT supplies `pack='unknown'` or `source='external'`, Then PostgreSQL rejects it; only canonical pack/source enums are accepted.
- [ ] Scenario: cross-repo-percentile-shape -- Given the `cross_repo_percentile` table, When the migration runs, Then it has exactly the columns p50, p90, p99, histogram_json, built_at (no `p95.system` or other invented kinds materialised as rows).

## Stage 1.4: Policy and Audit and Refactor schema migrations

### Implementation Steps
- [ ] Add `migrations/0003_policy_audit_refactor.up.sql` creating Policy tables `rule`, `rule_pack`, `policy_version`, `policy_activation`, `threshold`, `override` and Audit tables `evaluation_run`, `evaluation_verdict`, `finding` and Refactor tables `hot_spot`, `refactor_plan`, `refactor_task` -- all in the single `clean_code` schema (per tech-spec C9 / Sec 8.1.3). No invented tables (`rule_pack_revision`, `policy_override`, `audit_event`, `audit_anchor`, `effort_estimate` are NOT created -- they were rejected by iter 1 evaluator item 1).
- [ ] Encode `PolicyVersion` as immutable (append-only PK `policy_version_id`, columns `signature`, `signed_at`, `signing_key_id`, `rulepack_set_hash`, `refactor_weights JSONB`) per architecture Sec 5.3.3.
- [ ] Encode `PolicyActivation` as append-only with `(activation_id PK, scope, policy_version_id FK, activated_at, activated_by)` and a partial unique index enforcing one active row per scope per architecture Sec 5.3.4.
- [ ] Encode `Override(override_id PK, policy_version_id, scope, rule_id, mute BOOL, reason, created_at, created_by)` -- NO `expires_at` column and NO TTL enforcement, since tech-spec Sec 10A pins v1 to latest-row-wins (iter 1 evaluator item 5).
- [ ] Encode `EvaluationVerdict.verdict` as enum `pass | warn | block` (architecture Sec 5.4.2 / tech-spec Sec 4.8); add `degraded BOOL` and `degraded_reason TEXT` columns separately, with `degraded_reason` constrained to the closed set `xrepo_edges_unavailable | samples_pending | policy_signature_invalid | percentile_stale` per architecture Sec 5.5.
- [ ] Encode `Finding.delta` as enum `regression | improvement | flat | new | resolved` per architecture Sec 5.4.3.
- [ ] Encode `RefactorTask.kind` as enum `extract_function | split_class | introduce_interface | break_cycle | reduce_inheritance | reduce_coupling | reduce_lcom | reduce_duplication` matching architecture Sec 5.6.3 and add columns `task_id PK, plan_id FK, scope_id, rule_id, effort_hours, expected_metric_delta JSONB, status`.
- [ ] Add table COMMENTs on `evaluation_run`, `evaluation_verdict`, `finding` naming the Audit WAL as their durability mechanism and pointing at architecture Sec 7.10.
- [ ] Add `migrations/0003_policy_audit_refactor.down.sql` reversing 0003.

### Dependencies
- phase-foundation-and-schema/stage-catalog-and-lifecycle-schema-migrations

### Test Scenarios
- [ ] Scenario: verdict-enum-only-canonical -- Given the `evaluation_verdict` table, When an INSERT supplies `verdict='fail'` or `verdict='gated'`, Then PostgreSQL rejects it (only `pass|warn|block` are accepted) -- guards against iter 1 evaluator item 6 regression.
- [ ] Scenario: override-no-expires-column -- Given the `override` table after `migrate-up`, When `\d clean_code.override` runs, Then no `expires_at` column exists (guards iter 1 evaluator item 5).
- [ ] Scenario: degraded-reason-closed-set -- Given the `evaluation_verdict` table, When an INSERT supplies `degraded_reason='other'`, Then it fails the check constraint; the four canonical reasons (incl. `percentile_stale` reserved for Insights only) all succeed at column level.
- [ ] Scenario: policy-activation-one-active -- Given a `policy_activation` row with scope `org=acme` and `deactivated_at IS NULL`, When a second activation for the same scope with `deactivated_at IS NULL` is inserted, Then the partial unique index rejects it.

## Stage 1.5: PostgreSQL role grants and schema isolation

### Implementation Steps
- [ ] Add `migrations/0004_roles.up.sql` creating PostgreSQL roles `clean_code_repo_indexer`, `clean_code_metric_ingestor`, `clean_code_policy_steward`, `clean_code_rule_engine`, `clean_code_aggregator`, `clean_code_refactor_planner`, `clean_code_evaluator`, `clean_code_management`, `clean_code_audit_replayer` per architecture Sec 8.6.
- [ ] Grant Metric Ingestor sole INSERT/UPDATE on `metric_sample`, `metric_retraction`, and `commit.scan_status`; grant Repo Indexer INSERT on `commit` (initial row only) and `repo_event`.
- [ ] Grant Policy Steward sole INSERT on `rule`, `rule_pack`, `policy_version`, `policy_activation`, `threshold`, `override`.
- [ ] Grant SOLID Rule Engine sole INSERT on `finding` (Audit sub-store writer per architecture G1); grant Evaluator sole INSERT on `evaluation_run`, `evaluation_verdict`.
- [ ] Grant Cross Repo Aggregator sole INSERT on `repo_metric_snapshot`, `cross_repo_percentile`, `portfolio_snapshot` and sole INSERT on `metric_sample` rows where `pack='system'`.
- [ ] Grant Refactor Planner sole INSERT on `hot_spot`, `refactor_plan`, `refactor_task`.
- [ ] Grant Audit Replayer SELECT-only on Audit tables plus INSERT on the Audit tables for replay paths only.
- [ ] Add `internal/storage/roles_test.go` that connects as each role and verifies the writer-ownership matrix (e.g. evaluator INSERT into `finding` fails with permission denied).

### Dependencies
- phase-foundation-and-schema/stage-measurement-schema-and-active-row-index
- phase-foundation-and-schema/stage-policy-and-audit-and-refactor-schema-migrations

### Test Scenarios
- [ ] Scenario: role-isolation-matrix -- Given each writer role bound to a session, When that role attempts INSERT on a table outside its writer-ownership per architecture G1, Then PostgreSQL returns permission denied; the role's owned writes succeed.
- [ ] Scenario: aggregator-only-system-pack -- Given the aggregator role, When it attempts INSERT into `metric_sample` with `pack='base'`, Then the row-level grant denies the insert; with `pack='system'` it succeeds.

# Phase 2: AST Adapter and foundation tier compute

## Dependencies
- phase-foundation-and-schema

## Stage 2.1: Tree sitter parser fleet and canonical AST proto

### Implementation Steps
- [ ] Add `proto/ast/v1/ast.proto` defining `AstFile`, `AstScope`, `AstSymbol`, `AstEdge` with the canonical scope_kind enum `function | method | class | interface | module | package` so all recipes consume a single shape.
- [ ] Wire `make proto` to generate Go bindings into `internal/ast/v1/`.
- [ ] Add `internal/ast/parser/` Tree-sitter adapter wrapping the grammars for Go, TypeScript, Python, Java, C# (per architecture Sec 4.1.1) behind a `Parser` interface with `Parse(ctx, path, bytes) (*AstFile, error)`.
- [ ] Add `internal/ast/parser/registry.go` exposing per-language parser factories selected by file extension + Linguist-style content sniff.
- [ ] Add language-tagged unit tests under `internal/ast/parser/testdata/<lang>/` covering at least one fixture per supported language and asserting the canonical AST proto fields are populated.

### Dependencies
- phase-foundation-and-schema/stage-postgresql-role-grants-and-schema-isolation

### Test Scenarios
- [ ] Scenario: parser-supports-five-languages -- Given a fixture file per supported language, When the registry returns a parser and `Parse` runs, Then each returns a non-empty `AstFile` with the language tag set and at least one `AstScope`.
- [ ] Scenario: proto-round-trip -- Given a parsed `AstFile`, When it is serialised to protobuf wire format and deserialised, Then the resulting struct equals the original (no information loss).

## Stage 2.2: Scope identity derivation and ScopeBinding writer

### Implementation Steps
- [ ] Add `internal/ast/scope/identity.go` computing `scope_id` as a deterministic UUIDv5 of `(repo_id, sha, scope_kind, canonical_signature)` per architecture Sec 5.2.3.
- [ ] Add `internal/ast/scope/signature.go` building the canonical signature per scope kind (function: full path + parameter type list; class/interface: full path; module: file path; package: directory path) so two scans of the same SHA produce identical scope_ids.
- [ ] Add `internal/storage/scope_binding_writer.go` performing batched `INSERT ... ON CONFLICT (scope_id) DO NOTHING` into `scope_binding`, with explicit `parent_scope_id` linkage.
- [ ] Wire the writer behind the Metric Ingestor (only ingestor calls scope writer to satisfy writer-ownership G1).
- [ ] Add `internal/ast/scope/identity_test.go` verifying determinism (same inputs => same UUID) and uniqueness (different scope_kind or signature => different UUID).

### Dependencies
- phase-ast-adapter-and-foundation-tier-compute/stage-tree-sitter-parser-fleet-and-canonical-ast-proto

### Test Scenarios
- [ ] Scenario: scope-id-determinism -- Given the same `(repo_id, sha, scope_kind, canonical_signature)` invoked twice, When `DeriveScopeID` runs, Then it returns the same UUID.
- [ ] Scenario: scope-binding-idempotent-write -- Given a `scope_binding` row already present, When the writer re-inserts the same `scope_id`, Then no error surfaces and the row count remains unchanged.

## Stage 2.3: Base pack foundation recipes

### Implementation Steps
- [ ] Add `internal/metrics/recipes/` with one file per recipe, each implementing the `Recipe.Compute(ast *AstFile) []MetricSampleDraft` interface and tagging output with `pack='base'`, `source='computed'`, and the canonical `metric_kind` value.
- [ ] Implement `recipes/cyclo.go` emitting `metric_kind='cyclo'` (architecture Sec 1.4.1) at scope_kind `function|method`.
- [ ] Implement `recipes/cognitive_complexity.go` emitting `metric_kind='cognitive_complexity'` at scope_kind `function|method`.
- [ ] Implement `recipes/loc.go` emitting `metric_kind='loc'` at scope_kind `function|method|class|module`.
- [ ] Register the three recipes in `recipes/registry.go` keyed by metric_kind; emit a startup log line listing the registered base-pack recipes.
- [ ] Add table-driven tests `recipes/cyclo_test.go`, `recipes/cognitive_complexity_test.go`, `recipes/loc_test.go` against language-tagged fixtures with known expected values.

### Dependencies
- phase-ast-adapter-and-foundation-tier-compute/stage-scope-identity-derivation-and-scopebinding-writer

### Test Scenarios
- [ ] Scenario: base-recipes-only-canonical-kinds -- Given the recipe registry after init, When listing the registered metric_kinds for `pack='base'`, Then the result is exactly `{cyclo, cognitive_complexity, loc}` (no `cyclomatic_complexity`, `lines_of_code`, `function_length`, `parameter_count`, or `nesting_depth` -- iter 1 evaluator item 3).
- [ ] Scenario: cyclo-known-value -- Given a Go fixture function with two `if` branches and one `for` loop, When the cyclo recipe runs, Then it emits a `MetricSampleDraft(metric_kind='cyclo', value=4)` at the function scope.
- [ ] Scenario: loc-counts-physical-lines -- Given a 42-line Python module fixture, When the loc recipe runs at module scope, Then it emits `value=42`.

## Stage 2.4: SOLID pack foundation recipes

### Implementation Steps
- [ ] Implement `recipes/lcom4.go` emitting `metric_kind='lcom4'` (architecture Sec 1.4.1 SOLID pack) at scope_kind `class` per the four-component cohesion algorithm.
- [ ] Implement `recipes/fan_in.go` emitting `metric_kind='fan_in'` at scope_kind `module|package` counting inbound references from other modules.
- [ ] Implement `recipes/fan_out.go` emitting `metric_kind='fan_out'` at scope_kind `module|package` counting outbound references to other modules.
- [ ] Implement `recipes/depth_of_inheritance.go` emitting `metric_kind='depth_of_inheritance'` at scope_kind `class`.
- [ ] Implement `recipes/interface_width.go` emitting `metric_kind='interface_width'` at scope_kind `interface|class` (public surface size).
- [ ] Implement `recipes/coupling_between_objects.go` emitting `metric_kind='coupling_between_objects'` at scope_kind `class`.
- [ ] Register all six in `recipes/registry.go` keyed by metric_kind with `pack='solid'`, `source='computed'`; emit a startup log listing them.
- [ ] Add language-tagged fixture tests asserting each recipe emits exactly the canonical `metric_kind` value (no `cyclomatic_complexity` / `lines_of_code` aliases anywhere; iter 1 evaluator item 3).

### Dependencies
- phase-ast-adapter-and-foundation-tier-compute/stage-base-pack-foundation-recipes

### Test Scenarios
- [ ] Scenario: solid-recipes-only-canonical-kinds -- Given the registry after init, When listing `pack='solid'` recipes, Then the metric_kinds are exactly `{lcom4, fan_in, fan_out, depth_of_inheritance, interface_width, coupling_between_objects}`.
- [ ] Scenario: lcom4-class-known-value -- Given a Java class fixture with two disjoint method clusters sharing no fields, When the lcom4 recipe runs, Then it emits `value=2`.
- [ ] Scenario: cbo-counts-distinct-targets -- Given a class referencing four distinct external classes, When the coupling_between_objects recipe runs, Then it emits `value=4`.

## Stage 2.5: Cycle and duplication recipes

### Implementation Steps
- [ ] Implement `recipes/cycle_member.go` emitting `metric_kind='cycle_member'` at scope_kind `module|package` with `value=1` when the scope participates in a dependency cycle, `value=0` otherwise (architecture Sec 1.4.1).
- [ ] Implement `recipes/duplication_ratio.go` emitting `metric_kind='duplication_ratio'` at scope_kind `function|method|module` using a 50-token sliding window comparison.
- [ ] Both recipes register with `pack='base'`, `source='computed'`.
- [ ] Add tests covering: a three-module cycle (all three emit `cycle_member=1`); a function with 80% duplicate body emits `duplication_ratio=0.8`.

### Dependencies
- phase-ast-adapter-and-foundation-tier-compute/stage-base-pack-foundation-recipes

### Test Scenarios
- [ ] Scenario: cycle-member-flags-participants -- Given modules A->B->C->A, When the cycle_member recipe runs, Then all three emit `value=1`; a module D outside the cycle emits `value=0`.
- [ ] Scenario: duplication-ratio-bounded-zero-to-one -- Given any input scope, When the duplication_ratio recipe runs, Then the emitted `value` is in `[0,1]` (boundary check).

## Stage 2.6: modification_count_in_window materialiser

### Implementation Steps
- [ ] Add `internal/metrics/materialisers/modification_count.go` reading rows fed by `ingest.churn` (Phase 4) and materialising `metric_kind='modification_count_in_window'` with `pack='base'`, `source='derived'` per architecture Sec 1.4.1.
- [ ] Window size comes from tech-spec Sec 8.2 `window_days=90`; expose as configurable.
- [ ] Materialiser runs as part of the Metric Ingestor (same writer-ownership role) inside the same ScanRun as the foundation recipes so the active-row uniqueness invariant holds.
- [ ] Skip emitting a sample when no churn rows exist in the window for a given scope (no zero-fill noise).
- [ ] Add unit test with a synthetic churn stream verifying the count.

### Dependencies
- phase-ast-adapter-and-foundation-tier-compute/stage-base-pack-foundation-recipes

### Test Scenarios
- [ ] Scenario: materialiser-emits-canonical-kind -- Given churn rows for scope `pkg.Foo.bar` in the last 90 days, When the materialiser runs, Then it emits `MetricSample(metric_kind='modification_count_in_window', pack='base', source='derived')`.
- [ ] Scenario: out-of-window-rows-ignored -- Given churn rows older than 90 days, When the materialiser runs, Then no `metric_sample` row is written for that scope.

# Phase 3: Repo Indexer and Metric Ingestor

## Dependencies
- phase-foundation-and-schema
- phase-ast-adapter-and-foundation-tier-compute

## Stage 3.1: Repo Indexer and Commit lifecycle

### Implementation Steps
- [ ] Add `internal/repo_indexer/` service consuming Git webhooks and CLI rescan triggers; on each new SHA, INSERT a `commit` row (default `scan_status='pending'`) and a `repo_event(kind='register')` if needed.
- [ ] Repo Indexer is the ONLY writer of new `commit` rows (architecture G1); it never updates `scan_status` (the Metric Ingestor owns transitions).
- [ ] Define the canonical `Commit.scan_status` transition diagram in code: `pending -> scanning -> scanned` on success, `pending -> scanning -> failed` on error -- no `complete`, no `superseded`, no `orphaned` states (iter 1 evaluator item 2). Add a Go enum `ScanStatus` with only the four canonical values.
- [ ] Add `internal/repo_indexer/handler_test.go` covering: new SHA inserts a `commit` with `scan_status='pending'`; duplicate SHA event is a no-op.

### Dependencies
- phase-foundation-and-schema/stage-postgresql-role-grants-and-schema-isolation

### Test Scenarios
- [ ] Scenario: commit-states-only-canonical -- Given the `ScanStatus` enum at compile time, When `reflect.TypeOf(ScanStatus(0)).String()` enumerates its values, Then exactly `{pending, scanning, scanned, failed}` are present (no `complete`, no `superseded`, no `orphaned`).
- [ ] Scenario: new-sha-inserts-pending -- Given a webhook payload for a new SHA, When Repo Indexer processes it, Then a `commit` row appears with `scan_status='pending'` and a single `repo_event(kind='register')` is appended.

## Stage 3.2: Metric Ingestor and ScanRun state machine

### Implementation Steps
- [ ] Add `internal/metric_ingestor/` service that picks the oldest `commit.scan_status='pending'` row, opens a `scan_run(kind='full', sha_binding='single', status='running')`, sets `commit.scan_status='scanning'`, then drives the recipe registry over the parsed AST and writes `metric_sample` rows.
- [ ] On success, set `scan_run.status='succeeded'` and `commit.scan_status='scanned'`; on failure, `scan_run.status='failed'` and `commit.scan_status='failed'` -- only the four canonical Commit states and three canonical ScanRun states (iter 1 evaluator item 2).
- [ ] Ensure the Metric Ingestor is the SOLE writer of `commit.scan_status` (enforced by Phase 1.5 role grants).
- [ ] Add the matching `ScanRun.kind` enum in code (`full | delta | external_single | external_per_row | retract`) matching architecture Sec 5.7; refuse to insert unknown kinds.
- [ ] Add timeout from tech-spec Sec 8.2 `scan_timeout=30min`; a scan exceeding it cancels its context and is marked `failed`.
- [ ] Add `internal/metric_ingestor/state_test.go` covering happy path and timeout path.

### Dependencies
- phase-repo-indexer-and-metric-ingestor/stage-repo-indexer-and-commit-lifecycle
- phase-ast-adapter-and-foundation-tier-compute/stage-base-pack-foundation-recipes
- phase-ast-adapter-and-foundation-tier-compute/stage-solid-pack-foundation-recipes
- phase-ast-adapter-and-foundation-tier-compute/stage-cycle-and-duplication-recipes
- phase-ast-adapter-and-foundation-tier-compute/stage-modification-count-in-window-materialiser

### Test Scenarios
- [ ] Scenario: happy-path-states -- Given a `commit(scan_status='pending')`, When the Metric Ingestor processes it successfully, Then the row transitions `pending -> scanning -> scanned` and a single `scan_run(status='succeeded')` is appended.
- [ ] Scenario: failure-path-states -- Given a recipe that panics, When the Metric Ingestor catches the panic, Then the commit ends in `scan_status='failed'` and `scan_run.status='failed'`.
- [ ] Scenario: scan-run-kind-enum-rejects-invalid -- Given the ScanRun writer, When asked to insert `kind='external_double'`, Then it returns an error before reaching PostgreSQL.

## Stage 3.3: Active row uniqueness enforcement

### Implementation Steps
- [ ] In the Metric Ingestor write path, use `INSERT ... ON CONFLICT ON CONSTRAINT metric_sample_active_uq DO NOTHING` so re-runs of the ingestor over the same SHA whose rows are still active are no-ops (architecture Sec 5.2.2).
- [ ] Do NOT use any procedural `swap_active`, `swap` trigger, or stored function (iter 1 evaluator item 1 -- there is no `swap_active` verb in the canonical model).
- [ ] Add `internal/metric_ingestor/active_row_test.go` covering: re-ingest same SHA twice; with intervening retraction the second write succeeds.

### Dependencies
- phase-repo-indexer-and-metric-ingestor/stage-metric-ingestor-and-scanrun-state-machine

### Test Scenarios
- [ ] Scenario: re-ingest-without-retract-is-no-op -- Given a `metric_sample` row already present and active, When the Metric Ingestor re-ingests the same SHA, Then no second row appears (ON CONFLICT DO NOTHING) and no error is returned.
- [ ] Scenario: re-ingest-after-retract-succeeds -- Given a sample retracted via `metric_retraction(sample_id)`, When the Metric Ingestor re-ingests, Then a new `metric_sample` row appears with a fresh `sample_id`.

## Stage 3.4: Retraction and rescan flow

### Implementation Steps
- [ ] Implement `mgmt.retract_sample(sample_id, reason, actor)` verb: append a `repo_event(kind='retract_intent', payload={sample_id, reason})` and dispatch to the Metric Ingestor, which opens a `scan_run(kind='retract')` and appends `metric_retraction(sample_id, retracted_at, retracted_by_scan_run_id, reason)`.
- [ ] Implement `mgmt.rescan(repo_id, sha)` verb: append a `repo_event(kind='rescan_intent')` and enqueue a `scan_run(kind='full')` for the given SHA via the Metric Ingestor.
- [ ] Mgmt surface never writes Measurement rows directly -- it only emits `repo_event` and delegates (architecture Sec 6.3).
- [ ] Add idempotency: `mgmt.retract_sample` for an already-retracted sample is a no-op (returns the existing retraction row).

### Dependencies
- phase-repo-indexer-and-metric-ingestor/stage-active-row-uniqueness-enforcement

### Test Scenarios
- [ ] Scenario: retract-appends-retraction-row -- Given an active sample, When `mgmt.retract_sample` is invoked with a reason, Then a `metric_retraction` row appears, a `scan_run(kind='retract', status='succeeded')` is recorded, and the sample is no longer active per the partial unique index.
- [ ] Scenario: rescan-enqueues-scan-run -- Given a `mgmt.rescan(repo_id, sha)` call, When the verb completes, Then a `repo_event(kind='rescan_intent')` exists and a `scan_run(kind='full', status='running')` is observable.

## Stage 3.5: Stale ScanRun sweep loop

### Implementation Steps
- [ ] Add `internal/metric_ingestor/sweep.go` running a periodic loop every tech-spec Sec 8.2 `periodic_sweep_cadence=5min`.
- [ ] Sweep finds `scan_run(status='running')` rows older than `scan_timeout=30min` and transitions them to `status='failed'` with a sweep-attributed reason -- ONLY the canonical ScanRun states (no `orphaned`; iter 1 evaluator item 2).
- [ ] Sweep also cleans up `commit.scan_status='scanning'` rows whose owning `scan_run` has failed, setting them to `scan_status='failed'` (Metric Ingestor is the sole writer, so this is in-process).
- [ ] Add metrics counters `cleancode_sweep_stale_scans_total` and `cleancode_sweep_failed_commits_total` exposed for Prometheus.
- [ ] Add `internal/metric_ingestor/sweep_test.go` with synthetic stale rows and a fake clock.

### Dependencies
- phase-repo-indexer-and-metric-ingestor/stage-metric-ingestor-and-scanrun-state-machine

### Test Scenarios
- [ ] Scenario: stale-scan-run-becomes-failed -- Given a `scan_run(status='running')` older than 30 minutes, When the sweep runs, Then the row transitions to `status='failed'` (NOT `orphaned`, NOT `superseded`).
- [ ] Scenario: stale-commit-becomes-failed -- Given a commit at `scan_status='scanning'` whose scan_run was just marked failed, When the sweep finalises, Then the commit transitions to `scan_status='failed'`.

# Phase 4: External Metric Ingest Webhook

## Dependencies
- phase-repo-indexer-and-metric-ingestor

## Stage 4.1: Webhook transport and HMAC verification

### Implementation Steps
- [ ] Add `internal/ingest/webhook/` HTTP handler at `/v1/ingest/{verb}` accepting POST with HMAC-SHA256 signature header per tech-spec Sec 7 (Authentication invariants).
- [ ] Verify the signature against the per-tenant secret resolved by `(tenant_id, signing_key_id)`; reject invalid signatures with `401`.
- [ ] Add idempotency layer: compute `payload_hash = sha256(canonicalised body)`; if a `scan_run(payload_hash=...)` already exists for this verb, return the stored scan_run_id without re-executing.
- [ ] Dispatch the verified payload to the per-verb handler (`coverage`, `test_balance`, `churn`, `defects`).
- [ ] Add `internal/ingest/webhook/handler_test.go` covering valid signature, invalid signature, replay returns cached scan_run.

### Dependencies
- phase-repo-indexer-and-metric-ingestor/stage-metric-ingestor-and-scanrun-state-machine

### Test Scenarios
- [ ] Scenario: invalid-signature-rejected -- Given a webhook POST with an invalid HMAC header, When the handler verifies, Then it returns `401` and does NOT enqueue a scan_run.
- [ ] Scenario: replay-returns-cached-scan-run -- Given a payload already processed (same `payload_hash`), When the same body is POSTed again, Then the handler returns the original `scan_run_id` and no new `scan_run` row is appended.

## Stage 4.2: ingest coverage verb Cobertura parser

### Implementation Steps
- [ ] Implement `internal/ingest/coverage/cobertura.go` parsing the Cobertura XML format named by the operator pin `external-metric-coverage-format` (architecture Sec 1.6).
- [ ] Map per-file lines-covered ratios to `MetricSample(metric_kind='coverage_line_ratio', pack='ingested', source='ingested')` and branches to `metric_kind='coverage_branch_ratio'` -- the ONLY two coverage kinds permitted by tech-spec Sec 4.1.1 (iter 1 evaluator item 4 removed `coverage_line` and `coverage_branch` aliases).
- [ ] Open a `scan_run(kind='external_single', sha_binding='per_row', status='running')` for the upload; on success mark `succeeded`.
- [ ] Bind each row's `scope_id` by looking up the existing `scope_binding` for `(repo_id, sha, scope_kind, canonical_signature)`; if the binding is missing, skip the row and log a `coverage_skipped_unbound_scope` counter (do NOT invent a scope).
- [ ] Add `internal/ingest/coverage/cobertura_test.go` against a real fixture.

### Dependencies
- phase-external-metric-ingest-webhook/stage-webhook-transport-and-hmac-verification

### Test Scenarios
- [ ] Scenario: coverage-emits-only-canonical-kinds -- Given a Cobertura upload, When the verb runs, Then the appended `metric_sample` rows have `metric_kind IN ('coverage_line_ratio','coverage_branch_ratio')` -- no `coverage_line` or `coverage_branch` aliases.
- [ ] Scenario: unbound-scope-skipped -- Given a Cobertura row for a file/function with no matching `scope_binding`, When the parser runs, Then no `metric_sample` row is appended and `coverage_skipped_unbound_scope` increments.

## Stage 4.3: ingest test_balance verb

### Implementation Steps
- [ ] Implement `internal/ingest/test_balance/` parsing a JUnit-XML stream and emitting `MetricSample(metric_kind='pass_first_try_ratio', pack='ingested', source='ingested')` per architecture Sec 1.4.2 / tech-spec Sec 4.1.1.
- [ ] Compute the ratio as `passes_first_run / total_runs` over the supplied window; scope_kind defaults to `package` unless the payload carries a finer scope.
- [ ] Open `scan_run(kind='external_single', sha_binding='per_row')`.
- [ ] Explicitly do NOT emit test-count metrics, test-duration metrics, or any other `test_balance.*` metric kinds (iter 1 evaluator item 4 removed those).
- [ ] Add test fixture covering a JUnit file with mixed pass/fail/flake outcomes.

### Dependencies
- phase-external-metric-ingest-webhook/stage-webhook-transport-and-hmac-verification

### Test Scenarios
- [ ] Scenario: test-balance-emits-only-pass-first-try-ratio -- Given a JUnit upload, When the verb runs, Then the appended `metric_sample` rows have exactly one `metric_kind='pass_first_try_ratio'` (no test-count or duration kinds).
- [ ] Scenario: ratio-clamped-zero-to-one -- Given a JUnit upload, When the verb computes the ratio, Then the emitted value is in `[0,1]` and `total_runs=0` results in NO row written (avoid NaN).

## Stage 4.4: ingest churn verb feeds materialiser

### Implementation Steps
- [ ] Implement `internal/ingest/churn/` accepting a per-file modification stream and writing rows into the `churn_event` staging table (internal table inside `clean_code` schema, not exposed as a metric).
- [ ] Open `scan_run(kind='external_per_row', sha_binding='per_row', to_sha=NULL)`; record `payload_hash` for idempotency.
- [ ] The verb writes ZERO `metric_sample` rows directly (iter 1 evaluator item 4); it ONLY feeds the `modification_count_in_window` materialiser (Stage 2.6) which is the sole writer of that metric_kind.
- [ ] Add a documentation comment in the verb naming the materialiser and pointing at tech-spec Sec 4.1.1.
- [ ] Add `internal/ingest/churn/handler_test.go` verifying no `metric_sample` insert happens in the verb's call stack and the materialiser picks up the rows on its next pass.

### Dependencies
- phase-external-metric-ingest-webhook/stage-webhook-transport-and-hmac-verification
- phase-ast-adapter-and-foundation-tier-compute/stage-modification-count-in-window-materialiser

### Test Scenarios
- [ ] Scenario: churn-writes-no-metric-sample -- Given a churn upload, When the verb returns, Then `SELECT COUNT(*) FROM metric_sample WHERE scan_run_id=$1` returns 0; only `churn_event` rows are appended.
- [ ] Scenario: materialiser-consumes-churn -- Given churn rows appended for a scope, When the modification_count materialiser runs next, Then it emits the canonical `metric_kind='modification_count_in_window'` sample with `pack='base'`, `source='derived'`.

## Stage 4.5: ingest defects verb store only

### Implementation Steps
- [ ] Implement `internal/ingest/defects/` accepting a defects payload (e.g. JIRA export) but in v1 the verb does NOT write any `metric_sample` row (iter 1 evaluator item 4 -- tech-spec Sec 4.1.1 pins v1 to store-only at ScanRun boundary).
- [ ] Open `scan_run(kind='external_per_row', sha_binding='per_row', to_sha=NULL, payload_hash=...)` for idempotency only; mark `succeeded` on parse OK.
- [ ] Discard the payload body after recording `payload_hash` (no persistence of defect rows in v1).
- [ ] Add a code-level comment naming the v1 pin and the future migration path (`defect_density` becomes a derived metric in a later story).
- [ ] Add `internal/ingest/defects/handler_test.go` verifying zero `metric_sample` rows are appended and a `scan_run(kind='external_per_row')` row exists.

### Dependencies
- phase-external-metric-ingest-webhook/stage-webhook-transport-and-hmac-verification

### Test Scenarios
- [ ] Scenario: defects-v1-writes-no-metric -- Given a defects upload, When the verb completes, Then `metric_sample` row count is unchanged AND a `scan_run(kind='external_per_row', status='succeeded')` row exists; no `defect_density` or other metric kind is written.
- [ ] Scenario: defects-idempotent -- Given the same defects payload POSTed twice, When the second call runs, Then the same `scan_run_id` is returned and no second scan_run row is appended.

# Phase 5: Policy Steward and SOLID Rule Engine

## Dependencies
- phase-foundation-and-schema

## Stage 5.1: Policy Steward signing key store

### Implementation Steps
- [ ] Add `internal/policy/keys/` Ed25519 keypair manager with `signing_key_id` rotation; private key sealed via the operator KMS.
- [ ] Implement key-rotation overlap window from tech-spec Sec 8.2 `policy_publish_overlap_min_seconds=86400` (24h): two key_ids may co-exist during overlap, both accepted by the evaluator.
- [ ] Expose `policy.keys.list_active` read verb returning `[{key_id, fingerprint, valid_from, valid_until}]`.
- [ ] Add `internal/policy/keys/manager_test.go` covering rotation: old key remains valid for 24h after a new key is published; after 24h+1s, signatures by the old key fail verification.

### Dependencies
- phase-foundation-and-schema/stage-postgresql-role-grants-and-schema-isolation

### Test Scenarios
- [ ] Scenario: overlap-window-enforced -- Given a key rotation at T0, When a payload signed by the old key arrives at T0+23h59m, Then verification succeeds; at T0+24h+1s, verification fails.
- [ ] Scenario: kms-unavailable-blocks-start -- Given the KMS is unreachable at startup and the `policy-signing-required=v1 required` pin (architecture Sec 1.6), When the service initialises, Then it exits non-zero with a clear error.

## Stage 5.2: Policy publish activate and rulepack verbs

### Implementation Steps
- [ ] Implement `policy.publish(rulepack_set, refactor_weights)` verb appending an immutable `policy_version(policy_version_id, signature, signing_key_id, rulepack_set_hash, refactor_weights JSONB)` row -- never UPDATE (architecture Sec 5.3.3).
- [ ] Implement `policy.activate(policy_version_id, scope)` verb appending a `policy_activation(activation_id, scope, policy_version_id, activated_at, activated_by)` row.
- [ ] Implement `policy.rulepack.add(rulepack)` and `policy.rulepack.remove(rulepack_id)` verbs writing `rule_pack` and `rule` rows; both are append-only.
- [ ] All four verbs require a valid signing key (Stage 5.1); refuse unsigned payloads.
- [ ] Add `internal/policy/steward_test.go` covering publish -> activate -> evaluator picks up the new version.

### Dependencies
- phase-policy-steward-and-solid-rule-engine/stage-policy-steward-signing-key-store

### Test Scenarios
- [ ] Scenario: policy-version-immutable -- Given an existing `policy_version` row, When any UPDATE statement targets it, Then PostgreSQL returns permission denied (no role has UPDATE) AND the Steward verb path has no UPDATE call.
- [ ] Scenario: activation-supersedes-previous -- Given an active policy_version for `org=acme`, When `policy.activate(new_id, org=acme)` runs, Then a new `policy_activation` row appears and reads at the scope return the new version.

## Stage 5.3: Override append only mute lifecycle

### Implementation Steps
- [ ] Implement `mgmt.override(scope, rule_id, mute BOOL, reason)` verb delegating to the Policy Steward which appends an `override(override_id, policy_version_id, scope, rule_id, mute, reason, created_at, created_by)` row (architecture Sec 6.3, tech-spec Sec 10A).
- [ ] Latest-row-wins read semantics: the evaluator reads `MAX(created_at) WHERE scope MATCHES AND rule_id=$1` to determine the active mute state.
- [ ] The `override` row schema has NO `expires_at` column and the verb refuses any caller-supplied `expires_at` field (iter 1 evaluator item 5 -- tech-spec Sec 10A pins v1 to no TTL).
- [ ] Unmute is achieved by appending a new `override` row with `mute=false`; old rows are NEVER deleted or UPDATEd.
- [ ] Do NOT add a max-mute-age enforcement timer (iter 1 evaluator item 5 -- aged-mute is an Insights report only, see Phase 10).
- [ ] Add `internal/policy/override_test.go` covering: append mute=true; append mute=false; latest-row read returns false.

### Dependencies
- phase-policy-steward-and-solid-rule-engine/stage-policy-publish-activate-and-rulepack-verbs

### Test Scenarios
- [ ] Scenario: override-no-expires-field -- Given a `mgmt.override` request with `expires_at='2030-01-01'`, When the verb runs, Then it returns a validation error naming the field as unsupported in v1; no row is appended.
- [ ] Scenario: latest-row-wins -- Given two override rows for the same scope/rule with `mute=true` then `mute=false`, When the evaluator reads the active mute, Then it sees `mute=false`.
- [ ] Scenario: no-ttl-enforcement -- Given an override row older than any candidate TTL (e.g. 365 days), When time advances and no scheduled job runs, Then the row remains the active state (latest-row-wins still returns it).

## Stage 5.4: Predicate DSL evaluator

### Implementation Steps
- [ ] Add `internal/policy/dsl/` parser+evaluator for the predicate DSL described in architecture Sec 5.3.2 supporting `metric_kind`, `scope_kind`, comparison operators, AND/OR/NOT, and reference to `Threshold` rows.
- [ ] Predicates are pure functions over `MetricSample` rows -- no side effects, no IO.
- [ ] Cache parsed predicates per `policy_version_id` so re-evaluation is hot-path cheap.
- [ ] Add `internal/policy/dsl/parser_test.go` covering well-formed predicates and rejection of malformed ones with line/column error messages.

### Dependencies
- phase-policy-steward-and-solid-rule-engine/stage-policy-publish-activate-and-rulepack-verbs

### Test Scenarios
- [ ] Scenario: dsl-rejects-unknown-metric-kind -- Given a predicate referencing `metric_kind='lines_of_code'` (a non-canonical name), When the parser runs, Then it returns a validation error naming the unknown metric_kind (canon-guard against iter 1 evaluator item 3 regression).
- [ ] Scenario: dsl-deterministic -- Given the same predicate and the same `MetricSample` input, When evaluated twice, Then it returns the same boolean result.

## Stage 5.5: SOLID rule packs

### Implementation Steps
- [ ] Add `policy/rulepacks/solid/srp.yaml`: SRP rule per architecture Sec 1.3 consuming `metric_kind IN ('lcom4','interface_width')`.
- [ ] Add `policy/rulepacks/solid/ocp.yaml`: OCP rule consuming `metric_kind IN ('modification_count_in_window','depth_of_inheritance')` -- the materialiser dependency is here in the RULE definition, NOT in the Stage 2.4 recipe stage (iter 1 evaluator item 10).
- [ ] Add `policy/rulepacks/solid/lsp.yaml`: LSP rule consuming `metric_kind='depth_of_inheritance'` plus override-method-signature checks emitted by Stage 2.4.
- [ ] Add `policy/rulepacks/solid/isp.yaml`: ISP rule consuming `metric_kind='interface_width'`.
- [ ] Add `policy/rulepacks/solid/dip.yaml`: DIP rule consuming `metric_kind IN ('fan_out','coupling_between_objects')`.
- [ ] Each rulepack is signed and ingested via `policy.rulepack.add` at startup if absent.
- [ ] Add `policy/rulepacks/solid/solid_test.go` loading each pack and asserting it references only canonical metric_kinds.

### Dependencies
- phase-policy-steward-and-solid-rule-engine/stage-predicate-dsl-evaluator
- phase-ast-adapter-and-foundation-tier-compute/stage-solid-pack-foundation-recipes
- phase-ast-adapter-and-foundation-tier-compute/stage-modification-count-in-window-materialiser

### Test Scenarios
- [ ] Scenario: solid-rulepacks-load -- Given the five SOLID rulepack files, When the Policy Steward loads them, Then all five appear as `rule_pack` rows with `pack='solid'` and the contained predicates parse cleanly.
- [ ] Scenario: solid-rulepacks-only-canonical-kinds -- Given the loaded predicates, When scanning their metric_kind references, Then each is one of `{lcom4, fan_in, fan_out, depth_of_inheritance, interface_width, coupling_between_objects, modification_count_in_window}` and no non-canonical alias appears.

## Stage 5.6: Decoupled functional areas rule pack

### Implementation Steps
- [ ] Add `policy/rulepacks/decoupling/cycles.yaml`: decoupling rule consuming `metric_kind='cycle_member'` (block when any module in a watched scope is in a cycle).
- [ ] Add `policy/rulepacks/decoupling/coupling.yaml`: decoupling rule consuming `metric_kind IN ('fan_in','fan_out','coupling_between_objects')` with thresholds from `Threshold` rows.
- [ ] Add `policy/rulepacks/decoupling/duplication.yaml`: decoupling rule consuming `metric_kind='duplication_ratio'`.
- [ ] Signed and loaded as `pack='decoupling'` rule_packs.
- [ ] Add a test asserting each predicate references only canonical metric_kinds.

### Dependencies
- phase-policy-steward-and-solid-rule-engine/stage-predicate-dsl-evaluator
- phase-ast-adapter-and-foundation-tier-compute/stage-cycle-and-duplication-recipes

### Test Scenarios
- [ ] Scenario: decoupling-loads -- Given the three decoupling rulepack files, When the Steward loads them, Then `pack='decoupling'` rule_packs exist with parsed predicates.
- [ ] Scenario: cycles-rule-fires-on-cycle-member -- Given a `metric_sample(metric_kind='cycle_member', value=1)`, When the predicate evaluates, Then it returns true (would trigger a Finding).

## Stage 5.7: SOLID Rule Engine batch worker

### Implementation Steps
- [ ] Add `internal/rule_engine/` worker consuming the latest active `policy_version` (Stage 5.2) and evaluating each rule's predicate over `metric_sample` rows for newly-scanned SHAs.
- [ ] On a positive match, append a `finding(finding_id, repo_id, sha, scope_id, rule_id, policy_version_id, metric_kind, value, delta, severity, created_at)` row -- writer-ownership: Rule Engine is the SOLE writer of `finding` (architecture G1 / Phase 1.5 grants).
- [ ] Compute `Finding.delta` against the prior SHA's sample for the same scope+metric: `regression`/`improvement`/`flat`/`new`/`resolved` (architecture Sec 5.4.3).
- [ ] Apply Override mute state from Stage 5.3 BEFORE appending the Finding (muted findings are not written; Insights surface tracks mute hits separately).
- [ ] Add `internal/rule_engine/worker_test.go` covering: finding emitted; muted scope produces no finding; delta=regression when value worsens past threshold.

### Dependencies
- phase-policy-steward-and-solid-rule-engine/stage-override-append-only-mute-lifecycle
- phase-policy-steward-and-solid-rule-engine/stage-solid-rule-packs
- phase-policy-steward-and-solid-rule-engine/stage-decoupled-functional-areas-rule-pack
- phase-repo-indexer-and-metric-ingestor/stage-metric-ingestor-and-scanrun-state-machine

### Test Scenarios
- [ ] Scenario: finding-emitted-on-rule-hit -- Given a SHA with a `metric_sample(metric_kind='lcom4', value=12)` exceeding the SRP threshold (10), When the rule engine runs, Then a `finding(rule_id='solid.srp', delta='regression')` row appears with `policy_version_id` pinned.
- [ ] Scenario: muted-scope-skipped -- Given an `override(scope, rule_id, mute=true)` latest row, When the rule engine evaluates that scope+rule, Then no `finding` row is appended.

# Phase 6: Evaluator Surface and Management Surface

## Dependencies
- phase-repo-indexer-and-metric-ingestor
- phase-policy-steward-and-solid-rule-engine

## Stage 6.1: Evaluator gate verb and degraded handling

### Implementation Steps
- [ ] Implement `eval.gate(repo_id, sha, scope?)` verb appending an `evaluation_run(run_id, repo_id, sha, scope, policy_version_id, caller, started_at)` row and computing the verdict.
- [ ] Verdict is the canonical enum `pass | warn | block` (architecture Sec 5.4.2 / tech-spec Sec 4.8) -- NOT `pass|fail|gated` (iter 1 evaluator item 6). Implement as a Go enum `Verdict { Pass, Warn, Block }` with no other values.
- [ ] Block when any unmuted Finding has `severity='block'` for the scope under evaluation; warn when any unmuted Finding has `severity='warn'`; pass otherwise.
- [ ] Add a SEPARATE `degraded BOOL` and `degraded_reason` field to the verdict; degraded conditions map to `verdict='warn'` per the operator pin `gate-degraded-policy=warn` (architecture Sec 1.6).
- [ ] `degraded_reason` is constrained to `samples_pending | policy_signature_invalid | xrepo_edges_unavailable` for eval.gate -- `percentile_stale` is NOT a valid eval.gate reason (it lives on the Insights surface per Stage 7.3; iter 1 evaluator item 8).
- [ ] Append `evaluation_verdict(verdict_id, run_id, scope, verdict, degraded, degraded_reason, settled_at)` after compute.
- [ ] Add `internal/evaluator/gate_test.go` covering: clean pass; block on severity=block; warn on degraded; degraded_reason rejects percentile_stale.

### Dependencies
- phase-policy-steward-and-solid-rule-engine/stage-solid-rule-engine-batch-worker

### Test Scenarios
- [ ] Scenario: verdict-enum-only-canonical -- Given the `Verdict` Go enum at compile time, When iterating its values, Then they are exactly `{pass, warn, block}` (no `fail`, no `gated`) -- guards iter 1 evaluator item 6.
- [ ] Scenario: degraded-maps-to-warn -- Given a samples_pending degraded condition (no findings present), When `eval.gate` runs, Then the verdict is `warn` with `degraded=true, degraded_reason='samples_pending'`.
- [ ] Scenario: percentile-stale-not-on-gate -- Given any code path inside `eval.gate`, When the degraded_reason validator runs, Then `percentile_stale` is rejected as an invalid eval.gate reason.

## Stage 6.2: Management write verbs and repo onboarding

### Implementation Steps
- [ ] Implement `mgmt.register_repo(repo_url, default_branch, modes)` writing a `repo` row plus a `repo_event(kind='register')`.
- [ ] Implement `mgmt.set_mode(repo_id, mode)` writing a `repo_event(kind='mode_change', payload={mode})` and updating `repo.mode`; allowed modes are `embedded | linked` per architecture Sec 1.6 `ast-mode-default`.
- [ ] Re-export `mgmt.retract_sample` (Stage 3.4), `mgmt.rescan` (Stage 3.4), `mgmt.override` (Stage 5.3) under the Management Surface namespace.
- [ ] Each write verb requires an authenticated caller (OIDC subject) recorded as `actor` on the resulting RepoEvent / Override / Finding row.
- [ ] Add `internal/management/verbs_test.go` covering each write verb's happy path and unauthenticated rejection.

### Dependencies
- phase-evaluator-surface-and-management-surface/stage-evaluator-gate-verb-and-degraded-handling
- phase-repo-indexer-and-metric-ingestor/stage-retraction-and-rescan-flow
- phase-policy-steward-and-solid-rule-engine/stage-override-append-only-mute-lifecycle

### Test Scenarios
- [ ] Scenario: register-repo-idempotent -- Given a repo already registered, When `mgmt.register_repo` is called with the same URL, Then the existing repo_id is returned and no duplicate `repo` row appears.
- [ ] Scenario: set-mode-emits-event -- Given a repo at mode `embedded`, When `mgmt.set_mode(repo_id, 'linked')` runs, Then a `repo_event(kind='mode_change')` is appended and subsequent `mgmt.read.repo` returns mode=`linked`.

## Stage 6.3: Management read verbs and insights projections

### Implementation Steps
- [ ] Implement SHA-pinned read verbs `mgmt.read.repo(repo_id)`, `mgmt.read.metric_sample(repo_id, sha, scope_id, metric_kind)`, `mgmt.read.metric_samples(repo_id, sha, filter)`, `mgmt.read.findings(repo_id, sha)`, `mgmt.read.regressions(repo_id, sha)`, `mgmt.read.refactor_plan(repo_id, sha)` reading the active-row view per Phase 1.3 partial unique index.
- [ ] Implement latest-dashboard read verbs `mgmt.read.cross_repo(metric_kind, scope_kind)`, `mgmt.read.portfolio(metric_kind)` reading `cross_repo_percentile` and `portfolio_snapshot` populated by Phase 7.
- [ ] Both modes route through a single `internal/management/reader.go` with explicit `mode = sha_pinned | latest_dashboard` per architecture Sec 6.3.
- [ ] Read paths never trigger writes (read-only roles enforced by Phase 1.5).
- [ ] Add `internal/management/reader_test.go` covering both modes.

### Dependencies
- phase-evaluator-surface-and-management-surface/stage-evaluator-gate-verb-and-degraded-handling

### Test Scenarios
- [ ] Scenario: sha-pinned-returns-active-row -- Given two `metric_sample` rows for the same quintuple with the older retracted, When `mgmt.read.metric_sample` runs, Then it returns the active (non-retracted) row only.
- [ ] Scenario: latest-dashboard-returns-snapshot -- Given a populated `cross_repo_percentile`, When `mgmt.read.cross_repo` runs, Then it returns the p50/p90/p99/histogram_json columns directly (no on-the-fly recompute).

## Stage 6.4: HTTP JSON gateway and OIDC auth

### Implementation Steps
- [ ] Add `internal/api/` HTTP+JSON gateway exposing every verb at `/v1/{namespace}/{verb}` with OIDC bearer-token auth (per tech-spec Sec 7).
- [ ] Map verb names 1:1 to HTTP path components; refuse unknown verbs with `404`.
- [ ] Emit OTel spans tagged with `verb`, `caller_subject`, and `repo_id` per architecture Sec 8.
- [ ] Add `internal/api/handler_test.go` covering: missing token returns 401, bad audience returns 403, valid token reaches the handler.

### Dependencies
- phase-evaluator-surface-and-management-surface/stage-management-write-verbs-and-repo-onboarding
- phase-evaluator-surface-and-management-surface/stage-management-read-verbs-and-insights-projections

### Test Scenarios
- [ ] Scenario: oidc-rejects-missing-token -- Given an HTTP request to any `/v1/*` route without an Authorization header, When the handler runs, Then it returns `401` with `WWW-Authenticate: Bearer`.
- [ ] Scenario: unknown-verb-404 -- Given a POST to `/v1/eval/unknown_verb`, When the gateway routes, Then it returns `404` and emits no `evaluation_run` row.

# Phase 7: Cross Repo Aggregator

## Dependencies
- phase-repo-indexer-and-metric-ingestor
- phase-external-metric-ingest-webhook

## Stage 7.1: Aggregator cadence loop and snapshot writers

### Implementation Steps
- [ ] Add `internal/aggregator/` worker running on tech-spec Sec 8.2 `aggregator_cadence=15min`.
- [ ] On each tick, read active `metric_sample` rows for all repos, group by `metric_kind` + `scope_kind`, and write rows into `repo_metric_snapshot` (per-repo p50/p90/p99 columns) and `cross_repo_percentile` (cross-repo p50/p90/p99 + `histogram_json` columns) and `portfolio_snapshot` (weighted_mean across portfolio).
- [ ] Aggregator is the SOLE writer of `repo_metric_snapshot`, `cross_repo_percentile`, `portfolio_snapshot` per architecture G1 / Phase 1.5 grants.
- [ ] Snapshot tables carry a `built_at` timestamp updated on every tick (used by Stage 7.3 freshness banner).
- [ ] Add `internal/aggregator/worker_test.go` covering one tick over a synthetic dataset.

### Dependencies
- phase-repo-indexer-and-metric-ingestor/stage-active-row-uniqueness-enforcement

### Test Scenarios
- [ ] Scenario: tick-writes-snapshots -- Given five repos with active `metric_sample` rows for `lcom4`, When the aggregator ticks, Then `repo_metric_snapshot` has five rows for that metric_kind and `cross_repo_percentile` has one row with non-null p50/p90/p99/histogram_json/built_at.
- [ ] Scenario: aggregator-is-sole-writer -- Given any non-aggregator role, When it attempts INSERT into `cross_repo_percentile`, Then PostgreSQL returns permission denied; the aggregator role succeeds.

## Stage 7.2: System tier metric composer

### Implementation Steps
- [ ] Add `internal/aggregator/system_tier.go` writing exactly the SEVEN canonical system-tier metric_kinds per architecture Sec 1.4.2: `xrepo_dep_depth`, `arch_debt_ratio`, `velocity_trend`, `arch_fitness`, `blast_radius`, `xservice_test_reliability`, `knowledge_index`.
- [ ] Each system-tier sample is written as `metric_sample(pack='system', source='derived')`; the aggregator is the sole writer of `pack='system'` per Phase 1.5 grants. NO invented metric_kinds like `p50.system` or `p95.system` are written -- percentile vectors live in `cross_repo_percentile` columns, not as fake metric_kind rows (iter 1 evaluator item 7).
- [ ] When the aggregator runs in embedded mode and cross-repo edges are unavailable, stamp the resulting verdict path with `degraded_reason='xrepo_edges_unavailable'` (per architecture Sec 5.5; this reason flows through eval.gate, not Insights).
- [ ] When upstream samples are missing for a metric_kind, skip writing and emit a `samples_pending` counter (the same `degraded_reason='samples_pending'` propagates to consumers).
- [ ] Add `internal/aggregator/system_tier_test.go` enumerating the registered system-tier kinds and asserting the set is exactly the seven canonical names.

### Dependencies
- phase-cross-repo-aggregator/stage-aggregator-cadence-loop-and-snapshot-writers

### Test Scenarios
- [ ] Scenario: system-tier-only-canonical-kinds -- Given the system_tier composer at runtime, When listing the metric_kinds it will write, Then the set is exactly `{xrepo_dep_depth, arch_debt_ratio, velocity_trend, arch_fitness, blast_radius, xservice_test_reliability, knowledge_index}` -- no `p50.system`, `p90.system`, `p95.system`, `p99.system` (iter 1 evaluator item 7).
- [ ] Scenario: embedded-mode-stamps-xrepo-degraded -- Given the aggregator in embedded mode with no xrepo edges, When it composes, Then no system-tier `arch_debt_ratio` row is written for affected scopes and the degraded counter labelled `reason=xrepo_edges_unavailable` increments.

## Stage 7.3: Insights Surface percentile freshness banner

### Implementation Steps
- [ ] Add `internal/management/insights/freshness.go` consumed by the Management read verbs `mgmt.read.cross_repo` and `mgmt.read.portfolio` (Stage 6.3) -- this is the Insights surface, NOT eval.gate (iter 1 evaluator item 8).
- [ ] On each read, compare `cross_repo_percentile.built_at` (Stage 7.2) against tech-spec Sec 8.2 `freshness_window_seconds=3600`; if the snapshot is older, attach `degraded=true, degraded_reason='percentile_stale'` to the Insights envelope.
- [ ] `percentile_stale` is INSIGHTS-ONLY -- the eval.gate verb refuses to accept this reason (verified in Stage 6.1 test scenario `percentile-stale-not-on-gate`).
- [ ] Add `internal/management/insights/freshness_test.go` with a fake clock covering: fresh snapshot returns no banner; stale snapshot returns `percentile_stale` banner; eval.gate path never produces this reason.

### Dependencies
- phase-cross-repo-aggregator/stage-system-tier-metric-composer
- phase-evaluator-surface-and-management-surface/stage-management-read-verbs-and-insights-projections

### Test Scenarios
- [ ] Scenario: stale-percentile-banner-on-insights -- Given a `cross_repo_percentile.built_at` older than `freshness_window_seconds`, When `mgmt.read.cross_repo` runs, Then the response envelope carries `degraded=true, degraded_reason='percentile_stale'`.
- [ ] Scenario: fresh-percentile-no-banner -- Given a `built_at` within the freshness window, When `mgmt.read.cross_repo` runs, Then the response envelope has `degraded=false` and no degraded_reason field.
- [ ] Scenario: gate-never-emits-percentile-stale -- Given any path through `eval.gate`, When degraded_reason is set, Then it is never `percentile_stale` (asserted by code-path test).

# Phase 8: Refactor Planner

## Dependencies
- phase-policy-steward-and-solid-rule-engine
- phase-cross-repo-aggregator

## Stage 8.1: Composite hotspot scoring

### Implementation Steps
- [ ] Add `internal/refactor/hotspot.go` computing `score = alpha*complexity_z + beta*churn_z + gamma*coupling_z + delta*finding_count` per architecture Sec 3.9.
- [ ] Weights `alpha, beta, gamma, delta` come from `policy_version.refactor_weights JSONB`; the refactor planner READS the active `policy_version` and embeds `policy_version_id` on every `hot_spot` row (so re-deriving from old weights remains reproducible).
- [ ] Input metric_kinds: complexity from `cyclo`+`cognitive_complexity`, churn from `modification_count_in_window`, coupling from `coupling_between_objects`+`fan_out`, finding_count from `finding` rows where `delta IN ('regression','new')`.
- [ ] Write `hot_spot(hotspot_id, repo_id, sha, scope_id, score, complexity_z, churn_z, coupling_z, finding_count, policy_version_id, built_at)` rows.
- [ ] Add `internal/refactor/hotspot_test.go` covering a synthetic input that produces a known score.

### Dependencies
- phase-cross-repo-aggregator/stage-system-tier-metric-composer
- phase-policy-steward-and-solid-rule-engine/stage-solid-rule-engine-batch-worker

### Test Scenarios
- [ ] Scenario: hotspot-score-formula -- Given known z-scores `(1.0, 2.0, 0.5)` and `finding_count=3` with weights `(1,1,1,1)`, When `score` is computed, Then it equals `1.0 + 2.0 + 0.5 + 3 = 6.5` (round to 2dp).
- [ ] Scenario: hotspot-pins-policy-version -- Given the active `policy_version_id='pv-42'`, When a `hot_spot` row is written, Then `hot_spot.policy_version_id='pv-42'` is recorded.

## Stage 8.2: Refactor plan and task generation

### Implementation Steps
- [ ] Add `internal/refactor/planner.go` reading the top-N hotspots per repo (N from `policy_version.refactor_weights.top_n`) and writing `refactor_plan(plan_id, repo_id, sha, hotspot_ids JSONB, policy_version_id, generated_at)`.
- [ ] For each hotspot, emit one or more `refactor_task(task_id, plan_id, scope_id, rule_id, kind, effort_hours, expected_metric_delta JSONB, status)` rows.
- [ ] `task.kind` is the canonical enum `extract_function | split_class | introduce_interface | break_cycle | reduce_inheritance | reduce_coupling | reduce_lcom | reduce_duplication` (architecture Sec 5.6.3); refuse to write unknown kinds.
- [ ] `task.status` enum: `proposed | in_progress | done | rejected | superseded` (architecture Sec 5.6.4).
- [ ] No `effort_estimate` table (iter 1 evaluator item 1) -- the estimate lives as a column `effort_hours` on the `refactor_task` row itself.
- [ ] Add `internal/refactor/planner_test.go` covering happy path and rule-kind mapping.

### Dependencies
- phase-refactor-planner/stage-composite-hotspot-scoring

### Test Scenarios
- [ ] Scenario: plan-generates-canonical-task-kinds -- Given a hotspot flagged by `solid.srp`, When the planner generates tasks, Then a `refactor_task` row appears with `kind='split_class'` or `kind='reduce_lcom'` (canonical enum) and `rule_id='solid.srp'`.
- [ ] Scenario: no-effort-estimate-table -- Given the schema after Phase 1 migrations, When the planner persists effort, Then it writes `refactor_task.effort_hours` and no other table named `effort_estimate` exists (guards iter 1 evaluator item 1).

## Stage 8.3: ML effort model loader and version pinning

### Implementation Steps
- [ ] Add `internal/refactor/effort_model.go` loading an external ML model artefact named by the operator pin `refactor-effort-source` (architecture Sec 1.6 default `ML model from historical commits`).
- [ ] Pin the model version inside `policy_version.refactor_weights.effort_model_version`; each generated `refactor_task` inherits the version through its plan's `policy_version_id` (no `effort_model_version` column on the task -- avoid duplicating state).
- [ ] If the model artefact URI is not configured AND the pin requires a model, refuse service startup with a clear error.
- [ ] Add `internal/refactor/effort_model_test.go` covering: model present produces deterministic estimate; model absent + pin required blocks startup.

### Dependencies
- phase-refactor-planner/stage-refactor-plan-and-task-generation

### Test Scenarios
- [ ] Scenario: missing-model-blocks-startup -- Given `refactor-effort-source=ML model from historical commits` and no model URI configured, When the planner initialises, Then startup exits non-zero with an error naming the missing config.
- [ ] Scenario: effort-model-version-pinned-via-policy -- Given a generated `refactor_task`, When inspecting its plan's `policy_version.refactor_weights.effort_model_version`, Then the value matches the loaded model artefact version.

# Phase 9: Audit WAL and reliability hardening

## Dependencies
- phase-evaluator-surface-and-management-surface

## Stage 9.1: Audit WAL frame writer

### Implementation Steps
- [ ] Add `internal/audit/wal/` writer scoped EXCLUSIVELY to the Audit sub-store -- the only tables whose writes feed the WAL are `evaluation_run`, `evaluation_verdict`, `finding` (architecture Sec 7.10 / tech-spec Sec 4.13). Catalog, Measurement, Policy, and Refactor writes do NOT route through this WAL (iter 1 evaluator item 11).
- [ ] Each WAL frame is a serialised `AuditFrame{frame_id, table, op, row_pk, row_json, written_at, signing_key_id, signature}` appended to a per-partition file under `data/wal/audit/`.
- [ ] The Evaluator and Rule Engine wrap their PostgreSQL INSERTs into Audit tables in a single transaction that writes the row AND a WAL frame, committing both atomically.
- [ ] Do NOT introduce an `audit_event` projection table or `audit_anchor` table -- the Audit table rows themselves ARE the audit trail (iter 1 evaluator item 1 and tech-spec Sec 4.13). The WAL is the durability mechanism, not a parallel projection.
- [ ] Add `internal/audit/wal/writer_test.go` covering: WAL frame is written iff a PostgreSQL row is committed; rollback prevents WAL frame from being readable by the reconciler.

### Dependencies
- phase-evaluator-surface-and-management-surface/stage-evaluator-gate-verb-and-degraded-handling

### Test Scenarios
- [ ] Scenario: wal-scope-only-audit-tables -- Given any code path in the service, When grepping the writer call sites, Then `internal/audit/wal` is referenced ONLY from `internal/evaluator/` and `internal/rule_engine/` (not from catalog/measurement/policy/refactor) -- iter 1 evaluator item 11.
- [ ] Scenario: no-projection-table -- Given the schema, When `\dt clean_code.*` runs, Then no tables named `audit_event` or `audit_anchor` exist; only `evaluation_run`, `evaluation_verdict`, `finding` carry audit semantics.

## Stage 9.2: Audit WAL Reconciler replay only

### Implementation Steps
- [ ] Add `internal/audit/reconciler/` worker that on service restart reads `data/wal/audit/` frames, verifies signatures, and replays MISSING rows back into the Audit tables.
- [ ] Reconciler is REPLAY-ONLY: it never inserts a row whose `(table, row_pk)` already exists; it never deletes a row; it never modifies a non-Audit table (iter 1 evaluator item 11).
- [ ] Preserve `evaluation_run.caller` as recorded in the original frame -- the reconciler does NOT substitute itself as the caller.
- [ ] Reconciler writes via the `clean_code_audit_replayer` role (Phase 1.5) which has INSERT-only on Audit tables.
- [ ] Add `internal/audit/reconciler/replay_test.go` covering: missing row gets re-inserted; existing row is left untouched; caller is preserved.

### Dependencies
- phase-audit-wal-and-reliability-hardening/stage-audit-wal-frame-writer

### Test Scenarios
- [ ] Scenario: reconciler-replays-missing-rows -- Given a WAL frame for a row missing from `finding`, When the reconciler runs, Then the row is INSERTed and reads return it.
- [ ] Scenario: reconciler-preserves-caller -- Given a WAL frame for `evaluation_run` with `caller='ci-bot'`, When the reconciler replays after restart, Then the replayed row's `caller` column equals `'ci-bot'` (not `'reconciler'`).
- [ ] Scenario: reconciler-cannot-write-non-audit -- Given the audit replayer role, When the reconciler attempts INSERT into `metric_sample`, Then PostgreSQL returns permission denied.

## Stage 9.3: AST subprocess isolation and mode flip safety

### Implementation Steps
- [ ] Wrap the Tree-sitter parser fleet (Stage 2.1) in a per-language subprocess pool with rlimit-bounded memory and a hard timeout (architecture Sec 9 / tech-spec Sec 9).
- [ ] On `mgmt.set_mode(repo_id, mode)` transitions between `embedded` and `linked`, drain in-flight scans for the repo before flipping; new scans pick up the new mode.
- [ ] Add `internal/ast/isolation/subprocess_test.go` covering: OOM in subprocess returns a typed error and does NOT crash the host; mode flip drains.

### Dependencies
- phase-audit-wal-and-reliability-hardening/stage-audit-wal-reconciler-replay-only

### Test Scenarios
- [ ] Scenario: subprocess-oom-returns-error -- Given a parser subprocess that allocates beyond its rlimit, When the parse runs, Then the host process receives a typed `ParserOOM` error and the host remains running (no panic).
- [ ] Scenario: mode-flip-drains-scans -- Given two in-flight scans for `repo_id=r1`, When `mgmt.set_mode(r1, 'linked')` is called, Then both scans complete under the old mode and the next scan starts under `linked`.

## Stage 9.4: OTel telemetry across all surfaces

### Implementation Steps
- [ ] Add `internal/telemetry/` initialising OTel SDK with the OTLP exporter from `internal/config`.
- [ ] Wrap every verb handler (eval.gate, mgmt.*, ingest.*, policy.*) with a span tagged `verb`, `repo_id`, `policy_version_id`, `degraded`, `degraded_reason`, `verdict` per architecture Sec 8.
- [ ] Export Prometheus metrics for sweep counters (Stage 3.5), aggregator tick duration (Stage 7.1), WAL replay duration (Stage 9.2), and rule engine evaluations/sec (Stage 5.7).
- [ ] Add `internal/telemetry/integration_test.go` running a fake OTLP receiver and asserting spans for each surface arrive with the expected tags.

### Dependencies
- phase-audit-wal-and-reliability-hardening/stage-audit-wal-reconciler-replay-only

### Test Scenarios
- [ ] Scenario: gate-span-carries-verdict-tag -- Given `eval.gate` returning `warn`, When OTLP receives the span, Then the span carries `verdict='warn'`, `degraded=true`, and `degraded_reason='samples_pending'` tags (verifies canonical enum reaches telemetry; iter 1 evaluator item 6).
- [ ] Scenario: prometheus-counter-shape -- Given the aggregator runs one tick, When `/metrics` is scraped, Then `cleancode_aggregator_tick_duration_seconds` exists with the expected bucket labels.

# Phase 10: Linked mode integration and rollout

## Dependencies
- phase-refactor-planner
- phase-audit-wal-and-reliability-hardening

## Stage 10.1: Optional agent memory linked mode adapter

### Implementation Steps
- [ ] Add `internal/linked/` client wrapping the agent-memory cross-repo edge service (architecture Sec 1.6 `ast-mode-default=embedded`).
- [ ] In linked mode, the aggregator (Stage 7.2) reads cross-repo edges from the agent-memory linked-mode endpoint; in embedded mode the field returns empty and the aggregator stamps `degraded_reason='xrepo_edges_unavailable'` for affected outputs.
- [ ] Linked-mode usage is gated by `mgmt.set_mode(repo_id, 'linked')` and the global config flag; default remains embedded.
- [ ] Add `internal/linked/client_test.go` covering: linked endpoint reachable returns edges; unreachable returns empty + degraded stamp.

### Dependencies
- phase-cross-repo-aggregator/stage-system-tier-metric-composer

### Test Scenarios
- [ ] Scenario: linked-mode-uses-edges -- Given linked mode + reachable agent-memory, When the aggregator composes `arch_debt_ratio`, Then xrepo edges are factored in and `degraded=false`.
- [ ] Scenario: linked-mode-unreachable-degrades -- Given linked mode + unreachable agent-memory, When the aggregator composes, Then `degraded=true, degraded_reason='xrepo_edges_unavailable'` is stamped on affected outputs.

## Stage 10.2: Aged mute insights report

### Implementation Steps
- [ ] Add `internal/management/insights/aged_mutes.go` reading the latest `override(mute=true)` row per (scope, rule_id) and reporting any whose `created_at` exceeds a configurable threshold (default 90 days).
- [ ] The report is EXCLUSIVELY an Insights surface read; no enforcement is performed (iter 1 evaluator item 5 -- v1 has NO TTL enforcement in code).
- [ ] Operators unmute by appending an `override(mute=false)` row via `mgmt.override`; the aged-mute row drops off the report on the next read.
- [ ] Expose `mgmt.read.insights.aged_mutes(threshold_days?)` returning the list.
- [ ] Add `internal/management/insights/aged_mutes_test.go` covering: row 100 days old appears in report; subsequent `mute=false` append removes it.

### Dependencies
- phase-policy-steward-and-solid-rule-engine/stage-override-append-only-mute-lifecycle
- phase-evaluator-surface-and-management-surface/stage-management-read-verbs-and-insights-projections

### Test Scenarios
- [ ] Scenario: aged-mute-listed-not-enforced -- Given an `override(mute=true, created_at=now()-100d)`, When the aged-mutes report runs, Then the override appears in the response AND it remains the active mute (no automatic flip; iter 1 evaluator item 5).
- [ ] Scenario: unmute-removes-from-report -- Given an aged mute, When operator appends `override(mute=false)` via `mgmt.override`, Then the next aged-mutes report omits the (scope, rule_id) pair.

## Stage 10.3: Load and conformance tests

### Implementation Steps
- [ ] Add `test/load/` k6 scenario simulating 100 repos and 50 scans/min sustained for 30 minutes; assert p99 `eval.gate` latency under tech-spec Sec 12 SLO targets.
- [ ] Add `test/conformance/canonical_names_test.go` reading every metric_kind reference in `policy/rulepacks/**/*.yaml`, `internal/metrics/recipes/**`, and `internal/aggregator/system_tier.go`, and asserting the set is a subset of the canonical metric_kinds in architecture Sec 1.4 + tech-spec Sec 4.1.1.
- [ ] Add `test/conformance/canonical_states_test.go` asserting Commit.scan_status, ScanRun.status, Verdict, and Override (no `expires_at`) match the canonical enums.
- [ ] Add `test/conformance/wal_scope_test.go` asserting the `internal/audit/wal` package is imported only from `internal/evaluator` and `internal/rule_engine`.

### Dependencies
- phase-refactor-planner/stage-ml-effort-model-loader-and-version-pinning
- phase-audit-wal-and-reliability-hardening/stage-otel-telemetry-across-all-surfaces

### Test Scenarios
- [ ] Scenario: canonical-names-conformance -- Given the conformance test running across the whole repo, When it inventories metric_kind references, Then no reference uses a non-canonical alias (this is the regression test for iter 1 evaluator items 3, 4, 7).
- [ ] Scenario: load-target-met -- Given 100 repos at 50 scans/min for 30 minutes, When k6 reports the p99 `eval.gate` latency, Then it is below the tech-spec Sec 12 SLO.

## Stage 10.4: Cross repo end to end happy path

### Implementation Steps
- [ ] Add `test/e2e/cross_repo_happy_path/` script that: registers three repos via `mgmt.register_repo`, posts coverage uploads for each, runs Metric Ingestor to scanned state, runs aggregator one tick, calls `mgmt.read.cross_repo('coverage_line_ratio', 'package')`, and asserts a single response row with p50/p90/p99/histogram_json populated and `built_at` within freshness window (so no `percentile_stale` banner).
- [ ] The script also calls `eval.gate(repo_id, sha)` for each repo and asserts canonical verdicts in `{pass, warn, block}` only (iter 1 evaluator item 6).
- [ ] Add a follow-up assertion calling `mgmt.read.cross_repo` after advancing the fake clock past `freshness_window_seconds`: assert `degraded=true, degraded_reason='percentile_stale'` appears in the Insights response and that `eval.gate` calls still emit only `samples_pending|policy_signature_invalid|xrepo_edges_unavailable` (never `percentile_stale`).

### Dependencies
- phase-cross-repo-aggregator/stage-insights-surface-percentile-freshness-banner
- phase-refactor-planner/stage-refactor-plan-and-task-generation
- phase-linked-mode-integration-and-rollout/stage-aged-mute-insights-report

### Test Scenarios
- [ ] Scenario: cross-repo-e2e-fresh -- Given three registered repos with coverage uploads and one aggregator tick, When the e2e script asserts on the read paths, Then the cross_repo response has populated percentile columns and `degraded=false`, and `eval.gate` returns a canonical verdict.
- [ ] Scenario: cross-repo-e2e-stale -- Given the same setup with the fake clock advanced past `freshness_window_seconds`, When the script asserts, Then `mgmt.read.cross_repo` carries `percentile_stale` AND `eval.gate` never emits `percentile_stale` (iter 1 evaluator item 8 regression guard).

## Stage 10.5: Rollout playbook and operator runbooks

### Implementation Steps
- [ ] Add `services/clean-code/docs/runbook.md` documenting: registering a repo, retracting a sample, overriding a rule (append-only mute), unmuting (append `mute=false`), rescanning a SHA, rotating the signing key.
- [ ] Add `services/clean-code/docs/rollout.md` describing the embedded -> linked migration: pre-flight checks, expected counter shifts (`degraded_reason='xrepo_edges_unavailable'` should drop to zero after `mgmt.set_mode`).
- [ ] Cross-link to architecture.md and tech-spec.md sibling docs by relative path; do NOT duplicate their content.
- [ ] Add a CHANGELOG entry under `services/clean-code/CHANGELOG.md` listing the v1 surface (schema `clean_code`, 12 foundation metric_kinds, 3 ingested kinds, 7 system-tier kinds, verdict `pass|warn|block`, override append-only no TTL).

### Dependencies
- phase-linked-mode-integration-and-rollout/stage-cross-repo-end-to-end-happy-path

### Test Scenarios
- [ ] Scenario: runbook-references-canonical-verbs -- Given the runbook content, When grepping for verb names, Then only canonical names appear (`mgmt.register_repo`, `mgmt.retract_sample`, `mgmt.rescan`, `mgmt.override`, `policy.publish`, `policy.activate`, `eval.gate`) and no non-canonical names (`Policy.Override.Add`, `Policy.Override.Lift` are absent; iter 1 evaluator item 5).
- [ ] Scenario: changelog-lists-canonical-surface -- Given `services/clean-code/CHANGELOG.md`, When parsing the v1 entry, Then it names: schema `clean_code`, verdict `pass|warn|block`, override has no `expires_at`, and the foundation/system metric_kind counts match the canonical lists.

