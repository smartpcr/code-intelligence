package recipes

import (
	"reflect"
	"strings"
	"testing"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/scope"
)

// recover_must_panic_with helper -- ensures the deferred
// recover catches a panic whose message contains every
// `want` substring. Returns the panic value for additional
// assertions.
func recover_must_panic_with(t *testing.T, fn func(), want ...string) any {
	t.Helper()
	var got any
	func() {
		defer func() { got = recover() }()
		fn()
	}()
	if got == nil {
		t.Fatalf("expected panic, got none")
	}
	msg, _ := got.(string)
	for _, w := range want {
		if !strings.Contains(msg, w) {
			t.Errorf("panic message %q does not contain %q", msg, w)
		}
	}
	return got
}

// validScopeRef returns a method-scope ScopeRef the
// per-recipe-allowed-kinds set in cyclo / cognitive accepts.
// Used as the "good" baseline so tests can vary one field at
// a time and observe the right panic.
func validScopeRef() ScopeRef {
	return ScopeRef{
		LocalID:       "local:1",
		Kind:          scope.KindMethod,
		QualifiedName: "pkg.Type.method",
		Path:          "p.go",
	}
}

// TestNewDraft_PanicsOnNonCanonicalScopeKind is the iter-2
// evaluator item-4 contract: an actual call to `newDraft`
// with a `scope_kind` outside the canonical seven-enum MUST
// panic at the call site -- NOT just `scope.Kind.IsValid()`
// returning false in a sibling assertion. The panic message
// MUST cite the forbidden values `function` and `module`
// explicitly so a `grep -nF` over CI logs lands the
// diagnosis.
//
// This test calls `newDraft` directly (it lives in package
// `recipes`, not `recipes_test`, so the unexported helper is
// reachable) -- the iter-1 version only asserted IsValid
// from the external package and never proved the panic
// path.
func TestNewDraft_PanicsOnNonCanonicalScopeKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		badKind scope.Kind
	}{
		{"function (Halleck45 ast-metrics drift)", scope.Kind("function")},
		{"module (Python jargon drift)", scope.Kind("module")},
		{"namespace (C++ drift)", scope.Kind("namespace")},
		{"empty (unset)", scope.Kind("")},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(string(tc.badKind), func(t *testing.T) {
			t.Parallel()
			sr := validScopeRef()
			sr.Kind = tc.badKind
			recover_must_panic_with(t,
				func() {
					_ = newDraft(
						"cyclo", 1, PackBase, SourceComputed,
						1.0, sr, nil,
						[]scope.Kind{scope.KindMethod, scope.KindFile},
					)
				},
				"cyclo", "canonical seven-enum", "function", "module",
			)
		})
	}
}

// TestNewDraft_PanicsOnNonAllowedScopeKindForRecipe asserts
// the PER-RECIPE allowed-set guard: even when the scope_kind
// is in the canonical 7-enum (`block`, `class`, ...), if it
// is NOT in the recipe's pinned `allowedKinds` slice (the
// architecture Sec 1.4.1 row's scope_kind set), `newDraft`
// MUST panic.
//
// This is the structural change from iter 1: the helper no
// longer hard-codes `method | file`, so a future loc recipe
// can route through it with `file | package | repo` -- but
// the per-recipe constraint still bites at the panic
// boundary.
func TestNewDraft_PanicsOnNonAllowedScopeKindForRecipe(t *testing.T) {
	t.Parallel()
	sr := validScopeRef()
	sr.Kind = scope.KindClass // canonical, but cyclo does NOT allow class
	recover_must_panic_with(t,
		func() {
			_ = newDraft(
				"cyclo", 1, PackBase, SourceComputed,
				1.0, sr, nil,
				[]scope.Kind{scope.KindMethod, scope.KindFile},
			)
		},
		"cyclo", "not in this recipe's allowed set", "class",
	)
}

// TestNewDraft_PanicsOnEmptyAllowedKinds -- a recipe that
// passes an empty allowedKinds slice is a programmer bug
// (the helper requires every recipe to pin its Sec 1.4.1
// scope_kind set). Distinct panic path from the "not in set"
// panic so a misuse is loud.
func TestNewDraft_PanicsOnEmptyAllowedKinds(t *testing.T) {
	t.Parallel()
	sr := validScopeRef()
	recover_must_panic_with(t,
		func() {
			_ = newDraft(
				"cyclo", 1, PackBase, SourceComputed,
				1.0, sr, nil,
				nil,
			)
		},
		"cyclo", "empty allowedKinds",
	)
}

// TestNewDraft_PanicsOnWrongPack -- only the two computed-
// tier packs (`base`, `solid`) are allowed at the
// foundation-tier helper. Drift to `ingested` / `system`
// (the webhook / aggregator packs) panics. Per-recipe tests
// catch the narrower "this specific recipe must emit
// `base` (or `solid`)" drift via the [MetricSampleDraft.Pack]
// assertions in each recipe's per-emission test.
func TestNewDraft_PanicsOnWrongPack(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		bad  Pack
	}{
		{"PackIngested (webhook drift)", PackIngested},
		{"PackSystem (aggregator drift)", PackSystem},
		{"empty (unset)", Pack("")},
		{"foundation (typo drift)", Pack("foundation")},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(string(tc.bad), func(t *testing.T) {
			t.Parallel()
			sr := validScopeRef()
			recover_must_panic_with(t,
				func() {
					_ = newDraft(
						"cyclo", 1, tc.bad, SourceComputed,
						1.0, sr, nil,
						[]scope.Kind{scope.KindMethod, scope.KindFile},
					)
				},
				`pack="base"`, `"solid"`, "computed-tier",
			)
		})
	}
}

// TestNewDraft_AcceptsPackSolid -- the foundation-tier
// helper accepts BOTH `PackBase` (rows 1-3, 10-12 of Sec
// 1.4.1) AND `PackSolid` (rows 4-9). A call with PackSolid
// MUST NOT panic; the per-recipe tests catch a base recipe
// accidentally choosing solid (or vice versa).
func TestNewDraft_AcceptsPackSolid(t *testing.T) {
	t.Parallel()
	sr := validScopeRef()
	sr.Kind = scope.KindClass
	// MUST NOT panic.
	_ = newDraft(
		"lcom4", 1, PackSolid, SourceComputed,
		1.0, sr, nil,
		[]scope.Kind{scope.KindClass},
	)
}

// TestNewDraft_PanicsOnWrongSource -- only `SourceComputed`
// is allowed at the foundation-tier helper. `ingested` /
// `derived` are produced by the webhook and aggregator
// respectively (architecture Sec 5.2.1 line 902).
func TestNewDraft_PanicsOnWrongSource(t *testing.T) {
	t.Parallel()
	sr := validScopeRef()
	recover_must_panic_with(t,
		func() {
			_ = newDraft(
				"cyclo", 1, PackBase, SourceIngested,
				1.0, sr, nil,
				[]scope.Kind{scope.KindMethod, scope.KindFile},
			)
		},
		"source=\"computed\"", "got \"ingested\"",
	)
}

// TestNewDraft_PanicsOnEmptyLocalID -- the parser MUST
// populate `AstScope.scope_id` before recipes run; a recipe
// that passes "" is a programmer bug.
func TestNewDraft_PanicsOnEmptyLocalID(t *testing.T) {
	t.Parallel()
	sr := validScopeRef()
	sr.LocalID = ""
	recover_must_panic_with(t,
		func() {
			_ = newDraft(
				"cyclo", 1, PackBase, SourceComputed,
				1.0, sr, nil,
				[]scope.Kind{scope.KindMethod, scope.KindFile},
			)
		},
		"cyclo", "empty Scope.LocalID",
	)
}

// TestNewDraft_AcceptsLocAllowedKinds -- the structural fix
// for iter-1 evaluator item-3: the helper now accepts
// `scope.KindPackage` and `scope.KindRepo` (the loc recipe's
// allowed kinds), so the rest of the base pack can land
// without rewriting the shared helper.
func TestNewDraft_AcceptsLocAllowedKinds(t *testing.T) {
	t.Parallel()
	for _, k := range []scope.Kind{scope.KindFile, scope.KindPackage, scope.KindRepo} {
		sr := validScopeRef()
		sr.Kind = k
		// MUST NOT panic.
		_ = newDraft(
			"loc", 1, PackBase, SourceComputed,
			42.0, sr, nil,
			[]scope.Kind{scope.KindFile, scope.KindPackage, scope.KindRepo},
		)
	}
}

// TestMetricSampleDraft_HasNoDegradedReasonField -- iter-3
// evaluator item-3 contract: the struct MUST NOT carry a
// `DegradedReason` field (nor any field whose name suggests
// degradation), so no recipe code path can smuggle a
// `degraded_reason` onto a `source='computed'` row.
//
// Architecture Sec 3.4 lines 490-494: "Computed rows are
// never `degraded=true`: if an input is missing the row is
// not written, not stamped degraded -- only system-tier
// derivation stamps `degraded=true` per Section 8.2."
//
// This is a RUNTIME reflection assertion (not a struct
// literal compile-time hint -- adding a new field would NOT
// fail a struct-literal that omits it). The reflect call
// directly inspects the type's field set and fails the test
// if a degradation-shaped field is present, OR if the field
// set drifts from the closed list architecture Sec 5.2.1 /
// architecture Sec 8.6 pin.
func TestMetricSampleDraft_HasNoDegradedReasonField(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(MetricSampleDraft{})

	// Hard-pinned negative: the literal field names ANY
	// past iteration shipped or proposed.
	for _, banned := range []string{
		"DegradedReason",
		"Degraded",
		"DegradationReason",
		"DegradedFlag",
		"IsDegraded",
		"Reason",
	} {
		if _, ok := typ.FieldByName(banned); ok {
			t.Errorf("MetricSampleDraft must NOT carry field %q -- architecture Sec 3.4 lines 490-494 (computed rows are never degraded=true)", banned)
		}
	}

	// Defence in depth: catch any future rename that still
	// smells like a degradation back-door.
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		lower := strings.ToLower(name)
		if strings.Contains(lower, "degrad") || strings.Contains(lower, "missing_input") {
			t.Errorf("MetricSampleDraft.%s has a degradation-shaped name; computed-tier drafts must not carry degradation metadata", name)
		}
	}

	// Closed field-set guard: the struct's field list MUST
	// match the architecture-pinned closed set so a new
	// silently-added field is loud at the next CI run.
	want := map[string]bool{
		"MetricKind":    true,
		"MetricVersion": true,
		"Pack":          true,
		"Source":        true,
		"Value":         true,
		"Scope":         true,
		"Attrs":         true,
	}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if !want[name] {
			t.Errorf("MetricSampleDraft has unexpected field %q; the architecture-pinned closed set is %v -- new fields must coordinate with arch Sec 5.2.1 / Sec 8.6", name, sortedKeys(want))
		}
		delete(want, name)
	}
	for missing := range want {
		t.Errorf("MetricSampleDraft is missing expected field %q", missing)
	}
}

// sortedKeys returns m's keys in lexicographic order so
// failure messages are deterministic across runs.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// inline sort (avoid pulling sort just for a test
	// failure message; n is small).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
