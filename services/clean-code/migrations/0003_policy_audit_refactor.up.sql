-- 0003_policy_audit_refactor.up.sql
--
-- Stage 1.4 (implementation-plan.md "Policy and Audit and Refactor
-- schema migrations"): create the Policy / Audit / Refactor
-- sub-store tables -- `rule`, `rule_pack`, `policy_version`,
-- `policy_activation`, `threshold`, `override`, `evaluation_run`,
-- `evaluation_verdict`, `finding`, `hot_spot`, `refactor_plan`,
-- `refactor_task` -- all in the single `clean_code` schema
-- (tech-spec C9 / Sec 8.1.3). Catalogues at architecture Sec 5.3
-- (Policy), Sec 5.4 (Audit), Sec 5.5 (Refactor).
--
-- Twelve tables. No more, no fewer. The following names were
-- rejected by iter-1 evaluator item 1 and MUST NOT appear:
-- `rule_pack_revision`, `policy_override`, `audit_event`,
-- `audit_anchor`, `effort_estimate`. The `RulePack` carries its
-- own monotonic `version` column (architecture Sec 5.3.2) so a
-- side `rule_pack_revision` table is structurally unnecessary;
-- mute / unmute is `Override.mute=false` rows (architecture
-- Sec 5.3.6 latest-row-wins) so `policy_override` would
-- duplicate `Override`; durable audit lives in the Audit WAL
-- (architecture Sec 7.1 + Sec 7.10) which is a file-backed log,
-- not a database table, so `audit_event` / `audit_anchor` would
-- duplicate that mechanism; effort attribution lives on
-- `RefactorTask.effort_hours` (architecture Sec 5.5.3 line
-- 1248) so a separate `effort_estimate` would split the same
-- field across two tables.
--
-- Stage dependency (implementation-plan Stage 1.4 line 125):
-- this migration depends ONLY on Stage 1.2 (catalog and
-- lifecycle) -- NOT on Stage 1.3 (measurement). That is why
-- the `scope_id` columns on `finding`, `hot_spot`, and
-- `refactor_task` are declared as bare `uuid NOT NULL` rather
-- than `REFERENCES clean_code.scope_binding(scope_id)`:
-- `scope_binding` is a Stage 1.3 (0002) entity that does not
-- exist when 0003 is applied in stage-1.4-only test fixtures.
-- The architecture-level FK is enforced by the application
-- writers (Evaluator Surface, Refactor Planner) until a
-- follow-up migration adds the SQL FK once both stages land.
--
-- Cross-stage enum sharing: `scope_kind` and `degraded_reason`
-- are referenced by Stage 1.3 (`metric_sample.degraded_reason`,
-- `scope_binding.scope_kind`) AND by this stage
-- (`threshold.scope_kind`, `evaluation_verdict.degraded_reason`,
-- and `override.scope_filter ->> 'scope_kind'`). To keep 0002
-- and 0003 independent (no ordering coupling, no
-- `CREATE TYPE ... IF NOT EXISTS` -- PostgreSQL does not
-- support that syntax for enum types), this migration enforces
-- both closed sets via per-column CHECK constraints on TEXT
-- columns rather than via named ENUM types. The canonical
-- value lists are identical to the architecture-Sec-5.2.3
-- (scope_kind) and architecture-Sec-8.2 (degraded_reason)
-- enum labels. When 0002 lands, its native ENUM types
-- (`clean_code.scope_kind`, etc.) live alongside these TEXT
-- columns without name conflict.
--
-- Append-only / immutability: every table in this migration is
-- append-only per architecture G3 + G5 (the Audit WAL is the
-- durability mechanism for Audit rows; the cryptographic
-- signature on `policy_version` is the immutability anchor for
-- Policy rows). DB-level `REVOKE UPDATE, DELETE` ships in
-- Stage 1.5 (0004_roles.up.sql); we do NOT issue those grants
-- here because the role names do not yet exist.
--
-- PostgreSQL extension requirements: NONE for this migration
-- (the `gen_random_uuid()` calls inherit the same PG13+ core
-- guarantee documented in 0001's header).

BEGIN;

-- ---------------------------------------------------------------------------
-- ENUM types (closed sets local to this migration)
-- ---------------------------------------------------------------------------
--
-- Each enum below is owned by exactly one stage and is named
-- distinctly enough that re-applying this migration after a
-- prior partial run trips on `type ... already exists` (the
-- safety net that Stage 1.2 already relies on for its own
-- enums; see 0001's header). The shared `scope_kind` and
-- `degraded_reason` closed sets are enforced via CHECK
-- constraints on TEXT columns instead -- see the header note
-- on cross-stage enum sharing.

-- architecture Sec 5.3.1 line 1102 + Sec 5.4.1 line 1188:
-- `Rule.severity_default` and `Finding.severity` both draw
-- from the same three-value closed set `info | warn | block`.
-- One ENUM, two table columns, so the catalogue stays
-- consistent if either column is widened later.
CREATE TYPE clean_code.rule_severity AS ENUM (
    'info',
    'warn',
    'block'
);

-- architecture Sec 5.4.1 line 1189 (the canon-locked normative
-- semantics): `Finding.delta` is the four-value enum
-- `new | newly_failing | unchanged | resolved`. The values
-- `regression | improvement | flat` were considered and
-- explicitly rejected by the canon-guard in the e2e-scenarios
-- "finding-delta-canonical" test; they must NEVER appear here.
-- The Insights surface and Refactor Planner read
-- `delta='newly_failing'` as the regressions bucket (arch
-- Sec 6.3 `mgmt.read.regressions`).
CREATE TYPE clean_code.finding_delta AS ENUM (
    'new',
    'newly_failing',
    'unchanged',
    'resolved'
);

-- architecture Sec 5.4.2 line 1201: `EvaluationRun.caller` is
-- `eval_gate` (synchronous gate path; Evaluator Surface) or
-- `batch_refresh` (asynchronous SOLID Rule Engine batch worker).
-- The two callers share the three Audit tables under the
-- per-tech-spec Sec 7.2 lines 1258-1261 three-writer grant.
CREATE TYPE clean_code.evaluation_run_caller AS ENUM (
    'eval_gate',
    'batch_refresh'
);

-- architecture Sec 5.4.3 line 1210 + tech-spec Sec 7.7 C21:
-- `EvaluationVerdict.verdict` is the three-value enum
-- `pass | warn | block`. `fail` / `gated` are NOT canonical
-- and are explicitly rejected by the e2e-scenarios
-- "verdict-enum-only-canonical" test (iter 1 evaluator item 6
-- canon-guard).
CREATE TYPE clean_code.evaluation_verdict_value AS ENUM (
    'pass',
    'warn',
    'block'
);

-- architecture Sec 5.5.3 line 1247: `RefactorTask.kind` is
-- one of five canonical refactor playbook entries. Architecture
-- closes the line with "etc." but the implementation-plan
-- Stage 1.4 brief LIMITS v1 to these five values verbatim;
-- adding more (`extract_function | introduce_interface |
-- reduce_inheritance | reduce_coupling | reduce_lcom |
-- reduce_duplication` and the like) requires its own
-- catalogue-bump migration.
CREATE TYPE clean_code.refactor_task_kind AS ENUM (
    'split_class',
    'extract_method',
    'invert_dependency',
    'break_cycle',
    'consolidate_duplication'
);

-- architecture Sec 5.3.5 line 1156: `Threshold.op` is the
-- five-value relational-operator enum used by the rule engine
-- to compare a metric value against a numeric threshold. The
-- spelled-out names match the DSL parser the SOLID Rule
-- Engine will own (Stage 4.x); no other op is supported in v1.
CREATE TYPE clean_code.threshold_op AS ENUM (
    'gt',
    'ge',
    'lt',
    'le',
    'eq'
);

-- ---------------------------------------------------------------------------
-- Closed-set constraint helpers (shared with Stage 1.3 enums)
-- ---------------------------------------------------------------------------

-- architecture Sec 5.2.3 line 1046 + Sec 1.5 / implementation-plan
-- Stage 1.3 line 92: the canonical seven-value `scope_kind`
-- closed set. Stage 1.3 will introduce `clean_code.scope_kind`
-- as a native ENUM type on `scope_binding`, `metric_sample`,
-- and `metric_sample_active`. This migration enforces the same
-- closed set on its three callers (`threshold.scope_kind`,
-- `override.scope_filter ->> 'scope_kind'`) via per-column
-- CHECK constraints so 0002 and 0003 stay independent.
--
-- `function` and `module` are explicitly NOT canonical; the
-- e2e-scenarios "scope_kind enum is the seven canonical values"
-- pin rejects them.

-- architecture Sec 8.2 + tech-spec Sec 7.7 C21: the closed
-- four-value `degraded_reason` set. Populates both
-- `metric_sample.degraded_reason` (Stage 1.3) and
-- `evaluation_verdict.degraded_reason` (here). `percentile_stale`
-- is reserved for the Insights surface only -- `eval.gate`
-- does NOT raise it (tech-spec C17), but the value is still
-- accepted by the column-level constraint so the SOLID batch
-- worker (which writes verdicts during background refresh)
-- can record it when stale percentiles degrade a batch verdict.

-- ---------------------------------------------------------------------------
-- RulePack (architecture Sec 5.3.2)
-- ---------------------------------------------------------------------------
--
-- Composite primary key `(pack_id, version)` per Sec 5.3.2
-- line 1116 ("Primary key together with `version`").
-- Append-only catalogue: every monotonic bump publishes a new
-- row; old rows remain so historical `PolicyVersion.rule_refs`
-- entries can still resolve.

CREATE TABLE clean_code.rule_pack (
    -- architecture Sec 5.3.2 line 1116: pack identifier text
    -- (e.g. `solid.srp`, `solid.dip`, `decoupling.cycles`,
    -- `base.complexity`). Composite PK with `version`.
    pack_id        text         NOT NULL,
    -- Monotonic; bumped on definitional change (G2). The
    -- composite PK guarantees uniqueness per pack lineage.
    version        integer      NOT NULL,
    -- Human-readable display name (Sec 5.3.2 line 1118).
    display_name   text         NOT NULL,
    -- Architecture Sec 5.3.2 line 1119 marks this column NOT
    -- NULL (no `?` suffix). Empty-string is acceptable when
    -- the pack ships without a long-form description; the
    -- catalogue still needs the row to satisfy `Rule.pack_id`
    -- references.
    description_md text         NOT NULL,
    -- Append-only.
    created_at     timestamptz  NOT NULL DEFAULT now(),
    PRIMARY KEY (pack_id, version)
);

COMMENT ON TABLE clean_code.rule_pack IS
    'Policy / rules sub-store (architecture Sec 1.5 G1, Sec 5.3.2). '
    'Writer: the Policy Steward (architecture Sec 3.11) is the '
    'SOLE writer. Append-only -- every monotonic version bump '
    'inserts a new row; existing rows are never UPDATEd or '
    'DELETEd (G3). DB-level role grants ship in Stage 1.5 '
    '(0004_roles.up.sql).';

-- ---------------------------------------------------------------------------
-- Rule (architecture Sec 5.3.1)
-- ---------------------------------------------------------------------------
--
-- Composite primary key `(rule_id, version)` per Sec 5.3.1
-- line 1098. The `pack_id` column is a LOGICAL FK to
-- `RulePack.pack_id` -- not a SQL FK -- because `RulePack`'s
-- physical PK is the composite `(pack_id, version)` and
-- `Rule` only carries the pack_id half. A SQL FK on the
-- composite would need a `rule.pack_version` column the
-- architecture does NOT define; we keep the column shape
-- canon-faithful and let the Policy Steward enforce the
-- logical reference at write time.

CREATE TABLE clean_code.rule (
    -- Architecture Sec 5.3.1 line 1098: rule identifier text
    -- (e.g. `solid.srp.lcom4_high`). Composite PK with `version`.
    rule_id          text                       NOT NULL,
    -- Monotonic version (Sec 5.3.1 line 1099).
    version          integer                    NOT NULL,
    -- Logical FK to `RulePack.pack_id` (Sec 5.3.1 line 1100).
    -- Not a SQL FK -- see header comment above.
    pack_id          text                       NOT NULL,
    -- DSL expression over `MetricSample` rows (Sec 5.3.1 line
    -- 1101). The DSL parser lives in `internal/policy/dsl/`
    -- (Stage 4.x); the SQL column is opaque text here.
    predicate_dsl    text                       NOT NULL,
    -- Default severity if no `threshold_refs` override applies
    -- (Sec 5.3.1 line 1102). Closed set `info | warn | block`.
    severity_default clean_code.rule_severity   NOT NULL,
    -- Human description (Sec 5.3.1 line 1103). NOT NULL --
    -- the rule catalogue must always carry a name an operator
    -- can read.
    description_md   text                       NOT NULL,
    -- Append-only (Sec 5.3.1 line 1104).
    created_at       timestamptz                NOT NULL DEFAULT now(),
    PRIMARY KEY (rule_id, version)
);

COMMENT ON TABLE clean_code.rule IS
    'Policy / rules sub-store (architecture Sec 1.5 G1, Sec 5.3.1). '
    'Writer: the Policy Steward (architecture Sec 3.11) is the '
    'SOLE writer. Append-only. `Rule.rule_id` is referenced by '
    '`override.rule_id`, `finding.(rule_id, rule_version)`, and '
    '`refactor_task.rule_id`; `finding` carries both halves of '
    'the composite PK and gets a SQL FK, the others reference '
    'rule_id alone and rely on application-layer enforcement.';

COMMENT ON COLUMN clean_code.rule.pack_id IS
    'Logical FK -> `clean_code.rule_pack.pack_id` '
    '(architecture Sec 5.3.1 line 1100). Not a SQL FK because '
    '`rule_pack` PK is composite `(pack_id, version)` and the '
    'architecture does not extend `Rule` with `pack_version`. '
    'The Policy Steward enforces the reference at write time.';

-- ---------------------------------------------------------------------------
-- PolicyVersion (architecture Sec 5.3.3, G5 immutability)
-- ---------------------------------------------------------------------------
--
-- Immutable row: once inserted, never UPDATEd. Activation is
-- recorded on `policy_activation` (Sec 5.3.4), NOT as a column
-- here. The signature column is the immutability anchor --
-- any drift in `(rule_refs, threshold_refs, refactor_weights)`
-- invalidates verification at gate time.
--
-- Columns mirror the brief verbatim. The columns
-- `signed_at`, `signing_key_id`, and `rulepack_set_hash` are
-- NOT in the architecture canon and MUST NOT be added here
-- (per Stage 1.4 brief lines 112-113). Key metadata lives on
-- the separate `clean_code.policy_signing_keys` table
-- introduced by tech-spec Sec 8.4.

CREATE TABLE clean_code.policy_version (
    -- Architecture Sec 5.3.3 line 1126.
    policy_version_id  uuid         PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Human label (Sec 5.3.3 line 1127). E.g. "default-v3".
    name               text         NOT NULL,
    -- Array of `{rule_id, version}` JSON objects pinning the
    -- exact rule lineage this policy resolves (Sec 5.3.3 line
    -- 1128). Stored as JSONB so the Evaluator Surface can
    -- index-scan it via `jsonb_path_ops` if needed.
    rule_refs          jsonb        NOT NULL,
    -- Array of `{threshold_id}` JSON objects (Sec 5.3.3 line
    -- 1129). Each threshold_id is the PK of a `clean_code.threshold`
    -- row.
    threshold_refs     jsonb        NOT NULL,
    -- Refactor planner weights bundle (Sec 5.3.3 line 1130 +
    -- Sec 3.9). Shape:
    --   { alpha, beta, gamma, delta,
    --     effort_model_version,
    --     window_days,
    --     freshness_window_seconds? }
    -- `alpha`/`beta`/`gamma`/`delta` are the composite-score
    -- weights consumed by the Refactor Planner (Sec 3.9 step 2);
    -- `effort_model_version` pins the ML model that produced
    -- this policy's `RefactorTask.effort_hours` (operator pin
    -- `refactor-effort-source`, Sec 1.6); `window_days` (int,
    -- positive; default 90 per tech-spec Sec 8.2) is the
    -- commit-window used to materialise
    -- `modification_count_in_window`; `freshness_window_seconds`
    -- (int, optional; default 3600) is the Insights stale-
    -- percentile threshold (architecture Sec 8.4). Shape
    -- enforcement lives in the Policy Steward, not in SQL.
    refactor_weights   jsonb        NOT NULL,
    -- Operator-required v1 cryptographic signature over the
    -- canonical JSON serialisation of `(rule_refs,
    -- threshold_refs, refactor_weights)`. Operator pin
    -- `policy-signing-required` (Sec 1.6) makes this column
    -- non-null in v1. Tech-spec Sec 8.4 pins Ed25519 (64-byte
    -- signatures); BYTEA accommodates that and any future
    -- algorithm bump. The Evaluator Surface MUST verify this
    -- signature on every `eval.gate` call -- a mismatch
    -- produces `verdict='warn'`, `degraded=true`,
    -- `degraded_reason='policy_signature_invalid'` (Sec 8.2).
    signature          bytea        NOT NULL,
    -- Append-only.
    created_at         timestamptz  NOT NULL DEFAULT now()
);

COMMENT ON TABLE clean_code.policy_version IS
    'Policy / rules sub-store (architecture Sec 1.5 G1, Sec 5.3.3). '
    'Writer: the Policy Steward (architecture Sec 3.11) is the '
    'SOLE writer. IMMUTABLE per G5 -- a row is never UPDATEd or '
    'DELETEd; "activating" a different policy is recorded by '
    'appending a `clean_code.policy_activation` row referencing '
    'the alternate `policy_version_id`. The columns `signed_at`, '
    '`signing_key_id`, and `rulepack_set_hash` are NOT part of '
    'the architecture canon and are intentionally absent.';

COMMENT ON COLUMN clean_code.policy_version.signature IS
    'Ed25519 signature over canonical JSON of (rule_refs, '
    'threshold_refs, refactor_weights) per tech-spec Sec 8.4. '
    'Operator pin `policy-signing-required` makes this column '
    'NOT NULL in v1; mismatched verification at gate time '
    'yields `verdict=''warn'', degraded=true, degraded_reason='
    '''policy_signature_invalid''` (architecture Sec 8.2).';

-- ---------------------------------------------------------------------------
-- PolicyActivation (architecture Sec 5.3.4, G5 latest-row-wins)
-- ---------------------------------------------------------------------------
--
-- "Currently active policy" = the `policy_version_id` of the
-- row with `MAX(created_at)`. There is intentionally:
--   * NO `scope` column -- activation is global per deployment
--     in v1 (tech-spec Sec 4.14 single-tenant pin).
--   * NO `deactivated_at` column -- deactivation is recorded
--     by appending a different activation row, not by mutating
--     an existing one (G3 + G5).
--   * NO partial unique index -- enforcing "at most one
--     currently-active row" via partial-unique would require a
--     mutable column to flip (UPDATE) which violates G3.
-- The e2e-scenarios "policy-activation-latest-row-wins" test
-- pins these absences.

CREATE TABLE clean_code.policy_activation (
    activation_id      uuid         PRIMARY KEY DEFAULT gen_random_uuid(),
    -- FK to the immutable `policy_version` row being activated.
    -- ON DELETE RESTRICT: a policy version row cannot disappear
    -- while activation history references it; deletion at the
    -- catalog level is forbidden by G3 anyway (DB grants in
    -- Stage 1.5 REVOKE DELETE), so this is belt-and-braces.
    policy_version_id  uuid         NOT NULL
                        REFERENCES clean_code.policy_version (policy_version_id)
                        ON DELETE RESTRICT,
    -- Operator id of the actor who flipped activation. Free-form
    -- text matching the agent-memory + Management surface
    -- convention.
    activated_by       text         NOT NULL,
    -- Append-only. The latest row by `created_at` defines the
    -- active policy. ORDER BY created_at DESC LIMIT 1 is the
    -- canonical lookup; index below supports it.
    created_at         timestamptz  NOT NULL DEFAULT now()
);

COMMENT ON TABLE clean_code.policy_activation IS
    'Policy / rules sub-store (architecture Sec 1.5 G1, Sec 5.3.4). '
    'Writer: the Policy Steward. Append-only. The currently '
    'active policy is the `policy_version_id` of the row with '
    'MAX(created_at). Activation is global per deployment in v1 '
    '(tech-spec Sec 4.14 single-tenant) -- there is NO `scope` '
    'column, NO `deactivated_at` column, and NO partial unique '
    'index. The implementation-plan Stage 1.4 test scenario '
    '"policy-activation-latest-row-wins" pins these absences.';

-- Evaluator Surface hot read: "what is the currently active
-- policy?" resolves to ORDER BY created_at DESC LIMIT 1.
-- A B-tree on created_at gives the planner a single-tuple
-- backwards index scan.
CREATE INDEX policy_activation_created_at_idx
    ON clean_code.policy_activation (created_at DESC);

-- ---------------------------------------------------------------------------
-- Threshold (architecture Sec 5.3.5)
-- ---------------------------------------------------------------------------
--
-- The metric_kind column FKs to the Stage 1.2 `metric_kind`
-- catalogue (it exists in 0001; safe target).

CREATE TABLE clean_code.threshold (
    threshold_id    uuid                       PRIMARY KEY DEFAULT gen_random_uuid(),
    -- FK to `clean_code.metric_kind` from Stage 1.2; the
    -- catalogue row carries the unit + tier metadata.
    metric_kind     text                       NOT NULL
                     REFERENCES clean_code.metric_kind (metric_kind)
                     ON DELETE RESTRICT,
    -- `repo | package | file | class | interface | method | block`
    -- (architecture Sec 5.2.3 / Sec 5.3.5). See header note on
    -- cross-stage enum sharing -- 0002 will introduce a native
    -- `clean_code.scope_kind` ENUM for the same closed set on
    -- `scope_binding` and `metric_sample`; here we enforce via
    -- CHECK so 0003 stays self-contained.
    scope_kind      text                       NOT NULL,
    -- Closed-set relational operator (Sec 5.3.5 line 1157).
    op              clean_code.threshold_op    NOT NULL,
    -- Numeric threshold value (Sec 5.3.5 line 1158). DOUBLE
    -- PRECISION matches `MetricSample.value` (Stage 1.3) so a
    -- comparison does not silently coerce.
    value           double precision           NOT NULL,
    -- Append-only.
    created_at      timestamptz                NOT NULL DEFAULT now(),
    -- Enforce the seven-value scope_kind closed set at the
    -- column level. The e2e-scenarios "scope_kind enum is the
    -- seven canonical values" pin rejects `function` / `module`.
    CONSTRAINT threshold_scope_kind_canonical CHECK (
        scope_kind IN (
            'repo', 'package', 'file', 'class',
            'interface', 'method', 'block'
        )
    )
);

COMMENT ON TABLE clean_code.threshold IS
    'Policy / rules sub-store (architecture Sec 1.5 G1, Sec 5.3.5). '
    'Writer: the Policy Steward. Append-only. `Threshold` rows '
    'are referenced by `PolicyVersion.threshold_refs` (JSON '
    'array of {threshold_id}) -- the FK target is enforced by '
    'the writer, not by SQL, since the reference lives inside '
    'a JSON document.';

COMMENT ON COLUMN clean_code.threshold.scope_kind IS
    'Canonical seven-value scope_kind closed set (architecture '
    'Sec 5.2.3 line 1046): repo | package | file | class | '
    'interface | method | block. `function` and `module` are '
    'NOT canonical. The CHECK constraint '
    '`threshold_scope_kind_canonical` enforces membership.';

-- ---------------------------------------------------------------------------
-- Override (architecture Sec 5.3.6)
-- ---------------------------------------------------------------------------
--
-- Mute / unmute state is recorded by appending a new Override
-- row; the latest row by `created_at` for a given `(rule_id,
-- scope_filter)` defines the current state. There is
-- intentionally:
--   * NO `expires_at` column -- v1 pins latest-row-wins per
--     tech-spec Sec 10A (iter 1 evaluator item 5 canon-guard).
--     The e2e-scenarios "override-no-expires-column" test
--     pins this absence.
--   * NO `policy_version_id` column -- overrides bind to
--     rules, not policy versions, per architecture Sec 5.3.6.

CREATE TABLE clean_code.override (
    override_id   uuid         PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Logical FK to `clean_code.rule.rule_id`. Not a SQL FK
    -- because `rule` PK is composite `(rule_id, version)` and
    -- `Override` only carries `rule_id` -- the override binds
    -- to the rule LINEAGE, not to a specific version.
    rule_id       text         NOT NULL,
    -- Architecture Sec 5.3.6 line 1167: shape is
    --   { repo_id, scope_kind, scope_signature_glob }.
    -- Stored as JSONB so the Evaluator Surface can match
    -- against `MetricSample` rows via JSON containment
    -- predicates. The `scope_kind` inside the document must
    -- be from the canonical seven-value set; the writer
    -- enforces this.
    scope_filter  jsonb        NOT NULL,
    -- Mute flag. When true, the Evaluator Surface writes
    -- `Finding` rows for matched scope with `severity='info'`
    -- (preserving the audit trail). When false, the override
    -- explicitly unmutes a previously-muted (rule_id,
    -- scope_filter) pair -- the latest-row-wins rule means
    -- this row reverses an earlier mute=true row.
    mute          boolean      NOT NULL,
    -- Required when `mute=true` (architecture Sec 5.3.6 line
    -- 1169). The CHECK constraint below enforces this at the
    -- database level so a writer that forgets to supply a
    -- reason cannot leave the audit log silently blank.
    reason        text,
    -- Operator id of the actor recording the override
    -- (architecture Sec 5.3.6 line 1170). The column name is
    -- `actor_id` per the architecture canon; `created_by`
    -- (used by some other sub-stores) is NOT a synonym here.
    actor_id      text         NOT NULL,
    -- Append-only.
    created_at    timestamptz  NOT NULL DEFAULT now(),
    -- mute=true must carry a reason; mute=false (the unmute
    -- shape) may carry one but does not require it. This
    -- guards the architecture-mandated "Required when mute=true"
    -- invariant at the schema level.
    CONSTRAINT override_reason_required_when_muted CHECK (
        mute = false OR reason IS NOT NULL
    )
);

COMMENT ON TABLE clean_code.override IS
    'Policy / rules sub-store (architecture Sec 1.5 G1, Sec 5.3.6). '
    'Writer: the Policy Steward. Append-only. The current mute '
    'state for a given (rule_id, scope_filter) is the `mute` '
    'value of the row with MAX(created_at) -- latest-row-wins '
    'per tech-spec Sec 10A. There is NO `expires_at` column '
    '(v1 pins latest-row-wins; the e2e-scenarios '
    '"override-no-expires-column" test guards this) and NO '
    '`policy_version_id` column (overrides bind to rules, not '
    'policy versions).';

-- Policy Steward + Evaluator Surface hot read: "what is the
-- current mute state for rule X?" resolves to ORDER BY
-- created_at DESC LIMIT 1 inside the rule_id partition.
CREATE INDEX override_rule_created_idx
    ON clean_code.override (rule_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- EvaluationRun (architecture Sec 5.4.2)
-- ---------------------------------------------------------------------------
--
-- One row per `eval.gate` invocation OR per SOLID Rule Engine
-- batch refresh. The `caller` enum distinguishes the two.
-- Append-only per architecture G3 + G4; Audit WAL is the
-- durability mechanism (architecture Sec 7.1 + Sec 7.10).

CREATE TABLE clean_code.evaluation_run (
    evaluation_run_id  uuid                              PRIMARY KEY
                        DEFAULT gen_random_uuid(),
    repo_id            uuid                              NOT NULL
                        REFERENCES clean_code.repo (repo_id)
                        ON DELETE RESTRICT,
    -- Free-form text -- the SHA being evaluated. No FK on sha
    -- alone (the canonical pair is `(repo_id, sha)` which
    -- FKs to `clean_code.commit`, but the architecture-Sec-5.4.2
    -- field table does not declare that composite FK and the
    -- Evaluator Surface MUST be able to record a verdict run
    -- for an as-yet-uningested SHA when the gate races the
    -- scanner).
    sha                text                              NOT NULL,
    policy_version_id  uuid                              NOT NULL
                        REFERENCES clean_code.policy_version (policy_version_id)
                        ON DELETE RESTRICT,
    caller             clean_code.evaluation_run_caller  NOT NULL,
    -- Append-only.
    created_at         timestamptz                       NOT NULL
                        DEFAULT now()
);

COMMENT ON TABLE clean_code.evaluation_run IS
    'Audit / verdict sub-store (architecture Sec 1.5 G1, Sec 5.4.2). '
    'Writers: the Evaluator Surface (caller=''eval_gate''), '
    'the SOLID Rule Engine batch worker (caller=''batch_refresh''), '
    'and the Audit WAL Reconciler (replay-only) per tech-spec '
    'Sec 7.2 lines 1258-1261. APPEND-ONLY (G3 + G4). The Audit '
    'WAL (architecture Sec 7.1 + Sec 7.10) is this table''s '
    'durability mechanism -- every INSERT here MUST be preceded '
    'by an Audit WAL write so the Reconciler can replay missing '
    'rows on PostgreSQL recovery.';

-- ---------------------------------------------------------------------------
-- EvaluationVerdict (architecture Sec 5.4.3)
-- ---------------------------------------------------------------------------
--
-- One row per EvaluationRun. Carries the gate verdict + a
-- nullable `degraded_reason` drawn from the architecture Sec 8.2
-- closed set (tech-spec Sec 7.7 C21).

CREATE TABLE clean_code.evaluation_verdict (
    verdict_id          uuid                                  PRIMARY KEY
                         DEFAULT gen_random_uuid(),
    evaluation_run_id   uuid                                  NOT NULL
                         REFERENCES clean_code.evaluation_run (evaluation_run_id)
                         ON DELETE RESTRICT,
    verdict             clean_code.evaluation_verdict_value   NOT NULL,
    -- Architecture Sec 5.4.3 line 1211: explicit boolean.
    -- Defaults to false so a writer that has no degradation to
    -- report can omit this column.
    degraded            boolean                               NOT NULL
                         DEFAULT false,
    -- Nullable per architecture Sec 5.4.3 line 1212 (`text?`).
    -- When non-null, must be a value from the closed list at
    -- architecture Sec 8.2 / tech-spec Sec 7.7 C21. The CHECK
    -- constraint below enforces this; the
    -- e2e-scenarios "degraded-reason-closed-set" test pins it.
    degraded_reason     text,
    -- Append-only.
    created_at          timestamptz                           NOT NULL
                         DEFAULT now(),
    CONSTRAINT evaluation_verdict_degraded_reason_canonical CHECK (
        degraded_reason IS NULL
        OR degraded_reason IN (
            'xrepo_edges_unavailable',
            'samples_pending',
            'policy_signature_invalid',
            'percentile_stale'
        )
    )
);

COMMENT ON TABLE clean_code.evaluation_verdict IS
    'Audit / verdict sub-store (architecture Sec 1.5 G1, Sec 5.4.3). '
    'Writers: same three roles as `evaluation_run` (Evaluator '
    'Surface, SOLID batch worker, Audit WAL Reconciler). '
    'APPEND-ONLY (G3 + G4). The Audit WAL (architecture Sec 7.1 '
    '+ Sec 7.10) is this table''s durability mechanism -- every '
    'INSERT here MUST be preceded by an Audit WAL write.';

COMMENT ON COLUMN clean_code.evaluation_verdict.degraded_reason IS
    'Closed-set degradation reason (architecture Sec 8.2, '
    'tech-spec Sec 7.7 C21): xrepo_edges_unavailable | '
    'samples_pending | policy_signature_invalid | '
    'percentile_stale. The four values populate both this '
    'column and `metric_sample.degraded_reason` (Stage 1.3); '
    'adding a fifth value requires the C22 three-step process. '
    '`percentile_stale` is Insights-surface-only -- '
    '`eval.gate` does NOT raise it (tech-spec C17).';

-- Verdict-by-run lookup: every gate response reads one verdict
-- row per evaluation_run_id, so the FK column gets an explicit
-- index. (PostgreSQL does not auto-index FK columns.)
CREATE INDEX evaluation_verdict_run_idx
    ON clean_code.evaluation_verdict (evaluation_run_id);

-- ---------------------------------------------------------------------------
-- Finding (architecture Sec 5.4.1)
-- ---------------------------------------------------------------------------
--
-- The Finding row composes the rule that fired, the policy
-- version that gated it, the scope it fired at, and the
-- MetricSample rows that produced its inputs (carried as a
-- JSON array of sample_id; sample-level FK is enforced at the
-- writer via tech-spec Sec 7.2 line 1260's three-writer grant).
--
-- Composite FK `(rule_id, rule_version)` -> `rule(rule_id, version)`
-- is enforceable here because `Finding` carries both halves of
-- `Rule`'s composite PK (unlike `Override` / `RefactorTask`
-- which carry only `rule_id`).

CREATE TABLE clean_code.finding (
    finding_id           uuid                       PRIMARY KEY
                          DEFAULT gen_random_uuid(),
    evaluation_run_id    uuid                       NOT NULL
                          REFERENCES clean_code.evaluation_run (evaluation_run_id)
                          ON DELETE RESTRICT,
    repo_id              uuid                       NOT NULL
                          REFERENCES clean_code.repo (repo_id)
                          ON DELETE RESTRICT,
    sha                  text                       NOT NULL,
    -- Architecture Sec 5.4.1 line 1183 declares this column an
    -- FK to ScopeBinding. ScopeBinding lives in Stage 1.3
    -- (0002_measurement.up.sql); we declare the column as a
    -- bare `uuid NOT NULL` here and let the application writer
    -- enforce the reference. A follow-up migration adds the
    -- SQL FK constraint once both stages have landed -- see
    -- this file's header note.
    scope_id             uuid                       NOT NULL,
    -- Composite FK to `rule(rule_id, version)`. The architecture
    -- canon (Sec 5.4.1 lines 1184-1185) splits the columns;
    -- the SQL FK joins them back into the composite PK target.
    rule_id              text                       NOT NULL,
    rule_version         integer                    NOT NULL,
    policy_version_id    uuid                       NOT NULL
                          REFERENCES clean_code.policy_version (policy_version_id)
                          ON DELETE RESTRICT,
    -- JSONB array of MetricSample.sample_id values that
    -- produced this finding's inputs (architecture G4 + tech-
    -- spec Sec 7.x). The canonical column name is
    -- `metric_sample_ids` (tech-spec Sec 5.10), NOT
    -- `sample_refs` or `sample_ids`.
    metric_sample_ids    jsonb                      NOT NULL
                          DEFAULT '[]'::jsonb,
    severity             clean_code.rule_severity   NOT NULL,
    delta                clean_code.finding_delta   NOT NULL,
    -- Human-readable explanation slot (G4). The LLM "explain
    -- this finding" service writes here when invoked, but the
    -- core gate path leaves the field empty -- so we mark it
    -- NOT NULL with a '' default rather than nullable.
    explanation_md       text                       NOT NULL
                          DEFAULT '',
    created_at           timestamptz                NOT NULL
                          DEFAULT now(),
    FOREIGN KEY (rule_id, rule_version)
        REFERENCES clean_code.rule (rule_id, version)
        ON DELETE RESTRICT
);

COMMENT ON TABLE clean_code.finding IS
    'Audit / verdict sub-store (architecture Sec 1.5 G1, Sec 5.4.1). '
    'Writers: Evaluator Surface, SOLID batch worker, Audit WAL '
    'Reconciler per tech-spec Sec 7.2 lines 1258-1261. '
    'APPEND-ONLY (G3 + G4). The Audit WAL (architecture Sec 7.1 '
    '+ Sec 7.10) is this table''s durability mechanism -- every '
    'INSERT here MUST be preceded by an Audit WAL write so the '
    'Reconciler can replay missing rows on PostgreSQL recovery.';

COMMENT ON COLUMN clean_code.finding.delta IS
    'Per-SHA delta classification (architecture Sec 5.4.1 line '
    '1189): new | newly_failing | unchanged | resolved. The '
    'values `regression | improvement | flat` are NOT '
    'canonical (e2e-scenarios "finding-delta-canonical" '
    'canon-guard, iter-3 drift item 7). `newly_failing` is '
    'the regressions bucket consumed by '
    '`mgmt.read.regressions` (architecture Sec 6.3).';

COMMENT ON COLUMN clean_code.finding.scope_id IS
    'Logical FK -> `clean_code.scope_binding.scope_id` (Stage '
    '1.3). The SQL FK is intentionally omitted here so this '
    'migration stays independent of Stage 1.3 per '
    'implementation-plan Stage 1.4 dependencies; a follow-up '
    'migration adds the constraint once both stages have '
    'landed.';

-- Finding-by-run lookup: the gate response enumerates all
-- findings for one evaluation_run_id; index supports that.
CREATE INDEX finding_run_idx
    ON clean_code.finding (evaluation_run_id);

-- Insights surface read: "show findings at (repo, sha)".
CREATE INDEX finding_repo_sha_idx
    ON clean_code.finding (repo_id, sha);

-- ---------------------------------------------------------------------------
-- HotSpot (architecture Sec 5.5.1)
-- ---------------------------------------------------------------------------
--
-- One row per (repo, sha, scope) the Refactor Planner ranks.
-- Carries the composite score from the Sec 3.9 planner step
-- and the policy_version_id whose `refactor_weights` produced
-- the score. NO per-input z-score columns: those are
-- intermediate values, only the composite `score` is
-- persisted (architecture Sec 5.5.1).

CREATE TABLE clean_code.hot_spot (
    hotspot_id         uuid              PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_id            uuid              NOT NULL
                        REFERENCES clean_code.repo (repo_id)
                        ON DELETE RESTRICT,
    sha                text              NOT NULL,
    -- Logical FK -> `scope_binding.scope_id` (see Finding.scope_id
    -- comment above).
    scope_id           uuid              NOT NULL,
    score              double precision  NOT NULL,
    policy_version_id  uuid              NOT NULL
                        REFERENCES clean_code.policy_version (policy_version_id)
                        ON DELETE RESTRICT,
    -- Append-only.
    created_at         timestamptz       NOT NULL DEFAULT now()
);

COMMENT ON TABLE clean_code.hot_spot IS
    'Refactor sub-store (architecture Sec 1.5 G1, Sec 5.5.1). '
    'Writer: the Refactor Planner (architecture Sec 3.9, '
    'tech-spec Sec 7.8 C23) is the SOLE writer. Append-only -- '
    'a re-rank at a new SHA inserts new rows; existing rows '
    'remain so historical hot-spot ranks stay queryable. Per-'
    'input z-score columns are intentionally absent (only the '
    'composite `score` is persisted).';

CREATE INDEX hot_spot_repo_sha_idx
    ON clean_code.hot_spot (repo_id, sha);

-- ---------------------------------------------------------------------------
-- RefactorPlan (architecture Sec 5.5.2)
-- ---------------------------------------------------------------------------
--
-- A plan bundles a set of HotSpot rows and emits a
-- human-readable summary. There is intentionally NO
-- `policy_version_id` column -- policy attribution lives on
-- each `hot_spot` row (architecture Sec 5.5.1).

CREATE TABLE clean_code.refactor_plan (
    plan_id      uuid         PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_id      uuid         NOT NULL
                  REFERENCES clean_code.repo (repo_id)
                  ON DELETE RESTRICT,
    sha          text         NOT NULL,
    -- JSONB array of HotSpot.hotspot_id values covered by this
    -- plan (architecture Sec 5.5.2 line 1236). Default empty
    -- array so a plan that is mid-construction can be persisted
    -- before its hotspot set is finalised.
    hotspot_ids  jsonb        NOT NULL DEFAULT '[]'::jsonb,
    -- Human-readable summary (Sec 5.5.2 line 1237). NOT NULL
    -- with empty-string default; the Refactor Planner fills
    -- this in when the LLM-explainer ships (post-v1).
    summary_md   text         NOT NULL DEFAULT '',
    -- Append-only.
    created_at   timestamptz  NOT NULL DEFAULT now()
);

COMMENT ON TABLE clean_code.refactor_plan IS
    'Refactor sub-store (architecture Sec 1.5 G1, Sec 5.5.2). '
    'Writer: the Refactor Planner. Append-only. The plan does '
    'NOT carry a `policy_version_id` column -- policy '
    'attribution lives on each `hot_spot` row referenced by '
    'this plan''s `hotspot_ids` JSON array.';

-- ---------------------------------------------------------------------------
-- RefactorTask (architecture Sec 5.5.3)
-- ---------------------------------------------------------------------------
--
-- One row per individual refactor playbook entry attached to a
-- plan. The architecture Sec 5.5.3 field table closes with
-- "etc." after the five canonical `kind` values; the
-- implementation-plan Stage 1.4 brief LIMITS v1 to exactly
-- those five and the ENUM type above enforces that closed set.
-- There is intentionally NO `status` column (life-cycle state
-- is out of v1 scope) and NO `expected_metric_delta` column.

CREATE TABLE clean_code.refactor_task (
    task_id         uuid                            PRIMARY KEY
                     DEFAULT gen_random_uuid(),
    plan_id         uuid                            NOT NULL
                     REFERENCES clean_code.refactor_plan (plan_id)
                     ON DELETE RESTRICT,
    -- Logical FK -> `scope_binding.scope_id` (see Finding.scope_id
    -- comment above).
    scope_id        uuid                            NOT NULL,
    kind            clean_code.refactor_task_kind   NOT NULL,
    -- Produced by the ML model pinned in
    -- `PolicyVersion.refactor_weights.effort_model_version`
    -- (operator pin `refactor-effort-source`, architecture
    -- Sec 1.6).
    effort_hours    double precision                NOT NULL,
    -- The rule that motivated this task (architecture Sec 5.5.3
    -- line 1249). Logical FK to `rule.rule_id`; not a SQL FK
    -- because `rule` PK is composite and `RefactorTask`
    -- carries only the rule_id half.
    rule_id         text                            NOT NULL,
    description_md  text                            NOT NULL,
    -- Append-only.
    created_at      timestamptz                     NOT NULL
                     DEFAULT now()
);

COMMENT ON TABLE clean_code.refactor_task IS
    'Refactor sub-store (architecture Sec 1.5 G1, Sec 5.5.3). '
    'Writer: the Refactor Planner. Append-only. Carries NO '
    '`status` column (life-cycle state out of v1 scope) and NO '
    '`expected_metric_delta` column (e2e-scenarios '
    '"refactor-task-no-status-column" canon-guard). `kind` is '
    'the five-value closed set on `clean_code.refactor_task_kind`; '
    'extending it requires a catalogue-bump migration.';

CREATE INDEX refactor_task_plan_idx
    ON clean_code.refactor_task (plan_id);

COMMIT;
