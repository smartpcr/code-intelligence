# Tech Spec -- code-intelligence:CLEAN-CODE

> Story: `code-intelligence:CLEAN-CODE` (13 points)
> Title: clean code
> Branch: `story/code-intelligence-CLEAN-CODE/plan-tech-spec`
> Sibling plan docs (read in parallel):
>   - `architecture.md` -- components, data model, interfaces, invariants
>   - `implementation-plan.md` -- phased build order, milestones, owners
>   - `e2e-scenarios.md` -- end-to-end happy-path + degraded-path walkthroughs
>
> This document owns: **problem statement, in/out-of-scope, non-goals,
> hard constraints, parameter pins, and risks**. It does NOT redefine
> component boundaries, data model schemas, or wire-format payloads --
> those live in `architecture.md` and are referenced here by section
> number. When this doc and `architecture.md` disagree, the operator
> arbitrates and the resolution is recorded in Sec 11 below.

---

## 1. Document Scope

### 1.1 What this tech-spec owns

This tech-spec is the **decision ledger** for the CLEAN-CODE story. It
turns the operator's free-form story description into a list of
binding commitments the implementation must honour. Concretely, it
owns:

- **Problem statement** -- the user-facing pain the story addresses,
  restated in engineering terms (Sec 2).
- **Strategy survey** -- the approaches we evaluated, the one we
  chose, and the ones we rejected with reasons (Sec 3).
- **In-scope / out-of-scope split** -- what v1 ships, what it
  deliberately punts (Sec 4, Sec 5, Sec 6).
- **Hard constraints** -- the invariants the implementation MUST
  preserve; violating any of these is a release-blocker (Sec 7).
- **Parameter pins** -- the numeric defaults, algorithm choices, and
  schema-level DDL conventions that `architecture.md` explicitly
  defers to this doc (Sec 8).
- **Risks** -- failure modes we anticipate, plus their mitigations
  and residual exposure (Sec 9).
- **Locked-decisions roll-up** -- a one-screen summary table the
  reviewer can sign off on without paging through the rest (Sec 10).

### 1.2 What this tech-spec does NOT own

- **Component boundaries and responsibilities** -- owned by
  `architecture.md` Sec 3 (Metric Ingestor, AST Adapter, Repo Indexer,
  Compute Engine, SOLID Rule Engine, Evaluator Surface, Insights
  Surface, Refactor Planner, Cross-Repo Aggregator, Policy Steward,
  External Metric Ingest Webhook, Management).
- **Data model schemas** -- owned by `architecture.md` Sec 5
  (`Repo`, `Commit`, `RepoEvent`, `MetricKind`, `ScanRun`,
  `MetricSample`, `ScopeBinding`, `MetricRetraction`,
  `RepoMetricSnapshot`, `CrossRepoPercentile`,
  `PortfolioSnapshot`, `Rule`, `RulePack`, `PolicyVersion`,
  `PolicyActivation`, `Threshold`, `Override`,
  `EvaluationRun`, `EvaluationVerdict`, `Finding`, `HotSpot`,
  `RefactorPlan`, `RefactorTask`).
- **Wire-format payloads** -- owned by `architecture.md` Sec 6 (the
  four gRPC verb namespaces are `eval.*` for the Evaluator Surface,
  `mgmt.*` for the Management surface including all `mgmt.read.*`
  Insights-backing dashboard reads, `ingest.*` for the External
  Metric Ingest Webhook, and `policy.*` for the Policy Steward;
  there is no separate `Insights` surface verb namespace in
  architecture Sec 6 -- Insights dashboards consume `mgmt.read.*`).
- **Phased build order, ticket breakdown, owners, dates** -- owned
  by `implementation-plan.md`.
- **End-to-end walkthroughs** -- owned by `e2e-scenarios.md`.

### 1.3 Audience

The intended readers are:

- The **operator** signing off on locked decisions before
  implementation starts.
- The **engineers** implementing the Metric Ingestor / AST Adapter /
  Compute Engine / Policy Steward services -- they will use the
  hard-constraint list (Sec 7) as their acceptance criteria.
- The **evaluator agent** consuming the metric store via `eval.gate`
  -- it relies on the parameter pins (Sec 8) and the closed
  `degraded_reason` list (Sec 7.7) being stable.
- Future **refactor architects** scoping big-repo cleanup efforts --
  they will use the risk register (Sec 9) to scope their guardrails.

---

## 2. Problem Statement

### 2.1 Operator-authored ask (verbatim)

> we need to establish clean code standards, which will be used by
> evaluator prompt to drive the following standards:
> 1) decoupled functional areas
> 2) SOLID coding principal
>
> goal of this story:
> - implement system level metrics where we can show measurable code
>   qualities for big repo, and cross-repo measurements
> - the measurements will be used by evaluator agent to guard bad
>   practices
> - the metrics will be used for refactoring effort of big repo

### 2.2 Restated in engineering terms

The operator is asking for **three coupled deliverables** that
together raise the floor on code quality across the org's repo
portfolio:

1. **A standards corpus that is machine-checkable.** "Clean code"
   today is asserted in PR reviews via human judgment. The operator
   wants the same standards encoded as numeric thresholds and rule
   packs so they can be applied uniformly. The two named pillars
   decompose into the architecture's normative `metric_kind`
   enum (Sec 1.4): (a) **decoupled functional areas** -- driven
   by `cycle_member` + `coupling_between_objects` foundation
   metrics and the `arch_debt_ratio` + `arch_fitness` system-
   tier composites (Sec 4.4) -- and (b) **SOLID** -- decomposes
   into per-principle rule packs joining on the `pack='solid'`
   foundation metric_kinds (Sec 4.3): SRP via `lcom4`, OCP via
   `fan_in`, LSP via `depth_of_inheritance`, ISP via
   `interface_width`, DIP via `fan_in` + `fan_out` +
   `coupling_between_objects`.
2. **A measurement pipeline that scales to big repos AND across
   the portfolio.** Single-file linters do not solve this -- the
   operator names "big repo" and "cross-repo" explicitly. That
   forces an AST-front-end with incremental scan support, a
   metric store that retains history per `(repo, sha, scope)`,
   and a percentile aggregator that can rank a repo against its
   peers.
3. **Two downstream consumers that close the loop.** The metrics
   are not an end in themselves. They must be (a) consumed by
   the evaluator agent as a gate that fails bad practices before
   they merge, and (b) consumed by a refactor planner that
   converts the largest measurable deltas into prioritised work
   items for big-repo cleanup.

### 2.3 Why the references shape the design

- **augmentcode 12-metrics guide** -- gives us the candidate
  foundation-tier metric list (cyclomatic, cognitive, change
  failure rate, MTTR, lead time, defect density, code coverage,
  duplication, technical debt ratio, maintainability index, churn,
  ownership). We adopt the subset that is measurable from source
  + VCS without requiring incident-management integration in v1.
  See `architecture.md` Sec 1.4 for the adopted catalogue (12
  foundation + 7 system tier).
- **Halleck45/ast-metrics** -- validates the **tree-sitter +
  per-language recipe** front-end approach. We are not depending
  on the project as a library, but we adopt its architectural
  shape: one parser front end, language-specific compute recipes
  that read the parsed AST and emit metric samples.

### 2.4 Why a tech-spec exists (and not just an architecture doc)

The architecture doc names the components and their data flow but
**deliberately punts** numeric defaults and certain DDL choices to
this doc. Specifically `architecture.md` explicitly tags the
following decisions as "pinned in tech-spec":

- `scan_timeout` and periodic-sweep cadence for orphan `ScanRun`
  cleanup (architecture Sec 3.1).
- The exact active-row uniqueness DDL for `MetricSample` --
  pinned in Sec 8.7 as a separate `metric_sample_active`
  pointer table (architecture G2/G3, Sec 5.2.1, Sec 5.2.2).
- `window_days` default for the `modification_count_in_window`
  SOLID input materialisation (NOT for ML effort-model training
  -- architecture Sec 1.4 row 12 and Sec 5.3.3 are explicit
  that `window_days` lives on `PolicyVersion.refactor_weights`
  and feeds the Metric Ingestor's churn materialisation on
  `ingest.churn` arrival) and the `freshness_window_seconds`
  default for percentile freshness (architecture Sec 5.3.3).
- The signing algorithm and key-rotation cadence for
  `PolicyVersion.signature` (architecture Sec 5.3.3).
- The transport pin for the management surface (architecture Sec 6).
- The full-scan cadence (architecture Sec 8.6).
- The SOLID rule pack DDL and the Compute Engine per-language
  recipes (architecture Sec 10).

Sec 8 below pins each of these.

---

## 3. Strategy Survey

### 3.1 Approaches evaluated

| Approach | Front-end | Persistence | Evaluator integration | Verdict |
|----------|-----------|-------------|------------------------|---------|
| A: Wrap existing linters (golangci-lint, eslint, pylint, etc.) and aggregate their JSON | Per-language linter binary | None (re-run per request) | Parse linter exit codes | REJECTED -- linters disagree on metric semantics; cross-repo ranking impossible; no historical trend; SOLID rules absent |
| B: Adopt SonarQube CE as the metric back end and read its API | Sonar scanners | SonarQube DB | REST gate | REJECTED -- operationally heavy; closed metric catalogue; licence friction for some orgs; cross-repo percentile relies on Sonar org licence |
| C: Tree-sitter front end + bespoke compute engine + own metric store (THIS DOC) | tree-sitter grammars | PostgreSQL append-only `MetricSample` table | gRPC `eval.gate` | SELECTED -- matches augmentcode + ast-metrics references; gives us full control over the metric catalogue, retention, percentile aggregation, and refactor scoring |
| D: LLM-only "review" (no metrics, no store) | None | None | Prompt-only | REJECTED -- no reproducibility; no gate that can fail deterministically; no refactor ranking |

### 3.2 Selected approach (summary)

We build a **tree-sitter-backed AST adapter** that emits a
canonical AST representation per file, a **Compute Engine** that
applies per-language recipes to produce metric samples, a
**PostgreSQL-backed append-only `MetricSample` store** with active-
row uniqueness enforced by a separate `metric_sample_active`
pointer table (Sec 8.7), a **signed
`PolicyVersion`** that maps metric thresholds + SOLID rule packs
to verdicts, an **`eval.gate` gRPC surface** the evaluator agent
calls per PR, a **Cross-Repo Aggregator** that produces derived
percentile snapshots, and a **Refactor Planner** that ranks
findings by effort-weighted impact.

Component boundaries, sub-store ACLs, and the canonical metric
catalogue are owned by `architecture.md` Sec 1.4, Sec 1.5, Sec 1.5.1, Sec 3.

### 3.3 Rejected alternatives -- expanded reasons

- **A (wrap linters).** Every linter encodes its own definition
  of "cyclomatic complexity" or "cohesion". Aggregating their
  outputs would force us to either pick one definition (and
  re-explain to users why our number differs from their CLI) or
  publish multiple numbers per concept (and watch teams cherry-
  pick the favourable one). The operator's "cross-repo
  measurement" goal is unsatisfiable when each repo's number is
  defined by whichever linter that team installed.
- **B (SonarQube).** SonarQube's metric catalogue is closed --
  adding a new metric requires forking the back end. The
  operator-pinned `refactor-effort-source=ML model from
  historical commits` (architecture Sec 1.6 row 5) is impossible to
  plug into SonarQube without bypassing its scoring. Also fails
  the operator-pinned `policy-signing-required=v1 required`
  (architecture Sec 1.6 row 4) -- Sonar quality profiles are not
  cryptographically signed.
- **D (LLM-only).** Cannot satisfy "guard bad practices" because
  there is no deterministic input the evaluator can fail on. A
  PR that scored "fine" yesterday could score "bad" today purely
  from prompt drift. Also no refactor ranking output.

### 3.4 Why we explicitly endorse the operator pins

The five operator pins recorded in `architecture.md` Sec 1.6 (ast
mode default = embedded, external metric coverage format =
Cobertura XML, gate degraded policy = warn, policy signing
required = v1 required, refactor effort source = ML model from
historical commits) are **inputs**, not findings. This tech-spec
treats them as immutable and orients the constraints (Sec 7) and
pins (Sec 8) to honour them.

---

## 4. In Scope (v1)

The following capabilities are committed deliverables of the
CLEAN-CODE story. Each item is anchored to the architecture doc
section that defines its component/data shape so the implementor
has a single forward reference.

### 4.1 Foundation-tier metric catalogue (12 metrics)

The 12 foundation-tier metric_kinds from architecture Sec
1.4.1 are implemented for v1. They are listed there by name;
the canonical set is `cyclo`, `cognitive_complexity`, `loc`,
`lcom4`, `fan_in`, `fan_out`, `depth_of_inheritance`,
`interface_width`, `coupling_between_objects`, `cycle_member`,
`duplication_ratio`, and `modification_count_in_window`.
Each metric is emitted per `Scope` (method / file / package /
repo as the architecture table specifies) and lands in
`MetricSample` with `pack IN ('base', 'solid')` (architecture
Sec 5.2). `solid`-pack rows (`lcom4`, `fan_in`, `fan_out`,
`depth_of_inheritance`, `interface_width`,
`coupling_between_objects`) are the direct inputs to the
SRP / OCP / DIP / LSP / ISP rule packs (Sec 4.3).

### 4.1.1 Ingested-pack foundation metric_kinds (pinned in tech-spec)

The four `ingest.*` verbs (architecture Sec 6.4) write foundation-
tier rows with `pack='ingested'` and `source='ingested'` per the
architecture's enum at Sec 5.2.1 (the `pack` enum is `base | solid
| ingested | system`; the `source` enum is `computed | ingested |
derived`). Architecture Sec 1.4.1 lists the 12 `computed` /
foundation kinds but **delegates the ingested-pack `metric_kind`
names to this tech-spec** (architecture Sec 1.4.2 row 6 names
`pass_first_try_ratio` in prose for `xservice_test_reliability`
input; the rest are pinned here). The v1 ingested-pack catalogue
is:

| `metric_kind` | Scope | Written by | Consumed by | Source verb |
| --- | --- | --- | --- | --- |
| `coverage_line_ratio` | file, package, repo | Metric Ingestor (writes the `MetricSample` row; the External Metric Ingest Webhook enqueues the `ScanRun` per arch Sec 3.12) | Insights; threshold rules on `PolicyVersion.threshold_refs` | `ingest.coverage` |
| `coverage_branch_ratio` | file, package, repo | Metric Ingestor | Insights; threshold rules | `ingest.coverage` |
| `pass_first_try_ratio` | repo | Metric Ingestor | Cross-Repo Aggregator (composes into system-tier `xservice_test_reliability` per arch Sec 1.4.2 row 6) | `ingest.test_balance` |

Notes:

- All three rows are foundation-tier (`pack='ingested'`) and
  carry `source='ingested'`; they participate in the active-row
  uniqueness check (C1) over the same quintuple as the
  `computed` rows.
- The catalogue intentionally omits any `metric_kind` for
  `ingest.churn` and `ingest.defects` payloads: `ingest.churn`
  payloads feed the `modification_count_in_window` materialiser
  (Sec 8.2 `window_days`, Sec 4.1 catalogue row 12) so the
  resulting `MetricSample` rows are written with
  `metric_kind='modification_count_in_window'` and
  `pack='base'` (the materialiser is the writer, not the
  webhook), and `ingest.defects` is v1-deferred per Sec 4.11.
- Adding an ingested-pack `metric_kind` requires (a) a new row
  in this catalogue table, (b) a `MetricKind` Catalog/Lifecycle
  row (architecture Sec 5.1), and (c) a `MetricVersion`
  assignment per C4.

### 4.2 System-tier metric catalogue (7 metrics)

The 7 system-tier metric_kinds from architecture Sec 1.4.2
are implemented for v1. The canonical set is
`xrepo_dep_depth`, `arch_debt_ratio`, `velocity_trend`,
`arch_fitness`, `blast_radius`, `xservice_test_reliability`,
and `knowledge_index`. These are composed by the Cross-Repo
Aggregator (architecture Sec 3.10 step 4) from foundation-tier
rows and land in `MetricSample` with `pack='system'` AND
`source='derived'` -- the carve-out row in the C5 ACL table.
When a required input is missing the row is still written but
stamped `degraded=true` with a `degraded_reason` drawn from
the C21 closed list (this is the canonical mechanism for the
`xrepo_edges_unavailable` and `samples_pending` reasons in
embedded mode).

### 4.3 SOLID rule packs (one rule pack per principle)

Five rule packs (SRP, OCP, LSP, ISP, DIP) shipped as
`RulePack` rows in the Policy / rules sub-store and
referenced from `PolicyVersion.rule_refs`. Each rule pack
composes thresholds against the foundation-tier `pack='solid'`
metric_kinds whose architecture briefs name them as the
driving input: SRP joins on `lcom4`, OCP joins on `fan_in`
(architecture Sec 1.4.1 row 5 brief), LSP joins on
`depth_of_inheritance`, ISP joins on `interface_width`, DIP
joins on `fan_in` + `fan_out` + `coupling_between_objects`.
Rule packs emit `Finding` rows that cite their input
`MetricSample` ids per C14. Architecture Sec 3 (SOLID Rule
Engine) owns the rule schema; Sec 7 owns the active-row
identity invariants the rules join on.

### 4.4 Decoupled-functional-areas rule pack

A sixth rule pack that gates on the coupling cluster:
foundation-tier `cycle_member` and `coupling_between_objects`
(architecture Sec 1.4.1 rows 9-10, both tagged "Drives
decoupling rule" in their briefs) plus system-tier
`arch_debt_ratio` and `arch_fitness` (architecture Sec 1.4.2
rows 2 and 4) for the cross-package composite signal. This
is the operator's first named pillar; it is treated as a
peer of the SOLID packs, not a sub-rule of DIP.

### 4.5 AST adapter front end (tree-sitter, embedded mode default)

A tree-sitter-backed parser fleet that produces canonical AST
proto. Operator pin: `ast-mode-default=embedded` (architecture
Sec 1.6 row 1) -- the adapter runs in-process inside the Metric
Ingestor by default; out-of-process "linked" mode is supported
but opt-in. v1 language coverage is pinned in Sec 8.6.

### 4.6 Append-only `MetricSample` store with retraction

PostgreSQL `MetricSample` table with `MetricRetraction`
satellite table for soft-deletes. Active-row uniqueness on
`(repo_id, sha, scope_id, metric_kind, metric_version)` is
enforced by a separate `metric_sample_active` pointer table
whose DDL is pinned in Sec 8.7 (preserving G3 immutability of
`MetricSample` rows). G2 + G3 invariants from architecture Sec
1.5 are the acceptance criteria.

### 4.7 Signed `PolicyVersion` lifecycle

`PolicyVersion` rows are versioned, Ed25519-signed (Sec 8.4),
kill-switchable, and immutable once published. Operator pin:
`policy-signing-required=v1 required` (architecture Sec 1.6 row
4). G5 invariant from architecture Sec 1.5 is the acceptance
criterion.

### 4.8 Evaluator gate (`eval.gate` gRPC surface)

The gate accepts `(repo_id, sha, policy_version)` and returns
an `EvaluationVerdict` row whose `verdict` field is one of
`pass | warn | block` (architecture Sec 5.4.3 field row) plus
a separate `degraded` boolean + nullable `degraded_reason`
string and a list of cited `Finding` rows. Operator pin:
`gate-degraded-policy=warn` (architecture Sec 1.6 row 3) --
when a degraded condition is detected the verdict is mapped
to `warn` rather than `block`, so missing samples / stale
percentiles / invalid signatures never hard-block a PR. The
closed list of `degraded_reason` values is pinned in Sec 7.7.

### 4.9 Refactor planner

An ML-model-backed ranking surface that converts open findings
into a prioritised refactor backlog. Operator pin:
`refactor-effort-source=ML model from historical commits`
(architecture Sec 1.6 row 5). Read-only against the metric store
(it does not own its own sub-store -- see Sec 7.2).

### 4.10 Cross-Repo Aggregator with derived percentile snapshots

A scheduled job that snapshots cross-repo percentiles per
metric_kind into `CrossRepoPercentile`. G6 invariant
(percentiles are derived, never authoritative) is the
acceptance criterion.

### 4.11 External metric ingest webhook (four verbs)

A webhook endpoint surfaced by the Metric Ingestor that accepts
external-mode payloads in **four ingest shapes**, mirroring the
four `ScanRun` modes (architecture Sec 1.5.1 row 3, Sec 3.12,
Sec 6.4):

| Verb | SHA binding | Payload | Notes |
|------|-------------|---------|-------|
| `ingest.coverage` | `single` (one `sha` per call) | Cobertura XML (operator pin `external-metric-coverage-format=Cobertura XML`, arch Sec 1.6 row 2) | Metric Ingestor parses the XML and writes `coverage_line_ratio` and `coverage_branch_ratio` `MetricSample` rows (Sec 4.1.1 ingested-pack catalogue; the `coverage_line_ratio` name is anchored at arch Sec 4.3 lines 772-774, `coverage_branch_ratio` follows the same convention under arch's "(and similar)" clause), `pack='ingested'`, `source='ingested'`. Other coverage formats (JaCoCo, lcov, Clover, Cobertura JSON) are out of scope **for this verb's payload** in v1 (Sec 5.6) |
| `ingest.test_balance` | `single` | JSON `{scope_id, attempt_count, pass_count}` rows | Writes ingested-pack foundation row `pass_first_try_ratio` (Sec 4.1.1); the Cross-Repo Aggregator promotes it to system-tier `xservice_test_reliability` (architecture Sec 3.10 step 4, Sec 1.4.2 row 6) |
| `ingest.churn` | `per_row` (each payload row carries its own `sha`) | JSON `{repo_id, file_path, sha, modified_at}` rows | Drives `modification_count_in_window` materialisation (catalogue row 12; the materialiser writes `pack='base'` rows -- the webhook itself does not write a per-row `MetricSample`). Parent `ScanRun.to_sha=NULL`; `window_days` (Sec 8.2) is the commit-window applied at materialisation |
| `ingest.defects` | `per_row` | JSON `{repo_id, file_path, sha, defect_id, severity}` rows | **v1 pin: store-only at the `ScanRun` boundary.** The Metric Ingestor accepts the payload, persists a `ScanRun` row with `kind='external_per_row'`, `sha_binding='per_row'`, `to_sha=NULL`, and records `payload_hash` (architecture Sec 5.7 `ScanRun.payload_hash`) for idempotency. **The defect payload body itself is NOT persisted** -- upstream `ScanRun` (architecture Sec 5.7 lines 1269-1280) carries `payload_hash` only and no payload-body/backlog field, so the Ingestor acks the webhook, records the hash, and discards the body. **No `MetricSample` row is written by this verb in v1** (the architecture metric catalogue at Sec 1.4.1 + Sec 1.4.2 names no defect-derived foundation `metric_kind`, the augmentcode-referenced incident-derived metrics are reserved per Sec 5.8, and `MetricSample.metric_kind` is `NOT NULL` per arch Sec 5.2.1 + Sec 8.7 DDL -- so writing a row would require either an invented metric_kind or a NULL violation, both forbidden). A v2 follow-on (a) extends the `ScanRun`-or-sibling schema upstream to hold the payload body, (b) adds a defect-driven foundation `metric_kind` (likely candidates: per-file `defect_count`, per-scope `severity_weighted_defect_density`), and (c) materialises rows from re-ingested payloads at that point; the v1 pin owner is tech-spec Sec 4.11. |

All four verbs route through the Metric Ingestor's single-writer
role grant (C8). The `source` field on resulting `MetricSample`
rows (architecture Sec 5.2.1) is set per-row as follows:

- `ingest.coverage` and `ingest.test_balance` -- the Metric
  Ingestor writes the ingested-pack rows in Sec 4.1.1 directly
  from the payload, so those rows carry `pack='ingested'` and
  `source='ingested'`.
- `ingest.churn` -- the webhook itself does **not** write any
  `MetricSample` row. The payload feeds the
  `modification_count_in_window` materialiser (Sec 8.2,
  catalogue row 12); the materialiser computes the count over
  the configured window from the ingested commit rows and
  writes `pack='base'`, `source='computed'`,
  `metric_kind='modification_count_in_window'`. `source` here
  is `computed` (not `ingested`) because the materialiser is
  the computing writer; the `ingested` provenance is recorded
  on `MetricSample.attrs_json` as a separate annotation (C19)
  rather than on the `source` enum.
- `ingest.defects` -- v1 writes only a `ScanRun` row per the
  pin row above; no `MetricSample` row.

Webhook transport is REST + signed-HMAC body per Sec 8.5.

### 4.12 Management surface

A REST/JSON management surface for repo onboarding, policy
publication, kill-switch toggling, and audit-log retrieval.
Transport pin in Sec 8.5.

### 4.13 Audit WAL + reconciler

A per-service local write-ahead log that buffers writes to
the Audit/verdict sub-store (`EvaluationRun`,
`EvaluationVerdict`, `Finding`) when PostgreSQL is
unreachable, plus a reconciler (architecture Sec 7.10) that
replays buffered WAL frames once the database returns. The
reconciler is **replay-only** -- it never originates a new
`EvaluationRun` and always preserves the original
`EvaluationRun.caller` value from the WAL frame. There is no
separate `AuditEntry` table; "audit" here means the
append-only `EvaluationRun` / `EvaluationVerdict` / `Finding`
rows themselves serve as the audit trail.

### 4.14 Single-tenant deployment (v1)

A single logical "org" per deployment. Multi-tenant
isolation (per-tenant schemas, per-tenant signing keys, per-
tenant kill switches) is out of scope in v1 (Sec 5.10). The
architecture data model (architecture Sec 5) intentionally
carries **no** `tenant_id` column on any table in v1; the
single-tenant assumption is preserved at the
schema-isolation boundary (one `clean_code` schema per
deployment, Sec 8.1.3) rather than at the row level. A v2
multi-tenant migration will adopt per-schema isolation
(one `clean_code_<tenant>` schema per tenant, extending the
Sec 8.1.3 single-tenant shape); the v2 shape is pinned at
Sec 10A ("multi-tenant v2 shape") and no `tenant_id`
column is pre-reserved here.

---

## 5. Out of Scope (v1)

The following are explicitly **not** part of v1. Each item names
the reason and (where applicable) the v2 placeholder so future
planners do not re-litigate.

- **5.1 Style / formatter rules.** Things like indentation,
  brace placement, import ordering, naming conventions. These
  are owned by existing per-language formatters (gofmt,
  prettier, black, ktlint) and re-implementing them adds noise
  without raising the quality floor. The CLEAN-CODE evaluator
  gate explicitly does NOT lint style.
- **5.2 Auto-fix / code generation.** The evaluator gate reports
  findings; it does not rewrite code. A future
  "refactor-as-a-service" story can build on the refactor
  planner's output but is not v1.
- **5.3 UI / dashboard build.** The Insights Surface (architecture
  Sec 3) exposes JSON; the actual UI consumes the JSON and is owned
  by a separate front-end story. v1 ships the gRPC + REST
  surfaces only.
- **5.4 Multi-tenant isolation primitives.** Per-tenant schemas,
  per-tenant signing keys, per-tenant kill switches. v1 is
  single-tenant. No `tenant_id` column is reserved in the
  v1 data model (architecture Sec 5 carries none); the
  isolation boundary in v1 is the schema (Sec 8.1.3). v2
  shape is pinned at Sec 10A ("multi-tenant v2 shape"):
  per-schema isolation extends the v1 shape.
- **5.5 Jira / Linear / GitHub Issues integration.** The refactor
  planner emits `HotSpot` / `RefactorPlan` / `RefactorTask` rows;
  pushing them into an external tracker is a follow-on
  integration story.
- **5.6 Non-Cobertura coverage payloads for `ingest.coverage`**
  (JaCoCo, lcov, Clover, Cobertura JSON). The `ingest.coverage`
  verb (Sec 4.11) accepts **only** Cobertura XML in v1 per
  operator pin (architecture Sec 1.6 row 2). v2 can add adapters
  behind the same `External Metric Ingest Webhook` (architecture
  Sec 3).
- **5.7 Runtime / dynamic metrics.** APM data (latency, error
  rate, throughput), trace-derived call graphs, runtime
  coverage. v1 is static-analysis only. Risk 9.1 acknowledges
  the AST-vs-runtime drift this creates.
- **5.8 Incident-derived metrics.** Change failure rate, MTTR,
  lead time for changes. These were named in the augmentcode
  reference but require incident-management integration (which
  the org lacks today). They are reserved metric_kind slots in
  `architecture.md` Sec 1.4 but no compute recipe is shipped.
- **5.9 Cross-language deduplication.** A v1 "duplication" signal
  is per-language only. Detecting the same algorithm copy-pasted
  between Go and TypeScript is out of scope.
- **5.10 LLM-graded findings.** Findings are deterministic
  threshold-vs-metric joins. An LLM "explain this finding"
  feature is out of scope for v1 to keep `eval.gate`
  reproducible.
- **5.11 Online learning of the effort model.** The refactor
  planner's ML effort model is trained offline on historical
  commits and shipped as a versioned artefact. Continuous online
  updates are out of scope; see Risk 9.5.
- **5.12 Migrating existing repos to pass v1 policy.** The story
  builds the measurement and gating infrastructure. Actually
  fixing existing repo violations is a downstream refactor
  effort that the planner enables -- but is not part of this
  story.
- **5.13 Custom user-authored rule packs.** v1 ships the SOLID
  packs + the decoupled-areas pack. User-extensible rule DSL
  is v2.
- **5.14 Multiple AST front ends.** v1 standardises on
  tree-sitter (operator pin via reference to ast-metrics).
  Adding Roslyn / Eclipse JDT / TypeScript Compiler API as
  alternative front ends is v2.

---

## 6. Non-goals

Non-goals differ from "out of scope" -- these are things the
project will be **mistaken for** and explicitly is not. They are
recorded here so the operator can wave the doc at scope-creep
attempts.

- **6.1 Not a replacement for linters.** Existing linters
  (golangci-lint, eslint, pylint, ktlint, etc.) continue to run
  in CI. CLEAN-CODE measures the structural quality those
  linters cannot.
- **6.2 Not a replacement for CI.** The `eval.gate` is a gate
  that CI calls; it does not run tests, builds, or deploys.
- **6.3 Not a code-generation pipeline.** The refactor planner
  outputs prioritised tickets, not patches.
- **6.4 Not a semantic-search / code-intelligence catalogue.**
  The sibling `code-intelligence:AGENT-MEMORY` story owns
  semantic search and retrieval. CLEAN-CODE consumes the same
  AST adapter back end but writes to its own sub-stores.
- **6.5 Not a security scanner.** SAST findings (CWE / OWASP)
  are owned by a separate scanner stack. CLEAN-CODE is purely
  structural / maintainability-oriented.
- **6.6 Not a dependency / supply-chain auditor.** SCA findings
  (CVE, licence) are owned by a separate scanner.
- **6.7 Not a performance profiler.** Big-O / hot-path analysis
  is owned by APM. CLEAN-CODE's coupling metrics correlate with
  performance but are not a performance signal.
- **6.8 Not an automatic style enforcer.** See Sec 5.1.
- **6.9 Not a continuous online-learning system.** See Sec 5.11.

---

## 7. Hard Constraints

Hard constraints are the invariants the implementation MUST
preserve. Each constraint is numbered `C<n>` and cross-references
the architecture invariant it derives from. Violating any of
these is a release-blocker.

### 7.1 Identity and integrity (architecture G2, G3)

- **C1.** The active-row identity contract on the tuple
  `(repo_id, sha, scope_id, metric_kind, metric_version)` --
  "at most one active row per quintuple ... enforced by a
  partial unique index on the quintuple" -- is owned by
  architecture G2 + Sec 5.2.1 lines 991-1003. "Active" means
  not present in the `MetricRetraction` satellite table
  (architecture G2 lines 148-157). Architecture lines 1001-1003
  delegate the **exact DDL** to this doc ("the exact DDL is in
  `tech-spec.md`"); Sec 8.7 below implements that DDL. The
  v1 implementation enforces the architecture-mandated
  uniqueness via a `PRIMARY KEY (repo_id, sha, scope_id,
  metric_kind, metric_version)` constraint on a side relation
  `metric_sample_active` that by construction contains
  exactly the active row set (Sec 8.7). A PRIMARY KEY in
  PostgreSQL is realised as a unique B-tree index, so the
  active-row quintuple uniqueness architecture mandates is
  enforced by a unique B-tree index in this implementation;
  the index is **physically** on the side relation rather
  than on `metric_sample` because (a) PostgreSQL rejects
  subqueries in index predicates (so a literal `WHERE
  sample_id NOT IN (SELECT ... FROM MetricRetraction)`
  predicate is not expressible in PostgreSQL DDL) and (b)
  `metric_sample` rows are immutable per C2 / G3 (so no
  mutable flag column on `metric_sample` may be used as the
  index predicate). The side-relation implementation is one
  PostgreSQL-valid shape that honours both G2 (active-row
  uniqueness over the quintuple) and G3 (`MetricSample`
  immutability); it does not change the architecture
  contract, only realises it.
- **C2.** `MetricSample` rows are **immutable** per
  architecture G3 (architecture Sec 5.2.1 lines 908-920). No
  column on `MetricSample` is ever updated after insert,
  including `degraded` / `degraded_reason`. Corrections are
  issued by appending an `MetricRetraction` row that
  tombstones the prior `sample_id` plus appending a new
  `MetricSample` row with a fresh `sample_id`; the
  `metric_sample_active` pointer row is then atomically
  swapped to reference the new sample (Sec 8.7
  transactional pattern). Per architecture Sec 5.2.1 lines
  981-989 (`mgmt.retract_sample` followed by `mgmt.rescan(repo_id,
  sha)`), the correction MAY be issued **at the same SHA** --
  the retracted row remains in `metric_sample` as the audit
  trail (G3) and does not block a fresh active row at the
  same quintuple because the uniqueness key in C1 is
  enforced over the **active** row set only (a retracted
  row is a tombstone, not an active row). The corrected row
  MAY also land at a later HEAD SHA; both flows are
  architecture-supported. Direct `UPDATE` or `DELETE` on
  `MetricSample` is forbidden at the role-grant level (Sec 8.7).
- **C3.** Identity invariants persist across the
  embedded<->linked AST mode flip (operator pin row 1). The
  `scope_id` derivation MUST mirror architecture G2 /
  ScopeBinding (architecture Sec 5.2.3 line 1043): a
  deterministic UUID from `(repo_id, scope_kind,
  canonical_signature, first_seen_sha)`. None of those four
  inputs depend on which mode the adapter ran in. (Risk 9.13.)
- **C4.** `metric_version` MUST be incremented when a compute
  recipe changes in a way that alters the numeric output for
  the same input AST. Recipes are versioned independently per
  metric_kind so a fix to LCOM does not retract every other
  metric.

### 7.2 Sub-store ACL (architecture G1)

- **C5.** Five logical sub-stores, each with the writer set
  pinned by architecture Sec 1.5 G1 and reconciled at Sec
  1.5.1 (mirroring architecture's ACL table verbatim; the
  Measurement sub-store has a carve-out row for system-tier
  derived rows that is listed separately for clarity but is
  logically part of Measurement):

  | Sub-store | Tables | Writer(s) | Notes |
  |-----------|--------|-----------|-------|
  | Catalog / Lifecycle | `Repo`, `Commit`, `RepoEvent`, `MetricKind`, `ScanRun` | Management (writes `Repo`, `RepoEvent`, and every `Commit` column **except** `scan_status`) + Metric Ingestor (writes `ScanRun` and the `Commit.scan_status` column **only**) | The `Commit.scan_status` `'pending'` initial value is supplied by a schema-level column DEFAULT -- no component writes it explicitly. See architecture Sec 1.5.1 row 1 for the normative split. Repo Indexer (architecture Sec 3.3) is Management's delegated writer for the `Commit` columns Management owns |
  | Measurement | `MetricSample` rows where `pack IN ('base', 'solid', 'ingested')`, `ScopeBinding`, `MetricRetraction` | Metric Ingestor | Foundation-tier rows + retractions |
  | Measurement (system-tier carve-out) | `MetricSample` rows where `pack='system'` AND `source='derived'`, `RepoMetricSnapshot`, `CrossRepoPercentile`, `PortfolioSnapshot` | Cross-Repo Aggregator | The **only** writer of `pack='system'` rows; see architecture Sec 3.10 step 4 and Sec 1.5.1 row 2 |
  | Policy / rules | `Rule`, `RulePack`, `PolicyVersion`, `PolicyActivation`, `Threshold`, `Override` | Policy Steward | The **only** writer of `RulePack`; Management has no `mgmt.publish_rulepack` delegate (architecture Sec 1.5.1 row 4) |
  | Audit / verdict | `EvaluationRun`, `EvaluationVerdict`, `Finding` | Evaluator Surface (`eval.gate` caller) + SOLID Rule Engine batch worker (`EvaluationRun.caller='batch_refresh'`, architecture Sec 3.6) + Audit WAL Reconciler (replay-only, architecture Sec 7.10) | Three callers share one write path; `EvaluationRun.caller` tags each row's origin. WAL Reconciler never originates new rows |
  | Refactor | `HotSpot`, `RefactorPlan`, `RefactorTask` | Refactor Planner (architecture Sec 3.9) | Read-only against Measurement + Policy + Audit + Catalog |

- **C6.** All other components are read-only against sub-stores
  they do not own. Cross-writer access is enforced by per-
  service role grants in PostgreSQL (`GRANT INSERT ON ...`
  only to the named writer role).
- **C7.** The Refactor Planner is read-only against ALL sub-
  stores it consumes -- it never writes findings, never
  retracts metrics, never publishes policy. Its only writes
  are to `HotSpot`, `RefactorPlan`, and `RefactorTask`.
- **C8.** External Metric Ingest Webhook routes coverage data
  into the Measurement sub-store via the Metric Ingestor; it
  does NOT get an independent grant.

### 7.3 Policy lifecycle (architecture G5; operator pin row 4)

- **C9.** Every `PolicyVersion` row published to production
  MUST carry a valid Ed25519 signature (Sec 8.4) issued by a key
  in the active key set. Unsigned policies fail publication.
- **C10.** `PolicyVersion` rows are **immutable** once
  published. Updates publish a new version; the prior version
  remains queryable for retroactive audit.
- **C11.** The kill-switch toggles **activation**, not the
  policy contents. A killed policy version remains in the
  table and can be re-activated.
- **C12.** `eval.gate` MUST verify the policy signature before
  consulting the policy contents. A signature-verification
  failure returns `DEGRADED` with `degraded_reason =
  policy_signature_invalid` (Sec 7.7) and does NOT block the PR
  -- per operator pin `gate-degraded-policy=warn`.
- **C13.** Key rotation MUST overlap: a new key is published
  alongside the old key for at least one full publish cycle
  before the old key is retired. (Risk 9.3 mitigation.)

### 7.4 Findings cite their inputs (architecture G4)

- **C14.** Every `Finding` row MUST cite the `MetricSample`
  rows that drove the verdict via the
  `Finding.metric_sample_ids` JSON array of `sample_id`
  values (architecture Sec 5.4.1 lines 1174-1188; field name
  is **`metric_sample_ids`**, not `sample_refs`). A finding
  with an empty `metric_sample_ids` is invalid and MUST NOT
  be written.
- **C15.** Cited `MetricSample` rows MUST be active at the
  time the finding is emitted. The Evaluator Surface joins
  against `clean_code.metric_sample_active` (Sec 8.7) before
  writing -- the active-pointer table is the authoritative
  active-set source.
- **C16.** When a cited `MetricSample` is later retracted,
  the finding is NOT automatically retracted -- the historic
  audit trail is preserved. New gates evaluating the same
  `(repo, sha, policy_version)` re-emit findings from the
  current active row set.

### 7.5 Percentiles are derived (architecture G6)

- **C17.** `CrossRepoPercentile` rows are **derived** from the
  current active `MetricSample` set. They are NEVER consulted
  as authoritative input to `eval.gate` -- the gate joins the
  raw `MetricSample` rows. Percentiles are an Insights-only
  output.
- **C18.** Percentile snapshots carry a `built_at`
  timestamp (architecture Sec 5.2.5 line 1078 -- the field
  is named `built_at`, not `computed_at`). Snapshots older
  than `freshness_window_seconds` (Sec 8.2) are reported as
  stale and trigger a `DEGRADED` response with
  `degraded_reason = percentile_stale` if the Insights
  consumer specifically requested ranked output.
  `eval.gate` itself never depends on percentile freshness.

### 7.6 AST is the canonical front end (architecture G7)

- **C19.** Every metric value lands via the AST Adapter
  **except** for the four `External Metric Ingest Webhook`
  verbs whose payloads are inherently non-AST. The full set
  of non-AST exceptions in v1 is:

  | Verb (arch Sec 6.4 lines 1360-1377) | Payload | Non-AST rationale | v1 MetricSample shape |
  |------|---------|--------------------|------|
  | `ingest.coverage` | Cobertura XML | Coverage is a test-runtime measurement, not a source-tree property | `pack='ingested'`, `source='ingested'`, `metric_kind IN (coverage_line_ratio, coverage_branch_ratio)` per Sec 4.1.1 |
  | `ingest.test_balance` | JSON test-balance rows | Test pass/fail counts are CI-runtime data | `pack='ingested'`, `source='ingested'`, `metric_kind='pass_first_try_ratio'` per Sec 4.1.1 |
  | `ingest.churn` | JSON churn rows | Commit history is VCS data, not source-tree data | Feeds the `modification_count_in_window` materialiser (Sec 8.2); the materialiser writes `pack='base'`, `source='computed'`, `metric_kind='modification_count_in_window'` -- the webhook itself does NOT write a MetricSample row |
  | `ingest.defects` | JSON defect rows | Defect tracker payload is external system data | v1: ScanRun row only, **no** MetricSample row (no v1 `metric_kind` is defined; see Sec 4.11 and Sec 10A pin "ingest.defects v1") |

  For the rows that DO produce a `MetricSample`, the
  `source` field (architecture Sec 5.2.1) is `ingested` and
  `pack` is `ingested` per architecture Sec 1.4.1 + Sec 1.5
  G1 Measurement row, matching the Sec 4.1.1 catalogue. No
  metric value MAY be derived from raw regex / line-counting /
  text-grep heuristics on source files outside these
  enumerated exceptions; `provenance` on the resulting
  `MetricSample.attrs_json` records which external verb
  produced each row so downstream consumers can distinguish.
- **C20.** Tree-sitter parse errors are NOT silently dropped
  -- they produce a `ScanRun` row in `status='failed'` (the
  closed `ScanRun.status` enum at architecture Sec 5.7 line
  1280 is `running | succeeded | failed`; this doc does not
  add states) with the parse-error payload captured for
  diagnosis. Per-sample degradation (e.g. system-tier rows
  missing inputs) is signalled on `MetricSample.degraded` /
  `MetricSample.degraded_reason` (C21), NOT on the
  `ScanRun.status` enum.

### 7.7 Closed `degraded_reason` list (architecture Sec 8.2)

- **C21.** The set of values permitted in **both**
  `MetricSample.degraded_reason` (architecture Sec 5.2.2 +
  G3 line 194) **and** `EvaluationVerdict.degraded_reason`
  (architecture Sec 5.4.3) is a single **closed** enum:

  | Value | Trigger |
  |-------|---------|
  | `xrepo_edges_unavailable` | Cross-repo edges sub-graph not yet materialised for this repo |
  | `samples_pending` | One or more `MetricSample` rows for the requested `(repo, sha)` are still being computed |
  | `policy_signature_invalid` | The named `PolicyVersion` failed signature verification |
  | `percentile_stale` | (Insights surface only) percentile snapshot older than `freshness_window_seconds` |

- **C22.** Adding a new `degraded_reason` value requires (a) a
  new `PolicyVersion` publication that activates the new
  reason, (b) a code change in the Evaluator Surface, and (c)
  an update to this constraint table. Drive-by additions are
  forbidden.

### 7.8 Refactor planner is read-only across boundary

- **C23.** The Refactor Planner is forbidden from writing to
  Catalog, Measurement, Policy, or Audit sub-stores. Its
  writes are scoped to `HotSpot`, `RefactorPlan`, and
  `RefactorTask`. (Mirrors C7; called out here separately
  because it is the most commonly violated invariant in early
  implementations.)

### 7.9 Mode-flip preservation

- **C24.** Switching between `ast-mode=embedded` and
  `ast-mode=linked` MUST NOT change the value of any
  `scope_id`, `metric_kind`, or `metric_version` for the same
  source input. Operationally this means the AST canonicalisation
  (the proto representation of the parsed tree) is identical
  across modes, and `scope_id` derivation is mode-agnostic.

### 7.10 Audit append-only

- **C25.** `EvaluationRun`, `EvaluationVerdict`, and `Finding`
  rows are append-only. No `UPDATE` or `DELETE` role grant is
  issued against these tables to any service. Retention is
  governed by partition drop, not row delete (Sec 8.7).
  Mirrors architecture G3 + G4 across the Audit/verdict
  sub-store.

---

## 8. Parameter Pins

### 8.1 Storage engine and topology

- **8.1.1 Engine.** PostgreSQL 16+. We pin 16 specifically
  because the active-row enforcement (C1, Sec 8.7) relies on
  the planner's ability to do efficient FK joins from
  `metric_sample_active` to `metric_sample` and on the
  `ON CONFLICT ... DO UPDATE` upsert semantics that are stable
  in 14+ and well-tuned by 16. Earlier versions work but
  have known regressions around partition-pruned FK lookups.
- **8.1.2 Instance topology.** Single primary + read replica.
  The Insights Surface and Cross-Repo Aggregator read from
  the replica; all writers go to the primary.
- **8.1.3 Schema isolation and instance sharing.** CLEAN-CODE
  lives in a dedicated `clean_code` schema, separate from
  `agent_memory`. **Pinned default for v1:** the schema is
  hosted on the **shared PostgreSQL instance** that already
  carries `agent_memory`, with per-schema role grants
  isolating the two services' writers (C5, Sec 8.7). Rationale:
  AGENT-MEMORY precedent uses the same instance-per-org +
  schema-per-service shape; a second physical cluster doubles
  ops cost without removing the schema isolation that the
  per-role grant scheme already provides. Operators who need a
  dedicated cluster (e.g. for noisy-neighbour isolation or
  per-tenant DB-level data-residency rules in a v2 multi-tenant
  deployment, Sec 5.4) MAY override by re-targeting the
  `clean_code` schema at a separate connection string -- no
  schema, code, or role-grant changes are required. Locked
  decision row 22 reflects this pin.
- **8.1.4 Partitioning.** `MetricSample` is partitioned by
  `(metric_kind, sample_date_bucket)` where `sample_date_bucket`
  is monthly. `EvaluationRun`, `EvaluationVerdict`, and
  `Finding` are partitioned by month on their respective
  `created_at` timestamp columns (architecture Sec 5.2.1 line
  906 for `MetricSample.created_at`; Sec 5.4.1-5.4.3 lines
  1190-1212 for `EvaluationRun.created_at`,
  `EvaluationVerdict.created_at`, `Finding.created_at`).
  Retention is partition-drop only.

### 8.2 Numeric defaults

| Pin | Default | Source / rationale |
|-----|---------|--------------------|
| `scan_timeout` | 30 min | Long enough for a 1M-LOC monorepo at p95 parse cost; short enough that an orphaned scan is detected within a single sweep |
| `periodic_sweep_cadence` | 5 min | Sweeps the `ScanRun` table for rows in `running` state past `scan_timeout` and transitions them to `failed` (architecture Sec 5.7 line 1280 `ScanRun.status` enum is `running | succeeded | failed`). 5 min keeps `samples_pending` windows narrow. |
| `full_scan_cadence` | nightly (00:00 UTC) | Re-parses every active repo to catch drift; incremental scans run per push intra-day |
| `window_days` (`PolicyVersion.refactor_weights`) | 90 | Architecture Sec 1.4 row 12 + Sec 5.3.3: the commit-window the Metric Ingestor uses to materialise `modification_count_in_window` SOLID input rows on `ingest.churn` arrival. NOT a training-window for the effort model -- the effort model is trained offline on its own corpus (Sec 4.9 / Risk 9.5) |
| `freshness_window_seconds` (percentile) | 3600 | `PolicyVersion.refactor_weights.freshness_window_seconds` -- Insights stale-percentile threshold (architecture Sec 8.4 / Sec 5.3.3). `eval.gate` does not depend on this (C17) |
| `policy_publish_overlap_min_seconds` | 86400 (24 h) | Minimum key-rotation overlap (C13 mitigation) |
| `degraded_warn_window` | per-PR | Operator pin: gate degraded -> warn, applied at the gate response, not stored |

### 8.3 SLO targets

| Surface | Metric | p50 | p95 | p99 | Notes |
|---------|--------|-----|-----|-----|-------|
| `eval.gate` | response latency | 200 ms | 800 ms | 2 s | Per-PR call; CI cannot block on more |
| Metric Ingestor | sample-write throughput | 5k/s | -- | -- | Aggregate across all scans |
| AST Adapter | parse rate (LOC/s, embedded) | 50k | -- | -- | Per worker; horizontal scale by adding workers |
| Insights | ranked-percentile query | 100 ms | 500 ms | 1.5 s | Hits read replica + materialised percentile rows |
| Refactor Planner | recommendation regeneration | 5 min | -- | 30 min | Run on demand or post full-scan |

### 8.4 Policy signing

- **Algorithm.** Ed25519. Chosen for (a) deterministic
  signatures (same input always produces same signature, aids
  audit reproduction), (b) small signature size (64 bytes),
  (c) no parameter choices that can be misconfigured (no
  curve / hash / padding picks).
- **Signature scope.** The signed payload is the canonical
  JSON serialisation of the tuple `(rule_refs, threshold_refs,
  refactor_weights)` drawn from the `PolicyVersion` row --
  matching architecture Sec 5.3.3's `signature` field row
  ("Cryptographic signature over `rule_refs`, `threshold_refs`,
  `refactor_weights`"). Canonical serialisation rules: keys
  sorted lexicographically, no whitespace, ASCII-encoded
  values. Identity fields (`policy_version_id`, timestamps,
  status flags) are NOT part of the signed payload so a
  re-activation does not require re-signing.
- **Key storage.** Active and prior public keys live in the
  `clean_code.policy_signing_keys` table. Private keys live in
  the deployment's secret manager (out-of-band; service reads
  via env var on boot).
- **Rotation cadence.** Quarterly (90 days) is the default
  ceiling. Compromise triggers immediate rotation.
- **Overlap.** New key MUST be published and listed as active
  for at least `policy_publish_overlap_min_seconds` (Sec 8.2)
  before the old key is retired. During overlap, both keys
  successfully verify (C13).

### 8.5 Transport and authN

- **Agent-facing gRPC verb namespaces (per architecture Sec 6).**
  The four verb namespaces are pinned to gRPC + protobuf with
  mTLS for service-to-service auth:
  - `eval.*` -- Evaluator Surface (architecture Sec 6.2);
    the v1 verb is `eval.gate`.
  - `mgmt.*` -- Management surface (architecture Sec 6.3);
    write verbs `mgmt.register_repo`, `mgmt.set_mode`,
    `mgmt.retract_sample`, `mgmt.rescan`, `mgmt.override`,
    plus the `mgmt.read.*` family that backs the Insights
    Surface dashboards (`mgmt.read.repo`,
    `mgmt.read.metric_sample(s)`, `mgmt.read.findings`,
    `mgmt.read.regressions`, `mgmt.read.cross_repo`,
    `mgmt.read.portfolio`, `mgmt.read.refactor_plan`).
    Architecture Sec 6 does NOT define a separate `Insights`
    verb namespace; Insights dashboards are HTTP/JSON
    projections over `mgmt.read.*`.
  - `ingest.*` -- External Metric Ingest Webhook
    (architecture Sec 6.4); `ingest.coverage`,
    `ingest.test_balance`, `ingest.churn`, `ingest.defects`.
  - `policy.*` -- Policy Steward (architecture Sec 6.5);
    `policy.publish`, `policy.activate`,
    `policy.publish_rulepack`. There is no `policy.override`
    verb (architecture Sec 6.5 note); operator mute / unmute
    flows through `mgmt.override`.
- **HTTP/JSON gateway (operator-facing).** Repo onboarding,
  policy publication, kill-switch toggle, audit-log retrieval,
  Insights dashboard reads. The gateway terminates HTTP/JSON
  at the edge and translates to the underlying `mgmt.*` /
  `policy.*` gRPC calls; gated by OIDC bearer tokens. This
  pins the architecture Sec 6 "HTTP/JSON gateway" deferred
  decision to REST/JSON specifically and removes the prior
  draft's invented surface names `MetricIngest`,
  `EvaluatorGate`, `RefactorPlanner.Read`, and `Insights.Read`
  -- those names are NOT part of architecture Sec 6 and were
  retired in this iteration.
- **External webhook (per-verb transport, four verbs from
  Sec 4.11 / arch Sec 6.4).** All four verbs run over REST
  with a signed-HMAC header for source verification; the
  body media-type varies per verb:

  | Verb | Transport | Body media-type | Body shape |
  |------|-----------|------------------|------------|
  | `ingest.coverage` | REST + HMAC-signed | `application/xml` | Cobertura XML (operator pin row 2) |
  | `ingest.test_balance` | REST + HMAC-signed | `application/json` | `{scope_id, attempt_count, pass_count}` rows |
  | `ingest.churn` | REST + HMAC-signed | `application/json` | `{repo_id, file_path, sha, modified_at}` rows |
  | `ingest.defects` | REST + HMAC-signed | `application/json` | `{repo_id, file_path, sha, defect_id, severity}` rows |
- **AuthZ.** Per-service PostgreSQL role mapping (one role
  per writer per C5). REST surface enforces OIDC group
  claims; gRPC surface enforces mTLS SAN.

### 8.6 Compute Engine: per-language recipes and v1 language coverage

- **Recipe registry.** Each `metric_kind` x language pair is
  implemented as a `Recipe` interface with three methods:
  `applies_to(file_node) bool`, `compute(file_node) []Sample`,
  `version() int`. Recipes are loaded into the Compute Engine
  at boot from a static registry; runtime hot-swap is out of
  scope.
- **v1 language coverage.** **Pinned:** Go + Python +
  TypeScript + Java -- the org's top-4 languages by repo
  count. All four have mature, version-stable tree-sitter
  grammars (`tree-sitter-go`, `tree-sitter-python`,
  `tree-sitter-typescript`, `tree-sitter-java`) and have
  recipe-shape precedent in Halleck45/ast-metrics. Adding a
  language post-v1 is a recipe-pack addition (a row in
  `recipe_manifest` plus the per-language `Recipe`
  implementations) and not a schema change. The pin is
  recorded as locked decision row 21; operators who need an
  additional v1 language MAY add it by shipping the recipe
  pack and the grammar pin in the same release.
- **Recipe versioning.** Bumping a recipe's `version()` MUST
  bump the corresponding `metric_version` on emitted samples
  (C4). The Compute Engine publishes a `recipe_manifest` table
  that maps `(language, metric_kind) -> recipe_version` for
  audit.

### 8.7 Schema DDL conventions

The following DDL fragments are normative for v1. The full
schema lives in `services/clean-code/migrations/` after
implementation; the fragments here pin the architecture-
deferred choices.

**Active-row uniqueness: implementing architecture's partial
unique index** (C1, C2):

The architecture (G2 lines 148-157, Sec 5.2.1 lines 991-1003)
**is the authoritative source** for the active-row uniqueness
contract; this section is the **implementation** of that
contract, not a restatement or replacement of it. Architecture
mandates "at most one active row per quintuple `(repo_id, sha,
scope_id, metric_kind, metric_version)` ... enforced by a
partial unique index on the quintuple" while G3 (lines 908-920,
1035-1037) requires `MetricSample` rows to be **immutable** --
no column on `MetricSample` is ever updated, including any
retraction flag. Architecture line 1003 explicitly delegates
the DDL realisation to this section ("the exact DDL is in
`tech-spec.md`"); the implementation below honours both G2 and
G3 within PostgreSQL's expression-language limits.

**Implementation note (informative, not authoritative).** The
literal predicate architecture writes for the partial unique
index -- `WHERE sample_id NOT IN (SELECT sample_id FROM
MetricRetraction)` -- is a subquery, and PostgreSQL rejects
subqueries in `CREATE INDEX ... WHERE` predicates; a trigger-
maintained `is_retracted` boolean on `MetricSample` is also
unavailable because mutating that column would violate G3
immutability (C2). The implementation below therefore
**enforces architecture's quintuple uniqueness over the active
row set by materialising the active row set as a side
relation** -- a `MetricSample`-referencing pointer table
whose own `PRIMARY KEY` is a unique B-tree over the quintuple
(PRIMARY KEY in PostgreSQL is realised as a unique index).
The semantic guarantee (at most one active row per quintuple)
is identical to architecture's mandated partial unique index;
the physical relation that carries the index is different
because the natural-predicate form is not expressible in
PostgreSQL DDL. If the architecture maintainers later choose
to formalise this implementation in `architecture.md`, that
amendment is theirs to make -- this tech-spec does not claim
authority over the architecture contract and does not request
an upstream change as part of this PR.

```sql
-- Append-only row store. Plain, no triggers, no flag columns.
CREATE TABLE clean_code.metric_sample (
    sample_id      uuid PRIMARY KEY,
    repo_id        uuid NOT NULL,
    sha            text NOT NULL,
    scope_id       uuid NOT NULL,
    metric_kind    text NOT NULL,
    metric_version int  NOT NULL,
    value          double precision NOT NULL,
    pack           text NOT NULL,
    source         text NOT NULL,
    degraded       boolean NOT NULL DEFAULT false,
    degraded_reason text,
    producer_run_id uuid NOT NULL,
    attrs_json     jsonb,
    created_at     timestamptz NOT NULL DEFAULT now(),
    sample_date_bucket date GENERATED ALWAYS AS
        (date_trunc('month', created_at)::date) STORED
) PARTITION BY LIST (metric_kind);
-- per-metric_kind sub-partition by sample_date_bucket monthly

-- Tombstone log. Append-only; one row per retraction event.
CREATE TABLE clean_code.metric_retraction (
    retraction_id uuid PRIMARY KEY,
    sample_id     uuid NOT NULL UNIQUE
                  REFERENCES clean_code.metric_sample(sample_id),
    reason        text NOT NULL,
    appended_by   text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);
-- UNIQUE on sample_id prevents double-retraction.

-- Active-pointer table. One row per active quintuple.
-- Mutable by DESIGN -- the Metric Ingestor updates this row
-- when retraction + new sample land together. MetricSample
-- itself remains untouched (G3 / C2).
CREATE TABLE clean_code.metric_sample_active (
    repo_id        uuid NOT NULL,
    sha            text NOT NULL,
    scope_id       uuid NOT NULL,
    metric_kind    text NOT NULL,
    metric_version int  NOT NULL,
    sample_id      uuid NOT NULL
                   REFERENCES clean_code.metric_sample(sample_id),
    PRIMARY KEY (repo_id, sha, scope_id, metric_kind, metric_version)
);

CREATE UNIQUE INDEX metric_sample_active_sample_id_uniq
    ON clean_code.metric_sample_active (sample_id);
```

**Transactional pattern (Metric Ingestor, single writer per
C5):**

```sql
BEGIN;
-- 1. Append the new MetricSample row.
INSERT INTO clean_code.metric_sample (...) VALUES ($new_sample);

-- 2. If a prior active row existed for the same quintuple
--    (G2 allows retract-then-reinsert), tombstone it.
INSERT INTO clean_code.metric_retraction (sample_id, reason,
       appended_by)
SELECT a.sample_id, 'superseded', 'ingestor'
  FROM clean_code.metric_sample_active a
 WHERE (a.repo_id, a.sha, a.scope_id, a.metric_kind,
        a.metric_version)
     = ($repo, $sha, $scope, $kind, $version);

-- 3. Upsert the active-pointer row. PostgreSQL's standard
--    ON CONFLICT does the swap atomically.
INSERT INTO clean_code.metric_sample_active (repo_id, sha,
       scope_id, metric_kind, metric_version, sample_id)
VALUES ($repo, $sha, $scope, $kind, $version, $new_sample.id)
ON CONFLICT (repo_id, sha, scope_id, metric_kind, metric_version)
DO UPDATE SET sample_id = EXCLUDED.sample_id;
COMMIT;
```

**Reader pattern (Evaluator, Insights, Refactor Planner):**

```sql
-- Active-set query for a SHA-pinned read (Sec 7.7 / arch G3
-- Read-time semantics): join through the pointer, not the
-- raw sample table.
SELECT s.*
  FROM clean_code.metric_sample_active a
  JOIN clean_code.metric_sample s ON s.sample_id = a.sample_id
 WHERE a.repo_id = $repo AND a.sha = $sha;
```

Why this shape:

- **MetricSample is immutable (C2 / G3).** No trigger, no
  flag, no `UPDATE` ever touches `MetricSample`. The DDL grant
  in this section's next block explicitly revokes `UPDATE` and
  `DELETE` on `clean_code.metric_sample` from every role.
- **Active-row uniqueness implementation honours the
  architecture-mandated partial unique index (C1 / G2).**
  Architecture Sec 5.2.1 lines 991-1003 owns the contract
  ("at most one active row per quintuple ... enforced by a
  partial unique index on the quintuple"); this section
  implements that contract via the `PRIMARY KEY` on
  `metric_sample_active(repo_id, sha, scope_id, metric_kind,
  metric_version)` -- a unique B-tree index over the
  quintuple, scoped to the active row set by construction
  (the side relation contains only the active row pointers).
  The choice of physical relation is an implementation
  detail driven by the PostgreSQL DDL constraint described
  in the implementation note above; architecture's semantic
  guarantee (at most one active row per quintuple) is
  preserved exactly. The
  `metric_sample_active_sample_id_uniq` index additionally
  ensures a single pointer never references two samples.
- **Retract-then-reinsert at the same quintuple is supported**
  (architecture G2 lines 154-156, Sec 5.2.1 lines 981-989 for
  the `mgmt.retract_sample` + `mgmt.rescan(repo_id, sha)`
  same-SHA correction flow) by the `ON CONFLICT ... DO
  UPDATE` clause on `metric_sample_active` in the transactional
  pattern. The retracted `MetricSample` row stays in the table
  forever as the audit trail; a fresh `MetricSample` at the
  same quintuple (same `(repo_id, sha, scope_id, metric_kind,
  metric_version)`) lands as a new active row because the
  uniqueness check is over the active row set only.
- **Readers never see retracted samples** because they join
  through `metric_sample_active` rather than scanning
  `metric_sample` directly. The reader pattern is normative.

**Append-only role grants** (C2, C25):

```sql
-- Metric Ingestor role: append to samples + retractions + active-pointer swaps.
GRANT INSERT, SELECT ON clean_code.metric_sample TO clean_code_metric_ingestor;
GRANT INSERT, SELECT ON clean_code.metric_retraction TO clean_code_metric_ingestor;
GRANT INSERT, SELECT, UPDATE ON clean_code.metric_sample_active TO clean_code_metric_ingestor;
-- MetricSample remains strictly immutable.
REVOKE UPDATE, DELETE ON clean_code.metric_sample FROM PUBLIC, clean_code_metric_ingestor;
REVOKE DELETE         ON clean_code.metric_retraction FROM PUBLIC, clean_code_metric_ingestor;
REVOKE DELETE         ON clean_code.metric_sample_active FROM PUBLIC, clean_code_metric_ingestor;
-- Audit/verdict sub-store: append-only across the board for all three callers
-- (Evaluator Surface, SOLID Rule Engine batch worker, WAL Reconciler replay-only)
GRANT INSERT, SELECT ON clean_code.evaluation_run TO clean_code_evaluator, clean_code_solid_batch, clean_code_wal_reconciler;
GRANT INSERT, SELECT ON clean_code.evaluation_verdict TO clean_code_evaluator, clean_code_solid_batch, clean_code_wal_reconciler;
GRANT INSERT, SELECT ON clean_code.finding TO clean_code_evaluator, clean_code_solid_batch, clean_code_wal_reconciler;
REVOKE UPDATE, DELETE ON clean_code.evaluation_run, clean_code.evaluation_verdict, clean_code.finding FROM PUBLIC, clean_code_evaluator, clean_code_solid_batch, clean_code_wal_reconciler;
```

**Partitioning conventions** (Sec 8.1.4):

The `metric_sample` partitioning shape is pinned alongside the
table CREATE in the active-row block above: `PARTITION BY LIST
(metric_kind)` with monthly sub-partitions keyed on
`sample_date_bucket`. The active-pointer table
(`metric_sample_active`) is **not** partitioned -- it is the
hot read path for SHA-pinned reads and benefits from the single
B-tree on its primary key. Retention on `metric_sample` is
partition-drop only (Sec 8.1.4); when a `metric_sample`
partition is dropped, the corresponding `metric_sample_active`
rows are dropped via FK cascade enforced at the application
layer (the Metric Ingestor sweep, Sec 8.2 `periodic_sweep_cadence`).

**Enum types.** `degraded_reason` (C21; populates both
`MetricSample.degraded_reason` and `EvaluationVerdict
.degraded_reason`), `verdict` (`pass | warn | block` --
matches architecture Sec 5.4.3), `policy_status` (`draft |
published | killed`), and `scan_run_status` (`running |
succeeded | failed` -- matches architecture Sec 5.7 line
1280 `ScanRun.status` field verbatim; **not** to be confused
with `commit_scan_status` below) are declared as PostgreSQL
`ENUM` types. The `Commit.scan_status` column (Catalog /
Lifecycle) uses a **separate** `commit_scan_status` enum
(`pending | scanning | scanned | failed` -- architecture Sec
5.1.2 line 864); the two enums are intentionally distinct
because `Commit.scan_status` tracks per-SHA pipeline phase
while `ScanRun.status` tracks per-scan-execution outcome.
Adding a value to any of these enums requires a migration;
this is a deliberate friction to preserve C22.

**Naming conventions.** snake_case tables/columns,
`<kind>_id` for primary keys, `created_at` / `updated_at` /
`signed_at` timestamps in `timestamptz` (the canonical
append-only timestamp column name across all sub-stores is
`created_at`, matching the upstream data model at
architecture Sec 5.2.1 line 906 `MetricSample.created_at`,
Sec 5.4.1-5.4.3 lines 1190-1212 for `EvaluationRun`,
`EvaluationVerdict`, `Finding`, and the rest of the row
tables).

---

## 9. Identified Risks

Risks are recorded in **Trigger / Impact / Mitigation /
Residual** form. The risk register is the operator's living
document; new risks are appended here when they are
discovered.

### 9.1 Static AST drifts from runtime behaviour

- **Trigger.** Reflection, code generation, dynamic dispatch,
  monkey-patching, dependency injection containers, plugin
  registration -- runtime structure that the AST cannot see.
- **Impact.** Coupling and SOLID findings under-count real
  dependencies. A SOLID-clean static graph can hide a
  spaghetti runtime graph.
- **Mitigation.** Document the limitation in the
  `eval.gate` response payload as a per-finding caveat.
  Configure per-language recipes to detect common DI/
  reflection patterns (`reflect.TypeOf` in Go, `@Inject` in
  Java, `importlib` in Python) and emit a `provenance`
  marker on affected scopes so the evaluator can warn.
- **Residual.** Acknowledged. v2 may add a runtime-trace
  ingest adapter; out of scope here.

### 9.2 Embedded-mode degraded-state blast radius

- **Trigger.** A panic in a tree-sitter grammar (rare but
  documented) takes the AST adapter down. In embedded mode
  (operator pin), the adapter is in the same process as the
  Metric Ingestor.
- **Impact.** Loss of the Ingestor process triggers a flood
  of `samples_pending` degraded responses across the gate
  fleet until the process restarts.
- **Mitigation.** Run tree-sitter parses in a subprocess
  worker pool with crash isolation even in embedded mode --
  "embedded" means in-pod, not in-thread. Worker crashes
  are counted as `parse_panics_total` metric; threshold
  triggers an automatic flip to `linked` mode via the
  Management surface.
- **Residual.** A correlated panic across all workers
  remains possible if a grammar regression lands. Mitigated
  by pinning tree-sitter grammar versions in `recipe_manifest`.

### 9.3 Policy signing key compromise

- **Trigger.** Private signing key leaks (insider, malware,
  misconfigured CI secret).
- **Impact.** Attacker can publish a `PolicyVersion` that
  passes every PR.
- **Mitigation.** (a) Quarterly rotation by default (Sec 8.4).
  (b) Overlap window pinned at 24 h minimum (Sec 8.2 row 6)
  ensures rotation is feasible without verification gaps.
  (c) `EvaluationRun` rows tagged with the verifying
  `PolicyVersion` id record every `eval.gate` call, enabling
  forensic reconstruction of which policy version each
  verdict was emitted under. (d) Kill-switch (C11) allows
  operator to deactivate the compromised version immediately;
  the prior version remains in effect.
- **Residual.** Detection latency between compromise and
  rotation; bounded by audit-log review cadence.

### 9.4 Cross-repo percentile staleness on big portfolios

- **Trigger.** Portfolio grows to 1000+ repos; nightly
  full-scan + percentile recompute exceeds the
  `freshness_window_seconds`.
- **Impact.** Insights consumers see `percentile_stale`
  warnings. `eval.gate` is NOT affected (C17).
- **Mitigation.** (a) Incremental percentile recompute on
  per-repo scan completion (delta the affected metric_kind
  buckets). (b) Horizontal scale of the Cross-Repo
  Aggregator. (c) Operator can raise
  `freshness_window_seconds` per Insights consumer.
- **Residual.** Operationally accepted; gate is unaffected
  by design.

### 9.5 ML effort-model drift

- **Trigger.** Codebase evolves; historical commit patterns
  cease to predict current refactor cost (e.g. team adopts
  trunk-based dev, churn pattern flips).
- **Impact.** Refactor planner mis-ranks
  recommendations; teams lose trust in the backlog.
- **Mitigation.** (a) Re-train **on a quarterly cadence** with
  a separate `model_training_lookback_days` parameter (default
  `90`) that is **distinct from** `PolicyVersion.refactor_weights.window_days`
  (Sec 8.2). The two parameters happen to share the same default
  value but mean different things: `window_days` is the
  commit-window the Metric Ingestor uses to materialise
  `modification_count_in_window` SOLID input rows on
  `ingest.churn` arrival (architecture Sec 5.3.3, Sec 3.1 case
  3); `model_training_lookback_days` is the rolling historical
  window the offline trainer consumes from
  `EvaluationRun`/`EvaluationVerdict` to fit the effort model.
  Conflating the two would couple a policy-publish parameter to
  the ML training cadence, which we explicitly forbid. (b)
  Publish model evaluation metrics (MAE on hold-out PRs)
  alongside the model version in `recipe_manifest`. (c)
  Operator can manually re-rank or override per-recommendation.
- **Residual.** Online learning is out of scope (Sec 5.11);
  drift between quarterly trains is accepted.

### 9.6 SOLID false positives on framework code

- **Trigger.** Generated code, framework glue, test
  fixtures, vendored libraries -- code where SOLID
  violations are by design or out of the team's control.
- **Impact.** Gate fails on code the team cannot/should
  not fix; team distrusts the gate.
- **Mitigation.** (a) Path-level exclusions live on `Override`
  rows, **not** on `PolicyVersion`. Per architecture Sec 5.3.6
  (lines 1160-1170), `Override` carries a `scope_filter` JSON
  field of shape `{repo_id, scope_kind, scope_signature_glob}`
  plus a `mute` boolean; the Compute Engine respects the
  scope_filter and the Evaluator Surface short-circuits findings
  on matched scopes (writing `severity='info'` to preserve the
  audit trail when `mute=true`, per architecture Sec 5.3.6 line
  1167). Operators emit exclusions through `mgmt.override`
  (architecture Sec 6.3 / Sec 1.5.1 row 5 -- `policy.override`
  does NOT exist on the Policy Steward surface); the Policy
  Steward appends the `Override` row in the Policy/rules
  sub-store; future evaluations honour the active overrides.
  `PolicyVersion` itself carries only `rule_refs`,
  `threshold_refs`, `refactor_weights`, and `signature`
  (architecture Sec 5.3.3 lines 1125-1131) -- it does NOT
  carry path exclusions. (b) Per-finding mute / unmute also
  flows through `mgmt.override` and lands in `Override`. See
  Risk 9.10 for the mute-storm counter-risk.
- **Residual.** Tuning exclusions / mutes is per-team
  operational work; CLEAN-CODE provides the surfaces.

### 9.7 Cobertura-only coverage adapter blocks teams

- **Trigger.** Team uses JaCoCo / lcov / Clover and cannot
  emit Cobertura.
- **Impact.** Coverage metric is absent for the team;
  policies that require coverage thresholds either pass
  trivially or fail wholesale.
- **Mitigation.** (a) Coverage thresholds are opt-in per
  `PolicyVersion`. (b) Document the Cobertura conversion
  tools (`cover2cover`, `lcov-to-cobertura`) in the
  operator runbook. (c) v2 placeholder for additional
  format adapters at the External Metric Ingest Webhook.
- **Residual.** Teams without conversion tooling are
  unmeasured on coverage until v2.

### 9.8 Cycle-detection cost on monorepos

- **Trigger.** Repo with 10k+ packages; package-cycle
  computation is super-linear.
- **Impact.** Compute Engine timeouts; `samples_pending`
  windows extend; `eval.gate` returns `DEGRADED`.
- **Mitigation.** (a) Cycle detection runs as a separate
  pass scheduled outside the per-PR critical path. (b) The
  catalog metric is `cycle_member` (architecture Sec 1.4.1
  row 10; foundation-tier boolean rows -- 1 iff the scope
  participates in a strongly-connected component, with the
  cycle id carried in `MetricSample.attrs_json.cycle_id`,
  architecture Sec 5.2.1 field `attrs_json`). Computation is
  per-package/per-file rather than a single whole-repo cycle
  search, and per-cycle aggregation rolls up to the
  Cross-Repo Aggregator's system-tier `arch_debt_ratio`
  (architecture Sec 1.4.2). (c) Operator can disable
  `cycle_member` evaluation on opt-out repos via an
  `Override` row with the `cycle_member` rule muted and a
  `scope_filter` scoped to `repo_id` (architecture Sec 5.3.6).
- **Residual.** Cycle metric latency on the largest repos
  is acknowledged; the gate degrades gracefully.

### 9.9 Active-pointer table lock contention

- **Trigger.** Two concurrent scan runs against the same
  `(repo, sha)` both attempt to write samples; the
  `metric_sample_active` upsert serialises them.
- **Impact.** Tail-latency spike on Metric Ingestor writes.
- **Mitigation.** (a) Repo Indexer's scan-state machine
  guarantees one running `ScanRun` per `(repo, sha)` at a
  time; concurrent writers are an indexer bug, not a
  legitimate scenario. (b) `MetricRetraction` + new
  `MetricSample` are issued in the same transaction to
  avoid index churn.
- **Residual.** A bug in the indexer state machine can
  still produce contention; reconciler (architecture Sec 7)
  detects the resulting drift.

### 9.10 Operator mute-storm poisoning

- **Trigger.** Operator mutes findings aggressively to
  unblock a release; mutes outlive the underlying issue.
- **Impact.** Gate becomes meaningless; bad practices
  re-enter the codebase under cover of stale mutes.
- **Mitigation.** (a) The architecture's `Override` schema
  (architecture Sec 5.3.6 lines 1160-1170) is **append-only
  with latest-row-wins semantics**: the latest `Override`
  row by `created_at` for a given `(rule_id, scope_filter)`
  defines the current mute state (architecture Sec 5.3.6
  line 1170). To "expire" a mute, operators append a new
  `Override` row with `mute=false` via `mgmt.override`
  (architecture Sec 6.3 / Sec 1.5.1 row 5); the prior
  `mute=true` row stays as audit. (b) Insights surface
  exposes an "aged-mute" report listing every `(rule_id,
  scope_filter)` whose latest `Override` row is older than
  a configurable horizon (default 90 days). (c) Mute /
  unmute is operator-driven; CLEAN-CODE provides the
  visibility and the append-only history -- it does not
  add a TTL column to `Override` (architecture Sec 5.3.6
  has no `expires_at`) and does not couple
  `policy.publish` to mute lifecycle (architecture Sec 6.5
  `policy.publish` at lines 1381-1387 has no mute-reset
  semantics). A built-in TTL field and a publish-time
  mute-reset are explicitly out of scope for v1; see Sec
  10A pin "mute lifecycle".
- **Residual.** Operational governance; CLEAN-CODE
  provides the visibility, the latest-row-wins unmute
  path, and the aged-mute report.

### 9.11 ML training data leak

- **Trigger.** Effort-model training pulls historical
  commits including secrets / PII left in code.
- **Impact.** Trained model could regurgitate sensitive
  strings; persisted features embed the data.
- **Mitigation.** (a) Effort-model features are
  numerical (LOC changed, files touched, time-of-day),
  not raw commit text. (b) Training pipeline runs in an
  isolated environment; the produced model artefact is
  the only output. (c) Pre-training scan with the org's
  existing secrets detector (out of CLEAN-CODE scope).
- **Residual.** Acknowledged; mitigated by feature
  selection.

### 9.12 Future cross-store staleness (vector index)

- **Trigger.** v2 adds a vector index for code semantic
  search adjacent to the metric store; stale vector index
  desynchronises from active `MetricSample` set.
- **Impact.** Hypothetical -- not a v1 risk.
- **Mitigation.** Reserved here so v2 planners do not
  re-discover. Plan: vector index is derived (G6 pattern)
  with the same `built_at` (architecture Sec 5.2.5 line
  1078 freshness-clock convention) + freshness window
  mechanism.
- **Residual.** Out of v1 scope.

### 9.13 Mode flip (embedded <-> linked) breaks identity

- **Trigger.** Operator flips `ast-mode` between embedded
  and linked. If `scope_id` derivation is mode-dependent,
  identity continuity (C3) breaks and the partial unique
  index admits two "active" rows for the same logical
  scope.
- **Impact.** Active-row uniqueness invariant violated;
  retractions misalign; findings cite wrong samples.
- **Mitigation.** (a) `scope_id` is derived from
  `(repo_id, scope_kind, canonical_signature, first_seen_sha)`
  per architecture G2 / ScopeBinding (architecture Sec 5.2.3
  line 1043) -- none of which depend on the AST mode. (b)
  Integration test in CI flips the mode between two scans of
  the same input and asserts identical `scope_id` set.
- **Residual.** A bug in `scope_id` derivation breaks
  this; the reconciler will detect the drift but cannot
  retroactively fix the misaligned rows.

### 9.14 Tree-sitter grammar version drift

- **Trigger.** A tree-sitter grammar releases a breaking
  change to its node tree; recipes silently mis-parse and
  emit garbage metrics.
- **Impact.** Metric values change for unchanged source.
  Active-row uniqueness sees a duplicate identity if the
  grammar bump did not also bump `metric_version`.
- **Mitigation.** (a) Grammar versions are pinned in
  `recipe_manifest`. (b) Bumping a grammar requires
  bumping the dependent recipe versions (C4 forces the
  `metric_version` bump). (c) Grammar bumps land behind a
  feature flag and roll out per-repo for canary.
- **Residual.** Discipline-dependent; reconciler reports
  unexpected metric_version churn.

### 9.15 Multi-language file ambiguity

- **Trigger.** Files with embedded languages (HTML +
  inline `<script>`, Markdown + fenced code, Vue / Svelte
  single-file components).
- **Impact.** AST adapter routes the whole file to one
  grammar; recipes for the embedded language never run.
- **Mitigation.** (a) v1 treats embedded-language regions
  as opaque -- the host language's recipes apply only.
  (b) `Scope` carries a `language` field so embedded
  regions can be added in v2 without schema change.
- **Residual.** Embedded-language coverage gap
  acknowledged; affected metric_kinds are absent rather
  than wrong.

---

## 10. Locked Decisions (single-glance roll-up)

The table below is the entire decision ledger summarised. If
you only read one section of this doc, read this one.

| # | Decision | Pin | Source |
|---|----------|-----|--------|
| 1 | Storage engine | PostgreSQL 16+ | Sec 8.1.1 |
| 2 | Schema isolation | dedicated `clean_code` schema | Sec 8.1.3 |
| 3 | `MetricSample` partitioning | `(metric_kind, month)` | Sec 8.1.4 |
| 4 | Active-row uniqueness | separate `metric_sample_active` pointer table with PK on the identity quintuple; `MetricSample` immutable | C1, C2, Sec 8.7 |
| 5 | Metric / audit append-only | no `UPDATE` / `DELETE` grants on `MetricSample`; `UPDATE` only on `metric_sample_active` pointer | C2, C25, Sec 8.7 |
| 6 | `scan_timeout` | 30 min | Sec 8.2 |
| 7 | Sweep cadence | 5 min | Sec 8.2 |
| 8 | Full-scan cadence | nightly 00:00 UTC | Sec 8.2 |
| 9 | Refactor effort `window_days` (churn-materialisation only) | 90 days; **distinct from** `model_training_lookback_days` | Sec 8.2, Risk 9.5 |
| 10 | Percentile `freshness_window_seconds` | 3600 | Sec 8.2 |
| 11 | Key-rotation overlap floor | 86400 s | Sec 8.2 |
| 12 | Policy signing algorithm | Ed25519 | Sec 8.4 |
| 13 | Policy signing required | v1 required (operator pin) | C9, arch Sec 1.6 |
| 14 | AST mode default | embedded (operator pin) | Sec 4.5, arch Sec 1.6 |
| 15 | External coverage payload (verb `ingest.coverage`) | Cobertura XML only (operator pin) | Sec 4.11, arch Sec 1.6 |
| 16 | Gate degraded policy | warn (operator pin) | Sec 4.8, arch Sec 1.6 |
| 17 | Refactor effort source | ML model on historical commits (operator pin) | Sec 4.9, arch Sec 1.6 |
| 18 | Agent transport | gRPC + protobuf, mTLS | Sec 8.5 |
| 19 | Management transport | REST + JSON, OIDC | Sec 8.5 |
| 20 | External webhook transport | REST + signed-HMAC body (per verb in Sec 4.11) | Sec 8.5 |
| 21 | v1 language coverage | Go + Python + TypeScript + Java | Sec 8.6 |
| 22 | Cross-store deployment | shared PostgreSQL instance with `agent_memory`, separate `clean_code` schema, per-role grants | Sec 8.1.3 |
| 23 | Closed `degraded_reason` set | 4 values, table at C21 | Sec 7.7 |
| 24 | Sub-store writer model | 5 logical sub-stores with split-writer carve-outs per architecture Sec 1.5 G1 + Sec 1.5.1 reconciliation (Catalog/Lifecycle = Management + Metric Ingestor; Measurement system-tier carve-out = Cross-Repo Aggregator; Audit/verdict shared by Evaluator + SOLID batch + WAL Reconciler); see C5 table for the per-row writers | Sec 7.2 |
| 25 | Refactor Planner write scope | `HotSpot` / `RefactorPlan` / `RefactorTask` only | C7, C23 |
| 26 | Findings cite samples | mandatory via `Finding.metric_sample_ids`; rejected if empty | C14 |
| 27 | Percentiles derived, not authoritative | gate joins raw samples | C17 |
| 28 | AST is canonical front end | external webhook is the only exception | C19 |
| 29 | Mode-flip identity preservation | `scope_id` from `(repo_id, scope_kind, canonical_signature, first_seen_sha)` -- mode-agnostic | C3, Risk 9.13 |
| 30 | Multi-tenant | out of scope v1; **no** `tenant_id` column reserved in v1 data model (architecture Sec 5 carries none); isolation boundary is the `clean_code` schema (Sec 8.1.3); v2 shape pinned at Sec 10A as per-schema isolation | Sec 4.14, Sec 5.4, Sec 10A |

---

## 10A. Tech-spec pins for cross-doc reconciliation

The following decisions were raised in prior iterations as
candidates for operator escalation; they are **pinned in this
tech-spec** for v1. **Architecture remains the authoritative
source for every contract referenced in this section**; the
pins below are tech-spec-side **implementation choices** that
honour architecture's mandates within the constraints of the
target runtime. No operator answer is required to proceed;
architecture has not been amended and this PR does not
request an upstream amendment. If a future architecture
maintainer chooses to formalise any of these implementation
choices in `architecture.md`, that amendment is theirs to
own -- the pins here are reference material, not a directive
to architecture.

- **Pin: active-row DDL.** Architecture Sec 5.2.1 lines
  991-1003 owns the active-row constraint ("at most one
  active row per quintuple ... enforced by a partial unique
  index on the quintuple"). This tech-spec **implements**
  that constraint via the `PRIMARY KEY` on
  `clean_code.metric_sample_active(repo_id, sha, scope_id,
  metric_kind, metric_version)` (a side relation containing
  exactly the active row pointers; its PRIMARY KEY is a
  unique B-tree index over the quintuple, so the active row
  set is unique by construction). The choice of a side
  relation rather than a `CREATE UNIQUE INDEX ... WHERE`
  on `metric_sample` is forced by PostgreSQL's prohibition
  on subqueries in index predicates (Sec 7.1 C1, Sec 8.7
  implementation note); the semantic guarantee is identical
  to architecture's mandated partial unique index. This pin
  does not change the upstream contract and does not
  request an upstream amendment.
- **Pin: `ingest.defects` v1 behaviour.** v1 accepts the
  webhook verb and persists a `ScanRun` row with `kind=
  'external_per_row'`, `sha_binding='per_row'`, `to_sha=NULL`,
  and records `payload_hash` per architecture Sec 5.7
  (`ScanRun.payload_hash`); the defect payload body itself
  is not persisted in v1 because `ScanRun` carries only
  `payload_hash` upstream and no payload-body field. **No
  `MetricSample` row is written by this verb in v1** because
  no defect-derived `metric_kind` exists in the architecture
  catalogue and `MetricSample.metric_kind NOT NULL`
  (Sec 4.11, Sec 8.7 DDL). A v2 follow-on extends the
  upstream schema with a payload-body store, adds the
  defect-derived `metric_kind`, and materialises rows from
  re-ingested payloads (Sec 4.11 v1-pin row).
- **Pin: multi-tenant v2 shape.** v1 is single-tenant per
  Sec 4.14 / Sec 5.4. v2 multi-tenant migration adopts
  **per-schema isolation** (one `clean_code_<tenant>` schema
  per tenant, extending the Sec 8.1.3 single-tenant shape).
  No `tenant_id` column is reserved in v1 tables; a
  per-schema migration does not require row-level columns
  (Sec 5.4).
- **Pin: mute lifecycle.** v1 ships without a TTL column on
  `Override` (architecture Sec 5.3.6 has no `expires_at`)
  and without mute-reset semantics on `policy.publish`
  (architecture Sec 6.5). Mute / unmute lifecycle is
  operator governance via (a) the Insights aged-mute report
  and (b) the latest-row-wins `Override` semantics from
  architecture Sec 5.3.6 line 1170 -- operator appends a
  new `Override` row with `mute=false` via `mgmt.override`
  to unmute (Risk 9.10).

---

## 11. Cross-references

- **Sibling plan docs.**
  - `architecture.md` -- components (Sec 3), data model (Sec 5),
    interfaces (Sec 6), invariants G1-G7 (Sec 1.5), writer-ownership
    reconciliation table (Sec 1.5.1), operator pins (Sec 1.6),
    closed `degraded_reason` list (Sec 8.2), public-contract
    summary (Sec 9). This tech-spec defers all of these to the
    architecture doc and only re-states the invariants as
    hard constraints in Sec 7.
  - `implementation-plan.md` -- to be read alongside Sec 4
    (in-scope deliverables) for the phased build order. If
    `implementation-plan.md` proposes shipping a v1 item
    listed in Sec 5 / Sec 6, flag the disagreement in the next
    iteration summary.
  - `e2e-scenarios.md` -- to be read alongside Sec 7 (hard
    constraints) and Sec 9 (risks). Every degraded-mode
    scenario in `e2e-scenarios.md` MUST correspond to a
    value in the closed `degraded_reason` list (C21). Every
    mitigation cited in Sec 9 should have a happy-path or
    degraded-path scenario validating it.
- **Sibling story.** `code-intelligence:AGENT-MEMORY`
  (already shipped) -- shares the AST adapter back end and
  the PostgreSQL deployment shape. See AGENT-MEMORY
  `tech-spec.md` for the structural template this doc
  mirrors and for the storage / transport precedents.
- **External references.**
  - augmentcode 12-metrics guide --
    https://www.augmentcode.com/guides/12-code-quality-metrics-every-dev-team-should-track
    -- source of the foundation-tier metric catalogue
    (Sec 4.1).
  - Halleck45/ast-metrics -- https://github.com/Halleck45/ast-metrics
    -- architectural reference for the tree-sitter front-end
    + per-language recipe shape (Sec 4.5, Sec 8.6). We are not
    depending on it as a library.

---

## 12. Glossary

- **Active row.** A `MetricSample` row whose `sample_id` is
  referenced by a row in `clean_code.metric_sample_active`
  for its identity quintuple (equivalently: not present in
  `MetricRetraction`).
- **Scope.** A unit of source code -- file, package, module,
  or repo -- that a metric attaches to. Identified by
  `scope_id`, which is mode-agnostic (C3).
- **Sub-store.** One of the five logical groupings of
  tables defined by architecture G1 (Sec 1.5) and reconciled
  at architecture Sec 1.5.1. v1 follows the **split-writer
  carve-out model** documented in C5 / locked decision 24,
  NOT a single-writer-per-sub-store model: Catalog /
  Lifecycle has two writers (Management writes most `Commit`
  columns; Metric Ingestor writes `ScanRun` + the
  `Commit.scan_status` column only); Measurement has a
  carve-out row where the Cross-Repo Aggregator is the
  exclusive writer of `pack='system'` rows; Audit / verdict
  is shared by three callers (Evaluator Surface, SOLID Rule
  Engine batch worker, Audit WAL Reconciler -- the last
  replay-only). The C5 table is the authoritative listing.
- **Foundation-tier metric.** A per-scope numeric value
  carried on a `MetricSample` row with `pack IN ('base',
  'solid', 'ingested')` (architecture Sec 5.2.1 `pack` enum).
  The v1 set is the 12 AST-derived (`pack='base'` /
  `pack='solid'`) rows in Sec 4.1 plus the 3 externally-
  ingested (`pack='ingested'`) rows pinned in Sec 4.1.1.
- **System-tier metric.** A per-scope numeric value derived
  from cross-scope or cross-module relationships -- the 7
  metrics listed in Sec 4.2.
- **Rule pack.** A `PolicyVersion`-embedded set of metric
  thresholds + boolean conditions that produce `Finding`
  rows.
- **Recipe.** A per-language implementation of a
  `metric_kind`, versioned independently per kind (C4,
  Sec 8.6).
- **Degraded.** An `eval.gate` outcome that is neither
  `PASS` nor `FAIL`; communicated via a closed
  `degraded_reason` enum (C21).
