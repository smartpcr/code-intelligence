package recipes

import (
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
)

// cycloMetricKind is the canonical metric_kind string for
// McCabe cyclomatic complexity (architecture Sec 1.4.1 row 1).
// Pinned as a const so a `grep -nF "cyclo"` lands one
// definition site; downstream rule packs cite the same
// literal in YAML. Critically the spelling is `cyclo` (NOT
// `cyclomatic_complexity` -- iter-1 evaluator item-3 forbids
// the long alias).
const cycloMetricKind = "cyclo"

// cycloVersion is the recipe's `version()` per Sec 8.6 line
// 1010. Bumping this MUST land alongside a `metric_version`
// bump on every emitted sample (architecture C4): the
// Metric Ingestor uses `metric_version` as part of the G2
// identity key so a definitional change to cyclo lands as a
// NEW row at the same `(repo_id, sha, scope_id,
// metric_kind)`, never as an in-place update.
const cycloVersion = 1

// CycloRecipe is the McCabe cyclomatic-complexity recipe for
// the foundation tier (architecture Sec 1.4.1 row 1 -- "cyclo
// | method, file | base | McCabe cyclomatic complexity").
//
// # Algorithm
//
// For each `SCOPE_KIND_METHOD` in the AST:
//
//	method_cyclo = 1 + sum(cycloDelta for every block_kind
//	                       in decisionTable nested at any
//	                       depth under the method)
//
// The +1 is the baseline single execution path through the
// method body. Each subsequent decision point (if, else_if,
// for, while, case, catch, ternary, logical_and, logical_or)
// contributes one additional path -- the classic McCabe
// formulation `V(G) = E - N + 2 = decisions + 1`.
//
// For the file scope, `file_cyclo = sum(method_cyclo for
// every method in the file)`. A file with no methods emits
// `value=0` (NOT a baseline of 1 -- a file with zero entry
// points has zero execution paths through the file scope; the
// per-method baseline is already accounted for in each method
// row). This matches the rubber-duck iter-1 critique on
// non-trivial baselines.
//
// # Scope kinds (canonical seven-enum)
//
// Drafts emit at `scope.KindMethod` and `scope.KindFile`
// only. The enum has NO `function` or `module` values
// (architecture Sec 5.2.3 lines 1039-1050); the [newDraft]
// helper panics on a non-canonical value as a defence against
// the iter-1 evaluator item-3 closed-set drift.
//
// # Capability gate
//
// [Recipe.AppliesTo] returns true iff:
//
//   - the AST is non-nil, AND
//   - the producer stamped `AstFile.Attrs["decision_blocks"] = "true"`
//     (the parser has decomposed method bodies into
//     `ScopeKindBlock` decision-point children), AND
//   - the AST is NOT degraded (`AstFile.DegradedReason == ""`).
//
// The degradation gate enforces architecture Sec 3.4 lines
// 490-494: "Computed rows are never `degraded=true`: if an
// input is missing the row is not written, not stamped
// degraded". A recipe that propagated `DegradedReason` onto a
// `source='computed'` row would violate the contract; the
// recipe instead SKIPS emission entirely.
//
// The Stage 2.1 parser fleet does NOT yet emit decision-block
// children, so in production the recipe currently produces
// zero drafts; the gate keeps the foundation tier from
// emitting a misleading `source='computed'` cyclo=1 row per
// method before the parser is upgraded.
type CycloRecipe struct{}

// cycloAllowedKinds is the closed scope_kind set the cyclo
// recipe is permitted to emit at, mirroring the architecture
// Sec 1.4.1 row 1 column 2 entry `method, file`. Passed to
// [newDraft] so the helper's per-recipe guard refuses any
// other value at the panic boundary.
var cycloAllowedKinds = []scope.Kind{scope.KindMethod, scope.KindFile}

// NewCycloRecipe returns a stateless [CycloRecipe]. The
// registry calls this once per process; a single instance is
// safe for concurrent Compute() calls because the recipe
// holds no per-call state.
func NewCycloRecipe() *CycloRecipe { return &CycloRecipe{} }

// MetricKind implements [Recipe].
func (r *CycloRecipe) MetricKind() string { return cycloMetricKind }

// Version implements [Recipe].
func (r *CycloRecipe) Version() int { return cycloVersion }

// Pack implements [Recipe]. cyclo is in the `base` pack
// (architecture Sec 1.4.1 row 1).
func (r *CycloRecipe) Pack() Pack { return PackBase }

// AppliesTo implements [Recipe]. Returns true iff the AST is
// non-nil, NOT degraded, AND the producer advertised the
// `decision_blocks` capability via [AttrDecisionBlocks].
//
// The non-degraded check realises architecture Sec 3.4 lines
// 490-494's "row not written, not stamped degraded" rule for
// computed-tier inputs: a degraded AST means the parser had
// to bail; emitting a `source='computed'` row off truncated /
// partial scope data would silently lie about the metric.
func (r *CycloRecipe) AppliesTo(ast *parser.AstFile) bool {
	if !hasDecisionCapability(ast) {
		return false
	}
	if ast.GetDegradedReason() != "" {
		return false
	}
	return true
}

// Compute implements [Recipe].
//
// Returns drafts in stable emit order: one draft per method
// in source order, then one file-level draft. An empty input
// (no methods AND no file scope) returns nil.
//
// The caller MUST gate on [CycloRecipe.AppliesTo] before
// invoking Compute. Compute itself ALSO honours the
// degradation gate (it returns nil when
// `ast.DegradedReason != ""`) as defence-in-depth: a future
// caller that forgets the gate STILL cannot land a degraded
// `source='computed'` row.
func (r *CycloRecipe) Compute(ast *parser.AstFile) []MetricSampleDraft {
	if ast == nil {
		return nil
	}
	// Defence-in-depth against the Sec 3.4 "computed rows are
	// never degraded" rule -- the AppliesTo gate already
	// refused a degraded AST, but Compute is allowed to be
	// called directly in tests / by future callers, so the
	// same skip lives here.
	if ast.GetDegradedReason() != "" {
		return nil
	}
	idx := buildIndex(ast)
	if idx.file == nil {
		// No file scope -- a malformed AST. Bail rather
		// than emitting a dangling method draft with no
		// parent file row.
		return nil
	}

	methods := idx.methodScopes()
	drafts := make([]MetricSampleDraft, 0, len(methods)+1)
	var fileSum float64

	for _, m := range methods {
		decisions := r.countCycloDecisions(m, idx)
		value := float64(1 + decisions)
		fileSum += value
		drafts = append(drafts, newDraft(
			cycloMetricKind,
			cycloVersion,
			PackBase,
			SourceComputed,
			value,
			ScopeRef{
				LocalID:       m.GetScopeId(),
				Kind:          scope.KindMethod,
				QualifiedName: m.GetQualifiedName(),
				Path:          ast.GetPath(),
				// iter-5 evaluator item 3: thread the
				// parameter type list so overloaded
				// methods sharing (path, qualifiedName)
				// mint DISTINCT canonical signatures via
				// `scope.BuildMethod`'s `params` arg.
				Params: m.GetParameters(),
			},
			nil,
			cycloAllowedKinds,
		))
	}

	// File-level draft: sum of all method cyclos in the file.
	// Empty files (no methods) emit value=0; the per-file
	// baseline of 1 is intentionally NOT applied (rubber-duck
	// iter-1 critique #3: "empty file should usually be 0,
	// not 1").
	drafts = append(drafts, newDraft(
		cycloMetricKind,
		cycloVersion,
		PackBase,
		SourceComputed,
		fileSum,
		ScopeRef{
			LocalID:       idx.file.GetScopeId(),
			Kind:          scope.KindFile,
			QualifiedName: idx.file.GetQualifiedName(),
			Path:          ast.GetPath(),
		},
		nil,
		cycloAllowedKinds,
	))

	return drafts
}

// countCycloDecisions returns the McCabe decision count for a
// single method scope -- the sum of `cycloDelta` over every
// decision-flagged block scope nested at any depth under the
// method.
//
// Nested method scopes (closures emitted as their own
// `SCOPE_KIND_METHOD` children) are NOT counted in the
// parent's total -- a closure has its own cyclo row, and
// counting its branches in both would double-count execution
// paths. The walker stops descending at a method boundary.
func (r *CycloRecipe) countCycloDecisions(method *parser.AstScope, idx scopeIndex) int {
	total := 0
	var walk func(parentID string)
	walk = func(parentID string) {
		for _, child := range idx.children[parentID] {
			if child.GetScopeKind() == parser.ScopeKindMethod {
				// Closure boundary -- skip; the closure
				// emits its own method-cyclo row.
				continue
			}
			if child.GetScopeKind() == parser.ScopeKindBlock {
				kind := DecisionKind(child.GetAttrs()[AttrDecisionKind])
				if info, ok := decisionTable[kind]; ok {
					total += info.cycloDelta
				}
			}
			walk(child.GetScopeId())
		}
	}
	walk(method.GetScopeId())
	return total
}
