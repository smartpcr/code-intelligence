# `cleanc` JSONL refactor-prompt format (`RefactorPromptRecord`)

> **Authority order** (per repository `README.md`): when this document
> and the source disagree, the **source wins** --
> [`services/clean-code/internal/cli/suggest/record.go`](../../services/clean-code/internal/cli/suggest/record.go).
> This file is the consumer contract for downstream AI coders; the
> on-wire shape is pinned by Go struct field tags, asserted by
> `services/clean-code/internal/cli/suggest/emitter_test.go`, and
> versioned by the `PromptFormatVersion` constant.
>
> **Spec anchors:** REFACTOR-GUIDE
> [`architecture.md`](../stories/code-intelligence-REFACTOR-GUIDE/architecture.md)
> Sec 3.7.3 / Sec 4.6 (L7 Option A structured edit instructions);
> [`tech-spec.md`](../stories/code-intelligence-REFACTOR-GUIDE/tech-spec.md)
> Sec 4.9 (emitter contract), Sec 8.2 (snippet cap), Sec R12
> (prompt-format versioning).

## 1. Wire format

`cleanc analyze --emit-prompts <path>` writes a **JSON Lines** stream
to `<path>`: one record per `RefactorTask`, one record per line,
UTF-8, LF line terminators, no trailing array brackets, no trailing
comma. Each line is a complete, parseable JSON object.

- One record per `(ScopeID, RuleID)` pair (the Stage 4.1 emitter's
  fan-out invariant).
- Record order is the engine's stable scope-binding order; see
  `internal/cli/suggest/emitter.go` for the canonical sort key.
- The sidecar is emitted **after** the `--out` markdown report and
  `--findings` JSON sidecar, so the operator can pipe it into an
  AI coder without re-running analysis.
- Empty result set produces a zero-byte file (NOT a single empty
  line). Downstream tools should treat the absence of records as
  "no findings" rather than as an error.

The contract is the same when emitted to stdout via the conventional
`--emit-prompts -` sentinel (when supported by the operator wiring).

## 2. Format versioning

Every record carries a `prompt_format_version` field whose value is
the `suggest.PromptFormatVersion` constant -- currently:

```
v1.2026.05
```

Downstream prompt templates **MUST** pin against this value. The
version is bumped (and this document amended) whenever:

- A field is added, removed, or renamed in `RefactorPromptRecord`,
  `Scope`, or `MetricEvidence`.
- The semantic meaning of an existing field changes.
- The set of allowed values for an enum-shaped field changes.

A bump is a wire-format break; consumers that pin against the old
version should refuse to consume the new stream and fall back to a
re-render path.

## 3. Top-level fields (`RefactorPromptRecord`)

Field order below matches the Go struct declaration order; the JSON
encoder preserves declaration order, so the field order is part of
the wire contract for line-by-line diff stability.

| JSON field                  | Go type            | Semantics |
| --------------------------- | ------------------ | --------- |
| `task_id`                   | `string` (UUID)    | Originating `refactor.RefactorTask.TaskID`. Stable across re-runs at the same SHA. |
| `plan_id`                   | `string` (UUID)    | Originating `refactor.RefactorTask.PlanID`; groups every task that came out of one planner invocation. |
| `repo_id`                   | `string` (UUID)    | Deterministic repo UUID minted by `internal/cli/repocontext.MintRepoID` (CLEAN-CODE arch G2); hashed from the absolute repo path so re-runs against the same checkout collide. |
| `head_sha`                  | `string`           | `git HEAD` SHA the analysis ran against, detected by `repocontext.DetectHeadSHA`. Empty string when the repo is not a git checkout. |
| `policy_version_id`         | `string` (UUID)    | UUID of the in-memory `steward.PolicyVersion` the engine evaluated against. Re-issued on every dev-mode load (unsigned bundles do not survive across runs). |
| `task_kind`                 | `string` (enum)    | Canonical `refactor.TaskKind` string. Closed set: `split_class`, `extract_method`, `invert_dependency`, `break_cycle`, `consolidate_duplication` (tech-spec Sec 5.2). Any other value is a wiring bug and the emitter fails closed with `refactor.ErrRejectedTaskKindAlias`. |
| `rule_id`                   | `string`           | `steward.Rule.RuleID` that fired (e.g. `decoupling.cycle_member_present`, `solid.lcom4_high`). Dotted namespace = `<pack>.<rule_short_name>`. |
| `rule_version`              | `int`              | Integer version of the loaded rule row (`steward.Rule.Version`); lets prompt templates skip records minted against an older rule revision. |
| `severity`                  | `string` (enum)    | Rule's default severity (`steward.Rule.SeverityDefault`). Closed set: `info`, `warn`, `error` (also accepted as `block` in the operator-facing `--exit-on` flag; the record always uses the rule-pack-side spelling). |
| `scope`                     | `object` (`Scope`) | Per-record scope sub-object; see §4. |
| `source_snippet`            | `string`           | Raw source bytes between `scope.start_line` and `scope.end_line`, **read fresh from disk** by `suggest.ExtractSnippet` (never from the parser's normalised form, per constraint C12 / R4 mitigation). Bytes are emitted verbatim except for JSON-mandated escaping; the loader MUST NOT assume any particular line-ending normalisation. |
| `source_snippet_truncated`  | `bool`             | `true` when the snippet cap (default 200 lines, tech-spec Sec 8.2) fired and `source_snippet` is shorter than the scope's full line range. When true, the snippet ends with a `... [truncated N lines]` sentinel on the last retained line. |
| `metric_evidence`           | `array` of `MetricEvidence` | Supporting metric samples that drove the rule firing; see §5. Always a JSON array (never `null`); empty `[]` when the rule did not consume any metric kinds (e.g. a structural-only rule) or when every supporting sample was absent from the in-memory `MetricSampleReader`. |
| `prose_suggestion`          | `string`           | Human-authored remediation prose copied verbatim from the rule's `DescriptionMD` field (or, when the description is empty, from `RefactorTask.DescriptionMD`). Markdown is preserved as written; the AI coder is expected to render or strip it. |
| `effort_hours`              | `float64`          | Estimated remediation cost in hours, sourced from `refactor.RefactorTask.EffortHours`. Bounded to `[0.1, 80.0]` and rounded half-up to one decimal place (tech-spec Sec 8.5). |
| `effort_source`             | `string` (enum)    | Estimator that produced `effort_hours`. Closed set: `ml` (ONNX model, Stage 8.3) or `fallback` (deterministic heuristic). Surfacing the mode in every record is constraint C15. |
| `prompt_format_version`     | `string`           | Wire-format pin; always equal to `suggest.PromptFormatVersion` at the moment the record is emitted. See §2. |

## 4. `scope` sub-object

The `scope` field carries the location triple the AI coder needs to
locate the offending region. Every sub-field mirrors the
`ScopeBinding` row the walker / parser populated for the offending
scope (architecture Sec 4.3).

| JSON field   | Go type | Semantics |
| ------------ | ------- | --------- |
| `signature`  | `string` | Canonical signature minted by `internal/cli/scopebinding` (e.g. fully-qualified class / function name, package path for module-kind scopes). Stable across re-runs at the same SHA. |
| `kind`       | `string` | Scope kind enum mirroring `ScopeBinding.ScopeKind`. Typical values: `class`, `function`, `method`, `module`, `package`. The closed set is the union of values emitted by the four pinned parsers. |
| `file_path`  | `string` | Repo-relative path to the scope's host file. **Forward slashes on every platform** (the walker normalises Windows separators before minting the binding). |
| `start_line` | `int`    | 1-based inclusive starting line of the scope in `file_path`. |
| `end_line`   | `int`    | 1-based inclusive ending line of the scope in `file_path`. |

## 5. `metric_evidence` array elements

Each element supports the rule firing with one metric sample. The
triple `(value, threshold, op)` lets the AI coder describe to the
developer exactly why the rule fired (e.g. "cyclo was 23, threshold
is >= 15").

| JSON field    | Go type   | Semantics |
| ------------- | --------- | --------- |
| `metric_kind` | `string`  | Canonical metric name (e.g. `cyclo`, `loc`, `lcom4`, `fan_in`); matches the `metric.Kind` set populated by `internal/metrics/recipes/`. |
| `value`       | `float64` | Observed sample value at the offending scope. |
| `threshold`   | `float64` | Rule's configured threshold extracted from the DSL predicate. |
| `op`          | `string`  | DSL comparison operator that fired. Closed set: `>=`, `>`, `<`, `<=`, `==`, `!=`. |

Evidence rows are emitted in DSL declaration order. An empty
`metric_evidence: []` is **not** a wiring bug: it indicates either
(a) a structural-only rule (e.g. import-cycle detection) or (b) a
sample miss against the in-memory `MetricSampleReader`. The emitter
NEVER fabricates zero-valued evidence rows -- if the sample is
absent the row is omitted.

## 6. Worked example

The following record was emitted by the `p0-go-cycle` e2e scenario
(after the field set graduates to `--emit-prompts` coverage); UUIDs
and the head SHA are masked to `<uuid>` / `<sha>` to mirror the
golden-file normaliser's mask.

```json
{
  "task_id": "<uuid>",
  "plan_id": "<uuid>",
  "repo_id": "<uuid>",
  "head_sha": "<sha>",
  "policy_version_id": "<uuid>",
  "task_kind": "break_cycle",
  "rule_id": "decoupling.cycle_member_present",
  "rule_version": 1,
  "severity": "warn",
  "scope": {
    "signature": "example.com/cyclefixture/pkg/a",
    "kind": "package",
    "file_path": "pkg/a/a.go",
    "start_line": 1,
    "end_line": 12
  },
  "source_snippet": "package a\n\nimport \"example.com/cyclefixture/pkg/b\"\n\nfunc CallB() string {\n\treturn b.Greet()\n}\n",
  "source_snippet_truncated": false,
  "metric_evidence": [
    {
      "metric_kind": "cycle_member",
      "value": 1,
      "threshold": 1,
      "op": ">="
    }
  ],
  "prose_suggestion": "Break the import cycle by introducing an interface in a third package that both `pkg/a` and `pkg/b` depend on, then move the shared abstraction behind that boundary (Dependency Inversion).",
  "effort_hours": 1.4,
  "effort_source": "fallback",
  "prompt_format_version": "v1.2026.05"
}
```

### Round-trip hand-off to an AI coder

The canonical framing for piping a record into Copilot Chat / Claude
/ etc. is:

> Here is a structured refactor request emitted by `cleanc`.
> Synthesise a unified diff that implements the `prose_suggestion`
> against the `source_snippet`, preserving the scope's `signature`
> and respecting the `metric_evidence` thresholds. Reply with the
> diff only.

Followed by the JSON object as the message body. The AI coder
receives every field it needs -- the offending bytes
(`source_snippet`), the location (`scope.file_path` + line range),
the canonical refactor category (`task_kind`), and the human-authored
remediation prose (`prose_suggestion`) -- and never has to read any
other file to produce a candidate patch.

The emitter itself **never** rewrites source bytes (per CLEAN-CODE
architecture Sec 1.2 "no auto-fix" clause); the JSONL sidecar is
the AI coder's only hand-off surface today. The `apply` verb --
which would land mechanical patches -- is reserved at exit code 64
pending operator pin `cli-l7-authority` (REFACTOR-GUIDE
architecture Sec 6.3).

## 7. Compatibility notes for consumers

- **Pin against `prompt_format_version`.** Templates that ignore
  the version field will silently drift when fields are added.
- **Tolerate unknown JSON fields.** Field additions within the same
  major version do NOT bump `prompt_format_version`; consumers
  should ignore unknown fields rather than error.
- **Do NOT depend on field ordering inside `metric_evidence`.**
  The order is the DSL declaration order, which is stable per
  rule but may change across rule-pack revisions (a `rule_version`
  bump).
- **Do NOT assume any particular value for `head_sha` on a non-git
  checkout.** The field can be the empty string; consumers should
  fall back to `repo_id` for cache keys.
- **Treat empty `metric_evidence: []` as semantically equivalent
  to absent supporting metrics**, not as a wiring bug.

## 8. Cross-references

- [`services/clean-code/internal/cli/suggest/record.go`](../../services/clean-code/internal/cli/suggest/record.go)
  -- source of truth for the JSON struct tags and the
  `PromptFormatVersion` constant.
- [`services/clean-code/internal/cli/suggest/emitter.go`](../../services/clean-code/internal/cli/suggest/emitter.go)
  -- emitter implementation, fail-closed sentinel errors
  (`ErrNilBindingTable`, `ErrNilWriter`, `MissingScopeBindingError`).
- [`services/clean-code/internal/cli/suggest/emitter_test.go`](../../services/clean-code/internal/cli/suggest/emitter_test.go)
  -- table-driven assertions that pin the wire shape.
- [`docs/cleanc/USAGE.md`](USAGE.md) -- operator walkthrough of the
  `--emit-prompts` workflow (§6.3).
- [`docs/stories/code-intelligence-REFACTOR-GUIDE/tech-spec.md`](../stories/code-intelligence-REFACTOR-GUIDE/tech-spec.md)
  -- Sec 4.9 emitter contract, Sec 8.2 snippet cap, Sec R12 versioning.
- [`docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md`](../stories/code-intelligence-REFACTOR-GUIDE/architecture.md)
  -- Sec 3.7.3 / Sec 4.6 L7 Option A.
