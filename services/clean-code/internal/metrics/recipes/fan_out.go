package recipes

import (
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
)

// fanOutMetricKind is the canonical metric_kind string for
// the OUTBOUND coupling metric (architecture Sec 1.4.1 row 6
// -- "fan_out | method, class, file | solid | Outbound
// references from a scope. Drives DIP / DI rules"). Pinned
// as a const so `grep -nF "fan_out"` lands one definition
// site. NOT `fanout`, `fan-out`, or `outgoing_calls` -- the
// closed-set spelling is exactly `fan_out`.
const fanOutMetricKind = "fan_out"

// fanOutVersion is the recipe's `version()` per Sec 8.6 line
// 1010. A bump MUST coincide with a `metric_version` bump on
// every emitted sample (architecture C4) so a definitional
// change (e.g. "count distinct callees instead of call
// sites") lands as a new row at the same `(repo_id, sha,
// scope_id, metric_kind)`.
const fanOutVersion = 1

// fanOutCallEndpoints is a (from-enclosing-scope,
// to-enclosing-scope) pair derived from one `calls` edge:
// `from` and `to` each hold the enclosing scope_id for the
// edge's endpoint (or "" for external/unresolved). Cached
// once per Compute call so the per-scope fan_out count is
// O(edges) without re-resolving refs.
type fanOutCallEndpoints struct {
	from string
	to   string
}

// FanOutRecipe is the outbound-coupling recipe for the
// foundation tier (architecture Sec 1.4.1 row 6).
//
// # Algorithm
//
// Fan-out at a scope S = the number of OUTGOING `calls`
// edges whose `from` endpoint resolves into S's subtree AND
// whose `to` endpoint resolves OUTSIDE S's subtree:
//
//	fan_out(S) = |{e in edges : e.kind == "calls" AND
//	    enclosing_scope(e.from) in subtree(S) AND
//	    enclosing_scope(e.to) NOT in subtree(S)}|
//
// Self-calls (recursion) and intra-subtree calls do NOT
// count: a method that calls itself, or a class whose method
// calls another method of the same class, has zero outbound
// COUPLING contribution for that edge.
//
// For the FILE scope specifically: every `edge.from` in a
// per-file edge list resolves into the file scope's subtree
// (because edges live on `AstFile.Edges` and reference
// scopes/symbols declared in the file); the file-level
// fan_out is therefore the count of `calls` edges whose `to`
// is NOT in the file's subtree (i.e. external symbols or
// scopes from other files). This is per-file authoritative
// because `from` is always local: a `calls` edge whose
// caller is in this file is fully visible HERE.
//
// # Scope kinds (canonical seven-enum)
//
// Drafts emit at `scope.KindMethod`, `scope.KindClass`, and
// `scope.KindFile` per architecture Sec 1.4.1 row 6 column 2
// entry `method, class, file`. The [newDraft] helper panics
// on any other value; the recipe's [fanOutAllowedKinds] slice
// refuses the rest of the canonical 7-enum AND the drift
// candidates `function`, `module`, `namespace`.
//
// All three scope_kinds are DIRECTLY EMITTED by this per-file
// recipe -- consistent with `fan_in` (which also emits at all
// three pinned scope_kinds per Sec 1.4.1 row 5). Fan-out is
// per-file authoritative because `edge.from` is always
// local; see [FanOutRecipe.Compute] for the details.
//
// # Capability + degradation gate
//
// [Recipe.AppliesTo] returns true iff the AST is non-nil,
// NOT degraded, AND the producer stamped [AttrCallEdges] =
// "true" on the file-level attrs. The Stage 2.1 parser fleet
// does NOT yet emit `calls` edges (it emits `imports`,
// `extends`, `implements`, `contains`), so production scans
// currently produce zero fan_out drafts; this matches
// architecture Sec 3.4 lines 490-494 ("Computed rows are
// never `degraded=true`: if an input is missing the row is
// not written, not stamped degraded"). When a future parser
// stage emits `calls` and stamps `call_edges="true"`, this
// recipe lights up automatically.
type FanOutRecipe struct{}

// fanOutAllowedKinds is the METRIC_KIND APPLICABILITY SET --
// the closed scope_kind set the SCHEMA accepts for a
// persisted `MetricSample(metric_kind='fan_out')` row,
// mirroring architecture Sec 1.4.1 row 6 column 2 entry
// `method, class, file`. NOT `module`, `package`, `repo`,
// `function`, or `interface` -- the row's applicability is
// pinned to the three call-site-host kinds. The architecture
// table on row 6 pins exactly these three; package-level and
// repo-level coupling is captured by sibling metrics
// (`coupling_between_objects` lands later) rather than
// rolling fan_out up.
//
// All three values are also in [fanOutDirectlyEmittedKinds]
// (fan_out is per-file authoritative at every level). Kept as
// a named variable so the architecture pin is auditable via
// a `grep -nF "fanOutAllowedKinds"`.
var fanOutAllowedKinds = []scope.Kind{scope.KindMethod, scope.KindClass, scope.KindFile}

// fanOutDirectlyEmittedKinds is the scope_kind set this
// per-file recipe directly emits drafts at. For fan_out this
// is IDENTICAL to [fanOutAllowedKinds] because `edge.from` is
// always inside the file -- per-file is authoritative at
// every level.
//
// Same shape as `fan_in` (which also emits at all three
// pinned scope_kinds per Sec 1.4.1 row 5); fan_out is
// authoritative at file-scope because `edge.from` resolves
// locally, while fan_in is authoritative at file-scope
// because each emitted row counts exactly the inbound
// `calls` edges THE PRODUCER SURFACED in this file's edge
// list (see [FanInRecipe] doc "Authoritative per-parser
// semantics").
var fanOutDirectlyEmittedKinds = fanOutAllowedKinds

// NewFanOutRecipe returns a stateless [FanOutRecipe]. Safe
// for concurrent Compute calls.
func NewFanOutRecipe() *FanOutRecipe { return &FanOutRecipe{} }

// MetricKind implements [Recipe].
func (r *FanOutRecipe) MetricKind() string { return fanOutMetricKind }

// Version implements [Recipe].
func (r *FanOutRecipe) Version() int { return fanOutVersion }

// Pack implements [Recipe]. fan_out is in the `solid` pack
// (architecture Sec 1.4.1 row 6).
func (r *FanOutRecipe) Pack() Pack { return PackSolid }

// AppliesTo implements [Recipe]. Returns true iff the AST is
// non-nil, NOT degraded, AND the producer advertised the
// `call_edges` capability via [AttrCallEdges].
//
// The non-degraded check realises architecture Sec 3.4 lines
// 490-494 ("computed rows are never degraded=true"): a
// degraded AST means the parser had to bail mid-file, and
// emitting a `source='computed'` fan_out row off truncated
// call-graph data would silently lie about coupling.
func (r *FanOutRecipe) AppliesTo(ast *parser.AstFile) bool {
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
// source order. A degraded AST or one without the
// `call_edges` capability returns nil -- defence-in-depth
// against a caller that forgot to gate on
// [FanOutRecipe.AppliesTo].
//
// # Per-scope value computation
//
// The recipe walks `ast.Edges` ONCE, filtering by
// `kind == "calls"`. For each surviving edge it resolves
// `edge.from` to an enclosing scope_id (the method / block /
// class the call site is lexically inside) via
// [scopeIndex.refEnclosingScope], and resolves `edge.to` the
// same way. Per emit-scope S the recipe counts edges whose
// `from` is in S's subtree AND whose `to` is NOT in S's
// subtree.
//
// For the FILE scope the `to NOT in subtree` test reduces to
// "to is external to this AstFile" -- i.e. the index does not
// know the target scope/symbol. An edge whose target IS in
// the file but in a different class still counts as fan_out
// at the originating CLASS scope but NOT at the file scope.
// This matches the architecture's intent: file-level fan_out
// measures the file's coupling to OTHER FILES, class-level
// fan_out measures the class's coupling to OTHER CLASSES /
// EXTERNAL SYMBOLS.
//
// The `allowedKinds` slice passed to [newDraft] is
// [fanOutDirectlyEmittedKinds] (`{method, class, file}`),
// identical to [fanOutAllowedKinds] -- fan_out is per-file
// authoritative at every level.
func (r *FanOutRecipe) Compute(ast *parser.AstFile) []MetricSampleDraft {
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

	// Materialise the call-edge endpoint pairs ONCE so the
	// per-scope counting is O(scopes * edges) at worst,
	// without re-walking `idx.byID` for every edge.
	callEdges := make([]fanOutCallEndpoints, 0, len(ast.GetEdges()))
	for _, e := range ast.GetEdges() {
		if e == nil || e.GetKind() != EdgeKindCalls {
			continue
		}
		callEdges = append(callEdges, fanOutCallEndpoints{
			from: idx.refEnclosingScope(e.GetFrom()),
			to:   idx.refEnclosingScope(e.GetTo()),
		})
	}

	// Emit at each kind in source order. The recipe deliberately
	// uses `subtreeIDs` per emit-scope (memoised on the index
	// across the loop) rather than maintaining a global
	// edge-bucket-by-scope structure: typical files have
	// ~10s of methods and ~10s of edges, so the O(M+E) per
	// scope is cheap and the code is easier to audit.
	drafts := make([]MetricSampleDraft, 0)

	// METHOD scope -- one row per method.
	for _, m := range idx.scopesByKind(parser.ScopeKindMethod) {
		drafts = append(drafts, r.emit(ast, m, scope.KindMethod, idx, callEdges))
	}
	// CLASS scope -- one row per class.
	for _, c := range idx.scopesByKind(parser.ScopeKindClass) {
		drafts = append(drafts, r.emit(ast, c, scope.KindClass, idx, callEdges))
	}
	// FILE scope -- exactly one row.
	drafts = append(drafts, r.emit(ast, idx.file, scope.KindFile, idx, callEdges))

	return drafts
}

// emit computes fan_out at one scope `s` of kind `kind` and
// returns the corresponding draft. callEdges is the
// pre-resolved list of (from-enclosing, to-enclosing) tuples
// for every `calls` edge in the file.
func (r *FanOutRecipe) emit(ast *parser.AstFile, s *parser.AstScope, kind scope.Kind, idx scopeIndex, callEdges []fanOutCallEndpoints) MetricSampleDraft {
	sub := idx.subtreeIDs(s.GetScopeId())
	count := 0
	for _, e := range callEdges {
		if e.from == "" || !sub[e.from] {
			continue
		}
		if e.to != "" && sub[e.to] {
			continue
		}
		count++
	}
	return newDraft(
		fanOutMetricKind,
		fanOutVersion,
		PackSolid,
		SourceComputed,
		float64(count),
		ScopeRef{
			LocalID:       s.GetScopeId(),
			Kind:          kind,
			QualifiedName: s.GetQualifiedName(),
			Path:          ast.GetPath(),
			// iter-5 evaluator item 3: see fan_in.go.
			Params: s.GetParameters(),
		},
		nil,
		fanOutDirectlyEmittedKinds,
	)
}
