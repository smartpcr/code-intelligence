# `cleanc` JSONL refactor-prompt format

> Status: **draft** — populated incrementally as the P1
> structured-prompt-emitter stages land their record fields.

When invoked with `--emit-prompts <path>` (or `--emit-prompts -`
for stdout), `cleanc analyze` writes a **JSON Lines** stream
where each line is exactly one JSON object describing one
refactor task. The stream is emitted **after** the `--out`
markdown report and the `--findings` JSON sidecar so an
operator can pipe the file into an AI coder (Copilot Chat,
Claude, etc.) without re-running the analysis.

## Stream contract

- One record per `RefactorTask` (one task per `(ScopeID, RuleID)`
  pair, per `suggest.JSONL.Emit`).
- UTF-8, LF line terminators.
- Each line is a complete, parseable JSON object — no trailing
  comma, no surrounding array.
- Order of records is the engine's stable scope-binding order
  (NOT the insertion / map-iteration order); see the per-stage
  contracts in `services/clean-code/internal/cli/suggest/` for
  the canonical sort key.

## Record fields

The full per-field schema is pinned by the workstream brief
for the P1 structured-prompt-emitter stages and is materialised
in [`internal/cli/suggest/emitter.go`](../../services/clean-code/internal/cli/suggest/emitter.go).
The high-level shape is:

```json
{
  "rule_id":          "decoupling.cycle_member_present",
  "task_kind":        "extract_method",
  "scope_kind":       "package",
  "file":             "pkg/a/a.go",
  "line_range":       {"start": 1, "end": 80},
  "source_snippet":   "<source bytes the task applies to>",
  "prose_suggestion": "<rule-pack suggested_refactor markdown>"
}
```

(Fields land incrementally; see `CHANGELOG.md` for the per-stage
field additions.)

## End-to-end golden coverage

The `tests/e2e/cleanc/` harness does **not yet** ship a JSONL
golden scenario; the P0 scenarios exercise the markdown +
findings + diagnostics surface only. A `p1-emit-prompts`
scenario lands with the P1 structured-prompt-emitter
work-streams. Until then, the existing godog suites under
`services/clean-code/test/e2e/code-intelligence-REFACTOR-GUIDE/p1_structured_prompt_emitter_*`
are the authoritative coverage for the emitter contract.

## Cross-references

- `docs/stories/code-intelligence-REFACTOR-GUIDE/tech-spec.md`
  — Sec 6 / Sec 8 (CLI surface, JSONL emitter contract).
- `docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md`
  — Sec 5 (L7 structured edit instructions, Option A).
- `services/clean-code/internal/cli/suggest/` — Go source of
  the emitter (`emitter.go`, `emitter_test.go`).
