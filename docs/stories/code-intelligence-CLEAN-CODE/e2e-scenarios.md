# Story `code-intelligence:CLEAN-CODE` -- End-to-End Scenarios

> Sibling docs (read together, do NOT duplicate):
> - `architecture.md` -- canonical sub-stores, invariants G1-G7, operator pins (Sec 1.6), closed `degraded_reason` set (Sec 8.2).
> - `tech-spec.md` -- hard constraints C1-C25, parameter pins (Sec 8.2), SLO targets (Sec 8.3), signing pins (Sec 8.4), DDL/role grants (Sec 8.7), locked decisions (Sec 10).
> - `implementation-plan.md` -- 10 build phases and per-phase stages (canon alignment guarantees, lines 8-37).

This document encodes the QA contract for the `clean_code` service. Every
scenario is **anchored** with `[arch Sec X.Y]`, `[tech-spec Sec X.Y]`, or
`[tech-spec C##]` references so the evaluator and downstream agents can
prove behaviour back to the spec. Phase numbers here mirror
`implementation-plan.md` phases 1-10 with one additional Phase 11
(`Load and SLO conformance`) for the lab-bare-metal validation.

## How to read this doc

- **Phase H1** (`# Phase N: ...`) -- one phase per build slice. Each
  Phase H1 is **immediately** followed by `### Setup` and then
  `### Scenarios` (H3s, as the prompt requires).
- **Background blocks** at the top of `### Scenarios` apply to every
  scenario in that phase.
- **Tags** on scenarios:
  - `@happy` -- nominal path.
  - `@edge` -- boundary / unusual input.
  - `@degraded` -- partial-data path (closed `degraded_reason` set per
    [arch Sec 8.2]).
  - `@invariant` -- canon-guard; failure breaks G1-G7 or a C-constraint.
  - `@security` -- mTLS / OIDC / HMAC / Ed25519 / role-grant.
  - `@perf` -- SLO probe.
- **Canonical names only.** The scenarios use the exact names locked in
  the spec (e.g. `cyclo`, `lcom4`, `cognitive_complexity`,
  `xrepo_dep_depth`, `arch_debt_ratio`, `verdict in {pass|warn|block}`,
  `delta in {new|newly_failing|unchanged|resolved}`). Any deviation
  (e.g. `cyclomatic_complexity`, `verdict=gated`) MUST be rejected by
  the schema; scenarios in Phase 1 prove the rejection.
- **Connection strings are env-var names** (e.g. `CLEAN_CODE_PG_URL`),
  never literal hostnames or credentials. The matching values are
  injected from Vault / GH environment / ADO variable group depending
  on the phase's setup type.

The env-var names referenced across phases:

| Env var | Purpose |
| --- | --- |
| `CLEAN_CODE_PG_URL` | PostgreSQL DSN for the `clean_code` schema. |
| `CLEAN_CODE_KMS_URL` | Ed25519 signing key store (for `policy.publish`). |
| `CLEAN_CODE_OIDC_ISSUER` | OIDC issuer for the management surface. |
| `CLEAN_CODE_WEBHOOK_HMAC_SECRET` | Shared HMAC for external `ingest.*` verbs. |
| `CLEAN_CODE_OTEL_ENDPOINT` | OTLP gRPC endpoint for telemetry. |
| `AGENT_MEMORY_GRPC_ENDPOINT` | Sister service endpoint for linked-mode tests. |

---

# Phase 1: Foundation and schema migrations

Implements `implementation-plan.md` Phase 1 (lines 39-155). Establishes
the single `clean_code` Postgres schema, all enum types, role grants
(four service roles plus `clean_code_wal_reconciler`), forward+reverse
migration parity, and the canon-guard CHECK constraints. No service
processes run yet -- this phase is the schema contract.

### Setup
- **Type**: compose
- **Local**: `docker compose -f tests/e2e/phase-01-foundation/docker-compose.yml up -d --build` then `make test-phase-01`.
- **CI runner**: GitHub-hosted `ubuntu-latest` (label `ubuntu-latest`); no lab hardware required.
- **Secrets**: none (compose-local Postgres superuser is generated per-run and exported as `CLEAN_CODE_PG_URL`).
- **Pre-test bootstrap**: `make migrate-up` (applies every numbered SQL migration), then `make migrate-down && make migrate-up` (forward/reverse parity check).

Compose services at `tests/e2e/phase-01-foundation/docker-compose.yml`:
- `postgres` (Postgres 16, healthcheck `pg_isready`, exposes 5432 internally only; DSN exported as `CLEAN_CODE_PG_URL`).
- `migrator` (one-shot container that runs `make migrate-up`; exit 0 gates the test job).

### Scenarios

```gherkin
Background:
  Given a fresh Postgres database reachable via `CLEAN_CODE_PG_URL`
    And the migrator container has applied every numbered migration in `db/migrations/`
    And the active migration head equals the value in `MIGRATION_HEAD`
```

```gherkin
@happy @invariant
Feature: Single-schema layout [arch Sec 1.5, tech-spec C1]
  Scenario: Only the `clean_code` schema exists for service objects
    When the suite queries `information_schema.schemata`
    Then the result contains exactly one user schema named `clean_code`
     And no schemas named `catalog`, `measurement`, `policy`, `audit`, or `refactor` exist
```

```gherkin
@happy @invariant
Feature: Canonical metric_kind enum [arch Sec 1.4.1, Sec 1.4.2, tech-spec Sec 4.1.1]
  Scenario: metric_kind enum contains the 22 locked names and nothing else
    When the suite queries `pg_type` for the `metric_kind` enum labels
    Then the labels are exactly the 22 names:
      | cyclo                          |
      | cognitive_complexity           |
      | loc                            |
      | lcom4                          |
      | fan_in                         |
      | fan_out                        |
      | depth_of_inheritance           |
      | interface_width                |
      | coupling_between_objects       |
      | cycle_member                   |
      | duplication_ratio              |
      | modification_count_in_window   |
      | coverage_line_ratio            |
      | coverage_branch_ratio          |
      | pass_first_try_ratio           |
      | xrepo_dep_depth                |
      | arch_debt_ratio                |
      | velocity_trend                 |
      | arch_fitness                   |
      | blast_radius                   |
      | xservice_test_reliability      |
      | knowledge_index                |
     And no label named `cyclomatic_complexity`, `lines_of_code`, `coverage_line`, `coverage_branch`, `p50.system`, or `p95.system` exists
```

```gherkin
@invariant
Feature: scope_kind enum is the seven canonical values [arch Sec 5.1, tech-spec C2]
  Scenario: scope_kind labels are exactly the seven locked names
    When the suite queries the `scope_kind` enum labels
    Then the labels are exactly `repo`, `package`, `file`, `class`, `interface`, `method`, `block`
     And no label named `function` or `module` exists
```

```gherkin
@invariant
Feature: verdict and delta enums are tight [arch Sec 5.4.1, tech-spec C8, C12]
  Scenario: verdict labels are exactly three values
    When the suite queries the `verdict_kind` enum labels
    Then the labels are exactly `pass`, `warn`, `block`
     And no label named `fail` or `gated` exists

  Scenario: finding delta labels are exactly four values
    When the suite queries the `finding_delta_kind` enum labels
    Then the labels are exactly `new`, `newly_failing`, `unchanged`, `resolved`
     And no label named `regression`, `improvement`, or `flat` exists
```

```gherkin
@invariant
Feature: degraded_reason CHECK enforces the closed set [arch Sec 8.2, tech-spec C21]
  Scenario: Insert with a valid degraded_reason succeeds
    Given a row would be inserted into `evaluation_verdict` with `degraded=true` and `degraded_reason='xrepo_edges_unavailable'`
    When the insert is attempted
    Then the row is persisted

  Scenario Outline: All four members of the closed set are accepted
    When the suite inserts an `evaluation_verdict` row with `degraded=true` and `degraded_reason='<reason>'`
    Then the row is persisted
    Examples:
      | reason                       |
      | xrepo_edges_unavailable      |
      | samples_pending              |
      | policy_signature_invalid     |
      | percentile_stale             |

  Scenario: Insert with a non-member degraded_reason is rejected
    When the suite inserts an `evaluation_verdict` row with `degraded=true` and `degraded_reason='unknown'`
    Then Postgres returns a CHECK constraint violation
     And the row is NOT persisted
```

```gherkin
@invariant
Feature: ScanRun and Commit status enums are tight [arch Sec 5.2.2, Sec 5.2.3]
  Scenario: Commit.scan_status labels are the four locked names
    When the suite queries the `commit_scan_status` enum labels
    Then the labels are exactly `pending`, `scanning`, `scanned`, `failed`

  Scenario: ScanRun.status labels are the three locked names
    When the suite queries the `scan_run_status` enum labels
    Then the labels are exactly `running`, `succeeded`, `failed`

  Scenario: RepoEvent.kind labels are exactly the four past-tense names
    When the suite queries the `repo_event_kind` enum labels
    Then the labels are exactly `registered`, `retired`, `retract_intent`, `mode_changed`
     And no label named `register`, `retire`, `change_mode`, or `intent_to_retract` exists
```

```gherkin
@invariant
Feature: RefactorTask.kind enum [arch Sec 5.5.3]
  Scenario: refactor_task_kind labels are exactly five values
    When the suite queries the `refactor_task_kind` enum labels
    Then the labels are exactly `split_class`, `extract_method`, `invert_dependency`, `break_cycle`, `consolidate_duplication`
     And no label named `extract_function`, `reduce_lcom`, or `introduce_interface` exists
```

```gherkin
@invariant
Feature: MetricSample append-only and active-pointer uniqueness [arch Sec 1.5.1, Sec 5.2.1, tech-spec C5]
  Scenario: DELETE on `metric_sample_active` is REVOKEd from both writer roles
    When the suite, connected as `clean_code_metric_ingestor`, attempts `DELETE FROM metric_sample_active`
    Then Postgres returns `permission denied for table metric_sample_active`

    When the suite, connected as `clean_code_xrepo_aggregator`, attempts `DELETE FROM metric_sample_active`
    Then Postgres returns `permission denied for table metric_sample_active`

  Scenario: PRIMARY KEY on metric_sample_active is exactly the quintuple (sample_id is NOT part of the PK)
    When the suite reads `pg_indexes` and `pg_constraint` for `metric_sample_active`
    Then the PRIMARY KEY constraint covers exactly the columns `(repo_id, sha, scope_id, metric_kind, metric_version)` (no `sample_id` column inside the PK -- per tech-spec Sec 7.1.b lines 1108-1115 and implementation-plan Stage 1.3 line 90)
     And `sample_id` is a NOT NULL FK column with a SEPARATE UNIQUE INDEX named `metric_sample_active_sample_id_uniq` (per tech-spec lines 1118-1119)
     And the suite asserts the two indexes are distinct (the PK index and the sample_id unique index are NOT the same physical index)

  Scenario: MetricRetraction.sample_id FK is UNIQUE
    When the suite reads constraints on `metric_retraction`
    Then `sample_id` is both a FOREIGN KEY to `metric_sample(sample_id)` AND a UNIQUE constraint
```

```gherkin
@invariant @security
Feature: Two-writer carve-out on Measurement [arch Sec 1.5, tech-spec C5]
  Scenario: clean_code_metric_ingestor cannot write `pack='system'`
    Given the suite is connected as `clean_code_metric_ingestor`
    When the suite inserts a `metric_sample` row with `pack='system'`
    Then Postgres returns a row-level-security or trigger denial
     And the row is NOT persisted

  Scenario: clean_code_xrepo_aggregator cannot write `pack='base'`
    Given the suite is connected as `clean_code_xrepo_aggregator`
    When the suite inserts a `metric_sample` row with `pack='base'`
    Then Postgres returns a row-level-security or trigger denial
     And the row is NOT persisted

  Scenario: clean_code_xrepo_aggregator may write `pack='system' AND source='derived'`
    Given the suite is connected as `clean_code_xrepo_aggregator`
    When the suite inserts a `metric_sample` row with `pack='system'` and `source='derived'`
    Then the row is persisted
```

```gherkin
@invariant @security
Feature: Three-writer ACL on Audit tables [arch Sec 1.5, tech-spec C8]
  Scenario Outline: <role> may INSERT/SELECT but not UPDATE/DELETE on audit tables
    Given the suite is connected as `<role>`
    When the suite issues `INSERT INTO <table> (...) VALUES (...)`
    Then the row is persisted

    When the suite issues `UPDATE <table> SET ... WHERE ...`
    Then Postgres returns `permission denied for table <table>`

    When the suite issues `DELETE FROM <table>`
    Then Postgres returns `permission denied for table <table>`
    Examples:
      | role                          | table                |
      | clean_code_evaluator          | evaluation_run       |
      | clean_code_evaluator          | evaluation_verdict   |
      | clean_code_evaluator          | finding              |
      | clean_code_solid_batch        | evaluation_run       |
      | clean_code_solid_batch        | evaluation_verdict   |
      | clean_code_solid_batch        | finding              |
      | clean_code_wal_reconciler     | evaluation_run       |
      | clean_code_wal_reconciler     | evaluation_verdict   |
      | clean_code_wal_reconciler     | finding              |
```

```gherkin
@invariant
Feature: Override has no expires_at column [arch Sec 5.5.3, tech-spec C16]
  Scenario: information_schema.columns has no `expires_at` on override
    When the suite queries `information_schema.columns` for `override`
    Then no column named `expires_at` exists
```

```gherkin
@invariant
Feature: refactor_task has no status and no expected_metric_delta [arch Sec 5.5.3, tech-spec C19]
  Scenario: information_schema.columns excludes both columns
    When the suite queries `information_schema.columns` for `refactor_task`
    Then no column named `status` exists
     And no column named `expected_metric_delta` exists
```

```gherkin
@invariant
Feature: Refactor traceback goes task -> plan -> hotspot -> policy_version [arch Sec 5.5.1-5.5.3, tech-spec C20]
  Scenario: hot_spot carries policy_version_id, refactor_plan does NOT
    When the suite queries `information_schema.columns`
    Then `hot_spot` has a column `policy_version_id` (NOT NULL FK to `policy_version`)
     And `refactor_plan` has NO column named `policy_version_id`
```

```gherkin
@invariant
Feature: No invented tables [implementation-plan.md lines 8-37]
  Scenario: prohibited table names do not exist
    When the suite queries `information_schema.tables` for `table_schema='clean_code'`
    Then no table is named `audit_event`
     And no table is named `audit_anchor`
     And no table is named `effort_estimate`
     And no table is named `rule_pack_revision`
     And no table is named `policy_override`
     And no table is named `swap_active`
```

```gherkin
@invariant
Feature: No tenant_id columns (v1 single-tenant) [arch Sec 1.7, tech-spec C25]
  Scenario: no service table has a `tenant_id` column
    When the suite queries `information_schema.columns` for `table_schema='clean_code' AND column_name='tenant_id'`
    Then the result set is empty
```

```gherkin
@invariant
Feature: Forward / reverse migration parity
  Scenario: down + up restores the same schema hash
    Given the suite has captured a schema fingerprint via `pg_dump -s` after `migrate-up`
    When the suite runs `migrate-down --all` then `migrate-up`
    Then a fresh `pg_dump -s` produces the byte-identical fingerprint
```

---

# Phase 2: AST Adapter and foundation tier compute

Implements `implementation-plan.md` Phase 2 (lines 156-261). The AST
adapter ingests source code for the four v1 languages (Go, Python,
TypeScript, Java) and emits **11 of the 12** foundation `metric_sample`
rows per scope. The 12th foundation kind, `modification_count_in_window`,
is **NOT** produced by the AST adapter -- it is produced by the
**Metric Ingestor's modification_count materialiser** (implementation-plan
Stage 2.6 lines 247-261) consuming rows fed by `ingest.churn` (arch
Sec 1.4.1 row 12 line 105, tech-spec Sec 4.1.1 lines 287-291, Sec 4.11,
Sec 8.2 `window_days=90`). The materialiser lives in the same writer
role as the AST adapter (the Metric Ingestor) and emits its row with
`pack='base'`, `source='computed'` while recording `attrs_json.provenance='ingested'`
as the provenance annotation. Phase 2 is in-process pure-Go; the
materialiser is exercised with a synthetic churn stream (no DB
required). End-to-end churn-webhook -> materialiser plumbing is
re-asserted in Phase 4.

### Setup
- **Type**: inline
- **Local**: `make test-phase-02` (runs `go test ./internal/ast/...` against the fixture corpus under `tests/fixtures/ast/{go,python,typescript,java}` and `go test ./internal/metrics/materialisers/...` against a synthetic churn stream).
- **CI runner**: GitHub-hosted `ubuntu-latest`.
- **Secrets**: none.
- **Pre-test bootstrap**: `make fixtures-ast` extracts `tests/fixtures/ast.tar.zst` if missing; `make fixtures-churn-synth` generates the in-memory churn fixtures used by the materialiser scenarios.

### Scenarios

```gherkin
Background:
  Given the AST adapter is loaded in-process
    And the fixture corpus under `tests/fixtures/ast/` contains
        Go, Python, TypeScript, and Java reference projects
```

```gherkin
@happy @invariant
Feature: AST adapter emits EXACTLY the 11 AST-derived foundation kinds (NOT `modification_count_in_window`) [arch Sec 1.4.1, Sec 5.2.1; implementation-plan Stage 2.4-2.5]
  Scenario: A Java fixture file produces samples covering 11 of 12 foundation metric_kinds, partitioned by pack
    Given the fixture `tests/fixtures/ast/java/billing/InvoiceProcessor.java` defines one class with three methods
    When the AST adapter parses the file and emits foundation metrics for every applicable scope
    Then the union of emitted `metric_sample.metric_kind` values across the file's scopes is exactly these 11 names:
        `cyclo`, `cognitive_complexity`, `loc`, `lcom4`, `fan_in`, `fan_out`,
        `depth_of_inheritance`, `interface_width`, `coupling_between_objects`,
        `cycle_member`, `duplication_ratio`
     And NO row has `metric_kind='modification_count_in_window'` (this kind is materialised by the Metric Ingestor's `modification_count` materialiser from `ingest.churn` data per arch Sec 1.4.1 row 12 line 105, tech-spec Sec 4.1.1 lines 287-291; the AST adapter is NOT a producer of this kind)
     And the 5 rows emitted with `pack='base'` from AST are exactly `cyclo`, `cognitive_complexity`, `loc`, `cycle_member`, `duplication_ratio` (the 6th `pack='base'` kind `modification_count_in_window` is not emitted by AST)
     And the 6 rows emitted with `pack='solid'` are exactly `lcom4`, `fan_in`, `fan_out`, `depth_of_inheritance`, `interface_width`, `coupling_between_objects`
     And every emitted row has `source='computed'` (the MetricSample source enum is `computed | ingested | derived` per arch Sec 5.2.1 -- never `ast` or `external`)
     And the class scope `com.example.billing.InvoiceProcessor` emits exactly the 6 class-applicable kinds:
        `lcom4`, `fan_in`, `fan_out`, `depth_of_inheritance`, `interface_width`, `coupling_between_objects`
```

```gherkin
@happy @invariant
Feature: Metric Ingestor materialiser emits `modification_count_in_window` from churn (NOT the AST adapter) [arch Sec 1.4.1 row 12 line 105, tech-spec Sec 4.1.1 lines 287-291, implementation-plan Stage 2.6 lines 247-261]
  Scenario: Synthetic churn stream feeds the materialiser; it emits a single canonical row per scope
    Given a synthetic churn stream `C90` carries 7 commits touching scope `pkg.Foo.bar` within the last 90 days
      And the AST adapter has previously emitted the 11 AST-derived foundation rows for `pkg.Foo.bar` in the same `ScanRun`
    When the `modification_count` materialiser (`internal/metrics/materialisers/modification_count.go`) runs over `C90`
    Then exactly one `metric_sample` row is emitted with `metric_kind='modification_count_in_window'`
     And the emitted row has `pack='base'` and `source='computed'` (per tech-spec Sec 4.1.1 line 290 -- the materialiser is the **computing** writer; ingested provenance is recorded on `attrs_json`, NOT on the `source` enum)
     And the emitted row's `attrs_json.provenance` equals the literal string `ingested` (implementation-plan Stage 2.6 line 260)
     And the emitted row's `attrs_json.window_days` equals `90` (the operator pin from tech-spec Sec 8.2)
     And the materialiser's writer identity is the Metric Ingestor (same role that owns the AST adapter samples); the AST adapter has emitted zero rows for this metric_kind in this scan_run_id

  Scenario: Out-of-window churn rows are ignored (no zero-fill noise)
    Given a synthetic churn stream `C_OLD` whose commits are all older than 90 days for scope `pkg.Bar.baz`
    When the materialiser runs over `C_OLD`
    Then zero `metric_sample` rows are emitted with `metric_kind='modification_count_in_window'` for `pkg.Bar.baz` (per implementation-plan Stage 2.6 line 253: skip when no churn rows exist in the window)

  Scenario: AST adapter is NOT a producer of `modification_count_in_window` (canon-guard, no churn fed)
    Given the fixture `tests/fixtures/ast/java/billing/InvoiceProcessor.java`
      And NO churn stream is fed (the materialiser has no input)
    When the AST adapter alone runs over the fixture and the producer-attribution registry is queried
    Then no `metric_sample` row exists with `metric_kind='modification_count_in_window'` (the AST adapter is not registered as a producer of this kind)
     And the registry maps `modification_count_in_window` to the writer identity `modification_count_materialiser`, NOT to any AST language analyzer
```

```gherkin
@happy
Feature: scope_id is deterministic UUIDv5 across SHAs [arch Sec 5.1, G2]
  Scenario: Same canonical signature at two SHAs yields the same scope_id
    Given the fixture file is read at SHA `aaaa1111` and at SHA `bbbb2222`
      And the class `com.example.billing.InvoiceProcessor` exists at both SHAs
    When the adapter computes `scope_id` for the class at both SHAs
    Then both runs produce the same UUIDv5 derived from `(repo_id, scope_kind='class', canonical_signature='com.example.billing.InvoiceProcessor', first_seen_sha='aaaa1111')`
```

```gherkin
@edge
Feature: cycle_member emitted only inside dependency cycles [arch Sec 1.4.1]
  Scenario: A package with a cycle emits cycle_member=1 for each participant
    Given the fixture `tests/fixtures/ast/go/cycle/{a,b,c}.go` forms an a->b->c->a cycle
    When the adapter computes foundation metrics for the package
    Then `cycle_member=1` rows exist for `a`, `b`, and `c`
     And no `cycle_member=1` row exists for any acyclic package in the fixture set

  Scenario: An acyclic graph emits no value=1 cycle_member rows
    Given the fixture `tests/fixtures/ast/python/acyclic/` has no import cycles
    When the adapter computes foundation metrics
    Then zero `metric_sample` rows exist with `metric_kind='cycle_member'` and `value=1.0`
     And every in-project file scope and package scope emits a `metric_kind='cycle_member'` row with `value=0.0` and empty `attrs_json` (the brief's universal `value=0 otherwise` contract; see implementation-plan Stage 2.5 "file D outside the cycle emits value=0")
```

```gherkin
@edge
Feature: Duplication detection is content-canonicalised [arch Sec 1.4.1]
  Scenario: Whitespace-only differences are still flagged as duplication
    Given two TypeScript functions differing only in indentation and trailing newlines
    When the adapter computes `duplication_ratio` for the containing file
    Then the value is greater than 0.95 and less than or equal to 1.0
```

```gherkin
@edge
Feature: Window default for `modification_count_in_window` materialiser is 90 days [tech-spec Sec 8.2]
  Scenario: Window default is 90 days and is read from the materialiser config (NOT from the AST adapter)
    Given the materialiser config has no `window_days` override
    When the `modification_count` materialiser runs over a synthetic churn stream
    Then the count covers commits whose `modified_at` is within the last 90 days
     And the emitted row's `attrs_json.window_days` field equals `90`
     And the AST adapter does NOT read or honour the `window_days` config (it is not a producer of this kind)
```

```gherkin
@invariant @security
Feature: Registry refuses non-v1 languages [tech-spec C3]
  Scenario: A C# source file is rejected
    Given the fixture `tests/fixtures/ast/csharp/Program.cs`
    When the registry is asked to dispatch to an analyzer for `language='csharp'`
    Then the registry returns error code `LANGUAGE_NOT_SUPPORTED`
     And no metric_sample rows are produced

  Scenario: A Rust source file is rejected
    Given the fixture `tests/fixtures/ast/rust/main.rs`
    When the registry is asked to dispatch to an analyzer for `language='rust'`
    Then the registry returns error code `LANGUAGE_NOT_SUPPORTED`
```

```gherkin
@perf
Feature: AST adapter throughput SLO [tech-spec Sec 8.3]
  Scenario: A single worker processes >=50k LOC per second
    Given a synthetic Go fixture containing 1,000,000 LOC partitioned into 200 files
    When the AST adapter processes the fixture with a single worker thread
    Then the elapsed wall time is <= 20 seconds (i.e. >= 50,000 LOC/sec)
     And the OTel span `ast.adapter.run` reports `lines_processed >= 1000000`
```

---

# Phase 3: Repo Indexer and Metric Ingestor

Implements `implementation-plan.md` Phase 3 (lines 263-350). Brings the
indexer up against a real Postgres, wires the ingestor's write path
including the active-pointer side relation, and proves the
single-writer constraint on `metric_sample_active` for the two
permitted roles.

### Setup
- **Type**: compose
- **Local**: `docker compose -f tests/e2e/phase-03-indexer-ingestor/docker-compose.yml up -d --build` then `make test-phase-03`.
- **CI runner**: GitHub-hosted `ubuntu-latest`.
- **Secrets**: none (compose-local Postgres and OTel collector).
- **Pre-test bootstrap**: `make migrate-up` against `CLEAN_CODE_PG_URL`; `make seed-fixtures-phase-03` loads a 3-repo, 12-SHA fixture corpus.

Compose services at `tests/e2e/phase-03-indexer-ingestor/docker-compose.yml`:
- `postgres` (Postgres 16; DSN -> `CLEAN_CODE_PG_URL`).
- `otel-collector` (gRPC 4317 internal; endpoint -> `CLEAN_CODE_OTEL_ENDPOINT`).
- `indexer` (the `clean-code-indexer` service binary).
- `ingestor` (the `clean-code-metric-ingestor` service binary, role `clean_code_metric_ingestor`).

### Scenarios

```gherkin
Background:
  Given the compose stack is healthy (postgres `pg_isready`, otel collector accepting traffic)
    And the migrator has reached the current head
    And the indexer and ingestor services are running and have registered with the OTel collector
    And three repos are registered: `repo-a` (scan_mode=embedded), `repo-b` (scan_mode=embedded), `repo-c` (scan_mode=external)
```

```gherkin
@happy
Feature: Indexer enqueues a full ScanRun on a new SHA [arch Sec 3.5]
  Scenario: A new commit on repo-a creates pending Commit row and ScanRun
    Given `repo-a` is on SHA `0000aaaa` with `Commit.scan_status='scanned'`
    When the indexer observes a new SHA `1111bbbb`
    Then a `Commit` row is upserted with `scan_status='pending'`
     And a `ScanRun` row is created with `kind='delta'`, `sha_binding='single'`, `status='running'`, `to_sha='1111bbbb'` (canonical `ScanRun.kind` enum is `full | delta | external_single | external_per_row | retract` per implementation-plan Stage 1.2)
     And after the AST adapter completes, the `ScanRun` transitions `running -> succeeded`
     And the `Commit.scan_status` transitions `pending -> scanning -> scanned`
```

```gherkin
@happy @invariant
Feature: Ingestor writes immutable MetricSample plus active pointer [arch Sec 1.5.1, Sec 5.2.1]
  Scenario: First sample for a (repo,sha,scope,kind,version) tuple
    Given no prior `metric_sample` exists for `(repo-a, 1111bbbb, scope_id=S1, cyclo, metric_version=v1)`
    When the ingestor writes a new `metric_sample` row `M1`
    Then `metric_sample` contains exactly one row matching the tuple, with `sample_id=M1`
     And `metric_sample_active` contains exactly one row whose `sample_id=M1`
     And no row in `metric_sample` was UPDATEd (only INSERTed)
```

```gherkin
@happy @invariant
Feature: Retraction appends a new sample and re-points the active row [arch Sec 1.5.1]
  Scenario: Sample `M1` is retracted and replaced by `M2`
    Given an active row exists pointing at `M1`
    When the ingestor receives `mgmt.retract_sample(sample_id=M1, reason='outlier')`
    Then a `metric_retraction(sample_id=M1)` row is appended (UNIQUE)
     And a new `metric_sample` row `M2` is INSERTed for the same tuple with `attributes.retraction_of=M1`
     And `metric_sample_active` is UPSERTed so the active sample_id for the tuple equals `M2`
     And `M1` remains physically present in `metric_sample` (G3 append-only)

  Scenario: DELETE on metric_sample_active is denied even during retraction
    When the ingestor attempts `DELETE FROM metric_sample_active WHERE sample_id=M1`
    Then Postgres returns `permission denied for table metric_sample_active`
```

```gherkin
@invariant
Feature: Ingestor role cannot write the system pack [tech-spec C5]
  Scenario: Ingestor INSERT with `pack='system'` is denied
    Given the ingestor is bound to role `clean_code_metric_ingestor`
    When it attempts `INSERT INTO metric_sample (..., pack) VALUES (..., 'system')`
    Then Postgres returns a denial
     And the row is NOT persisted
```

```gherkin
@edge
Feature: Concurrent ingest for the same tuple is idempotent
  Scenario: Two ingestor workers race on the same (tuple, value)
    Given two ingestor workers W1 and W2 receive the same metric for `(repo-a, 1111bbbb, S1, cyclo, v1)` with value `7`
    When both call `ingest.metric` concurrently
    Then exactly one `metric_sample` row exists for that tuple at the end of the test
     And exactly one row exists in `metric_sample_active` for that tuple
     And no duplicate-key error is surfaced to the second caller
     (the second caller's response carries `idempotent_replay=true`)
```

```gherkin
@perf
Feature: Ingestor sustained write throughput SLO [tech-spec Sec 8.3]
  Scenario: 5,000 samples/sec sustained over 60 seconds with p99 latency <= 25 ms
    Given the suite drives the ingestor at 5,000 samples/sec for 60 seconds (300k samples)
    When the run completes
    Then no errors are returned to callers
     And the ingestor's `ingest.metric.write_latency_ms` p99 (OTel histogram) is <= 25
     And exactly 300,000 rows appear in `metric_sample` for the test repo
```

---

# Phase 4: External Metric Ingest Webhook

Implements `implementation-plan.md` Phase 4 (lines 352-437). Adds the
REST+HMAC webhook for `ingest.coverage`, `ingest.test_balance`,
`ingest.churn`, and `ingest.defects`. Validates payload shapes,
single vs per_row ScanRun shapes, and the "store-only" behaviour for
`ingest.defects` in v1.

### Setup
- **Type**: compose
- **Local**: `docker compose -f tests/e2e/phase-04-webhook/docker-compose.yml up -d --build` then `make test-phase-04`.
- **CI runner**: GitHub-hosted `ubuntu-latest`.
- **Secrets**: none (an ephemeral 32-byte HMAC secret is generated per-run and exported as `CLEAN_CODE_WEBHOOK_HMAC_SECRET`).
- **Pre-test bootstrap**: `make migrate-up`; `make seed-repo-d` (creates `repo-d` in `external` mode with a known SHA `dddd0001`).

Compose services at `tests/e2e/phase-04-webhook/docker-compose.yml`:
- `postgres` (Postgres 16; DSN -> `CLEAN_CODE_PG_URL`).
- `webhook` (the `clean-code-webhook` service, mounting `CLEAN_CODE_WEBHOOK_HMAC_SECRET`).
- `ingestor` (same as Phase 3).
- `otel-collector` (gRPC; endpoint -> `CLEAN_CODE_OTEL_ENDPOINT`).

### Scenarios

```gherkin
Background:
  Given the webhook is reachable at `https://webhook:8443/ingest`
    And `CLEAN_CODE_WEBHOOK_HMAC_SECRET` is shared between client and server
    And `repo-d` is registered in `external` scan_mode at SHA `dddd0001`
```

```gherkin
@happy @security
Feature: HMAC validation per [tech-spec Sec 8.5]
  Scenario: Request with a valid signature is accepted
    Given the client signs the request body with `CLEAN_CODE_WEBHOOK_HMAC_SECRET` using HMAC-SHA256
    When the request hits `POST /ingest/coverage`
    Then the server responds 202 Accepted with a `scan_run_id` in the body

  Scenario: Request with a missing signature is rejected
    When the client sends a request omitting the `X-Forge-Signature` header
    Then the server responds 401 Unauthorized
     And no `ScanRun` row is created

  Scenario: Request with a malformed signature is rejected
    When the client sends a request with `X-Forge-Signature: sha256=BAD`
    Then the server responds 401 Unauthorized
     And no `ScanRun` row is created
```

```gherkin
@happy
Feature: ingest.coverage accepts Cobertura XML [arch Sec 1.6 operator pin]
  Scenario: Valid Cobertura XML produces foundation+ingested rows
    Given the request `Content-Type` is `application/xml`
      And the body is a valid Cobertura XML covering `repo-d` SHA `dddd0001`
    When the webhook accepts the payload and the ingestor processes it
    Then the resulting `ScanRun` row has `kind='external_single'`, `sha_binding='single'`, and `to_sha='dddd0001'`
     And `metric_sample` contains exactly N rows with `metric_kind='coverage_line_ratio'` (one per scope in the XML)
     And `metric_sample` contains exactly N rows with `metric_kind='coverage_branch_ratio'`
     And every row has `pack='ingested'` and `source='ingested'` (NOT `source='external'`; the source enum is `computed | ingested | derived` per arch Sec 5.2.1 / tech-spec Sec 4.1.1)

  Scenario: Coverage payload with a non-XML Content-Type is rejected
    When the client posts a JSON payload to `/ingest/coverage`
    Then the server responds 415 Unsupported Media Type
     And no `ScanRun` row is created
```

```gherkin
@happy
Feature: ingest.test_balance accepts JSON [arch Sec 1.4.1]
  Scenario: Valid JSON produces pass_first_try_ratio rows
    Given the body is JSON `[{"scope_id":"S1","attempt_count":3,"pass_count":3},{"scope_id":"S2","attempt_count":2,"pass_count":1}]`
    When the webhook accepts the payload and the ingestor processes it
    Then `metric_sample` contains a row for `S1` with `pass_first_try_ratio=1.0`
     And `metric_sample` contains a row for `S2` with `pass_first_try_ratio=0.5`
     And every emitted row has `pack='ingested'` and `source='ingested'`
     And the resulting `ScanRun.kind='external_single'`, `ScanRun.sha_binding='single'`, and `to_sha='dddd0001'`
```

```gherkin
@happy
Feature: ingest.churn writes ZERO metric_sample rows directly [tech-spec C7, arch Sec 4.11]
  Scenario: Churn payload feeds the materialiser only
    When the webhook accepts a valid `ingest.churn` payload for repo-d
    Then a `ScanRun` row is persisted with `kind='external_per_row'`, `sha_binding='per_row'`, and `to_sha IS NULL`
     And zero new rows appear in `metric_sample` directly attributable to this scan_run_id
     And the churn materialiser feeds the `modification_count_in_window` recomputation, which lands a row with `pack='base'` and `source='computed'` on the next ingestor pass
```

```gherkin
@happy @invariant
Feature: ingest.defects is store-only in v1 [tech-spec C7, implementation-plan Stage 4.4]
  Scenario: Defects payload is stored but emits no metric_sample
    When the webhook accepts a valid `ingest.defects` payload
    Then a `ScanRun` row is persisted with `kind='external_per_row'`, `sha_binding='per_row'`, `to_sha IS NULL`, and a non-null `payload_hash`
     And zero rows appear in `metric_sample` referencing this scan_run_id (no `metric_kind='defect_density'` row exists in v1)
     And the original payload is retrievable via `mgmt.read.scan_run(scan_run_id)`
```

```gherkin
@edge @invariant
Feature: ScanRun kind and sha_binding are correct per verb [arch Sec 5.2.3, implementation-plan Stage 1.2]
  Scenario Outline: <verb> produces ScanRun with <kind> / <sha_binding>
    When the webhook accepts a valid payload for `<verb>`
    Then the resulting `ScanRun.kind='<kind>'` and `ScanRun.sha_binding='<sha_binding>'`
     And `to_sha` is `<to_sha_state>`
    Examples:
      | verb                | kind              | sha_binding | to_sha_state         |
      | ingest.coverage     | external_single   | single      | equal to commit SHA  |
      | ingest.test_balance | external_single   | single      | equal to commit SHA  |
      | ingest.churn        | external_per_row  | per_row     | NULL                 |
      | ingest.defects      | external_per_row  | per_row     | NULL                 |

  Scenario: ScanRun.kind enum is exactly the five canonical values
    When the suite queries the `scan_run_kind` enum labels
    Then the labels are exactly `full`, `delta`, `external_single`, `external_per_row`, `retract`
     And no label named `single`, `per_row`, `ast_foundation`, or `incremental` exists

  Scenario: ScanRun.sha_binding enum is exactly two values
    When the suite queries the `scan_run_sha_binding` enum labels
    Then the labels are exactly `single`, `per_row`
```

```gherkin
@security
Feature: Idempotent replay defends against retries [tech-spec C9]
  Scenario: Identical body+signature replayed within 24h is a no-op
    Given a webhook call with `Idempotency-Key: K1` was accepted at T0
    When the exact same call is replayed at T0+30min
    Then the server responds 202 Accepted with the SAME `scan_run_id`
     And no new `ScanRun` row was created
     And no new `metric_sample` rows were inserted
```

---

# Phase 5: Policy Steward and SOLID Rule Engine

Implements `implementation-plan.md` Phase 5 (lines 439-568). Brings up
the `policy.publish` / `policy.activate` / `policy.publish_rulepack`
verbs with Ed25519 signing, the 24h signing-key rotation overlap
(`policy_publish_overlap_min_seconds=86400`, implementation-plan Stage
5.1 -- two signing keys may co-exist during the overlap), the
append-only `PolicyActivation` mechanism (arch Sec 5.3.4 -- there is
NO `status` column on `PolicyVersion`; the active version is the
latest row in `policy_activation` by `created_at`), and the rule
engine that evaluates SOLID rules against a metric snapshot.

### Setup
- **Type**: compose
- **Local**: `docker compose -f tests/e2e/phase-05-policy-engine/docker-compose.yml up -d --build` then `make test-phase-05`.
- **CI runner**: GitHub-hosted `ubuntu-latest`.
- **Secrets**: none (compose includes a `kms-mock` container backed by an in-memory Ed25519 keypair generated per-run; URL exported as `CLEAN_CODE_KMS_URL`).
- **Pre-test bootstrap**: `make migrate-up`; `make seed-phase-05` registers `repo-a`/`repo-b` with a baseline metric_sample snapshot.

Compose services at `tests/e2e/phase-05-policy-engine/docker-compose.yml`:
- `postgres` (Postgres 16; DSN -> `CLEAN_CODE_PG_URL`).
- `kms-mock` (Ed25519 sign/verify HTTP shim; URL -> `CLEAN_CODE_KMS_URL`).
- `policy-steward` (the `clean-code-policy-steward` service).
- `rule-engine` (the `clean-code-rule-engine` service, role `clean_code_solid_batch`).
- `otel-collector` (endpoint -> `CLEAN_CODE_OTEL_ENDPOINT`).

### Scenarios

```gherkin
Background:
  Given the compose stack is healthy
    And the `kms-mock` exposes an Ed25519 keypair with key-id label `K1` (the kms-mock's own opaque identifier; `kid` is NOT a column on `policy_version`)
    And a baseline metric snapshot exists for `repo-a` at SHA `aaaa1111`
```

```gherkin
@happy @security
Feature: policy.publish signs a PolicyVersion with Ed25519 [tech-spec Sec 8.4, arch Sec 5.3.3]
  Scenario: A valid policy document is signed and persisted
    Given a candidate `PolicyVersion` payload `D1` is valid against the schema
    When the operator calls `policy.publish(D1)`
    Then a `policy_version` row is INSERTed with non-null `signature` covering `rule_refs`, `threshold_refs`, `refactor_weights`
     And the Ed25519 signature verifies against the active signing key returned by `kms-mock`
     And no row is written to `policy_activation` (publish does NOT activate; activation is a separate verb -- arch Sec 5.3.3 callout "PolicyVersion is immutable")

  Scenario: policy_version columns are EXACTLY the seven canonical names
    When the suite queries `information_schema.columns` for `policy_version`
    Then the column set is exactly `policy_version_id`, `name`, `rule_refs`, `threshold_refs`, `refactor_weights`, `signature`, `created_at`
     And no column named `signature_alg`, `kid`, `status`, `activated_at`, `superseded_at`, or `expires_at` exists
     And no UPDATE is ever issued against `policy_version` (the row is immutable per G5 -- arch Sec 5.3.3)
```

```gherkin
@happy @invariant
Feature: policy.activate is an append to PolicyActivation [arch Sec 5.3.4]
  Scenario: Activation appends a row; latest row by created_at wins
    Given `policy.publish(D1)` was previously called, producing `policy_version_id=D1`
      And the current active PolicyVersion (latest `policy_activation` row) is `D0`
    When the operator calls `policy.activate(policy_version_id=D1)`
    Then a NEW `policy_activation` row is INSERTed with `policy_version_id=D1`, `activated_by` non-null, `created_at=NOW()`
     And no existing `policy_activation` row was UPDATEd or DELETEd
     And `policy_version` rows for `D0` and `D1` are byte-identical to their pre-activation state (no `status` column to flip; activation does NOT mutate PolicyVersion)
     And the current active PolicyVersion (latest row in `policy_activation` by `created_at`) is now `D1`

  Scenario: Re-activating an older PolicyVersion is also an append
    When the operator calls `policy.activate(policy_version_id=D0)` (reverting to the previously-active version)
    Then a new `policy_activation` row is INSERTed with `policy_version_id=D0`
     And the latest row by `created_at` is now `D0` again
     And no prior `policy_activation` row was UPDATEd

  Scenario: policy_activation columns are EXACTLY the four canonical names
    When the suite queries `information_schema.columns` for `policy_activation`
    Then the column set is exactly `activation_id`, `policy_version_id`, `activated_by`, `created_at`
     And no column named `target_time`, `valid_from`, `valid_until`, `superseded_at`, or `status` exists
```

```gherkin
@happy @security
Feature: Policy signing-key rotation overlap is 86400s (24h) [tech-spec Sec 8.2 `policy_publish_overlap_min_seconds=86400`, implementation-plan Stage 5.1]
  Scenario: Old key remains valid for 24h after rotation
    Given a signing-key rotation occurred at T0 producing a new key `K2` while `K1` is still cached as active
    When a `policy.publish` payload signed by `K1` arrives at T0 + 23h 59min
    Then the evaluator (and steward) verifies the signature successfully
     And the published `policy_version.signature` is accepted

  Scenario: Old key fails verification 24h+epsilon after rotation
    When a payload signed by `K1` arrives at T0 + 24h + 1s
    Then the steward rejects the publish call with an unverified-signature error
     And no `policy_version` row is INSERTed
     And an `eval.gate` invocation observing a policy still signed with `K1` at this point surfaces `verdict='warn'`, `degraded=true`, `degraded_reason='policy_signature_invalid'`
```

```gherkin
@invariant
Feature: There is no `policy.override` verb [tech-spec C13]
  Scenario: Server rejects an attempted call to `policy.override`
    When the suite calls `POST /policy/override`
    Then the server responds 404 Not Found (verb does not exist)
     And no row is written to any policy table
```

```gherkin
@happy
Feature: policy.publish_rulepack registers a SOLID rule pack [arch Sec 5.3.1, 5.3.2, implementation-plan Stage 5.2]
  Scenario: A valid rule pack is signed and stored, append-only
    Given a rule pack `R1` containing rules `srp.lcom4_threshold`, `dip.fan_in_high`, `isp.interface_width_max`
      And the request payload is signed with the active Ed25519 key
    When the operator calls `policy.publish_rulepack(R1)`
    Then `rule_pack` and `rule` rows are INSERTed (append-only per arch Sec 5.3.1 / 5.3.2)
     And the steward refuses unsigned payloads (implementation-plan Stage 5.2: "All three verbs require a valid signing key; refuse unsigned payloads")
     And no row in `rule_pack` or `rule` is UPDATEd
     And the rule pack participates in PolicyVersion bundles by being referenced from `policy_version.rule_refs`
```

```gherkin
@happy
Feature: Rule engine emits one verdict + N findings per evaluation [arch Sec 3.7, Sec 5.4.2]
  Scenario: A batch-refresh evaluation transaction writes exactly the expected rows
    Given an active PolicyVersion `P1` (latest `policy_activation` row) references rule pack `R1`
      And a metric snapshot for `repo-a` SHA `aaaa1111` violates `srp.lcom4_threshold` on scope `S1` and `S2`
    When the rule engine evaluates the snapshot against `P1` under the batch-refresh job
    Then exactly one `evaluation_run` row is INSERTed with `caller='batch_refresh'` (the EvaluationRun.caller enum is `eval_gate | batch_refresh` per arch Sec 5.4.2 -- never `rule_engine`)
     And exactly one `evaluation_verdict` row is INSERTed whose `evaluation_run_id` FK references the run (arch Sec 5.4.3 line 1208)
     And exactly two `finding` rows are INSERTed (one per violating scope), each whose `evaluation_run_id` FK references the SAME `evaluation_run` row (arch Sec 5.4.1 line 1179 -- Finding FKs the EvaluationRun, NOT the EvaluationVerdict)
     And NO `finding` row has any column named `verdict_id` (Finding does not FK the verdict; the run is the join key)
     And all writes complete in a single transaction (suite verifies via Postgres txn id)

  Scenario: EvaluationRun.caller enum is exactly two values
    When the suite queries the `evaluation_run_caller` enum labels
    Then the labels are exactly `eval_gate`, `batch_refresh`
     And no label named `rule_engine`, `wal_reconciler`, `policy_steward`, or `mgmt_surface` exists
```

```gherkin
@invariant
Feature: evaluation_verdict has `created_at` not `settled_at`, and no `scope` column [arch Sec 5.4.1]
  Scenario: The columns are exactly as locked
    When the suite queries `information_schema.columns` for `evaluation_verdict`
    Then a column named `created_at` exists
     And no column named `settled_at` exists
     And no column named `scope` exists
```

```gherkin
@invariant
Feature: Finding.delta is one of the four canonical values
  Scenario Outline: <delta> is produced when conditions match
    Given the prior verdict on the same scope was <prior>
      And the current evaluation result on the same scope is <current>
    When the rule engine emits a finding
    Then `finding.delta='<delta>'`
    Examples:
      | prior   | current  | delta          |
      | absent  | violate  | new            |
      | pass    | violate  | newly_failing  |
      | violate | violate  | unchanged      |
      | violate | pass     | resolved       |
```

---

# Phase 6: Evaluator Surface and Management Surface

Implements `implementation-plan.md` Phase 6 (lines 570-644). Brings up
the `eval.gate` synchronous surface and the `mgmt.*` REST/JSON surface
behind OIDC. Validates the gate's two degraded short-circuits, the
warn-on-degraded operator pin, and the management surface's role
isolation.

### Setup
- **Type**: compose
- **Local**: `docker compose -f tests/e2e/phase-06-evaluator-mgmt/docker-compose.yml up -d --build` then `make test-phase-06`.
- **CI runner**: GitHub-hosted `ubuntu-latest`.
- **Secrets**: none (compose includes a `dex` OIDC issuer with a test realm; issuer URL exported as `CLEAN_CODE_OIDC_ISSUER`).
- **Pre-test bootstrap**: `make migrate-up`; `make seed-phase-06` (creates `repo-a` baseline + active PolicyVersion + active rule pack); `make tokens` mints OIDC bearer tokens for roles `dev`, `lead`, `auditor`.

Compose services at `tests/e2e/phase-06-evaluator-mgmt/docker-compose.yml`:
- `postgres` (Postgres 16; DSN -> `CLEAN_CODE_PG_URL`).
- `dex` (OIDC issuer; URL -> `CLEAN_CODE_OIDC_ISSUER`).
- `kms-mock` (Ed25519 signer; URL -> `CLEAN_CODE_KMS_URL`).
- `evaluator` (the `clean-code-evaluator` service, role `clean_code_evaluator`).
- `mgmt-surface` (the `clean-code-mgmt` REST/JSON service).
- `rule-engine` (from Phase 5).
- `otel-collector` (endpoint -> `CLEAN_CODE_OTEL_ENDPOINT`).

### Scenarios

```gherkin
Background:
  Given the compose stack is healthy
    And a PolicyVersion `P1` is active for `repo-a` at SHA `aaaa2222`
    And a baseline metric snapshot exists for `repo-a` SHA `aaaa2222`
```

```gherkin
@happy
Feature: eval.gate clean path delegates to the rule engine [arch Sec 3.7]
  Scenario: A request with all samples present invokes the engine
    Given every required metric_sample for `repo-a` SHA `aaaa2222` is present and fresh
    When the caller invokes `eval.gate(repo=repo-a, sha=aaaa2222)`
    Then the response carries `verdict in {pass|warn|block}` per rule engine output
     And exactly one `evaluation_run` row exists for this call with `caller='eval_gate'` (the eval.gate verb is the caller; the synchronous rule-engine delegation does NOT mint its own run row -- arch Sec 3.7, Sec 5.4.2)
     And exactly one `evaluation_verdict` row exists whose `evaluation_run_id` FK equals the run's id, with `degraded=false` (arch Sec 5.4.3 line 1208)
     And every `finding` row (N >= 0) carries the SAME `evaluation_run_id` FK to that run -- Finding references the EvaluationRun directly, not the EvaluationVerdict (arch Sec 5.4.1 line 1179)
     And no `finding` row has a column named `verdict_id`

  Scenario: Clean path latency SLO
    Given the above clean-path conditions
    When 1000 calls are issued back-to-back
    Then p50 <= 200 ms, p95 <= 800 ms, p99 <= 2000 ms (OTel histogram on `eval.gate.duration_ms`)
```

```gherkin
@degraded @invariant
Feature: eval.gate short-circuit for samples_pending [arch Sec 3.7, Sec 8.2]
  Scenario: Required samples are missing; gate writes its OWN run+verdict
    Given a required metric_sample is missing for scope `S1` at SHA `aaaa2222`
    When `eval.gate(repo=repo-a, sha=aaaa2222)` is invoked
    Then exactly one `evaluation_run` row is INSERTed with `caller='eval_gate'`
     And exactly one `evaluation_verdict` row is INSERTed with `verdict='warn'`, `degraded=true`, `degraded_reason='samples_pending'`
     And the verdict's `evaluation_run_id` FK to the run is non-null (arch Sec 5.4.3 line 1208)
     And ZERO `finding` rows are written
     And the rule engine was NOT invoked (suite verifies via OTel: no `rule_engine.evaluate` span)
```

```gherkin
@degraded @invariant
Feature: eval.gate short-circuit for policy_signature_invalid [arch Sec 3.7, Sec 8.2]
  Scenario: PolicyVersion signature fails Ed25519 verify; gate writes its OWN run+verdict
    Given `P1`'s signature was tampered with after publish
    When `eval.gate(repo=repo-a, sha=aaaa2222)` is invoked
    Then exactly one `evaluation_run` is INSERTed with `caller='eval_gate'`
     And exactly one `evaluation_verdict` is INSERTed with `verdict='warn'`, `degraded=true`, `degraded_reason='policy_signature_invalid'`
     And ZERO `finding` rows are written
     And the rule engine was NOT invoked
```

```gherkin
@invariant
Feature: eval.gate REJECTS percentile_stale [arch Sec 8.2]
  Scenario: Stale percentiles are an Insights-only signal
    Given the cross-repo aggregator marked the latest aggregate `percentile_stale`
    When `eval.gate(repo=repo-a, sha=aaaa2222)` is invoked
    Then the gate ignores `percentile_stale` (does NOT emit it on `evaluation_verdict.degraded_reason`)
     And the verdict reflects the underlying rule-engine result (clean path) or one of the other two degraded reasons
     And `mgmt.read.insights(repo=repo-a)` separately surfaces `percentile_stale` to the operator
```

```gherkin
@happy @security
Feature: mgmt.register_repo requires `lead` role [tech-spec Sec 8.5]
  Scenario: `lead` token can register a new repo
    Given the caller presents a bearer token with claim `role=lead`
    When the caller invokes `mgmt.register_repo(name='repo-z', scan_mode='embedded', language='go')`
    Then the server responds 200 OK with the new `repo_id`
     And a `repo` row exists and a `repo_event(kind='registered')` is appended

  Scenario: `dev` token is denied
    Given the caller presents a bearer token with claim `role=dev`
    When the caller invokes `mgmt.register_repo(...)`
    Then the server responds 403 Forbidden
     And no `repo` row is created
```

```gherkin
@happy
Feature: mgmt.set_mode emits a RepoEvent with past-tense kind [arch Sec 5.2.4]
  Scenario: Changing scan_mode appends RepoEvent(kind='mode_changed')
    Given `repo-a.scan_mode='embedded'`
    When a `lead` caller invokes `mgmt.set_mode(repo=repo-a, scan_mode='external')`
    Then `repo.scan_mode='external'`
     And a `repo_event` row is appended with `kind='mode_changed'`
```

```gherkin
@happy
Feature: mgmt.override creates an override without expires_at [arch Sec 5.5.3, tech-spec C16]
  Scenario: Override is created and is latest-row-wins
    When a `lead` caller invokes `mgmt.override(repo=repo-a, scope=S1, rule='srp.lcom4_threshold', mute=true, reason='legacy')`
    Then an `override` row is INSERTed with `mute=true`, `created_at=NOW()`
     And the row has no `expires_at` column
     And a subsequent `eval.gate` call returns `verdict='pass'` for the muted rule even if the metric still violates

  Scenario: Unmute is an append, not an update
    When the same caller invokes `mgmt.override(repo=repo-a, scope=S1, rule='srp.lcom4_threshold', mute=false, reason='reviewed')`
    Then a NEW `override` row is INSERTed (the prior row is not UPDATEd)
     And the latest row (by `created_at`) governs subsequent evaluations
```

```gherkin
@happy
Feature: mgmt.retract_sample triggers append-and-repoint flow [arch Sec 1.5.1]
  Scenario: Caller marks a sample as an outlier
    Given metric_sample `M9` is the active sample for tuple T
    When a `lead` caller invokes `mgmt.retract_sample(sample_id=M9, reason='instrumentation_bug')`
    Then a `metric_retraction(sample_id=M9)` row is appended
     And a fresh `metric_sample` row `M10` is INSERTed and pointed-at by `metric_sample_active`
```

```gherkin
@happy
Feature: mgmt.rescan enqueues a ScanRun [arch Sec 3.5]
  Scenario: Force a rescan of an already-scanned SHA
    Given `repo-a` SHA `aaaa2222` has `Commit.scan_status='scanned'`
    When a `lead` caller invokes `mgmt.rescan(repo=repo-a, sha=aaaa2222, kind='full')`
    Then a new `ScanRun` is created with `kind='full'`, `sha_binding='single'`, `status='running'`, `to_sha='aaaa2222'`
     And on completion the prior `metric_sample` rows are NOT updated (G3)
     And `metric_sample_active` is re-pointed at the latest samples
```

```gherkin
@security
Feature: Auditor role is read-only [arch Sec 1.5]
  Scenario: `auditor` can read but not write
    Given the caller presents a bearer token with claim `role=auditor`
    When the caller invokes `mgmt.read.findings(repo=repo-a, sha=aaaa2222)`
    Then the server responds 200 OK with the finding list

    When the caller invokes `mgmt.register_repo(...)`
    Then the server responds 403 Forbidden
```

---

# Phase 7: Cross-Repo Aggregator and Insights freshness

Implements `implementation-plan.md` Phase 7 (lines 646-700). Brings up
the `clean_code_xrepo_aggregator` service, the 15-minute aggregation
cadence, the 7 system-tier metric_kinds, the `xrepo_edges_unavailable`
degraded path, and the `percentile_stale` Insights-only signal.

### Setup
- **Type**: compose
- **Local**: `docker compose -f tests/e2e/phase-07-aggregator/docker-compose.yml up -d --build` then `make test-phase-07`.
- **CI runner**: GitHub-hosted `ubuntu-latest`.
- **Secrets**: none.
- **Pre-test bootstrap**: `make migrate-up`; `make seed-phase-07` loads 10 repos with cross-repo dependency edges.

Compose services at `tests/e2e/phase-07-aggregator/docker-compose.yml`:
- `postgres` (Postgres 16; DSN -> `CLEAN_CODE_PG_URL`).
- `xrepo-aggregator` (role `clean_code_xrepo_aggregator`).
- `mgmt-surface` (for `mgmt.read.insights`).
- `evaluator` (for cross-checking degraded behaviour against Phase 6).
- `otel-collector` (endpoint -> `CLEAN_CODE_OTEL_ENDPOINT`).

### Scenarios

```gherkin
Background:
  Given 10 repos with cross-repo dependency edges are loaded
    And foundation+ingested metric_sample rows exist for all 10
    And the aggregator's cadence is pinned to 15 minutes
```

```gherkin
@happy @invariant
Feature: System-tier produces exactly 7 metric_kinds [arch Sec 1.4.2]
  Scenario: One aggregator pass emits 7 distinct system metric_kinds
    When the aggregator runs once
    Then the new `metric_sample` rows have `pack='system'` and `source='derived'`
     And the distinct `metric_kind` values are exactly
        `xrepo_dep_depth`, `arch_debt_ratio`, `velocity_trend`,
        `arch_fitness`, `blast_radius`, `xservice_test_reliability`, `knowledge_index`
     And no rows are tagged `p50.system` or `p95.system`
```

```gherkin
@happy
Feature: Aggregator cadence is 15 minutes [tech-spec Sec 8.2]
  Scenario: Successive runs are 15 minutes apart
    When the suite observes two consecutive aggregator runs
    Then the timestamps differ by 15 minutes (+/- 30 seconds)
```

```gherkin
@degraded
Feature: xrepo_edges_unavailable degrades aggregates [arch Sec 8.2]
  Scenario: Missing dependency edges produce a degraded aggregate
    Given `repo-7` is missing its export of cross-repo edges
    When the aggregator runs
    Then aggregates that depend on `repo-7` are flagged degraded with `degraded_reason='xrepo_edges_unavailable'`
     And independent aggregates remain non-degraded
```

```gherkin
@degraded @invariant
Feature: percentile_stale is Insights-only [arch Sec 8.2]
  Scenario: Stale percentiles surface via mgmt.read.insights
    Given the most recent aggregation is older than the configured percentile freshness threshold
    When `mgmt.read.insights(repo=repo-1)` is called
    Then the response includes `degraded=true` with `degraded_reason='percentile_stale'`
     And `eval.gate(repo=repo-1, sha=...)` does NOT propagate `percentile_stale` to its verdict's `degraded_reason`
     (instead the gate either runs clean or surfaces one of the other three degraded_reasons if applicable)
```

```gherkin
@invariant
Feature: Aggregator role cannot write the base pack [tech-spec C5]
  Scenario: Aggregator INSERT with pack='base' is denied
    Given the aggregator service is bound to role `clean_code_xrepo_aggregator`
    When it attempts `INSERT INTO metric_sample (..., pack) VALUES (..., 'base')`
    Then Postgres returns a denial
     And no row is persisted
```

---

# Phase 8: Refactor Planner

Implements `implementation-plan.md` Phase 8 (lines 702-756). Validates
the refactor planner's hot-spot rollup, plan/task generation, the
operator-pinned effort-source ("ML model from historical commits"), and
the absence of disallowed columns (`refactor_task.status`,
`refactor_task.expected_metric_delta`,
`refactor_plan.policy_version_id`).

### Setup
- **Type**: compose
- **Local**: `docker compose -f tests/e2e/phase-08-refactor-planner/docker-compose.yml up -d --build` then `make test-phase-08`.
- **CI runner**: GitHub-hosted `ubuntu-latest`.
- **Secrets**: none.
- **Pre-test bootstrap**: `make migrate-up`; `make seed-phase-08` seeds findings + system aggregates for `repo-a`.

Compose services at `tests/e2e/phase-08-refactor-planner/docker-compose.yml`:
- `postgres` (Postgres 16; DSN -> `CLEAN_CODE_PG_URL`).
- `refactor-planner` (the `clean-code-refactor-planner` service).
- `mgmt-surface` (for read APIs).
- `otel-collector` (endpoint -> `CLEAN_CODE_OTEL_ENDPOINT`).

### Scenarios

```gherkin
Background:
  Given findings and system-tier aggregates exist for `repo-a` SHA `aaaa3333`
    And an active PolicyVersion `P1` covers the findings
```

```gherkin
@happy @invariant
Feature: refactor_task.kind is one of five canonical values [arch Sec 5.5.3]
  Scenario: A generated plan emits tasks within the closed set
    When the planner runs against the seeded data
    Then every `refactor_task.kind` is in `split_class`, `extract_method`, `invert_dependency`, `break_cycle`, `consolidate_duplication`
     And no task has `kind='extract_function'`, `kind='reduce_lcom'`, or `kind='introduce_interface'`
```

```gherkin
@happy
Feature: hot_spot carries the policy_version_id [arch Sec 5.5.1]
  Scenario: Each hot_spot row references its policy_version
    When the planner runs
    Then every `hot_spot` row has a non-null `policy_version_id` referencing `P1`
     And `refactor_plan` has NO column named `policy_version_id`
```

```gherkin
@invariant
Feature: Effort estimation traces task -> plan -> hotspot -> policy_version -> effort_model_version [arch Sec 5.5.2, Sec 5.5.3]
  Scenario: A task carries an effort estimate joined back through hotspot via the canonical multi-hop path
    Given a `refactor_task` row `T1` exists with non-null `plan_id` referencing `RefactorPlan` row `P1`
      And `P1.hotspot_ids` is a JSON array (arch Sec 5.5.2 line 1235 -- the column is `hotspot_ids` JSON, NOT a scalar `plan.hot_spot_id`)
    When a `refactor_task` is emitted
    Then the suite dereferences `T1.plan_id -> P1`
     And the suite picks any single id `H_id` from the JSON array `P1.hotspot_ids` and resolves it to a `HotSpot` row `H1`
     And `H1.policy_version_id` resolves to PolicyVersion row `PV1`
     And the effort-model identifier lives inside the JSON path `PV1.refactor_weights.effort_model_version` (arch Sec 5.5.3 line 1247 -- the model version is a JSON field on `policy_version.refactor_weights`, NOT a top-level `policy_version.effort_model_version_id` column)
     And `policy_version` has NO column named `effort_model_version_id` (the model version is JSON-embedded, not a scalar column)
     And the effort source pin is satisfied (operator pin: `ML model from historical commits`)
```

```gherkin
@invariant
Feature: refactor_task has no status and no expected_metric_delta
  Scenario: The DDL omits both columns (re-asserted at integration level)
    When the suite queries `information_schema.columns` for `refactor_task`
    Then no column named `status` exists
     And no column named `expected_metric_delta` exists

  Scenario: Planner output contains no client-side status field either
    When the planner emits a plan via `mgmt.read.refactor_plan(plan_id)`
    Then the JSON response has no `status` field on any task
     And no `expected_metric_delta` field on any task
```

```gherkin
@perf
Feature: Planner regeneration SLO [tech-spec Sec 8.3]
  Scenario: A planner regeneration finishes within 5 minutes for the seed corpus
    When the planner regenerates plans for the 50-hotspot seed corpus
    Then total wall time is <= 5 minutes (p50)
     And the OTel span `refactor_planner.regenerate` reports `hot_spots_processed=50`
```

---

# Phase 9: Audit WAL and reliability hardening

Implements `implementation-plan.md` Phase 9 (lines 758-823). Validates
the audit WAL replay semantics (`evaluation_run | evaluation_verdict |
finding` only), the REPLAY-ONLY reconciler (`clean_code_wal_reconciler`
role preserves the original `caller`), and that
Catalog/Measurement/Policy/Refactor do NOT route through the WAL.

### Setup
- **Type**: compose
- **Local**: `docker compose -f tests/e2e/phase-09-audit-wal/docker-compose.yml up -d --build` then `make test-phase-09`.
- **CI runner**: GitHub-hosted `ubuntu-latest`.
- **Secrets**: none.
- **Pre-test bootstrap**: `make migrate-up`; `make seed-phase-09` (a baseline metric snapshot + active PolicyVersion).

Compose services at `tests/e2e/phase-09-audit-wal/docker-compose.yml`:
- `postgres` (Postgres 16; DSN -> `CLEAN_CODE_PG_URL`).
- `evaluator` (role `clean_code_evaluator`, also writes to the WAL stream).
- `rule-engine` (role `clean_code_solid_batch`).
- `wal-reconciler` (role `clean_code_wal_reconciler`, REPLAY-ONLY).
- `fault-injector` (a sidecar that can sever Postgres connectivity from evaluator/rule-engine).
- `otel-collector` (endpoint -> `CLEAN_CODE_OTEL_ENDPOINT`).

### Scenarios

```gherkin
Background:
  Given the compose stack is healthy
    And an active PolicyVersion `P1` covers `repo-a` SHA `aaaa4444`
    And a baseline metric snapshot exists for that (repo,sha)
```

```gherkin
@happy @invariant
Feature: WAL scope is exactly three tables [arch Sec 1.5, tech-spec C24]
  Scenario: Reconciler may write only to evaluation_run, evaluation_verdict, finding
    When the suite enumerates tables for which `clean_code_wal_reconciler` has INSERT
    Then the result is exactly `evaluation_run`, `evaluation_verdict`, `finding`
     And no `INSERT` privilege exists on `metric_sample`, `policy_version`, `repo`, `repo_event`, `override`, `hot_spot`, `refactor_plan`, `refactor_task`

  Scenario: Reconciler may NOT UPDATE or DELETE
    When the suite attempts `UPDATE evaluation_verdict SET verdict='pass' WHERE ...` as `clean_code_wal_reconciler`
    Then Postgres returns a denial

    When the suite attempts `DELETE FROM evaluation_run` as `clean_code_wal_reconciler`
    Then Postgres returns a denial
```

```gherkin
@happy @invariant
Feature: Reconciler preserves the original caller [arch Sec 1.5.1]
  Scenario: Replay of an evaluator-originated run preserves caller='eval_gate'
    Given an `eval.gate` short-circuit produced a WAL entry with `caller='eval_gate'`
      And the in-memory write was lost before commit (simulated via `fault-injector`)
    When the reconciler replays the WAL entry
    Then a single `evaluation_run` row is materialised with `caller='eval_gate'` (NOT `caller='wal_reconciler'`)
     And the matching `evaluation_verdict` is materialised with the original `degraded` and `degraded_reason` preserved
```

```gherkin
@happy
Feature: Catalog/Measurement/Policy/Refactor do NOT route through the WAL [arch Sec 1.5, tech-spec C24]
  Scenario: A normal mgmt.register_repo emits no WAL entry
    When a `lead` caller invokes `mgmt.register_repo(name='repo-x', ...)`
    Then no WAL entry is produced
     And the `repo` row is INSERTed directly by the management surface

  Scenario: A normal policy.publish emits no WAL entry
    When the operator invokes `policy.publish(D1)`
    Then no WAL entry is produced
     And the `policy_version` row is INSERTed directly by the policy steward
```

```gherkin
@edge
Feature: Crash during eval.gate clean path -> reconciler completes the trio
  Scenario: Engine commits crash; reconciler materialises run+verdict+findings
    Given the evaluator was mid-transaction when `fault-injector` severed its DB connection
      And the WAL entry was durably persisted before the crash
    When `wal-reconciler` runs
    Then exactly one `evaluation_run`, one `evaluation_verdict`, and N `finding` rows exist
     And no duplicate run/verdict exists after a subsequent reconciler pass (idempotent replay)
```

```gherkin
@edge
Feature: Reconciler is idempotent across repeated replays [tech-spec C24]
  Scenario: Running the reconciler twice over the same WAL has no extra side effects
    Given a WAL entry that has already been materialised
    When the reconciler is run again over the same WAL window
    Then no new rows are INSERTed into evaluation_run / evaluation_verdict / finding
     And no UPDATE or DELETE is issued
```

---

# Phase 10: Linked-mode integration and cross-repo e2e

Implements `implementation-plan.md` Phase 10 (lines 825-909) but
focuses specifically on integration with the sister
`code-intelligence:AGENT-MEMORY` service via
`AGENT_MEMORY_GRPC_ENDPOINT`. Validates the linked-mode population of
`scope_binding.agent_memory_node_id` and the side-by-side, no-data-loss
deployment topology.

### Setup
- **Type**: compose
- **Local**: `docker compose -f tests/e2e/phase-10-linked-mode/docker-compose.yml up -d --build` then `make test-phase-10`.
- **CI runner**: GitHub-hosted `ubuntu-latest`.
- **Secrets**: none.
- **Pre-test bootstrap**: `make migrate-up` (both `clean_code` and `agent_memory` schemas); `make seed-phase-10`.

Compose services at `tests/e2e/phase-10-linked-mode/docker-compose.yml`:
- `postgres` (Postgres 16; DSN -> `CLEAN_CODE_PG_URL`; same instance hosts `agent_memory` schema for the linked-mode test).
- `agent-memory` (the sister story's binary; gRPC endpoint -> `AGENT_MEMORY_GRPC_ENDPOINT`).
- `evaluator`, `indexer`, `ingestor`, `mgmt-surface` (the same clean-code services from previous phases).
- `kms-mock` (URL -> `CLEAN_CODE_KMS_URL`).
- `otel-collector` (endpoint -> `CLEAN_CODE_OTEL_ENDPOINT`).

### Scenarios

```gherkin
Background:
  Given both stacks are healthy
    And `repo-a` is registered in the clean-code service AND in the agent-memory service
```

```gherkin
@happy
Feature: scope_binding.agent_memory_node_id is populated only in linked mode [arch Sec 5.1]
  Scenario: Linked mode is on; scope_binding rows carry the FK
    Given the deployment is in linked mode (env `CLEAN_CODE_LINKED_MODE=true`)
    When the indexer creates a new `scope_binding` row for class `S1`
    Then `scope_binding.agent_memory_node_id` is non-null and resolves via gRPC to an agent-memory node

  Scenario: Linked mode is off; column is NULL
    Given the deployment is in standalone mode (env `CLEAN_CODE_LINKED_MODE=false`)
    When the indexer creates a new `scope_binding` row
    Then `scope_binding.agent_memory_node_id IS NULL`
```

```gherkin
@happy
Feature: Cross-service evaluation enriches findings with agent-memory context
  Scenario: A finding's response embeds an agent-memory node reference when linked
    Given linked mode is on
      And the rule engine emits a finding on scope `S1`
    When `mgmt.read.findings(repo=repo-a, sha=aaaa5555)` is invoked
    Then each finding includes a `links.agent_memory_node_id` field
     And the suite can resolve that ID over `AGENT_MEMORY_GRPC_ENDPOINT` to a valid node
```

```gherkin
@edge @degraded
Feature: AGENT_MEMORY_GRPC_ENDPOINT down does NOT degrade eval.gate
  Scenario: Linked-mode call survives sister-service unavailability
    Given linked mode is on
      And the `agent-memory` container is down
    When `eval.gate(repo=repo-a, sha=aaaa5555)` is invoked
    Then the gate completes normally (NOT degraded -- linked-mode enrichment is best-effort)
     And the returned findings have `links.agent_memory_node_id IS NULL`
     And the OTel span records `linked_mode.enrichment_skipped=true`
```

```gherkin
@invariant
Feature: Standalone-mode rollback leaves no orphan FK [arch Sec 1.7]
  Scenario: Switching linked -> standalone preserves existing rows
    Given existing `scope_binding` rows have non-null `agent_memory_node_id`
    When the operator restarts the deployment with `CLEAN_CODE_LINKED_MODE=false`
    Then the existing rows are NOT modified (FK values are retained)
     And new `scope_binding` inserts have `agent_memory_node_id IS NULL`
```

---

# Phase 11: Load and SLO conformance

Validates the SLO targets from `tech-spec.md` Sec 8.3 at production
scale (100 repos x 50 scans/min x 30 min) on dedicated hardware. This
is the only `lab-*` phase -- compose cannot produce predictable
percentile measurements at the required load.

### Setup
- **Type**: lab-bare-metal
- **Local**: NOT supported. (Developers may run a scaled-down profile via `docker compose -f tests/e2e/phase-07-aggregator/docker-compose.yml` for smoke checks, but SLO numbers are NOT collected locally.)
- **CI runner**: ADO agent pool `forge-lab-hw`; GitHub self-hosted runner labels `[self-hosted, linux, clean-code-perf]`.
- **Secrets**:
  - Azure Key Vault: `kv-forge-shared`
    - `kv-forge-shared/clean-code-perf-pgsql-superuser`
    - `kv-forge-shared/clean-code-perf-kms-token`
    - `kv-forge-shared/clean-code-perf-oidc-client-secret`
  - ADO variable group: `forge-clean-code-perf-vg` (pulls from KV above).
  - GitHub environment: `forge-lab-clean-code-perf`
    - Secret `CLEAN_CODE_PERF_PG_PASSWORD`
    - Secret `CLEAN_CODE_PERF_KMS_TOKEN`
    - Secret `CLEAN_CODE_PERF_OIDC_CLIENT_SECRET`
  - Connection strings exposed to the test job as env-var names only:
    `CLEAN_CODE_PG_URL`, `CLEAN_CODE_KMS_URL`, `CLEAN_CODE_OIDC_ISSUER`,
    `CLEAN_CODE_WEBHOOK_HMAC_SECRET`, `CLEAN_CODE_OTEL_ENDPOINT`.
- **Pre-test bootstrap**: `make lab-bootstrap` (provisions the Postgres instance on the lab box and applies migrations); `make seed-phase-11` (loads 100 repos x 12 SHAs each from the perf fixture corpus, ~120k pre-existing metric_sample rows).

### Scenarios

```gherkin
Background:
  Given the lab box has 16 cores, 64 GB RAM, NVMe storage
    And Postgres is sized at shared_buffers=16 GB, max_connections=200
    And the OTel collector is co-located on a sibling node
    And the perf fixture corpus is loaded
```

```gherkin
@perf
Feature: eval.gate SLO targets [tech-spec Sec 8.3]
  Scenario: 30-minute steady-state load satisfies p50/p95/p99
    Given the load generator drives 100 repos x 50 scans/min x 30 min (=150,000 eval.gate calls)
    When the run completes
    Then `eval.gate.duration_ms` p50 <= 200, p95 <= 800, p99 <= 2000
     And error rate (non-2xx responses) is <= 0.1 percent
     And no degraded short-circuit fires due to infrastructure (compose-like) reasons
```

```gherkin
@perf
Feature: Ingestor SLO targets [tech-spec Sec 8.3]
  Scenario: Ingestor sustains 5,000 samples/sec for 30 minutes
    Given the load generator drives the ingestor at 5,000 samples/sec for 30 minutes (9,000,000 samples)
    When the run completes
    Then no caller-visible errors are returned
     And exactly 9,000,000 rows appear in `metric_sample` for the test corpus
     And the ingestor's p99 write latency is <= 25 ms
```

```gherkin
@perf
Feature: AST adapter throughput SLO at scale [tech-spec Sec 8.3]
  Scenario: 8 parallel workers sustain >=400k LOC/sec aggregate
    Given the AST adapter is configured with 8 worker threads
    When the adapter processes the perf fixture (8M LOC) end-to-end
    Then aggregate throughput is >= 400,000 LOC/sec (i.e. each worker >= 50k LOC/sec)
     And per-worker variance is within +/- 15 percent
```

```gherkin
@perf
Feature: Insights ranked-percentile SLO [tech-spec Sec 8.3]
  Scenario: mgmt.read.insights latency targets
    Given the seeded 100-repo corpus
    When 5,000 sequential `mgmt.read.insights(repo=...)` calls are issued
    Then p50 <= 100 ms, p95 <= 500 ms, p99 <= 1500 ms
     And the `percentile_stale` flag fires for <= 1 percent of calls
```

```gherkin
@perf
Feature: Refactor planner regeneration SLO at scale [tech-spec Sec 8.3]
  Scenario: Planner regenerates plans for 100 repos within 30 minutes
    Given each of 100 repos carries >= 5 hotspots (>= 500 hotspots total)
    When the planner regenerates plans across the entire corpus
    Then total wall time is <= 30 minutes (p99 cohort completion)
     And per-repo p50 is <= 5 minutes
```

```gherkin
@perf @degraded
Feature: System remains within SLO under one degraded reason at a time
  Scenario Outline: SLO holds when <reason> is injected for 10 percent of calls
    Given the fault injector marks 10 percent of `eval.gate` calls as `<reason>`
    When the 30-minute load run completes
    Then `eval.gate.duration_ms` p99 remains <= 2000 (degraded short-circuit is cheaper than the engine path)
     And the verdict distribution shows `warn` rate >= 10 percent (the injected ones)
     And no `degraded_reason='percentile_stale'` appears on any `evaluation_verdict` row
    Examples:
      | reason                       |
      | xrepo_edges_unavailable      |
      | samples_pending              |
      | policy_signature_invalid     |
```

```gherkin
@perf @security
Feature: Signing throughput does not bottleneck publishes
  Scenario: 100 sequential policy.publish calls satisfy p99 <= 1s
    When the load generator drives 100 sequential `policy.publish` calls signed via `kms-mock`
    Then p50 <= 250 ms and p99 <= 1000 ms
     And every persisted `policy_version.signature` verifies with the active signing key (the kms-mock's key-id label is matched against the steward's signing-key cache; there is no `kid` column on `policy_version`)
```

---

# Appendix A: Cross-phase canon-guard checklist

This appendix is the QA short-list. Every item must hold on every
phase that touches the corresponding sub-store. The detailed assertions
above implement the checklist; this list is what an operator scans
when triaging an evaluator failure.

- Single `clean_code` schema (no `catalog.`/`measurement.`/`policy.`/`audit.`/`refactor.`).
- The 22 metric_kinds are the only ones in the enum (12 foundation + 3 ingested + 7 system).
- Foundation tier is split: 6 rows are `pack='base'` (`cyclo`, `cognitive_complexity`, `loc`, `cycle_member`, `duplication_ratio`, `modification_count_in_window`) and 6 rows are `pack='solid'` (`lcom4`, `fan_in`, `fan_out`, `depth_of_inheritance`, `interface_width`, `coupling_between_objects`).
- MetricSample.source enum is exactly `computed | ingested | derived` -- never `ast` or `external`. AST-emitted rows are `source='computed'`; webhook-emitted rows are `source='ingested'`; aggregator-emitted rows are `source='derived'`.
- `scope_kind` is exactly 7 values; `verdict` is exactly 3; `delta` is exactly 4.
- `ScanRun.kind` is exactly `full | delta | external_single | external_per_row | retract`; `ScanRun.sha_binding` is exactly `single | per_row`. There is NO `ScanRun.mode` column.
- `RepoEvent.kind` is past-tense (`registered | retired | retract_intent | mode_changed`).
- `MetricSample` is append-only (G3); `metric_sample_active` is a side relation whose PRIMARY KEY is EXACTLY the quintuple `(repo_id, sha, scope_id, metric_kind, metric_version)` with `sample_id` carried as a NOT NULL FK column under a SEPARATE `metric_sample_active_sample_id_uniq` UNIQUE INDEX (tech-spec Sec 7.1.b lines 1108-1119, implementation-plan Stage 1.3 line 90); DELETE is REVOKEd from BOTH writer roles.
- Two-writer carve-out on Measurement (`ingestor` writes `base|solid|ingested`, `xrepo_aggregator` writes `system AND source=derived`).
- Three-writer ACL on Audit (`evaluator`, `solid_batch`, `wal_reconciler` get INSERT+SELECT; UPDATE/DELETE revoked).
- `Override` has NO `expires_at`; unmute is an APPEND.
- `refactor_task` has NO `status` and NO `expected_metric_delta`; `hot_spot.policy_version_id` exists but `refactor_plan.policy_version_id` does NOT.
- `eval.gate` short-circuits ONLY for `samples_pending` and `policy_signature_invalid`. `xrepo_edges_unavailable` flows through the engine. `percentile_stale` is INSIGHTS-ONLY -- `eval.gate` rejects it.
- `EvaluationRun.caller` enum is exactly `eval_gate | batch_refresh` -- never `rule_engine`, `wal_reconciler`, `policy_steward`, or `mgmt_surface`.
- WAL covers ONLY `evaluation_run | evaluation_verdict | finding`. Reconciler is REPLAY-ONLY, preserves original `caller`.
- No invented tables (`audit_event`, `audit_anchor`, `effort_estimate`, `rule_pack_revision`, `policy_override`, `swap_active`).
- v1 single-tenant: NO `tenant_id` columns. v1 languages: Go, Python, TypeScript, Java only -- Registry refuses C#/Rust.
- `PolicyVersion` is immutable (G5) with columns EXACTLY `policy_version_id`, `name`, `rule_refs`, `threshold_refs`, `refactor_weights`, `signature`, `created_at` -- no `signature_alg`, `kid`, `status`, `activated_at`, or `superseded_at`. Activation is recorded by appending a `PolicyActivation(activation_id, policy_version_id, activated_by, created_at)` row; the latest row by `created_at` is the active version.
- Signing-key rotation overlap is 86400s (24h) per `policy_publish_overlap_min_seconds`. Two signing keys may co-exist during overlap; both verify successfully.
- Cobertura XML is the only `ingest.coverage` payload (operator pin `external-metric-coverage-format=Cobertura XML`).
- `ingest.churn` writes ZERO `metric_sample` rows directly; `ingest.defects` is store-only in v1.

---

# Appendix B: Open assumptions for the next iteration

- **Lab pool for Phase 11**: pinned `forge-lab-hw` as the ADO pool and `[self-hosted, linux, clean-code-perf]` as the GH self-hosted labels. If the operator wants compose-only SLO measurement (acknowledging the precision penalty), this would collapse Phase 11 into a `compose` phase running on `ubuntu-latest-large`.
- **OIDC issuer in dev/CI**: pinned `dex` as the compose-local issuer for Phases 6+. If the operator prefers `keycloak` or a real Entra tenant for CI, the compose service swaps but the env-var name (`CLEAN_CODE_OIDC_ISSUER`) stays the same.
- **KMS surface for Phase 5**: pinned a `kms-mock` HTTP shim that exposes Ed25519 sign/verify. The shim wire format matches Azure Key Vault REST so the production swap is config-only. If the operator wants the lab phase to talk to a real Azure Key Vault, only `CLEAN_CODE_KMS_URL` and the KV-bound credentials change.

## Iteration Summary

- **Path written**: `docs/stories/code-intelligence-CLEAN-CODE/e2e-scenarios.md`
- **Iteration**: 4 (file existed pre-edit at 85,019 bytes; iter-4 edits brought it to 89,096 bytes -- one structural fix to Phase 2 producer attribution).
- **Coverage matrix**: 11 phases, each with `### Setup` (Type/Local/CI runner/Secrets/Pre-test bootstrap) followed by `### Scenarios` (Background + Gherkin features). Phase 11 is `lab-bare-metal` with BOTH Key Vault path AND GH environment named.
- **Connection strings**: ALL referenced as env-var names (`CLEAN_CODE_PG_URL`, `CLEAN_CODE_KMS_URL`, `CLEAN_CODE_OIDC_ISSUER`, `CLEAN_CODE_WEBHOOK_HMAC_SECRET`, `CLEAN_CODE_OTEL_ENDPOINT`, `AGENT_MEMORY_GRPC_ENDPOINT`).

### Prior feedback resolution

Iter-3 evaluator scored 88/iterate with 1 numbered issue. It is addressed in iter 4:

- [x] 1. FIXED -- Phase 2 producer attribution for `modification_count_in_window` -- STRUCTURAL split (not a word-tweak). Three coordinated changes:
  - (a) Phase 2 intro (lines 321-336): rewrote to say AST adapter emits **11 of the 12** foundation kinds, with `modification_count_in_window` produced separately by the Metric Ingestor's `modification_count` materialiser (`internal/metrics/materialisers/modification_count.go`) consuming `ingest.churn` rows. Updated the `Local` and `Pre-test bootstrap` lines to add `./internal/metrics/materialisers/...` and `make fixtures-churn-synth`.
  - (b) Foundation-kinds feature (lines 345-360): split into two Features. The first now asserts the AST adapter emits **exactly 11** AST-derived kinds and explicitly excludes `modification_count_in_window` (with arch/tech-spec citations); the `pack='base'` row count drops from 6 to 5 because the 6th `pack='base'` kind is materialiser-owned. The second is a brand-new Feature "Metric Ingestor materialiser emits `modification_count_in_window` from churn (NOT the AST adapter)" with three scenarios: (i) synthetic-churn-stream emits exactly one row with `pack='base', source='computed'` and `attrs_json.provenance='ingested'`; (ii) out-of-window rows are ignored; (iii) AST-only run (no churn fed) produces zero rows for this kind and the producer-attribution registry maps the kind to `modification_count_materialiser`, NOT to any AST language analyzer.
  - (c) Window-default feature (formerly lines 396-403): rewrote so the `window_days=90` assertion fires on the materialiser config, not on "the adapter computes", and adds an explicit canon-guard "AST adapter does NOT read or honour the `window_days` config".

  Aligns with arch Sec 1.4.1 row 12 line 105, tech-spec Sec 4.1.1 lines 287-291, implementation-plan Stage 2.6 lines 247-261. Verification:

```
$ grep -nF "emits the 12 foundation `metric_sample` rows per scope" docs\stories\code-intelligence-CLEAN-CODE\e2e-scenarios.md
(empty)
$ grep -nF "all 12 foundation metric_kinds" docs\stories\code-intelligence-CLEAN-CODE\e2e-scenarios.md
(empty)
$ grep -nF "exactly the 12 names" docs\stories\code-intelligence-CLEAN-CODE\e2e-scenarios.md
(empty)
$ grep -nF "When the adapter computes `modification_count_in_window`" docs\stories\code-intelligence-CLEAN-CODE\e2e-scenarios.md
(empty)
$ grep -nF "AST adapter is NOT a producer" docs\stories\code-intelligence-CLEAN-CODE\e2e-scenarios.md
365:     And NO row has `metric_kind='modification_count_in_window'` (...; the AST adapter is NOT a producer of this kind)
391:  Scenario: AST adapter is NOT a producer of `modification_count_in_window` (canon-guard, no churn fed)
$ grep -nF "modification_count_materialiser" docs\stories\code-intelligence-CLEAN-CODE\e2e-scenarios.md
396:     And the registry maps `modification_count_in_window` to the writer identity `modification_count_materialiser`, NOT to any AST language analyzer
```

Phase 4 was already canon-correct on `ingest.churn` (line ~618 says it writes zero `metric_sample` directly and feeds the materialiser); no Phase 4 edits required.

### Earlier iterations (re-confirmed clean in iter 4)

- [x] iter-1 item 1: Postgres 16 -- still empty on `grep -nF "Postgres 15"`.
- [x] iter-1 item 2: Foundation 6+6 base/solid split with `source='computed'` -- iter-4 narrowed the AST-emitted set to 11 and added a separate materialiser feature for the 12th, preserving the 6+6 pack totals at the system level.
- [x] iter-1 item 3: Ingested `pack='ingested', source='ingested'` -- only Appendix-A negation hit remains.
- [x] iter-1 item 4: ScanRun.kind + sha_binding -- only Appendix-A negation hit remains.
- [x] iter-1 item 5: PolicyVersion 7-column / PolicyActivation append-only / 86400s key-rotation overlap -- clean.
- [x] iter-1 item 6: EvaluationRun.caller enum -- clean.
- [x] iter-2 item 1: `metric_sample_active` PK on quintuple with separate `metric_sample_active_sample_id_uniq` -- clean.
- [x] iter-2 item 2: Finding FKs EvaluationRun via `evaluation_run_id` (NOT verdict) -- clean.
- [x] iter-2 item 3: Refactor effort multi-hop trace through `hotspot_ids` JSON array and `refactor_weights.effort_model_version` JSON field -- clean.
- [x] iter-2 item 4: Refactor anchors cite `arch Sec 5.5.1-5.5.3` -- clean.

### Story-description coverage
- "decoupled functional areas" -- Phases 1/3/5/7/9 (writer carve-outs, audit-WAL scoping).
- "SOLID coding principal" -- Phase 5 (srp.lcom4_threshold, dip.fan_in_high, isp.interface_width_max).
- "system level metrics for big repo, and cross-repo measurements" -- Phase 2 (11 AST-derived foundation kinds + the materialiser-emitted `modification_count_in_window`, totalling 12 foundation rows) + Phase 7 (7 system-tier metrics) + Phase 11 (100-repo scale).
- "used by evaluator agent to guard bad practices" -- Phase 6 (eval.gate clean + degraded short-circuits) + Phase 7 (percentile_stale Insights-only).
- "metrics for refactoring effort" -- Phase 8 (planner with multi-hop `RefactorPlan.hotspot_ids` JSON array -> `HotSpot.policy_version_id` -> `PolicyVersion.refactor_weights.effort_model_version` JSON field).

### Open questions for next iteration
None. All operator pins from prior iterations remain locked in the sibling docs.
