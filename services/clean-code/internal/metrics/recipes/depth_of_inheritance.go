package recipes

import (
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
)

// depthOfInheritanceMetricKind is the canonical metric_kind
// string for the Depth-of-Inheritance metric (Chidamber &
// Kemerer DIT) -- architecture Sec 1.4.1 row 9 --
// "depth_of_inheritance | class | solid | Length of the
// inheritance chain from this class to the root. Drives LSP /
// composition-over-inheritance rules". Pinned as a const so
// `grep -nF "depth_of_inheritance"` lands one definition site.
// NOT `dit`, `inheritance_depth`, or `class_depth` -- the
// closed-set spelling is exactly `depth_of_inheritance`.
const depthOfInheritanceMetricKind = "depth_of_inheritance"

// depthOfInheritanceVersion is the recipe's `version()` per
// Sec 8.6 line 1010. A bump MUST coincide with a
// `metric_version` bump on every emitted sample (architecture
// C4): definitional changes (e.g. "count multiple-inheritance
// edges as separate branches" or "exclude Object/object root")
// land as a new row at the same `(repo_id, sha, scope_id,
// metric_kind)`.
const depthOfInheritanceVersion = 1

// depthOfInheritanceAllowedKinds is the closed scope_kind set
// the recipe is permitted to emit at, mirroring architecture
// Sec 1.4.1 row 9 column 2 entry `class`. Passed to [newDraft]
// so the per-recipe panic guard refuses any other value --
// including the architectural drift candidates `interface`
// (DIT applies to class implementation hierarchies, not
// interface method-signature hierarchies) and `module`
// (NOT in the canonical 7-enum).
var depthOfInheritanceAllowedKinds = []scope.Kind{scope.KindClass}

// EdgeKindExtends is the canonical [parser.AstEdge.Kind]
// string for an inheritance edge -- one class extends another
// class or interface, or one interface extends another
// interface (per the parser fleet's emission shape; see
// `internal/ast/parser/parser.go:8-18` for the edge-kind
// roster). Pinned here so the depth_of_inheritance and
// coupling_between_objects recipes cite the same literal.
const EdgeKindExtends = "extends"

// EdgeKindImplements is the canonical [parser.AstEdge.Kind]
// string for an implements edge -- one class implements an
// interface. coupling_between_objects treats implements
// edges as outbound coupling targets; depth_of_inheritance
// does NOT consider them as inheritance steps (the DIT
// definition is implementation inheritance only).
const EdgeKindImplements = "implements"

// EdgeKindEmbeds is the canonical [parser.AstEdge.Kind]
// string for an embedding edge -- Go's struct-embedding
// shape. The Go parser emits `embeds` for structs embedding
// other types (see `internal/ast/parser/go.go:181-184`).
// depth_of_inheritance treats embeds as a form of
// implementation inheritance (the embedded type's method
// set promotes to the outer type's method set in Go's
// composition model); coupling_between_objects also counts
// embeds as outbound coupling.
const EdgeKindEmbeds = "embeds"

// EdgeKindImports is the canonical [parser.AstEdge.Kind]
// string for an import edge -- one file/scope imports
// another package or module. coupling_between_objects treats
// imports from within a class scope's subtree as outbound
// coupling targets when the target resolves outside the class
// boundary.
const EdgeKindImports = "imports"

// DepthOfInheritanceRecipe is the DIT recipe for the
// foundation tier (architecture Sec 1.4.1 row 9). It computes
// the length of a class's inheritance chain, the primary
// signal behind LSP (Liskov Substitution) and
// composition-over-inheritance rules.
//
// # Algorithm (per-file, scope-tree only)
//
// For each `SCOPE_KIND_CLASS` scope C in the AST:
//
//  1. Initialise depth = 0.
//
//  2. Follow outbound `extends` and `embeds` edges from C.
//     Each edge contributes ONE level of inheritance:
//
//     - If the edge target resolves to ANOTHER class scope
//       within this file (target id is a known scope id), the
//     recipe recurses into that scope's extends chain
//     (cycle-guarded; an extends cycle is a parser bug but
//     the walk would otherwise loop forever).
//
//     - If the edge target resolves to an EXTERNAL
//     identifier (`qualified:Foo` ref -- the parser's
//     externalScopeRef shape), the recipe adds 1 for the
//     ancestor and stops; we do not have visibility into
//     the external type's own ancestry from a single
//     AstFile, and a future cross-file resolver pass is
//     reserved for Stage 2.5+ (`tech-spec.md` Sec C16).
//
//  3. Take the MAXIMUM depth across all outbound extends/
//     embeds edges -- a class with multiple bases (Python,
//     C++, Scala traits) is at the depth of its deepest
//     ancestor chain.
//
// The implements edge does NOT contribute: per the DIT
// definition (Chidamber & Kemerer 1994), the metric counts
// implementation-inheritance steps only. Interfaces being
// implemented contribute to coupling_between_objects (a
// separate row, separate recipe) but NOT to DIT.
//
// # Per-file semantics and the "scope tree only" pin
//
// The architecture's lit-metric table (Sec 3.3, e2e Sec 4.5,
// tech-spec Sec 4.4) flags depth_of_inheritance as "LIT
// (scope tree only)". The recipe honours this by treating
// every outbound extends/embeds target as at least one
// inheritance step, even when the target is external --
// classes that extend stdlib base classes (`fmt.Stringer`'s
// implementations, Python `object` subclasses, Java classes
// extending external libs) get DIT >= 1 from the per-file
// recipe. That is the AUTHORITATIVE value per Sec 5.2.1
// lines 909-921 G3 (rows immutable once written) for what
// THIS file's edges contributes to the class's DIT.
//
// A future cross-file resolver (Stage 2.5+, tech-spec Sec
// C16) MAY land an enhanced DIT value at a fresh
// `metric_version` for cross-file ancestry tracing; until
// then, this recipe's value is the floor of the true
// inheritance depth (it may understate but cannot overstate
// the true chain length, because every counted step
// corresponds to a real extends/embeds edge the parser
// surfaced).
//
// # Scope kinds (canonical seven-enum)
//
// Drafts emit at `scope.KindClass` only. The [newDraft]
// helper panics on any other value; the per-recipe
// `allowedKinds` slice refuses `interface`, `method`,
// `file`, `package`, `repo`, and `block`.
//
// # Capability + degradation gate
//
// [Recipe.AppliesTo] returns true iff the AST is non-nil and
// NOT degraded. No capability gate is required -- the
// Stage 2.1 parser fleet emits `extends` / `embeds` edges
// from day one (Go: type embedding & interface embedding;
// Java/TS: `extends` keyword; Python: base class declaration).
// This places `depth_of_inheritance` in the "lit" foundation-
// tier set alongside `loc`, `cycle_member`, and
// `interface_width`.
type DepthOfInheritanceRecipe struct{}

// NewDepthOfInheritanceRecipe returns a stateless
// [DepthOfInheritanceRecipe]. Safe for concurrent Compute.
func NewDepthOfInheritanceRecipe() *DepthOfInheritanceRecipe {
	return &DepthOfInheritanceRecipe{}
}

// MetricKind implements [Recipe].
func (r *DepthOfInheritanceRecipe) MetricKind() string { return depthOfInheritanceMetricKind }

// Version implements [Recipe].
func (r *DepthOfInheritanceRecipe) Version() int { return depthOfInheritanceVersion }

// Pack implements [Recipe]. depth_of_inheritance is in the
// `solid` pack (architecture Sec 1.4.1 row 9 -- "Drives LSP /
// composition-over-inheritance rules").
func (r *DepthOfInheritanceRecipe) Pack() Pack { return PackSolid }

// AppliesTo implements [Recipe]. Returns true iff the AST is
// non-nil AND NOT degraded. No capability gate -- extends/
// embeds edges are universally emitted by the Stage 2.1
// parser fleet.
func (r *DepthOfInheritanceRecipe) AppliesTo(ast *parser.AstFile) bool {
	if ast == nil {
		return false
	}
	if ast.GetDegradedReason() != "" {
		return false
	}
	return true
}

// Compute implements [Recipe]. Emits one draft per class
// scope, in source order, whose value is the per-file DIT
// computed via outbound extends/embeds edges. An AST with no
// class scopes returns nil.
//
// The caller MUST gate on
// [DepthOfInheritanceRecipe.AppliesTo] before invoking
// Compute; the recipe ALSO honours the degradation gate as
// defence-in-depth.
func (r *DepthOfInheritanceRecipe) Compute(ast *parser.AstFile) []MetricSampleDraft {
	if ast == nil {
		return nil
	}
	if ast.GetDegradedReason() != "" {
		return nil
	}
	idx := buildIndex(ast)
	classes := idx.scopesByKind(parser.ScopeKindClass)
	if len(classes) == 0 {
		return nil
	}

	// Build a from-scope -> targets index over extends/embeds
	// edges so the per-class chain walk is O(C * avg-chain)
	// rather than re-scanning every edge per class.
	inheritEdges := map[string][]*parser.AstRef{}
	for _, e := range ast.GetEdges() {
		if e == nil {
			continue
		}
		k := e.GetKind()
		if k != EdgeKindExtends && k != EdgeKindEmbeds {
			continue
		}
		from := e.GetFrom()
		if from == nil || from.GetKind() != parser.RefKindScope || from.GetId() == "" {
			continue
		}
		inheritEdges[from.GetId()] = append(inheritEdges[from.GetId()], e.GetTo())
	}

	drafts := make([]MetricSampleDraft, 0, len(classes))
	for _, c := range classes {
		depth := r.depthFrom(c.GetScopeId(), inheritEdges, idx, map[string]bool{})
		drafts = append(drafts, newDraft(
			depthOfInheritanceMetricKind,
			depthOfInheritanceVersion,
			PackSolid,
			SourceComputed,
			float64(depth),
			ScopeRef{
				LocalID:       c.GetScopeId(),
				Kind:          scope.KindClass,
				QualifiedName: c.GetQualifiedName(),
				Path:          ast.GetPath(),
			},
			nil,
			depthOfInheritanceAllowedKinds,
		))
	}
	return drafts
}

// depthFrom returns the maximum inheritance-chain length
// rooted at scope `fromID`. Each outbound extends/embeds
// edge from `fromID` contributes one level; in-file
// resolvable targets recurse; external (`qualified:Foo`)
// targets contribute the single edge step and stop.
//
// `visited` is a per-class-call cycle guard: an extends
// cycle is a parser bug, but the recipe MUST NOT loop
// forever even when fed malformed input. Re-visiting an
// already-walked scope returns 0 for that branch.
func (r *DepthOfInheritanceRecipe) depthFrom(
	fromID string,
	inheritEdges map[string][]*parser.AstRef,
	idx scopeIndex,
	visited map[string]bool,
) int {
	if fromID == "" || visited[fromID] {
		return 0
	}
	visited[fromID] = true
	defer delete(visited, fromID)

	targets := inheritEdges[fromID]
	if len(targets) == 0 {
		return 0
	}

	best := 0
	for _, t := range targets {
		if t == nil {
			continue
		}
		step := 1
		// In-file target -> recurse to follow further extends.
		// External target (`qualified:...`) -> the single step
		// is the recipe's per-file authoritative count; the
		// external chain is invisible.
		if t.GetKind() == parser.RefKindScope {
			if _, ok := idx.byID[t.GetId()]; ok {
				step += r.depthFrom(t.GetId(), inheritEdges, idx, visited)
			}
		}
		if step > best {
			best = step
		}
	}
	return best
}
