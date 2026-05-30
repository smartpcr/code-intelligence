package recipes

import (
	"strings"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
)

// couplingBetweenObjectsMetricKind is the canonical
// metric_kind string for the Chidamber & Kemerer CBO metric
// -- architecture Sec 1.4.1 row 10 -- "coupling_between_objects
// | class | solid | Number of distinct external classes a
// class depends on. Drives DIP / decoupling rules". Pinned
// as a const so a `grep -nF "coupling_between_objects"` lands
// one definition site. NOT `cbo`, `class_coupling`, or
// `external_deps` -- the closed-set spelling is exactly
// `coupling_between_objects`.
const couplingBetweenObjectsMetricKind = "coupling_between_objects"

// couplingBetweenObjectsVersion is the recipe's `version()`
// per Sec 8.6 line 1010. A bump MUST coincide with a
// `metric_version` bump on emitted samples (architecture
// C4): definitional changes (e.g. "count method-level
// granularity" or "weigh by edge multiplicity") land as a
// new row at the same `(repo_id, sha, scope_id,
// metric_kind)`.
const couplingBetweenObjectsVersion = 1

// couplingBetweenObjectsAllowedKinds is the closed scope_kind
// set this recipe is permitted to emit at, mirroring
// architecture Sec 1.4.1 row 10 column 2 entry `class`.
// Passed to [newDraft] so the per-recipe panic guard refuses
// any other value (interface / method / file / package / repo
// / block).
var couplingBetweenObjectsAllowedKinds = []scope.Kind{scope.KindClass}

// CouplingBetweenObjectsRecipe is the CBO recipe for the
// foundation tier (architecture Sec 1.4.1 row 10). It counts
// the distinct external classes / interfaces a given class
// depends on -- the primary signal behind DIP (Dependency
// Inversion) and decoupling rule packs.
//
// # Algorithm (per-file)
//
// For each `SCOPE_KIND_CLASS` scope C in the AST:
//
//  1. Compute C's subtree -- C plus all its descendant
//     scopes (methods, nested blocks, embedded types). See
//     [scopeIndex.subtreeIDs].
//
//  2. Walk every edge in the AST. An edge is INBOUND-FROM-C
//     iff its `from` ref resolves to a scope inside C's
//     subtree. Only the following edge kinds count toward
//     CBO:
//
//     - `extends` (C extends a parent class)
//     - `implements` (C implements an interface)
//     - `embeds` (Go-style struct embedding)
//     - `imports` (a scope inside C imports an external pkg)
//     - `calls` (a method inside C calls another method)
//     - `reads_field` / `writes_field` / `uses_field` (a
//     method inside C accesses an external object's field)
//
//     The `contains` kind is EXCLUDED -- it is a structural
//     edge expressing the scope tree, not a coupling.
//
//  3. The target identifier is collected as a unique key:
//
//     - For an external ref (`qualified:Foo`), the qualified
//     name IS the key -- two distinct outbound edges to
//     `qualified:fmt.Println` count as one external class.
//
//     - For an in-file scope ref, the recipe walks up the
//     scope tree to the nearest class/interface ancestor
//     (a sibling class IS a coupling target; a sibling
//     method on another class is the same coupling target
//     as the class itself). If the resolved class is C
//     itself (or any scope inside C's subtree), the target
//     does NOT count -- self-coupling is excluded by the
//     classical CBO definition.
//
//     - For a symbol ref, the recipe resolves the symbol's
//     `scope_id` first, then applies the in-file-scope
//     rule above.
//
//  4. CBO = size of the unique-target set.
//
// # Partial-coverage note (Stage 2.1 -> Stage 2.2 reality)
//
// `recipe.go` lines 55-122 document that today's Stage 2.1
// parser fleet does NOT yet emit `call_edges`
// (gated by [AttrCallEdges]) or `field_accesses` (gated by
// [AttrFieldAccesses]) for any language. This recipe
// nevertheless runs UNCONDITIONALLY (no capability gate):
//
//   - The architecture's lit-metric matrix (Sec 3.3 / e2e
//     Sec 4.5 / tech-spec Sec 4.4) classifies CBO as
//     "partial (depends on edges)" -- it lights up the
//     extends / implements / embeds / imports subset of edge
//     kinds today, and grows monotonically as the parser
//     fleet adds call/field-access emission.
//
//   - Running without the capability gate is a deliberate
//     correctness choice: a class that ONLY extends and
//     imports (no method bodies that call out) is correctly
//     reported with a positive CBO today; gating on the
//     missing capability would silently emit zero rows for
//     such a file, violating the e2e expectation that "the
//     DIP rule fires on any class that imports / extends
//     three or more external types".
//
//   - The recipe's `metric_version` MUST bump (to 2) when
//     the parser fleet starts emitting `call_edges` or
//     `field_accesses`, because the value definition will
//     drift even though the algorithm is unchanged.
//     [couplingBetweenObjectsVersion] stays at 1 until that
//     pinned bump happens (tech-spec Sec C16).
//
// # Scope kinds (canonical seven-enum)
//
// Drafts emit at `scope.KindClass` only. The [newDraft]
// helper panics on any other value; the per-recipe
// `allowedKinds` slice refuses every other Kind including
// interface (interfaces don't have external dependencies in
// the CBO sense; they ARE the dependency target).
//
// # Capability + degradation gate
//
// [Recipe.AppliesTo] returns true iff the AST is non-nil and
// NOT degraded. No capability gate per the partial-coverage
// note above.
type CouplingBetweenObjectsRecipe struct{}

// NewCouplingBetweenObjectsRecipe returns a stateless
// [CouplingBetweenObjectsRecipe]. Safe for concurrent
// Compute calls.
func NewCouplingBetweenObjectsRecipe() *CouplingBetweenObjectsRecipe {
	return &CouplingBetweenObjectsRecipe{}
}

// MetricKind implements [Recipe].
func (r *CouplingBetweenObjectsRecipe) MetricKind() string {
	return couplingBetweenObjectsMetricKind
}

// Version implements [Recipe].
func (r *CouplingBetweenObjectsRecipe) Version() int { return couplingBetweenObjectsVersion }

// Pack implements [Recipe]. coupling_between_objects is in
// the `solid` pack (architecture Sec 1.4.1 row 10 -- "Drives
// DIP / decoupling rules").
func (r *CouplingBetweenObjectsRecipe) Pack() Pack { return PackSolid }

// AppliesTo implements [Recipe]. Returns true iff the AST is
// non-nil AND NOT degraded. No capability gate -- this
// recipe lights up partial today and grows monotonically as
// the parser fleet adds call/field-access edges (see the
// partial-coverage note on [CouplingBetweenObjectsRecipe]).
func (r *CouplingBetweenObjectsRecipe) AppliesTo(ast *parser.AstFile) bool {
	if ast == nil {
		return false
	}
	if ast.GetDegradedReason() != "" {
		return false
	}
	return true
}

// cboCountedEdgeKinds is the closed set of edge kinds that
// contribute to coupling_between_objects. Pinned as a slice
// of sentinel strings rather than a map literal so a
// `grep -nF EdgeKindExtends` lands one definition site at
// the package boundary, and so the test harness can iterate
// over the exact list to assert each kind is exercised.
var cboCountedEdgeKinds = []string{
	EdgeKindExtends,
	EdgeKindImplements,
	EdgeKindEmbeds,
	EdgeKindImports,
	EdgeKindCalls,
	EdgeKindReadsField,
	EdgeKindWritesField,
	EdgeKindUsesField,
}

// cboCounts reports whether `kind` is in the
// [cboCountedEdgeKinds] set.
func cboCounts(kind string) bool {
	for _, k := range cboCountedEdgeKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// Compute implements [Recipe]. Emits one draft per class
// scope, in source order, whose value is the count of
// distinct external coupling targets reachable from the
// class's subtree via the [cboCountedEdgeKinds] set. An AST
// with no class scopes returns nil.
//
// The caller MUST gate on
// [CouplingBetweenObjectsRecipe.AppliesTo] before invoking
// Compute; the recipe ALSO honours the degradation gate as
// defence-in-depth.
func (r *CouplingBetweenObjectsRecipe) Compute(ast *parser.AstFile) []MetricSampleDraft {
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

	// Pre-bucket edges by the scope-id of their `from`
	// endpoint so the per-class subtree walk is O(C * |subtree|)
	// rather than O(C * |E|).
	edgesByFromScope := map[string][]*parser.AstEdge{}
	for _, e := range ast.GetEdges() {
		if e == nil {
			continue
		}
		if !cboCounts(e.GetKind()) {
			continue
		}
		from := e.GetFrom()
		if from == nil {
			continue
		}
		// Normalise the from-endpoint to a scope id.
		fromScope := idx.refEnclosingScope(from)
		if fromScope == "" {
			continue
		}
		edgesByFromScope[fromScope] = append(edgesByFromScope[fromScope], e)
	}

	drafts := make([]MetricSampleDraft, 0, len(classes))
	for _, c := range classes {
		subtree := idx.subtreeIDs(c.GetScopeId())
		targets := map[string]struct{}{}
		for sid := range subtree {
			for _, e := range edgesByFromScope[sid] {
				key := r.targetKey(e.GetTo(), subtree, idx)
				if key == "" {
					continue
				}
				targets[key] = struct{}{}
			}
		}
		drafts = append(drafts, newDraft(
			couplingBetweenObjectsMetricKind,
			couplingBetweenObjectsVersion,
			PackSolid,
			SourceComputed,
			float64(len(targets)),
			ScopeRef{
				LocalID:       c.GetScopeId(),
				Kind:          scope.KindClass,
				QualifiedName: c.GetQualifiedName(),
				Path:          ast.GetPath(),
			},
			nil,
			couplingBetweenObjectsAllowedKinds,
		))
		// Reset is not needed -- `targets` is a per-class local
		// map; `subtree` likewise.
	}
	return drafts
}

// targetKey reduces an edge's `to` ref to the unique-target
// key used by the unique set:
//
//   - nil / empty-id -> "" (skip)
//   - External `qualified:Foo` ref -> "qualified:Foo"
//     verbatim (two edges to the same external type collapse
//     to one count).
//   - In-file scope ref -> the nearest class/interface
//     ancestor's scope_id. A ref pointing INSIDE the
//     class-under-analysis's own subtree returns "" (self-
//     coupling is excluded by classical CBO).
//   - Symbol ref -> resolve to the owning scope_id, then
//     apply the in-file-scope rule above.
//   - Anything else (unknown ref kind, unresolvable id) ->
//     "" (skip).
func (r *CouplingBetweenObjectsRecipe) targetKey(
	ref *parser.AstRef,
	selfSubtree map[string]bool,
	idx scopeIndex,
) string {
	if ref == nil {
		return ""
	}
	id := ref.GetId()
	if id == "" {
		return ""
	}
	// External reference -- the parser emits
	// `qualified:<name>` for any target outside the current
	// AstFile (see `internal/ast/parser/internal.go:203`).
	if strings.HasPrefix(id, "qualified:") {
		return id
	}
	switch ref.GetKind() {
	case parser.RefKindScope:
		return r.classifyScopeTarget(id, selfSubtree, idx)
	case parser.RefKindSymbol:
		sym, ok := idx.symbolsByID[id]
		if !ok || sym == nil {
			return ""
		}
		owner := sym.GetScopeId()
		if owner == "" {
			return ""
		}
		return r.classifyScopeTarget(owner, selfSubtree, idx)
	default:
		return ""
	}
}

// classifyScopeTarget walks up the scope tree from `id`
// looking for the nearest class/interface ancestor. The
// returned key is that ancestor's scope_id. If the climb
// either (a) returns to a scope inside `selfSubtree`, or (b)
// runs off the top without finding a class/interface, the
// key is "" (skip).
func (r *CouplingBetweenObjectsRecipe) classifyScopeTarget(
	id string,
	selfSubtree map[string]bool,
	idx scopeIndex,
) string {
	if id == "" {
		return ""
	}
	// Self-coupling exclusion: any target inside this class's
	// own subtree (the class itself, a method on it, a nested
	// block, etc.) is not external coupling.
	if selfSubtree[id] {
		return ""
	}
	// Walk up looking for the nearest class/interface
	// ancestor. The scope tree is bounded by the AstFile so
	// the walk terminates either at the file scope (no
	// match -> skip) or at the root (parent_scope_id == "").
	guard := 0
	cur := id
	for cur != "" {
		guard++
		if guard > 256 {
			// Defensive: producer is contractually acyclic,
			// but recipes must not loop forever on malformed
			// input. Sec 4.5 line 488.
			return ""
		}
		s, ok := idx.byID[cur]
		if !ok || s == nil {
			return ""
		}
		k := s.GetScopeKind()
		if k == parser.ScopeKindClass || k == parser.ScopeKindInterface {
			if selfSubtree[cur] {
				return ""
			}
			return cur
		}
		cur = s.GetParentScopeId()
	}
	return ""
}
