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
- **Wire-format payloads** -- owned by `architecture.md` Sec 6 (gRPC
  proto definitions for `MetricIngest`, `EvaluatorGate`, `Insights`,
  `RefactorPlanner`, `Management`).
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
- The exact partial-unique-index DDL for the `MetricSample`
  active-row uniqueness invariant (architecture Sec 5.2.1).
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
row uniqueness enforced by a partial unique index, a **signed
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
enforced by a partial unique index whose DDL is pinned in
Sec 8.7. G2 + G3 invariants from architecture Sec 1.5 are the
acceptance criteria.

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

### 4.11 External metric ingest webhook (Cobertura XML only in v1)

A webhook endpoint that accepts Cobertura XML coverage reports
and normalises them into `MetricSample` rows tagged with a
provenance marker. Operator pin: `external-metric-coverage
-format=Cobertura XML` (architecture Sec 1.6 row 2). Other
coverage formats are explicitly out of scope (Sec 5).

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

A single logical "org" per deployment. Multi-tenant isolation
(per-tenant schemas, per-tenant signing keys) is out of scope
in v1 (Sec 5.10) but the data model carries a `tenant_id` column
so v2 does not require a migration.

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
  single-tenant; the `tenant_id` column is reserved.
- **5.5 Jira / Linear / GitHub Issues integration.** The refactor
  planner emits `HotSpot` / `RefactorPlan` / `RefactorTask` rows;
  pushing them into an external tracker is a follow-on
  integration story.
- **5.6 Non-Cobertura coverage formats** (JaCoCo, lcov, Clover,
  Cobertura JSON). v1 ingests **only** Cobertura XML per
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

- **C1.** The `MetricSample` table MUST enforce active-row
  identity on the tuple `(repo_id, sha, scope_id, metric_kind,
  metric_version)`. "Active" means not present in the
  `MetricRetraction` satellite table. Enforcement is by a
  partial unique index whose DDL is pinned in Sec 8.7.
- **C2.** `MetricSample` rows are **immutable**. Corrections
  are issued by appending a new row + recording a retraction
  for the prior row in the same transaction. Direct `UPDATE`
  on `MetricSample` is forbidden at the role-grant level
  (Sec 8.7).
- **C3.** Identity invariants persist across the
  embedded<->linked AST mode flip (operator pin row 1). The
  `scope_id` derivation MUST be deterministic from
  `(repo_id, file_path_normalised, ast_node_path)` and MUST
  NOT depend on which mode the adapter ran in. (Risk 9.13.)
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
  rows that drove the verdict via a `Finding.sample_refs`
  array of `sample_id` values. A finding with empty
  `sample_refs` is invalid and MUST NOT be written.
- **C15.** Cited `MetricSample` rows MUST be active at the
  time the finding is emitted. The Evaluator Surface joins
  against the active-row partial index (Sec 8.7) before writing.
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
- **C18.** Percentile snapshots carry a `computed_at`
  timestamp. Snapshots older than `freshness_window_seconds`
  (Sec 8.2) are reported as stale and trigger a `DEGRADED`
  response with `degraded_reason = percentile_stale` if the
  Insights consumer specifically requested ranked output.
  `eval.gate` itself never depends on percentile freshness.

### 7.6 AST is the canonical front end (architecture G7)

- **C19.** Every metric value lands via the AST Adapter. No
  metric MAY be derived from raw regex / line-counting /
  text-grep heuristics. The one exception is the External
  Metric Ingest Webhook for Cobertura XML coverage, which is
  marked `provenance = external` on the resulting
  `MetricSample` rows so downstream consumers can distinguish.
- **C20.** Tree-sitter parse errors are NOT silently dropped
  -- they produce a `ScanRun` row in `degraded` state with
  the parse-error payload captured for diagnosis.

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
  because the active-row partial unique index (C1, Sec 8.7)
  relies on the planner improvements to partial indexes that
  shipped in 16. Earlier versions parse the DDL but the
  planner ignores the index for inequality predicates in some
  query shapes.
- **8.1.2 Instance topology.** Single primary + read replica.
  The Insights Surface and Cross-Repo Aggregator read from
  the replica; all writers go to the primary.
- **8.1.3 Schema isolation.** CLEAN-CODE lives in a dedicated
  `clean_code` schema, separate from `agent_memory`. Whether
  they share a physical PostgreSQL instance is an
  ops/deployment choice and not pinned here. Open question
  `metrics-store-engine` captures the operator's call.
- **8.1.4 Partitioning.** `MetricSample` is partitioned by
  `(metric_kind, sample_date_bucket)` where `sample_date_bucket`
  is monthly. `EvaluationRun`, `EvaluationVerdict`, and
  `Finding` are partitioned by month on their respective
  `recorded_at` / `evaluated_at` timestamp columns. Retention
  is partition-drop only.

### 8.2 Numeric defaults

| Pin | Default | Source / rationale |
|-----|---------|--------------------|
| `scan_timeout` | 30 min | Long enough for a 1M-LOC monorepo at p95 parse cost; short enough that an orphaned scan is detected within a single sweep |
| `periodic_sweep_cadence` | 5 min | Sweeps the `ScanRun` table for rows in `running` state past `scan_timeout` and transitions them to `degraded`. 5 min keeps `samples_pending` windows narrow. |
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

- **Agent-facing gRPC.** `eval.gate`, `MetricIngest`,
  `RefactorPlanner.Read`, `Insights.Read`. gRPC + protobuf.
  mTLS for service-to-service.
- **Management surface (REST/JSON).** Repo onboarding, policy
  publication, kill-switch toggle, audit-log retrieval. REST
  + JSON, gated by OIDC bearer tokens. This pins the
  architecture Sec 6 "HTTP/JSON gateway" deferred decision to
  REST/JSON specifically.
- **External webhook (Cobertura).** REST + XML body, signed
  HMAC header for source verification.
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
- **v1 language coverage.** UNDECIDED -- captured as open
  question `v1-language-coverage`. Default proposal pending
  operator confirmation: Go + Python + TypeScript + Java
  (covering the org's top-4 languages by repo count). All four
  have mature tree-sitter grammars. Adding a language post-v1
  is a recipe-pack addition, not a schema change.
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

**Active-row partial unique index on `MetricSample`** (C1):

```sql
-- Append-only constraint: no direct UPDATE / DELETE grant
CREATE INDEX metric_sample_active_uniq
  ON clean_code.metric_sample (
    repo_id,
    sha,
    scope_id,
    metric_kind,
    metric_version
  )
  WHERE sample_id NOT IN (
    SELECT sample_id FROM clean_code.metric_retraction
  );
-- Note: enforced as a unique index; PostgreSQL 16+ planner
-- correctly uses this for active-row joins. We rely on the
-- retraction insert + new-sample insert being in the same
-- transaction (C2) so the active set is consistent.
```

> Implementation note: the literal partial-unique-index form
> above uses a subquery, which PostgreSQL rejects in index
> predicates. The production form materialises an
> `is_retracted boolean` column on `MetricSample` and uses
> `WHERE is_retracted = false` in the index predicate, with
> a trigger on `MetricRetraction` that flips the flag inside
> the same transaction. The semantic guarantee (active-row
> uniqueness) is the constraint that matters; the DDL shape
> is an implementation detail captured here so the implementor
> does not reinvent it.

**Append-only role grants** (C2, C25):

```sql
-- Metric Ingestor role
GRANT INSERT, SELECT ON clean_code.metric_sample TO clean_code_metric_ingestor;
GRANT INSERT, SELECT ON clean_code.metric_retraction TO clean_code_metric_ingestor;
-- Explicitly NO UPDATE or DELETE grant
REVOKE UPDATE, DELETE ON clean_code.metric_sample FROM PUBLIC, clean_code_metric_ingestor;
-- Audit/verdict sub-store: append-only across the board for all three callers
-- (Evaluator Surface, SOLID Rule Engine batch worker, WAL Reconciler replay-only)
GRANT INSERT, SELECT ON clean_code.evaluation_run TO clean_code_evaluator, clean_code_solid_batch, clean_code_wal_reconciler;
GRANT INSERT, SELECT ON clean_code.evaluation_verdict TO clean_code_evaluator, clean_code_solid_batch, clean_code_wal_reconciler;
GRANT INSERT, SELECT ON clean_code.finding TO clean_code_evaluator, clean_code_solid_batch, clean_code_wal_reconciler;
REVOKE UPDATE, DELETE ON clean_code.evaluation_run, clean_code.evaluation_verdict, clean_code.finding FROM PUBLIC, clean_code_evaluator, clean_code_solid_batch, clean_code_wal_reconciler;
```

**Partitioning conventions** (Sec 8.1.4):

```sql
CREATE TABLE clean_code.metric_sample (
    sample_id     uuid PRIMARY KEY,
    repo_id       uuid NOT NULL,
    sha           text NOT NULL,
    scope_id      text NOT NULL,
    metric_kind   text NOT NULL,
    metric_version int  NOT NULL,
    value         double precision NOT NULL,
    is_retracted  boolean NOT NULL DEFAULT false,
    recorded_at   timestamptz NOT NULL DEFAULT now(),
    sample_date_bucket date GENERATED ALWAYS AS (date_trunc('month', recorded_at)::date) STORED
) PARTITION BY LIST (metric_kind);
-- per-metric_kind sub-partition by sample_date_bucket monthly
```

**Enum types.** `degraded_reason` (C21; populates both
`MetricSample.degraded_reason` and `EvaluationVerdict
.degraded_reason`), `verdict` (`pass | warn | block` --
matches architecture Sec 5.4.3), `policy_status` (`draft |
published | killed`), `scan_state` (`pending | running |
completed | degraded`) are declared as PostgreSQL `ENUM`
types. Adding a value requires a migration; this is a
deliberate friction to preserve C22.

**Naming conventions.** snake_case tables/columns,
`<kind>_id` for primary keys, `recorded_at` / `updated_at` /
`signed_at` timestamps in `timestamptz`.

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
- **Mitigation.** (a) Re-train quarterly on the rolling
  `window_days=90` window (Sec 8.2). (b) Publish model
  evaluation metrics (MAE on hold-out PRs) alongside the
  model version in `recipe_manifest`. (c) Operator can
  manually re-rank or override per-recommendation.
- **Residual.** Online learning is out of scope (Sec 5.11);
  drift between quarterly trains is accepted.

### 9.6 SOLID false positives on framework code

- **Trigger.** Generated code, framework glue, test
  fixtures, vendored libraries -- code where SOLID
  violations are by design or out of the team's control.
- **Impact.** Gate fails on code the team cannot/should
  not fix; team distrusts the gate.
- **Mitigation.** (a) `PolicyVersion` carries a
  `path_exclusions` glob list; the Compute Engine respects
  exclusions and the Evaluator Surface short-circuits
  findings on excluded scopes. (b) Per-finding mute / unmute
  flows through the `mgmt.override` verb (architecture Sec
  1.5.1 row 5 -- `policy.override` does NOT exist on the
  Policy Steward surface) and lands in the `Override` row in
  the Policy / rules sub-store; future evaluations honour
  active overrides. See Risk 9.10 for the mute-storm
  counter-risk.
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
  metric_kind `package_cycle_count` is computed per
  module / package and aggregated -- not as a single
  whole-repo cycle search. (c) Operator can disable the
  metric on opt-out repos via `PolicyVersion`.
- **Residual.** Cycle metric latency on the largest repos
  is acknowledged; the gate degrades gracefully.

### 9.9 Active-row partial unique index lock contention

- **Trigger.** Two concurrent scan runs against the same
  `(repo, sha)` both attempt to write samples; the partial
  unique index serialises them.
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
- **Mitigation.** (a) Mutes carry an `expires_at`; default
  TTL is 30 days. (b) Insights surface exposes a
  "muted-findings aged > 90d" report. (c)
  `PolicyVersion.publish` can reset all mutes (operator
  opt-in).
- **Residual.** Operational governance; CLEAN-CODE
  provides the visibility.

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
  with the same `computed_at` + freshness window mechanism.
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
  `(repo_id, normalised_file_path, ast_node_path)` --
  none of which depend on the AST mode. (b) Integration
  test in CI flips the mode between two scans of the same
  input and asserts identical `scope_id` set.
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
| 4 | Active-row uniqueness | partial unique index via `is_retracted` flag | C1, Sec 8.7 |
| 5 | Metric / audit append-only | no `UPDATE` / `DELETE` grants | C2, C25, Sec 8.7 |
| 6 | `scan_timeout` | 30 min | Sec 8.2 |
| 7 | Sweep cadence | 5 min | Sec 8.2 |
| 8 | Full-scan cadence | nightly 00:00 UTC | Sec 8.2 |
| 9 | Refactor effort `window_days` | 90 | Sec 8.2 |
| 10 | Percentile `freshness_window_seconds` | 3600 | Sec 8.2 |
| 11 | Key-rotation overlap floor | 86400 s | Sec 8.2 |
| 12 | Policy signing algorithm | Ed25519 | Sec 8.4 |
| 13 | Policy signing required | v1 required (operator pin) | C9, arch Sec 1.6 |
| 14 | AST mode default | embedded (operator pin) | Sec 4.5, arch Sec 1.6 |
| 15 | External coverage format | Cobertura XML only (operator pin) | Sec 4.11, arch Sec 1.6 |
| 16 | Gate degraded policy | warn (operator pin) | Sec 4.8, arch Sec 1.6 |
| 17 | Refactor effort source | ML model on historical commits (operator pin) | Sec 4.9, arch Sec 1.6 |
| 18 | Agent transport | gRPC + protobuf, mTLS | Sec 8.5 |
| 19 | Management transport | REST + JSON, OIDC | Sec 8.5 |
| 20 | External webhook transport | REST + XML, HMAC | Sec 8.5 |
| 21 | v1 language coverage | UNDECIDED -- open question `v1-language-coverage` | Sec 8.6 |
| 22 | Cross-store deployment | UNDECIDED -- open question `metrics-store-engine` | Sec 8.1.3 |
| 23 | Closed `degraded_reason` set | 4 values, table at C21 | Sec 7.7 |
| 24 | Writer count per sub-store | exactly 1, table at C5 | Sec 7.2 |
| 25 | Refactor Planner write scope | `HotSpot` / `RefactorPlan` / `RefactorTask` only | C7, C23 |
| 26 | Findings cite samples | mandatory; rejected if empty | C14 |
| 27 | Percentiles derived, not authoritative | gate joins raw samples | C17 |
| 28 | AST is canonical front end | external webhook is the only exception | C19 |
| 29 | Mode-flip identity preservation | `scope_id` mode-agnostic | C3, C24, Risk 9.13 |
| 30 | Multi-tenant | out of scope v1; `tenant_id` reserved | Sec 4.14, Sec 5.4 |

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
  not present in `MetricRetraction` (equivalently:
  `is_retracted = false`).
- **Scope.** A unit of source code -- file, package, module,
  or repo -- that a metric attaches to. Identified by
  `scope_id`, which is mode-agnostic (C3).
- **Sub-store.** One of the five logical groupings of
  tables with a single writer (C5).
- **Foundation-tier metric.** A per-scope numeric value
  derived from a single language's AST -- the 12 metrics
  listed in Sec 4.1.
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
