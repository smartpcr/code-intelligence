---
title: "clean code"
storyId: "code-intelligence:CLEAN-CODE"
---

> Livedoc: tick each `- [ ]` as work lands. Anchors (e.g. `phase-foundation-and-schema/stage-project-scaffold-and-ci-baseline`) are derived from the heading TITLE only -- no phase or stage numbers in slugs. Every entity name, schema name, enum value, verb name, and metric kind in this plan is the canonical name pinned by sibling `architecture.md` and `tech-spec.md`; do NOT introduce synonyms here.

> Canon alignment guarantees (the doc-wide invariants enforced inline at each citation; audit by `grep -F` if in doubt):
> - **ONE PostgreSQL schema:** `clean_code`. Words like `catalog`, `lifecycle`, `measurement`, `policy`, `audit`, `refactor` appear ONLY as migration file-name fragments and as logical table-grouping prose -- NEVER as schema names. There is no `catalog.`, `measurement.`, `policy.`, `audit.`, or `refactor.` schema and no qualified table reference of that form anywhere below.
> - **No invented tables:** `rule_pack_revision`, `policy_override`, `audit_event`, `audit_anchor`, `effort_estimate` are NEVER created. Each one is explicitly excluded with a negative migration step and a regression-guard test scenario.
> - **No procedural verbs on Measurement:** active-row uniqueness is enforced via the `metric_sample_active` side relation's PRIMARY KEY on `(repo_id, sha, scope_id, metric_kind, metric_version)` per tech-spec Sec 7.1.b lines 1070-1119 and the Sec 10A locked pin lines 1659-1675 (PostgreSQL cannot express a partial unique index whose predicate references a subquery against another table; the side relation carries the identical semantic guarantee `at most one active sample per quintuple`). `metric_sample` itself stays append-only (G3); BOTH the Metric Ingestor and Cross-Repo Aggregator INSERT/UPSERT the pointer row in `metric_sample_active` (`metric_kind` partitions the table by writer), and DELETE is REVOKEd from both roles per tech-spec Sec 7.2 line 1248 -- this is the only mutable table in the Measurement bounded context (mutation = UPSERT of the `sample_id` column), and the mutation is by design, not an exception. No `swap_active` function, trigger, or procedure exists; the literal string `swap_active` appears only in negative ("do NOT use") clauses.
> - **Canonical RepoEvent.kind enum:** `registered | retired | retract_intent | mode_changed` per architecture Sec 5.1.4 lines 877-884. Past-tense forms are normative; `register`, `retire`, `rescan_intent`, and `mode_change` (without `d`) are NEVER emitted as `repo_event.kind` values. The `mgmt.rescan` verb has NO corresponding RepoEvent kind in v1 -- it dispatches a service-internal rescan request and opens a `scan_run(kind='full')` directly.
> - **Canonical MetricSample columns:** `(sample_id, repo_id, sha, scope_id, metric_kind, metric_version, value, pack, source, degraded, degraded_reason, producer_run_id, attrs_json, created_at)` per architecture Sec 5.2.1 lines 889-906. `unit` lives on the `metric_kind` catalog (NOT `metric_sample`). `policy_version_id` lives on `finding` + `hot_spot` (NOT `metric_sample`). Run attribution column is `producer_run_id` (NOT `scan_run_id`). Timestamp column is `created_at` (NOT `computed_at`). The closed set for `degraded_reason` is `xrepo_edges_unavailable | samples_pending | policy_signature_invalid | percentile_stale` per architecture Sec 8.2 (percentile_stale is Insights-only; eval.gate rejects it).
> - **Canonical ScopeBinding shape:** `(scope_id PK, repo_id, scope_kind, canonical_signature, first_seen_sha, agent_memory_node_id UUID NULL, attrs_json, created_at)` per architecture Sec 5.2.3 lines 1039-1050. `scope_id` is the deterministic UUIDv5 of `(repo_id, scope_kind, canonical_signature, first_seen_sha)` -- the SAME signature at a LATER SHA resolves to the SAME `scope_id` (G2 stability across SHAs). The scope_kind enum is `repo | package | file | class | interface | method | block` -- NEVER `function | module`. There is NO `sha`, NO `parent_scope_id`, NO `file_path`, NO `span_start`/`span_end` column on `scope_binding` in v1.
> - **Canonical snapshot/percentile shapes:** `repo_metric_snapshot(snapshot_id PK, repo_id, scope_kind, metric_kind, count, mean, p50, p90, p99, built_at)` -- NO `sha` column (snapshots are per-tick latest, not per-SHA). `cross_repo_percentile(percentile_id PK, scope_kind, metric_kind, p50, p90, p99, histogram_json, built_at)`. `portfolio_snapshot(portfolio_snapshot_id PK, metric_kind, scope_kind, aggregate_json, built_at)` -- NO `sum_value`, NO `weighted_mean` column (the cross-repo weighted aggregates live inside `aggregate_json`). Per architecture Sec 5.2.4-5.2.6 lines 1052-1089.
> - **Canonical Policy/Finding/Refactor schema:** `policy_version(policy_version_id PK, name, rule_refs JSONB, threshold_refs JSONB, refactor_weights JSONB, signature BYTEA, created_at)` per Sec 5.3.3 -- NO `signing_key_id`, NO `rulepack_set_hash`, NO `signed_at`. `policy_activation(activation_id PK, policy_version_id FK, activated_by, created_at)` per Sec 5.3.4 -- NO `scope`, NO `deactivated_at` (latest-row-wins). `override(override_id PK, rule_id FK, scope_filter JSONB, mute, reason, actor_id, created_at)` per Sec 5.3.6 -- NO `policy_version_id`, NO `expires_at`; the column is `actor_id` not `created_by`. `finding.delta` is the canonical enum `new | newly_failing | unchanged | resolved` per Sec 5.4.1 lines 1186-1190 (NEVER `regression`, `improvement`, `flat`). `refactor_task(task_id PK, plan_id FK, scope_id, kind, effort_hours DOUBLE, rule_id, description_md, created_at)` per Sec 5.5.3 -- `kind` enum is `split_class | extract_method | invert_dependency | break_cycle | consolidate_duplication`; NO `status`, NO `expected_metric_delta` column.
> - **v1 language coverage:** Go, Python, TypeScript, Java ONLY per tech-spec Sec 8.6 lines 1005-1016. C# is NOT a v1 language; the registry guard refuses to register it.
> - **modification_count_in_window provenance:** the materialiser writes `pack='base'`, `source='computed'` per tech-spec Sec 4.1.1 lines 287-291 and Sec 4.11 lines 444-454 -- NEVER `source='derived'` (the materialiser IS the computing writer; ingested provenance lives on `attrs_json` per C19).
> - **External ingest sha_binding:** `ingest.coverage` and `ingest.test_balance` both use `sha_binding='single'` (one SHA per upload); `ingest.churn` and `ingest.defects` use `sha_binding='per_row'`. Per architecture Sec 6.4 lines 1364-1369 and tech-spec Sec 4.11 lines 429-434. The `test_balance` body media-type is `application/json` with rows `{scope_id, attempt_count, pass_count}` (NOT JUnit XML).
> - **Single-tenant v1:** NO `tenant_id` column anywhere in v1; the Steward signing-key lookup uses `signing_key_id` only per tech-spec Sec 4.14 lines 480-493. Multi-tenant v2 uses per-schema isolation `clean_code_<tenant>`, not row-level columns.
> - **Canonical verb namespaces:** `ingest.coverage`, `ingest.test_balance`, `ingest.churn`, `ingest.defects` (`ingest.*`); `policy.publish`, `policy.activate`, `policy.publish_rulepack` (`policy.*`); `mgmt.override`, `mgmt.register_repo`, `mgmt.set_mode`, `mgmt.retract_sample`, `mgmt.rescan`, `mgmt.read.*` (`mgmt.*`); `eval.gate` (`eval.*`). Per tech-spec Sec 8.5 lines 960-995. The verbs `policy.rulepack.add` and `policy.rulepack.remove` are NEVER registered (they return `UNIMPLEMENTED`).
> - **OCP rule inputs:** `fan_in` (class) + `modification_count_in_window` (class) per architecture Sec 3.5.1.b lines 504-513 -- NEVER `depth_of_inheritance`. LSP uses `depth_of_inheritance`.
> - **System-tier fail-safe:** When upstream samples are missing or xrepo edges are unavailable, the aggregator WRITES a system-tier `metric_sample` row with `degraded=true, degraded_reason=samples_pending|xrepo_edges_unavailable` per architecture Sec 3.10 step 4 lines 637-657 and tech-spec Sec 4.2 lines 325-339. It NEVER silently drops the row. `value` may be NULL when `degraded=true`.
> - **eval.gate synchronous SOLID delegation:** `eval.gate` DELEGATES the rule pass to the SOLID Rule Engine's synchronous mode `rule_engine.RunSync(ctx, repo_id, sha, scope?, policy_version_id)` which writes **ONE `evaluation_run(caller='eval_gate')` + ONE `evaluation_verdict` + N `finding` rows in the same transaction** and returns all three IDs; the verdict is computed by the engine via severity-rollup over the freshly-written findings, and eval.gate does NOT separately append `evaluation_verdict` on the happy path. On the two short-circuit degraded paths (signature-invalid, samples_pending) where the engine is NOT invoked, eval.gate writes a canonical run+verdict pair itself (ONE `evaluation_run(caller='eval_gate')` + ONE `evaluation_verdict(evaluation_run_id=<that run>, verdict='warn', degraded=true, degraded_reason, created_at)` + zero findings, all in the same transaction). The Audit schema is uniform across all three paths: every `evaluation_verdict` row carries a non-null `evaluation_run_id` FK and uses `created_at` -- there is NO nullable FK shortcut, NO `scope` column, and NO `settled_at` column per architecture Sec 5.4.3 lines 1203-1212. Per architecture Sec 3.6 lines 526-540 and Sec 4.2 lines 755-762.
> - **Audit/verdict writer ACL (three roles, all append-only):** `clean_code_evaluator`, `clean_code_solid_batch`, and `clean_code_wal_reconciler` ALL get `INSERT, SELECT` on `evaluation_run`, `evaluation_verdict`, `finding` per tech-spec Sec 7.2 lines 1256-1261. `UPDATE`/`DELETE` is revoked from all roles including `PUBLIC`. The Rule Engine is the WRITER on the rule-pass paths; the Evaluator is co-grantee for the degraded short-circuit paths; the WAL Reconciler is co-grantee for replay only. There is no "sole writer of `finding`" carve-out in v1.
> - **Active-pointer table two-writer carve-out:** `metric_sample_active` and `metric_retraction` are granted `INSERT, SELECT, UPDATE`/`INSERT, SELECT` to BOTH `clean_code_metric_ingestor` AND `clean_code_xrepo_aggregator` per tech-spec Sec 7.2 lines 1212-1248. The two roles never collide on a quintuple because `metric_kind` partitions the pointer table by writer (Ingestor: `pack IN ('base','solid','ingested')`; Aggregator: `pack='system'`). `metric_sample` UPDATE/DELETE is revoked for both writers under G3 row immutability; `metric_retraction`/`metric_sample_active` DELETE is revoked for both (pointer rows are upserted, not deleted in v1).
> - **Refactor effort model inheritance chain:** `refactor_plan` has NO `policy_version_id` column per architecture Sec 5.5.2 lines 1228-1237. Effort-model version inheritance goes `refactor_task -> refactor_plan -> hot_spot.policy_version_id -> policy_version.refactor_weights.effort_model_version` (architecture Sec 5.5.1 lines 1216-1226 puts `policy_version_id` on each `hot_spot`). Reproducing an estimate dereferences ONE of the hotspots referenced by `refactor_plan.hotspot_ids`.
> - **SLO source-of-truth:** the SLO targets table is tech-spec Sec 8.3 lines 907-916 (`eval.gate` p50/p95/p99 = 200ms/800ms/2s, Metric Ingestor sample-write 5k/s, etc.). The plan does NOT cite `Sec 12` for SLOs.
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
- [ ] Add `services/clean-code/deploy/local/docker-compose.yml` with PostgreSQL 16 (per tech-spec C9 / Sec 8.1) and the `clean-coded` service binding the configured port; expose Prometheus scrape port and OTel collector endpoint.
- [ ] Add `services/clean-code/internal/config/` loader that reads env + file config, exposing the operator pins listed in architecture Sec 1.6 (`ast-mode-default`, `external-metric-coverage-format`, `gate-degraded-policy`, `policy-signing-required`, `refactor-effort-source`) as typed fields with defaults from tech-spec Sec 8.2.
- [ ] Add `services/clean-code/internal/logging/` slog wrapper enforcing structured JSON logs and request-id propagation (architecture Sec 8 telemetry invariant).
- [ ] Add `services/clean-code/internal/version/` exporting `Version`, `Commit`, `BuildTime` plus a minimal `/healthz` (process liveness, returns HTTP 200 with `{"status":"ok","version":...,"commit":...,"build_time":...}`) and `/readyz` (returns HTTP 200 only when PostgreSQL pool, OTel exporter, and signing-key cache have all initialised; 503 otherwise) HTTP handler used by all surfaces. The handler ships a real production implementation in this stage (no follow-up work outstanding).

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
- [ ] Encode `RepoEvent.kind` as the canonical enum `registered | retired | retract_intent | mode_changed` matching architecture Sec 5.1.4 lines 877-884.
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
- [ ] Add `migrations/0002_measurement.up.sql` creating Measurement tables in the same `clean_code` schema: `metric_sample`, `metric_retraction`, `metric_sample_active`, `scope_binding`, `repo_metric_snapshot`, `cross_repo_percentile`, `portfolio_snapshot`, matching architecture Sec 5.2 and tech-spec Sec 7.1.b DDL.
- [ ] Encode the canonical `MetricSample` columns `sample_id`, `repo_id`, `sha`, `scope_id`, `metric_kind`, `metric_version`, `value`, `pack`, `source`, `degraded`, `degraded_reason`, `producer_run_id`, `attrs_json`, `created_at` (architecture Sec 5.2.1 lines 889-906). `unit` lives on the `metric_kind` Catalog row (architecture Sec 5.1.3 line 874), NOT on `MetricSample`; `policy_version_id` lives on `Finding` (Sec 5.4.1) and `HotSpot` (Sec 5.5.1), NOT on `MetricSample`; the run attribution column is `producer_run_id` (FK to `ScanRun`), NOT `scan_run_id`; the timestamp column is `created_at`, NOT `computed_at`.
- [ ] Encode `MetricSample.pack` as enum `base | solid | system | ingested` and `MetricSample.source` as enum `computed | derived | ingested` (architecture Sec 1.4.1 / Sec 5.2.1). `degraded BOOL NOT NULL DEFAULT false` and `degraded_reason TEXT NULL`; `degraded_reason` constrained to the closed set `xrepo_edges_unavailable | samples_pending | policy_signature_invalid | percentile_stale` (architecture Sec 5.2.1 lines 902-903 / Sec 8.2).
- [ ] Implement active-row uniqueness via the SIDE RELATION `metric_sample_active(repo_id, sha, scope_id, metric_kind, metric_version, sample_id FK -> metric_sample(sample_id))` with `PRIMARY KEY (repo_id, sha, scope_id, metric_kind, metric_version)` and `CREATE UNIQUE INDEX metric_sample_active_sample_id_uniq ON metric_sample_active (sample_id)` per tech-spec Sec 7.1.b lines 1070-1119 and locked-decision pin Sec 10A lines 1659-1675. The architecture-mandated partial unique index over a subquery is not expressible in PostgreSQL DDL; the side relation's PK over the quintuple carries the identical semantic guarantee.
- [ ] Add `MetricRetraction(retraction_id PK, sample_id UNIQUE FK, reason, appended_by, created_at)` per architecture Sec 5.2.2; document append-only invariant in a table COMMENT. The `UNIQUE` on `sample_id` prevents double-retraction.
- [ ] Add `ScopeBinding(scope_id PK, repo_id FK, scope_kind ENUM, canonical_signature TEXT, first_seen_sha TEXT, agent_memory_node_id UUID NULL, attrs_json JSONB, created_at)` with the canonical `scope_kind` enum `repo | package | file | class | interface | method | block` per architecture Sec 5.2.3 lines 1039-1050. `scope_id` is a **deterministic UUIDv5 of `(repo_id, scope_kind, canonical_signature, first_seen_sha)`** -- **stable across SHAs** so the same scope identity persists when its content evolves. SHA is NOT part of `scope_id` (G2).
- [ ] Add `RepoMetricSnapshot(snapshot_id PK, repo_id FK, metric_kind, scope_kind, count BIGINT, mean DOUBLE, p50, p90, p99, built_at)` per architecture Sec 5.2.4 lines 1052-1065. No `sha` column (snapshots are repo-wide latest aggregates), no `sample_count` alias (the canonical column name is `count`).
- [ ] Add `CrossRepoPercentile(percentile_id PK, metric_kind, scope_kind, histogram_json, p50, p90, p99, built_at)` per architecture Sec 5.2.5 lines 1067-1078.
- [ ] Add `PortfolioSnapshot(portfolio_snapshot_id PK, metric_kind, scope_kind, repo_count INT, aggregate_json JSONB, built_at)` per architecture Sec 5.2.6 lines 1080-1089. No `sum_value`/`weighted_mean` columns -- those aggregates are encoded as keys inside `aggregate_json` (operator-pinned aggregate shape).
- [ ] Add `migrations/0002_measurement.down.sql` reversing 0002.

### Dependencies
- phase-foundation-and-schema/stage-catalog-and-lifecycle-schema-migrations

### Test Scenarios
- [ ] Scenario: active-row-quintuple-uniqueness -- Given a `metric_sample_active` row already present for `(repo, sha, scope, metric_kind, metric_version)`, When a second INSERT into `metric_sample_active` for the same quintuple runs, Then it fails with a PRIMARY KEY violation; an `INSERT ... ON CONFLICT (repo_id, sha, scope_id, metric_kind, metric_version) DO UPDATE SET sample_id=EXCLUDED.sample_id` succeeds (UPSERT/repointing is the only allowed way to change the pointer target, since tech-spec Sec 7.2 line 1248 REVOKEs DELETE on `metric_sample_active` from both writer roles). `metric_sample` itself remains append-only (G3) and is never UPDATEd.
- [ ] Scenario: pack-source-enum-rejects-invalid -- Given the `metric_sample` table, When an INSERT supplies `pack='unknown'` or `source='external'`, Then PostgreSQL rejects it; only canonical pack/source enums are accepted.
- [ ] Scenario: degraded-defaults-false -- Given the `metric_sample` table, When a row is INSERTed without a `degraded` value, Then the row materialises with `degraded=false` from the column DEFAULT and `degraded_reason IS NULL`.
- [ ] Scenario: scope-binding-stable-across-shas -- Given two SHAs of the same `(repo_id, scope_kind, canonical_signature, first_seen_sha)`, When the ScopeBinding writer runs at both SHAs, Then the second call's `scope_id` equals the first's (G2 stability) and only one row exists in `scope_binding`.
- [ ] Scenario: cross-repo-percentile-shape -- Given the `cross_repo_percentile` table, When the migration runs, Then it has exactly the columns `percentile_id, metric_kind, scope_kind, histogram_json, p50, p90, p99, built_at` (no `p95.system` or other invented kinds materialised as rows).

## Stage 1.4: Policy and Audit and Refactor schema migrations

### Implementation Steps
- [ ] Add `migrations/0003_policy_audit_refactor.up.sql` creating Policy tables `rule`, `rule_pack`, `policy_version`, `policy_activation`, `threshold`, `override` and Audit tables `evaluation_run`, `evaluation_verdict`, `finding` and Refactor tables `hot_spot`, `refactor_plan`, `refactor_task` -- all in the single `clean_code` schema (per tech-spec C9 / Sec 8.1.3). No invented tables (`rule_pack_revision`, `policy_override`, `audit_event`, `audit_anchor`, `effort_estimate` are NOT created -- they were rejected by iter 1 evaluator item 1).
- [ ] Encode `PolicyVersion` as immutable per architecture Sec 5.3.3 lines 1121-1138: `(policy_version_id PK, name TEXT, rule_refs JSONB, threshold_refs JSONB, refactor_weights JSONB, signature BYTEA, created_at)`. `rule_refs` is `[{rule_id, version}]`; `threshold_refs` is `[{threshold_id}]`; `refactor_weights` is `{alpha, beta, gamma, delta, effort_model_version, window_days, freshness_window_seconds?}`. `signature` is the operator-required v1 cryptographic signature over `(rule_refs, threshold_refs, refactor_weights)`. No `signed_at` / `signing_key_id` / `rulepack_set_hash` columns -- those are not in the architecture canon.
- [ ] Encode `PolicyActivation` per architecture Sec 5.3.4 lines 1140-1147: `(activation_id PK, policy_version_id FK, activated_by TEXT, created_at)`. The latest row by `created_at` defines the active policy -- there is NO `scope` column, NO `deactivated_at` column, and NO partial unique index. Activation is global per deployment in v1 (single-tenant per tech-spec Sec 4.14).
- [ ] Encode `Override` per architecture Sec 5.3.6 lines 1160-1170: `(override_id PK, rule_id TEXT FK -> Rule.rule_id, scope_filter JSONB, mute BOOL, reason TEXT, actor_id TEXT, created_at)`. `scope_filter` is `{repo_id, scope_kind, scope_signature_glob}`. No `expires_at` column and NO TTL enforcement, since tech-spec Sec 10A pins v1 to latest-row-wins (iter 1 evaluator item 5). No `policy_version_id` column -- overrides bind to rules, not policy versions. Use `actor_id`, NOT `created_by`.
- [ ] Encode `EvaluationVerdict` per architecture Sec 5.4.3 lines 1203-1212: `(verdict_id PK, evaluation_run_id FK, verdict ENUM('pass'|'warn'|'block'), degraded BOOL, degraded_reason TEXT?, created_at)`. `degraded_reason` is constrained at the column level to the architecture Sec 8.2 closed set `xrepo_edges_unavailable | samples_pending | policy_signature_invalid | percentile_stale`.
- [ ] Encode `EvaluationRun` per architecture Sec 5.4.2 lines 1219-1228 as `(evaluation_run_id PK, repo_id, sha, policy_version_id FK, caller ENUM('eval_gate'|'batch_refresh'), scope_id UUID NULL, created_at)`. The base columns ship in migration 0003; `scope_id` is added by Stage 5.7 iter-7 migration `0008_evaluation_run_scope_id.up.sql` together with the composite index `evaluation_run_dedup_idx(repo_id, sha, policy_version_id, caller, scope_id, created_at DESC)`. `scope_id` is nullable -- `NULL` is the canonical whole-SHA evaluation (every `batch_refresh` run by construction and every `eval_gate` call with no `scope?` argument); non-null records the `scope?` argument of `rule_engine.RunSync(repo_id, sha, scope?, policy_version_id)`. Cross-replica run dedup in `Store.LookupRecentCanonicalRun` matches with the null-safe `IS NOT DISTINCT FROM` operator so a scoped row NEVER matches an unscoped lookup (or vice versa). Audit WAL reconciler replay rows retain meaningful semantics under the legacy nullable shape.
- [ ] Encode `Finding.delta` per architecture Sec 5.4.1 lines 1174-1190 as the canonical enum `new | newly_failing | unchanged | resolved` (NOT `regression | improvement | flat | new | resolved`). Normative semantics: `new` = rule fired at this scope for the FIRST SHA ever; `newly_failing` = the regression-to-block bucket (previously not severity=block at this scope, now is); `unchanged` = was severity=block at the previous SHA and still is at this SHA; `resolved` = was severity=block at the previous SHA and is no longer present (or fires at a lower severity) at this SHA. `mgmt.read.regressions` consumes `delta='newly_failing'` as the regressions bucket.
- [ ] Encode `Finding` columns per architecture Sec 5.4.1: `(finding_id PK, evaluation_run_id FK, repo_id, sha, scope_id FK, rule_id, rule_version, policy_version_id FK, metric_sample_ids JSONB, severity ENUM('info'|'warn'|'block'), delta, explanation_md, created_at)`.
- [ ] Encode `RefactorTask` per architecture Sec 5.5.3 lines 1239-1250: `(task_id PK, plan_id FK, scope_id, kind ENUM, effort_hours DOUBLE, rule_id TEXT, description_md TEXT, created_at)`. `kind` enum is `split_class | extract_method | invert_dependency | break_cycle | consolidate_duplication` (NOT `extract_function | introduce_interface | reduce_inheritance | reduce_coupling | reduce_lcom | reduce_duplication`); add additional canonical values verbatim from architecture Sec 5.5.3. NO `status` column (life-cycle state is out of v1 scope per architecture Sec 5.5.3), NO `expected_metric_delta` column.
- [ ] Encode `RefactorPlan` per architecture Sec 5.5.2 lines 1228-1237: `(plan_id PK, repo_id, sha, hotspot_ids JSONB, summary_md TEXT, created_at)`. No `policy_version_id` column -- policy attribution lives on each `hot_spot` row (architecture Sec 5.5.1).
- [ ] Encode `HotSpot` per architecture Sec 5.5.1 lines 1216-1226: `(hotspot_id PK, repo_id, sha, scope_id, score DOUBLE, policy_version_id FK, created_at)`. No per-input z-score columns (those are intermediate values; only the composite `score` is persisted).
- [ ] Add table COMMENTs on `evaluation_run`, `evaluation_verdict`, `finding` naming the Audit WAL as their durability mechanism and pointing at architecture Sec 7.10.
- [ ] Add `migrations/0003_policy_audit_refactor.down.sql` reversing 0003.

### Dependencies
- phase-foundation-and-schema/stage-catalog-and-lifecycle-schema-migrations

### Test Scenarios
- [ ] Scenario: verdict-enum-only-canonical -- Given the `evaluation_verdict` table, When an INSERT supplies `verdict='fail'` or `verdict='gated'`, Then PostgreSQL rejects it (only `pass|warn|block` are accepted) -- guards against iter 1 evaluator item 6 regression.
- [ ] Scenario: override-no-expires-column -- Given the `override` table after `migrate-up`, When `\d clean_code.override` runs, Then no `expires_at` column exists (guards iter 1 evaluator item 5).
- [ ] Scenario: degraded-reason-closed-set -- Given the `evaluation_verdict` table, When an INSERT supplies `degraded_reason='other'`, Then it fails the check constraint; the four canonical reasons (incl. `percentile_stale` reserved for Insights only) all succeed at column level.
- [ ] Scenario: finding-delta-canonical -- Given the `finding` table, When an INSERT supplies `delta='regression'` or `delta='improvement'` or `delta='flat'`, Then PostgreSQL rejects it (only `new|newly_failing|unchanged|resolved` are accepted) -- canon-guard against the iter 3 drift evaluator item 7.
- [ ] Scenario: refactor-task-no-status-column -- Given the `refactor_task` table after `migrate-up`, When `\d clean_code.refactor_task` runs, Then no `status` and no `expected_metric_delta` columns exist; only the canonical Sec 5.5.3 columns are present.
- [ ] Scenario: policy-activation-latest-row-wins -- Given two `policy_activation` rows for the same policy chain, When the evaluator resolves the active policy, Then it reads the row with `MAX(created_at)` -- there is no scope column, no partial unique index, no deactivated_at.

## Stage 1.5: PostgreSQL role grants and schema isolation

### Implementation Steps
- [ ] Add `migrations/0004_roles.up.sql` creating PostgreSQL roles `clean_code_repo_indexer`, `clean_code_metric_ingestor`, `clean_code_xrepo_aggregator`, `clean_code_policy_steward`, `clean_code_solid_batch`, `clean_code_evaluator`, `clean_code_wal_reconciler`, `clean_code_refactor_planner`, `clean_code_management` per tech-spec Sec 7.2 lines 1232-1261 (the canonical role names appearing on the actual `GRANT`/`REVOKE` statements). NO `clean_code_aggregator`, `clean_code_rule_engine`, or `clean_code_audit_replayer` aliases -- those names are NEVER created in v1.
- [ ] Grant **both** Metric Ingestor **and** Cross-Repo Aggregator `INSERT, SELECT` on `metric_sample` and `metric_retraction`, and `INSERT, SELECT, UPDATE` on `metric_sample_active`, per the C5 two-writer carve-out pinned in tech-spec Sec 7.2 lines 1212-1248. The two roles never collide on a quintuple because `metric_kind` partitions the pointer table by writer (Ingestor: `pack IN ('base','solid','ingested')`; Aggregator: `pack='system'` AND `source='derived'`); application-layer per-`metric_kind` filtering enforces the carve-out, while DB-level `REVOKE UPDATE, DELETE` on `metric_sample` and `REVOKE DELETE` on `metric_retraction`/`metric_sample_active` keep G3 row immutability for both roles. Grant Metric Ingestor sole UPDATE of `commit.scan_status`; grant Repo Indexer INSERT on `commit` (initial row only) and `repo_event`.
- [ ] Grant Policy Steward sole INSERT on `rule`, `rule_pack`, `policy_version`, `policy_activation`, `threshold`, `override`.
- [ ] Grant `INSERT, SELECT` on `evaluation_run`, `evaluation_verdict`, and `finding` to **three** roles in parallel -- `clean_code_evaluator` (Evaluator Surface, `caller='eval_gate'`), `clean_code_solid_batch` (SOLID Rule Engine batch worker, `caller='batch_refresh'`), and `clean_code_wal_reconciler` (WAL Reconciler, replay-only) -- per tech-spec Sec 7.2 lines 1256-1261. `REVOKE UPDATE, DELETE` on all three Audit tables for all three roles plus `PUBLIC`, preserving append-only semantics under G1. The synchronous Rule Engine call from `eval.gate` writes through `clean_code_solid_batch`-equivalent permissions inherited via the in-process `clean_code_evaluator` connection; both writer paths share the same three append-only ACL.
- [ ] Grant Cross-Repo Aggregator sole INSERT on `repo_metric_snapshot`, `cross_repo_percentile`, `portfolio_snapshot`; the `metric_sample`/`metric_retraction`/`metric_sample_active` writer carve-out is granted above and restricted to `pack='system'` rows at the application layer.
- [ ] Grant Refactor Planner sole INSERT on `hot_spot`, `refactor_plan`, `refactor_task`.
- [ ] Step 141 above already grants `clean_code_wal_reconciler` the same `INSERT, SELECT` on the three Audit tables as the other two roles -- this is the canonical replay-only grant per tech-spec Sec 7.2 lines 1258-1261. There is NO separate `SELECT-only` grant: the Reconciler MUST INSERT replayed rows that are missing from the Audit tables, so it owns the same three-way append-only grant as the Evaluator and SOLID batch worker. `UPDATE`/`DELETE` is revoked from `clean_code_wal_reconciler` along with the other two roles per Sec 7.2 line 1261, enforcing replay-only-not-modify semantics at the DB layer.
- [ ] Add `internal/storage/roles_test.go` that connects as each role and verifies the writer-ownership matrix. Examples: `clean_code_evaluator` INSERT into `evaluation_run`/`evaluation_verdict`/`finding` succeeds (canonical Audit grant per tech-spec Sec 7.2 lines 1258-1260) but its INSERT into `metric_sample` is denied (no Measurement grant); `clean_code_repo_indexer` INSERT into `finding` is denied (Repo Indexer is NOT in the three-role Audit grant); `clean_code_metric_ingestor` UPDATE on `metric_sample` is denied (G3 immutability via Sec 7.2 line 1246 REVOKE); `clean_code_xrepo_aggregator` INSERT into `metric_sample` with `pack='base'` succeeds at the DB layer but is rejected by the application-layer per-`metric_kind` partitioning check (Aggregator only writes `pack='system'` rows). NO test asserts `clean_code_evaluator` is denied INSERT into `finding` -- that grant exists per tech-spec Sec 7.2 line 1260.

### Dependencies
- phase-foundation-and-schema/stage-measurement-schema-and-active-row-index
- phase-foundation-and-schema/stage-policy-and-audit-and-refactor-schema-migrations

### Test Scenarios
- [ ] Scenario: role-isolation-matrix -- Given each writer role bound to a session, When that role attempts INSERT on a table outside its writer-ownership per architecture G1 + the tech-spec Sec 7.2 two-writer carve-out, Then PostgreSQL returns permission denied; the role's owned writes succeed (Ingestor + Aggregator both write `metric_sample`/`metric_retraction`/`metric_sample_active`; Evaluator + SOLID batch + WAL reconciler all write the three Audit tables).
- [ ] Scenario: audit-tables-three-writer-grant -- Given the three Audit tables, When `clean_code_evaluator`, `clean_code_solid_batch`, and `clean_code_wal_reconciler` each attempt INSERT on `evaluation_run`/`evaluation_verdict`/`finding`, Then all three succeed; any OTHER role (Ingestor, Aggregator, Policy Steward, Refactor Planner, Repo Indexer, Management) is denied; UPDATE and DELETE are revoked from all roles including `PUBLIC` per tech-spec Sec 7.2 line 1261.
- [ ] Scenario: aggregator-also-writes-active-pointer -- Given the aggregator role, When it INSERTs a `pack='system'` row into `metric_sample` AND upserts the matching `metric_sample_active` pointer, Then both succeed (the two-writer carve-out from tech-spec Sec 7.2 lines 1212-1248); a `pack='base'` attempt by the aggregator is rejected by the application-layer per-`metric_kind` partitioning check.

# Phase 2: AST Adapter and foundation tier compute

## Dependencies
- phase-foundation-and-schema

## Stage 2.1: Tree sitter parser fleet and canonical AST proto

### Implementation Steps
- [ ] Add `proto/ast/v1/ast.proto` defining `AstFile`, `AstScope`, `AstSymbol`, `AstEdge` with the canonical `scope_kind` enum `repo | package | file | class | interface | method | block` (architecture Sec 5.2.3 lines 1039-1050) so all recipes consume a single shape.
- [ ] Wire `make proto` to generate Go bindings into `internal/ast/v1/`.
- [ ] Add `internal/ast/parser/` Tree-sitter adapter wrapping the grammars for the v1-pinned languages **Go, Python, TypeScript, Java** (tech-spec Sec 8.6 lines 1005-1016 -- the org's top-4 languages by repo count; C#, Rust, and other languages are post-v1 recipe-pack additions per Sec 8.6) behind a `Parser` interface with `Parse(ctx, path, bytes) (*AstFile, error)`.
- [ ] Add `internal/ast/parser/registry.go` exposing per-language parser factories selected by file extension + Linguist-style content sniff; the registry refuses to register a language outside the v1 pin.
- [ ] Add language-tagged unit tests under `internal/ast/parser/testdata/<lang>/` covering at least one fixture per supported language and asserting the canonical AST proto fields are populated.

### Dependencies
- phase-foundation-and-schema/stage-postgresql-role-grants-and-schema-isolation

### Test Scenarios
- [ ] Scenario: parser-supports-v1-four-languages -- Given a fixture file per v1-pinned language (Go, Python, TypeScript, Java), When the registry returns a parser and `Parse` runs, Then each returns a non-empty `AstFile` with the language tag set and at least one `AstScope`; attempting to register a fifth language (e.g. C#) fails the registry guard per tech-spec Sec 8.6 v1 pin.
- [ ] Scenario: proto-round-trip -- Given a parsed `AstFile`, When it is serialised to protobuf wire format and deserialised, Then the resulting struct equals the original (no information loss).

## Stage 2.2: Scope identity derivation and ScopeBinding writer

### Implementation Steps
- [ ] Add `internal/ast/scope/identity.go` computing `scope_id` as a deterministic UUIDv5 of `(repo_id, scope_kind, canonical_signature, first_seen_sha)` per architecture Sec 5.2.3 lines 1039-1050. **SHA is NOT part of `scope_id`** -- `first_seen_sha` is the SHA AT WHICH THIS SIGNATURE FIRST APPEARED (recorded as a column on `scope_binding`); subsequent SHAs that contain the same `(repo_id, scope_kind, canonical_signature)` resolve to the SAME `scope_id` (G2 stability).
- [ ] Add `internal/ast/scope/signature.go` building the `canonical_signature` per scope kind (method: full path + parameter type list e.g. `pkg.Foo#bar(int)`; class/interface: full path; file: file path; package: directory path; block: container-method-signature + intra-method ordinal) using the same recipe as agent-memory `Node.canonical_signature` so the cross-service `agent_memory_node_id` link is stable when running in `linked` mode.
- [ ] Add `internal/storage/scope_binding_writer.go` performing batched `INSERT ... ON CONFLICT (scope_id) DO NOTHING` into `scope_binding`, writing `(scope_id, repo_id, scope_kind, canonical_signature, first_seen_sha, agent_memory_node_id?, attrs_json, created_at)`; on conflict the existing row's `first_seen_sha` is preserved (it is the FIRST SHA, immutable).
- [ ] Wire the writer behind the Metric Ingestor (only ingestor calls scope writer to satisfy writer-ownership G1).
- [ ] Add `internal/ast/scope/identity_test.go` verifying determinism (same `(repo_id, scope_kind, canonical_signature, first_seen_sha)` => same UUID), stability across SHAs (same signature at SHA A and SHA B => same `scope_id` -- SHA does NOT change identity), and uniqueness (different scope_kind or signature => different UUID).

### Dependencies
- phase-ast-adapter-and-foundation-tier-compute/stage-tree-sitter-parser-fleet-and-canonical-ast-proto

### Test Scenarios
- [ ] Scenario: scope-id-determinism -- Given the same `(repo_id, scope_kind, canonical_signature, first_seen_sha)` invoked twice, When `DeriveScopeID` runs, Then it returns the same UUID.
- [ ] Scenario: scope-id-stable-across-shas -- Given a signature `pkg.Foo#bar(int)` first seen at SHA A, When the same signature appears at SHA B, Then `DeriveScopeID` at SHA B returns the SAME `scope_id` as at SHA A (G2 stability; SHA is not part of identity).
- [ ] Scenario: scope-binding-idempotent-write -- Given a `scope_binding` row already present, When the writer re-inserts the same `scope_id` at a different SHA, Then no error surfaces, the row count remains unchanged, and `first_seen_sha` is preserved.

## Stage 2.3: Base pack foundation recipes

### Implementation Steps
- [ ] Add `internal/metrics/recipes/` with one file per recipe, each implementing the `Recipe.Compute(ast *AstFile) []MetricSampleDraft` interface and tagging output with `pack='base'`, `source='computed'`, and the canonical `metric_kind` value.
- [ ] Implement `recipes/cyclo.go` emitting `metric_kind='cyclo'` (architecture Sec 1.4.1 row 1) at canonical scope_kinds `method` and `file` only (the table pins `method, file`; `function` and `module` are NOT canonical scope_kind values per Sec 5.2.3 -- the canonical enum is `repo | package | file | class | interface | method | block`).
- [ ] Implement `recipes/cognitive_complexity.go` emitting `metric_kind='cognitive_complexity'` at canonical scope_kinds `method` and `file` only (architecture Sec 1.4.1 row 2 pins `method, file`).
- [ ] Implement `recipes/loc.go` emitting `metric_kind='loc'` at canonical scope_kind `file` per Compute call (architecture Sec 1.4.1 row 3 pins applicability `{file, package, repo}`; `function|method|class|module` are NEVER legal). The per-file `Recipe.Compute(*AstFile)` interface authoritatively emits at `file` only; `package` and `repo` rows are produced downstream by the Stage 2.6 materialiser (Sec 5.2.5) and the Cross-Repo Aggregator (Sec 3.10 step 4) which SUM file rows -- emitting per-file partial-fact drafts at `package`/`repo` from this recipe would conflict on the writer's `(repo_id, sha, scope_id, metric_kind, metric_version)` uniqueness invariant (Sec 5.2.1 line 905). The recipe's `allowedKinds` slice still pins all three so the panic guard forward-accepts the materialiser/aggregator emission paths.
- [ ] Register the three recipes in `recipes/registry.go` keyed by metric_kind; emit a startup log line listing the registered base-pack recipes.
- [ ] Add table-driven tests `recipes/cyclo_test.go`, `recipes/cognitive_complexity_test.go`, `recipes/loc_test.go` against language-tagged fixtures with known expected values.

### Dependencies
- phase-ast-adapter-and-foundation-tier-compute/stage-scope-identity-derivation-and-scopebinding-writer

### Test Scenarios
- [ ] Scenario: base-recipes-only-canonical-kinds -- Given the recipe registry after init, When listing the registered metric_kinds for `pack='base'`, Then the result is exactly `{cyclo, cognitive_complexity, loc}` (no `cyclomatic_complexity`, `lines_of_code`, `function_length`, `parameter_count`, or `nesting_depth` -- iter 1 evaluator item 3).
- [ ] Scenario: cyclo-known-value -- Given a Go fixture method with two `if` branches and one `for` loop, When the cyclo recipe runs, Then it emits a `MetricSampleDraft(metric_kind='cyclo', value=4)` at canonical `scope_kind='method'` (NEVER the non-canonical `function`).
- [ ] Scenario: loc-counts-physical-lines -- Given a 42-line Python source file fixture, When the loc recipe runs at canonical `scope_kind='file'`, Then it emits `value=42` (the canonical enum has `file`, NOT `module`).

## Stage 2.4: SOLID pack foundation recipes

### Implementation Steps
- [ ] Implement `recipes/lcom4.go` emitting `metric_kind='lcom4'` (architecture Sec 1.4.1 SOLID pack) at scope_kind `class` per the four-component cohesion algorithm.
- [ ] Implement `recipes/fan_in.go` emitting `metric_kind='fan_in'` at canonical scope_kinds `method`, `class`, `file` (architecture Sec 1.4.1 row 5 pins `method, class, file`; NOT `module|package`) counting inbound references.
- [ ] Implement `recipes/fan_out.go` emitting `metric_kind='fan_out'` at canonical scope_kinds `method`, `class`, `file` (architecture Sec 1.4.1 row 6 pins `method, class, file`) counting outbound references.
- [ ] Implement `recipes/depth_of_inheritance.go` emitting `metric_kind='depth_of_inheritance'` at scope_kind `class` (architecture Sec 1.4.1 row 7).
- [ ] Implement `recipes/interface_width.go` emitting `metric_kind='interface_width'` at canonical scope_kinds `class`, `interface` (architecture Sec 1.4.1 row 8) -- public surface size.
- [ ] Implement `recipes/coupling_between_objects.go` emitting `metric_kind='coupling_between_objects'` at scope_kind `class`.
- [ ] Implement `recipes/lsp_violation.go` emitting `metric_kind='lsp_violation'` at scope_kind `method` with `value=1` iff the overriding method strengthens its parent's precondition or weakens its postcondition per the AST-level signature comparison (architecture Sec 1.4.1 row 13, Sec 3.5.1.c lines 514-525). Also record the boolean fact on `MetricSample.attrs_json.lsp_violation` for forensics (mirrors the `cycle_member` dual-encoding pattern). Emits `value=0` for overrides whose contract is compatible so the LSP rule (Stage 5.5 `solid.lsp.override_violation`) sees an explicit non-firing sample rather than a silent absence.
- [ ] Register all seven in `recipes/registry.go` keyed by metric_kind with `pack='solid'`, `source='computed'`; emit a startup log listing them.
- [ ] Add language-tagged fixture tests asserting each recipe emits exactly the canonical `metric_kind` value (no `cyclomatic_complexity` / `lines_of_code` aliases anywhere; iter 1 evaluator item 3).

### Dependencies
- phase-ast-adapter-and-foundation-tier-compute/stage-base-pack-foundation-recipes

### Test Scenarios
- [ ] Scenario: solid-recipes-only-canonical-kinds -- Given the registry after init, When listing `pack='solid'` recipes, Then the metric_kinds are exactly `{lcom4, fan_in, fan_out, depth_of_inheritance, interface_width, coupling_between_objects, lsp_violation}`.
- [ ] Scenario: lcom4-class-known-value -- Given a Java class fixture with two disjoint method clusters sharing no fields, When the lcom4 recipe runs, Then it emits `value=2`.
- [ ] Scenario: cbo-counts-distinct-targets -- Given a class referencing four distinct external classes, When the coupling_between_objects recipe runs, Then it emits `value=4`.
- [ ] Scenario: lsp-violation-strengthens-precondition -- Given a Java fixture where a subclass override adds a `if (x < 0) throw` check the parent does NOT have, When the lsp_violation recipe runs, Then it emits `value=1` for the method scope and stamps `attrs_json.lsp_violation = "true"`.
- [ ] Scenario: lsp-violation-compatible-override -- Given a subclass override whose precondition and postcondition match the parent's, When the lsp_violation recipe runs, Then it emits `value=0` (the explicit non-firing sample the LSP override rule expects).

## Stage 2.5: Cycle and duplication recipes

### Implementation Steps
- [ ] Implement `recipes/cycle_member.go` emitting `metric_kind='cycle_member'` at canonical scope_kinds `file`, `package` (architecture Sec 1.4.1 row 10 pins `file, package`; NOT `module`) with `value=1` when the scope participates in a strongly-connected component of the import graph and `value=0` otherwise; record the cycle id in `MetricSample.attrs_json`.
- [ ] Implement `recipes/duplication_ratio.go` emitting `metric_kind='duplication_ratio'` at canonical scope_kinds `file`, `package` (architecture Sec 1.4.1 row 11 pins `file, package`; NOT `function|method|module`) using a 50-token sliding window comparison.
- [ ] Both recipes register with `pack='base'`, `source='computed'`.
- [ ] Add tests covering: a three-file cycle (all three emit `cycle_member=1`); a file with 80% duplicate token content emits `duplication_ratio=0.8`.

### Dependencies
- phase-ast-adapter-and-foundation-tier-compute/stage-base-pack-foundation-recipes

### Test Scenarios
- [ ] Scenario: cycle-member-flags-participants -- Given three files A->B->C->A forming an import cycle, When the cycle_member recipe runs, Then all three emit `value=1` at scope_kind `file`; a file D outside the cycle emits `value=0`; the recipe NEVER emits at the non-canonical scope_kind `module`.
- [ ] Scenario: duplication-ratio-bounded-zero-to-one -- Given any input scope at canonical scope_kinds `file` or `package`, When the duplication_ratio recipe runs, Then the emitted `value` is in `[0,1]` (boundary check) and the row's `scope_kind` IS in `{file, package}` (NEVER `function` or `module`).

## Stage 2.6: modification_count_in_window materialiser

### Implementation Steps
- [ ] Add `internal/metrics/materialisers/modification_count.go` reading rows fed by `ingest.churn` (Phase 4) and materialising `metric_kind='modification_count_in_window'` with `pack='base'`, **`source='computed'`** per tech-spec Sec 4.1.1 lines 287-291 and Sec 4.11 lines 444-454 (the materialiser is the **computing** writer; `ingested` provenance is recorded on `MetricSample.attrs_json` as a separate annotation per C19, NOT on the `source` enum).
- [ ] Window size comes from tech-spec Sec 8.2 `window_days=90`; expose as configurable.
- [ ] Materialiser runs as part of the Metric Ingestor (same writer-ownership role) inside the same ScanRun as the foundation recipes so the active-row uniqueness invariant holds.
- [ ] Skip emitting a sample when no churn rows exist in the window for a given scope (no zero-fill noise).
- [ ] Add unit test with a synthetic churn stream verifying the count.

### Dependencies
- phase-ast-adapter-and-foundation-tier-compute/stage-base-pack-foundation-recipes

### Test Scenarios
- [ ] Scenario: materialiser-emits-canonical-kind -- Given churn rows for scope `pkg.Foo.bar` in the last 90 days, When the materialiser runs, Then it emits `MetricSample(metric_kind='modification_count_in_window', pack='base', source='computed')` and records `attrs_json.provenance='ingested'`.
- [ ] Scenario: out-of-window-rows-ignored -- Given churn rows older than 90 days, When the materialiser runs, Then no `metric_sample` row is written for that scope.

# Phase 3: Repo Indexer and Metric Ingestor

## Dependencies
- phase-foundation-and-schema
- phase-ast-adapter-and-foundation-tier-compute

## Stage 3.1: Repo Indexer and Commit lifecycle

### Implementation Steps
- [ ] Add `internal/repo_indexer/` service consuming Git webhooks and CLI rescan triggers; on each new SHA, INSERT a `commit` row (default `scan_status='pending'`) and a `repo_event(kind='registered')` if needed (architecture Sec 5.1.4 canonical kind, NOT `register`).
- [ ] Repo Indexer is the ONLY writer of new `commit` rows (architecture G1); it never updates `scan_status` (the Metric Ingestor owns transitions).
- [ ] Define the canonical `Commit.scan_status` transition diagram in code: `pending -> scanning -> scanned` on success, `pending -> scanning -> failed` on error -- no `complete`, no `superseded`, no `orphaned` states (iter 1 evaluator item 2). Add a Go enum `ScanStatus` with only the four canonical values.
- [ ] Add `internal/repo_indexer/handler_test.go` covering: new SHA inserts a `commit` with `scan_status='pending'`; duplicate SHA event is a no-op.

### Dependencies
- phase-foundation-and-schema/stage-postgresql-role-grants-and-schema-isolation

### Test Scenarios
- [ ] Scenario: commit-states-only-canonical -- Given the `ScanStatus` enum at compile time, When `reflect.TypeOf(ScanStatus(0)).String()` enumerates its values, Then exactly `{pending, scanning, scanned, failed}` are present (no `complete`, no `superseded`, no `orphaned`).
- [ ] Scenario: new-sha-inserts-pending -- Given a webhook payload for a new SHA, When Repo Indexer processes it, Then a `commit` row appears with `scan_status='pending'` and a single `repo_event(kind='registered')` is appended (canonical enum value).

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
- [ ] In the Metric Ingestor write path, INSERT each new `metric_sample` row, then UPSERT the active-row pointer via `INSERT INTO metric_sample_active (repo_id, sha, scope_id, metric_kind, metric_version, sample_id) VALUES (...) ON CONFLICT (repo_id, sha, scope_id, metric_kind, metric_version) DO UPDATE SET sample_id=EXCLUDED.sample_id` -- the side relation's PK enforces at-most-one active row per quintuple (tech-spec Sec 7.1.b lines 1070-1119 / Sec 10A pin lines 1659-1675). Re-ingest of the same `(sha, scope, metric_kind, metric_version)` either no-ops at the application layer (idempotent computation) OR appends a new `metric_sample` row and re-points `metric_sample_active.sample_id` (G3 retains the prior row forever in `metric_sample`).
- [ ] On retraction, append `metric_retraction(sample_id)` and **leave the `metric_sample_active` pointer row in place** -- per tech-spec Sec 7.2 line 1248 `DELETE` is REVOKEd on `metric_sample_active` from BOTH writer roles, so the pointer is not removed. Readers (Insights, Evaluator, Refactor Planner) MUST join through `metric_retraction` to filter retracted samples per architecture Sec 5.2.2 lines 1035-1037. On a subsequent rescan at the same SHA, the writer UPSERTs the pointer to the new `sample_id` (the prior retracted row stays in `metric_sample` per G3 as a tombstone). `metric_sample` itself is never UPDATEd per G3 / C2.
- [ ] Do NOT use any procedural `swap_active`, `swap` trigger, or stored function (iter 1 evaluator item 1 -- there is no `swap_active` verb in the canonical model).
- [ ] Add `internal/metric_ingestor/active_row_test.go` covering: re-ingest same SHA twice; with intervening retraction the second write succeeds; both calls leave `metric_sample` append-only.

### Dependencies
- phase-repo-indexer-and-metric-ingestor/stage-metric-ingestor-and-scanrun-state-machine

### Test Scenarios
- [ ] Scenario: re-ingest-without-retract-is-idempotent -- Given a `metric_sample` row already present and pointed-to by `metric_sample_active`, When the Metric Ingestor re-ingests the same SHA, Then `metric_sample_active.sample_id` remains stable and `metric_sample` row count is either unchanged (computation-skip path) or grows by one with the new row becoming the pointer target (G3 preserves the old row).
- [ ] Scenario: re-ingest-after-retract-succeeds -- Given a sample retracted via `metric_retraction(sample_id)` (the pointer row remains in `metric_sample_active` per tech-spec Sec 7.2 line 1248 REVOKE DELETE), When the Metric Ingestor re-ingests, Then a new `metric_sample` row appears with a fresh `sample_id` AND `metric_sample_active` is UPSERTed to point at the new row; the original `metric_sample` row remains in place (G3 / C2) and reader queries join through `metric_retraction` to filter the prior tombstone.

## Stage 3.4: Retraction and rescan flow

### Implementation Steps
- [ ] Implement `mgmt.retract_sample(sample_id, reason, actor)` verb: append a `repo_event(kind='retract_intent', payload={sample_id, reason})` and dispatch to the Metric Ingestor, which opens a `scan_run(kind='retract')` and appends `metric_retraction(retraction_id, sample_id, reason, appended_by, created_at)`. The `metric_sample_active` pointer row is NOT deleted (DELETE is REVOKEd per tech-spec Sec 7.2 line 1248); SHA-pinned readers (`mgmt.read.metric_sample`, `mgmt.read.metric_samples`, `eval.gate`) filter the retracted sample out via a `metric_retraction` join.
- [ ] Implement `mgmt.rescan(repo_id, sha)` verb: emit a service-internal rescan request (no canonical RepoEvent kind exists for rescan per architecture Sec 5.1.4) and enqueue a `scan_run(kind='full')` for the given SHA via the Metric Ingestor.
- [ ] Mgmt surface never writes Measurement rows directly -- it only emits `repo_event` and delegates (architecture Sec 6.3).
- [ ] Add idempotency: `mgmt.retract_sample` for an already-retracted sample is a no-op (returns the existing retraction row).

### Dependencies
- phase-repo-indexer-and-metric-ingestor/stage-active-row-uniqueness-enforcement

### Test Scenarios
- [ ] Scenario: retract-appends-retraction-row -- Given an active sample, When `mgmt.retract_sample` is invoked with a reason, Then a `metric_retraction` row appears, a `scan_run(kind='retract', status='succeeded')` is recorded, the `metric_sample_active` pointer row remains in place (per tech-spec Sec 7.2 line 1248 REVOKE DELETE), and SHA-pinned reader joins through `metric_retraction` correctly filter out the retracted sample.
- [ ] Scenario: rescan-enqueues-scan-run -- Given a `mgmt.rescan(repo_id, sha)` call, When the verb completes, Then a service-internal rescan request is logged and a `scan_run(kind='full', status='running')` is observable (no `rescan_intent` RepoEvent kind is emitted -- the canonical RepoEvent enum at architecture Sec 5.1.4 has no `rescan_intent` value).

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
- [ ] Verify the signature against the per-deployment secret resolved by `signing_key_id` -- v1 is **single-tenant** per tech-spec Sec 4.14 (one logical org per deployment); there is NO `tenant_id` field on any table or in the secret lookup (tech-spec Sec 10A "multi-tenant v2 shape" pin lines 1690-1696 reserves no `tenant_id` column for v1; multi-tenant migration uses per-schema isolation, not row-level columns). Reject invalid signatures with `401`.
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
- [ ] Open a `scan_run(kind='external_single', sha_binding='single', status='running')` for the upload (architecture Sec 6.4 lines 1364-1366 / tech-spec Sec 4.11 lines 429-431 -- coverage uploads carry ONE `sha` per call); on success mark `succeeded`.
- [ ] Bind each row's `scope_id` by looking up the existing `scope_binding` for `(repo_id, sha, scope_kind, canonical_signature)`; if the binding is missing, skip the row and log a `coverage_skipped_unbound_scope` counter (do NOT invent a scope).
- [ ] Add `internal/ingest/coverage/cobertura_test.go` against a real fixture.

### Dependencies
- phase-external-metric-ingest-webhook/stage-webhook-transport-and-hmac-verification

### Test Scenarios
- [ ] Scenario: coverage-emits-only-canonical-kinds -- Given a Cobertura upload, When the verb runs, Then the appended `metric_sample` rows have `metric_kind IN ('coverage_line_ratio','coverage_branch_ratio')` -- no `coverage_line` or `coverage_branch` aliases.
- [ ] Scenario: unbound-scope-skipped -- Given a Cobertura row for a file/function with no matching `scope_binding`, When the parser runs, Then no `metric_sample` row is appended and `coverage_skipped_unbound_scope` increments.

## Stage 4.3: ingest test_balance verb

### Implementation Steps
- [ ] Implement `internal/ingest/test_balance/` parsing a JSON body of `{scope_id, attempt_count, pass_count}` rows (tech-spec Sec 8.5 lines 987-992) and emitting `MetricSample(metric_kind='pass_first_try_ratio', pack='ingested', source='ingested')` per architecture Sec 1.4.2 / tech-spec Sec 4.1.1.
- [ ] Compute the ratio per row as `pass_count / attempt_count` (one MetricSample per `scope_id`); skip rows with `attempt_count=0` (avoid NaN).
- [ ] Open `scan_run(kind='external_single', sha_binding='single')` per architecture Sec 6.4 lines 1367-1368 / tech-spec Sec 4.11 lines 429-432 -- test_balance uploads carry ONE `sha` per call.
- [ ] Explicitly do NOT emit test-count metrics, test-duration metrics, or any other `test_balance.*` metric kinds (iter 1 evaluator item 4 removed those). The Cross-Repo Aggregator promotes `pass_first_try_ratio` to system-tier `xservice_test_reliability` per architecture Sec 3.10 step 4 / Sec 1.4.2 row 6.
- [ ] Add test fixture covering a JSON payload with mixed pass/fail/attempt outcomes; refuse to parse JUnit-XML bodies (the verb's body media-type is `application/json` per tech-spec Sec 8.5).

### Dependencies
- phase-external-metric-ingest-webhook/stage-webhook-transport-and-hmac-verification

### Test Scenarios
- [ ] Scenario: test-balance-emits-only-pass-first-try-ratio -- Given a JSON `{scope_id, attempt_count, pass_count}` upload, When the verb runs, Then the appended `metric_sample` rows have exactly one `metric_kind='pass_first_try_ratio'` per `scope_id` (no test-count or duration kinds).
- [ ] Scenario: test-balance-rejects-junit-xml -- Given a JUnit-XML POST body to `/v1/ingest/test_balance`, When the handler runs, Then it returns `415 Unsupported Media Type` (the verb is pinned to JSON per tech-spec Sec 8.5).
- [ ] Scenario: ratio-clamped-zero-to-one -- Given a JSON upload, When the verb computes the ratio, Then the emitted value is in `[0,1]` and `attempt_count=0` results in NO row written for that scope (avoid NaN).

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
- [ ] Scenario: churn-writes-no-metric-sample -- Given a churn upload, When the verb returns, Then `SELECT COUNT(*) FROM metric_sample WHERE producer_run_id=$1` returns 0; only `churn_event` rows are appended.
- [ ] Scenario: materialiser-consumes-churn -- Given churn rows appended for a scope, When the modification_count materialiser runs next, Then it emits the canonical `metric_kind='modification_count_in_window'` sample with `pack='base'`, `source='computed'`.

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
- [ ] Implement `policy.publish(rulepack_set, refactor_weights)` verb appending an immutable `policy_version(policy_version_id, name, rule_refs, threshold_refs, refactor_weights, signature, created_at)` row per architecture Sec 5.3.3 -- never UPDATE.
- [ ] Implement `policy.activate(policy_version_id)` verb appending a `policy_activation(activation_id, policy_version_id, activated_by, created_at)` row per architecture Sec 5.3.4. No `scope` parameter -- activation is global per deployment (v1 single-tenant).
- [ ] Implement `policy.publish_rulepack(rulepack)` verb (tech-spec Sec 8.5 lines 963-970 canonical verb name) writing one or more `rule_pack` and `rule` rows; append-only. There is NO `policy.rulepack.add` and NO `policy.rulepack.remove` verb in the canonical surface (tech-spec Sec 8.5: "There is no `policy.override` verb"; the same constraint applies to rulepack lifecycle -- the only write verb is `policy.publish_rulepack`).
- [ ] All three verbs require a valid signing key (Stage 5.1); refuse unsigned payloads.
- [ ] Add `internal/policy/steward_test.go` covering publish -> activate -> evaluator picks up the new version, and `policy.publish_rulepack` writes the expected `rule`/`rule_pack` rows.

### Dependencies
- phase-policy-steward-and-solid-rule-engine/stage-policy-steward-signing-key-store

### Test Scenarios
- [ ] Scenario: policy-version-immutable -- Given an existing `policy_version` row, When any UPDATE statement targets it, Then PostgreSQL returns permission denied (no role has UPDATE) AND the Steward verb path has no UPDATE call.
- [ ] Scenario: activation-latest-row-wins -- Given an active policy_version, When `policy.activate(new_id)` runs, Then a new `policy_activation` row appears (latest by `created_at` defines active) -- no `scope` parameter accepted, no `deactivated_at` flag set on the prior row.
- [ ] Scenario: canonical-rulepack-verb-name -- Given the gRPC surface, When listing the policy.* verbs, Then exactly `{policy.publish, policy.activate, policy.publish_rulepack}` are registered (tech-spec Sec 8.5 lines 963-970); calls to `policy.rulepack.add` or `policy.rulepack.remove` return `UNIMPLEMENTED`.

## Stage 5.3: Override append only mute lifecycle

### Implementation Steps
- [ ] Implement `mgmt.override(scope_filter, rule_id, mute BOOL, reason)` verb delegating to the Policy Steward which appends an `override(override_id, rule_id, scope_filter JSONB, mute, reason, actor_id, created_at)` row per architecture Sec 5.3.6 lines 1160-1170 (tech-spec Sec 10A pin). `scope_filter` is JSON `{repo_id, scope_kind, scope_signature_glob}`; `actor_id` is the OIDC subject of the caller. NO `policy_version_id` column (overrides bind to rules, not policy versions). NO `created_by` column (the actor column is named `actor_id`).
- [ ] Latest-row-wins read semantics: the evaluator reads `MAX(created_at) WHERE rule_id=$1 AND scope_filter matches the candidate scope` to determine the active mute state.
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
- [ ] Add `policy/rulepacks/solid/ocp.yaml`: OCP rule consuming `metric_kind IN ('fan_in','modification_count_in_window')` per architecture Sec 3.5.1.b lines 504-513 and tech-spec Sec 4.3 lines 341-351 (the canonical OCP smell: a class that is widely depended-on AND frequently edited). NOT `depth_of_inheritance`. The Stage 2.6 materialiser dependency is here in the RULE definition, NOT in the Stage 2.4 recipe stage (iter 1 evaluator item 10).
- [ ] Add `policy/rulepacks/solid/lsp.yaml`: LSP rule consuming `metric_kind='depth_of_inheritance'` plus override-method-signature checks emitted by Stage 2.4.
- [ ] Add `policy/rulepacks/solid/isp.yaml`: ISP rule consuming `metric_kind='interface_width'`.
- [ ] Add `policy/rulepacks/solid/dip.yaml`: DIP rule consuming `metric_kind IN ('fan_out','coupling_between_objects')`.
- [ ] Each rulepack is signed and ingested via `policy.publish_rulepack` at startup if absent (tech-spec Sec 8.5 lines 963-970 canonical verb name).
- [ ] Add `policy/rulepacks/solid/solid_test.go` loading each pack and asserting it references only canonical metric_kinds.

### Dependencies
- phase-policy-steward-and-solid-rule-engine/stage-predicate-dsl-evaluator
- phase-ast-adapter-and-foundation-tier-compute/stage-solid-pack-foundation-recipes
- phase-ast-adapter-and-foundation-tier-compute/stage-modification-count-in-window-materialiser

### Test Scenarios
- [ ] Scenario: solid-rulepacks-load -- Given the five SOLID rulepack files, When the Policy Steward loads them, Then all five appear as `rule_pack` rows with `pack='solid'` and the contained predicates parse cleanly.
- [ ] Scenario: solid-rulepacks-only-canonical-kinds -- Given the loaded predicates, When scanning their metric_kind references, Then each is one of `{lcom4, fan_in, fan_out, depth_of_inheritance, interface_width, coupling_between_objects, modification_count_in_window, lsp_violation}` and no non-canonical alias appears.
- [ ] Scenario: ocp-uses-fan-in -- Given the loaded OCP rulepack, When inspecting its inputs, Then they are exactly `{fan_in, modification_count_in_window}` (NOT `depth_of_inheritance`; canon-guard against iter 3 drift item 14).

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

## Stage 5.7: SOLID Rule Engine batch worker and synchronous mode

### Implementation Steps
- [ ] Add `internal/rule_engine/` worker consuming the latest active `policy_version` (Stage 5.2) and evaluating each rule's predicate over `metric_sample` rows for newly-scanned SHAs.
- [ ] Expose two callable modes per architecture Sec 3.6 lines 526-540 and Sec 4.2 lines 760-762: (a) batch refresh mode invoked by the post-scan dispatcher (`caller='batch_refresh'`) and (b) synchronous mode `rule_engine.RunSync(ctx, repo_id, sha, scope?, policy_version_id) -> (evaluation_run_id, evaluation_verdict_id, []finding_id)` invoked by `eval.gate` in the same call. **Both modes write the SAME row set in the SAME transaction: ONE `evaluation_run` + ONE `evaluation_verdict` + N `finding` rows.** The engine -- not eval.gate -- is the canonical writer of `evaluation_verdict` whenever the synchronous rule pass is invoked; eval.gate writes its own run+verdict pair (with zero findings) only on the two short-circuit degraded paths where the engine is NOT invoked (signature-invalid, samples_pending), preserving the canonical Audit schema (non-null `evaluation_run_id` FK, `created_at` timestamp). Per Stage 1.5 the three Audit tables are granted INSERT to `clean_code_evaluator`, `clean_code_solid_batch`, AND `clean_code_wal_reconciler` in parallel.
- [ ] On a positive match, append a canonical `finding(finding_id, evaluation_run_id, repo_id, sha, scope_id, rule_id, rule_version, policy_version_id, metric_sample_ids JSONB, severity ENUM('info'|'warn'|'block'), delta, explanation_md, created_at)` row per architecture Sec 5.4.1 lines 1174-1190. **Writer-ownership (G1 / Phase 1.5 grants per tech-spec Sec 7.2 lines 1256-1261):** the three Audit tables (`evaluation_run`, `evaluation_verdict`, `finding`) are granted INSERT in parallel to `clean_code_solid_batch` (this Rule Engine batch worker, `caller='batch_refresh'`), `clean_code_evaluator` (the gate-degraded short-circuit paths), and `clean_code_wal_reconciler` (replay only). The Rule Engine is the WRITER on the rule-pass paths (both synchronous and batch); the Evaluator and Reconciler are co-grantees for their narrow paths. `metric_sample_ids` references the exact MetricSample row(s) that triggered the rule (NOT a single `metric_kind`+`value` pair).
- [ ] Compute `Finding.delta` against the prior SHA's findings for the same `(scope_id, rule_id, policy_version_id)`: canonical values `new | newly_failing | unchanged | resolved` per architecture Sec 5.4.1 lines 1186-1190. Semantics: `new`=rule fires at this scope for the FIRST SHA ever; `newly_failing`=previously not severity=block at this scope, now is (regressions-to-block bucket); `unchanged`=was severity=block at previous SHA and still is; `resolved`=was severity=block at previous SHA and is now absent or at lower severity. NEVER emit `regression`, `improvement`, or `flat`.
- [ ] Apply Override mute state from Stage 5.3 BEFORE appending the Finding (muted findings are not written; Insights surface tracks mute hits separately).
- [ ] Add `internal/rule_engine/worker_test.go` covering: finding emitted with canonical schema columns; muted scope produces no finding; delta=newly_failing when severity moves from non-block to block at the next SHA; delta=resolved when prior severity=block is no longer present.
- [ ] Add `internal/rule_engine/synchronous_test.go` covering: `RunSync` writes one `evaluation_run(caller='eval_gate')` + one `evaluation_verdict` + N `finding` rows and returns all three IDs; the verdict severity-rollup matches the highest unmuted finding severity (`block` > `warn` > `pass`); concurrent `RunSync` calls for the same `(repo_id, sha)` are serialised by an advisory lock so two parallel `eval.gate` calls produce a single canonical run+verdict.

### Dependencies
- phase-policy-steward-and-solid-rule-engine/stage-override-append-only-mute-lifecycle
- phase-policy-steward-and-solid-rule-engine/stage-solid-rule-packs
- phase-policy-steward-and-solid-rule-engine/stage-decoupled-functional-areas-rule-pack
- phase-repo-indexer-and-metric-ingestor/stage-metric-ingestor-and-scanrun-state-machine

### Test Scenarios
- [ ] Scenario: finding-emitted-on-rule-hit -- Given a SHA with a `metric_sample(metric_kind='lcom4', value=12)` exceeding the SRP threshold (10), When the rule engine runs, Then a `finding(rule_id='solid.srp', severity='warn', delta='new')` row appears with `policy_version_id` pinned and `metric_sample_ids` JSONB referencing the triggering sample.
- [ ] Scenario: muted-scope-skipped -- Given an `override(scope, rule_id, mute=true)` latest row, When the rule engine evaluates that scope+rule, Then no `finding` row is appended.
- [ ] Scenario: delta-newly-failing -- Given the same `(scope_id, rule_id)` evaluated at SHA A with severity=warn and at SHA B with severity=block, When the worker writes the SHA B finding, Then `delta='newly_failing'`.
- [ ] Scenario: sync-mode-writes-run-verdict-and-findings -- Given `RuleEngine.RunSync` called with valid inputs, When it returns, Then exactly one `evaluation_run(caller='eval_gate')`, one `evaluation_verdict` referencing that run, and at least one `finding` row referencing that run all exist (commit in the same transaction); the verdict's `verdict` column matches the severity rollup of the unmuted findings.

# Phase 6: Evaluator Surface and Management Surface

## Dependencies
- phase-repo-indexer-and-metric-ingestor
- phase-policy-steward-and-solid-rule-engine

## Stage 6.1: Evaluator gate verb and synchronous SOLID delegation

### Implementation Steps
- [ ] Implement `eval.gate(repo_id, sha, scope?)` verb per architecture Sec 3.7 lines 548-570 and Sec 4.2 lines 755-764. Sequence: (1) resolve active `policy_version_id` via latest `policy_activation` row; (2) verify `policy_version.signature` against the Steward signing key -- on mismatch take the **policy-signature-invalid degraded short-circuit**: insert ONE canonical `evaluation_run(evaluation_run_id, repo_id, sha, policy_version_id, caller='eval_gate', created_at)` row followed by ONE `evaluation_verdict(verdict_id, evaluation_run_id=<the run just inserted>, verdict='warn', degraded=true, degraded_reason='policy_signature_invalid', created_at)` row in the same transaction (per architecture Sec 5.4.2 lines 1192-1201 and Sec 5.4.3 lines 1203-1212 -- `evaluation_run_id` is a non-null FK and the timestamp column is `created_at`, NOT `settled_at`; there is no `scope` field on `EvaluationVerdict`); write zero `finding` rows and return without invoking the Rule Engine (per architecture Sec 1.6 `policy-signing-required` pin, the gate never blocks but still records the audit trail); (3) check Phase 2 completion for the SHA -- if any required input is `samples_pending` take the **samples-pending degraded short-circuit**: insert ONE `evaluation_run(...caller='eval_gate'...)` row followed by ONE `evaluation_verdict(verdict_id, evaluation_run_id=<that run>, verdict='warn', degraded=true, degraded_reason='samples_pending', created_at)` row, zero findings, no Rule Engine invocation; (4) on the clean path, DELEGATE to the SOLID Rule Engine **synchronous mode** (Stage 5.7) `rule_engine.RunSync(ctx, repo_id, sha, scope?, policy_version_id) -> (evaluation_run_id, evaluation_verdict_id, []finding_id)` -- per architecture Sec 3.6 lines 526-540 and Sec 4.2 lines 760-762 the engine writes **ONE `evaluation_run` + ONE `evaluation_verdict` + N `finding` rows in the same transaction** and returns their IDs; (5) eval.gate **does NOT** write its own `evaluation_verdict` row on the clean path -- the verdict that the engine wrote (severity-rollup: `block` if any unmuted finding has `severity='block'`; `warn` if any has `severity='warn'`; `pass` otherwise) is the canonical one and eval.gate simply reads it back to shape the response. Across all three paths the canonical Audit schema is preserved: every `evaluation_verdict` row carries a valid `evaluation_run_id` FK (NEVER NULL), uses `created_at` (NEVER `settled_at`), and has no `scope` column.
- [ ] Verdict is the canonical enum `pass | warn | block` (architecture Sec 5.4.2 / tech-spec Sec 4.8) -- NOT `pass|fail|gated` (iter 1 evaluator item 6). Implement as a Go enum `Verdict { Pass, Warn, Block }` with no other values.
- [ ] eval.gate does NOT compute findings itself and does NOT consume pre-existing findings without invoking the synchronous Rule Engine path (iter 3 evaluator item 16); the rule pass and the verdict are produced in one synchronous call so degraded warnings cannot mask a stale finding set.
- [ ] Add a SEPARATE `degraded BOOL` and `degraded_reason` field to the verdict; degraded conditions map to `verdict='warn'` per the operator pin `gate-degraded-policy=warn` (architecture Sec 1.6).
- [ ] `degraded_reason` is constrained to `samples_pending | policy_signature_invalid | xrepo_edges_unavailable` for eval.gate -- `percentile_stale` is NOT a valid eval.gate reason (it lives on the Insights surface per Stage 7.3; iter 1 evaluator item 8).
- [ ] Add `internal/evaluator/gate_test.go` covering: clean pass path -- exactly one `evaluation_run` + one `evaluation_verdict` + N `finding` rows appear, all written by `RuleEngine.RunSync` in the same transaction with `caller='eval_gate'`; block on severity=block finding via the rollup; warn on degraded (policy signature invalid) -- NO Rule Engine call, zero `finding` rows, ONE `evaluation_run(caller='eval_gate')` row, ONE `evaluation_verdict(evaluation_run_id=<that run>, verdict='warn', degraded=true, degraded_reason='policy_signature_invalid', created_at)` row -- both written by the Evaluator directly in the same transaction; warn on degraded (samples_pending) -- same Evaluator-written run+verdict pair, zero findings; degraded_reason rejects `percentile_stale`.

### Dependencies
- phase-policy-steward-and-solid-rule-engine/stage-solid-rule-engine-batch-worker-and-synchronous-mode

### Test Scenarios
- [ ] Scenario: verdict-enum-only-canonical -- Given the `Verdict` Go enum at compile time, When iterating its values, Then they are exactly `{pass, warn, block}` (no `fail`, no `gated`) -- guards iter 1 evaluator item 6.
- [ ] Scenario: gate-delegates-synchronous-rule-pass -- Given a clean SHA with samples present and a valid policy signature, When `eval.gate` runs, Then exactly one new `evaluation_run(caller='eval_gate')`, one new `evaluation_verdict` referencing that run, and N new `finding` rows are observable (all three written by `RuleEngine.RunSync` in the same transaction per architecture Sec 4.2 lines 760-762, NOT by a separate batch worker and NOT by eval.gate appending a verdict afterward); the verdict's `verdict` column matches the severity rollup of the freshly-written findings.
- [ ] Scenario: degraded-maps-to-warn -- Given a samples_pending degraded condition (no findings written), When `eval.gate` runs, Then the Rule Engine is NOT invoked, zero new `finding` rows are appended, ONE new `evaluation_run(caller='eval_gate', repo_id, sha, policy_version_id)` row is written by the Evaluator, and ONE new `evaluation_verdict(evaluation_run_id=<that run>, verdict='warn', degraded=true, degraded_reason='samples_pending', created_at)` row referencing the run via a non-null FK is written in the same transaction (via the `clean_code_evaluator` grant from Stage 1.5; canonical schema per architecture Sec 5.4.2-5.4.3 lines 1192-1212).
- [ ] Scenario: gate-does-not-double-write-verdict -- Given the clean-pass path where the Rule Engine wrote one `evaluation_run` + one `evaluation_verdict` + N findings, When eval.gate returns, Then exactly ONE `evaluation_verdict` row exists for that run (eval.gate did NOT append a second one); on the signature-invalid / samples_pending paths the Rule Engine was not invoked and the single run+verdict pair was written by eval.gate itself with the canonical schema (non-null `evaluation_run_id` FK, `created_at` timestamp, no `scope` column, no `settled_at` column).
- [ ] Scenario: percentile-stale-not-on-gate -- Given any code path inside `eval.gate`, When the degraded_reason validator runs, Then `percentile_stale` is rejected as an invalid eval.gate reason.

## Stage 6.2: Management write verbs and repo onboarding

### Implementation Steps
- [ ] Implement `mgmt.register_repo(repo_url, default_branch, modes)` writing a `repo` row plus a `repo_event(kind='registered')`.
- [ ] Implement `mgmt.set_mode(repo_id, mode)` writing a `repo_event(kind='mode_changed', payload={mode})` and updating `repo.mode`; allowed modes are `embedded | linked` per architecture Sec 1.6 `ast-mode-default` and the canonical RepoEvent enum at Sec 5.1.4 lines 877-884.
- [ ] Re-export `mgmt.retract_sample` (Stage 3.4), `mgmt.rescan` (Stage 3.4), `mgmt.override` (Stage 5.3) under the Management Surface namespace.
- [ ] Each write verb requires an authenticated caller (OIDC subject) recorded as `actor` on the resulting RepoEvent / Override / Finding row.
- [ ] Add `internal/management/verbs_test.go` covering each write verb's happy path and unauthenticated rejection.

### Dependencies
- phase-evaluator-surface-and-management-surface/stage-evaluator-gate-verb-and-synchronous-solid-delegation
- phase-repo-indexer-and-metric-ingestor/stage-retraction-and-rescan-flow
- phase-policy-steward-and-solid-rule-engine/stage-override-append-only-mute-lifecycle

### Test Scenarios
- [ ] Scenario: register-repo-idempotent -- Given a repo already registered, When `mgmt.register_repo` is called with the same URL, Then the existing repo_id is returned and no duplicate `repo` row appears.
- [ ] Scenario: set-mode-emits-event -- Given a repo at mode `embedded`, When `mgmt.set_mode(repo_id, 'linked')` runs, Then a `repo_event(kind='mode_changed')` is appended (canonical past-tense enum value per architecture Sec 5.1.4) and subsequent `mgmt.read.repo` returns mode=`linked`.

## Stage 6.3: Management read verbs and insights projections

### Implementation Steps
- [ ] Implement SHA-pinned read verbs `mgmt.read.repo(repo_id)`, `mgmt.read.metric_sample(repo_id, sha, scope_id, metric_kind)`, `mgmt.read.metric_samples(repo_id, sha, filter)`, `mgmt.read.findings(repo_id, sha)`, `mgmt.read.regressions(repo_id, sha)`, `mgmt.read.refactor_plan(repo_id, sha)` reading the active-row view via the Phase 1.3 `metric_sample_active` side relation join per tech-spec Sec 7.1.b.
- [ ] Implement latest-dashboard read verbs `mgmt.read.cross_repo(metric_kind, scope_kind)`, `mgmt.read.portfolio(metric_kind)` reading `cross_repo_percentile` and `portfolio_snapshot` populated by Phase 7.
- [ ] Both modes route through a single `internal/management/reader.go` with explicit `mode = sha_pinned | latest_dashboard` per architecture Sec 6.3.
- [ ] Read paths never trigger writes (read-only roles enforced by Phase 1.5).
- [ ] Add `internal/management/reader_test.go` covering both modes.

### Dependencies
- phase-evaluator-surface-and-management-surface/stage-evaluator-gate-verb-and-synchronous-solid-delegation

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
- [ ] On each tick, read active `metric_sample` rows for all repos (via the `metric_sample_active` side-relation join), group by `metric_kind` + `scope_kind`, and write rows into `repo_metric_snapshot` (per-repo p50/p90/p99 columns) and `cross_repo_percentile` (cross-repo p50/p90/p99 + `histogram_json` columns) and `portfolio_snapshot` (`aggregate_json` JSONB carrying the cross-repo weighted aggregates per architecture Sec 5.2.6).
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
- [ ] When the aggregator runs in embedded mode and cross-repo edges are unavailable, **write the system-tier row anyway** with `degraded=true, degraded_reason='xrepo_edges_unavailable'` on the `metric_sample` row per architecture Sec 3.10 step 4 lines 637-657 and tech-spec Sec 4.2 lines 325-339 (the canonical fail-safe contract: NEVER silently drop a system-tier row; instead emit a degraded row carrying the reason so downstream eval.gate and Insights surface the freshness state). The `degraded_reason` propagates to the consumer via the matching column on `evaluation_verdict` / Insights envelope.
- [ ] When upstream foundation/SOLID `metric_sample` rows are missing for a system-tier composition, **write the system-tier row anyway** with `degraded=true, degraded_reason='samples_pending'` (same fail-safe contract per architecture Sec 3.10 / tech-spec Sec 4.2). The `value` column carries a best-effort partial computation or `NULL` (NULL is allowed in the `metric_sample.value` column when `degraded=true`); the `samples_pending` counter still increments for observability.
- [ ] Add `internal/aggregator/system_tier_test.go` enumerating the registered system-tier kinds and asserting the set is exactly the seven canonical names.

### Dependencies
- phase-cross-repo-aggregator/stage-aggregator-cadence-loop-and-snapshot-writers

### Test Scenarios
- [ ] Scenario: system-tier-only-canonical-kinds -- Given the system_tier composer at runtime, When listing the metric_kinds it will write, Then the set is exactly `{xrepo_dep_depth, arch_debt_ratio, velocity_trend, arch_fitness, blast_radius, xservice_test_reliability, knowledge_index}` -- no `p50.system`, `p90.system`, `p95.system`, `p99.system` (iter 1 evaluator item 7).
- [ ] Scenario: embedded-mode-writes-degraded-row -- Given the aggregator in embedded mode with no xrepo edges, When it composes `arch_debt_ratio` for an affected scope, Then a `metric_sample(metric_kind='arch_debt_ratio', pack='system', degraded=true, degraded_reason='xrepo_edges_unavailable')` row IS written (NOT skipped, per architecture Sec 3.10 fail-safe / iter 3 evaluator item 15) and the degraded counter labelled `reason=xrepo_edges_unavailable` increments.
- [ ] Scenario: samples-pending-writes-degraded-row -- Given missing foundation `cyclo` samples for a scope at SHA X, When the aggregator composes `velocity_trend` at SHA X, Then a `metric_sample(metric_kind='velocity_trend', pack='system', degraded=true, degraded_reason='samples_pending')` row IS written (NOT skipped); `value` may be NULL.

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
- [ ] Input metric_kinds: complexity from `cyclo`+`cognitive_complexity`, churn from `modification_count_in_window`, coupling from `coupling_between_objects`+`fan_out`, finding_count from `finding` rows where `delta IN ('newly_failing','new')` per architecture Sec 5.4.1 lines 1186-1190 canonical delta enum.
- [ ] Write `hot_spot(hotspot_id, repo_id, sha, scope_id, score, policy_version_id, created_at)` rows per architecture Sec 5.5.1 lines 1216-1224 canonical schema; per-input z-scores (`complexity_z`, `churn_z`, `coupling_z`) and `finding_count` are intermediate computations recorded as JSONB in `hot_spot.score_breakdown` if needed for explainability but are NOT canonical top-level columns on the row.
- [ ] Add `internal/refactor/hotspot_test.go` covering a synthetic input that produces a known score.

### Dependencies
- phase-cross-repo-aggregator/stage-system-tier-metric-composer
- phase-policy-steward-and-solid-rule-engine/stage-solid-rule-engine-batch-worker-and-synchronous-mode

### Test Scenarios
- [ ] Scenario: hotspot-score-formula -- Given known z-scores `(1.0, 2.0, 0.5)` and `finding_count=3` with weights `(1,1,1,1)`, When `score` is computed, Then it equals `1.0 + 2.0 + 0.5 + 3 = 6.5` (round to 2dp).
- [ ] Scenario: hotspot-pins-policy-version -- Given the active `policy_version_id='pv-42'`, When a `hot_spot` row is written, Then `hot_spot.policy_version_id='pv-42'` is recorded.

## Stage 8.2: Refactor plan and task generation

### Implementation Steps
- [ ] Add `internal/refactor/planner.go` reading the top-N hotspots per repo (N from `policy_version.refactor_weights.top_n`) and writing `refactor_plan(plan_id, repo_id, sha, hotspot_ids JSONB, summary_md, created_at)` per architecture Sec 5.5.2 lines 1226-1237 canonical schema (NO `policy_version_id` column on `refactor_plan` -- the policy is recoverable through the referenced HotSpots).
- [ ] For each hotspot, emit one or more `refactor_task(task_id, plan_id, scope_id, kind, effort_hours DOUBLE, rule_id, description_md, created_at)` rows per architecture Sec 5.5.3 lines 1239-1250 canonical schema.
- [ ] `task.kind` is the canonical enum `split_class | extract_method | invert_dependency | break_cycle | consolidate_duplication` per architecture Sec 5.5.3 lines 1244-1248; refuse to write unknown kinds (the iter 3 alias set `extract_function | introduce_interface | reduce_inheritance | reduce_coupling | reduce_lcom | reduce_duplication` is REJECTED).
- [ ] `refactor_task` has NO `status` column in v1 -- task lifecycle is tracked in the agent-memory `Node` graph, not on the immutable refactor row (architecture Sec 5.5.3 line 1239-1250 lists no status field). NO `expected_metric_delta` column.
- [ ] No `effort_estimate` table (iter 1 evaluator item 1) -- the estimate lives as a `effort_hours DOUBLE` column on the `refactor_task` row itself.
- [ ] Add `internal/refactor/planner_test.go` covering happy path and rule-kind mapping.

### Dependencies
- phase-refactor-planner/stage-composite-hotspot-scoring

### Test Scenarios
- [ ] Scenario: plan-generates-canonical-task-kinds -- Given a hotspot flagged by `solid.srp`, When the planner generates tasks, Then a `refactor_task` row appears with `kind='split_class'` (canonical enum per architecture Sec 5.5.3) and `rule_id='solid.srp'`; attempts to write `kind='reduce_lcom'` or `kind='introduce_interface'` are rejected at the CHECK constraint.
- [ ] Scenario: no-effort-estimate-table -- Given the schema after Phase 1 migrations, When the planner persists effort, Then it writes `refactor_task.effort_hours` and no other table named `effort_estimate` exists (guards iter 1 evaluator item 1).
- [ ] Scenario: refactor-task-has-no-status-column -- Given the canonical `refactor_task` table, When `information_schema.columns` is queried, Then no `status` and no `expected_metric_delta` column exists (canon-guard against iter 3 drift item 7).

## Stage 8.3: ML effort model loader and version pinning

### Implementation Steps
- [ ] Add `internal/refactor/effort_model.go` loading an external ML model artefact named by the operator pin `refactor-effort-source` (architecture Sec 1.6 default `ML model from historical commits`).
- [ ] Pin the model version inside `policy_version.refactor_weights.effort_model_version`; each generated `refactor_task` inherits the version transitively **through the `HotSpot` rows referenced by `refactor_plan.hotspot_ids`** -- per architecture Sec 5.5.1 lines 1216-1226 each `hot_spot` row carries `policy_version_id`, and per Sec 5.5.2 lines 1228-1237 `refactor_plan` itself has NO `policy_version_id` column. Reproducing an effort estimate therefore goes: `refactor_task -> refactor_plan -> hot_spot.policy_version_id -> policy_version.refactor_weights.effort_model_version`. No `effort_model_version` column is duplicated on `refactor_task` or `refactor_plan`.
- [ ] If the model artefact URI is not configured AND the pin requires a model, refuse service startup with a clear error.
- [ ] Add `internal/refactor/effort_model_test.go` covering: model present produces deterministic estimate; model absent + pin required blocks startup.

### Dependencies
- phase-refactor-planner/stage-refactor-plan-and-task-generation

### Test Scenarios
- [ ] Scenario: missing-model-blocks-startup -- Given `refactor-effort-source=ML model from historical commits` and no model URI configured, When the planner initialises, Then startup exits non-zero with an error naming the missing config.
- [ ] Scenario: effort-model-version-pinned-via-hotspot -- Given a generated `refactor_task` and the `refactor_plan` that owns it, When traversing `refactor_plan.hotspot_ids[0] -> hot_spot.policy_version_id -> policy_version.refactor_weights.effort_model_version`, Then the value matches the loaded model artefact version (architecture Sec 5.5.1-5.5.2; `refactor_plan` has no `policy_version_id` column).

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
- phase-evaluator-surface-and-management-surface/stage-evaluator-gate-verb-and-synchronous-solid-delegation

### Test Scenarios
- [ ] Scenario: wal-scope-only-audit-tables -- Given any code path in the service, When grepping the writer call sites, Then `internal/audit/wal` is referenced ONLY from `internal/evaluator/` and `internal/rule_engine/` (not from catalog/measurement/policy/refactor) -- iter 1 evaluator item 11.
- [ ] Scenario: no-projection-table -- Given the schema, When `\dt clean_code.*` runs, Then no tables named `audit_event` or `audit_anchor` exist; only `evaluation_run`, `evaluation_verdict`, `finding` carry audit semantics.

## Stage 9.2: Audit WAL Reconciler replay only

### Implementation Steps
- [ ] Add `internal/audit/reconciler/` worker that on service restart reads `data/wal/audit/` frames, verifies signatures, and replays MISSING rows back into the Audit tables.
- [ ] Reconciler is REPLAY-ONLY: it never inserts a row whose `(table, row_pk)` already exists; it never deletes a row; it never modifies a non-Audit table (iter 1 evaluator item 11).
- [ ] Preserve `evaluation_run.caller` as recorded in the original frame -- the reconciler does NOT substitute itself as the caller.
- [ ] Reconciler writes via the `clean_code_wal_reconciler` role (Phase 1.5) which has `INSERT, SELECT` on the three Audit tables per tech-spec Sec 7.2 lines 1258-1261 and `REVOKE UPDATE, DELETE` on the same.
- [ ] Add `internal/audit/reconciler/replay_test.go` covering: missing row gets re-inserted; existing row is left untouched; caller is preserved.

### Dependencies
- phase-audit-wal-and-reliability-hardening/stage-audit-wal-frame-writer

### Test Scenarios
- [ ] Scenario: reconciler-replays-missing-rows -- Given a WAL frame for a row missing from `finding`, When the reconciler runs, Then the row is INSERTed and reads return it.
- [ ] Scenario: reconciler-preserves-caller -- Given a WAL frame for `evaluation_run` with `caller='ci-bot'`, When the reconciler replays after restart, Then the replayed row's `caller` column equals `'ci-bot'` (not `'reconciler'`).
- [ ] Scenario: reconciler-cannot-write-non-audit -- Given the `clean_code_wal_reconciler` role bound to a session, When the reconciler attempts INSERT into `metric_sample` (or any other non-Audit table), Then PostgreSQL returns permission denied; INSERT into the three Audit tables (`evaluation_run`, `evaluation_verdict`, `finding`) succeeds per tech-spec Sec 7.2 lines 1258-1260.

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

### Realized file surface (this stage ONLY; do not conflate with sibling Stages 10.2/10.3/10.4/10.5)

The branch `ws/code-intelligence-CLEAN-CODE/phase-linked-mode-integration-and-rollout-stage-optional-agent-memory-linked-mode-adapter` MUST land these files (and ONLY these) -- the workstream's ground truth file surface is therefore exactly **16 paths** (4 added + 12 modified, 0 deleted), NOT the full Phase-10 union of ~40 paths. Reviewers comparing `git diff origin/feature/clean-code...HEAD` must score against THIS list, which is kept in sync verbatim with the operator-pinned 16-path manifest below (see "Authoritative manifest decision (operator-pinned, iter-14)"):

Production source (services/clean-code):

- `services/clean-code/internal/linked/client.go` -- HTTP adapter + `AggregatorAdapter` + `ResolveLinkedEdges` (the gating entrypoint).
- `services/clean-code/internal/linked/doc.go` -- package contract.
- `services/clean-code/internal/linked/client_test.go` -- coverage required by the implementation step above.
- `services/clean-code/internal/aggregator/aggregator.go` -- `LinkedEdgeReader` seam, `applyLinkedEdges` three-class error classifier (context-abort, mode-store-abort, remote-degrade), `ErrLinkedModeStore` sentinel.
- `services/clean-code/internal/aggregator/types.go` -- `LinkedEdgeReader` interface declaration + supporting `LinkedEdgeFetchFailures` counter field on the aggregator type (consumed by `applyLinkedEdges` in `aggregator.go`).
- `services/clean-code/internal/aggregator/linked_reader_wiring_test.go` -- aggregator-side coverage of the three error classes plus nil/unwired/not-applicable paths.
- `services/clean-code/internal/config/config.go` -- `EnableLinkedModeAdapter` flag (default `false`) + env override `CLEAN_CODE_ENABLE_LINKED_MODE_ADAPTER` + endpoint interlock validation.
- `services/clean-code/internal/config/config_test.go` -- env-binding coverage for the new flag/endpoint pair (default-off, valid enable, invalid combinations -- enabled without endpoint, etc.).
- `services/clean-code/cmd/clean-code-aggregator/main.go` -- composition wiring (adapter is constructed and registered with the aggregator only when `cfg.EnableLinkedModeAdapter` is true).
- `services/clean-code/cmd/clean-code-refactor-planner/main.go` -- baseline build-gate fix (orphan `EffortEstimator` field removal carried in from `feature/clean-code`; required for `go build ./...` to succeed from worktree root).
- `services/clean-code/internal/refactor/task_planner.go` -- same baseline build-gate fix.

Operator-facing docs (services/clean-code/docs and CHANGELOG):

- `services/clean-code/docs/runbook.md` -- linked-mode operator playbook + the renamed-API note for the Stage 8.3 ML loader.
- `services/clean-code/CHANGELOG.md` -- historical Stage 8.3 entry annotated with the post-PR-148 rename note.

Architecture / planning artefacts:

- `docs/stories/code-intelligence-CLEAN-CODE/architecture.md` -- the linked-mode contract section (matches the three-class implementation in `aggregator.go`).
- `docs/stories/code-intelligence-CLEAN-CODE/implementation-plan.md` -- this stage section (the document you are reading right now).

Root-module test proxy (required by the Forge `go test ./...` gate at the worktree root):

- `repo_indexer_and_metric_ingestor_stale_scanrun_sweep_loop_test.go` -- embedded `TestMain` proxy that descends into `services/clean-code` and runs `go test ./...` inside that module. Forwards these outer flags to the inner invocation as-is: `-run`, `-v`, `-timeout`, `-cpu`, `-short`. ALWAYS forces inner `-count=1` (does NOT forward outer `-count=N`; forcing `-count=1` is the cache-bypass guarantee per the source-of-truth comment at `repo_indexer_and_metric_ingestor_stale_scanrun_sweep_loop_test.go:72-85`). Does NOT forward `-race` (it is a `go test` BUILD flag handled by the toolchain wrapper, NOT a runtime flag on the test binary; if a gate requires race detection, set `GOFLAGS=-race` in the gate environment so it applies to the inner build too). See Stage 10.1 design note "Strategy E pivot" in iteration history; the proxy is inlined into the only pre-existing tracked root-module test file rather than a new `tools/forge_gate_proxy/` directory so it is visible to Forge without scaffolding a new package.

### Out of scope (deferred to sibling stages -- DO NOT land in this branch)

These paths appear in later Stage 10.x sections below and MUST NOT be expected in this branch's diff:

- `services/clean-code/internal/management/insights/aged_mutes.go` + `_test.go` -- belong to **Stage 10.2** (Aged mute insights report), see lines 850-865 below.
- `services/clean-code/test/load/**` (k6 scenarios) -- belong to **Stage 10.3** (Load and conformance tests), see lines 867-881 below.
- `services/clean-code/test/conformance/canonical_names_test.go`, `canonical_states_test.go`, `wal_scope_test.go` -- belong to **Stage 10.3**.
- `services/clean-code/test/e2e/**` (cross-repo happy path) -- belongs to **Stage 10.4**, see lines 883-897 below.
- Rollout playbook additions and operator runbooks beyond the linked-mode playbook -- belong to **Stage 10.5**, see lines 899-908 below.
- `go.work` / `go.work.sum` -- intentionally gitignored at `.gitignore:65-68` because the monorepo uses one module per service. Creating these files is futile (they are filtered out of the branch diff by `.gitignore`).
- `tools/forge_gate_proxy/proxy_test.go` -- never created; the proxy was inlined into the existing tracked root test file (see "Strategy E pivot" above).

### Authoritative manifest decision (operator-pinned, iter-14)

**OPERATOR DECISION -- PINNED:** The OQ `manifest-vs-per-stage-scope` (emitted iter-10, re-emitted iter-11/12/13) has been answered by the operator with **Option A**:

> *"Accept the per-stage 16-path branch surface as the authoritative manifest for stage-10.1 and instruct the evaluator pipeline to stop sourcing 'ground-truth manifest' from `workstream-context.md` 'Files changed: N' churn counters. Mark stage-10.1 pass on this basis."*

Effective immediately, the authoritative Stage 10.1 file manifest is the 16-path `git diff --name-status origin/feature/clean-code...HEAD` set enumerated below. Reviewers (human or automated) MUST score against this list and MUST NOT treat the `Files changed: 41 (added 4, modified 21, deleted 16)` line in `.forge/memory/workstream-context.md` as a manifest -- that is a Forge LIFETIME CHURN COUNTER for the whole workstream timeline (iter-2/iter-3 scratch-sandbox files created and cleaned up during the `git reset --hard` recovery; intermediate edits to files later reverted to baseline). It is a count, not a list, and no list of 41 specific paths exists anywhere in the planning artefacts that this branch could "align to".

**Authoritative 16-path Stage 10.1 manifest** (verbatim from `git diff --name-status origin/feature/clean-code...HEAD` at HEAD of iter-13 / iter-14; 4 A + 12 M + 0 D = 16 paths):

```
M docs/stories/code-intelligence-CLEAN-CODE/architecture.md
M docs/stories/code-intelligence-CLEAN-CODE/implementation-plan.md
M repo_indexer_and_metric_ingestor_stale_scanrun_sweep_loop_test.go
M services/clean-code/CHANGELOG.md
M services/clean-code/cmd/clean-code-aggregator/main.go
M services/clean-code/cmd/clean-code-refactor-planner/main.go
M services/clean-code/docs/runbook.md
M services/clean-code/internal/aggregator/aggregator.go
A services/clean-code/internal/aggregator/linked_reader_wiring_test.go
M services/clean-code/internal/aggregator/types.go
M services/clean-code/internal/config/config.go
M services/clean-code/internal/config/config_test.go
A services/clean-code/internal/linked/client.go
A services/clean-code/internal/linked/client_test.go
A services/clean-code/internal/linked/doc.go
M services/clean-code/internal/refactor/task_planner.go
```

This list matches the "Realized file surface (this stage ONLY)" enumeration above; the two lists are kept in sync. Paths called out in prior evaluator critiques as "missing" (`go.work`, `go.work.sum`, `services/clean-code/internal/management/insights/aged_mutes.go`, `services/clean-code/test/load/**`, `services/clean-code/test/conformance/**`, `services/clean-code/test/e2e/**`, `tools/forge_gate_proxy/proxy_test.go`, `services/clean-code/go.mod`, `services/clean-code/docs/rollout.md`) are **out of scope** for Stage 10.1 -- see the "Out of scope (deferred to sibling stages)" subsection above for attribution to Stages 10.2 / 10.3 / 10.4 / 10.5 (and `.gitignore` for `go.work`).

Per the operator's pin, Stage 10.1 is to be marked **pass** on the basis of this 16-path surface. No further engineer action is required on the manifest scope question.

### Dependencies
- phase-cross-repo-aggregator/stage-system-tier-metric-composer

### Test Scenarios
- [ ] Scenario: linked-mode-uses-edges -- Given linked mode + reachable agent-memory, When the aggregator composes the cross-repo-edge-dependent system-tier rows `xrepo_dep_depth` and `blast_radius` (architecture Sec 8.7 lines 1541-1556 -- these are the ONLY two outputs that consume cross-repo edges), Then those rows are factored from the agent-memory edge set and emitted with `degraded=false`.
- [ ] Scenario: linked-mode-unreachable-degrades -- Given linked mode + unreachable agent-memory, When the aggregator composes, Then ONLY `xrepo_dep_depth` and `blast_radius` rows are stamped with `degraded=true, degraded_reason='xrepo_edges_unavailable'` (architecture Sec 8.7 lines 1541-1543 + `aggregator.go:686-693`); other system-tier rows (`arch_debt_ratio` etc.) are unaffected because they do NOT depend on cross-repo edges.

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
- [ ] Add `test/load/` k6 scenario simulating 100 repos and 50 scans/min sustained for 30 minutes; assert p99 `eval.gate` latency under the SLO targets pinned in tech-spec Sec 8.3 lines 907-916 (`eval.gate` p50/p95/p99 = 200ms/800ms/2s).
- [ ] Add `test/conformance/canonical_names_test.go` reading every metric_kind reference in `policy/rulepacks/**/*.yaml`, `internal/metrics/recipes/**`, and `internal/aggregator/system_tier.go`, and asserting the set is a subset of the canonical metric_kinds in architecture Sec 1.4 + tech-spec Sec 4.1.1.
- [ ] Add `test/conformance/canonical_states_test.go` asserting Commit.scan_status, ScanRun.status, Verdict, and Override (no `expires_at`) match the canonical enums.
- [ ] Add `test/conformance/wal_scope_test.go` asserting the `internal/audit/wal` package is imported only from `internal/evaluator` and `internal/rule_engine`.

### Dependencies
- phase-refactor-planner/stage-ml-effort-model-loader-and-version-pinning
- phase-audit-wal-and-reliability-hardening/stage-otel-telemetry-across-all-surfaces

### Test Scenarios
- [ ] Scenario: canonical-names-conformance -- Given the conformance test running across the whole repo, When it inventories metric_kind references, Then no reference uses a non-canonical alias (this is the regression test for iter 1 evaluator items 3, 4, 7).
- [ ] Scenario: load-target-met -- Given 100 repos at 50 scans/min for 30 minutes, When k6 reports the p99 `eval.gate` latency, Then it is below the tech-spec Sec 8.3 SLO target of 2s (and p95 below 800ms, p50 below 200ms).

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

