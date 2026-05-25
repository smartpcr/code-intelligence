# Clean Code -- Architecture

> Story: `code-intelligence:CLEAN-CODE` -- 13 points
> Companion docs (parallel-authored): `tech-spec.md`, `implementation-plan.md`, `e2e-scenarios.md`
> This file owns the component / data-model / interface / sequence-flow contracts; siblings own DDL, phasing, and end-to-end test scenarios respectively.

## 1. Purpose and Scope

This document defines the architecture of the **Clean Code** service: a
system-level metrics, evaluation, and refactor-planning surface that
turns "clean code" from a slogan into measurable, queryable, and
gate-able contracts. It is the service that lets the evaluator agent
**block** a change that violates decoupling or SOLID guard-rails, and
lets a refactoring lead see **where the debt actually is** across one
big repo or many.

The story description fixes three goals; every later section traces
back to one of them:

1. **Decoupled functional areas.** Detect and quantify coupling /
   cohesion / cycles at file / package / repo scope.
2. **SOLID coding principles.** Detect and quantify SRP / OCP / LSP /
   ISP / DIP violations and surface them as findings.
3. **Big-repo and cross-repo measurement.** Compute metrics per scope
   per SHA, aggregate them across repos in the portfolio, and expose
   percentile context so a team can ask "are we worse than median?".

### 1.1 Greenfield anchoring

The worktree on branch
`story/code-intelligence-CLEAN-CODE/plan-architecture` contains
only `README.md`, `.gitignore`, `docs/`, and the existing
`services/agent-memory/` subtree. **There is no
`services/clean-code/` tree yet**, no schema, no compute engine, no
gate verb. Every component, table, and interface named in this
document is introduced by this story. The implementation plan
(`implementation-plan.md`) decides the language / package layout
when the tech spec is locked; this document only names module
boundaries the way the spec will name database tables.

The sibling service that this document is allowed to depend on is
`services/agent-memory/` (story `code-intelligence:AGENT-MEMORY`,
already shipped). The dependency is **optional** and gated by
`Repo.mode in {embedded, linked}` (see Section 1.6 operator pin
`ast-mode-default`). In `embedded` mode the Clean Code service
parses repos itself with a standalone tree-sitter front end
(ast-metrics-style); it does **not** import or call agent-memory at
all. In `linked` mode it composes with `agent-memory.GraphReader`
for cross-repo call edges (Section 8.7) by calling the existing
agent-memory MCP verb `mgmt.read.graph_node(node_id, sha?)`
(anchor: AGENT-MEMORY architecture Section 6.2 line 766). The
default is `embedded` (operator pin, Section 1.6).

### 1.2 In scope / out of scope

| In scope | Out of scope |
| --- | --- |
| Foundation-tier code metrics per scope per SHA (cyclomatic complexity, fan-in / fan-out, LCOM, depth-of-inheritance, etc.) | Style / formatter rules (delegated to language-specific linters; this service does not re-implement gofmt / prettier / black) |
| System-tier metrics that compose foundation-tier rows (arch-debt-ratio, blast-radius, knowledge-index, velocity-trend, xrepo-dep-depth, arch-fitness, xservice-test-reliability) | Source-code rewriting / auto-fix (the Refactor Planner emits rows, never patches) |
| SOLID rule pack on top of foundation-tier rows | Per-rule semantic linting that is already covered by an upstream linter (we *consume* its findings via `ingest.*` instead) |
| Cross-repo percentiles and portfolio snapshots | UI / dashboard implementation (only the read contract is in scope) |
| Evaluator gate verb (`eval.gate`) with kill-switch overrides | CI / pipeline integration (the evaluator agent calls `eval.gate`; this doc does not pin Jenkins / GitHub Actions wiring) |
| External-metric ingest webhook (coverage, churn, defect, test-balance) | Hosting coverage tooling itself (we accept Cobertura XML payloads per operator pin `external-metric-coverage-format`) |
| Refactor planner that emits `HotSpot` / `RefactorPlan` / `RefactorTask` rows with ML-derived effort estimates | Workflow / Jira integration (the rows are read by an external workflow tool, out of scope) |
| Append-only audit (`EvaluationRun` / `EvaluationVerdict` / `Finding` / `MetricRetraction`) | Per-tenant isolation (single tenant in v1; multi-tenant deferred) |

### 1.3 References from the story brief

The story description names two external references; this
architecture honours both:

- **augmentcode 12-metrics guide** -- the foundation-tier and
  system-tier metric catalogue (Section 1.4) is the intersection of
  that guide with what is mechanically measurable from source code
  and OTel-style telemetry.
- **ast-metrics (Halleck45)** -- the AST Adapter (Section 3.2) is
  modelled on its tree-sitter front end and per-language metric
  recipes. The default `ast-mode-default` operator pin is
  `embedded` for exactly this reason (Section 1.6).

### 1.4 Metric catalogue (normative)

Every metric this service emits belongs to exactly one **tier** and
exactly one **pack**.

#### 1.4.1 Foundation tier (`pack='base'` or `pack='solid'`)

Computed directly from the AST (Section 3.2) by the Compute Engine
(Section 3.4). One sample per `(repo_id, sha, scope_id,
metric_kind)`.

| # | `metric_kind` | Scope | Pack | Brief |
| --- | --- | --- | --- | --- |
| 1 | `cyclo` | method, file | base | McCabe cyclomatic complexity. |
| 2 | `cognitive_complexity` | method, file | base | SonarSource-style cognitive complexity. |
| 3 | `loc` | file, package, repo | base | Lines of code (non-blank, non-comment). |
| 4 | `lcom4` | class | solid | Lack of cohesion of methods (LCOM4 variant). Drives SRP rule. |
| 5 | `fan_in` | method, class, file | solid | Number of incoming call sites. Drives OCP / DIP rules. |
| 6 | `fan_out` | method, class, file | solid | Number of outgoing call sites. Drives DIP rule. |
| 7 | `depth_of_inheritance` | class | solid | DIT. Drives LSP rule. |
| 8 | `interface_width` | class, interface | solid | Public method count. Drives ISP rule. |
| 9 | `coupling_between_objects` | class | solid | CBO. Drives DIP / decoupling rule. |
| 10 | `cycle_member` | file, package | base | 1 iff the scope participates in a strongly-connected component in the import graph; cycle id in `attrs_json`. Drives decoupling rule. |
| 11 | `duplication_ratio` | file, package | base | Fraction of tokens that recur as a clone of length >= 50. |
| 12 | `modification_count_in_window` | file, method | base | Count of touching commits in the last `PolicyVersion.refactor_weights.window_days` window (Section 5.3.3); materialised by the Metric Ingestor on `ingest.churn` arrival (Section 3.1 case 3, Section 3.5.1.b). |
| 13 | `lsp_violation` | method | solid | AST-level Liskov override-signature indicator. `value=1` iff an overriding method strengthens its parent's precondition or weakens its postcondition (Section 3.5.1.c); `value=0` otherwise. Drives the LSP override rule. Emitted by Stage 2.4 (Adapter, Section 3.2) as a first-class `MetricSample` row -- this is the DSL-expressible projection of the same boolean fact also recorded in `MetricSample.attrs_json.lsp_violation` (the latter retained for forensics). The dual encoding mirrors `cycle_member` (row 10), which is likewise a 0/1 AST-derived structural metric_kind whose detail (the cycle id) lives in `attrs_json`. |

#### 1.4.2 System tier (`pack='system'`)

Composed by the **Cross-Repo Aggregator** (Section 3.10 step 4) from
foundation-tier `MetricSample` rows; this is the **only** path that
produces `pack='system'` rows. `source` is always `'derived'`. When
a required input is missing, the row is **still written** but
stamped with `degraded=true` and `degraded_reason` drawn from the
Section 8.2 closed list (the closed list is the contract -- no other
reason strings appear in `MetricSample.degraded_reason`).

| # | `metric_kind` | Scope | Required inputs | Notes |
| --- | --- | --- | --- | --- |
| 1 | `xrepo_dep_depth` | repo | cross-repo edges (Section 8.7). In `embedded` mode the edge set is empty; the row is written with `degraded=true`, `degraded_reason='xrepo_edges_unavailable'`. | Counts the longest dependency chain through the portfolio. |
| 2 | `arch_debt_ratio` | package, repo | `cycle_member` rows from foundation tier. | Sum of cycle weights / total package weight. |
| 3 | `velocity_trend` | repo | `modification_count_in_window` over rolling 4 windows. | Trend slope; degrades to flat if fewer than 2 windows are populated. |
| 4 | `arch_fitness` | repo | `cycle_member`, `fan_in`, `coupling_between_objects`. | Composite "is the structure getting better or worse" score. |
| 5 | `blast_radius` | method, class | call-graph from Section 3.2 (`linked` mode) **or** local fan-in from foundation tier (`embedded` mode). In `embedded` mode the row is written with `degraded=true`, `degraded_reason='xrepo_edges_unavailable'`. | "If this method breaks, how much breaks with it?" |
| 6 | `xservice_test_reliability` | repo | **Aggregator-only**: composes the `pack='ingested'` foundation-tier `pass_first_try_ratio` rows written by the External Metric Ingest Webhook on `ingest.test_balance` (Section 3.12) into the system-tier row. Per the Section 1.5.1 reconciliation row 2, the Aggregator is the writer and the source is `derived`; the webhook is never the writer of `xservice_test_reliability`. | Rolling test-flakiness measure. |
| 7 | `knowledge_index` | file, repo | `modification_count_in_window` plus author / blame data from `ingest.churn`. When `ingest.churn` has not yet arrived for a repo, the row is written with `degraded=true`, `degraded_reason='samples_pending'`. | Bus-factor-style index. |

### 1.5 Guiding invariants (G1 -- G7)

These are the contract bedrocks every later section MUST respect.
Where you see `Gn` referenced elsewhere in this file, it is one of
these.

- **G1 -- Sub-store ACL: one writer per sub-store.** The Metrics
  Store is partitioned into **five logical sub-stores**, each with
  exactly one writer service (the "Measurement" sub-store has two
  writers and one carve-out for derived rows -- both are listed in
  the ACL table below and reconciled in Section 1.5.1):

  | Sub-store | Tables | Writer(s) | Readers |
  | --- | --- | --- | --- |
  | Catalog / Lifecycle | `Repo`, `Commit`, `RepoEvent`, `MetricKind`, `ScanRun` | Management (writes `Repo`, `RepoEvent`, and every `Commit` column **except** `scan_status`) + Metric Ingestor (writes `ScanRun` and the `Commit.scan_status` column **only** -- never any other `Commit` column; see Section 1.5.1 row 1 for the normative split) | All other components (read-only) |
  | Measurement | `MetricSample` (foundation-tier rows where `pack IN ('base', 'solid', 'ingested')`), `ScopeBinding`, `MetricRetraction` | Metric Ingestor | Compute Engine, SOLID Rule Engine, Refactor Planner, Cross-Repo Aggregator, Insights, Evaluator |
  | Measurement (system-tier carve-out) | `MetricSample` rows where `pack='system'` AND `source='derived'`, `RepoMetricSnapshot`, `CrossRepoPercentile`, `PortfolioSnapshot` | Cross-Repo Aggregator (the **only** writer of `pack='system'` rows; see Section 3.10 step 4) | Insights, Evaluator, Refactor Planner |
  | Policy / rules | `Rule`, `RulePack`, `PolicyVersion`, `PolicyActivation`, `Threshold`, `Override` | Policy Steward (Section 3.11) -- the **only** writer of `RulePack` (see Section 1.5.1 row 4) | Evaluator (read-only), Insights (read-only) |
  | Audit / verdict | `EvaluationRun`, `EvaluationVerdict`, `Finding` | Evaluator Surface (`eval.gate` caller); SOLID Rule Engine batch worker (`Section 3.6`, post-ingest refresh -- writes all three tables with `EvaluationRun.caller='batch_refresh'`); **and** Audit WAL Reconciler (Section 7.10, **replay-only** of WAL-buffered rows -- never originates a new `EvaluationRun`, only re-commits rows already written to the local WAL when PostgreSQL was unreachable, preserving the original `EvaluationRun.caller` value from the WAL frame) | Insights, Management |
  | Refactor | `HotSpot`, `RefactorPlan`, `RefactorTask` | Refactor Planner (Section 3.9) | Insights, Management |

- **G2 -- Identity by `(repo_id, sha, scope_id, metric_kind,
  metric_version)`, over the active row set.** Every **active**
  `MetricSample` row is uniquely keyed by this quintuple,
  uniformly across all three `source` values (`computed`,
  `ingested`, `derived`). A row is **active** iff no
  `MetricRetraction` row (Section 5.2.2) references its
  `sample_id`; retracted rows are tombstones and do NOT block a
  fresh insert at the same quintuple, but SHA-pinned readers
  always filter retracted rows out of consideration (Section
  5.2.1 Read-time semantics callout). At any instant at most one
  active row exists per quintuple. `metric_version` participates
  in the identity so a definitional change (a new formula for
  `cyclo`, say) lands as a **new row** alongside the old one (per
  G3); historical verdicts pinned to the prior version remain
  reproducible. `scope_id` is the entity the metric applies to (a
  method, a file, a package, a repo); it is a UUID with a
  `scope_kind` discriminator and is **stable across SHAs** so a
  chart of "this method's complexity over time" is a simple range
  scan.

  In **both** AST Adapter modes (Section 3.2), `scope_id` is minted
  from the same canonical-signature recipe that agent-memory uses
  to compute its `Node.fingerprint` (anchor: agent-memory
  architecture Section 5.2.1 field-row lines 430-431, where
  `node_id` is server-generated and the deterministic identity is
  the `fingerprint = sha256(repo_id || kind ||
  canonical_signature || first_seen_sha)`). Specifically `scope_id`
  is a deterministic uuid derived from `(repo_id, scope_kind,
  canonical_signature, first_seen_sha)` -- the same quadruple that
  feeds agent-memory's fingerprint pre-image. `scope_id` **never**
  aliases agent-memory's `node_id` directly (that would be
  undefined in `embedded` mode, where there is no agent-memory
  backing the Adapter, and would be brittle in `linked` mode
  because `node_id` is server-generated). When the clean-code
  service runs in `linked` mode the Adapter additionally records
  the agent-memory `node_id` in `ScopeBinding.agent_memory_node_id`
  (Section 5.2.3) so the two surfaces can be joined for the
  optional integrations in Section 8.7. Because both modes mint
  `scope_id` from the same pre-image, switching a repo from
  `embedded` to `linked` (or vice versa) preserves historical
  samples without rewrite.

- **G3 -- Metric rows are immutable.** A new measurement at a later
  SHA is a *new row*, not an update. A retracted measurement (the
  AST Adapter later learns the file was binary, generated, vendored,
  etc.) is recorded by appending a `MetricRetraction` row, never by
  deleting the original. The `degraded` / `degraded_reason` fields
  on `MetricSample` are likewise insert-only. This mirrors
  agent-memory's G5 retirement pattern.

- **G4 -- Findings cite their inputs.** Every `Finding` row
  (Section 5.4.1) carries (a) the rule id, (b) the rule version,
  (c) the exact `MetricSample` row ids that triggered it, and
  (d) a human-readable explanation slot. A finding is
  **reproducible**: given the same `(policy_version, rule_version,
  MetricSample ids)` the engine produces the same verdict.

- **G5 -- Policies are versioned, signed, and kill-switchable.**
  Operator-published rule packs become `PolicyVersion` rows
  (Section 5.3.3); each evaluation pins a `policy_version_id`. A
  rule can be muted at runtime by appending an `Override` row
  (Section 5.3.6) without rewriting the policy. The Evaluator
  Surface consults the Override table on every call so a noisy rule
  can be turned off in seconds. **Signature is v1 required**
  (operator pin `policy-signing-required`, Section 1.6):
  `PolicyVersion.signature` is cryptographically signed at publish
  time, and the evaluator agent **MUST** verify the signature on
  every `eval.gate` call before posting a verdict (see Section 8.3
  reliability invariant). *(Wen Zhong: every alert has an owner, a
  runbook, and a kill switch.)*

- **G6 -- Cross-repo percentiles are derived, never authoritative.**
  Per Section 3.10, `CrossRepoPercentile` rows are a materialised
  view over `MetricSample`. They can be rebuilt deterministically
  from `MetricSample` at any time; consumers are warned (via
  `degraded=true` with `degraded_reason='percentile_stale'`, Section
  8.4) when the view is stale.

- **G7 -- AST is the canonical front end.** Per the story's
  reference to `ast-metrics`, the AST Adapter (Section 3.2) is the
  single front end for all language parsing. New languages are
  added by registering a new tree-sitter grammar in the Adapter,
  not by writing language-specific paths inside the Compute Engine,
  the SOLID Rule Engine, or the Refactor Planner. This keeps
  language proliferation from leaking into downstream components.

### 1.5.1 Writer-ownership reconciliation (normative single-claim per disputed seam)

Five normative seams have surfaced in prior reviews as
"contradicted across sections". For each, this table is the
**single normative claim** the rest of the document is required to
match. If a later section appears to say otherwise, this table wins
and the later section is a bug. Every claim here is restated -- in
the same words -- at the section listed in the last column, so a
grep for the **bold keyword** lands on the same statement at both
ends.

| # | Disputed seam | Normative claim | Restated at |
| --- | --- | --- | --- |
| 1 | **`ScanRun` and `Commit` writer split** | The **Metric Ingestor** (Section 3.1) is the only writer of `ScanRun` rows. The `Commit` row has a two-writer column split with a **DB-default carve-out for the initial `pending` state**: **Management** (whose `Commit` writer slot is delegated to the **Repo Indexer** per Section 3.3) writes every `Commit` column (`repo_id`, `sha`, `parent_sha`, `committed_at`) on INSERT **except** `scan_status`; the `scan_status` column has a **schema-level column DEFAULT of `'pending'`** which the database engine supplies at INSERT time, so **no component** (not Management, not the Repo Indexer, not the Metric Ingestor) writes the `'pending'` value explicitly. After the row exists the **Metric Ingestor** is the only writer of `Commit.scan_status` and is the only component that may transition it `pending -> scanning -> scanned | failed`. Management NEVER writes `Commit.scan_status` and NEVER writes `ScanRun`. | Section 1.5 G1 (Catalog / Lifecycle ACL row), Section 3.1 (Ingestor responsibilities / Failure modes), Section 3.3 (Repo Indexer omits `scan_status` from INSERT), Section 4.1 step 1 (DB default supplies `'pending'`), Section 5.1.2 (`Commit.scan_status` field note), Section 5.7 (`ScanRun` writer column), Section 6.3 (Management write verbs note), Section 9 (public-contract summary rows) |
| 2 | **`xservice_test_reliability` source** | The **Cross-Repo Aggregator** (Section 3.10 step 4) is the writer; `source='derived'` and `pack='system'`. The External Metric Ingest Webhook writes only the foundation-tier `pass_first_try_ratio` input row (`pack='ingested'`); it **never** writes the system-tier `xservice_test_reliability` row directly. | Section 1.4.2 row 6, Section 3.10 step 4, Section 3.12 (webhook), Section 5.2.2 (sample `pack` enum) |
| 3 | **External ingest SHA shape** | The Metric Ingestor accepts external-mode payloads in **four** ingest shapes (Section 3.1): `external_cov(repo_id, sha, payload)`, `external_test(repo_id, sha, payload)`, `external_churn(repo_id, payload)`, `external_defects(repo_id, payload)`. For coverage and test-balance the payload is bound to a single SHA; for churn and defects each row carries its own SHA and the parent `ScanRun.sha_binding='per_row'` with `to_sha=NULL` (Section 5.7). The webhook surface in Section 6.4 mirrors these four shapes. | Section 3.1 preamble ("four modes"), Section 5.7 (`ScanRun.sha_binding` enum), Section 6.4 (verb signatures) |
| 4 | **`RulePack` writer and sub-store** | The **Policy Steward** (Section 3.11) is the only writer of `RulePack` rows, and the row lives in the **Policy / rules** sub-store alongside `Rule` and `PolicyVersion` (which it bundles). Management does NOT delegate `RulePack` writes via any `mgmt.*` verb; operators publish packs through `policy.publish_rulepack` (Section 6.5). `RulePack` is NOT in the Catalog / Lifecycle sub-store. | Section 1.5 G1 (Policy / rules ACL row), Section 5.3.2 (`RulePack` field table placement callout), Section 5.8 (sub-store summary table), Section 9 (Policy Steward Writes column) |
| 5 | **Override verb naming** | Operator mute / unmute uses the **`mgmt.override`** verb on the Management surface (Section 6.3); Management delegates to the Policy Steward, which appends the `Override` row. There is intentionally **no** `policy.override` verb on the Policy Steward surface (Section 6.5) -- all override traffic is routed via Management so audit logs are uniform across operator mutations. Sections that listed `policy.override` as a trigger in prior drafts are bugs. | Section 6.3 (`mgmt.override` definition), Section 6.5 (no-`policy.override` note), Section 4.6 (operator-mute flow uses `mgmt.override`), Section 9 (Policy Steward Trigger column lists `mgmt.override`) |

### 1.6 Operator pins (answered, normative)

Five questions were raised during prior reviews and have been
answered by the operator. They are now **normative** in this
document and re-stated at the relevant body section.

| Pin id | Question | Operator answer | Re-stated at |
| --- | --- | --- | --- |
| `ast-mode-default` | Default mode for the AST Adapter: embedded or linked? | **embedded** -- standalone tree-sitter, no agent-memory dependency | Section 3.2, Section 5.1 (`Repo.mode` default) |
| `external-metric-coverage-format` | v1 canonical coverage payload for `ingest.coverage`? | **Cobertura XML** | Section 3.12, Section 6.4 |
| `gate-degraded-policy` | When `eval.gate` detects `samples_pending`, warn or fail-closed? | **warn** -- never block on missing samples | Section 3.7 step 3, Section 8.2 |
| `policy-signing-required` | Is cryptographic signing of `PolicyVersion.signature` a v1 requirement? | **v1 required** -- evaluator agent MUST verify on every `eval.gate` call | Section 1.5 G5, Section 5.3.3, Section 8.3 |
| `refactor-effort-source` | Source of refactor-task effort estimates? | **ML model from historical commits** | Section 3.9, Section 5.5.3 (`RefactorTask.effort_hours`), Section 5.3.3 (`PolicyVersion.refactor_weights.effort_model_version`) |

---

## 2. Context

```
+----------------+     OTel / git events     +-----------------+
| code authors   |--------------------------> | Management      |
| (PRs, commits) |                            | (Repo / Commit) |
+----------------+                            +--------+--------+
                                                       |
                                                       v
                                                +---------------+
                                                | Metric        |
                                                | Ingestor      |
                                                | (Section 3.1) |
                                                +-------+-------+
                                                        |
                       +--------------------------------+
                       |
                       v
+--------------+  +-----------+  +--------------+  +---------------+
| AST Adapter  |  | Compute   |  | SOLID Rule   |  | Refactor      |
| (Section 3.2)|  | Engine    |  | Engine       |  | Planner       |
+--------------+  | (3.4)     |  | (3.6)        |  | (3.9)         |
                  +-----+-----+  +------+-------+  +-------+-------+
                        |               |                  |
                        v               v                  v
                  +-----------------------------------------------+
                  |                Metrics Store                  |
                  |  (Catalog / Measurement / Policy / Audit /    |
                  |   Refactor sub-stores; per Section 1.5 G1)    |
                  +-----+----+--------+-------+-------------------+
                        ^    |        ^       ^
                        |    |        |       |
       +----------------+    |        |       +-----------------+
       |                     v        |                         |
+--------------+  +---------------+  +----------------+  +---------------+
| Cross-Repo   |  | Evaluator     |  | Insights       |  | Policy        |
| Aggregator   |  | Surface       |  | Surface        |  | Steward       |
| (3.10)       |  | (3.7)         |  | (3.8)          |  | (3.11)        |
+------+-------+  +---------------+  +----------------+  +-------+-------+
       |                  ^                    ^                 |
       v                  |                    |                 |
+--------------+  +---------------+    +---------------+  +---------------+
| External     |  | evaluator     |    | operator UI / |  | operator UI   |
| Metric Ingest|  | agent         |    | dashboards    |  | (publish rule)|
| Webhook(3.12)|  +---------------+    +---------------+  +---------------+
+--------------+
```

Surfaces in plain English:

- **Management** is the lifecycle / control-plane surface: register
  a repo, change a repo's mode, retract a sample, rescan at a SHA,
  mute / override a rule, and read the public metric and finding
  views (all read verbs are `mgmt.read.*` per Section 6.3).
  Management is the **formal G1 writer** of the Catalog / Lifecycle
  sub-store (`Repo`, `RepoEvent`, `Commit`) but it **delegates**
  actual `Commit` row creation to the **Repo Indexer** (Section
  3.3, Management's formal G1 delegate) and it **never** writes
  `Commit.scan_status` -- the Metric Ingestor is the only writer
  of `Commit.scan_status` per Section 1.5.1 row 1.
- **Metric Ingestor** turns "a SHA appeared" or "an external payload
  arrived" into rows in the Measurement sub-store.
- **Compute Engine** consumes ASTs and writes foundation-tier rows.
- **SOLID Rule Engine** consumes Measurement rows and a
  `PolicyVersion` to emit `EvaluationRun` + `EvaluationVerdict` +
  `Finding` rows (also serves `eval.gate`).
- **Refactor Planner** turns `Finding` + `MetricSample` into
  `HotSpot` / `RefactorPlan` / `RefactorTask` rows.
- **Cross-Repo Aggregator** materialises portfolio snapshots,
  percentiles, and the system-tier `MetricSample` rows
  (`pack='system'`).
- **Policy Steward** is the operator-facing writer for the Policy
  sub-store.
- **Evaluator Surface** is the gate verb (`eval.gate`) consumed by
  the evaluator agent on every change.
- **Insights Surface** is the read-only verb set consumed by the
  operator UI.
- **External Metric Ingest Webhook** is the foreign-data entry
  point (coverage, churn, defects, test-balance).

---

## 3. Components

### 3.1 Metric Ingestor

**Purpose.** Single writer of the foundation-tier
`MetricSample` / `ScopeBinding` / `MetricRetraction` rows (per
Section 1.5 G1), plus sole writer of `ScanRun` (per Section 1.5.1
row 1).

**Inputs.** Three triggers, four modes:

- *Trigger A:* a new `Commit` row appears with `scan_status='pending'`
  (Management is the writer of Commit; the Ingestor is a reader of
  that lifecycle event).
- *Trigger B:* the External Metric Ingest Webhook (Section 3.12)
  enqueues a payload.
- *Trigger C:* an operator explicitly enqueues a re-scan via
  `mgmt.rescan(repo_id, sha?)` (Section 6.3).

**Four modes.** A `ScanRun` row carries `kind` and `sha_binding`
that together discriminate which of the four shapes is running:

1. `full(repo_id, sha)` -- full AST + Compute over the entire repo
   at the given SHA. `sha_binding='single'`, `to_sha=sha`.
2. `delta(repo_id, from_sha, to_sha)` -- AST + Compute over the
   files touched between two SHAs. `sha_binding='single'`,
   `to_sha=to_sha`.
3. `external_single(repo_id, sha, payload)` -- the External Metric
   Ingest Webhook delivered a coverage or test-balance payload
   bound to a single SHA. `sha_binding='single'`, `to_sha=sha`.
4. `external_per_row(repo_id, payload)` -- the External Metric
   Ingest Webhook delivered a churn or defect payload whose **rows
   each carry their own SHA**. `sha_binding='per_row'`,
   `to_sha=NULL`. Each emitted `MetricSample` row references its
   own SHA from the payload; the Ingestor still emits exactly one
   `ScanRun` row per ingest call so the audit trail and retry
   semantics are uniform.

Per Section 1.5.1 row 3 the four ingest shapes are also the
public-facing verb shape in Section 6.4.

**Outputs.** Writes to (per Section 1.5 G1):

- `MetricSample` (foundation-tier rows; `pack IN ('base', 'solid',
  'ingested')`; never `pack='system'` -- that is the Aggregator's
  carve-out).
- `ScopeBinding` (creates the row when a new `scope_id` is first
  observed; immutable thereafter).
- `MetricRetraction` (when the AST Adapter or the operator says a
  prior sample should not count towards verdicts).
- `ScanRun` (one per ingest call; see "Four modes" above).
- `Commit.scan_status` transitions (`pending -> scanning ->
  scanned | failed`); this is the **only** column the Ingestor
  may flip on `Commit` (Management owns the rest of the row).

**Failure modes.** Crash mid-scan leaves `ScanRun.status='running'`
and the linked `Commit.scan_status='scanning'`. **Recovery is owned
by the Metric Ingestor, not by the Audit WAL Reconciler** (the WAL
is scoped to Audit-sub-store rows only per Section 7.1 / Section
7.10, and Measurement / Catalog state is re-derivable from a
re-scan per G6). On startup -- and on a periodic sweep cadence
operator-pinned in the tech-spec -- the Ingestor scans for
`ScanRun` rows where `status='running' AND (now - started_at) >
scan_timeout` and either re-enters the scan (if the input is still
ingestible) or transitions both `ScanRun.status` and the linked
`Commit.scan_status` to `failed`. Idempotency: the Ingestor checks
`(repo_id, sha, scope_id, metric_kind, metric_version)` against
the **active row set** (Section 5.2.1) before insert per G2 -- a
re-run for the same SHA whose prior rows are still active is a
no-op (the row is already there); a re-run after a retraction of
the prior rows lands a fresh active row at the same quintuple,
because retracted rows are tombstoned out of the uniqueness check.

### 3.2 AST Adapter

**Purpose.** The single front end for all source-code parsing
(per G7). Walks a working tree at a given SHA, emits
language-normalised AST nodes that the Compute Engine consumes.

**Modes (per `Repo.mode`).**

- `embedded` (default; operator pin `ast-mode-default`, Section 1.6)
  -- the Adapter ships its own tree-sitter grammars and is
  standalone. No dependency on `services/agent-memory/`. This is
  ast-metrics-style.
- `linked` -- the Adapter reuses `agent-memory.GraphReader` to
  resolve cross-repo call edges. It still does its own per-file
  parsing; only the **cross-repo edge resolution** is delegated.
  `ScopeBinding.agent_memory_node_id` is populated in this mode
  (Section 5.2.3) so the two surfaces can be joined.

In both modes the Adapter mints `scope_id` from the same
canonical-signature recipe (G2), so switching modes preserves
historical samples.

**Output.** Stream of `(scope_kind, canonical_signature,
ast_node)` tuples consumed by the Compute Engine. The Adapter does
not write to the Metrics Store directly.

### 3.3 Repo Indexer

**Purpose.** Watches the git remote (or accepts webhooks from a
git host) and writes `Commit` rows when a new SHA appears. Owns
the `Commit` writer slot in the Catalog / Lifecycle sub-store
(delegated by Management, which is the formal G1 writer).

**Initial `scan_status` is a DB default, not an Indexer write.**
The Indexer INSERTs `Commit` rows naming **only** the columns
`(repo_id, sha, parent_sha, committed_at)`. The `scan_status`
column is **omitted from the INSERT column list**; the database
schema declares `scan_status enum NOT NULL DEFAULT 'pending'`, so
the engine supplies `'pending'` on the new row without any
application component writing the column. This keeps the
normative claim in Section 1.5.1 row 1 -- "the Metric Ingestor is
the only writer of `Commit.scan_status`" -- literally true at the
application layer: the Indexer never names the column on INSERT
and never updates the column afterward, and the `'pending'`
initial value is contributed by the DB schema, not by any
component. The Metric Ingestor is the only component that ever
writes `Commit.scan_status` explicitly (transitioning
`pending -> scanning -> scanned | failed`).

The Indexer is intentionally thin -- a few lines of glue between a
git event source and the Catalog table. The Metric Ingestor takes
over the moment a `Commit` row exists.

### 3.4 Compute Engine

**Purpose.** Consumes the AST stream from Section 3.2 and emits
one `MetricSample` row per `(scope, metric_kind)` per SHA. Writes
through the Metric Ingestor (the single G1 writer of `MetricSample`).

**Per-language deterministic recipes.** Each metric in the
foundation tier (Section 1.4.1) has a deterministic recipe keyed
by `(language, metric_kind, metric_version)`. The recipe is
expressed in terms of tree-sitter node kinds, not in
language-specific code paths in the Compute Engine -- per G7 the
Compute Engine is language-agnostic.

**Outputs.** `MetricSample` rows with `source='computed'`,
`pack IN ('base', 'solid')`, `degraded=false`. (Computed rows are
never `degraded=true`: if an input is missing the row is not
written, not stamped degraded -- only system-tier derivation
stamps `degraded=true` per Section 8.2.)

### 3.5 Compute Engine -- SOLID inputs

The SOLID rule pack (Section 3.6) does not introduce new
foundation-tier metric kinds; it composes the metrics in Section
1.4.1 in specific ways.

#### 3.5.1 SOLID input recipes

- **a. SRP (Single Responsibility).** Inputs: `lcom4` (class),
  `interface_width` (class). A class with `lcom4 >= threshold AND
  interface_width >= threshold` is flagged.
- **b. OCP (Open / Closed).** Inputs: `fan_in` (class),
  `modification_count_in_window` (class). A class that is widely
  depended-on (`fan_in >= threshold`) and frequently edited
  (`modification_count_in_window >= threshold`) is the canonical
  OCP smell. `window_days` for `modification_count_in_window` is
  drawn from `PolicyVersion.refactor_weights.window_days`
  (Section 5.3.3).
- **c. LSP (Liskov Substitution).** Inputs: `depth_of_inheritance`
  (class) plus an AST-level "overrides parent precondition / weakens
  postcondition" predicate from the Adapter (Section 3.2). Recorded
  as a single boolean `lsp_violation` attribute on the
  `MetricSample.attrs_json` AND projected as a first-class
  `metric_kind='lsp_violation'` row (Section 1.4.1 row 13,
  `scope_kind='method'`, `value=0|1`) so the Policy DSL --
  which exposes only the columnar `{metric_kind, scope_kind, pack,
  source, value, degraded}` fields and not `attrs_json` -- can
  consume the indicator directly. The dual encoding (attrs_json
  boolean for forensics + projected `metric_kind` row for DSL
  consumption) mirrors `cycle_member` (Section 1.4.1 row 10), which
  also surfaces the structural fact as a 0/1 metric_kind while
  retaining detail (the cycle id) on `attrs_json`.
- **d. ISP (Interface Segregation).** Inputs: `interface_width`
  (class, interface), `fan_in` (interface). A wide interface
  consumed by a small subset of its methods is the smell.
- **e. DIP (Dependency Inversion).** Inputs: `coupling_between_objects`,
  `fan_out`. A class with high CBO and high fan-out into concrete
  types (rather than abstractions) is flagged.

### 3.6 SOLID Rule Engine

**Purpose.** Evaluates `Rule` rows in the active `PolicyVersion`
against `MetricSample` rows to produce `Finding` rows. Operates
in two modes:

- *Synchronous mode* -- called from `eval.gate` (Section 3.7).
  Reads the policy, reads the sample subset for the gate scope,
  writes one `EvaluationRun` + one `EvaluationVerdict` + N
  `Finding` rows.
- *Batch refresh mode* -- a worker that re-runs the active
  `PolicyVersion` against the latest `MetricSample` rows after a
  large ingest (Section 4.7). Same write set, but
  `EvaluationRun.caller='batch_refresh'`. This is the writer slot
  recorded in the G1 Audit / verdict ACL above.

**Finding semantics.** Each `Finding` row carries `rule_id`,
`rule_version`, `metric_sample_ids` (array of FKs that triggered
it), `policy_version_id`, `severity`, `delta`, and a human-readable
`explanation_md`. The `delta` discriminator is normative -- see
Section 5.4.1.

### 3.7 Evaluator Surface

**Purpose.** Hosts the gate verb `eval.gate(policy_version_id?,
repo_id, sha, scope?)` consumed by the evaluator agent on every
change. Pinned semantics:

1. Resolves the active `PolicyVersion` if the caller did not pin
   one (per the latest `PolicyActivation` row, Section 5.3.4).
2. **Verifies the `PolicyVersion.signature`** (operator pin
   `policy-signing-required`, Section 1.6; Section 8.3 reliability
   invariant). On signature mismatch the verdict is `warn` with
   `degraded_reason='policy_signature_invalid'` (Section 8.2);
   the call does **not** block.
3. Loads the sample set for `(repo_id, sha, scope?)` and, if any
   required input is `samples_pending`, returns `verdict='warn'`
   with `degraded_reason='samples_pending'` (operator pin
   `gate-degraded-policy=warn`, Section 1.6 -- the gate **never**
   blocks on missing samples).
4. Delegates the rule pass to the SOLID Rule Engine
   (synchronous mode) and writes the resulting
   `EvaluationRun` / `EvaluationVerdict` / `Finding` rows.
5. Returns `(verdict, findings[], degraded?, degraded_reason?)`
   to the caller.

### 3.8 Insights Surface

**Purpose.** Read-only verbs for the operator UI (per Section
6.3). Joins `MetricSample`, `Finding`, `RepoMetricSnapshot`,
`CrossRepoPercentile`, and `PortfolioSnapshot` to answer "how is
this repo doing", "where is the worst hotspot", "where does this
repo sit on the portfolio curve".

Per G6, when the percentile view is stale the surface returns
`degraded=true` with `degraded_reason='percentile_stale'` (Section
8.4) so the UI can render a freshness banner.

### 3.9 Refactor Planner

**Purpose.** Post-ingest worker that turns `Finding` plus
`MetricSample` rows into `HotSpot`, `RefactorPlan`, and
`RefactorTask` rows. Reads `PolicyVersion.refactor_weights`
(Section 5.3.3) for the composite-score weights and the effort
model pin.

**Composite hotspot score.** Per scope:

```
score = alpha * complexity_z
      + beta  * churn_z
      + gamma * coupling_z
      + delta * finding_count
```

where `alpha`, `beta`, `gamma`, `delta` are the per-policy weights
from `PolicyVersion.refactor_weights` and the `_z` suffixes are
robust z-scores over the repo's foundation-tier distribution.

**Effort estimate.** Per operator pin `refactor-effort-source`
(Section 1.6), `RefactorTask.effort_hours` is produced by an **ML
model trained on historical commits**. The model version that
produced each row is pinned by
`PolicyVersion.refactor_weights.effort_model_version` (Section
5.3.3) so a model retraining is itself a policy publish.

**Wen Zhong principle.** The Refactor Planner **never** mutates
source code. Every output is a row, never a patch.

### 3.10 Cross-Repo Aggregator

**Purpose.** Cadence-driven worker that materialises portfolio
views and system-tier `MetricSample` rows. Per G1 sub-store
carve-out, it is the **only** writer of `pack='system'` rows. Per
G6, all snapshot / percentile / portfolio outputs are derived
views.

**Cadence.** Default `15m` (Section 8.4). The cadence is shorter
than the `freshness_window_seconds` policy default (`3600`) so the
freshness banner has 3-4x headroom under nominal load.

**Steps per tick.**

1. Reads the latest `MetricSample` rows added since the last tick.
2. Materialises `RepoMetricSnapshot` per `(repo_id, metric_kind,
   scope_kind)`: a small fixed-cardinality summary of each repo's
   distribution (count, mean, median, p90, p99).
3. Materialises `CrossRepoPercentile` per `(metric_kind,
   scope_kind)`: the full per-metric percentile vector across all
   repos. This is the input to the operator "portfolio" view
   (Section 6.3).
4. **Materialises the system-tier metrics** introduced in
   Section 1.4.2 (`xrepo_dep_depth`, `arch_debt_ratio`,
   `blast_radius`, `velocity_trend`, `xservice_test_reliability`,
   `arch_fitness`, `knowledge_index`) by composing foundation-tier
   `MetricSample` rows (and, in `linked` mode, agent-memory
   cross-repo edges per Section 8.7). For `xservice_test_reliability`
   the foundation input is the `pack='ingested'` row
   `pass_first_try_ratio` written by the External Metric Ingest
   Webhook on `ingest.test_balance` (Section 3.12); the Aggregator
   is the only writer that promotes it to the `pack='system'`,
   `source='derived'` `xservice_test_reliability` row, preserving
   the Measurement-sub-store ACL in G1. Each system-tier row is
   written as a `MetricSample` with `pack='system'` and
   `source='derived'`. When a required input is unavailable (e.g.
   no cross-repo edges in `embedded` mode, or no
   `pass_first_try_ratio` row yet ingested), the system-tier row is
   still written and stamped with `degraded=true` and
   `degraded_reason='xrepo_edges_unavailable'` or `'samples_pending'`
   (the two `MetricSample` fields added in Section 5.2.2 for
   exactly this purpose; values are drawn from the Section 8.2
   closed list); the row is never silently dropped.

Per **G6** the snapshot / percentile / portfolio rows are a
materialised view; they can be rebuilt deterministically from
`MetricSample` at any time. The system-tier `MetricSample` rows
are likewise derived and can be re-materialised from the
foundation tier; the Aggregator is the only writer per the
Measurement-sub-store ACL in G1.

### 3.11 Policy Steward (operator-facing writer)

**Purpose.** Owns the **policy lifecycle**: lets an operator
publish a new `PolicyVersion` (Section 5.3.3), edit a `Rule`
(Section 5.3.1), publish a `RulePack` (Section 5.3.2), or append
a `PolicyActivation` (Section 5.3.4) or `Override` (Section 5.3.6)
row. Single G1 writer of the Policy sub-store.

Every `PolicyVersion` published through the Steward is signed
(operator pin `policy-signing-required`, Section 1.6); the
signing key is held by the Steward process and not exposed to
other components.

### 3.12 External Metric Ingest Webhook

**Purpose.** Public webhook for foreign-data ingest: coverage,
churn, defects, test-balance. Authenticated; per-source signing
key. The webhook **enqueues** work for the Metric Ingestor; the
Ingestor is still the only G1 writer of the Measurement sub-store.

Per operator pin `external-metric-coverage-format` (Section 1.6),
`ingest.coverage` accepts **Cobertura XML** as the v1 canonical
payload. Other formats (lcov, JaCoCo) MAY be added in v2 but are
out of scope here.

Per Section 1.5.1 row 2 the webhook **never** writes the system-tier
`xservice_test_reliability` row directly; it writes only the
foundation-tier `pass_first_try_ratio` input row on
`ingest.test_balance`. The Aggregator (Section 3.10 step 4) is the
sole writer of the system-tier row.

### 3.13 Management surface

**Purpose.** Lifecycle / control-plane verbs (`mgmt.*`,
Section 6.3): register a repo, retract a sample, read public
views. Writer of `Repo`, `Commit` (sans `scan_status`; Ingestor
owns transitions), and `RepoEvent` (Catalog / Lifecycle
sub-store).

`mgmt.retract_sample(sample_id, reason)` does not write
`MetricRetraction` directly -- that table is in the Measurement
sub-store, owned by the Ingestor (G1). The Management surface
appends a `RepoEvent` row recording the intent, and delegates to
the Ingestor which appends the `MetricRetraction` row. This
preserves the single-writer-per-sub-store rule.

---

## 4. End-to-end sequence flows

### 4.1 New commit -> foundation-tier scan

1. **Repo Indexer** observes a new SHA on the watched remote and
   INSERTs a `Commit` row naming **only** `(repo_id, sha,
   parent_sha, committed_at)`; the `scan_status` column is
   **omitted from the INSERT** and the DB-level column DEFAULT
   (`scan_status enum NOT NULL DEFAULT 'pending'`, Section 3.3
   and Section 5.1.2) supplies the initial `'pending'` value. No
   application component (neither the Indexer nor any other)
   writes `Commit.scan_status` at this step, which preserves the
   Section 1.5.1 row 1 invariant that the Metric Ingestor is the
   only writer of that column. (Per G1 the Indexer is delegated
   by Management, which owns the Commit writer slot in the
   Catalog sub-store.)
2. **Metric Ingestor** picks up the pending Commit, opens a
   `ScanRun(kind='full', sha_binding='single', to_sha=sha)` row,
   and flips `Commit.scan_status='scanning'`.
3. Ingestor invokes the **AST Adapter** (Section 3.2) over the
   working tree at `sha`.
4. Ingestor invokes the **Compute Engine** (Section 3.4) which
   emits `MetricSample` rows (`source='computed'`, `pack IN
   ('base', 'solid')`, `degraded=false`).
5. Ingestor writes the rows (and any new `ScopeBinding` rows for
   first-seen scopes) into the Measurement sub-store.
6. Ingestor flips `Commit.scan_status='scanned'` and closes the
   `ScanRun`.
7. The **SOLID Rule Engine** batch worker (Section 3.6) picks
   up the new sample set and runs the active `PolicyVersion`
   (see Section 4.7 for the post-ingest refresh).
8. The **Cross-Repo Aggregator** picks up the new rows on its
   next tick and materialises snapshot / percentile / system-tier
   rows (Section 3.10).

### 4.2 Evaluator gate (`eval.gate`)

1. Evaluator agent calls `eval.gate(repo_id, sha, scope=PR-diff)`.
2. Evaluator Surface resolves the active `PolicyVersion` and
   **verifies its signature** (operator pin `policy-signing-required`,
   Section 1.6). On mismatch: `verdict='warn'` with
   `degraded_reason='policy_signature_invalid'`; return.
3. Surface loads the `MetricSample` rows for `(repo_id, sha,
   scope?)`. If any required input is `samples_pending`:
   `verdict='warn'` with `degraded_reason='samples_pending'`
   (operator pin `gate-degraded-policy=warn`); return.
4. Surface invokes the **SOLID Rule Engine** in synchronous mode.
5. Engine writes `EvaluationRun` (`caller='eval_gate'`), one
   `EvaluationVerdict`, and N `Finding` rows.
6. Surface returns `(verdict, findings[], degraded?,
   degraded_reason?)` to the evaluator agent.

### 4.3 External coverage payload arrives

1. CI emits a Cobertura XML payload on PR merge.
2. **External Metric Ingest Webhook** authenticates the call and
   enqueues a `ScanRun(kind='external_single',
   sha_binding='single', to_sha=sha)` job for the Metric Ingestor.
3. Ingestor parses the payload and emits `MetricSample` rows for
   `coverage_line_ratio` (and similar) with `source='ingested'`,
   `pack='ingested'`.
4. Ingestor closes the `ScanRun`. The Aggregator picks the new
   rows up on its next tick.

### 4.4 External churn payload arrives (per-row SHA)

1. CI emits a churn payload covering N commits; each row has its
   own SHA.
2. Webhook enqueues a `ScanRun(kind='external_per_row',
   sha_binding='per_row', to_sha=NULL)` job for the Ingestor.
3. Per metric_kind, the Ingestor uses the appropriate writer:
   - For raw per-row sample kinds (e.g. the future `velocity_trend`
     / `knowledge_index` system-tier inputs, which need to retain
     per-commit fidelity), the Ingestor emits **one `MetricSample`
     row per payload row**, each referencing its own SHA from the
     payload. Per G2 the
     `(repo_id, sha, scope_id, metric_kind, metric_version)` key
     remains unique across rows because the SHA differs.
   - For **computed-window** kinds whose value is an aggregate
     over the payload's rows (the canonical example is
     `modification_count_in_window` -- arch Sec 1.4.1 row 12 -- a
     COUNT of unique in-window SHAs per scope), the
     `modification_count_materialiser` (a computing writer per
     tech-spec Sec 4.1.1 lines 287-291 + Sec 4.11 lines 444-454)
     emits **one `MetricSample` row per scope**, stamped with the
     LATEST in-window SHA. The `(repo_id, sha, scope_id,
     metric_kind, metric_version)` key is still unique because
     only one row per scope is emitted for this metric_kind in
     this ScanRun.
4. Ingestor closes the `ScanRun`. Aggregator materialises
   `velocity_trend` / `knowledge_index` system-tier rows on its
   next tick.

### 4.5 Operator asks "where does my repo sit on the portfolio?"

1. Operator calls `mgmt.read.cross_repo(repo_id, metric_kind,
   scope_kind, sha?)` (Section 6.3).
2. Insights Surface looks up the most recent `CrossRepoPercentile`
   row for `(metric_kind, scope_kind)`.
3. If the row's `built_at` is older than the policy-defined
   freshness window, the surface returns `degraded=true` with
   `degraded_reason='percentile_stale'` (Section 8.4).
4. Reply carries `p50 / p90 / p99` plus a per-repo histogram so
   the UI can render "where does my repo sit on the portfolio
   curve".

### 4.6 Operator mutes a noisy rule (kill switch -- G5)

1. Operator opens a rule in the UI and submits
   `mgmt.override(rule_id, mute=true, reason, scope?)` on the
   Management surface.
2. Management delegates to the **Policy Steward** (the G1 writer
   of the Policy sub-store).
3. Steward appends an `Override` row (Section 5.3.6). The latest
   row by `created_at` for a `(rule_id, scope_filter)` pair
   defines the current mute state.
4. The next `eval.gate` call observes the new Override and
   suppresses findings for the muted rule (the rows are still
   emitted as `Finding` with `severity='info'` so the audit trail
   is preserved).

### 4.7 Post-ingest finding refresh (batch mode)

1. After a `ScanRun` closes for a repo, a Metric Ingestor sentinel
   row triggers the **SOLID Rule Engine batch worker**.
2. Worker reads the active `PolicyVersion` and the new sample set.
3. Worker writes one `EvaluationRun` (`caller='batch_refresh'`),
   one `EvaluationVerdict`, and N `Finding` rows (where the
   `delta` discriminator marks each finding as `new`,
   `newly_failing` (regression-to-block bucket; see Section 5.4.1
   normative definition), `unchanged`, or `resolved`).
4. The Refactor Planner consumes the new `Finding` rows and
   (re-)materialises `HotSpot` / `RefactorPlan` / `RefactorTask`
   rows for the repo (Section 3.9). The Cross-Repo Aggregator
   picks the new rows up on its next tick.

---

## 5. Data model

This section defines the entity catalogue (one heading per table)
plus the column-level field tables that downstream contracts pin
against. Every table belongs to exactly one G1 sub-store
(Section 1.5).

### 5.1 Catalog / Lifecycle sub-store

#### 5.1.1 Repo

| Field | Type | Notes |
| --- | --- | --- |
| `repo_id` | uuid | Primary key. |
| `display_name` | text | Free-form. |
| `repo_url` | text? | Operator-supplied canonical repo URL (e.g. `https://github.com/org/repo`). Nullable for back-compat with rows inserted before migration `0006_repo_url.up.sql`; new rows inserted via `mgmt.register_repo` MUST supply a non-empty URL. **WRITE-ONCE post-registration** -- changing this value would break canonical-signature parity (Section 5.2.1) and the G2 guarantee, because every `scope_binding.canonical_signature` for the repo embeds this URL as the per-repo stamp. The Metric Ingestor's `PGRepoURLLookup` reads this column once per `ResolveScopeIDs` call (cached for the process lifetime); `display_name` was REJECTED as the URL source (iter-6 evaluator item 1) because it is free-form per this section's row 2 and covered by Management UPDATE grants (`mgmt.rename_repo`). |
| `mode` | enum | `embedded` (default; operator pin `ast-mode-default`, Section 1.6) or `linked`. Determines AST Adapter mode (Section 3.2). |
| `default_branch` | text | E.g. `main`. |
| `default_branch_head` | text? | Head SHA of `default_branch`, cached by the Repo Indexer on push/merge webhooks so the Insights surface can answer "what's current?" with a single-row read instead of scanning `Commit`. Nullable until the first scan lands. The composite index `(repo_id, default_branch_head)` (implementation-plan Stage 1.2) backs the index-only-scan shape. The Repo Indexer (Section 3.3) is the only writer. |
| `created_at` | timestamp | Append-only. |

#### 5.1.2 Commit

| Field | Type | Notes |
| --- | --- | --- |
| `repo_id` | uuid | FK -> `Repo`. Part of composite primary key with `sha`. |
| `sha` | text | Part of composite primary key. |
| `parent_sha` | text | Nullable for the first commit. |
| `committed_at` | timestamp | Author / committer timestamp from git. |
| `scan_status` | enum, `NOT NULL DEFAULT 'pending'` | Allowed values `pending`, `scanning`, `scanned`, `failed`. The **initial `'pending'` value is supplied by the schema-level column DEFAULT on INSERT** -- the Repo Indexer (Section 3.3) omits this column from its INSERT column list, so no application component writes `'pending'` explicitly. **The Metric Ingestor is the only application writer of this column** and is the only component that may transition it (`pending -> scanning -> scanned | failed`) per Section 1.5.1 row 1. Management writes the rest of the `Commit` row via its Repo Indexer delegate (Section 3.3); no component writes any other `Commit` column after row creation. |

#### 5.1.3 MetricKind

| Field | Type | Notes |
| --- | --- | --- |
| `metric_kind` | text | Primary key. Matches the catalogue in Section 1.4. |
| `metric_version` | int | Monotonic; bumped on definitional change (G2). Part of the natural key in `MetricSample`. |
| `tier` | enum | `foundation`, `system`. |
| `pack` | text | `base`, `solid`, `ingested`, or `system`. |
| `unit` | text | E.g. `count`, `ratio`, `seconds`. |
| `description_md` | text | Human description. |

#### 5.1.4 RepoEvent

| Field | Type | Notes |
| --- | --- | --- |
| `event_id` | uuid | Primary key. |
| `repo_id` | uuid | FK -> `Repo`. |
| `kind` | enum | `registered`, `retired`, `retract_intent`, `mode_changed`. |
| `payload_json` | json | Per-kind payload. |
| `created_at` | timestamp | Append-only. |

### 5.2 Measurement sub-store

#### 5.2.1 MetricSample (canonical row of the service)

| Field | Type | Notes |
| --- | --- | --- |
| `sample_id` | uuid | Primary key. Server-generated. |
| `repo_id` | uuid | FK -> `Repo`. Part of the natural key. |
| `sha` | text | Part of the natural key. |
| `scope_id` | uuid | FK -> `ScopeBinding`. Part of the natural key. |
| `metric_kind` | text | FK -> `MetricKind`. Part of the natural key. |
| `metric_version` | int | Part of the natural key (G2). |
| `value` | double | Numeric value. |
| `pack` | enum | `base`, `solid`, `ingested`, `system`. |
| `source` | enum | `computed` (from the Compute Engine, Section 3.4), `ingested` (from the External Metric Ingest Webhook, Section 3.12), or `derived` (composed by the Cross-Repo Aggregator from other `MetricSample` rows, Section 3.10 step 4 -- the only path that produces `pack='system'` rows). |
| `degraded` | bool | `true` iff the sample was produced under any condition in the Section 8.2 closed list (e.g. a system-tier row written without its full input set). Defaults to `false`. The Cross-Repo Aggregator (Section 3.10 step 4) is the only writer that sets this to `true` in v1 -- foundation-tier (`source='computed'`) and ingested-tier (`source='ingested'`) rows are always written with `degraded=false` because they are produced from inputs that are either present or the row is not written at all. Settable at insert only per G3. |
| `degraded_reason` | text? | Set iff `degraded=true`. Drawn from the **Section 8.2 closed list** (`xrepo_edges_unavailable`, `samples_pending`, `policy_signature_invalid`, `percentile_stale`). Null when `degraded=false`. |
| `producer_run_id` | uuid | FK -> `ScanRun` (Section 5.7). Attribution. |
| `attrs_json` | json | Per-metric attributes (e.g. cycle id for `cycle_member`). Insert-time only. |
| `created_at` | timestamp | Append-only. |

> **Row immutability (G3).** No column on `MetricSample` is ever
> updated after insert. A retraction is an append to
> `MetricRetraction` (Section 5.2.2). The `degraded` /
> `degraded_reason` fields are likewise insert-only -- a sample
> that was written `degraded=true` because an input was missing
> at the Aggregator tick stays `degraded=true` forever at that
> SHA as the audit trail. **The corrected non-degraded row is
> not written at the same SHA** (that would collide with the
> uniqueness key below and break G2/G3); when the missing input
> later arrives, the **next** Aggregator tick that runs at a
> **later HEAD SHA** writes the corrected non-degraded
> `MetricSample` at that new SHA. The old degraded row remains
> queryable at its original SHA forever (G3 + G6).
>
> **Read-time semantics (normative -- the only correct way to
> consume around degraded rows).** Two read modes are defined
> on the **Management surface read verbs** (Section 3.13 /
> Section 6.3 `mgmt.read.*`) and the Evaluator Surface (Section
> 3.7 / Section 6.2 `eval.gate`); every reader MUST declare
> which mode it is in. **The mode is mechanically determined by
> whether the caller supplies a `sha` argument to the verb** --
> there is no separate "read API" component, only the per-verb
> SHA shape defined in Section 6.3:
>
> - **SHA-pinned reads** (`eval.gate`; any `mgmt.read.*` verb
>   whose Section 6.3 signature has `sha` as a **required**
>   argument -- `mgmt.read.metric_samples(repo_id, sha, ...)`,
>   `mgmt.read.regressions(repo_id, sha, ...)`,
>   `mgmt.read.refactor_plan(repo_id, sha)` -- and any
>   `mgmt.read.*` verb with optional `sha?` called WITH a `sha`
>   argument -- e.g. `mgmt.read.findings(repo_id, sha=...)`,
>   `mgmt.read.cross_repo(..., sha=...)`; plus the Refactor
>   Planner's internal reads at Section 3.9) MUST return the
>   row at the exact requested SHA. If the row at that SHA is
>   degraded, the reply carries `Degraded { reason }` (Section
>   6.1) and the `gate-degraded-policy=warn` rule (Section 1.6)
>   governs the resulting verdict. **A SHA-pinned reader NEVER
>   silently substitutes a later-SHA value for an earlier-SHA
>   query.** This preserves G2 (the `(repo_id, sha, scope_id,
>   metric_kind, metric_version)` identity), keeps every
>   historical verdict reproducible, and means a `Finding` or
>   `RefactorTask` persisted against a SHA always cites a row
>   that actually existed at that SHA.
> - **Latest-dashboard reads** (`mgmt.read.*` verbs whose
>   Section 6.3 signature has **no** `sha` argument --
>   `mgmt.read.repo(repo_id)`, `mgmt.read.portfolio(metric_kind,
>   scope_kind)` -- and any `mgmt.read.*` verb with optional
>   `sha?` called WITHOUT a `sha` argument -- e.g.
>   `mgmt.read.findings(repo_id)`, `mgmt.read.cross_repo(...)`;
>   plus the Insights Surface (Section 3.8) dashboards, which
>   call these no-`sha` / optional-`sha` verbs under the hood)
>   MAY substitute the latest non-degraded row per
>   `(repo_id, scope_id, metric_kind, metric_version)` via
>   `MAX(Commit.committed_at)` to give a "current best estimate"
>   view. These substitutions are NEVER persisted into a
>   `Finding`, a `RefactorTask`, or an `EvaluationVerdict`;
>   they are read-only UI projections and the reply explicitly
>   stamps `substituted_from_sha` on the response envelope so
>   the UI can render "as of SHA X" rather than the query SHA.
>
> The two modes are disjoint by design: a single call is
> either SHA-pinned or latest-dashboard, never both. The
> Evaluator Surface (Section 3.7) is always SHA-pinned; the
> Refactor Planner (Section 3.9) is always SHA-pinned; the
> Insights Surface (Section 3.8) dashboards are always
> latest-dashboard. Insights queries that need SHA-pinned
> reproducibility go through `mgmt.read.metric_samples(repo_id,
> sha, ...)` or `mgmt.read.findings(repo_id, sha=...)` on the
> Management surface (Section 3.13 / Section 6.3) instead of
> through the dashboard verbs.
>
> If an operator explicitly needs the degraded row removed from
> SHA-pinned gate inputs before a new HEAD SHA arrives, they
> call `mgmt.retract_sample` (Section 6.3), which appends a
> `MetricRetraction` row referencing the degraded sample id;
> SHA-pinned readers filter retracted rows out of consideration.
> A subsequent `mgmt.rescan(repo_id, sha)` (Section 6.3) re-runs
> the scan at the same SHA to land a fresh **active** row -- the
> new row has its own `MetricSample.sample_id` and the uniqueness
> check defined in the Uniqueness paragraph below is over the
> active row set only, so the prior retracted row (now a
> tombstone) does not block the insert.
>
> **Uniqueness (active-row-only -- this is the single normative
> definition referenced by G2 and by Section 3.1 idempotency).**
> Across all three `source` values (`computed`, `ingested`,
> `derived`), at any instant **at most one active row** exists
> per `(repo_id, sha, scope_id, metric_kind, metric_version)`,
> where a row is **active** iff no `MetricRetraction` row
> (Section 5.2.2) references its `sample_id`. Retracted rows are
> tombstones: they remain queryable as audit trail (G3) but they
> do NOT contribute to the uniqueness check and they do NOT
> appear in SHA-pinned reads. The active-row constraint is
> enforced by a partial unique index on the quintuple
> `WHERE sample_id NOT IN (SELECT sample_id FROM
> MetricRetraction)` (the exact DDL is in `tech-spec.md`).
> Practical implications:
>
> - Re-running the Metric Ingestor for the same SHA whose prior
>   rows are still active is a no-op (the active row is already
>   there). Re-running after a retraction lands a fresh active
>   row at the same quintuple (the retracted row is a tombstone).
> - A definitional bump (`metric_version`) is a *new active row*,
>   never a rewrite; the prior row stays active at its prior
>   `metric_version`.
> - For `source='derived'` rows the Cross-Repo Aggregator (Section
>   3.10 step 4) writes at most one row per quintuple per HEAD
>   SHA per tick; if its tick lands on a SHA where an **active**
>   derived row already exists (degraded or not), it **skips the
>   insert** for that SHA and waits for the next HEAD SHA. If the
>   prior derived row has been retracted (e.g. by `mgmt.retract_sample`
>   targeting a degraded derived sample), the next tick at the
>   same SHA may write a fresh active derived row at that
>   quintuple. This keeps active-row uniqueness uniform across
>   all three `source` values and makes G2's identity contract
>   hold without exception.

#### 5.2.2 MetricRetraction

| Field | Type | Notes |
| --- | --- | --- |
| `retraction_id` | uuid | Primary key. |
| `sample_id` | uuid | FK -> `MetricSample`. The row being retracted. |
| `reason` | text | Free-form (e.g. "file is vendored"). |
| `appended_by` | text | `ingestor` or `operator:<actor_id>`. |
| `created_at` | timestamp | Append-only. |

> **G3 mechanics.** Retraction never mutates the original
> `MetricSample` row. Readers (Insights, Evaluator, Refactor
> Planner) MUST filter out samples with a matching retraction row.

#### 5.2.3 ScopeBinding

| Field | Type | Notes |
| --- | --- | --- |
| `scope_id` | uuid | Primary key. **Stable across SHAs** for the same `canonical_signature` (G2). Deterministic uuid from `(repo_id, scope_kind, canonical_signature, first_seen_sha)`. |
| `repo_id` | uuid | FK -> `Repo`. |
| `scope_kind` | enum | `repo`, `package`, `file`, `class`, `interface`, `method`, `block`. |
| `canonical_signature` | text | Language-stable identifier (e.g. `pkg.Foo#bar(int)`). Same recipe as agent-memory `Node.canonical_signature`. |
| `first_seen_sha` | text | SHA at which this signature first appeared. |
| `agent_memory_node_id` | uuid? | Set iff the service is running in `linked` mode (Section 3.2). When set, this is the agent-memory `Node.node_id` (anchor: agent-memory architecture Section 5.2.1, lines 430-431). |
| `attrs_json` | json | Language-specific attributes (visibility, return type, etc.). Insert-time only. |
| `created_at` | timestamp | Append-only. |

#### 5.2.4 RepoMetricSnapshot

| Field | Type | Notes |
| --- | --- | --- |
| `snapshot_id` | uuid | Primary key. |
| `repo_id` | uuid | FK -> `Repo`. |
| `metric_kind` | text | Part of natural key. |
| `scope_kind` | enum | Part of natural key. |
| `count` | bigint | Sample count. |
| `mean` | double | Mean value. |
| `p50` | double | Median. |
| `p90` | double | 90th percentile. |
| `p99` | double | 99th percentile. |
| `built_at` | timestamp | When the Aggregator built this row. |

#### 5.2.5 CrossRepoPercentile

| Field | Type | Notes |
| --- | --- | --- |
| `percentile_id` | uuid | Primary key. |
| `metric_kind` | text | Part of natural key. |
| `scope_kind` | enum | Part of natural key. |
| `histogram_json` | json | Per-repo histogram for portfolio UI rendering. |
| `p50` | double |  |
| `p90` | double |  |
| `p99` | double |  |
| `built_at` | timestamp | Freshness clock (G6 / Section 8.4). |

#### 5.2.6 PortfolioSnapshot

| Field | Type | Notes |
| --- | --- | --- |
| `portfolio_snapshot_id` | uuid | Primary key. |
| `metric_kind` | text |  |
| `scope_kind` | enum |  |
| `repo_count` | int | Number of repos contributing. |
| `aggregate_json` | json | Operator-pinned aggregate shape. |
| `built_at` | timestamp |  |

### 5.3 Policy / rules sub-store

#### 5.3.1 Rule

| Field | Type | Notes |
| --- | --- | --- |
| `rule_id` | text | E.g. `solid.srp.lcom4_high`. Primary key with `version`. |
| `version` | int | Monotonic. |
| `pack_id` | text | FK -> `RulePack.pack_id`. |
| `predicate_dsl` | text | DSL expression over `MetricSample` rows. |
| `severity_default` | enum | `info`, `warn`, `block`. May be overridden per-policy in `threshold_refs`. |
| `description_md` | text | Human description. |
| `created_at` | timestamp | Append-only. |

#### 5.3.2 RulePack

> **Sub-store placement (G1).** `RulePack` is a **Policy / rules**
> sub-store row (Section 1.5 G1 ACL row 4, Section 1.5.1 row 4),
> sitting alongside `Rule` and `PolicyVersion` (which it bundles).
> Writer: Policy Steward (Section 3.11). No other component writes
> `RulePack`. `RulePack` is NOT a Catalog / Lifecycle row.

| Field | Type | Notes |
| --- | --- | --- |
| `pack_id` | text | E.g. `solid.srp`, `solid.dip`, `decoupling.cycles`, `base.complexity`. Primary key together with `version`. |
| `version` | int | Monotonic. |
| `display_name` | text | E.g. "Single Responsibility". |
| `description_md` | text | Human description. |
| `created_at` | timestamp | Append-only. |

#### 5.3.3 PolicyVersion (G5)

| Field | Type | Notes |
| --- | --- | --- |
| `policy_version_id` | uuid | Primary key. |
| `name` | text | E.g. "default-v3". |
| `rule_refs` | json | Array of `{rule_id, version}` entries that compose the policy. |
| `threshold_refs` | json | Array of `{threshold_id}` entries pinning the numeric parameters. |
| `refactor_weights` | json | `{alpha, beta, gamma, delta, effort_model_version, window_days, freshness_window_seconds?}` from Section 3.9. `alpha`/`beta`/`gamma`/`delta` are the per-policy composite-score weights consumed by the Refactor Planner (Section 3.9 step 2). `effort_model_version` pins the ML model that produced this policy's `RefactorTask.effort_hours` (operator pin `refactor-effort-source`, Section 1.6). `window_days` (int, positive; default `90` -- pinned in tech-spec) is the commit-window the Metric Ingestor uses to materialise `modification_count_in_window` SOLID input rows on `ingest.churn` arrival (Section 3.1 case 3, Section 3.5.1.b); a policy publish that changes `window_days` re-materialises subsequent windows at the new value but leaves prior rows immutable per G3. `freshness_window_seconds` (int, optional) is the Insights Surface's `percentile_stale` threshold (Section 8.4); defaults to `3600` (= 1 hour). |
| `signature` | bytes | **v1 required** (operator pin `policy-signing-required`, Section 1.6). Cryptographic signature over `rule_refs`, `threshold_refs`, `refactor_weights`. The Evaluator Surface MUST verify this signature on every `eval.gate` call before posting a verdict; a mismatch produces `verdict='warn'`, `degraded=true`, `degraded_reason='policy_signature_invalid'` (Section 8.2). Algorithm pinned in tech-spec. |
| `created_at` | timestamp | Append-only. |

> **PolicyVersion is immutable.** Activation is *not* a column
> on this table; it is recorded by appending a `PolicyActivation`
> row (Section 5.3.4). The "currently active" policy is the
> `policy_version_id` of the most recent `PolicyActivation` row.
> This preserves G5 (the row itself is signed and never edited)
> and keeps the audit trail of every activation flip.

#### 5.3.4 PolicyActivation (G5)

| Field | Type | Notes |
| --- | --- | --- |
| `activation_id` | uuid | Primary key. |
| `policy_version_id` | uuid | FK -> `PolicyVersion`. |
| `activated_by` | text | Operator id. |
| `created_at` | timestamp | Append-only. The latest row by `created_at` defines the active policy. |

#### 5.3.5 Threshold

| Field | Type | Notes |
| --- | --- | --- |
| `threshold_id` | uuid | Primary key. |
| `metric_kind` | text |  |
| `scope_kind` | enum |  |
| `op` | enum | `gt`, `ge`, `lt`, `le`, `eq`. |
| `value` | double |  |
| `created_at` | timestamp | Append-only. |

#### 5.3.6 Override

| Field | Type | Notes |
| --- | --- | --- |
| `override_id` | uuid | Primary key. |
| `rule_id` | text | FK -> `Rule.rule_id`. |
| `scope_filter` | json | E.g. `{repo_id, scope_kind, scope_signature_glob}`. |
| `mute` | bool | If true, `Finding` rows for matched scope are written with `severity='info'` (preserving audit trail). |
| `reason` | text | Required when `mute=true`. |
| `actor_id` | text | Operator id. |
| `created_at` | timestamp | Append-only. The latest row by `created_at` for a given `(rule_id, scope_filter)` defines the current mute state. |

### 5.4 Audit / verdict sub-store

#### 5.4.1 Finding

| Field | Type | Notes |
| --- | --- | --- |
| `finding_id` | uuid | Primary key. |
| `evaluation_run_id` | uuid | FK -> `EvaluationRun`. |
| `repo_id` | uuid | FK. |
| `sha` | text |  |
| `scope_id` | uuid | FK -> `ScopeBinding`. |
| `rule_id` | text | FK -> `Rule.rule_id`. |
| `rule_version` | int |  |
| `policy_version_id` | uuid | FK. |
| `metric_sample_ids` | json | Array of `MetricSample.sample_id` that triggered this finding (G4). |
| `severity` | enum | `info`, `warn`, `block`. |
| `delta` | enum | `new`, `newly_failing`, `unchanged`, `resolved`. **Normative semantics:** `new` = the rule fired at this scope for the **first SHA** ever; `newly_failing` = the **regression-to-block** bucket -- the rule was previously not `severity='block'` at this scope (either it did not fire, or it fired at a lower severity) and is now `severity='block'` at this scope at this SHA; `unchanged` = the rule was already `severity='block'` at this scope at the previous SHA and is still `severity='block'` at this SHA; `resolved` = the rule was `severity='block'` at the previous SHA and is no longer present (or fires at a lower severity) at this SHA. Insights and Refactor Planner consume `newly_failing` as the **regressions** bucket (per Section 6.3 `mgmt.read.regressions`). |
| `explanation_md` | text | Human-readable explanation slot (G4). |
| `created_at` | timestamp | Append-only. |

#### 5.4.2 EvaluationRun

| Field | Type | Notes |
| --- | --- | --- |
| `evaluation_run_id` | uuid | Primary key. |
| `repo_id` | uuid |  |
| `sha` | text |  |
| `policy_version_id` | uuid |  |
| `caller` | enum | `eval_gate` or `batch_refresh`. |
| `created_at` | timestamp | Append-only. |

#### 5.4.3 EvaluationVerdict

| Field | Type | Notes |
| --- | --- | --- |
| `verdict_id` | uuid | Primary key. |
| `evaluation_run_id` | uuid | FK. |
| `verdict` | enum | `pass`, `warn`, `block`. |
| `degraded` | bool |  |
| `degraded_reason` | text? | From Section 8.2 closed list. |
| `created_at` | timestamp | Append-only. |

### 5.5 Refactor sub-store

#### 5.5.1 HotSpot

| Field | Type | Notes |
| --- | --- | --- |
| `hotspot_id` | uuid | Primary key. |
| `repo_id` | uuid |  |
| `sha` | text |  |
| `scope_id` | uuid |  |
| `score` | double | Composite score from Section 3.9. |
| `policy_version_id` | uuid | The policy whose `refactor_weights` produced the score. |
| `created_at` | timestamp | Append-only. |

#### 5.5.2 RefactorPlan

| Field | Type | Notes |
| --- | --- | --- |
| `plan_id` | uuid | Primary key. |
| `repo_id` | uuid |  |
| `sha` | text |  |
| `hotspot_ids` | json | Array of `HotSpot.hotspot_id` covered by this plan. |
| `summary_md` | text |  |
| `created_at` | timestamp | Append-only. |

#### 5.5.3 RefactorTask

| Field | Type | Notes |
| --- | --- | --- |
| `task_id` | uuid | Primary key. |
| `plan_id` | uuid | FK. |
| `scope_id` | uuid |  |
| `kind` | enum | `split_class`, `extract_method`, `invert_dependency`, `break_cycle`, `consolidate_duplication`, etc. |
| `effort_hours` | double | Produced by the ML model pinned in `PolicyVersion.refactor_weights.effort_model_version` (operator pin `refactor-effort-source`, Section 1.6). |
| `rule_id` | text | The rule that motivated the task. |
| `description_md` | text |  |
| `created_at` | timestamp | Append-only. |

### 5.6 (Reserved -- system-tier carve-out documented in 5.2.1)

System-tier `MetricSample` rows have no separate table; they are
`MetricSample` rows with `pack='system'`, `source='derived'`,
produced exclusively by the Cross-Repo Aggregator (Section 3.10
step 4) per the Section 1.5.1 row 2 reconciliation. The
`RepoMetricSnapshot`, `CrossRepoPercentile`, and
`PortfolioSnapshot` tables (Sections 5.2.4 -- 5.2.6) are the
materialised-view rows the Aggregator also writes; per G6 they
are derived and can be rebuilt deterministically.

### 5.7 ScanRun

> **Sub-store placement (G1).** `ScanRun` is a Catalog /
> Lifecycle row; **the Metric Ingestor is the only writer**
> (per Section 1.5.1 row 1).

| Field | Type | Notes |
| --- | --- | --- |
| `scan_run_id` | uuid | Primary key. |
| `repo_id` | uuid | FK -> `Repo`. |
| `kind` | enum | `full`, `delta`, `external_single`, `external_per_row`, `retract`. |
| `sha_binding` | enum | `single` (one SHA for the whole run; `to_sha` is non-null) or `per_row` (each emitted `MetricSample` row carries its own SHA from the payload; `to_sha` is NULL). |
| `from_sha` | text? | Set for `kind='delta'`. |
| `to_sha` | text? | Set when `sha_binding='single'`; **NULL** when `sha_binding='per_row'` (e.g. churn / defect payloads -- per Section 1.5.1 row 3). |
| `payload_hash` | bytes? | sha256 of the input payload (external modes only); used for idempotency. |
| `started_at` | timestamp |  |
| `ended_at` | timestamp? |  |
| `status` | enum | `running`, `succeeded`, `failed`. |

### 5.8 Entity / sub-store summary

| Sub-store | Tables in this section |
| --- | --- |
| Catalog / Lifecycle | `Repo`, `Commit`, `MetricKind`, `RepoEvent`, `ScanRun` |
| Measurement | `MetricSample`, `MetricRetraction`, `ScopeBinding`, `RepoMetricSnapshot`, `CrossRepoPercentile`, `PortfolioSnapshot` |
| Policy / rules | `Rule`, `RulePack`, `PolicyVersion`, `PolicyActivation`, `Threshold`, `Override` |
| Audit / verdict | `EvaluationRun`, `EvaluationVerdict`, `Finding` |
| Refactor | `HotSpot`, `RefactorPlan`, `RefactorTask` |

---

## 6. Interfaces

All verbs are gRPC; HTTP/JSON gateways are out of scope for this
document (the tech spec pins them). Each verb is named
`<surface>.<verb>` per Section 3 surface names.

### 6.1 Common shapes

- `Degraded { reason: enum-from-Section-8.2 }` -- attached to any
  reply that could not be served from non-degraded inputs.
- `SampleRef { sample_id, repo_id, sha, scope_id, metric_kind,
  metric_version }` -- the natural-key projection used in finding
  citations (G4).

### 6.2 `eval.*` (Evaluator Surface)

- `eval.gate(repo_id, sha, scope?, policy_version_id?) ->
  (verdict, findings[], degraded?)` -- per Section 3.7 and 4.2.
  Pinned semantics:
  - The signature on the resolved `PolicyVersion` MUST be
    verified before any rule pass (operator pin
    `policy-signing-required`, Section 1.6); a mismatch maps to
    `verdict='warn'` with `degraded_reason='policy_signature_invalid'`
    (Section 8.2).
  - Missing input samples map to `verdict='warn'` with
    `degraded_reason='samples_pending'` (operator pin
    `gate-degraded-policy=warn`); the gate never blocks on
    missing inputs.

### 6.3 `mgmt.*` (Management surface)

Read verbs:

- `mgmt.read.repo(repo_id) -> Repo`
- `mgmt.read.metric_sample(sample_id) -> MetricSample`
- `mgmt.read.metric_samples(repo_id, sha, metric_kind?, scope?)
  -> SampleRef[]`
- `mgmt.read.findings(repo_id, sha?, severity?) -> Finding[]`
- `mgmt.read.regressions(repo_id, sha, prev_sha?) -> Finding[]`
  -- returns the **`Finding` rows with `delta='newly_failing'`**
  (per Section 5.4.1 normative definition: rule is now
  `severity='block'` at this scope at this SHA and was not at the
  previous SHA).
- `mgmt.read.cross_repo(repo_id, metric_kind, scope_kind, sha?)
  -> CrossRepoPercentileWithHistogram`
- `mgmt.read.portfolio(metric_kind, scope_kind) ->
  PortfolioSnapshot`
- `mgmt.read.refactor_plan(repo_id, sha) -> RefactorPlan`

Write verbs (Management is the writer of `Repo`, `Commit` columns
other than `scan_status`, and `RepoEvent`; per Section 3.13 and
G1):

- `mgmt.register_repo(display_name, default_branch, mode?) -> Repo`
- `mgmt.set_mode(repo_id, mode) -> Repo` -- writes a
  `RepoEvent(kind='mode_changed')` row.
- `mgmt.retract_sample(sample_id, reason) -> RetractionId` -- per
  Section 3.13, this writes a `RepoEvent(kind='retract_intent')`
  row in Catalog and delegates to the Metric Ingestor, which
  appends the actual `MetricRetraction` row in Measurement (G1).
- `mgmt.rescan(repo_id, sha?) -> ScanRunRef` -- enqueues a
  Metric Ingestor scan (the Ingestor writes the `ScanRun` row).
- `mgmt.override(rule_id, mute, reason, scope?) -> OverrideId`
  -- delegates to the Policy Steward, which appends the
  `Override` row (Section 3.11, Section 4.6).

### 6.4 `ingest.*` (External Metric Ingest Webhook)

Per Section 1.5.1 row 3, four shapes mirror the four `ScanRun` modes:

- `ingest.coverage(repo_id, sha, payload_cobertura_xml) ->
  ScanRunRef` -- `sha_binding='single'` (operator pin
  `external-metric-coverage-format`, Section 1.6).
- `ingest.test_balance(repo_id, sha, payload) -> ScanRunRef` --
  `sha_binding='single'`. Writes only the foundation-tier
  `pass_first_try_ratio` row; the Aggregator promotes it to the
  system-tier `xservice_test_reliability` row (Section 1.5.1 row 2,
  Section 3.10 step 4).
- `ingest.churn(repo_id, payload) -> ScanRunRef` --
  `sha_binding='per_row'`, parent `ScanRun.to_sha=NULL`. Each
  payload row carries its own SHA.
- `ingest.defects(repo_id, payload) -> ScanRunRef` --
  `sha_binding='per_row'`, parent `ScanRun.to_sha=NULL`. Each
  payload row carries its own SHA.

### 6.5 `policy.*` (Policy Steward)

- `policy.publish(name, rule_refs, threshold_refs, refactor_weights)
  -> PolicyVersion` -- signs the row (operator pin
  `policy-signing-required`, Section 1.6).
- `policy.activate(policy_version_id, actor_id) ->
  PolicyActivation`
- `policy.publish_rulepack(pack_id, display_name, ...) ->
  RulePack`

> **Note (no `policy.override` verb).** Operator mute / unmute
> goes through `mgmt.override` (Section 6.3), which delegates to
> the Policy Steward for the actual `Override` row append
> (Section 3.11, Section 4.6). There is intentionally **no**
> public `policy.override` verb on this surface -- all override
> traffic is routed via the Management surface so audit logs
> are uniform across all operator mutations (Section 1.5.1 row
> 5). Any earlier draft that listed `policy.override` as a
> trigger is a bug; the Section 9 Policy Steward trigger column lists
> `mgmt.override`.

---

## 7. Cross-cutting components

### 7.1 Audit WAL

Every **Audit-sub-store** write (`EvaluationRun`,
`EvaluationVerdict`, `Finding`) goes through a local
write-ahead log first; the row is committed to PostgreSQL only
after the WAL fsync succeeds. The Audit WAL Reconciler
(Section 7.10) replays WAL-buffered rows after a crash,
preserving the original `EvaluationRun.caller` recorded in the
WAL frame. **The WAL contract is scoped to the Audit sub-store
only**: Measurement writes do NOT use this WAL, because
Measurement rows are re-derivable from a re-scan (G6) -- on
PostgreSQL unreachability the Metric Ingestor fails the
in-flight `ScanRun` and a subsequent scan re-attempts to land
a fresh row. Catalog / Lifecycle writes (`Repo`, `Commit`,
`RepoEvent`, `ScanRun`) are single-row commits and rely on
PostgreSQL synchronous replication for durability rather than
on this local WAL. Policy / rules and Refactor sub-store writes
are likewise not WAL-buffered (they are infrequent
operator-triggered commits).

### 7.10 Audit WAL Reconciler

Sweeps the per-process WAL on service start (and periodically)
and replays any rows that were not committed before the crash.
Replays only Audit-sub-store rows (`EvaluationRun`,
`EvaluationVerdict`, `Finding`) -- the rest of the Metrics Store
is re-derivable from `MetricSample` (G6) or comes from the
Catalog sub-store, which is single-row commits.

---

## 8. Operational invariants

### 8.1 Telemetry

Every component emits OTel spans. Span attributes pin
`repo_id`, `sha`, `scope_id`, `metric_kind`, `policy_version_id`
where applicable. The Insights Surface returns the most recent
`evaluation_run_id` so an operator can trace a verdict back to
its rule pass.

### 8.2 Degraded-reason closed list (normative)

The `MetricSample.degraded_reason` and
`EvaluationVerdict.degraded_reason` columns draw from this
**closed** set in v1:

- `xrepo_edges_unavailable` -- cross-repo edges required by a
  system-tier metric are not present (`embedded` mode or
  agent-memory unavailable).
- `samples_pending` -- a required input row has not yet arrived
  (e.g. `pass_first_try_ratio` for `xservice_test_reliability`).
- `policy_signature_invalid` -- the resolved `PolicyVersion`
  failed signature verification (operator pin
  `policy-signing-required`).
- `percentile_stale` -- `CrossRepoPercentile.built_at` is older
  than `PolicyVersion.refactor_weights.freshness_window_seconds`
  (Section 8.4).

New reasons require a `PolicyVersion` publish + a code change;
they are NOT free-form.

### 8.3 Reliability invariants

- The evaluator agent MUST treat a `degraded=true,
  degraded_reason='policy_signature_invalid'` reply as a hard
  fail (escalate to operator). It does NOT block the underlying
  change (Wen Zhong: page the human only when the runbook
  can't); the policy is unsigned, not the change.
- The evaluator agent MUST treat any `verdict='block'` reply as a
  hard block on the change.
- The Refactor Planner MUST NOT mutate source code.

### 8.4 Cross-repo aggregation cadence and freshness

`CrossRepoPercentile.built_at` is the freshness clock. The
Insights Surface returns `degraded=true` with
`degraded_reason='percentile_stale'` if `now - built_at >
PolicyVersion.refactor_weights.freshness_window_seconds`
(the normative field path defined in Section 5.3.3; default
`3600` seconds = 1 hour, pinned in tech-spec). The Cross-Repo
Aggregator runs on a separate cadence (default `15m`) so the
freshness window has 3-4x headroom under nominal load.

### 8.5 Policy rollout and kill switches (Wen Zhong principle)

Every alert has an owner, a runbook, and a kill switch. This
manifests in the data model as:

- Owner -- `PolicyVersion.activated_by` (audit row in
  `PolicyActivation`).
- Runbook -- `Rule.description_md` plus the operator-managed
  runbook URL in `RulePack.description_md`.
- Kill switch -- append an `Override(mute=true, reason)` row;
  takes effect on the next `eval.gate` call.

### 8.6 Capacity assumptions

The Compute Engine is the hot path. Per-PR scans operate over the
**delta** subset (Section 3.1 mode 2) and finish in seconds; full
scans run on a separate cadence (operator-pinned in tech-spec)
and are budgeted in tens of seconds per 100 kLOC.

### 8.7 Optional agent-memory composition (`linked` mode)

When `Repo.mode='linked'` (operator opt-in), the AST Adapter
joins to `agent-memory.GraphReader` for cross-repo call edges.
`ScopeBinding.agent_memory_node_id` is populated so the two
surfaces can be joined for downstream operator dashboards. In
`embedded` mode (default) this column is NULL and the system-tier
`xrepo_dep_depth` / `blast_radius` rows are written with
`degraded=true`, `degraded_reason='xrepo_edges_unavailable'`
(Section 8.2). No other component takes a hard dependency on
agent-memory.

---

## 9. Public-contract summary

This table is the **one-stop** list of public writers per
sub-store, the verbs they expose, what they read, and what they
write. It is the authoritative cross-check against G1 and Section
1.5.1; if a row here disagrees with a body section, this table
wins.

| Writer | Trigger | Reads | Writes |
| --- | --- | --- | --- |
| Management | `mgmt.register_repo`, `mgmt.set_mode` | -- | Repo, RepoEvent (Catalog / Lifecycle). **Management is the formal G1 writer of `Commit`** (Section 1.5 G1 Catalog / Lifecycle row) but **delegates** actual `Commit` row creation to the Repo Indexer (next row, Section 3.3); Management itself never writes any `Commit` column directly, and never writes `Commit.scan_status` (Metric Ingestor row below, per Section 1.5.1 row 1). |
| Management | `mgmt.retract_sample` | MetricSample (Measurement; validates sample exists) | RepoEvent (Catalog / Lifecycle; records intent + delegates to the Metric Ingestor, which appends `MetricRetraction` in Measurement -- Management is NOT a Measurement writer per G1) |
| Worker -- Repo Indexer (Section 3.3) [Management's formal G1 delegate for `Commit` creation] | git remote watch / git host webhook (new SHA appears on a tracked branch) | -- | `Commit` rows in Catalog / Lifecycle -- writes **only** the columns `(repo_id, sha, parent_sha, committed_at)` on INSERT. The Indexer **omits `scan_status` from the INSERT column list**; the schema declares `scan_status enum NOT NULL DEFAULT 'pending'` (Section 5.1.2) so the database engine supplies the initial `'pending'` value with no application writer. The Indexer never names `scan_status` on INSERT, never updates the column afterward, never updates any other `Commit` column after row creation, and never writes any sub-store other than Catalog / Lifecycle. Per Section 1.5.1 row 1 the Metric Ingestor is the only application writer of `Commit.scan_status` (and the row below in this table records that writer). |
| Worker -- Metric Ingestor (Section 3.1) | full / delta / external scan | MetricKind (Catalog / Lifecycle; schema lookup) | MetricSample, ScopeBinding, MetricRetraction (Measurement); ScanRun (Catalog / Lifecycle); `Commit.scan_status` (Catalog / Lifecycle, **only this column** -- transitions `pending -> scanning -> scanned | failed` per Section 1.5.1 row 1; the Repo Indexer row above writes every other `Commit` column at row creation time, and no component updates `Commit` columns other than `scan_status` after that). |
| Worker -- SOLID Rule Engine batch (Section 3.6) | post-ingest refresh (Section 4.7) | MetricSample (Measurement); Rule, PolicyVersion, Override (Policy / rules) | **EvaluationRun, EvaluationVerdict, Finding** (Audit / verdict) -- per the expanded G1 ACL the batch worker writes all three tables; rows carry `EvaluationRun.caller='batch_refresh'`. |
| Worker -- Refactor Planner (Section 3.9) | post-ingest refresh | MetricSample, Finding | HotSpot, RefactorPlan, RefactorTask (Refactor) |
| Worker -- Cross-Repo Aggregator (Section 3.10) | cadence tick | MetricSample (Measurement; reads foundation-tier rows) | **MetricSample** (Measurement; **system-tier** rows only, stamped `pack='system'`, `source='derived'` per Section 3.10 step 4 -- the Aggregator is the **only** writer that produces `pack='system'` rows; foundation-tier writes remain with the Metric Ingestor per row above and per the Section 1.5.1 reconciliation table); RepoMetricSnapshot, CrossRepoPercentile, PortfolioSnapshot (Measurement; derived per G6) |
| Worker -- Audit WAL Reconciler (Section 7.10) | Metrics Store recovery | local audit WAL (process-local; not a Metrics Store sub-store) | EvaluationRun, EvaluationVerdict, Finding (Audit / verdict; replay-only of WAL-buffered rows) |
| Policy Steward (Section 3.11) | `policy.publish`, `policy.activate`, `policy.publish_rulepack`, and `mgmt.override` (delegated from the Management surface per Section 6.3 -- there is no `policy.override` verb, see Section 6.5 note and Section 1.5.1 row 5) | -- | Rule, RulePack, PolicyVersion, PolicyActivation, Threshold, Override (Policy / rules) |
| Evaluator Surface (Section 3.7) | `eval.gate` | MetricSample, PolicyVersion, Override | EvaluationRun, EvaluationVerdict, Finding (Audit / verdict; `EvaluationRun.caller='eval_gate'`) |
| External Metric Ingest Webhook (Section 3.12) | `ingest.coverage / churn / defects / test_balance` | -- (auth only) | enqueues `ScanRun` for the Metric Ingestor -- the Ingestor is the actual writer (per G1) |

---

## 10. Cross-references

- The SOLID rule pack DDL (per-rule predicate DSL grammar) and
  the Compute Engine's per-language recipes are defined in
  `tech-spec.md`.
- The phased build order (which component lands in which sprint,
  which `ScanRun` modes ship first) is defined in
  `implementation-plan.md`.
- The end-to-end gate / refactor / portfolio scenarios that this
  architecture must satisfy are defined in
  `e2e-scenarios.md`.
- The `agent-memory` service it composes with (optional, via
  `Repo.mode='linked'`) is specified in
  `docs/stories/code-intelligence-AGENT-MEMORY/architecture.md`
  (see Section 3.5 Hybrid Graph Store, Section 6.2.3 read verbs).
