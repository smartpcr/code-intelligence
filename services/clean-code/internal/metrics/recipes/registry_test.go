package recipes_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"forge/services/clean-code/internal/metrics/recipes"
)

// TestDefaultRegistry_FoundationTierRecipes is the
// implementation-plan Stage 2.3 + 2.4 scenario
// `foundation-tier-recipes-only-canonical-kinds` (lines 208,
// 221): the registry after init lists EXACTLY the six
// foundation-tier metric_kinds in sorted order --
// `{cognitive_complexity, cyclo, fan_in, fan_out, lcom4,
// loc}`. Forbidden aliases (`cyclomatic_complexity`,
// `lines_of_code`, `function_length`, `parameter_count`,
// `nesting_depth`, `incoming_calls`, `outgoing_calls`,
// `cohesion`) MUST NOT appear.
func TestDefaultRegistry_FoundationTierRecipes(t *testing.T) {
	t.Parallel()
	reg := recipes.DefaultRegistry()
	got := reg.MetricKinds()
	want := []string{
		"cognitive_complexity",
		"cyclo",
		"fan_in",
		"fan_out",
		"lcom4",
		"loc",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultRegistry().MetricKinds() = %v, want %v", got, want)
	}

	forbidden := []string{
		"cyclomatic_complexity", "lines_of_code", "function_length",
		"parameter_count", "nesting_depth", "cognitive",
		"incoming_calls", "outgoing_calls", "cohesion",
		"fanin", "fan-in", "fanout", "fan-out",
	}
	for _, bad := range forbidden {
		if reg.Lookup(bad) != nil {
			t.Errorf("Lookup(%q) returned a recipe; closed-set guard forbids this alias", bad)
		}
	}
}

// TestDefaultRegistry_DispatchByMetricKind -- each canonical
// metric_kind dispatches to a recipe whose MetricKind()
// round-trips.
func TestDefaultRegistry_DispatchByMetricKind(t *testing.T) {
	t.Parallel()
	reg := recipes.DefaultRegistry()
	for _, k := range []string{
		"cyclo",
		"cognitive_complexity",
		"loc",
		"lcom4",
		"fan_in",
		"fan_out",
	} {
		r := reg.Lookup(k)
		if r == nil {
			t.Errorf("Lookup(%q) returned nil; canonical foundation-tier recipe missing", k)
			continue
		}
		if got := r.MetricKind(); got != k {
			t.Errorf("Lookup(%q).MetricKind() = %q; round-trip drift", k, got)
		}
	}
}

// TestRegistry_DuplicateRegistrationPanics -- the closed-set
// `metric_kind` enum permits exactly one recipe per kind; a
// duplicate is always a programmer bug.
func TestRegistry_DuplicateRegistrationPanics(t *testing.T) {
	t.Parallel()
	reg := recipes.NewRegistry()
	reg.Register(recipes.NewCycloRecipe())
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on duplicate registration; got none")
		}
	}()
	reg.Register(recipes.NewCycloRecipe())
}

// TestRegistry_NilRecipeRegistrationPanics -- nil is loud.
func TestRegistry_NilRecipeRegistrationPanics(t *testing.T) {
	t.Parallel()
	reg := recipes.NewRegistry()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on nil Recipe registration; got none")
		}
	}()
	reg.Register(nil)
}

// TestRegistry_LookupMissingReturnsNil -- callers
// distinguish "not registered" from "registered but did not
// apply".
func TestRegistry_LookupMissingReturnsNil(t *testing.T) {
	t.Parallel()
	reg := recipes.NewRegistry()
	if reg.Lookup("nonexistent_kind") != nil {
		t.Fatalf("Lookup of unknown kind returned non-nil")
	}
}

// TestRegistry_MetricKindsIsSorted -- determinism contract
// (G2): two runs of the same binary produce the same
// MetricKinds slice in the same order; the recipe_manifest
// row depends on this.
func TestRegistry_MetricKindsIsSorted(t *testing.T) {
	t.Parallel()
	reg := recipes.DefaultRegistry()
	got := reg.MetricKinds()
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("MetricKinds() not sorted: %v", got)
		}
	}
}

// TestRegistry_NilSafeLookup -- a nil Registry's Lookup
// returns nil rather than nil-deref-panicking. Useful for
// tests that compose alternate registries.
func TestRegistry_NilSafeLookup(t *testing.T) {
	t.Parallel()
	var reg *recipes.Registry
	if reg.Lookup("cyclo") != nil {
		t.Fatalf("nil-Registry Lookup returned non-nil")
	}
	if reg.MetricKinds() != nil {
		t.Fatalf("nil-Registry MetricKinds returned non-nil")
	}
}

// TestRegistry_LogRegistered_EmitsStartupLine is the
// implementation-plan Stage 2.3 line 201 contract: the
// composition root MUST be able to emit one structured log
// line listing every registered recipe at startup. The line
// is required so operator-side observability can confirm
// "this binary booted with these foundation-tier recipes"
// against the `recipe_manifest` row architecture Sec 1.5
// pins.
//
// The test captures the line into an in-memory JSON handler
// and asserts:
//   - the line's message is the canonical "metric recipes registered" string,
//   - the line's count == 6 (the Stage 2.3 base pack + Stage 2.4 SOLID foundation),
//   - the line lists ALL six metric_kinds in sorted order,
//   - the line lists matching `metric_kind_versions` tokens,
//   - the line carries packs=['base','solid'] (sorted distinct) / source='computed'.
func TestRegistry_LogRegistered_EmitsStartupLine(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	reg := recipes.DefaultRegistry()
	reg.LogRegistered(logger)

	out := buf.String()
	if !strings.Contains(out, `"msg":"metric recipes registered"`) {
		t.Fatalf("startup log line missing canonical message; got: %s", out)
	}
	// Parse the structured JSON to assert the closed key set.
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("startup log line is not valid JSON: %v; raw: %s", err, out)
	}
	if got := rec["component"]; got != "recipes.registry" {
		t.Errorf("component=%v, want %q", got, "recipes.registry")
	}
	if got := rec["count"]; got != float64(6) {
		t.Errorf("count=%v, want 6 (Stage 2.3 base + Stage 2.4 SOLID: cyclo, cognitive_complexity, loc, lcom4, fan_in, fan_out)", got)
	}
	if got := rec["source"]; got != "computed" {
		t.Errorf("source=%v, want %q", got, "computed")
	}
	packs, _ := rec["packs"].([]any)
	wantPacks := []any{"base", "solid"}
	if !reflect.DeepEqual(packs, wantPacks) {
		t.Errorf("packs=%v, want %v (sorted distinct)", packs, wantPacks)
	}
	// metric_kinds and metric_kind_versions are emitted as
	// slices; JSON decodes them as []interface{}.
	kinds, _ := rec["metric_kinds"].([]any)
	wantKinds := []any{
		"cognitive_complexity",
		"cyclo",
		"fan_in",
		"fan_out",
		"lcom4",
		"loc",
	}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Errorf("metric_kinds=%v, want %v (sorted)", kinds, wantKinds)
	}
	versions, _ := rec["metric_kind_versions"].([]any)
	wantVersions := []any{
		"cognitive_complexity:v1",
		"cyclo:v1",
		"fan_in:v1",
		"fan_out:v1",
		"lcom4:v1",
		"loc:v1",
	}
	if !reflect.DeepEqual(versions, wantVersions) {
		t.Errorf("metric_kind_versions=%v, want %v", versions, wantVersions)
	}
}

// TestRegistry_LogRegistered_NilLoggerNoop -- nil logger is
// a no-op (a defensive safeguard for composition roots whose
// logger wiring lags). MUST NOT panic.
func TestRegistry_LogRegistered_NilLoggerNoop(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("LogRegistered with nil logger panicked: %v", r)
		}
	}()
	recipes.DefaultRegistry().LogRegistered(nil)
}

// TestRegistry_LogRegistered_NilReceiverNoop -- a nil
// *Registry's LogRegistered is a no-op. Same defensive
// shape as [Registry.Lookup] / [Registry.MetricKinds].
func TestRegistry_LogRegistered_NilReceiverNoop(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil-Registry LogRegistered panicked: %v", r)
		}
	}()
	var reg *recipes.Registry
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	reg.LogRegistered(logger)
	if buf.Len() != 0 {
		t.Errorf("nil-Registry LogRegistered emitted %d bytes; want 0", buf.Len())
	}
}

// TestDefaultRegistryWithLog_LogsAndReturns -- the
// composition-root convenience constructor BOTH constructs
// the canonical base-pack registry AND emits the startup
// log line, so a single call in `main.go` realises the
// impl-plan Stage 2.3 line 201 contract.
func TestDefaultRegistryWithLog_LogsAndReturns(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	reg := recipes.DefaultRegistryWithLog(logger)

	if reg == nil {
		t.Fatalf("DefaultRegistryWithLog returned nil")
	}
	want := []string{
		"cognitive_complexity",
		"cyclo",
		"fan_in",
		"fan_out",
		"lcom4",
		"loc",
	}
	if got := reg.MetricKinds(); !reflect.DeepEqual(got, want) {
		t.Errorf("MetricKinds()=%v, want %v", got, want)
	}
	if !strings.Contains(buf.String(), "metric recipes registered") {
		t.Errorf("DefaultRegistryWithLog did not emit the startup line; buf=%s", buf.String())
	}
}

// TestDefaultRegistry_NoStartupLogSideEffect -- DefaultRegistry
// (no -WithLog suffix) MUST NOT log via any global sink. The
// startup log line is the composition root's job and tests
// that compose alternate registries must not get a stray
// info line on slog.Default().
//
// We swap slog.Default() to a buffer, call DefaultRegistry,
// and assert the buffer is empty.
func TestDefaultRegistry_NoStartupLogSideEffect(t *testing.T) {
	// Cannot run t.Parallel because we mutate the process
	// default logger.
	prev := slog.Default()
	defer slog.SetDefault(prev)

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	_ = recipes.DefaultRegistry()
	if buf.Len() != 0 {
		t.Errorf("DefaultRegistry must be side-effect-free; got %d bytes on slog.Default: %s", buf.Len(), buf.String())
	}
}
