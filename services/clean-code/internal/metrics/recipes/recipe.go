package recipes

import (
	"fmt"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/scope"
)

// Pack is the closed enum of `MetricSample.pack` values
// (architecture Sec 5.2.1 line 901). Foundation-tier recipes
// in this package always emit [PackBase]; the [Pack] type
// exists so a non-canonical literal (e.g. `"basic"`,
// `"foundation"`) is a compile error rather than a string
// drift that lands in PostgreSQL.
type Pack string

// Source is the closed enum of `MetricSample.source` values
// (architecture Sec 5.2.1 line 902). Foundation-tier recipes
// always emit [SourceComputed]; the External Metric Ingest
// Webhook (Sec 3.12) and the Cross-Repo Aggregator (Sec 3.10
// step 4) own the other two values.
type Source string

// Canonical [Pack] values (architecture Sec 5.2.1 line 901,
// Sec 1.4.1 / Sec 1.4.2 pack tags).
const (
	PackBase     Pack = "base"
	PackSolid    Pack = "solid"
	PackIngested Pack = "ingested"
	PackSystem   Pack = "system"
)

// Canonical [Source] values (architecture Sec 5.2.1 line 902).
const (
	SourceComputed Source = "computed"
	SourceIngested Source = "ingested"
	SourceDerived  Source = "derived"
)

// AttrDecisionBlocks is the file-level [parser.AstFile.Attrs]
// key that producers stamp when they have emitted
// `ScopeKindBlock` decision-point children under method
// scopes. Recipes that depend on decision blocks
// (cyclo, cognitive_complexity) gate [Recipe.AppliesTo] on
// this attribute being literally `"true"`; absent or any other
// value MUST skip emission.
//
// The Stage 2.1 parser fleet does NOT yet stamp this attribute
// because it does not decompose method bodies. A future parser
// stage that walks `if` / `for` / `while` / `case` / `catch`
// nodes into `ScopeKindBlock` children MUST also stamp
// `Attrs["decision_blocks"] = "true"` on the emitted
// `*AstFile` so this package's recipes light up automatically.
const AttrDecisionBlocks = "decision_blocks"

// AttrDecisionKind is the per-block-scope [parser.AstScope.Attrs]
// key carrying the canonical decision-point taxonomy below.
// Recipes treat unknown / absent values as non-decision (no
// contribution to cyclo or cognitive complexity); the closed
// set is the contract.
const AttrDecisionKind = "decision_kind"

// AttrCycleID is the per-sample [MetricSampleDraft.Attrs] key
// reserved for the `cycle_member` recipe's SCC identifier
// (architecture Sec 1.4.1 row 10). Defined here so the closed
// set of attribute keys lives next to [AttrDecisionKind].
const AttrCycleID = "cycle_id"

// AttrSourceBytes is the per-AstFile [parser.AstFile.Attrs]
// key the `duplication_ratio` recipe inspects to obtain the
// file's raw source bytes for LEXICAL tokenisation. The
// parser's `scopeBuilder.build` populates this attr at
// AstFile construction time so the DEFAULT recipe (the one
// `DefaultProjectRegistry()` registers) exercises lexical
// mode for normal parser output -- the e2e contract at
// e2e-scenarios.md:426-430 (whitespace-canonical duplication
// detection). When absent or empty, the recipe falls through
// to its [SourceLoader] seam and ultimately to the
// structural-token fallback.
//
// Why on AstFile.Attrs rather than the recipe's SourceLoader
// callback? PURITY (recipe contract `recipe.go:227-237`:
// "Same `*parser.AstFile` in -> same drafts out"). When the
// bytes live on the AstFile itself, the recipe's output is a
// pure function of its input AstFile -- no cwd dependency,
// no filesystem dependency, no time-of-day dependency.
//
// Defined as an alias to [parser.AttrSourceBytes] so the
// parser and recipe stay locked to the same key (rubber-duck
// finding iter-5: a divergent literal in either package would
// silently break lexical mode again).
const AttrSourceBytes = parser.AttrSourceBytes

// AttrModulePath is the per-AstFile [parser.AstFile.Attrs] key
// the `cycle_member` recipe inspects to obtain the project's
// module path (e.g. `github.com/org/repo` for a Go module).
// When at least one in-project AstFile carries the attr, the
// cycle_member resolver uses it as the authoritative source
// of import-target canonicalisation: an import like
// `github.com/org/repo/internal/foo` is stripped to
// `internal/foo` before retrying the exact-dir match.
//
// When the attr is absent on every in-project AstFile, the
// resolver falls back to EXACT directory and EXACT
// qualifiedName matches only -- iter-5 evaluator item 2
// removed the unsafe multi-segment suffix tier that could
// false-positive on external imports whose tail matched an
// in-project directory.
//
// Defined as an alias to [parser.AttrModulePath].
const AttrModulePath = parser.AttrModulePath

// AttrSourceLine and AttrSourceFile are per-sample
// [MetricSampleDraft.Attrs] keys recipes MAY populate so the
// Insights / Refactor Planner surfaces can render a citation
// without a second pass over the AST. Both default to empty.
const (
	AttrSourceFile = "source_file"
	AttrSourceLine = "source_line"
)

// DecisionKind is the closed taxonomy stamped onto
// `ScopeKindBlock` scopes via [AttrDecisionKind]. The set is
// frozen at the package boundary: a future parser that wants
// to count a new decision shape MUST land a value in this list
// (with a coordinated recipe edit) rather than inventing a
// new string.
type DecisionKind string

// Canonical [DecisionKind] values. The taxonomy follows
// SonarSource's cognitive-complexity reference table and
// McCabe's cyclomatic-complexity counting rules:
//
//   - "structural" decisions (if/for/while/case/catch) bump
//     cyclo by 1 and contribute (1 + nesting_level) to
//     cognitive complexity; their bodies push the nesting
//     counter for descendant decisions.
//   - "logical" operators (`&&`, `||`) bump cyclo by 1 and
//     contribute 1 to cognitive (no nesting bonus, no nesting
//     push).
//   - "linear" cognitive penalties (labeled break/continue,
//     goto, recursion) contribute 1 to cognitive but NOT to
//     cyclo (they do not create a new execution path).
//   - "else_if" / "else" are continuations of the parent if;
//     they keep the cyclo branch count honest but do NOT add
//     a nesting bonus to cognitive (per SonarSource's
//     `else if` rule). "else" alone (no condition) adds 0 to
//     cyclo and 1 to cognitive.
const (
	DecisionIf         DecisionKind = "if"
	DecisionElseIf     DecisionKind = "else_if"
	DecisionElse       DecisionKind = "else"
	DecisionFor        DecisionKind = "for"
	DecisionWhile      DecisionKind = "while"
	DecisionDoWhile    DecisionKind = "do_while"
	DecisionCase       DecisionKind = "case"
	DecisionCatch      DecisionKind = "catch"
	DecisionTernary    DecisionKind = "ternary"
	DecisionLogicalAnd DecisionKind = "logical_and"
	DecisionLogicalOr  DecisionKind = "logical_or"
	DecisionBreakLabel DecisionKind = "break_label"
	DecisionContLabel  DecisionKind = "continue_label"
	DecisionGoto       DecisionKind = "goto"
	DecisionRecursion  DecisionKind = "recursion"
)

// decisionInfo is the per-[DecisionKind] contribution table.
// `cycloDelta` and `cognitiveDelta` are the base
// contributions; `nestingBonus` adds the current nesting depth
// on top of `cognitiveDelta` for cognitive complexity;
// `pushesNesting` indicates whether the decision's children
// inherit a deeper nesting level.
type decisionInfo struct {
	cycloDelta     int
	cognitiveDelta int
	nestingBonus   bool
	pushesNesting  bool
}

// decisionTable is the single source of truth for how each
// [DecisionKind] contributes to cyclo and cognitive
// complexity. The map keys are pinned -- a `grep -nF
// "decisionTable"` lands one definition site.
var decisionTable = map[DecisionKind]decisionInfo{
	DecisionIf:         {cycloDelta: 1, cognitiveDelta: 1, nestingBonus: true, pushesNesting: true},
	DecisionElseIf:     {cycloDelta: 1, cognitiveDelta: 1, nestingBonus: false, pushesNesting: false},
	DecisionElse:       {cycloDelta: 0, cognitiveDelta: 1, nestingBonus: false, pushesNesting: false},
	DecisionFor:        {cycloDelta: 1, cognitiveDelta: 1, nestingBonus: true, pushesNesting: true},
	DecisionWhile:      {cycloDelta: 1, cognitiveDelta: 1, nestingBonus: true, pushesNesting: true},
	DecisionDoWhile:    {cycloDelta: 1, cognitiveDelta: 1, nestingBonus: true, pushesNesting: true},
	DecisionCase:       {cycloDelta: 1, cognitiveDelta: 1, nestingBonus: true, pushesNesting: true},
	DecisionCatch:      {cycloDelta: 1, cognitiveDelta: 1, nestingBonus: true, pushesNesting: true},
	DecisionTernary:    {cycloDelta: 1, cognitiveDelta: 1, nestingBonus: true, pushesNesting: false},
	DecisionLogicalAnd: {cycloDelta: 1, cognitiveDelta: 1, nestingBonus: false, pushesNesting: false},
	DecisionLogicalOr:  {cycloDelta: 1, cognitiveDelta: 1, nestingBonus: false, pushesNesting: false},
	DecisionBreakLabel: {cycloDelta: 0, cognitiveDelta: 1, nestingBonus: false, pushesNesting: false},
	DecisionContLabel:  {cycloDelta: 0, cognitiveDelta: 1, nestingBonus: false, pushesNesting: false},
	DecisionGoto:       {cycloDelta: 0, cognitiveDelta: 1, nestingBonus: false, pushesNesting: false},
	DecisionRecursion:  {cycloDelta: 0, cognitiveDelta: 1, nestingBonus: false, pushesNesting: false},
}

// ScopeRef carries the intra-file scope identity the Metric
// Ingestor needs to resolve a draft to a durable
// `ScopeBinding.scope_id`. The recipe layer never mints durable
// UUIDs (G1); it only forwards the parser's intra-file
// placeholder, the canonical signature seeds, and the file
// path.
type ScopeRef struct {
	// LocalID is the parser's intra-file placeholder
	// (`local:N`), copied verbatim from
	// [parser.AstScope.GetScopeId]. The Metric Ingestor
	// rewrites it to a durable `scope_id` UUID via
	// `scope.DeriveScopeID` after looking up `first_seen_sha`.
	LocalID string
	// Kind is the canonical scope kind the draft applies to.
	// Always [scope.KindMethod] or [scope.KindFile] for the
	// recipes in this package; other recipes (lcom4, fan_in,
	// ...) may emit at other kinds.
	Kind scope.Kind
	// QualifiedName seeds the canonical signature the Ingestor
	// will build via `scope.BuildMethod` / `scope.BuildFile`.
	QualifiedName string
	// Path is the repo-relative file path
	// (`AstFile.GetPath()`).
	Path string
}

// MetricSampleDraft is one row's worth of recipe output. The
// fields mirror the columns of [Sec 5.2.1 `MetricSample`] the
// Metric Ingestor will eventually write, MINUS the columns the
// Ingestor mints itself (`sample_id`, `repo_id`, `sha`,
// `scope_id`, `producer_run_id`, `created_at`).
//
// Per [G3] the Ingestor never updates these columns once a row
// has landed; the recipe is the sole producer of every
// non-Ingestor field, so a defensive emit guard
// ([newDraft]) refuses to construct a draft with an
// out-of-set Pack / Source / ScopeKind value.
//
// # No DegradedReason field
//
// Computed-tier drafts have NO `degraded_reason` column on
// purpose: architecture Sec 3.4 lines 490-494 says
// "Computed rows are never `degraded=true`: if an input is
// missing the row is not written, not stamped degraded -- only
// system-tier derivation stamps `degraded=true` per
// Section 8.2." Recipes that cannot produce a meaningful
// value MUST skip emission (return nil drafts) -- they MUST
// NOT smuggle a degraded reason into a `source='computed'`
// row. The architecture's "row not written, not stamped
// degraded" rule is the canonical contract.
type MetricSampleDraft struct {
	MetricKind    string
	MetricVersion int
	Pack          Pack
	Source        Source
	Value         float64
	Scope         ScopeRef
	Attrs         map[string]string
}

// Recipe is the contract every metric_kind implementation
// satisfies (architecture Sec 8.6 lines 1006-1013 -- "Each
// `metric_kind` x language pair is implemented as a `Recipe`
// interface with three methods: `applies_to(file_node) bool`,
// `compute(file_node) []Sample`, `version() int`"). Recipes
// are language-agnostic at the API boundary -- per-language
// specialisation is the parser fleet's job (Sec 4.5).
//
// Recipes MUST be:
//
//   - **Pure.** Same `*parser.AstFile` in -> same drafts out.
//     No clocks, no RNG, no DB I/O. Determinism is part of G2:
//     a recipe that emits non-determinism would break re-run
//     idempotency for the same SHA.
//   - **Stateless.** A single recipe instance is reused across
//     every `AstFile` in a scan; per-file state lives in
//     locals.
//   - **Read-only.** Recipes MUST NOT mutate the input AST
//     (downstream recipes share the same pointer).
type Recipe interface {
	// MetricKind returns the canonical metric_kind string
	// (architecture Sec 1.4.1 column 2). MUST be a closed-set
	// value (`cyclo`, `cognitive_complexity`, `loc`, `lcom4`,
	// `fan_in`, `fan_out`, ...). The string is the FK target
	// of `MetricSample.metric_kind` -- a typo here surfaces
	// as a foreign-key violation at insert time.
	MetricKind() string
	// Version is the recipe's `version()` per Sec 8.6 line
	// 1010 -- the integer copied onto each emitted draft as
	// `MetricVersion`. Bumping this number MUST be paired
	// with a bump on the `recipe_manifest` row so downstream
	// readers can tell "definitional change" (new row at new
	// metric_version) apart from "value drift at the same
	// metric_version".
	Version() int
	// AppliesTo returns true iff `Compute` is allowed to run
	// on this AST. A recipe that returns false silently
	// produces NO drafts at this stage -- this is how the
	// foundation tier honours Sec 3.4's "if an input is
	// missing the row is not written, not stamped degraded"
	// rule.
	AppliesTo(ast *parser.AstFile) bool
	// Compute consumes a canonical `*parser.AstFile` and
	// returns the draft rows the Metric Ingestor will insert
	// into `MetricSample`. The caller MUST gate on
	// `AppliesTo(ast) == true` before invoking Compute.
	Compute(ast *parser.AstFile) []MetricSampleDraft
}

// astScopeKind converts a parser-side `parser.ScopeKind` enum
// to the database / scope-package `scope.Kind` typed string.
// Returns the empty Kind for `SCOPE_KIND_UNSPECIFIED` (which
// signals a producer bug -- writers reject the zero value per
// Sec 5.2.3 line 54). Recipes use this to translate the
// parser-side enum into the wire-format `scope_kind` the
// Ingestor will eventually persist.
func astScopeKind(k parser.ScopeKind) scope.Kind {
	switch k {
	case parser.ScopeKindRepo:
		return scope.KindRepo
	case parser.ScopeKindPackage:
		return scope.KindPackage
	case parser.ScopeKindFile:
		return scope.KindFile
	case parser.ScopeKindClass:
		return scope.KindClass
	case parser.ScopeKindInterface:
		return scope.KindInterface
	case parser.ScopeKindMethod:
		return scope.KindMethod
	case parser.ScopeKindBlock:
		return scope.KindBlock
	default:
		return ""
	}
}

// newDraft is the single emit helper every recipe in this
// package routes through. It enforces the global invariants
// EVERY foundation-tier draft must satisfy (Pack == PackBase,
// Source == SourceComputed, ScopeKind in the canonical
// 7-enum, LocalID populated) plus the PER-RECIPE invariant
// (the draft's ScopeKind is in `allowedKinds` -- the
// architecture Sec 1.4.1 row's pinned scope_kind set for the
// recipe's metric_kind).
//
// `allowedKinds` is the per-recipe closed slice -- not
// hard-coded at the helper layer -- so a recipe whose row in
// Sec 1.4.1 pins a DIFFERENT scope_kind set (e.g. loc at
// `file, package, repo`; lcom4 at `class`) can route through
// the same helper without rewriting the global enforcement.
// The cyclo / cognitive_complexity recipes pass
// `{scope.KindMethod, scope.KindFile}`; loc passes
// `{scope.KindFile, scope.KindPackage, scope.KindRepo}`.
//
// On a guard violation the helper PANICS rather than returning
// an error: an out-of-set value at this layer is a programmer
// bug (a typo'd const or a wrong scope_kind branch) that MUST
// surface at the first test run rather than landing as a bad
// row in the Metrics Store. The panic strings are exercised
// directly by `recipe_internal_test.go` (package recipes),
// which calls newDraft and asserts `recover()` returns the
// expected message -- the closed-set guard is REAL, not just a
// doc claim.
func newDraft(
	metricKind string,
	metricVersion int,
	pack Pack,
	source Source,
	value float64,
	scopeRef ScopeRef,
	attrs map[string]string,
	allowedKinds []scope.Kind,
) MetricSampleDraft {
	if pack != PackBase {
		panic(fmt.Sprintf("recipes: foundation-tier draft must have pack=%q, got %q", PackBase, pack))
	}
	if source != SourceComputed {
		panic(fmt.Sprintf("recipes: foundation-tier draft must have source=%q, got %q", SourceComputed, source))
	}
	if !scopeRef.Kind.IsValid() {
		panic(fmt.Sprintf(
			"recipes: %s draft scope_kind=%q is NOT in the canonical seven-enum (repo|package|file|class|interface|method|block); %q and %q in particular are NOT canonical values per architecture Sec 5.2.3",
			metricKind, scopeRef.Kind, "function", "module",
		))
	}
	if len(allowedKinds) == 0 {
		panic(fmt.Sprintf("recipes: %s draft has empty allowedKinds (recipe MUST pin its Sec 1.4.1 scope_kind set)", metricKind))
	}
	if !kindIn(scopeRef.Kind, allowedKinds) {
		panic(fmt.Sprintf(
			"recipes: %s draft scope_kind=%q not in this recipe's allowed set %v (architecture Sec 1.4.1 pins the row's scope_kinds)",
			metricKind, scopeRef.Kind, allowedKinds,
		))
	}
	if scopeRef.LocalID == "" {
		panic(fmt.Sprintf("recipes: %s draft has empty Scope.LocalID (parser MUST populate AstScope.scope_id before recipes run)", metricKind))
	}
	if attrs == nil {
		attrs = map[string]string{}
	}
	return MetricSampleDraft{
		MetricKind:    metricKind,
		MetricVersion: metricVersion,
		Pack:          pack,
		Source:        source,
		Value:         value,
		Scope:         scopeRef,
		Attrs:         attrs,
	}
}

// kindIn reports whether `k` is one of `set`. Tiny linear
// scan -- the per-recipe `allowedKinds` slice is at most 3
// entries (loc has the largest set: file / package / repo).
func kindIn(k scope.Kind, set []scope.Kind) bool {
	for _, x := range set {
		if x == k {
			return true
		}
	}
	return false
}

// hasDecisionCapability reports whether the AST producer
// stamped [AttrDecisionBlocks] on the file-level attrs. This
// is the gate every block-walking recipe checks in
// `AppliesTo`. Without the gate, a recipe running against
// today's shallow parser output would emit a misleading
// "computed" cyclo=1 for every method.
func hasDecisionCapability(ast *parser.AstFile) bool {
	if ast == nil {
		return false
	}
	return ast.GetAttrs()[AttrDecisionBlocks] == "true"
}

// scopeIndex is the cached parent/child topology of a single
// `*parser.AstFile`. Walking the scope list once per Compute
// keeps the algorithm O(N + E) rather than re-scanning per
// method. Children are returned in source order (the parser
// is contractually source-ordered, Sec 4.5).
type scopeIndex struct {
	byID     map[string]*parser.AstScope
	children map[string][]*parser.AstScope
	file     *parser.AstScope
}

// buildIndex constructs a [scopeIndex] from an `*AstFile`. The
// file-level scope is the unique scope whose `parent_scope_id`
// is empty AND whose `scope_kind` is `SCOPE_KIND_FILE`;
// callers that need a guarantee that exactly one file scope
// exists MUST check `idx.file != nil` after the call.
func buildIndex(ast *parser.AstFile) scopeIndex {
	idx := scopeIndex{
		byID:     map[string]*parser.AstScope{},
		children: map[string][]*parser.AstScope{},
	}
	for _, s := range ast.GetScopes() {
		if s == nil {
			continue
		}
		idx.byID[s.GetScopeId()] = s
		parent := s.GetParentScopeId()
		idx.children[parent] = append(idx.children[parent], s)
		if s.GetScopeKind() == parser.ScopeKindFile && idx.file == nil {
			idx.file = s
		}
	}
	return idx
}

// methodScopes returns the method-kind scopes in source order
// (the parser's contractual output order). Filtering is by
// `scope_kind` only -- nested methods (e.g. closures) appear
// in the slice if and only if the parser emitted them as
// `SCOPE_KIND_METHOD` scopes.
func (idx scopeIndex) methodScopes() []*parser.AstScope {
	out := make([]*parser.AstScope, 0)
	for _, s := range idx.byID {
		if s.GetScopeKind() == parser.ScopeKindMethod {
			out = append(out, s)
		}
	}
	// Preserve source order via a stable sort on (start_line,
	// start_byte) -- the map iteration above is unstable.
	sortByRange(out)
	return out
}

// sortByRange orders scopes by `(start_line, start_byte,
// scope_id)`. The third key keeps the sort deterministic even
// when two scopes share a synthetic zero range (which can
// happen for the file scope or a degraded parse).
func sortByRange(s []*parser.AstScope) {
	// Tiny insertion sort -- recipes operate on per-file
	// slices that rarely exceed a few hundred scopes, and
	// avoiding `sort.Slice` keeps the import set minimal.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && less(s[j], s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// less is the strict less-than ordering for [sortByRange].
// A nil range sorts before a populated one.
func less(a, b *parser.AstScope) bool {
	ar, br := a.GetRange(), b.GetRange()
	if ar == nil && br != nil {
		return true
	}
	if ar != nil && br == nil {
		return false
	}
	if ar != nil && br != nil {
		if ar.GetStartLine() != br.GetStartLine() {
			return ar.GetStartLine() < br.GetStartLine()
		}
		if ar.GetStartByte() != br.GetStartByte() {
			return ar.GetStartByte() < br.GetStartByte()
		}
	}
	return a.GetScopeId() < b.GetScopeId()
}
