package recipes

import (
	"forge/services/clean-code/internal/ast/parser"
	"forge/services/clean-code/internal/ast/scope"
)

// fanInMetricKind is the canonical metric_kind string for the
// INBOUND coupling metric (architecture Sec 1.4.1 row 5 --
// "fan_in | method, class, file | solid | Inbound references
// to a scope. Drives OCP, DIP rules"). Pinned as a const so
// `grep -nF "fan_in"` lands one definition site. NOT `fanin`,
// `fan-in`, or `incoming_calls` -- the closed-set spelling is
// exactly `fan_in`.
const fanInMetricKind = "fan_in"

// fanInVersion is the recipe's `version()` per Sec 8.6 line
// 1010. A bump MUST coincide with a `metric_version` bump on
// every emitted sample (architecture C4) so a definitional
// change (e.g. switching from "call sites" to "distinct
// callers") lands as a new row at the same `(repo_id, sha,
// scope_id, metric_kind)`.
const fanInVersion = 1

// FanInRecipe is the inbound-coupling recipe for the
// foundation tier (architecture Sec 1.4.1 row 5).
//
// # Algorithm
//
// Fan-in at a scope S = the number of INCOMING `calls` edges
// in this file's edge list whose `to` endpoint resolves into
// S's subtree AND whose `from` endpoint resolves OUTSIDE S's
// subtree:
//
//	fan_in(S) = |{e in ast.Edges : e.kind == "calls" AND
//	    enclosing_scope(e.to) in subtree(S) AND
//	    enclosing_scope(e.from) NOT in subtree(S)}|
//
// Self-calls (recursion) and intra-subtree calls do NOT
// count: a method that calls itself, or a class whose method
// calls another method of the same class, contributes zero
// to the CLASS-level fan_in for that edge.
//
// # Scope kinds (canonical seven-enum) and emission contract
//
// Drafts emit at `scope.KindMethod`, `scope.KindClass`, AND
// `scope.KindFile` per architecture Sec 1.4.1 row 5 column 2
// entry `method, class, file`. All three are in the closed
// applicability set AND are directly emitted by this per-file
// recipe. NEVER `function`, `module`, `namespace`, `package`,
// `repo`, `interface`, or `block` -- the [newDraft] helper
// panics on any out-of-set value.
//
// # Authoritative per-parser semantics
//
// Per architecture Sec 5.2.1 lines 909-921 (`MetricSample`
// row immutability, G3) and Sec 9 lines 1530-1538 (writer
// table -- the Cross-Repo Aggregator only writes
// `pack='system'`, `source='derived'` rows), the
// foundation-tier fan_in row written by this recipe at SHA X
// for file F is the AUTHORITATIVE row at that
// `(repo_id, sha, scope_id, metric_kind='fan_in',
// metric_version=1)` quintuple FOREVER. The
// `pack='solid', source='computed'` rows this recipe emits
// are never updated and never re-written at the same SHA
// (G3); a corrected count for the same scope lands at a
// NEW SHA as a fresh row.
//
// What this means for file-scope fan_in: the row counts
// EXACTLY the inbound `calls` edges THE PARSER SURFACED in
// `AstFile.Edges` at parse time -- nothing more, nothing
// less. The semantic is "the producer that stamped
// `AttrCallEdges` advertises that the `calls` edges in this
// file are complete enough for fan_in"; if a producer's
// resolver visits cross-file references and emits inbound-
// to-this-file edges (i.e. edges whose `from` is an
// unresolved-in-this-file ref but whose `to` is a local
// scope), those edges count toward the file-scope row. If a
// producer only emits outbound-from-locals (Stage 2.1
// shape), every edge has `from` resolving locally -- the
// per-file file-scope fan_in evaluates to 0, and that 0 is
// the AUTHORITATIVE measurement of what THIS file's parsed
// edge list contributes to the file-scope inbound count.
//
// # Capability gate is the producer's contract knob
//
// The `call_edges` capability flag ([AttrCallEdges]) is the
// producer's compact way to say "the `calls` edges in this
// file are the complete inbound-and-outbound call surface I
// resolved." A producer that emits only outbound calls MAY
// still stamp `call_edges="true"` -- the file-scope fan_in
// row will simply be 0 by construction (which is what the
// architecture's "no incoming call site this parser saw"
// reading means). A producer that withholds the stamp causes
// the recipe to skip emission entirely (Sec 3.4 "row not
// written, not stamped degraded"), avoiding a misleading
// fan_in=0 row from a producer that doesn't model calls at
// all.
//
// # Capability + degradation gate
//
// [Recipe.AppliesTo] returns true iff the AST is non-nil,
// NOT degraded, AND the producer stamped [AttrCallEdges] =
// "true" on the file-level attrs. The Stage 2.1 parser fleet
// does NOT yet emit `calls` edges (it emits `imports`,
// `extends`, `implements`, `contains`), so production scans
// currently produce zero fan_in drafts; this matches
// architecture Sec 3.4 lines 490-494 ("Computed rows are
// never `degraded=true`: if an input is missing the row is
// not written, not stamped degraded"). When a future parser
// stage emits `calls` and stamps `call_edges="true"`, this
// recipe lights up automatically.
type FanInRecipe struct{}

// fanInAllowedKinds is the METRIC_KIND APPLICABILITY SET --
// the closed scope_kind set the SCHEMA accepts for a
// persisted `MetricSample(metric_kind='fan_in')` row,
// mirroring architecture Sec 1.4.1 row 5 column 2 entry
// `method, class, file`. NOT `module`, `package`, `repo`,
// `function`, or `interface` -- the row's applicability is
// pinned to the three call-target-host kinds.
//
// Identical to [fanInDirectlyEmittedKinds]: per architecture
// Sec 1.4.1 row 5 the per-file recipe MUST emit at every
// kind in the applicability set. Each emitted row is the
// authoritative computed-tier value at its SHA (`source=
// 'computed'`, immutable per G3); see [FanInRecipe] doc
// "Authoritative per-parser semantics" for the contract.
var fanInAllowedKinds = []scope.Kind{scope.KindMethod, scope.KindClass, scope.KindFile}

// fanInDirectlyEmittedKinds is the scope_kind set this
// per-file recipe directly emits drafts at. For fan_in this
// is IDENTICAL to [fanInAllowedKinds] per architecture
// Sec 1.4.1 row 5 (`method, class, file` -- ALL three are
// pinned for emission). Each emitted row is the
// authoritative computed-tier count of inbound `calls`
// edges the producer surfaced in `ast.Edges` at this SHA.
// See [FanInRecipe] doc "Authoritative per-parser
// semantics" -- per architecture Sec 5.2.1 G3 the row is
// immutable once written; a corrected count lands at a NEW
// SHA.
var fanInDirectlyEmittedKinds = fanInAllowedKinds

// fanInCallEndpoints is a (from-enclosing-scope,
// to-enclosing-scope) pair derived from one `calls` edge.
// Cached once per Compute call so the per-scope fan_in count
// is O(edges) without re-resolving refs.
type fanInCallEndpoints struct {
	from string
	to   string
}

// NewFanInRecipe returns a stateless [FanInRecipe]. Safe for
// concurrent Compute calls.
func NewFanInRecipe() *FanInRecipe { return &FanInRecipe{} }

// MetricKind implements [Recipe].
func (r *FanInRecipe) MetricKind() string { return fanInMetricKind }

// Version implements [Recipe].
func (r *FanInRecipe) Version() int { return fanInVersion }

// Pack implements [Recipe]. fan_in is in the `solid` pack
// (architecture Sec 1.4.1 row 5).
func (r *FanInRecipe) Pack() Pack { return PackSolid }

// AppliesTo implements [Recipe]. Returns true iff the AST is
// non-nil, NOT degraded, AND the producer advertised the
// `call_edges` capability via [AttrCallEdges].
//
// The non-degraded check realises architecture Sec 3.4 lines
// 490-494 ("computed rows are never degraded=true"): a
// degraded AST means the parser had to bail mid-file, and
// emitting a `source='computed'` fan_in row off truncated
// call-graph data would silently lie about coupling.
func (r *FanInRecipe) AppliesTo(ast *parser.AstFile) bool {
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
// Emits one draft per `(method | class | file)` scope in
// source order, per architecture Sec 1.4.1 row 5. Every
// emitted row is the AUTHORITATIVE computed-tier value at
// its `(repo_id, sha, scope_id, metric_kind='fan_in',
// metric_version=1)` quintuple: the value is the exact
// count of inbound `calls` edges in `ast.Edges` that this
// producer surfaced. Per architecture Sec 5.2.1 lines
// 909-921 G3 the row is immutable once written; a
// corrected count for the same scope lands at a NEW SHA as
// a fresh row.
//
// The file-scope row's value is determined entirely by what
// the producer chose to put in `ast.Edges`:
//
//   - A producer whose resolver visits cross-file refs and
//     surfaces `calls` edges with `from` external + `to`
//     local in THIS file's edge list: the file-scope row's
//     value is exactly the count of such edges.
//   - A producer that surfaces only outbound-from-local
//     edges (Stage 2.1 shape; every edge's `from` resolves
//     to a local scope): the file-scope row's value is 0 by
//     construction. That 0 IS the authoritative value of
//     what this producer can say about file-scope inbound
//     calls at this SHA.
//
// A degraded AST or one without the `call_edges` capability
// returns nil -- defence-in-depth against a caller that
// forgot to gate on [FanInRecipe.AppliesTo].
//
// # Per-scope value computation
//
// The recipe walks `ast.Edges` ONCE, filtering by
// `kind == "calls"`, and resolves both endpoints to enclosing
// scope_ids via [scopeIndex.refEnclosingScope]. Per emit-
// scope S the recipe counts edges whose `to` is in S's
// subtree AND whose `from` is NOT in S's subtree. At the
// FILE scope the `from NOT in subtree` test reduces to
// "from is external to this AstFile" (an unresolvable ref or
// a ref to a symbol/scope outside the file's index).
//
// The `allowedKinds` slice passed to [newDraft] is
// [fanInDirectlyEmittedKinds] (`{method, class, file}`),
// identical to [fanInAllowedKinds] -- per architecture
// Sec 1.4.1 row 5 the per-file recipe MUST emit at every
// pinned scope_kind.
func (r *FanInRecipe) Compute(ast *parser.AstFile) []MetricSampleDraft {
	if ast == nil {
		return nil
	}
	if ast.GetDegradedReason() != "" {
		return nil
	}
	if !hasCallEdgesCapability(ast) {
		return nil
	}
	idx := buildIndex(ast)
	if idx.file == nil {
		return nil
	}

	// Materialise the call-edge endpoint pairs ONCE.
	callEdges := make([]fanInCallEndpoints, 0, len(ast.GetEdges()))
	for _, e := range ast.GetEdges() {
		if e == nil || e.GetKind() != EdgeKindCalls {
			continue
		}
		callEdges = append(callEdges, fanInCallEndpoints{
			from: idx.refEnclosingScope(e.GetFrom()),
			to:   idx.refEnclosingScope(e.GetTo()),
		})
	}

	drafts := make([]MetricSampleDraft, 0)

	// METHOD scope -- one row per method.
	for _, m := range idx.scopesByKind(parser.ScopeKindMethod) {
		drafts = append(drafts, r.emit(ast, m, scope.KindMethod, idx, callEdges))
	}
	// CLASS scope -- one row per class.
	for _, c := range idx.scopesByKind(parser.ScopeKindClass) {
		drafts = append(drafts, r.emit(ast, c, scope.KindClass, idx, callEdges))
	}
	// FILE scope -- exactly one row, AUTHORITATIVE per G3
	// (immutable once written; counts inbound edges the
	// producer surfaced in this file's edge list at this
	// SHA).
	drafts = append(drafts, r.emit(ast, idx.file, scope.KindFile, idx, callEdges))

	return drafts
}

// emit computes fan_in at one scope `s` of kind `kind` and
// returns the corresponding draft. callEdges is the
// pre-resolved list of (from-enclosing, to-enclosing) tuples
// for every `calls` edge in the file.
func (r *FanInRecipe) emit(ast *parser.AstFile, s *parser.AstScope, kind scope.Kind, idx scopeIndex, callEdges []fanInCallEndpoints) MetricSampleDraft {
	sub := idx.subtreeIDs(s.GetScopeId())
	count := 0
	for _, e := range callEdges {
		if e.to == "" || !sub[e.to] {
			continue
		}
		if e.from != "" && sub[e.from] {
			continue
		}
		count++
	}
	return newDraft(
		fanInMetricKind,
		fanInVersion,
		PackSolid,
		SourceComputed,
		float64(count),
		ScopeRef{
			LocalID:       s.GetScopeId(),
			Kind:          kind,
			QualifiedName: s.GetQualifiedName(),
			Path:          ast.GetPath(),
			// iter-5 evaluator item 3: GetParameters is
			// only populated by the parser for method
			// scopes (nil for class/file/package); the
			// canonical-signature helper ignores it for
			// non-method kinds. Pass-through is safe.
			Params: s.GetParameters(),
		},
		nil,
		fanInDirectlyEmittedKinds,
	)
}
