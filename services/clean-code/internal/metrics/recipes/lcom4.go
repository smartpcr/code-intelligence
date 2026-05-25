package recipes

import (
	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/scope"
)

// lcom4MetricKind is the canonical metric_kind string for the
// LCOM4 cohesion metric (architecture Sec 1.4.1 row 4 -- "lcom4
// | class | solid | Lack of cohesion of methods (LCOM4
// variant). Drives SRP rule"). Pinned as a const so a
// `grep -nF "lcom4"` lands one definition site; the SRP rule
// pack YAML cites the same literal. NOT `lack_of_cohesion`,
// `lcom`, or `lcom_4` -- the closed-set spelling is exactly
// `lcom4`.
const lcom4MetricKind = "lcom4"

// lcom4Version is the recipe's `version()` per architecture
// Sec 8.6 line 1010. Bumping this MUST coincide with a
// `metric_version` bump on emitted samples (C4) so a
// definitional change (e.g. switching from Hitz & Montazeri's
// LCOM4 to a different cohesion measure) lands as a new row at
// the same `(repo_id, sha, scope_id, metric_kind)` rather than
// silently mutating the persisted value.
const lcom4Version = 1

// LCOM4Recipe is the LCOM4 (Hitz & Montazeri) cohesion recipe
// for the foundation tier (architecture Sec 1.4.1 row 4).
//
// # Algorithm
//
// For each `SCOPE_KIND_CLASS` scope C in the AST:
//
//  1. Collect the class's methods: every `SCOPE_KIND_METHOD`
//     scope whose nearest enclosing class ancestor is C. A
//     closure nested inside a method is NOT a class method
//     (its enclosing class is still C, but the method walker
//     stops at the first method boundary; see
//     `methodsOfClass`).
//
//  2. Collect the class's instance fields: every `AstSymbol`
//     whose `Kind == SymbolKindField` AND whose `ScopeId` is
//     exactly C (NOT a descendant of C). A symbol declared
//     inside a method body is a local variable, NOT an
//     instance field, even if its `Kind` is mis-stamped as
//     `"field"`.
//
//  3. Build a connectivity graph on the method set:
//
//     - Two methods M1, M2 are connected if there exists an
//     instance field F of C such that BOTH M1 and M2 access
//     F. "Access" means an `AstEdge` whose `kind` is one of
//     [EdgeKindReadsField], [EdgeKindWritesField],
//     [EdgeKindUsesField] AND whose `from` resolves into the
//     method's subtree AND whose `to` resolves to F.
//
//     - Two methods M1, M2 are ALSO connected if there is a
//     `calls` edge between them within C (M1 calls M2 or
//     vice versa). Classical LCOM4 (Hitz & Montazeri, 1995)
//     uses BOTH shared-field-access AND intra-class method
//     invocation as connectivity edges; omitting the latter
//     would inflate LCOM4 for classes whose methods compose
//     via delegation rather than shared state.
//
//  4. LCOM4(C) = number of connected components in the
//     resulting undirected graph.
//
// Edge cases:
//
//   - 0 methods in C → emit `value = 0`. A class with no
//     methods has no cohesion to measure; the architecture
//     does NOT pin a sentinel for this case, but emitting
//     `value=0` is consistent with classical formulations
//     (no methods → no disjoint method clusters).
//   - 1 method in C → emit `value = 1` (the single method is
//     its own component).
//   - All methods share at least one field (or are
//     transitively connected via shared fields / intra-class
//     calls) → emit `value = 1` (maximum cohesion).
//   - No methods share any field AND no methods call each
//     other → emit `value = N` (where N is the method count;
//     maximum lack-of-cohesion).
//
// # Scope kind (canonical seven-enum)
//
// Drafts emit at `scope.KindClass` only (architecture Sec
// 1.4.1 row 4 column 2 entry `class`). The [newDraft] helper
// panics on any other value; the helper's per-recipe
// `allowedKinds` slice ([lcom4AllowedKinds]) refuses
// `interface` and the rest of the canonical 7-enum.
//
// # Capability + degradation gate
//
// [Recipe.AppliesTo] returns true iff the AST is non-nil,
// NOT degraded, AND the producer stamped BOTH
// [AttrFieldAccesses] = "true" AND [AttrCallEdges] = "true"
// on the file-level attrs. BOTH capabilities are required
// because the classical LCOM4 algorithm (Hitz & Montazeri)
// treats two methods as connected if EITHER they share a
// field OR one calls the other -- a producer that emits
// field accesses but NOT call edges can only see half the
// connectivity graph and would silently under-report
// cohesion on delegation-heavy classes (a Facade whose
// methods chain other methods without ever touching fields
// would look maximally non-cohesive at LCOM4 = method
// count). Symmetrically, a producer with call edges but no
// field accesses would over-report (no field-share
// connectivity at all).
//
// The Stage 2.1 parser fleet does NOT yet emit field-access
// edges OR call edges (it emits `imports`, `extends`,
// `implements`, `contains`), so production scans currently
// produce zero LCOM4 drafts; this is the correct behaviour
// per architecture Sec 3.4 lines 490-494 ("Computed rows are
// never `degraded=true`: if an input is missing the row is
// not written, not stamped degraded"). When a future parser
// stage emits `reads_field` / `writes_field` / `uses_field`
// edges AND `calls` edges AND stamps both
// `field_accesses="true"` AND `call_edges="true"`, this
// recipe lights up automatically.
type LCOM4Recipe struct{}

// lcom4AllowedKinds is the closed scope_kind set the recipe
// is permitted to emit at, mirroring architecture Sec 1.4.1
// row 4 column 2 entry `class`. Passed to [newDraft] so the
// helper's per-recipe guard refuses any other value at the
// panic boundary -- particularly the architectural drift
// candidates `interface` (LCOM4 is a CLASS-level metric;
// interfaces have no method bodies to share state through)
// and `module` (NOT in the canonical 7-enum).
var lcom4AllowedKinds = []scope.Kind{scope.KindClass}

// NewLCOM4Recipe returns a stateless [LCOM4Recipe]. Safe for
// concurrent Compute calls.
func NewLCOM4Recipe() *LCOM4Recipe { return &LCOM4Recipe{} }

// MetricKind implements [Recipe].
func (r *LCOM4Recipe) MetricKind() string { return lcom4MetricKind }

// Version implements [Recipe].
func (r *LCOM4Recipe) Version() int { return lcom4Version }

// Pack implements [Recipe]. lcom4 is in the `solid` pack
// (architecture Sec 1.4.1 row 4 -- "SOLID / SRP cohesion").
func (r *LCOM4Recipe) Pack() Pack { return PackSolid }

// AppliesTo implements [Recipe]. Returns true iff the AST is
// non-nil, NOT degraded, AND the producer advertised BOTH
// the `field_accesses` AND `call_edges` capabilities via
// [AttrFieldAccesses] and [AttrCallEdges].
//
// BOTH capabilities are required because the LCOM4 cohesion
// algorithm (Hitz & Montazeri) defines two methods of a
// class as "connected" if EITHER they touch a shared field
// OR one calls the other (see [LCOM4Recipe] doc "Algorithm").
// A producer that emits field accesses but NOT call edges
// can only see HALF of the connectivity graph: delegation-
// heavy classes (e.g. a Facade whose methods compose by
// chaining other methods, never touching fields) would
// appear maximally non-cohesive (LCOM4 = method count) even
// when they ARE cohesive by the algorithm. Better to skip
// entirely than emit a `source='computed'` row that lies.
//
// The non-degraded check realises architecture Sec 3.4 lines
// 490-494 ("computed rows are never degraded=true"): a
// degraded AST means the parser had to bail mid-file, and
// emitting a `source='computed'` LCOM4 row off truncated
// scope data would silently lie about cohesion.
func (r *LCOM4Recipe) AppliesTo(ast *parser.AstFile) bool {
	if !hasFieldAccessesCapability(ast) {
		return false
	}
	if !hasCallEdgesCapability(ast) {
		return false
	}
	if ast.GetDegradedReason() != "" {
		return false
	}
	return true
}

// Compute implements [Recipe].
//
// Returns one draft per class scope in source order. An AST
// with no class scopes returns nil. A degraded AST (or one
// without the field-accesses + call-edges capabilities)
// returns nil -- defence-in-depth against a caller that
// forgot to gate on [LCOM4Recipe.AppliesTo]. BOTH
// capabilities are required: see [LCOM4Recipe.AppliesTo] doc
// for why a producer with field accesses but no call edges
// would silently under-report cohesion on delegation-heavy
// classes.
func (r *LCOM4Recipe) Compute(ast *parser.AstFile) []MetricSampleDraft {
	if ast == nil {
		return nil
	}
	if ast.GetDegradedReason() != "" {
		return nil
	}
	if !hasFieldAccessesCapability(ast) {
		return nil
	}
	if !hasCallEdgesCapability(ast) {
		return nil
	}
	idx := buildIndex(ast)
	classes := idx.scopesByKind(parser.ScopeKindClass)
	if len(classes) == 0 {
		return nil
	}

	drafts := make([]MetricSampleDraft, 0, len(classes))
	for _, c := range classes {
		value := r.computeLCOM4(c, idx, ast)
		drafts = append(drafts, newDraft(
			lcom4MetricKind,
			lcom4Version,
			PackSolid,
			SourceComputed,
			float64(value),
			ScopeRef{
				LocalID:       c.GetScopeId(),
				Kind:          scope.KindClass,
				QualifiedName: c.GetQualifiedName(),
				Path:          ast.GetPath(),
			},
			nil,
			lcom4AllowedKinds,
		))
	}
	return drafts
}

// computeLCOM4 returns the number of connected components in
// the methods-of-class connectivity graph for class `c`. See
// [LCOM4Recipe] doc "Algorithm" for the precise definition.
func (r *LCOM4Recipe) computeLCOM4(c *parser.AstScope, idx scopeIndex, ast *parser.AstFile) int {
	classID := c.GetScopeId()
	methods := r.methodsOfClass(classID, idx)
	if len(methods) == 0 {
		return 0
	}
	if len(methods) == 1 {
		return 1
	}

	// Index method scope_ids for O(1) "is this scope a class
	// method?" lookups, AND for each method record the set of
	// scope_ids in its subtree so an edge whose `from`
	// resolves into a nested block / closure inside a method
	// still attributes to the correct method.
	methodIDs := make(map[string]int, len(methods)) // scope_id -> method index
	methodSubtrees := make([]map[string]bool, len(methods))
	for i, m := range methods {
		methodIDs[m.GetScopeId()] = i
		methodSubtrees[i] = idx.subtreeIDs(m.GetScopeId())
	}

	// Class-level instance fields: AstSymbol with Kind="field"
	// AND ScopeId == classID. Symbols declared INSIDE a
	// method body (locals) MUST be excluded; the ScopeId
	// match handles that because a local's ScopeId is the
	// method scope, not the class.
	classFields := make(map[string]bool)
	for _, sym := range ast.GetSymbols() {
		if sym == nil {
			continue
		}
		if sym.GetKind() != SymbolKindField {
			continue
		}
		if sym.GetScopeId() != classID {
			continue
		}
		classFields[sym.GetSymbolId()] = true
	}

	// Per-method field-access set.
	methodFields := make([]map[string]bool, len(methods))
	for i := range methodFields {
		methodFields[i] = map[string]bool{}
	}

	// Walk edges once:
	//   - field-access edge -> record method i accesses field F
	//   - calls edge -> if both endpoints resolve to methods
	//     of this class, union them.
	uf := newUnionFind(len(methods))
	for _, e := range ast.GetEdges() {
		if e == nil {
			continue
		}
		switch e.GetKind() {
		case EdgeKindReadsField, EdgeKindWritesField, EdgeKindUsesField:
			fromMethod := r.methodIndexFromRef(e.GetFrom(), methodIDs, methodSubtrees, idx)
			if fromMethod < 0 {
				continue
			}
			to := e.GetTo()
			if to == nil || to.GetKind() != parser.RefKindSymbol {
				continue
			}
			if !classFields[to.GetId()] {
				continue
			}
			methodFields[fromMethod][to.GetId()] = true
		case EdgeKindCalls:
			fromMethod := r.methodIndexFromRef(e.GetFrom(), methodIDs, methodSubtrees, idx)
			if fromMethod < 0 {
				continue
			}
			toMethod := r.methodIndexFromRef(e.GetTo(), methodIDs, methodSubtrees, idx)
			if toMethod < 0 {
				continue
			}
			if fromMethod == toMethod {
				continue
			}
			uf.Union(fromMethod, toMethod)
		}
	}

	// Pairs of methods sharing at least one class-field get
	// unioned. The O(F * M^2) shape is fine for class-scale
	// inputs (typical classes have <30 methods and <20
	// fields); a smarter inverted index is overkill here and
	// would risk introducing nondeterminism in the iteration
	// order.
	for fieldID := range classFields {
		var seed int = -1
		for i := 0; i < len(methods); i++ {
			if !methodFields[i][fieldID] {
				continue
			}
			if seed < 0 {
				seed = i
				continue
			}
			uf.Union(seed, i)
		}
	}

	return uf.ComponentCount()
}

// methodsOfClass returns the method scopes whose NEAREST
// enclosing class ancestor (per `parent_scope_id` walk) is
// `classID`. A method nested inside another method (a closure)
// is NOT counted here because its nearest enclosing class is
// still the outer class, BUT lcom4 treats it as part of the
// outer method's body (the outer method's subtree includes
// the closure's edges).
//
// To distinguish those cases, the walk stops at boundaries:
//
//   - If the walk reaches a method scope BEFORE the class
//     scope, the candidate is a closure / nested function;
//     skip it.
//   - If the walk reaches a class or interface scope whose
//     id is NOT `classID` (e.g. an inner class declared
//     inside `classID`'s body, or `classID` is itself inner
//     to some other class), the candidate is owned by that
//     OTHER class/interface — its nearest-enclosing-class
//     is the inner one, NOT `classID`. Skip it. This is
//     load-bearing for Java/Kotlin/Scala/C# where nested
//     class declarations are common (see
//     `internal/ast/parser/tree_sitter_java.go:286-287`
//     where `class_declaration` / `record_declaration` /
//     `enum_declaration` / `interface_declaration` nodes
//     in a class body emit nested class scopes whose
//     `parent_scope_id` is the outer class). Without this
//     boundary, `Outer.methodsOfClass(Outer)` would walk
//     `Inner.m1.parent = Inner` past `Inner` up to `Outer`
//     and incorrectly attribute `Inner.m1` to `Outer`,
//     inflating Outer's cohesion measurement.
//   - If the walk reaches `classID` directly OR via
//     non-method, non-class intermediaries (block / file),
//     the candidate IS a class method; include it.
func (r *LCOM4Recipe) methodsOfClass(classID string, idx scopeIndex) []*parser.AstScope {
	out := make([]*parser.AstScope, 0)
	for _, s := range idx.scopesByKind(parser.ScopeKindMethod) {
		seen := map[string]bool{}
		cur := s.GetParentScopeId()
		isClassMethod := false
		for cur != "" {
			if seen[cur] {
				break
			}
			seen[cur] = true
			parent := idx.byID[cur]
			if parent == nil {
				break
			}
			if parent.GetScopeKind() == parser.ScopeKindMethod {
				// Closure boundary -- the candidate is nested
				// inside another method, so it is NOT a
				// direct class method.
				break
			}
			if cur == classID {
				isClassMethod = true
				break
			}
			if parent.GetScopeKind() == parser.ScopeKindClass ||
				parent.GetScopeKind() == parser.ScopeKindInterface {
				// Nested-class boundary -- we hit a class
				// or interface that is NOT `classID`, so the
				// method's nearest-enclosing-class is this
				// inner class/interface, not `classID`.
				// Stop walking; do NOT attribute to classID.
				break
			}
			cur = parent.GetParentScopeId()
		}
		if isClassMethod {
			out = append(out, s)
		}
	}
	return out
}

// methodIndexFromRef resolves an [parser.AstRef] to a method
// index in `methods`. The resolution chain:
//
//  1. If ref is nil / empty -> -1.
//  2. If ref is a SCOPE ref pointing at a method in the class
//     -> that method's index.
//  3. If ref is a SCOPE ref pointing at a non-method scope
//     -> walk parents to find a method-of-class ancestor.
//  4. If ref is a SYMBOL ref -> resolve the symbol to its
//     declaring scope (`sym.scope_id`), then walk parents
//     to find a method-of-class ancestor.
//
// Returns -1 when no method ancestor is found (edge originates
// outside any class method, e.g. from a file-level expression
// or a different class's method).
func (r *LCOM4Recipe) methodIndexFromRef(ref *parser.AstRef, methodIDs map[string]int, methodSubtrees []map[string]bool, idx scopeIndex) int {
	startScope := idx.refEnclosingScope(ref)
	if startScope == "" {
		return -1
	}
	// Fast path: if startScope is exactly a class method, done.
	if i, ok := methodIDs[startScope]; ok {
		return i
	}
	// Slow path: walk parents to find a method-of-class
	// ancestor; check membership against each method's
	// subtree.
	for i, sub := range methodSubtrees {
		if sub[startScope] {
			return i
		}
	}
	return -1
}

// unionFind is a tiny disjoint-set-union with path compression
// and rank heuristic. n is fixed at construction; supports
// [unionFind.Union] and [unionFind.ComponentCount].
type unionFind struct {
	parent []int
	rank   []int
}

// newUnionFind returns a forest of n singleton components.
func newUnionFind(n int) *unionFind {
	uf := &unionFind{parent: make([]int, n), rank: make([]int, n)}
	for i := 0; i < n; i++ {
		uf.parent[i] = i
	}
	return uf
}

// Find returns the representative of i's component, with
// path compression.
func (uf *unionFind) Find(i int) int {
	for uf.parent[i] != i {
		uf.parent[i] = uf.parent[uf.parent[i]]
		i = uf.parent[i]
	}
	return i
}

// Union merges the components containing i and j.
func (uf *unionFind) Union(i, j int) {
	ri := uf.Find(i)
	rj := uf.Find(j)
	if ri == rj {
		return
	}
	if uf.rank[ri] < uf.rank[rj] {
		uf.parent[ri] = rj
	} else if uf.rank[ri] > uf.rank[rj] {
		uf.parent[rj] = ri
	} else {
		uf.parent[rj] = ri
		uf.rank[ri]++
	}
}

// ComponentCount returns the number of distinct components.
func (uf *unionFind) ComponentCount() int {
	seen := map[int]bool{}
	for i := range uf.parent {
		seen[uf.Find(i)] = true
	}
	return len(seen)
}
