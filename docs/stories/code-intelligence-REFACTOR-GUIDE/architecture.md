# Refactor Guide -- Architecture

> Story: `code-intelligence:REFACTOR-GUIDE` -- 21 points
> Companion docs (parallel-authored): [`tech-spec.md`](tech-spec.md),
> [`implementation-plan.md`](implementation-plan.md),
> [`e2e-scenarios.md`](e2e-scenarios.md).
> Authority: per the repo `README.md`, when docs and code disagree
> the docs win. The base service architecture lives in
> [`../code-intelligence-CLEAN-CODE/architecture.md`](../code-intelligence-CLEAN-CODE/architecture.md)
> ("CLEAN-CODE arch" below) and remains the single source of truth
> for the metric catalogue, the policy / rule data model, the
> writer-ownership invariants (G1 - G7), and the refactor planner.
> This document is a **derived plan** that bridges the existing
> service to a single-binary developer-laptop CLI; it does NOT
> redefine any contract that CLEAN-CODE arch already owns.

## 1. Purpose and Scope

`cleanc analyze <repo-path>` is a single-binary CLI that scans a
local checkout for clean-code violations and produces refactor
suggestions as actionable code-change inputs. It is the
developer-laptop on-ramp to the same metric / policy / planner
machinery the production `clean-code` service exposes -- with no
PostgreSQL, no HTTP gateway, and no docker stack required.

This document defines:

- The L1 - L9 component layers (Section 3) that compose the CLI,
  reusing the existing `services/clean-code/internal/{ast,metrics,
  rule_engine,refactor,policy}` packages and adding the missing
  walker / composition root / suggestion emitter / dev-policy
  loader.
- The in-memory data model the CLI assembles before handing each
  row to a reused interface (Section 4).
- The Go-level interfaces (Section 5) that bind the new CLI
  packages to the reused engine packages. **No new domain
  interface is invented**; every binding maps onto an existing
  contract (`refactor.PolicyReader`, `refactor.MetricSampleReader`,
  `refactor.FindingReader`, `refactor.HotSpotWriter`,
  `refactor.FindingDetailReader`, `refactor.HotSpotReader`,
  `refactor.RefactorPlanTaskWriter`, `rule_engine.Store`).
- End-to-end sequence flows for the primary scenarios (Section
  6): P0 analyze, P1 prompt emission, and the P3 mechanical-patch
  preview.
- The dev-mode boundaries (Section 7) for policy signing bypass
  and the ONNX effort-model fallback.

The story description's L1 - L9 gap table is the spine of every
later section; each component subsection in Section 3 begins by
restating its L-number and "missing / reusable / friction"
disposition.

### 1.1 In scope / out of scope

| In scope | Out of scope |
| --- | --- |
| `cleanc analyze <path>` CLI shell, sub-commands `analyze`, `report`, future `apply` | Re-implementing any metric recipe, rule engine, or planner contract (all reused from CLEAN-CODE) |
| Recursive repo walker honouring `.gitignore` / `.git/` / `node_modules` / `vendor/` / `target/` (Section 3.1) | Walking remote repos; the CLI accepts a local path only |
| Composition root that wires `parser.Registry` -> `recipes` -> `rule_engine.Engine` -> `refactor.Planner` -> `refactor.TaskPlanner` using only the in-memory stores already shipped in those packages | Re-implementing those packages or their SQL writers (the production service keeps owning them) |
| Dev-mode `PolicyVersion` synthesiser that loads YAML rule packs from disk (or embedded) and constructs an unsigned `steward.PolicyVersion` for the in-memory `rule_engine.InMemoryStore` | Re-implementing rule pack signature verification or threshold publishing; v1 production still requires signed policies per CLEAN-CODE arch G5 |
| Deterministic effort estimator that runs when the ONNX model is missing or non-loadable | Re-implementing the Stage 8.3 ML effort model |
| Markdown + JSON serialisers for `refactor.PlanResult` and the per-hotspot task list | UI / dashboard rendering |
| **Structured refactor-edit-instructions emitter** (the operator's pinned L7 Option A): one JSON-Lines record per `refactor_task`, carrying `rule_id`, `file`, `line_range`, `scope_kind`, `source_snippet`, `task_kind`, `prose_suggestion` -- pasted into an AI coder to synthesise patches | Synthesising patches inside the CLI binary itself (deferred to P3 mechanical patches; see open question `L7-authority`) |
| Stable `scope_id` minting from `(repo_id, scope_kind, canonical_signature, first_seen_sha)` where `repo_id` is a deterministic UUID derived from the absolute local path so re-runs are idempotent (CLEAN-CODE arch G2) | Cross-repo / portfolio percentiles; the CLI is single-repo, single-SHA |
| Graceful "dark metric" reporting: when a metric recipe sits on a parser attr that today's Stage 2.1 parser fleet does not emit (`decision_blocks`, `call_edges`, `field_accesses`), the CLI prints a one-line "metric dark: needs parser extension" diagnostic instead of silently dropping it | Extending the per-language parsers to close the dark-metric gap; that work is the P2 roadmap item and ships under a separate story |

### 1.2 References from the story brief

The story description's gap analysis pins the L1 - L9 layer
nomenclature this doc uses verbatim. Every L-numbered subsection
in Section 3 anchors back to the matching cell in the story's
"Summary" table and the matching "L`N` --" deep-dive. The four
operator-pinned open questions in the story description
("Architectural authority for L7", "Language priority", "Where
does the CLI binary live", "Rule pack distribution") are
re-emitted as structured `open-questions` at the bottom of this
file so the wizard can route them to the operator.

### 1.3 Operator pins (this story)

This story does not introduce new metric_kinds or rule packs;
the pins below are CLI-shape choices that the rest of the
document hangs from. Where a pin is **proposed** the operator
has not yet answered; the corresponding entry appears in the
Section 8 `open-questions` block and the next iteration of this
doc resolves it.

| Pin id | Question | Default proposed | Re-stated at |
| --- | --- | --- | --- |
| `cli-binary-location` | Where does the `cleanc` binary live? | `services/clean-code/cmd/cleanc/main.go` (sibling to the six existing service binaries) | Section 3.6, Section 9; open question `cli-binary-location` |
| `cli-policy-distribution` | How are rule packs distributed with the CLI? | `//go:embed` the canonical `services/clean-code/policy/rulepacks/{solid,decoupling}/*.yaml` set into the binary; allow `--policy <path>` override | Section 3.8, Section 7.1; open question `cli-policy-distribution` |
| `cli-l7-authority` | Does the CLI emit structured refactor prompts only (Option A) or also synthesise mechanical patches (Options B/C)? | **Option A only for P1**; Options B/C deferred to P3 pending an architecture amendment to lift the CLEAN-CODE arch Section 1.2 "no auto-fix" clause | Section 3.7, Section 6.2, Section 9; open question `L7-authority` |
| `cli-language-priority` | Which target language is P2's parser-attr extension first? | **Go first**, then Python, then TypeScript, then Java (matches the existing test corpus weighting) | Section 9; open question `language-priority` |
| `cli-dev-policy-signature` | Does dev-mode skip signature verification entirely or accept a baked-in dev key? | **Skip with a loud `WARNING: dev-mode policy is unsigned` banner**; production binaries refuse to skip via a `-tags prod` build constraint | Section 3.8, Section 7.2 |
| `cli-effort-fallback-formula` | When the ONNX effort model is missing, what deterministic estimator runs? | `effort_hours = round_half_up(0.02 * loc + 0.10 * cyclo + 0.05 * fan_in + 1.0, 1)` clamped to `[0.1, 80.0]` | Section 3.5, Section 4.5 |

### 1.4 Guiding invariants this story honours

The CLI is a **read-mostly composition** over the existing
engine packages; it does not author new sub-stores. The
CLEAN-CODE arch invariants G1 - G7 apply unchanged. Three of
them control CLI design directly:

- **G1 (one writer per sub-store).** The CLI does not write to
  any production sub-store. Every "write" in the CLI is into a
  process-local in-memory store (`rule_engine.InMemoryStore`,
  `refactor.InMemoryHotSpotWriter`,
  `refactor.InMemoryRefactorPlanTaskWriter`). The binary exits
  without persisting anything to disk by default; outputs are
  emitted to stdout / `--out` files only.
- **G2 (deterministic identity).** `scope_id` UUIDs are minted
  from `(repo_id, scope_kind, canonical_signature,
  first_seen_sha)`. The CLI mints `repo_id` as a deterministic
  UUID-v5 (namespace = a fixed `cleanc.local-repo` URL,
  name = the absolute local path normalised to forward slash);
  `first_seen_sha` is the current `HEAD` commit if a git repo is
  present, else the literal string `"working-copy"` so an
  un-versioned tree still yields stable IDs across re-runs.
- **G7 (AST is the canonical front end).** The CLI uses
  `parser.DefaultRegistry()` and never bypasses it. New
  languages land by registering grammars in that registry, not
  by introducing CLI-specific language branches.

### 1.5 Definition of "primary scenarios" used in Section 6

Three end-to-end flows anchor every sequence diagram:

1. **P0 analyze.** `cleanc analyze <path> --out report.md` walks
   the repo, computes the foundation-tier metrics today's parser
   fleet supports (`loc`, `duplication_ratio`, `cycle_member`,
   `interface_width`, `depth_of_inheritance`), evaluates the
   loaded rule pack, ranks hotspots, plans tasks, and writes a
   markdown report.
2. **P1 prompt emission.** `cleanc analyze <path> --emit-prompts
   prompts.jsonl` runs the same pipeline and additionally writes
   one JSON-Lines `RefactorPromptRecord` (Section 4.6) per
   emitted `refactor_task`. The file is the AI-coder hand-off.
3. **P3 mechanical-patch preview (gated, future).** `cleanc
   analyze <path> --apply <task_id> --dry-run` is the future
   surface; this doc only sketches it because the operator pin
   `cli-l7-authority` defers Option B/C to P3.

---

## 2. Context

The CLI is a single binary that loads code paths from disk,
parses them, ranks them, and writes output to stdout or files.
There is no network surface, no HTTP server, no database.

```
+-------------------+    invokes      +----------------------+
| developer at CLI  |---------------> | cleanc binary        |
| (terminal)        |                 |   (Section 3.6)      |
+-------------------+                 +-----------+----------+
                                                  |
                                                  v
                +----------------------------------+----------------------------------+
                |                       Composition root                              |
                |                          (Section 3.6)                              |
                +---+--------+--------+---------+----------+----------+---------+-----+
                    |        |        |         |          |          |         |
                    v        v        v         v          v          v         v
              +---------+ +------+ +--------+ +------+ +--------+ +-------+ +------+
              | Walker  | |Parser| |Recipes | |Rule  | |Refactor| | Task  | |Report|
              | (L1,    | |(L2,  | |(L3,    | |Engine| |Planner | |Planner| |/Sug. |
              |  3.1)   | | 3.2) | | 3.3)   | |(L4,  | |(L5,    | |(L5,   | |(3.7) |
              |         | |      | |        | | 3.4) | | 3.5)   | | 3.5)  | |      |
              +----+----+ +--+---+ +--+-----+ +--+---+ +--+-----+ +---+---+ +--+---+
                   |         |        |          |        |          |        |
                   |         v        v          v        v          v        v
                   |   +-----------------------------------------------------------+
                   |   |    In-memory state (process-local; no DB, no IO)          |
                   |   |  parser.Registry, InMemoryStore (rule_engine),            |
                   |   |  InMemoryMetricSampleReader, InMemoryFindingReader,       |
                   |   |  InMemoryHotSpotWriter, InMemoryHotSpotReader,            |
                   |   |  InMemoryFindingDetailReader,                             |
                   |   |  InMemoryRefactorPlanTaskWriter,                          |
                   |   |  DevPolicyLoader (Section 3.8)                            |
                   |   +-----------------------------------------------------------+
                   v
              +----------------------+
              | Local repo on disk   |
              | (read-only)          |
              +----------------------+

                                outputs (stdout / files)
                                          |
                                          v
                +----------------+   +-----------------+   +-------------------+
                | report.md      |   | prompts.jsonl   |   | findings.json     |
                | (P0, Sec 3.7)  |   | (P1, Sec 3.7)   |   | (always)          |
                +----------------+   +-----------------+   +-------------------+
```

Surfaces in plain English:

- **Walker (L1)** is the only component that touches the
  filesystem. It hands `(path, content_bytes)` pairs downstream.
- **Parser (L2)** is the existing `parser.Registry`. The CLI does
  not add new parsers; it uses `DefaultRegistry()` and the four
  v1-pinned languages (Go / Python / TypeScript / Java).
- **Recipes (L3)** are the existing
  `internal/metrics/recipes` set. Recipes that need parser attrs
  not yet emitted (`decision_blocks`, `call_edges`,
  `field_accesses`) yield zero drafts; the CLI surfaces that as
  a "dark metric" diagnostic, never as silent zero rows.
- **Rule Engine (L4)** is `rule_engine.Engine` over
  `rule_engine.InMemoryStore`. The CLI pre-loads the store with
  the active `PolicyVersion`, the rule rows, the
  `commit_parents` map (a single entry: current SHA -> "" so
  the engine treats every firing as `delta=new`), and the
  computed `Sample` rows.
- **Refactor Planner (L5)** is `refactor.Planner` +
  `refactor.TaskPlanner`. The CLI wires the in-memory variants
  of every reader / writer (`InMemoryMetricSampleReader`,
  `InMemoryFindingReader`, `InMemoryHotSpotWriter`,
  `InMemoryHotSpotReader`, `InMemoryFindingDetailReader`,
  `InMemoryRefactorPlanTaskWriter`).
- **Report / Suggest (L7)** are new packages
  (`internal/cli/report`, `internal/cli/suggest`) that serialise
  `PlanResult` and `RefactorTask` into markdown / JSON / JSONL
  artifacts.
- **DevPolicy (L8)** is a new package
  (`internal/cli/devpolicy`) that loads the embedded or
  filesystem rule packs and synthesises an in-memory
  `steward.PolicyVersion` with an empty `Signature` slot.

---

## 3. Components and responsibilities

Section 3 is organised by the L1 - L9 layer numbers from the
story brief. Each subsection opens with the layer's disposition
("missing", "reusable", "friction"), its target Go package
under `services/clean-code/`, and the exact reused / new
symbols.

### 3.1 L1 -- Repo Walker (NEW)

**Disposition:** missing. **Package:** `internal/cli/walk` (new).
**Responsibility:** turn a local repo path into a stream of
`(path, content)` pairs the parser registry can consume.

The walker is the only CLI component with filesystem side
effects. It owns:

- Recursive directory traversal of the supplied root path.
- Skip rules:
  - `.git/`, `node_modules/`, `vendor/`, `target/`, `dist/`,
    `build/`, `.next/`, `__pycache__/`, `.venv/`, `venv/` -- a
    hard-coded baseline list.
  - Files matched by `.gitignore` / `.git/info/exclude` /
    `~/.config/git/ignore`. The walker uses
    `github.com/go-git/go-git/v5/plumbing/format/gitignore` (a
    dependency already implied by the agent-memory service)
    instead of inventing a new gitignore engine. The next
    iteration of `tech-spec.md` MUST pin the exact import path
    if the operator answers `cli-policy-distribution` in a way
    that adds or removes a vendored package.
  - Files whose detected language is not in
    `parser.SupportedLanguages` (`go`, `python`, `typescript`,
    `java`). Detection delegates to `parser.DetectLanguage`.
  - Files over the per-file size cap. The cap mirrors the
    constant the production Metric Ingestor uses to keep parsed
    output bounded (`tech-spec.md` pins the literal value).
- Optional shallow git inspection: when the root path is a git
  working tree the walker reads `HEAD` to fill `repo.head_sha`
  (Section 4.1) and optionally the last `N` commits per file
  for the future `modification_count_in_window` metric (gated
  by `--with-churn`; off by default since the metric is dark
  until P2 lands the churn ingest path).

The walker emits one `WalkedFile` (Section 4.2) per kept file
to a buffered channel; the composition root fans out parser
calls across `GOMAXPROCS` goroutines.

**Failure modes:**

- A read error on a single file is non-fatal: the walker records
  a `WalkSkip{Path, Reason}` and continues. The composition
  root aggregates skips into the `findings.json` `skips` array.
- Walking a non-existent root path returns
  `ErrRootNotFound` and the CLI exits with code 2.
- Symlink loops are broken by a `visited` set keyed on
  `(device_id, inode)` on POSIX, on the canonical path string
  on Windows.

### 3.2 L2 -- AST Parser (REUSED)

**Disposition:** reusable as-is. **Package:**
`internal/ast/parser` (existing). **Responsibility:** turn
`(path, content)` into `*parser.AstFile`.

The CLI calls `parser.DefaultRegistry().Parse(ctx, path,
content)`. Per
`services/clean-code/internal/ast/parser/registry.go:97`, the
registry detects the language by extension and dispatches to
the registered factory. The CLI does not register new
languages; the per-language adapters self-register via
`init()`.

Failures:

- `parser.ErrUnsupportedLanguage` -- the walker should have
  filtered the file out, so reaching this path is a bug; the
  composition root logs and skips.
- `parser.ErrEmptyContent` -- the walker filters zero-byte files
  upstream for the same reason.

CGO / non-CGO selection follows the existing
`parsers_cgo.go` / `parsers_nocgo.go` build-tag fork; the CLI
inherits the active build's behaviour without intervention.

### 3.3 L3 -- Metric Recipes (REUSED, parser-gap aware)

**Disposition:** reusable but parser-gap-limited. **Package:**
`internal/metrics/recipes` (existing). **Responsibility:** turn
each `*parser.AstFile` plus the project-wide collection of
`*AstFile`s into `MetricSampleDraft` rows.

The CLI calls each recipe via the existing
`recipes.Recipe.AppliesTo` / `Recipe.Apply` contract. Recipes
that today gate on parser attrs the Stage 2.1 fleet does not
emit (`AttrDecisionBlocks`, `AttrCallEdges`,
`AttrFieldAccesses`) return `false` from `AppliesTo` and emit
zero drafts -- not a silent zero-value sample but a no-op.

The CLI's orchestrator (Section 3.6) tracks which recipes
returned zero drafts due to a known-dark attr and surfaces a
"dark metric" diagnostic per `(metric_kind, language)` pair in
the `report.md` and in a `--diagnostics` JSON output. This
makes the parser-attr gap visible to the operator without
faking samples.

The recipes that **light up today** for the four pinned
languages (per the story brief's L3 deep-dive):

| metric_kind | scope | pack | Lights up today? |
| --- | --- | --- | --- |
| `loc` | file, package, repo | base | yes |
| `duplication_ratio` | file, package | base | yes (uses `AttrSourceBytes`) |
| `cycle_member` | file, package | base | yes (uses `imports` / `extends` edges + `AttrModulePath`) |
| `interface_width` | class, interface | solid | yes (scope tree only) |
| `depth_of_inheritance` | class | solid | yes (scope tree only) |
| `coupling_between_objects` | class | solid | partial (depends on edges) |
| `cyclo` | method, file | base | **dark** -- needs `decision_blocks` attr |
| `cognitive_complexity` | method, file | base | **dark** -- needs `decision_blocks` attr |
| `fan_in` | method, class, file | solid | **dark** -- needs `call_edges` attr |
| `fan_out` | method, class, file | solid | **dark** -- needs `call_edges` attr |
| `lcom4` | class | solid | **dark** -- needs both `call_edges` AND `field_accesses` |
| `lsp_violation` | method | solid | dark until cross-class signature analysis ships |
| `modification_count_in_window` | file, method | base | dark until the `--with-churn` git-history path lands |

The dark metrics are listed here, not hidden, so a reader of
the plan can audit what the CLI does and does not cover at any
phase boundary. P2 of the roadmap (Section 9) closes the
parser-attr gap one language at a time.

### 3.4 L4 -- Rule Engine and Policy DSL (REUSED)

**Disposition:** reusable as-is. **Packages:**
`internal/rule_engine` and `internal/policy/{dsl,steward}`
(existing). **Responsibility:** evaluate the rule pack against
the computed samples and produce `Finding` rows.

The CLI uses `rule_engine.InMemoryStore` (see
`services/clean-code/internal/rule_engine/inmem_store.go:16-29`,
which documents the store as "used by ... the scaffold-mode
composition root ... when CLEAN_CODE_PG_URL is unset"). The
composition root pre-loads the store with:

1. The synthesised `steward.PolicyVersion` (Section 3.8) via
   `InMemoryStore.InsertPolicyVersion`.
2. Each `steward.Rule` referenced by the policy via
   `InMemoryStore.InsertRule` (the YAML loader emits one
   `steward.Rule` per `rules:` entry in the rule pack).
3. Each `rule_engine.Sample` synthesised from the recipe
   drafts (Section 4.4) via `InMemoryStore.InsertSample`.
4. A single `commit_parents` entry: `(repo_id, head_sha) -> ""`
   so the engine treats the working copy as a root commit; this
   makes every firing rule produce a `delta=new` finding (per
   the engine's documented "no prior" branch).

The CLI then calls `Engine.RunBatch(ctx, repoID, headSHA,
policyVersionID)` and consumes the returned `RunResult` for
audit (the `EvaluationRun`, `EvaluationVerdict`, and finding
ids land in the in-memory store and are read back by the
report renderer).

### 3.5 L5 -- Refactor Planner and Task Planner (REUSED, with effort fallback)

**Disposition:** reusable as-is plus a fallback effort
estimator. **Package:** `internal/refactor` (existing) plus a
new `internal/cli/effort` (new, ~50 LOC).

The CLI wires the planner per
`services/clean-code/internal/refactor/planner.go:498-545`:

- `refactor.NewInMemoryMetricSampleReader()` is fed the
  per-scope foundation-tier samples (filtered to
  `refactor.HotSpotInputMetricKinds`).
- `refactor.NewInMemoryFindingReader()` is fed the qualifying
  findings from the rule engine run (`delta IN ('new',
  'newly_failing')`).
- `refactor.NewInMemoryHotSpotWriter()` collects the emitted
  `HotSpot` rows.
- A `cliPolicyReader` wraps the synthesised `PolicyVersion`
  and returns a `refactor.PolicySnapshot{PolicyVersionID,
  Weights}` -- the type already satisfies the
  `refactor.PolicyReader` contract on
  `services/clean-code/internal/refactor/planner.go:39`.

Then the TaskPlanner is wired with:

- `refactor.NewInMemoryHotSpotReader()` populated from the
  HotSpot batch the Planner just wrote.
- `refactor.NewInMemoryFindingDetailReader()` populated from
  the rule engine's `Finding` rows reduced to
  `(scope_id, rule_id)` pairs.
- `refactor.NewInMemoryRefactorPlanTaskWriter()` to capture the
  emitted `RefactorPlan` + `RefactorTask` rows.

**Effort estimator fallback (L9).** The Stage 8.3 ML model is
loaded via the `PolicyVersion.RefactorWeights.EffortModelVersion`
seam; the production TaskPlanner reads the ONNX bytes at the
path implied by that version. When the file is missing or the
loader returns an error, the CLI's effort estimator runs the
deterministic formula pinned in Section 1.3 row
`cli-effort-fallback-formula`. The fallback is wired by
implementing the same effort-callback seam the production
TaskPlanner accepts (`tech-spec.md` pins the exact callback
shape); the in-memory writer then receives `RefactorTask`
rows with non-zero `EffortHours`. The fallback logs a single
WARNING line on the first invocation per run and exposes the
formula in `--diagnostics` so the report consumer knows the
estimates are heuristic.

### 3.6 L6 -- CLI composition root (NEW)

**Disposition:** missing. **Package:** `cmd/cleanc` (new, see
open question `cli-binary-location`) plus
`internal/cli/orchestrator` (new). **Responsibility:** parse
flags, wire every component above into one pipeline, drive the
pipeline, and dispatch the output writers.

The binary owns sub-commands:

- `cleanc analyze <path> [flags]` -- the primary verb.
- `cleanc report <findings.json>` -- re-renders a previously
  emitted `findings.json` into a fresh markdown report
  without re-running the pipeline. Useful for CI artifacts.
- `cleanc version` -- prints binary version + active rule
  pack ids and versions + active parser fleet from
  `parser.DefaultRegistry().Languages()`.
- `cleanc apply <task_id> [flags]` -- reserved verb for P3
  mechanical patches; in P0/P1 the verb prints "not
  implemented; pending operator pin `cli-l7-authority`" and
  exits with code 64 (`EX_USAGE`).

Flags on `analyze`:

| Flag | Default | Purpose |
| --- | --- | --- |
| `--out <path>` | stdout | Where to write the markdown report |
| `--findings <path>` | `findings.json` | Where to write the machine-readable findings + plan + tasks JSON |
| `--emit-prompts <path>` | unset (skip) | Where to write the JSON-Lines `RefactorPromptRecord` set (L7 Option A; P1) |
| `--policy <path>` | embedded | Override the embedded rule packs with a path to a directory of YAML files (see Section 3.8) |
| `--with-churn` | false | Enable shallow git history walk for `modification_count_in_window` (currently dark; flag reserved for P2 wiring) |
| `--top-n <int>` | 0 (use policy default) | Override `RefactorWeights.TopN` for ranking |
| `--exit-on <severity>` | `block` | Exit non-zero when any finding's severity >= this value (`warn`, `block`, `info`) |
| `--diagnostics <path>` | unset | Emit a JSON file listing dark metrics, skipped files, and effort-estimator mode |
| `--dev-mode` | true on non-prod build, false on `-tags prod` build | Allow unsigned policy; required to use `--policy <path>` (Section 7.2) |

The orchestrator drives the pipeline in five strict stages:

1. **Walk** (Section 3.1) -> stream of `WalkedFile`.
2. **Parse + recipe** -- fan out to `parser.DefaultRegistry()`
   and the recipe set; collect `MetricSampleDraft` rows.
3. **Load policy** (Section 3.8) -> `steward.PolicyVersion` +
   `steward.Rule` slice.
4. **Evaluate** -- pre-load the `InMemoryStore`, call
   `Engine.RunBatch`.
5. **Plan + tasks** -- `Planner.Plan` then `TaskPlanner.Plan`;
   collect `PlanAndTasksResult`.

Stage outputs flow through Section 4's in-memory data model;
the writers (Section 3.7) read the assembled artifacts and emit
the requested output formats.

### 3.7 L7 -- Report and Suggestion writers (NEW)

**Disposition:** the structured-prompt half of L7 ships in P1;
mechanical patches are deferred to P3 pending the operator
answer on `cli-l7-authority`. **Packages:**
`internal/cli/report` (new) and `internal/cli/suggest` (new).

#### 3.7.1 Markdown report (`internal/cli/report`)

Renders a `RunArtifact` (Section 4.7) into a markdown file with
sections:

1. **Header** -- repo path, head SHA, policy id + version,
   active parser fleet, dark-metric summary.
2. **Verdict** -- the engine's
   `EvaluationVerdict.Verdict` (`pass`, `warn`, `block`).
3. **Findings by severity** -- group `Finding` rows by
   `Severity`; per finding show
   `(scope_signature, rule_id, metric_kind, value, threshold,
   rule's description_md "Suggested refactor" excerpt)`.
4. **Hot-spot ranking** -- the top-N `HotSpot` rows in score
   order with `(scope_signature, score, breakdown z-scores,
   finding_count)`.
5. **Refactor plan** -- the `RefactorPlan.SummaryMD` and the
   `RefactorTask` list grouped by `TaskKind`, each row showing
   `(scope_signature, kind, effort_hours, rule_id,
   description_md)`.
6. **Diagnostics** -- skipped files, dark metrics, effort
   estimator mode (ML vs fallback formula).

#### 3.7.2 JSON artefact (`internal/cli/report`)

Renders the full `RunArtifact` (Section 4.7) as a single JSON
document for downstream consumers (CI, dashboards). The schema
mirrors the in-memory struct one-for-one.

#### 3.7.3 Structured prompt records (`internal/cli/suggest`)

The L7 Option A emitter. For each `RefactorTask` in the
`PlanAndTasksResult`, the emitter assembles a
`RefactorPromptRecord` (Section 4.6) and writes one JSON record
per line to `--emit-prompts`. The record carries enough context
for an AI coder pasted into Copilot / Claude to synthesise a
patch:

- `task_id`, `rule_id`, `task_kind` (canonical enum).
- `scope.signature`, `scope.kind`, `scope.file_path`,
  `scope.start_line`, `scope.end_line` resolved by joining
  `RefactorTask.ScopeID` against the in-memory
  `ScopeBinding` table (Section 4.3) the walker / parser
  populated.
- `source_snippet` -- the raw bytes between the scope's start
  and end line, capped at a configurable max (default 200
  lines) so very large scopes do not blow up the prompt.
- `metric_evidence` -- the originating samples
  (`metric_kind`, `value`, `threshold`) so the prompt explains
  why the task fired.
- `prose_suggestion` -- the `Rule.DescriptionMD` text (the
  "Suggested refactor:" prose already present in the rule pack
  YAML; e.g. `solid/srp.yaml:88-93`).
- `effort_hours` and `effort_source` (`ml` or `fallback`).

**Why this stays downstream of the planner.** The story brief's
L7 deep-dive warns that authoring patches inside the
clean-code service collides with the
`docs/stories/code-intelligence-CLEAN-CODE/architecture.md:58-59`
"no auto-fix" out-of-scope clause. By emitting prompt records
strictly downstream of the existing `RefactorTask` rows, the
suggest package does NOT rewrite source bytes; it serialises
context for an external coder. This preserves the existing
architectural boundary and defers the amendment debate to
P3 / operator answer on `cli-l7-authority`.

### 3.8 L8 -- Dev-mode Policy Loader (NEW)

**Disposition:** friction; new package handles it. **Package:**
`internal/cli/devpolicy` (new). **Responsibility:** read YAML
rule packs (embedded by default, filesystem when
`--policy <path>` is passed) and synthesise an in-memory
`steward.PolicyVersion` for the rule engine.

Inputs:

- Embedded rule packs: `//go:embed
  ../../policy/rulepacks/solid/*.yaml
  ../../policy/rulepacks/decoupling/*.yaml` (or whatever
  relative path the operator pin `cli-policy-distribution`
  resolves to).
- `--policy <path>` override: a directory containing one or
  more `.yaml` files in the same shape as the embedded set.

Outputs (in-memory, not persisted):

- One `steward.RulePack` per loaded YAML file (PK
  `(pack_id, version)`).
- One `steward.Rule` per entry in the YAML's `rules:` array.
- One `steward.PolicyVersion` referencing every loaded rule
  (no `Threshold` rows -- the v1 rule packs use literal
  predicates per the story brief's
  "v1 deliberately uses literal predicates" note in
  `solid/srp.yaml`).
- `PolicyVersion.Signature` is the empty byte slice; the CLI
  attaches a `dev_unsigned` flag to its diagnostics so the
  rule engine wrapper knows to skip signature verification.

**Signature bypass.** Production `clean-code` requires
`PolicyVersion.Signature` to validate (CLEAN-CODE arch G5). The
CLI bypasses this check ONLY in `--dev-mode`. The bypass is a
single function call on a wrapper around `Engine` (the wrapper
overrides the signature check; in non-dev builds the wrapper
type does not exist, enforced by a `//go:build !prod` tag on
the file that declares it). The CLI prints a loud banner on
every run:

```
WARNING: dev-mode policy is unsigned. Do NOT use cleanc output
as the source of truth for a production gate.
```

The banner is structural (not a config knob) per CLEAN-CODE
arch G5's "kill switch" invariant: an operator who silences the
banner has to recompile, leaving a build-artifact audit trail.

### 3.9 L9 -- Effort estimator fallback (NEW)

**Disposition:** friction; new package handles it. **Package:**
`internal/cli/effort` (new). **Responsibility:** provide a
deterministic effort estimator when the ONNX model is missing
or fails to load.

Inputs (per task):

- `ScopeInputs.Loc`, `ScopeInputs.Cyclo`,
  `ScopeInputs.FanIn` -- pulled from the same in-memory
  `MetricSampleReader` rows the Planner used (the values are
  already on the `Breakdown` carried by `PlanResult`).
- `TaskKind` -- used as a multiplier table:
  - `split_class`: 1.5x base
  - `invert_dependency`: 1.3x base
  - `break_cycle`: 1.4x base
  - `extract_method`: 0.7x base
  - `consolidate_duplication`: 1.0x base

Output: `effort_hours float64` clamped to `[0.1, 80.0]` and
rounded to 1 decimal. The estimator is pure; tests pin
deterministic outputs against fixture scopes.

The estimator wires into the TaskPlanner's effort callback
seam; when the seam is unset (no ML model loaded) the
TaskPlanner currently emits `0.0` placeholders. Wiring the
fallback is the CLI's responsibility, not the production
service's -- the production service still defers to the ML
model and treats `0.0` as "unestimated" per the
`task_planner.go:38-43` doc comment.

---

## 4. Data model

The CLI assembles a strictly in-memory data model. No new
persistent tables are introduced; every entity below maps to
an existing in-memory shape from the reused packages OR is a
CLI-local helper struct that lives only inside one process.

### 4.1 `RepoContext` (CLI-local)

| Field | Type | Source | Notes |
| --- | --- | --- | --- |
| `RootPath` | `string` | CLI flag | Absolute, normalised to forward slash |
| `RepoID` | `uuid.UUID` | derived | UUID-v5(namespace=`cleanc.local-repo`, name=`RootPath`); deterministic so re-runs reuse the same id |
| `HeadSHA` | `string` | `git rev-parse HEAD`, else `"working-copy"` | Stamped on every `MetricSample`, `Finding`, `HotSpot`, `RefactorTask` |
| `ModulePath` | `string` | per-language detection (Go: `go.mod`, TS: `package.json`, Java: top-level package, Python: PEP 621 `name`) | Forwarded to the parser via `AttrModulePath` so `cycle_member` resolves intra-repo imports |
| `IsGitRepo` | `bool` | `os.Stat(RootPath + "/.git")` | Gates the `--with-churn` code path |

### 4.2 `WalkedFile` (CLI-local, walker -> orchestrator)

| Field | Type | Notes |
| --- | --- | --- |
| `RepoRelPath` | `string` | Forward-slash; used as `AstFile.path` |
| `AbsPath` | `string` | OS-native; used only for IO |
| `Language` | `string` | Canonical (`go`, `python`, `typescript`, `java`) |
| `SizeBytes` | `int64` | For diagnostics |
| `Content` | `[]byte` | Read once by the walker; handed downstream by reference |

### 4.3 `ScopeBinding` (in-memory mirror of `clean_code.scope_binding`)

The CLI builds a process-local map keyed by `ScopeID` so the
suggest writer (Section 3.7.3) can resolve `(file_path,
start_line, end_line)` back from any `RefactorTask.ScopeID`.
The fields mirror CLEAN-CODE arch Section 5.2.3:

| Field | Type | Notes |
| --- | --- | --- |
| `ScopeID` | `uuid.UUID` | Per CLEAN-CODE arch G2 minted from `(repo_id, scope_kind, canonical_signature, first_seen_sha)` |
| `ScopeKind` | `string` | `method`, `class`, `interface`, `file`, `package`, `repo` |
| `Signature` | `string` | Human-readable canonical signature (e.g. `services/clean-code/internal/foo/bar.go::Bar.Compute`) |
| `FilePath` | `string` | Repo-relative |
| `StartLine`, `EndLine` | `int` | 1-indexed |
| `Language` | `string` | Echoes the parent file's language |

### 4.4 `Sample` (reused from `rule_engine`)

`rule_engine.Sample` (see
`services/clean-code/internal/rule_engine/types.go:143`) embeds
`dsl.Sample` and adds `ScopeSignature`. The CLI builds one
`Sample` per recipe draft:

- `dsl.Sample.RepoID`, `dsl.Sample.SHA` from `RepoContext`.
- `dsl.Sample.ScopeID`, `dsl.Sample.ScopeKind` from the recipe
  draft's scope reference.
- `dsl.Sample.MetricKind`, `dsl.Sample.Value` from the draft.
- `dsl.Sample.MetricVersion = 1` (CLI is the first writer; no
  re-computation history).
- `dsl.Sample.Pack`, `dsl.Sample.Source` from the draft's
  `Pack` / `SourceComputed`.
- `Sample.ScopeSignature` from the matching `ScopeBinding`.

### 4.5 `PolicyVersionInMemory` (mirrors `steward.PolicyVersion`)

The CLI's `devpolicy` package returns the canonical
`steward.PolicyVersion` shape (see
`services/clean-code/internal/policy/steward/types.go:112`):

| Field | CLI value | Notes |
| --- | --- | --- |
| `PolicyVersionID` | UUID-v5 of `(rule pack hash || effort_model_version)` | Stable per `(loaded packs, effort model)` so re-runs yield the same id |
| `Name` | `cleanc-dev-policy` | Identifies dev-mode in audit |
| `RuleRefs` | one ref per loaded rule | `(rule_id, version)` pairs |
| `ThresholdRefs` | empty | v1 rule packs use literal predicates |
| `RefactorWeights.TopN` | from `--top-n` flag, else 20 | Caps the plan and prompt set |
| `RefactorWeights.EffortModelVersion` | `"fallback-2026.05"` | Triggers the CLI effort fallback (Section 3.9) |
| `Signature` | empty `[]byte` | Bypassed only in `--dev-mode` (Section 7.2) |
| `CreatedAt` | `time.Now().UTC()` | Run timestamp |

### 4.6 `RefactorPromptRecord` (CLI-local, the L7 Option A payload)

The structured prompt record emitted by `internal/cli/suggest`,
one JSON object per line of `--emit-prompts`. This is the
operator's primary new artifact.

| Field | Type | Source |
| --- | --- | --- |
| `task_id` | `string` (UUID) | `RefactorTask.TaskID` |
| `plan_id` | `string` (UUID) | `RefactorTask.PlanID` |
| `repo_id` | `string` (UUID) | `RepoContext.RepoID` |
| `head_sha` | `string` | `RepoContext.HeadSHA` |
| `policy_version_id` | `string` (UUID) | `PolicyVersionInMemory.PolicyVersionID` |
| `task_kind` | `string` (canonical enum, Section 5.2) | `RefactorTask.Kind` |
| `rule_id` | `string` | `RefactorTask.RuleID` |
| `rule_version` | `int` | resolved from the loaded `steward.Rule` row |
| `severity` | `string` | rule's `SeverityDefault` |
| `scope.signature` | `string` | `ScopeBinding.Signature` |
| `scope.kind` | `string` | `ScopeBinding.ScopeKind` |
| `scope.file_path` | `string` | `ScopeBinding.FilePath` |
| `scope.start_line`, `scope.end_line` | `int` | `ScopeBinding.StartLine`/`EndLine` |
| `source_snippet` | `string` | raw bytes between start/end line, capped |
| `source_snippet_truncated` | `bool` | true when cap fired |
| `metric_evidence` | `[]MetricEvidence` | per supporting sample: `{metric_kind, value, threshold, op}` |
| `prose_suggestion` | `string` | `Rule.DescriptionMD` |
| `effort_hours` | `float64` | `RefactorTask.EffortHours` |
| `effort_source` | `string` (`ml` or `fallback`) | from the effort estimator |
| `prompt_format_version` | `string` | `"v1.2026.05"` -- bumped when this shape changes |

### 4.7 `RunArtifact` (CLI-local, the full output shape)

The composition root assembles a `RunArtifact` that the report
writers consume. It is the JSON shape `--findings <path>`
emits.

| Field | Type | Notes |
| --- | --- | --- |
| `Context` | `RepoContext` | Section 4.1 |
| `Policy` | `PolicyVersionInMemory` | Section 4.5 |
| `Files` | `[]WalkedFileSummary` | one per file -- `path`, `language`, `size_bytes`, `parse_status` |
| `Skips` | `[]WalkSkip` | walker / parser skip list |
| `DarkMetrics` | `[]DarkMetric` | per-language dark metric inventory |
| `Samples` | `[]Sample` | Section 4.4 |
| `Run` | `rule_engine.EvaluationRun` | the engine row |
| `Verdict` | `rule_engine.EvaluationVerdict` | the engine row |
| `Findings` | `[]rule_engine.Finding` | the engine rows |
| `HotSpots` | `[]refactor.HotSpot` | from the Planner |
| `Plan` | `refactor.RefactorPlan` | from the TaskPlanner |
| `Tasks` | `[]refactor.RefactorTask` | from the TaskPlanner |
| `Diagnostics` | `Diagnostics` | dark metrics, effort source, skip counts |

### 4.8 `RefactorTask.Kind` -- canonical enum

The CLI honours the canonical five-value enum pinned in
`services/clean-code/internal/refactor/task_planner.go:77-118`:

| Kind | Default rule families |
| --- | --- |
| `split_class` | `solid.srp.*`, `solid.isp.*` |
| `extract_method` | `solid.ocp.*`, `solid.lsp.*` (default fallback) |
| `invert_dependency` | `solid.dip.*`, `decoupling.coupling.*`, `decoupling.cbo*`, `decoupling.fan_in*`, `decoupling.fan_out*` |
| `break_cycle` | `decoupling.cycle_member`, `decoupling.cycles*` |
| `consolidate_duplication` | `decoupling.duplication*` |

The CLI MUST refuse any non-canonical task kind on the
emission path (matches the `ValidateTaskKind` /
`ErrRejectedTaskKindAlias` guard in `task_planner.go:181-191`).

---

## 5. Interfaces between components

The CLI introduces no new domain interfaces. Every cross-package
binding maps onto an existing contract; this section enumerates
those bindings so a reviewer can confirm the wiring without
reading the source tree.

### 5.1 Walker -> Orchestrator

```go
// internal/cli/walk
type Walker interface {
    // Walk drives the recursive traversal. The returned
    // channel is closed when the walk completes; errors
    // arrive on the err channel.
    Walk(ctx context.Context, root string) (<-chan WalkedFile, <-chan WalkSkip, <-chan error)
}
```

The orchestrator owns goroutine count; the walker does not
spawn parser goroutines.

### 5.2 Orchestrator -> Parser registry (reused)

```go
// services/clean-code/internal/ast/parser
ast, err := parser.DefaultRegistry().Parse(ctx, repoRelPath, content)
```

No new interface. The orchestrator passes the resulting
`*parser.AstFile` to the recipe set.

### 5.3 Orchestrator -> Recipes (reused)

The recipe set is iterated through the existing project
registry (`recipes.DefaultProjectRegistry()`). The orchestrator
calls `recipes.Recipe.AppliesTo(file)` then
`recipes.Recipe.Apply(...)` per recipe per file (and once per
project-scoped recipe). The dark-metric diagnostic is
collected by inspecting `Recipe.RequiredAttrs()` on a
zero-draft return (the inspector helper lives in the
orchestrator, NOT inside the recipes package, so the recipe
contract is not perturbed).

### 5.4 Orchestrator -> Rule engine (reused)

```go
// services/clean-code/internal/rule_engine
store := rule_engine.NewInMemoryStore()
store.InsertPolicyVersion(pv)
for _, r := range rules { store.InsertRule(r) }
for _, s := range samples { store.InsertSample(s) }
store.SetCommitParent(repoID, headSHA, "")

engine := rule_engine.NewEngine(store, /* dsl resolver, clock */)
result, err := engine.RunBatch(ctx, repoID, headSHA, pv.PolicyVersionID)
```

The CLI does not subclass `Engine` or `Store`; it wraps
`Engine.RunBatch` with a thin signature-bypass shim when
`--dev-mode` is on (Section 3.8).

### 5.5 Orchestrator -> Planner (reused)

```go
// services/clean-code/internal/refactor
policyReader := cliPolicyReader{pv: pv}             // PolicyReader
metricReader := refactor.NewInMemoryMetricSampleReader()
findingReader := refactor.NewInMemoryFindingReader()
hotSpotWriter := refactor.NewInMemoryHotSpotWriter()

planner, err := refactor.NewPlanner(
    &policyReader, metricReader, findingReader, hotSpotWriter,
)
planResult, err := planner.Plan(ctx, repoID, headSHA)
```

`cliPolicyReader` is a 5-line struct that satisfies
`refactor.PolicyReader` (`services/clean-code/internal/refactor/planner.go:39`)
by returning `(PolicySnapshot{...}, true, nil)`.

### 5.6 Orchestrator -> TaskPlanner (reused)

```go
hotSpotReader := refactor.NewInMemoryHotSpotReader()
findingDetailReader := refactor.NewInMemoryFindingDetailReader()
planTaskWriter := refactor.NewInMemoryRefactorPlanTaskWriter()

taskPlanner, err := refactor.NewTaskPlanner(
    &policyReader, hotSpotReader, findingDetailReader,
    planTaskWriter,
    refactor.WithEffortFunc(cliEffortCallback),
)
planAndTasks, err := taskPlanner.Plan(ctx, repoID, headSHA)
```

The `refactor.WithEffortFunc` option seam is the one the
`internal/cli/effort` package plugs into (Section 3.9). The
exact option-name is pinned in `tech-spec.md`; if the existing
`task_planner.go` does not yet expose this option the
implementation plan calls out adding it as a Stage-0 prereq
inside `services/clean-code/internal/refactor`.

### 5.7 Orchestrator -> Report writers (NEW, internal-only)

```go
// internal/cli/report
type Renderer interface {
    Render(ctx context.Context, art RunArtifact, w io.Writer) error
}

type Markdown struct{ ... }   // satisfies Renderer
type JSON     struct{ ... }   // satisfies Renderer
```

```go
// internal/cli/suggest
type PromptEmitter interface {
    Emit(ctx context.Context, art RunArtifact, w io.Writer) error
}
```

Both interfaces are CLI-internal; nothing outside `cmd/cleanc`
imports them.

### 5.8 DevPolicy -> Steward shapes (reused)

```go
// internal/cli/devpolicy
type Loader interface {
    Load(ctx context.Context, source LoaderSource) (Bundle, error)
}

type LoaderSource struct {
    UseEmbedded bool
    DirPath     string // honoured when UseEmbedded == false
}

type Bundle struct {
    PolicyVersion steward.PolicyVersion
    Rules         []steward.Rule
    RulePacks     []steward.RulePack
}
```

The loader produces canonical `steward.*` rows; the rule
engine wrapper accepts them unchanged.

---

## 6. End-to-end sequence flows

This section documents the three primary scenarios from
Section 1.5. Each diagram is ASCII; box-drawing characters are
NOT used because they corrupt under Windows codepage round-trips.

### 6.1 P0: `cleanc analyze <path> --out report.md`

```
+--------+    +-----------+    +--------+    +--------+    +--------+    +--------+    +---------+    +--------+
| User   |    | cmd/cleanc|    | walk   |    | parser |    | recipes|    |rule_eng|    |refactor |    | report |
+---+----+    +-----+-----+    +---+----+    +---+----+    +---+----+    +---+----+    +----+----+    +---+----+
    |              |              |              |              |              |              |              |
    | analyze path |              |              |              |              |              |              |
    |------------->|              |              |              |              |              |              |
    |              | flags + ctx  |              |              |              |              |              |
    |              |--- parse --->|              |              |              |              |              |
    |              |              |              |              |              |              |              |
    |              |     load embedded rule packs   (Section 3.8, internal/cli/devpolicy)                    |
    |              |              |              |              |              |              |              |
    |              | Walk(root)   |              |              |              |              |              |
    |              |------------->|              |              |              |              |              |
    |              |              | WalkedFile   |              |              |              |              |
    |              |              |  (stream)    |              |              |              |              |
    |              |              |------------->|              |              |              |              |
    |              |              |              | DefaultRegistry().Parse(ctx,path,content)  |              |
    |              |              |              |------------->|              |              |              |
    |              |              |              |              | per-recipe   |              |              |
    |              |              |              |              | AppliesTo /  |              |              |
    |              |              |              |              | Apply -> drafts             |              |
    |              |              |              |              |------------->|              |              |
    |              |              |              |              |              | InsertSample |              |
    |              |              |              |              |              | (per draft)  |              |
    |              |              |              |              |              |              |              |
    |              | RunBatch(repoID, headSHA, pv.PolicyVersionID)                                          |
    |              |--------------------------------------------------------> | EvaluationRun,              |
    |              |                                                          | Verdict, Findings           |
    |              |                                                          |------------->|              |
    |              |                                                          |              |              |
    |              | Planner.Plan(repoID, headSHA)                            |              |              |
    |              |-----------------------------------------------------------------------> | HotSpot      |
    |              |                                                                         | batch        |
    |              |                                                                         |              |
    |              | TaskPlanner.Plan(repoID, headSHA)                                       |              |
    |              |-----------------------------------------------------------------------> | Plan +       |
    |              |                                                                         | Tasks        |
    |              |                                                                         |              |
    |              | assemble RunArtifact (Section 4.7); pass to report.Markdown / report.JSON              |
    |              |---------------------------------------------------------------------------------------->|
    |              |                                                                                        |
    |              |                                          report.md (stdout or --out)  +  findings.json |
    |<-------------|                                                                                        |
    |  exit code   |                                                                                        |
```

Notes on the flow:

- The walker, parser, and recipes run in a single fan-out
  worker pool (size = `GOMAXPROCS`); the diagram collapses the
  fan-out to a single column for readability.
- The orchestrator collects all `MetricSampleDraft` rows
  before pre-loading the `InMemoryStore`; the engine is called
  exactly once via `RunBatch`.
- A non-zero `--exit-on` severity match makes the binary exit
  with code 1 after the report is written; the report itself
  always ships, so CI can attach it as an artifact even on
  failure.

### 6.2 P1: `cleanc analyze <path> --emit-prompts prompts.jsonl`

P1 is P0 plus the suggest emitter; only the additional steps
are diagrammed.

```
... (same flow as 6.1 up through TaskPlanner.Plan) ...
              |
              | for each RefactorTask in PlanAndTasksResult.Tasks:
              |   resolve ScopeBinding[task.ScopeID]
              |   read source bytes between StartLine..EndLine (capped)
              |   join: rule, metric evidence, effort source
              |   assemble RefactorPromptRecord (Section 4.6)
              |
              | suggest.Emit(ctx, art, promptsFile)
              |---------------------------------------------> +---------------+
              |                                               | suggest       |
              |                                               | emits JSONL   |
              |                                               +-------+-------+
              |                                                       |
              |                                                       v
              |                                               +---------------+
              |                                               | prompts.jsonl |
              |                                               +---------------+
```

Source snippet extraction is deterministic: the bytes between
the scope's start and end line are read fresh from the
filesystem (NOT from the parser's in-memory representation)
to preserve the original whitespace and comments. The cap
(default 200 lines) is enforced in the emitter; truncated
snippets carry `source_snippet_truncated: true`.

### 6.3 P3 (future): `cleanc analyze --apply <task_id> --dry-run`

P3 is documented here for completeness; the operator pin
`cli-l7-authority` defers actual implementation until after an
architecture amendment. The sequence:

```
+--------+    +-----------+    +-----------------+    +------------+    +--------+
| User   |    | cmd/cleanc|    | apply subcommand|    | transformer|    | stdout |
+---+----+    +-----+-----+    +--------+--------+    +-----+------+    +---+----+
    |              |                    |                   |               |
    | apply taskID |                    |                   |               |
    |------------->|                    |                   |               |
    |              | re-run P0 pipeline | (RunArtifact)     |               |
    |              |---------------------------->           |               |
    |              | lookup task by ID  |                   |               |
    |              |------------------->|                   |               |
    |              |                    | dispatch on       |               |
    |              |                    | task.Kind:        |               |
    |              |                    |  - break_cycle    |               |
    |              |                    |  - consolidate_   |               |
    |              |                    |    duplication    |               |
    |              |                    |--- ASTrewrite --->|               |
    |              |                    |                   | unified diff  |
    |              |                    |<------------------|               |
    |              | print diff (--dry-run) OR `git apply` (later)           |
    |              |----------------------------------------------->        |
    |<-------------|                                                        |
    |  exit code   |                                                        |
```

Per the open question `cli-l7-authority`, P3 lands ONLY after
either (a) the CLEAN-CODE arch Section 1.2 "no auto-fix"
clause is amended OR (b) the mechanical transformer is housed
in a sibling package outside the planner so the planner's
contract is unchanged. The architecture document for P3 will
be a new story; this doc only reserves the sub-command name
and exit code so the eventual addition is non-breaking.

---

## 7. Operating modes

### 7.1 Embedded vs filesystem rule packs

The CLI build embeds the canonical
`services/clean-code/policy/rulepacks/{solid,decoupling}/*.yaml`
set via `//go:embed`. The embedded set is the default; an
operator can override with `--policy <path>` where `<path>` is
a directory of YAML files in the same shape. The embedded
build guarantees the CLI works offline; the override exists
so an operator can try a new pack without rebuilding the
binary. The operator answer to `cli-policy-distribution`
finalises whether `--policy <path>` is allowed at all in
production builds (it is allowed unconditionally in dev
builds).

### 7.2 Dev-mode vs production builds

Two build tag domains govern the CLI:

| Build tag | Behaviour |
| --- | --- |
| (no tag) | `--dev-mode` defaults to `true`; signature bypass is available; `--policy <path>` is permitted |
| `-tags prod` | `--dev-mode` defaults to `false`; the signature-bypass file is excluded by `//go:build !prod`; `--policy <path>` is permitted only when `--dev-mode=true` (which fails fast since the bypass type is undefined in a `prod` build) |

The matrix exists so an operator who ships the CLI into a CI
container can build with `-tags prod` and be guaranteed that
no signed-policy bypass is reachable, while a developer on a
laptop sees the convenient default.

### 7.3 Effort estimator mode

Two modes:

- **ML mode.** `PolicyVersion.RefactorWeights.EffortModelVersion`
  resolves to an existing ONNX file (the production deployment
  path). The CLI's effort callback delegates to the existing
  loader; on success every `RefactorTask.EffortHours` carries
  an ML estimate.
- **Fallback mode.** The loader returns `os.ErrNotExist` OR a
  load error. The CLI's effort callback runs the deterministic
  formula in Section 1.3 row `cli-effort-fallback-formula`.
  Diagnostics emit `effort_source: fallback` on every prompt
  record.

The mode is decided once per run; mixed-mode output is
disallowed so the report's effort column has a single
interpretation.

---

## 8. Open questions

The story description's four open questions are re-emitted as
structured records below for the wizard. Section 1.3 has also
proposed defaults that the operator can confirm or override.

(See the `open-questions` JSON block immediately after the
Iteration Summary.)

---

## 9. Phased roadmap mapping

This section restates the story brief's P0 - P3 roadmap and
ties each phase to the Sections above so the sibling
`implementation-plan.md` can reuse the same anchors.

| Phase | Scope | This doc's anchors |
| --- | --- | --- |
| **P0** | CLI shell. L1 + L6 wiring + L4 + L5 plumbing; only the metrics that light up today (`loc`, `duplication_ratio`, `cycle_member`, `interface_width`, `depth_of_inheritance`) | Sections 3.1 - 3.6, 3.7.1 - 3.7.2, 3.8, 3.9; Section 6.1 |
| **P1** | Structured edit instructions (L7 Option A); the `--emit-prompts` flag and the `RefactorPromptRecord` JSONL emitter | Sections 3.7.3, 4.6; Section 6.2 |
| **P2** | Parser-attr expansion (L3): add `decision_blocks`, `call_edges`, `field_accesses` for Go first, then Python, TS, Java (per operator pin `cli-language-priority`) | Section 3.3 dark-metric table; outside this doc's primary scope -- ships under a separate story |
| **P3** | Mechanical patches (L7 Options B/C) for the 2-3 highest-value task kinds; gated by `cli-l7-authority` answer | Section 6.3; new story |

---

## 10. Cross-cutting concerns

- **Stable identity (G2).** The CLI mints `repo_id` per
  Section 1.4 and `scope_id` per CLEAN-CODE arch G2. Re-runs
  on the same path with the same SHA produce identical
  `MetricSample`, `Finding`, `HotSpot`, and `RefactorTask`
  UUIDs so a CI consumer can diff two runs without spurious
  noise.
- **No persistence by default.** The CLI exits without writing
  anything outside the user-named output files. There is no
  embedded SQLite, no cache directory, no temp files retained
  on success.
- **Determinism.** Walker ordering is sorted lexicographically;
  recipe iteration is sorted by metric_kind; planner output
  uses the existing `(Score DESC, ScopeID ASC)` sort. The
  composition root pins a fixed `time.Now()` reading per run
  so all `created_at` timestamps inside one `RunArtifact`
  match.
- **Concurrency.** Walker, parser, and recipes run on a worker
  pool sized to `GOMAXPROCS`; the rule engine, planner, and
  task planner run on the calling goroutine after every sample
  is collected. The in-memory stores are all mutex-guarded
  per the existing `internal/rule_engine/inmem_store.go:30`
  and `internal/refactor/planner.go:524` doc-comments.
- **Exit codes.** `0` = clean run, no `--exit-on` trigger.
  `1` = clean run, a finding crossed the `--exit-on` severity.
  `2` = walker error (path missing, permission denied).
  `64` = invalid CLI usage (`EX_USAGE`).
  `70` = internal engine error (`EX_SOFTWARE`).
- **Telemetry.** The CLI does not emit OTel by default (no
  collector on a developer laptop). A `--telemetry-otlp <url>`
  flag is reserved but unimplemented in P0/P1; the operator
  answer on a future pin will decide whether the CLI reuses
  the `clean-code` service's OTel wiring or a stripped-down
  variant.

---

## 11. References

- `services/clean-code/internal/ast/parser/parser.go:8-63` --
  `Parser` interface and `ParserVersion` constant the CLI
  inherits.
- `services/clean-code/internal/ast/parser/registry.go:93-107`
  -- `Registry.Parse` and `DetectLanguage` used by the CLI
  composition root.
- `services/clean-code/internal/metrics/recipes/recipe.go:55-160`
  -- the parser-attr gating constants
  (`AttrDecisionBlocks`, `AttrCallEdges`,
  `AttrFieldAccesses`) that define the dark-metric set.
- `services/clean-code/internal/rule_engine/inmem_store.go:16-29`
  -- `InMemoryStore` documented as the scaffold-mode store
  the CLI pre-loads.
- `services/clean-code/internal/rule_engine/types.go:143-198`
  -- `Sample` and `Finding` row shapes the CLI assembles and
  reads back.
- `services/clean-code/internal/refactor/planner.go:39-198,
  498-545` -- `PolicyReader` / `MetricSampleReader` /
  `FindingReader` / `HotSpotWriter` contracts; `PlanResult`
  shape; in-memory readers and writers the CLI wires.
- `services/clean-code/internal/refactor/task_planner.go:77-118,
  197-260, 281-460` -- canonical `TaskKind` enum, default
  rule->kind mapping, `RefactorPlan` / `RefactorTask` /
  `PlanAndTasksResult` row shapes, and the in-memory readers
  and writers the CLI wires.
- `services/clean-code/internal/policy/steward/types.go:112-200`
  -- `PolicyVersion`, `RulePack`, `Rule`, `Threshold` shapes
  the CLI's `devpolicy` loader synthesises.
- `services/clean-code/policy/rulepacks/{solid,decoupling}/*.yaml`
  -- the eight rule pack YAML files the CLI loads (embedded
  by default).
- `docs/stories/code-intelligence-CLEAN-CODE/architecture.md`
  -- the base service architecture and the source of truth
  for G1 - G7 invariants and the metric catalogue.

---

## Iteration Summary

- **Path written:** `docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md`
- **Coverage of the story description's brief:**
  - L1 walker -> Section 3.1
  - L2 parser reuse -> Section 3.2
  - L3 recipe reuse + dark-metric inventory -> Section 3.3
  - L4 rule engine reuse -> Section 3.4
  - L5 planner + task planner reuse + effort fallback wiring -> Section 3.5, 3.9
  - L6 CLI composition root -> Section 3.6
  - L7 structured-prompt emitter (Option A) -> Section 3.7, 4.6, 6.2
  - L8 dev-mode policy loader + signing bypass -> Section 3.8, 7.2
  - L9 effort model fallback -> Section 3.9, 7.3
  - Data model (entities + key fields) -> Section 4
  - Interfaces between components -> Section 5 (every binding mapped to an existing Go contract; no new domain interfaces invented)
  - End-to-end sequence flows for the primary scenarios -> Section 6 (P0 analyze, P1 prompt emission, P3 future apply)
  - P0 - P3 phased roadmap mapping -> Section 9
- **Iter 1 -- no prior evaluator feedback to reconcile.** When iter 2 arrives, the "Prior feedback resolution" subsection will appear here with one `[x]` per checkbox.
- **Sibling-doc anchors:** Section numbers above are intentionally stable so the parallel `tech-spec.md`, `implementation-plan.md`, and `e2e-scenarios.md` can cite "REFACTOR-GUIDE arch Section 3.X" without text-search drift.
- **Open questions for next iteration:**
  - 4 operator questions (the four pinned in the story brief) are emitted in the JSON block below.
  - Two CLI-shape pins are also surfaced (`cli-binary-location` and `cli-policy-distribution`) because both materially change Section 3.6 and Section 3.8 if the operator overrides them.

```json open-questions
{ "openQuestions": [
    { "id": "L7-authority",
      "text": "Will the team accept lifting the CLEAN-CODE arch Section 1.2 'no auto-fix' clause so the CLI can ship mechanical patches (Options B / C) in P3, or should the CLI strictly stay on structured-prompt Option A even in later phases?",
      "type": "choice",
      "choices": [
        "A-only: stay on structured prompts only; never ship mechanical patches inside the CLI",
        "amend: lift the no-auto-fix clause and ship mechanical patches in P3 inside services/clean-code",
        "sibling: keep the no-auto-fix clause but house mechanical patches in a sibling package outside services/clean-code"
      ]
    },
    { "id": "language-priority",
      "text": "Which target language gets the P2 parser-attr extension (decision_blocks / call_edges / field_accesses) first?",
      "type": "choice",
      "choices": ["Go", "Python", "TypeScript", "Java"]
    },
    { "id": "cli-binary-location",
      "text": "Where should the cleanc binary live in the deployed surface?",
      "type": "choice",
      "choices": [
        "sibling: services/clean-code/cmd/cleanc/ alongside the six existing service binaries",
        "separate-repo: a new repo that depends on services/clean-code as a Go module",
        "library: services/clean-code exports a pkg/ library and an external CLI repo wraps it"
      ]
    },
    { "id": "cli-policy-distribution",
      "text": "How are the SOLID / decoupling rule packs distributed with the CLI binary?",
      "type": "choice",
      "choices": [
        "embedded: //go:embed the YAML into the binary; --policy <path> is an override only",
        "external-required: the CLI refuses to run without --policy <path> pointing at a checked-out tree",
        "hybrid: embedded defaults plus a versioned download URL pinned in CLI config"
      ]
    }
] }
```

DONE
