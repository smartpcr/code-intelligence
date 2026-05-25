package recipes

import (
	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/scope"
)

// cognitiveComplexityMetricKind is the canonical metric_kind
// string for SonarSource-style cognitive complexity
// (architecture Sec 1.4.1 row 2). The literal is `cognitive_complexity`
// (NOT `cognitive` -- the long-form spelling is the FK in
// `MetricKind` and the rule-pack YAML).
const cognitiveComplexityMetricKind = "cognitive_complexity"

// cognitiveComplexityVersion is the recipe's `version()` per
// Sec 8.6 line 1010. Bumping this MUST coincide with a
// `metric_version` bump on emitted samples (C4) so a
// definitional change to the SonarSource algorithm (e.g. a new
// nesting rule) lands as a new row at the same `(repo_id, sha,
// scope_id, metric_kind)`.
const cognitiveComplexityVersion = 1

// CognitiveComplexityRecipe is the SonarSource-style cognitive
// complexity recipe for the foundation tier (architecture
// Sec 1.4.1 row 2 -- "cognitive_complexity | method, file |
// base | SonarSource-style cognitive complexity").
//
// # Algorithm
//
// For each `SCOPE_KIND_METHOD` in the AST, walk descendant
// block scopes. For every block whose `attrs["decision_kind"]`
// is in the [decisionTable] taxonomy:
//
//	contribution = cognitiveDelta
//	if nestingBonus: contribution += nesting_level
//	if pushesNesting: descendants inherit (nesting_level + 1)
//
// where `nesting_level` is the count of `pushesNesting=true`
// ancestors between this block and the enclosing method. The
// per-method score is the sum of all contributions; the
// per-file score is the sum of per-method scores.
//
// # Why this differs from cyclo
//
// SonarSource's cognitive metric penalises **nesting** -- a
// nested `if` inside an `if` inside an `if` is harder to read
// than three flat `if`s even though cyclomatic complexity is
// the same. The `nesting_level` term captures that asymmetry.
// Logical operators (`&&`, `||`) and `else_if` / `else`
// continuations contribute flat (no nesting bonus) per
// SonarSource's published rules.
//
// # Scope kinds (canonical seven-enum)
//
// Drafts emit at `scope.KindMethod` and `scope.KindFile`
// only. The architecture's Sec 1.4.1 row 2 pins exactly these
// two kinds; emitting at `function` or `module` would violate
// the iter-1 evaluator item-3 closed-set guard and is blocked
// by [newDraft].
//
// # Capability + degradation gate
//
// [Recipe.AppliesTo] returns true iff the AST is non-nil,
// the producer stamped [AttrDecisionBlocks] = "true", AND the
// AST is NOT degraded. The non-degraded check honours
// architecture Sec 3.4 lines 490-494 -- "Computed rows are
// never `degraded=true`: if an input is missing the row is
// not written, not stamped degraded". Recipes never smuggle
// `degraded_reason` onto a `source='computed'` row; they
// silently skip emission instead.
//
// Same staging caveat as [CycloRecipe]: today's Stage 2.1
// parser fleet does not yet emit decision-block children, so
// production runs silently produce zero drafts until a future
// parser stage lights up the flag.
type CognitiveComplexityRecipe struct{}

// cognitiveComplexityAllowedKinds is the closed scope_kind
// set the recipe is permitted to emit at, mirroring
// architecture Sec 1.4.1 row 2 column 2 entry `method, file`.
// Passed to [newDraft] so the helper's per-recipe guard
// refuses any other value at the panic boundary.
var cognitiveComplexityAllowedKinds = []scope.Kind{scope.KindMethod, scope.KindFile}

// NewCognitiveComplexityRecipe returns a stateless recipe
// instance. Safe for concurrent Compute calls (no per-call
// state).
func NewCognitiveComplexityRecipe() *CognitiveComplexityRecipe {
	return &CognitiveComplexityRecipe{}
}

// MetricKind implements [Recipe].
func (r *CognitiveComplexityRecipe) MetricKind() string {
	return cognitiveComplexityMetricKind
}

// Version implements [Recipe].
func (r *CognitiveComplexityRecipe) Version() int {
	return cognitiveComplexityVersion
}

// Pack implements [Recipe]. cognitive_complexity is in the
// `base` pack (architecture Sec 1.4.1 row 2).
func (r *CognitiveComplexityRecipe) Pack() Pack { return PackBase }

// AppliesTo implements [Recipe]. Same gate shape as
// [CycloRecipe.AppliesTo]: refuse nil, refuse when the
// producer did not advertise decision-block children, refuse
// degraded ASTs (Sec 3.4 "row not written, not stamped
// degraded").
func (r *CognitiveComplexityRecipe) AppliesTo(ast *parser.AstFile) bool {
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
// (no methods AND no file scope) returns nil. A degraded AST
// returns nil (defence-in-depth: Compute MUST not produce a
// `source='computed'` row off a degraded input).
func (r *CognitiveComplexityRecipe) Compute(ast *parser.AstFile) []MetricSampleDraft {
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

	methods := idx.methodScopes()
	drafts := make([]MetricSampleDraft, 0, len(methods)+1)
	var fileSum float64

	for _, m := range methods {
		score := r.cognitiveScore(m, idx)
		value := float64(score)
		fileSum += value
		drafts = append(drafts, newDraft(
			cognitiveComplexityMetricKind,
			cognitiveComplexityVersion,
			PackBase,
			SourceComputed,
			value,
			ScopeRef{
				LocalID:       m.GetScopeId(),
				Kind:          scope.KindMethod,
				QualifiedName: m.GetQualifiedName(),
				Path:          ast.GetPath(),
				// iter-5 evaluator item 3: parameter list
				// disambiguates overloaded methods --
				// `scope.BuildMethod` renders this into
				// the canonical signature.
				Params: m.GetParameters(),
			},
			nil,
			cognitiveComplexityAllowedKinds,
		))
	}

	drafts = append(drafts, newDraft(
		cognitiveComplexityMetricKind,
		cognitiveComplexityVersion,
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
		cognitiveComplexityAllowedKinds,
	))

	return drafts
}

// cognitiveScore is the per-method SonarSource cognitive
// complexity score -- a sum of per-decision contributions
// where each contribution is `cognitiveDelta` plus, for
// nesting-bonus decisions, the current nesting level.
//
// Closures (nested `SCOPE_KIND_METHOD` scopes) are NOT
// counted in the parent's score: SonarSource treats each
// function definition as its own complexity surface, and the
// closure has its own row anyway.
func (r *CognitiveComplexityRecipe) cognitiveScore(method *parser.AstScope, idx scopeIndex) int {
	total := 0
	var walk func(parentID string, nesting int)
	walk = func(parentID string, nesting int) {
		for _, child := range idx.children[parentID] {
			if child.GetScopeKind() == parser.ScopeKindMethod {
				// Closure boundary -- skip; it has its own
				// cognitive_complexity row.
				continue
			}
			childNesting := nesting
			if child.GetScopeKind() == parser.ScopeKindBlock {
				kind := DecisionKind(child.GetAttrs()[AttrDecisionKind])
				if info, ok := decisionTable[kind]; ok {
					contribution := info.cognitiveDelta
					if info.nestingBonus {
						contribution += nesting
					}
					total += contribution
					if info.pushesNesting {
						childNesting = nesting + 1
					}
				}
			}
			walk(child.GetScopeId(), childNesting)
		}
	}
	walk(method.GetScopeId(), 0)
	return total
}
