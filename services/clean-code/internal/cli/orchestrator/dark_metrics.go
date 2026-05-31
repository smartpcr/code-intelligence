// -----------------------------------------------------------------------
// <copyright file="dark_metrics.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package orchestrator

import (
	"fmt"
	"sort"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// DarkMetricClosurePhase is the canonical `closure_phase`
// stamp on every [DarkMetric] row the CLI emits in P0/P1
// (tech-spec REFACTOR-GUIDE Sec 8.7 line 1006: "always
// `\"P2\"` in P0/P1"). The literal is pinned as a named
// constant so a future phase rollout lands a coordinated
// edit here rather than divergent string drift across the
// orchestrator + report writer + JSON serializer.
const DarkMetricClosurePhase = "P2"

// Canonical `missing_attrs` values the dark-metric taxonomy
// admits (tech-spec Sec 8.7 line 1008: "The closed set of
// `missing_attrs` values is `decision_blocks`, `call_edges`,
// `field_accesses`."). Defined as the aliases to the
// recipes-package attr constants so a divergent literal here
// is impossible -- the table builder below references these
// directly so any future attr addition forces a coordinated
// edit in both packages.
const (
	darkAttrDecisionBlocks = recipes.AttrDecisionBlocks
	darkAttrCallEdges      = recipes.AttrCallEdges
	darkAttrFieldAccesses  = recipes.AttrFieldAccesses
)

// allowedDarkAttrs is the closed set of `missing_attrs`
// values [metricAttrRequirements] is allowed to reference,
// pinning the tech-spec Sec 8.7 line 1008 invariant. A
// future row that references a value outside this set is
// rejected by [validateMetricAttrRequirements] at package
// init -- the orchestrator MUST fail-closed (exit 70)
// rather than silently report an unknown attr.
var allowedDarkAttrs = map[string]struct{}{
	darkAttrDecisionBlocks: {},
	darkAttrCallEdges:      {},
	darkAttrFieldAccesses:  {},
}

// metricAttrRow declares the parser-attr capability set a
// single recipe's [recipes.Recipe.AppliesTo] predicate
// consults. The orchestrator iterates the closed
// [metricAttrRequirements] slice when `AppliesTo(file) ==
// false`, attributes the no-op to the row's `Attrs`, and
// surfaces a [DarkMetric] diagnostic per `(metric_kind,
// language)` pair.
//
// The `AffectedScopeKinds` slice is the set of parser-side
// `ScopeKind`s the recipe WOULD have evaluated had its
// `AppliesTo` returned true -- i.e. the scope kinds whose
// count contributes to `DarkMetric.AffectedScopeCount`.
// Pinned per-row (NOT derived from the recipe) so the
// recipe contract stays unchanged (architecture Sec 5.3
// "the dark-metric diagnostic is NOT collected by
// reflecting on the recipe").
type metricAttrRow struct {
	// Kind is the canonical [recipes.Recipe.MetricKind]
	// string (e.g. `"cyclo"`, `"fan_in"`, `"lcom4"`). MUST
	// match the recipe's MetricKind() byte-for-byte; a
	// typo would silently drop the row from the
	// dark-metric attribution path.
	Kind string

	// Attrs is the ORDERED slice of parser-attr constants
	// the recipe's AppliesTo predicate gates on (e.g.
	// `[decision_blocks]` for cyclo; `[call_edges,
	// field_accesses]` for lcom4). Order is preserved
	// in [DarkMetric.MissingAttrs] for byte-identical
	// determinism of the report writer (tech-spec D9).
	Attrs []string

	// AffectedScopeKinds is the closed set of parser-side
	// scope kinds the recipe iterates inside its Compute
	// body -- i.e. the universe whose cardinality maps to
	// `DarkMetric.AffectedScopeCount` (tech-spec Sec 8.7
	// line 1005: "Count of scopes the recipe would have
	// evaluated had the attr been stamped").
	//
	// Mirroring the recipe Compute bodies:
	//   - cyclo: every method scope (file-level scope
	//     excluded -- cyclo evaluates per-function
	//     complexity, not file-aggregated totals).
	//   - cognitive_complexity: every method + the file-
	//     level scope (one file-level draft per method-
	//     bearing file).
	//   - fan_in / fan_out: every method, class, and the
	//     file-level scope.
	//   - lcom4: every class scope (one draft per class).
	AffectedScopeKinds []parser.ScopeKind
}

// metricAttrRequirements is the CLI-local lookup table the
// orchestrator iterates when a recipe's AppliesTo returns
// false. The slice is the SINGLE SOURCE OF TRUTH for which
// `(metric_kind, missing_attr)` pairs the orchestrator
// surfaces as dark-metric diagnostics -- mirroring the
// `AppliesTo` gates in `recipes/recipe.go:55-122`
// (architecture.md Sec 3.3, Sec 5.3).
//
// A future recipe that gates on a new attr MUST land a row
// here in lockstep; [validateMetricAttrRequirements] (called
// from package init below) refuses to compile a row whose
// Attrs reference a value outside [allowedDarkAttrs].
var metricAttrRequirements = []metricAttrRow{
	{
		Kind:               "cyclo",
		Attrs:              []string{darkAttrDecisionBlocks},
		AffectedScopeKinds: []parser.ScopeKind{parser.ScopeKindMethod},
	},
	{
		Kind:               "cognitive_complexity",
		Attrs:              []string{darkAttrDecisionBlocks},
		AffectedScopeKinds: []parser.ScopeKind{parser.ScopeKindMethod, parser.ScopeKindFile},
	},
	{
		Kind:               "fan_in",
		Attrs:              []string{darkAttrCallEdges},
		AffectedScopeKinds: []parser.ScopeKind{parser.ScopeKindMethod, parser.ScopeKindClass, parser.ScopeKindFile},
	},
	{
		Kind:               "fan_out",
		Attrs:              []string{darkAttrCallEdges},
		AffectedScopeKinds: []parser.ScopeKind{parser.ScopeKindMethod, parser.ScopeKindClass, parser.ScopeKindFile},
	},
	{
		Kind:               "lcom4",
		Attrs:              []string{darkAttrCallEdges, darkAttrFieldAccesses},
		AffectedScopeKinds: []parser.ScopeKind{parser.ScopeKindClass},
	},
}

// metricAttrIndex is a lookup keyed by `MetricKind` for the
// constant slice [metricAttrRequirements]. Built once at
// package init; reading is map-fast at scan time.
var metricAttrIndex = func() map[string]metricAttrRow {
	m := make(map[string]metricAttrRow, len(metricAttrRequirements))
	for _, row := range metricAttrRequirements {
		m[row.Kind] = row
	}
	return m
}()

// init runs the [validateMetricAttrRequirements] guard at
// package load time so an out-of-set `missing_attr`
// literal (i.e. a typo or a forgotten coordinated edit) is
// a startup-time crash rather than a silent diagnostic
// drop.
func init() {
	if err := validateMetricAttrRequirements(metricAttrRequirements); err != nil {
		panic(fmt.Sprintf("orchestrator: invalid metricAttrRequirements table: %v", err))
	}
}

// validateMetricAttrRequirements enforces the closed-set
// invariants on the [metricAttrRequirements] table:
//
//   - every `Kind` is non-empty and unique
//   - every `Attrs` slice is non-empty
//   - every attr literal is in [allowedDarkAttrs] (tech-spec
//     Sec 8.7 line 1008 closed set)
//   - every `AffectedScopeKinds` slice is non-empty
//
// Returns a descriptive error on the first violation so the
// init-time panic message points at the row.
func validateMetricAttrRequirements(rows []metricAttrRow) error {
	seen := make(map[string]struct{}, len(rows))
	for i, row := range rows {
		if row.Kind == "" {
			return fmt.Errorf("row %d: Kind must not be empty", i)
		}
		if _, dup := seen[row.Kind]; dup {
			return fmt.Errorf("row %d: duplicate Kind %q", i, row.Kind)
		}
		seen[row.Kind] = struct{}{}
		if len(row.Attrs) == 0 {
			return fmt.Errorf("row %d (kind=%q): Attrs must not be empty", i, row.Kind)
		}
		for j, attr := range row.Attrs {
			if _, ok := allowedDarkAttrs[attr]; !ok {
				return fmt.Errorf(
					"row %d (kind=%q): Attrs[%d]=%q is NOT in the closed dark-metric attr set (tech-spec Sec 8.7 line 1008: {%q, %q, %q})",
					i, row.Kind, j, attr,
					darkAttrDecisionBlocks, darkAttrCallEdges, darkAttrFieldAccesses,
				)
			}
		}
		if len(row.AffectedScopeKinds) == 0 {
			return fmt.Errorf("row %d (kind=%q): AffectedScopeKinds must not be empty", i, row.Kind)
		}
	}
	return nil
}

// DarkMetric is one row of the CLI's `--diagnostics` JSON
// sidecar: a single `(metric_kind, language)` pair whose
// recipe stayed dark across the run because the parser
// fleet did not stamp the required attrs. Field shape is
// pinned by tech-spec REFACTOR-GUIDE Sec 8.7 lines 1000-1006.
//
// JSON field names use snake_case to match the tech-spec
// table verbatim (and the report writer's snapshot test).
type DarkMetric struct {
	// MetricKind is the recipe's `metric_kind` (e.g.
	// `"cyclo"`). Always one of the [metricAttrRequirements]
	// Kinds.
	MetricKind string `json:"metric_kind"`

	// Language is the file's detected language (e.g.
	// `"go"`, `"python"`, `"typescript"`, `"java"`). The
	// orchestrator emits one row per `(MetricKind, Language)`
	// pair so the report writer can render a per-language
	// breakdown without re-grouping.
	Language string `json:"language"`

	// MissingAttrs is the ordered list of unstamped
	// parser-attr constants the recipe's AppliesTo
	// predicate gates on. Order is byte-identical to the
	// `Attrs` field on the matching [metricAttrRow] so the
	// diagnostic's serialised form is deterministic
	// across runs (tech-spec D9).
	MissingAttrs []string `json:"missing_attrs"`

	// AffectedScopeCount is the count of scopes the recipe
	// would have evaluated across all files of `Language`
	// had the attrs been stamped. The count is computed
	// from the [metricAttrRow.AffectedScopeKinds] universe
	// summed across the run's AstFile corpus; a recipe
	// with no candidate scopes (e.g. a Go file with no
	// methods for cyclo) contributes zero to the count
	// but the row is still emitted so the operator sees
	// the attr gap.
	AffectedScopeCount int `json:"affected_scope_count"`

	// ClosurePhase is the phase that closes this dark
	// metric (always [DarkMetricClosurePhase] in P0/P1 per
	// tech-spec Sec 8.7 line 1006).
	ClosurePhase string `json:"closure_phase"`
}

// Diagnostics is the umbrella container for the CLI's
// dark-metric + future diagnostic rows. Future fields (e.g.
// degraded AST counts, recipe panic surfaces) land here
// rather than at top-level on `Result` so the
// `--diagnostics` JSON sidecar has a single root.
//
// Stage 2.5 emits `DarkMetrics` + `EffortSource`; the
// struct is open for additional fields without a breaking
// change because every consumer reads named fields.
type Diagnostics struct {
	// DarkMetrics is the deduped, sorted slice of
	// [DarkMetric] rows. One row per `(MetricKind,
	// Language)` pair observed across the run. Sorted by
	// `(MetricKind, Language)` for byte-identical
	// determinism (tech-spec D9). Empty slice (NOT nil)
	// when every recipe lit up for every language.
	DarkMetrics []DarkMetric `json:"dark_metrics"`

	// EffortSource is the stable, lowercase, snake_case
	// identifier of the effort estimator the orchestrator
	// resolved for this run. When the ONNX model is
	// unavailable (the default offline/dev scenario), the
	// value is [effort.FallbackEstimatorName] ("fallback").
	// Stamped by the orchestrator at the end of Run so
	// the `--diagnostics` JSON sidecar records which
	// effort source produced the per-task hours values.
	//
	// Anchor: tech-spec Sec 9.3 ("Effort model fallback").
	EffortSource string `json:"effort_source"`
}

// darkMetricKey is the dedup key the orchestrator uses
// while accumulating per-`(metric_kind, language)`
// counters. Languages with no detected files do not appear
// in the map -- the orchestrator never invents a dark row
// for a language that was not encountered in the scan.
type darkMetricKey struct {
	MetricKind string
	Language   string
}

// darkMetricAccumulator is the orchestrator's in-loop
// scratch state for accumulating per-`(metric_kind,
// language)` affected-scope counters. The orchestrator
// flushes it once at end-of-Run via [finalize].
type darkMetricAccumulator struct {
	rows map[darkMetricKey]*darkMetricRowState
}

// darkMetricRowState is the per-key counter the
// orchestrator increments file-by-file. `MissingAttrs` is
// copied from the [metricAttrRow] (not re-resolved) so the
// finalize step produces deterministic output independent
// of recipe iteration order.
type darkMetricRowState struct {
	MissingAttrs       []string
	AffectedScopeCount int
}

// newDarkMetricAccumulator returns a fresh accumulator. The
// orchestrator constructs one per [Orchestrator.Run] call
// so re-runs in the same process do not leak state.
func newDarkMetricAccumulator() *darkMetricAccumulator {
	return &darkMetricAccumulator{rows: map[darkMetricKey]*darkMetricRowState{}}
}

// observe accounts for one `(recipe, ast)` pair the
// orchestrator decided to skip via `AppliesTo(ast) ==
// false`. The accumulator looks the recipe up in
// [metricAttrIndex]; recipes whose MetricKind is NOT in the
// table (e.g. a recipe gated on degraded-only state, not on
// a parser-attr capability) are silently ignored -- their
// dark state is not a parser-attr gap and does not belong
// in the diagnostic.
//
// `ast` MUST be non-nil; callers guard upstream.
func (a *darkMetricAccumulator) observe(metricKind string, ast *parser.AstFile) {
	row, ok := metricAttrIndex[metricKind]
	if !ok {
		return
	}
	key := darkMetricKey{MetricKind: metricKind, Language: ast.GetLanguage()}
	state, exists := a.rows[key]
	if !exists {
		attrs := make([]string, len(row.Attrs))
		copy(attrs, row.Attrs)
		state = &darkMetricRowState{MissingAttrs: attrs}
		a.rows[key] = state
	}
	state.AffectedScopeCount += countAffectedScopes(ast, row.AffectedScopeKinds)
}

// countAffectedScopes returns the number of scopes on `ast`
// whose `ScopeKind` is in `kinds`. Used to populate
// [DarkMetric.AffectedScopeCount] -- the cardinality of the
// universe the recipe WOULD have evaluated had its
// `AppliesTo` returned true.
//
// A `nil` ast returns 0 (the orchestrator's parseOne path
// drops `nil` ASTs before recipe dispatch, so this is a
// belt-and-braces guard).
func countAffectedScopes(ast *parser.AstFile, kinds []parser.ScopeKind) int {
	if ast == nil || len(kinds) == 0 {
		return 0
	}
	set := make(map[parser.ScopeKind]struct{}, len(kinds))
	for _, k := range kinds {
		set[k] = struct{}{}
	}
	n := 0
	for _, sc := range ast.GetScopes() {
		if sc == nil {
			continue
		}
		if _, ok := set[sc.GetScopeKind()]; ok {
			n++
		}
	}
	return n
}

// finalize collapses the accumulator into a deterministic
// [Diagnostics] value. Rows are sorted by `(MetricKind,
// Language)`. An empty accumulator returns a
// `Diagnostics{DarkMetrics: []}` -- never `nil` -- so JSON
// serialisers emit `"dark_metrics": []` rather than
// `"dark_metrics": null` (operator-facing surfaces parse
// the empty array as "no dark metrics observed").
func (a *darkMetricAccumulator) finalize() Diagnostics {
	out := Diagnostics{DarkMetrics: []DarkMetric{}}
	if a == nil || len(a.rows) == 0 {
		return out
	}
	for key, state := range a.rows {
		out.DarkMetrics = append(out.DarkMetrics, DarkMetric{
			MetricKind:         key.MetricKind,
			Language:           key.Language,
			MissingAttrs:       state.MissingAttrs,
			AffectedScopeCount: state.AffectedScopeCount,
			ClosurePhase:       DarkMetricClosurePhase,
		})
	}
	sort.SliceStable(out.DarkMetrics, func(i, j int) bool {
		a, b := out.DarkMetrics[i], out.DarkMetrics[j]
		if a.MetricKind != b.MetricKind {
			return a.MetricKind < b.MetricKind
		}
		return a.Language < b.Language
	})
	return out
}

// validateBogusAttrRow exercises the production
// [validateMetricAttrRequirements] function with a single row
// whose Attrs contains "bogus_attr" — a value NOT in the
// closed [allowedDarkAttrs] set. Returns the error from the
// production validation path so callers (including the E2E
// subprocess helper) can assert both the exit code and the
// tech-spec Sec 8.7 error string without mirroring the
// validation logic.
//
// Re-exported for test builds via export_test.go.
func validateBogusAttrRow() error {
	return validateMetricAttrRequirements([]metricAttrRow{{
		Kind:               "bogus_metric",
		Attrs:              []string{"bogus_attr"},
		AffectedScopeKinds: []parser.ScopeKind{parser.ScopeKindMethod},
	}})
}
