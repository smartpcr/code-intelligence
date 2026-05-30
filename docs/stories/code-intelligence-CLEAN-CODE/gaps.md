# Gap Analysis: `cleanc analyze <repo-path>` CLI

> Generated: 2026-05-29
> Goal: a single-binary CLI that takes a local repo path, scans for policy
> violations, and produces refactor suggestions as actionable code changes —
> with NO PostgreSQL, NO HTTP gateway, NO docker stack.
> Companion docs: [`architecture.md`](architecture.md), [`tech-spec.md`](tech-spec.md),
> [`implementation-plan.md`](implementation-plan.md), [`e2e-scenarios.md`](e2e-scenarios.md).
> Authority: per the repo's `README.md`, when docs and code disagree the docs
> win. This file is **analysis, not contract** — it does NOT redefine the
> service's architecture; it surveys what's already shipped vs. what a
> developer-laptop CLI needs to bridge.

## Summary

| Layer                                | Status                                    | Notes                                                                                                                                                  |
| ------------------------------------ | ----------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------ |
| L1 Filesystem walker                 | **missing**                               | `clean-code-indexer` is a `/healthz`-only stub (`services/clean-code/cmd/clean-code-indexer/main.go:12-30`)                                            |
| L2 AST parse + language sniff        | **reusable as-is**                        | Pure `Parse(ctx, path, content)` + `Registry.Parse` with `DetectLanguage` (`services/clean-code/internal/ast/parser/registry.go:93-98`)                |
| L3 Metric recipes                    | **reusable but parser-gap-limited**       | Pure functions; ~half the catalogue won't light up today (see L3 details)                                                                              |
| L4 Rule engine + DSL                 | **reusable as-is**                        | `InMemoryStore` exists explicitly for "scaffold-mode when `CLEAN_CODE_PG_URL` is unset" (`services/clean-code/internal/rule_engine/inmem_store.go:16-29`) |
| L5 Refactor planner                  | **reusable as-is**                        | `InMemoryMetricSampleReader` exists for the same reason (`services/clean-code/internal/refactor/planner.go:510-538`); `PlanResult` is a serializable Go struct |
| L6 CLI composition root              | **missing**                               | None exists; every `cmd/clean-code-*` main is server-shaped                                                                                            |
| L7 Code-change suggestions as patches | **architectural blocker** (the main ask) | Auto-fix is **out of scope by design** (`docs/stories/code-intelligence-CLEAN-CODE/architecture.md` §1.2 line 59) — only "Suggested refactor:" prose in rule pack YAML today |
| L8 Policy signing                    | **friction**                              | `policy-signing-required = v1 required` (architecture §1.6) — CLI either bypasses or embeds a dev key                                                  |
| L9 Effort model ONNX                 | **friction**                              | Refactor planner expects `effort_model.onnx`; placeholder shipped but loader path needs a dev fallback                                                 |

---

## L1 — Filesystem walker (*missing*)

Nothing in the tree walks a directory and feeds the parser.
`services/clean-code/cmd/clean-code-indexer/main.go:12-30` is `/healthz` only.
The metric-ingestor receives single `(path, bytes)` payloads over HTTP, so
there is no walker on either side of that wire today.

**Need to build:**

- Recursive walker honoring `.gitignore` / `.git/` / `node_modules` /
  `vendor/` / `target/` / language-specific generated dirs.
- Per-file size cap (the architecture pins parser-side caps; reuse the same
  constants).
- Optional shallow git inspection to grab the current `HEAD` SHA and history
  for `modification_count_in_window`.

**Estimated effort:** half-day (Go `filepath.WalkDir` + a couple of skip lists;
`go-gitignore` lib if strict semantics are required).

---

## L2 — AST parsing (*reusable as-is*)

- `services/clean-code/internal/ast/parser/parser.go:35-63` — pure
  `Parse(ctx, path, content) (*AstFile, error)`, no DB.
- `services/clean-code/internal/ast/parser/registry.go:30-78` —
  `Registry.For(language)` and `Registry.Parse(path, content)` with
  `DetectLanguage` by file extension.
- `DefaultRegistry()` pre-populates **Go / Python / TypeScript / Java**
  (`services/clean-code/internal/ast/parser/parser.go:26`). Other languages
  return `ErrUnsupportedLanguage`.
- CGO present → real tree-sitter; non-CGO → scanner fallback
  (per `services/clean-code/internal/ast/parser/parsers_cgo.go` /
  `services/clean-code/internal/ast/parser/parsers_nocgo.go`).

**Estimated effort:** zero for the four languages above.

---

## L3 — Metric recipes (*reusable but parser-gap-limited*)

`services/clean-code/internal/metrics/recipes/recipe.go` plus the per-metric
files (`cyclo.go`, `lcom4.go`, `fan_in.go`, …) are pure functions over
`*AstFile`. No DB.

**However** — `services/clean-code/internal/metrics/recipes/recipe.go:55-122`
documents that today's Stage 2.1 parser fleet does NOT emit:

- `decision_blocks` attr → **`cyclo`, `cognitive_complexity`** silently emit nothing
- `call_edges` attr → **`fan_in`, `fan_out`** silently emit nothing
- `field_accesses` attr → **`lcom4`** silently emits nothing

**What likely lights up today** (each needs a smoke run to confirm):

- `loc` ✓ — trivial.
- `duplication_ratio` ✓ — lexical, uses `AttrSourceBytes`
  (`services/clean-code/internal/metrics/recipes/recipe.go:137-160`).
- `cycle_member` ✓ — needs `imports`/`extends` edges (parser does emit these
  per `services/clean-code/internal/ast/parser/parser.go:8-18`) +
  `AttrModulePath`.
- `interface_width` ✓ — class scope public method count, derivable from
  scope tree.
- `depth_of_inheritance` ✓ — same.
- `coupling_between_objects` — partial (depends on edges).
- `cyclo` / `cognitive_complexity` / `fan_in` / `fan_out` / `lcom4` —
  **dark** until the parser is extended.
- `lsp_violation` — needs cross-class signature analysis; status unclear.
- `modification_count_in_window` — needs git history, not AST.

**Estimated effort to close the parser attr gap:** 1-2 weeks per language to
add decision-block decomposition + call-site walking + field-access walking.
Bounded but non-trivial (tree-sitter query work).

---

## L4 — Rule engine + policy (*reusable as-is*)

- `services/clean-code/internal/rule_engine/inmem_store.go:16-29` —
  `InMemoryStore` literally documented as:
  > *"Used by … the scaffold-mode composition root (a future Stage 5.8 wiring)
  > when `CLEAN_CODE_PG_URL` is unset -- so a developer can exercise the engine
  > end-to-end against a fixture without a database."*
- `services/clean-code/internal/policy/dsl/` — pure DSL evaluator over a
  single `Sample`.
- `services/clean-code/internal/policy/steward/` — `PolicyVersion` / `Rule` /
  `RulePack` are Go structs.
- `services/clean-code/policy/rulepacks/{solid,decoupling}/*.yaml` — plain
  YAML with `predicate_dsl: "metric_kind == 'lcom4' AND scope_kind == 'class' AND value >= 10"`
  style predicates and markdown descriptions.

**Estimated effort:** zero for the engine itself. The CLI loads YAML packs →
builds an in-memory `PolicyVersion` → constructs `InMemoryStore` → calls
`Engine.Evaluate`.

---

## L5 — Refactor planner (*reusable as-is*)

- `services/clean-code/internal/refactor/planner.go:498-545` —
  `InMemoryMetricSample` + `InMemoryMetricSampleReader` (*"used by … the
  scaffold-mode composition root when `CLEAN_CODE_PG_URL` is unset"*).
- `services/clean-code/internal/refactor/planner.go:168-198` — `PlanResult`
  is a public Go struct, serializable.
- `services/clean-code/internal/refactor/task_planner.go:77-118` — five
  canonical `TaskKind` values: `split_class`, `extract_method`,
  `invert_dependency`, `break_cycle`, `consolidate_duplication`.
- `services/clean-code/internal/refactor/task_planner.go:197-260` — rule_id
  → TaskKind mapping table.

**Estimated effort:** zero, modulo the ONNX effort-model fallback (a
deterministic effort heuristic — `α·loc + β·cyclo + γ·fan_in` — is fine
for v1).

---

## L6 — CLI composition (*missing*)

No `cmd/cleanc/main.go`. The composition wiring needs to be written from
scratch but is straightforward given L2–L5 reusables.

**Estimated effort:** half-day to a day for the basic shell.

---

## L7 — Code-change suggestions as patches (*architectural blocker — the main ask*)

**Quote from `docs/stories/code-intelligence-CLEAN-CODE/architecture.md:58-59`:**

> "Source-code rewriting / auto-fix (the Refactor Planner emits rows, never patches)" — *out of scope.*

**What exists today:**

- Task kinds (categorical labels): `split_class`, `extract_method`,
  `invert_dependency`, `break_cycle`, `consolidate_duplication`.
- Rule pack YAML descriptions with "Suggested refactor:" prose
  (e.g. `services/clean-code/policy/rulepacks/solid/srp.yaml:84-104`:
  *"Suggested refactor: split the class along the cohesion boundaries (SRP)"*).
- The offending `scope_id` + `metric_kind` + `value` per finding.

**What does NOT exist:**

- Any diff / patch / structured-edit generator.
- Any per-language refactoring transformer.
- Any AST-rewriter.

**Three realistic paths forward — pick one or combine:**

| Option | What you ship                                                                                                                                                                  | Effort                            | Quality                                                                                                                                                            |
| ------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | --------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| **A**  | Structured edit instructions for an AI coder: JSON object per task `{rule_id, file, line_range, scope_kind, source_snippet, task_kind, prose_suggestion}` — paste into Copilot/Claude as a refactor prompt | **1-2 days**                      | Best ROI; the LLM does the patch synthesis                                                                                                                         |
| **B**  | Pattern-based transformers: hand-written Go functions, one per `(TaskKind × language)`, take the AST and emit a unified diff                                                  | **1-2 weeks per task-kind-language pair** | Mechanical but brittle; works well for narrow patterns (extract method, break import cycle) and badly for cross-cutting ones (split class)                          |
| **C**  | Hybrid: mechanical patches for the 2-3 highest-value cases (break_cycle via import inversion, consolidate_duplication via extract function), structured prompts for everything else | **2-3 weeks**                     | Best engineering outcome                                                                                                                                           |

**Recommended:** ship **A first**, then graduate to **C** as patterns emerge
from real-world usage.

> **Architectural note.** Per the repo authority rule (`README.md`: "docs win"),
> shipping L7 inside `services/clean-code` would require either an
> architecture amendment that lifts the "no auto-fix" out-of-scope clause, OR
> a sibling package that wraps the planner and emits suggestions without
> claiming to be the planner. Option A (structured prompts) is the safest
> framing because it stays strictly downstream of the planner and never
> rewrites source bytes from inside the service.

---

## L8 / L9 — Friction items

- **Policy signing.** The engine wants signed `PolicyVersion`s. The CLI needs
  either a `--dev-mode` flag that constructs an in-memory `PolicyVersion` and
  skips signature verification, or a baked-in dev signing key for local rule
  packs. The bypass is a few lines but must be auditable so production-mode
  operators don't accidentally ship signed builds with the bypass left on.
- **ONNX effort model.** Write a deterministic fallback estimator gated on
  the ONNX loader failing. Half-day.

---

## Cross-cutting risks

1. **The "lit-up metric" set today is a small subset.** Without the parser
   attr extensions (L3), you get findings on duplication / cycles /
   interface_width / DIT / loc but nothing on SRP/cohesion or fan-in/fan-out
   — that's a big chunk of the SOLID rule pack going dark.
2. **`scope_id` UUID minting** is deterministic from
   `(repo_id, scope_kind, canonical_signature, first_seen_sha)` per
   architecture G2 — the CLI must mint a stable `repo_id` per local path
   so re-runs are idempotent. Hashing the absolute path works.
3. **Rule pack signing** — see L8.
4. **License / text extraction.** `duplication_ratio` uses lexical
   tokenisation on source bytes; the CLI must read raw file bytes (not the
   parser's normalised form). Already supported via `AttrSourceBytes`
   (`services/clean-code/internal/metrics/recipes/recipe.go:137-160`).

---

## Concrete file plan

### New files

- `services/clean-code/cmd/cleanc/main.go` — CLI entry, flag parsing,
  sub-commands (`analyze`, `report`, future: `apply`).
- `services/clean-code/internal/cli/walk/` — repo walker with gitignore +
  size limits + language detection.
- `services/clean-code/internal/cli/orchestrator/` — `Walk → Parse → Recipes
  → Engine → Planner` pipeline.
- `services/clean-code/internal/cli/report/` — markdown + JSON serializers
  for `PlanResult`.
- `services/clean-code/internal/cli/suggest/` — Option A's structured-edit-instruction emitter.
- `services/clean-code/internal/cli/devpolicy/` — load YAML rule packs →
  in-memory unsigned `PolicyVersion` (with a loud "dev mode" banner).

### Reuse as-is

- `services/clean-code/internal/ast/parser/*`
- `services/clean-code/internal/metrics/recipes/*`
- `services/clean-code/internal/rule_engine/inmem_store.go` + `engine.go`
- `services/clean-code/internal/refactor/{planner,task_planner}.go` (InMemory variants)
- `services/clean-code/internal/policy/{dsl,steward}/*`
- `services/clean-code/policy/rulepacks/{solid,decoupling}/*.yaml`

### Eventually refactor

- `services/clean-code/internal/ast/parser/` per-language adapters — add
  decision-block decomposition + call-edge + field-access walking.
  **Without this, half the metric catalogue is dark.**

---

## Phased roadmap (recommended)

| Phase  | Scope                                                                                                                  | Effort                  | Deliverable                                                                                                                |
| ------ | ---------------------------------------------------------------------------------------------------------------------- | ----------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| **P0** | CLI shell. L1 + L6 + L4 + L5 wiring, only metrics that work today (`loc`, `duplication_ratio`, `cycle_member`, `interface_width`, `depth_of_inheritance`) | **2-3 days**            | `cleanc analyze <path> --out report.md` produces a markdown report of findings + hotspots + categorical refactor tasks     |
| **P1** | Structured edit instructions (L7 Option A)                                                                             | **+1-2 days**           | `cleanc analyze --emit-prompts prompts.jsonl` emits one structured refactor prompt per task, ready for AI coders            |
| **P2** | Parser attr expansion (L3) — add `decision_blocks`, `call_edges`, `field_accesses` for Go first, then Python, TS, Java | **1-2 weeks per language** | Full SOLID rule pack lights up, including `cyclo` / `lcom4` / `fan_in` / `fan_out`                                          |
| **P3** | Mechanical patches (L7 Options B/C) for the 2-3 highest-value task kinds                                               | **2-3 weeks**           | `cleanc analyze --apply <task_id>` emits a unified diff or applies it directly                                              |

---

## Open questions

1. **Architectural authority for L7.** Will the team accept lifting the
   "no auto-fix" out-of-scope clause for an Option B/C path, or should the
   CLI strictly stay on Option A (prompts only)?
2. **Language priority.** Is Go-first the right call for P2, or does the
   target repo language mix argue for Python / TypeScript first?
3. **Where does the CLI binary live in the deployed surface?** Sibling
   alongside the six existing service binaries (`cmd/cleanc/`), separate
   repo, or a `pkg/`-exported library that an external CLI wraps?
4. **Rule pack distribution.** Do we ship the YAML packs embedded in the
   binary (via `//go:embed`) so the CLI works offline, or require a
   `--policy <path>` flag pointed at a checked-out tree?
