// -----------------------------------------------------------------------
// <copyright file="record.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

// Package suggest implements the L7 Option A structured-prompt
// emitter described in architecture.md Sec 3.7.3 / Sec 4.6 and
// tech-spec.md Sec 4.9. The package owns the on-wire shape of
// the RefactorPromptRecord JSONL artifact emitted by
// `cleanc analyze --emit-prompts <path>`; downstream AI coders
// (Copilot / Claude / etc.) consume the artifact to synthesise
// patches. The emitter itself never rewrites source bytes (per
// CLEAN-CODE architecture Sec 1.2 "no auto-fix" clause).
package suggest

// PromptFormatVersion is the canonical wire-format version of
// RefactorPromptRecord. Every record emitted by this package
// MUST carry this value in its PromptFormatVersion field.
//
// Bump this constant -- and document the change in
// docs/cleanc/PROMPT-FORMAT.md -- whenever any of the following
// happen:
//
//   - A field is added, removed, or renamed in
//     RefactorPromptRecord, Scope, or MetricEvidence.
//   - The semantic meaning of an existing field changes.
//   - The set of allowed values for an enum-shaped field changes.
//
// Downstream prompt templates pin against this version so
// breaking shape changes do not silently regress their output.
//
// Anchor: tech-spec.md Sec R12 (prompt-format versioning),
// architecture.md Sec 4.6.
const PromptFormatVersion = "v1.2026.05"

// Scope is the per-record scope sub-object of
// RefactorPromptRecord. It mirrors the scope-binding fields the
// walker / parser populated (see architecture.md Sec 4.3) so the
// AI coder downstream can locate the offending region by file +
// line range and by stable canonical signature.
type Scope struct {
	// Signature is the canonical signature minted by the
	// scope-binding subsystem (e.g. fully-qualified class or
	// function name). Stable across re-runs at the same SHA.
	Signature string `json:"signature"`

	// Kind is the scope kind enum (e.g. "class", "function",
	// "module"). Matches ScopeBinding.ScopeKind.
	Kind string `json:"kind"`

	// FilePath is the repo-relative path to the scope's host
	// file. Forward slashes on every platform.
	FilePath string `json:"file_path"`

	// StartLine is the 1-based inclusive starting line of the
	// scope in FilePath.
	StartLine int `json:"start_line"`

	// EndLine is the 1-based inclusive ending line of the
	// scope in FilePath.
	EndLine int `json:"end_line"`
}

// MetricEvidence is one supporting metric sample for a rule
// firing. Multiple evidences appear when a rule consumes more
// than one metric kind (e.g. a SOLID rule combining cyclo +
// loc + lcom4). The triple (Value, Threshold, Op) lets the AI
// coder describe to the developer exactly why the rule fired.
type MetricEvidence struct {
	// MetricKind is the canonical metric name (e.g. "cyclo",
	// "loc", "lcom4"). Matches metric.Kind in the recipe set.
	MetricKind string `json:"metric_kind"`

	// Value is the observed sample value.
	Value float64 `json:"value"`

	// Threshold is the rule's configured threshold.
	Threshold float64 `json:"threshold"`

	// Op is the DSL comparison operator that fired
	// (e.g. ">=", ">", "<", "<=", "==", "!=").
	Op string `json:"op"`
}

// RefactorPromptRecord is the JSON-Lines wire shape emitted by
// the suggest package: one record per RefactorTask. The record
// is the AI-coder hand-off payload defined in
// architecture.md Sec 4.6 and the L7 Option A artifact pinned
// in tech-spec.md Sec 3 row "A: Structured edit instructions"
// (SELECTED).
//
// JSON field names below are normative: downstream prompt
// templates depend on them. Any rename is a wire-format change
// and MUST bump PromptFormatVersion.
type RefactorPromptRecord struct {
	// TaskID is the originating RefactorTask.TaskID (UUID).
	TaskID string `json:"task_id"`

	// PlanID is the originating RefactorTask.PlanID (UUID).
	PlanID string `json:"plan_id"`

	// RepoID is the deterministic repo UUID minted by the
	// repocontext subsystem (CLEAN-CODE arch G2).
	RepoID string `json:"repo_id"`

	// HeadSHA is the git commit SHA the analysis ran against.
	HeadSHA string `json:"head_sha"`

	// PolicyVersionID is the UUID of the in-memory
	// PolicyVersion the engine evaluated against
	// (PolicyVersionInMemory.PolicyVersionID).
	PolicyVersionID string `json:"policy_version_id"`

	// TaskKind is the canonical task-kind enum string from
	// refactor.TaskKind -- one of split_class, extract_method,
	// invert_dependency, break_cycle, consolidate_duplication
	// (see tech-spec.md Sec 5.2).
	TaskKind string `json:"task_kind"`

	// RuleID is the rule that fired (steward.Rule.RuleID).
	RuleID string `json:"rule_id"`

	// RuleVersion is the integer version of the loaded rule
	// row (steward.Rule.Version).
	RuleVersion int `json:"rule_version"`

	// Severity is the rule's default severity
	// (steward.Rule.SeverityDefault), e.g. "info", "warn",
	// "error".
	Severity string `json:"severity"`

	// Scope describes where in the repo the rule fired.
	Scope Scope `json:"scope"`

	// SourceSnippet is the raw bytes between Scope.StartLine
	// and Scope.EndLine, capped at the suggest emitter's
	// snippet limit (default 200 lines, tech-spec.md Sec 8.2).
	// MUST be read fresh from disk by ExtractSnippet -- never
	// from the parser's normalised form -- per constraint
	// C12 / R4 mitigation.
	SourceSnippet string `json:"source_snippet"`

	// SourceSnippetTruncated is true when the snippet cap
	// fired and SourceSnippet is shorter than the scope's
	// full line range.
	SourceSnippetTruncated bool `json:"source_snippet_truncated"`

	// MetricEvidence is the set of metric samples that
	// supported the rule firing. At least one element when
	// the rule was DSL-evaluated.
	MetricEvidence []MetricEvidence `json:"metric_evidence"`

	// ProseSuggestion is the human-authored remediation
	// prose copied verbatim from the rule's DescriptionMD
	// markdown field.
	ProseSuggestion string `json:"prose_suggestion"`

	// EffortHours is the estimated remediation cost, in
	// hours, sourced from RefactorTask.EffortHours.
	EffortHours float64 `json:"effort_hours"`

	// EffortSource names the estimator that produced
	// EffortHours -- either "ml" (Stage 8.3 ONNX model) or
	// "fallback" (deterministic heuristic). Surfacing the
	// mode in every record is constraint C15.
	EffortSource string `json:"effort_source"`

	// PromptFormatVersion is the wire-format pin -- always
	// PromptFormatVersion at the moment the record is
	// emitted. Downstream prompt templates pin against this
	// value so shape changes never silently regress.
	PromptFormatVersion string `json:"prompt_format_version"`
}
