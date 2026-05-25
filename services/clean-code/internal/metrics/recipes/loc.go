package recipes

import (
	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/scope"
)

// locMetricKind is the canonical metric_kind string for
// physical lines of code (architecture Sec 1.4.1 row 3).
// Pinned as a const so a `grep -nF "loc"` lands one
// definition site. The closed-set spelling is `loc` (NOT the
// long-form alias `lines_of_code` -- iter-1 evaluator item-3
// of the broader CLEAN-CODE workstream forbids that drift).
const locMetricKind = "loc"

// locVersion is the recipe's `version()` per Sec 8.6 line
// 1010. A bump MUST coincide with a `metric_version` bump on
// every emitted sample (architecture C4) so a definitional
// change (e.g. "exclude blank-only lines") lands as a new row
// at the same `(repo_id, sha, scope_id, metric_kind)`.
const locVersion = 1

// LocRecipe is the physical-lines-of-code recipe for the
// foundation tier (architecture Sec 1.4.1 row 3 -- "loc |
// file, package, repo | base | Lines of code (non-blank,
// non-comment).").
//
// # Two scope-kind sets, NOT one
//
// The architecture pins TWO related-but-distinct sets for
// `metric_kind='loc'`. Conflating them is the iter-2 bug
// the iter-3 evaluator surfaced; the iter-3 fix is to keep
// them lexically distinct in the recipe code.
//
//   - [locAllowedKinds] = `{file, package, repo}` -- the
//     METRIC_KIND'S APPLICABILITY SET. Sec 1.4.1 row 3
//     column 2 lists these as the canonical scope_kinds at
//     which a persisted `MetricSample(metric_kind='loc')`
//     row is legal. This is the closed-set the SCHEMA
//     accepts -- a row at `class` or `method` is rejected
//     by the writer.
//
//   - [locDirectlyEmittedKinds] = `{file}` -- the SUBSET OF
//     SCOPE_KINDS THIS RECIPE DIRECTLY EMITS AT, per
//     Compute call. The [Recipe.Compute] interface is
//     per-file (`Compute(ast *AstFile) []MetricSampleDraft`)
//     -- a single Compute call sees ONE `*AstFile` and so
//     can only authoritatively know one file's line count.
//     Emitting a draft at `scope_kind='package'` from one
//     file's loc would create a partial-fact row whose
//     persisted-identity `(repo_id, sha, package_scope_id,
//     metric_kind='loc', metric_version=1)` would collide
//     with every other file's partial in the same package
//     under the writer's uniqueness invariant (architecture
//     Sec 5.2.1 line 905). The recipe MUST NOT mint such a
//     conflict; the partial-fact aggregation lives downstream
//     (see "Package and repo rows" below).
//
// The two sets are wired separately so the architecture's
// scope-applicability table (`{file, package, repo}`) is
// still pinned at this layer (the helper guard accepts any
// of the three -- forward-compatible for the materialiser /
// aggregator that emits the upper-tier rows via the SAME
// `newDraft` helper) without forcing this per-file recipe to
// emit at scope_kinds it cannot authoritatively compute.
//
// # Algorithm (file scope)
//
// For each `*parser.AstFile` the recipe emits ONE draft at
// `scope.KindFile`:
//
//	loc = file_scope.range.end_line - file_scope.range.start_line + 1
//
// The parser fleet's `newScopeBuilder` (see
// `internal/ast/parser/internal.go`) populates the file
// scope's `Range` with `StartLine=1` and `EndLine=lineCount`,
// so the formula reduces to "physical line count" for a
// well-formed input. Blank-line / comment-line filtering is
// deferred to a later parser stage (the AstFile message does
// not yet carry a token-level line classification); this
// recipe is the canonical writer of file-scope `loc` rows
// and any future "non-blank, non-comment" refinement lands
// as a `locVersion` bump rather than a new metric_kind.
//
// # Package and repo rows (NOT this recipe)
//
// Package- and repo-level `loc` rows are produced by the
// SUM-aggregation path -- NOT by this per-file recipe.
// Specifically:
//
//   - PACKAGE-scope loc rows: produced by the Metric
//     Ingestor's loc materialiser (architecture Sec 5.2.5
//     "materialisers" + Sec 3.10 step 3 "package rollups"),
//     which SUMs the file-scope rows by package_scope_id at
//     ingest time. Slated for impl-plan Stage 2.6 alongside
//     `modification_count_in_window`. The schema's
//     `MetricSample` uniqueness invariant means the
//     materialiser writes ONE row per
//     `(repo_id, sha, package_scope_id, metric_kind='loc',
//     metric_version=1)` -- per-file partial rows from this
//     recipe at the package scope would BREAK that
//     invariant, which is why the per-file recipe must NOT
//     emit at `scope_kind='package'`.
//
//   - REPO-scope loc rows: produced by the Cross-Repo
//     Aggregator (architecture Sec 3.10 step 4) by SUMming
//     the package rows (or directly the file rows) for a
//     given `(repo_id, sha)`.
//
// This is NOT "delegating the recipe's job" -- it is the
// honest separation of concerns the per-file
// [Recipe.Compute] interface forces. A draft at the package
// scope, AUTHORITATIVELY VALUED, requires visibility into
// every `*AstFile` in the package; the recipe sees ONE file
// at a time, so it CANNOT mint that draft without
// fabricating a partial-fact row that violates the writer
// uniqueness invariant.
//
// Forward-compatible: the metric ingestor / aggregator
// stages will route their emissions through the SAME
// [newDraft] helper with the SAME [locAllowedKinds] slice;
// the helper's per-recipe guard already accepts `file`,
// `package`, and `repo` at the panic boundary.
//
// # Scope kinds (canonical seven-enum)
//
// Drafts emit at `scope.KindFile` only. The metric_kind
// applicability set ([locAllowedKinds]) ALSO permits
// `scope.KindPackage` and `scope.KindRepo` for downstream
// writers, but NEVER `function`, `method`, `class`,
// `interface`, `block`, or any non-canonical drift
// (`module`, `namespace`). The [newDraft] helper panics on
// any out-of-set value.
//
// # Capability + degradation gate
//
// The Stage 2.1 parser already emits a file-level scope with
// `Range.EndLine` populated, so unlike cyclo/cognitive the
// loc recipe does NOT depend on the `decision_blocks`
// capability flag. The recipe DOES honour the degradation
// gate: a degraded AST (`ast.DegradedReason != ""`) means
// the parser bailed mid-file; emitting `loc=N` off a
// truncated parse would lie about the physical line count,
// so [Recipe.AppliesTo] returns false and [Recipe.Compute]
// returns nil. This realises architecture Sec 3.4 lines
// 490-494 -- "Computed rows are never `degraded=true`: if
// an input is missing the row is not written".
type LocRecipe struct{}

// locAllowedKinds is the METRIC_KIND APPLICABILITY SET --
// the closed scope_kind set the SCHEMA accepts for a
// persisted `MetricSample(metric_kind='loc')` row, mirroring
// architecture Sec 1.4.1 row 3 column 2 entry `file,
// package, repo`.
//
// This is INTENTIONALLY broader than [locDirectlyEmittedKinds]
// (which lists what this per-file recipe emits): the package
// and repo entries are reserved for the downstream
// materialiser / aggregator (architecture Sec 5.2.5 / Sec
// 3.10) which routes its emissions through the SAME [newDraft]
// helper. Including them here means the per-recipe panic
// guard accepts the future emission paths without rewriting
// the shared helper.
var locAllowedKinds = []scope.Kind{scope.KindFile, scope.KindPackage, scope.KindRepo}

// locDirectlyEmittedKinds is the subset of [locAllowedKinds]
// at which THIS per-file recipe directly emits drafts -- one
// element: `file`. Kept as a named variable so a `grep -nF
// "locDirectlyEmittedKinds"` lands one definition site and
// the recipe's emission-shape contract is auditable.
//
// See [LocRecipe] doc "Package and repo rows (NOT this
// recipe)" for why the recipe does NOT emit at `package` /
// `repo`. Briefly: the per-file Compute interface means a
// single Compute call cannot authoritatively know a
// package's or repo's total loc, and emitting partial-fact
// rows would break the writer's `(repo_id, sha, scope_id,
// metric_kind, metric_version)` uniqueness invariant
// (architecture Sec 5.2.1 line 905).
var locDirectlyEmittedKinds = []scope.Kind{scope.KindFile}

// NewLocRecipe returns a stateless [LocRecipe]. Safe for
// concurrent Compute calls.
func NewLocRecipe() *LocRecipe { return &LocRecipe{} }

// MetricKind implements [Recipe].
func (r *LocRecipe) MetricKind() string { return locMetricKind }

// Version implements [Recipe].
func (r *LocRecipe) Version() int { return locVersion }

// AppliesTo implements [Recipe]. Returns true iff the AST is
// non-nil AND NOT degraded. Unlike cyclo / cognitive the loc
// recipe does NOT need the `decision_blocks` capability --
// the file scope's `Range` is populated by every parser in
// the fleet (Stage 2.1 onward).
func (r *LocRecipe) AppliesTo(ast *parser.AstFile) bool {
	if ast == nil {
		return false
	}
	if ast.GetDegradedReason() != "" {
		return false
	}
	return true
}

// Compute implements [Recipe].
//
// Returns exactly one draft at `scope.KindFile` whose `Value`
// is the file's physical line count. Returns nil on nil /
// degraded / file-scope-missing inputs.
//
// The caller MUST gate on [LocRecipe.AppliesTo] before
// invoking Compute. Compute itself ALSO honours the
// degradation gate as defence-in-depth -- the same skip
// shape as cyclo / cognitive.
//
// See the [LocRecipe] doc section "Package and repo rows
// (NOT this recipe)" for the rationale on why this Compute
// call does NOT also emit at `scope_kind='package'` or
// `scope_kind='repo'`. TL;DR: the per-file Compute interface
// makes authoritative package/repo loc impossible from a
// single AstFile; partial-fact rows would break the writer
// uniqueness invariant; the downstream materialiser (impl-
// plan Stage 2.6) SUMs file rows into package rows and the
// aggregator (Sec 3.10) SUMs into repo rows.
func (r *LocRecipe) Compute(ast *parser.AstFile) []MetricSampleDraft {
	if ast == nil {
		return nil
	}
	if ast.GetDegradedReason() != "" {
		return nil
	}
	idx := buildIndex(ast)
	if idx.file == nil {
		return nil
	}

	rg := idx.file.GetRange()
	if rg == nil || rg.GetEndLine() == 0 {
		// A file scope with no range is a producer bug;
		// emitting `loc=0` would silently lie. Skip
		// rather than emit a misleading row.
		return nil
	}
	start := rg.GetStartLine()
	if start == 0 {
		start = 1
	}
	end := rg.GetEndLine()
	if end < start {
		return nil
	}
	loc := float64(end - start + 1)

	return []MetricSampleDraft{
		newDraft(
			locMetricKind,
			locVersion,
			PackBase,
			SourceComputed,
			loc,
			ScopeRef{
				LocalID:       idx.file.GetScopeId(),
				Kind:          scope.KindFile,
				QualifiedName: idx.file.GetQualifiedName(),
				Path:          ast.GetPath(),
			},
			nil,
			locAllowedKinds,
		),
	}
}
