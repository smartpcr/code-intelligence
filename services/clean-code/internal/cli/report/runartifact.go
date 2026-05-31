// -----------------------------------------------------------------------
// <copyright file="runartifact.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package report

import (
	"context"
	"io"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/orchestrator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/repocontext"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/walk"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/refactor"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// Renderer is the single output contract every `cleanc analyze`
// report writer satisfies. Pinned by REFACTOR-GUIDE
// `architecture.md` Sec 5.7:
//
//	type Renderer interface {
//	    Render(ctx context.Context, art RunArtifact, w io.Writer) error
//	}
//
// The composition root constructs one writer per requested
// output ([Markdown] for `--out`; future JSON for `--findings`)
// and dispatches all of them through this single contract so a
// future report kind (HTML, SARIF, …) only adds a new
// implementor, not a new dispatch site.
//
// Renderers MUST be safe to invoke once per [RunArtifact] and
// MUST NOT mutate `art`; the composition root reuses the same
// artifact across every writer in the dispatch loop.
type Renderer interface {
	// Render writes the report bytes for `art` into `w`. The
	// returned error is the first I/O or formatting failure;
	// implementors MUST surface the underlying error verbatim
	// so the composition root's `--out` / `--findings` flag
	// surface reports the offending path in its operator-facing
	// diagnostic.
	//
	// `ctx` is honoured for cancellation only -- the renderers
	// themselves do not perform context-bound I/O beyond writing
	// to `w` -- but is part of the canonical signature so a
	// future renderer (e.g. one that resolves an external
	// template) can wire ctx through without a signature break.
	Render(ctx context.Context, art RunArtifact, w io.Writer) error
}

// RunArtifact is the CLI-local container the composition root
// assembles after the Stage 2.2 orchestrator finishes and the
// rule engine + refactor planner have run. Field shape and
// ordering mirror REFACTOR-GUIDE `architecture.md` Sec 4.7
// verbatim so the JSON sidecar (Sec 3.7.2) can serialise the
// struct one-for-one without a per-field translation step.
//
// Every field carries a single, well-defined source so the
// composition root is the only assembler and the renderers
// stay strictly read-only on the value.
//
// # Stage ownership
//
// The "Markdown Report Renderer" stage (this commit) ONLY
// consumes [RunArtifact.Context], [RunArtifact.Policy],
// [RunArtifact.Verdict], and the [RunArtifact.DarkMetrics]
// summary count to produce the header + verdict blocks. The
// remaining fields are populated by the composition root in
// downstream stages; [Markdown] tolerates their zero values
// today and will gain per-section renderers in subsequent
// reports-and-delivery stages without rewriting the container.
// SchemaVersionCurrent is the canonical schema version stamp
// the JSON renderer writes into every emitted document. The
// acceptance scenario pins this as `"v1.2026.05"`. Bumping this
// constant is a breaking-change signal to downstream consumers
// and MUST be paired with a brief amendment.
const SchemaVersionCurrent = "v1.2026.05"

type RunArtifact struct {
	// SchemaVersion is the version tag stamped on every JSON
	// artifact so downstream consumers can detect format drift.
	// The [JSON] renderer auto-stamps this to
	// [SchemaVersionCurrent] when the field is empty, so the
	// composition root does not need to set it explicitly.
	SchemaVersion string `json:"schemaVersion"`

	// Context is the repo-root identity stamped by the
	// composition root -- `repo_id`, `head_sha`, `module_path`
	// -- per architecture Sec 4.1. The renderer reads
	// `Context.RootPath` and `Context.HeadSHA` into the
	// header block.
	Context repocontext.RepoContext

	// Policy is the in-memory [steward.PolicyVersion] the
	// rule engine evaluated against -- minted by the
	// dev-policy loader (architecture Sec 3.8). The renderer
	// reads `Policy.PolicyVersionID` (the architecture-pinned
	// "policy id" cell, Sec 4.5 row 1) and
	// `Policy.RefactorWeights.EffortModelVersion` (the
	// architecture-pinned "version" cell, Sec 4.5 row 6 /
	// Sec 9.3) into the header block.
	//
	// `Policy.Name` is the dev-mode identity tag
	// (`"cleanc-dev-policy"`, architecture Sec 4.5 row 2);
	// it is NOT a version signal and the renderer
	// deliberately does NOT use it for the "policy id +
	// version" cell -- the iter-1 evaluator flagged that
	// pairing.
	Policy steward.PolicyVersion

	// Files is one [WalkedFileSummary] per file the walker
	// emitted -- `path`, `language`, `size_bytes`,
	// `parse_status`. Populated by the composition root from
	// the orchestrator's `Result.Files` corpus; consumed by
	// the diagnostics-section renderer (Sec 3.7.1 step 6) in
	// a downstream stage.
	Files []WalkedFileSummary

	// Skips is the walker + orchestrator skip rows, sorted
	// per `walk.Skipped` for determinism (architecture
	// Sec 4.7 row "Skips").
	Skips []walk.WalkSkip

	// DarkMetrics is the per-`(metric_kind, language)`
	// dark-metric inventory the orchestrator surfaced when a
	// recipe's `AppliesTo` returned false (architecture
	// Sec 3.3 / Sec 5.3 / Sec 8.7). The renderer reads
	// `len(DarkMetrics)` into the header's "dark-metric
	// summary count" cell (architecture Sec 3.7.1 step 1).
	//
	// This field is the orchestrator's
	// `Diagnostics.DarkMetrics` slice copied verbatim;
	// duplication with `Diagnostics.DarkMetrics` mirrors the
	// architecture Sec 4.7 table row-for-row so a future
	// schema audit cross-checks the JSON sidecar against the
	// architecture without inferring the relationship.
	DarkMetrics []orchestrator.DarkMetric

	// Samples is the rule-engine sample corpus (architecture
	// Sec 4.4). Empty on this stage; populated by the
	// composition root in a downstream stage.
	Samples []rule_engine.Sample

	// Run is the rule-engine `EvaluationRun` row stamped by
	// the engine on each evaluation (architecture Sec 4.7).
	Run rule_engine.EvaluationRun

	// Verdict is the rule-engine `EvaluationVerdict` row.
	// The renderer reads `Verdict.Verdict` (one of
	// `pass`/`warn`/`block`) into the verdict block per
	// architecture Sec 3.7.1 step 2.
	Verdict rule_engine.EvaluationVerdict

	// Findings is the rule-engine `Finding` rows produced by
	// the run (architecture Sec 4.7). Empty on this stage;
	// populated by the composition root in a downstream
	// stage.
	Findings []rule_engine.Finding

	// HotSpots is the refactor planner's hot-spot ranking
	// (architecture Sec 4.7). Empty on this stage.
	HotSpots []refactor.HotSpot

	// Plan is the task planner's [refactor.RefactorPlan]
	// (architecture Sec 4.7). Zero value on this stage.
	Plan refactor.RefactorPlan

	// Tasks is the task planner's per-finding refactor task
	// rows (architecture Sec 4.7). Empty on this stage.
	Tasks []refactor.RefactorTask

	// Diagnostics is the orchestrator's dark-metric +
	// effort-source diagnostic container (architecture
	// Sec 4.7 row "Diagnostics"). The renderer reads
	// `Diagnostics.EffortSource` into the diagnostics block
	// in a downstream stage.
	Diagnostics orchestrator.Diagnostics
}

// WalkedFileSummary mirrors the architecture Sec 4.7 row
// "Files" -- one summary per file the walker emitted. The
// shape is the CLI-local read-model surface the JSON sidecar
// exposes; the orchestrator's [walk.WalkedFile] shape is the
// internal walker type and is NOT serialised directly.
type WalkedFileSummary struct {
	// Path is the file's `RepoRelPath` from the walker,
	// forward-slash, repo-relative.
	Path string `json:"path"`

	// Language is the canonical language tag returned by
	// [parser.DetectLanguage] (`go`, `python`,
	// `typescript`, `java`). Empty when the file was
	// classified as `unsupported_language`.
	Language string `json:"language"`

	// SizeBytes is the file size in bytes from the walker.
	SizeBytes int64 `json:"size_bytes"`

	// ParseStatus is the parser disposition for this file,
	// one of: `parsed`, `parser_error`, `parser_panic`,
	// `skipped`. The composition root stamps it from the
	// orchestrator's `Result.Files` + `Result.Skips` join.
	ParseStatus string `json:"parse_status"`
}
