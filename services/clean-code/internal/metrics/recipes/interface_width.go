package recipes

import (
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
)

// interfaceWidthMetricKind is the canonical metric_kind
// string for the type's-method-count (Chidamber & Kemerer
// "Weighted Methods per Class" variant) -- architecture Sec
// 1.4.1 row 8 -- "interface_width | class, interface | solid
// | Method count of a class/interface's exposed surface.
// Drives ISP rule". Pinned as a const so a
// `grep -nF "interface_width"` lands one definition site.
// NOT `class_width`, `method_count`, or `wmc` -- the
// closed-set spelling is exactly `interface_width`.
const interfaceWidthMetricKind = "interface_width"

// interfaceWidthVersion is the recipe's `version()` per Sec
// 8.6 line 1010. A bump MUST coincide with a `metric_version`
// bump on emitted samples (architecture C4): definitional
// changes (e.g. "exclude private members" -- the current v1
// counts EVERY direct method child; a future v2 may filter on
// the parser-stamped visibility attrs) land as a new row at
// the same `(repo_id, sha, scope_id, metric_kind)`.
const interfaceWidthVersion = 1

// interfaceWidthAllowedKinds is the closed scope_kind set
// this recipe is permitted to emit at, mirroring architecture
// Sec 1.4.1 row 8 column 2 entry `class, interface`. Passed
// to [newDraft] so the per-recipe panic guard refuses any
// other value -- particularly the architectural drift
// candidates `module` (NOT in the canonical 7-enum), `repo`,
// or `method` (interface_width is a CONTAINER metric; method-
// scope rows would be nonsense).
var interfaceWidthAllowedKinds = []scope.Kind{scope.KindClass, scope.KindInterface}

// InterfaceWidthRecipe is the type-method-count recipe for the
// foundation tier (architecture Sec 1.4.1 row 8). It computes
// the size of a class or interface's exposed method surface
// -- the primary input to the ISP (Interface Segregation
// Principle) rule pack.
//
// # Algorithm (per-file)
//
// For each `SCOPE_KIND_CLASS` or `SCOPE_KIND_INTERFACE` scope
// T in the AST, the recipe emits ONE draft whose `value` is
// the count of `SCOPE_KIND_METHOD` scopes whose
// `parent_scope_id` equals T's scope id. Closures nested
// inside a method body are NOT counted because their
// `parent_scope_id` is the enclosing method, not T.
//
// Visibility filtering (the brief's "public method count"
// note) is DEFERRED to v2: the canonical seven-enum and the
// per-language parser fleet do not yet expose a uniform
// `visibility` attr, and the foundation-tier contract says
// definitional changes MUST bump `metric_version` rather than
// silently mutating values. v1 counts every direct method
// child of the type; the v1 value is therefore the
// AUTHORITATIVE upper bound on the type's public surface --
// downstream rule packs that need a stricter "public-only"
// reading can join against the per-method visibility attrs
// the parser stamps (`public`, `private`, `protected`,
// `static`) without re-running the recipe.
//
// # Scope kinds (canonical seven-enum)
//
// Drafts emit at `scope.KindClass` and `scope.KindInterface`
// only. The [newDraft] helper panics on any other value;
// the per-recipe `allowedKinds` slice [interfaceWidthAllowedKinds]
// refuses `method`, `file`, `package`, `repo`, and `block`.
//
// # Capability + degradation gate
//
// [Recipe.AppliesTo] returns true iff the AST is non-nil and
// NOT degraded. Unlike the call-graph and field-access
// metrics, this recipe needs NO capability attr -- it walks
// the scope tree only, which every Stage 2.1 parser populates
// from day one. This places `interface_width` in the "lit"
// foundation-tier set (architecture Sec 3.3 lit-metric table)
// alongside `loc`, `cycle_member`, and `depth_of_inheritance`.
//
// The non-degraded check realises Sec 3.4 lines 490-494: a
// degraded AST means the parser bailed mid-file, and a
// truncated scope tree would silently undercount methods.
// Better to skip emission than land a misleading row.
type InterfaceWidthRecipe struct{}

// NewInterfaceWidthRecipe returns a stateless
// [InterfaceWidthRecipe]. Safe for concurrent Compute calls.
func NewInterfaceWidthRecipe() *InterfaceWidthRecipe { return &InterfaceWidthRecipe{} }

// MetricKind implements [Recipe].
func (r *InterfaceWidthRecipe) MetricKind() string { return interfaceWidthMetricKind }

// Version implements [Recipe].
func (r *InterfaceWidthRecipe) Version() int { return interfaceWidthVersion }

// Pack implements [Recipe]. interface_width is in the `solid`
// pack (architecture Sec 1.4.1 row 8 -- "Drives ISP rule").
func (r *InterfaceWidthRecipe) Pack() Pack { return PackSolid }

// AppliesTo implements [Recipe]. Returns true iff the AST is
// non-nil AND NOT degraded. No capability gate -- the scope
// tree is universally populated.
func (r *InterfaceWidthRecipe) AppliesTo(ast *parser.AstFile) bool {
	if ast == nil {
		return false
	}
	if ast.GetDegradedReason() != "" {
		return false
	}
	return true
}

// Compute implements [Recipe]. Emits one draft per class /
// interface scope, in source order, whose value is the count
// of direct method children. An AST with no class or
// interface scopes returns nil.
//
// The caller MUST gate on [InterfaceWidthRecipe.AppliesTo]
// before invoking Compute; the recipe ALSO honours the
// degradation gate as defence-in-depth.
func (r *InterfaceWidthRecipe) Compute(ast *parser.AstFile) []MetricSampleDraft {
	if ast == nil {
		return nil
	}
	if ast.GetDegradedReason() != "" {
		return nil
	}
	idx := buildIndex(ast)

	classes := idx.scopesByKind(parser.ScopeKindClass)
	interfaces := idx.scopesByKind(parser.ScopeKindInterface)
	if len(classes)+len(interfaces) == 0 {
		return nil
	}

	drafts := make([]MetricSampleDraft, 0, len(classes)+len(interfaces))
	for _, c := range classes {
		drafts = append(drafts, r.emit(ast, c, scope.KindClass, idx))
	}
	for _, ifc := range interfaces {
		drafts = append(drafts, r.emit(ast, ifc, scope.KindInterface, idx))
	}
	return drafts
}

// emit builds one draft for a class/interface scope T whose
// value is the count of direct method children (children
// whose scope_kind is method AND whose parent_scope_id is
// T's scope id). Closures nested deeper than one level (i.e.
// methods whose nearest enclosing scope is another method,
// not T) are NOT counted because their direct parent is the
// method scope, not T.
func (r *InterfaceWidthRecipe) emit(ast *parser.AstFile, t *parser.AstScope, kind scope.Kind, idx scopeIndex) MetricSampleDraft {
	count := 0
	for _, child := range idx.children[t.GetScopeId()] {
		if child == nil {
			continue
		}
		if child.GetScopeKind() == parser.ScopeKindMethod {
			count++
		}
	}
	return newDraft(
		interfaceWidthMetricKind,
		interfaceWidthVersion,
		PackSolid,
		SourceComputed,
		float64(count),
		ScopeRef{
			LocalID:       t.GetScopeId(),
			Kind:          kind,
			QualifiedName: t.GetQualifiedName(),
			Path:          ast.GetPath(),
		},
		nil,
		interfaceWidthAllowedKinds,
	)
}
