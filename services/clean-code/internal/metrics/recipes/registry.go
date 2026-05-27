package recipes

import (
	"fmt"
	"log/slog"
	"sort"
)

// Registry is the foundation-tier recipe registry --
// architecture Sec 8.6 line 1008 "the Compute Engine consults
// a registry keyed by (language, metric_kind, metric_version)
// to dispatch the correct Recipe per AstFile".
//
// This is the in-process registry; the per-language /
// per-version dispatch lives in a later stage. At Stage 2.3
// the registry is keyed by `metric_kind` alone -- the recipes
// in this package are language-agnostic at the API boundary
// (per-language specialisation is the parser fleet's job,
// Sec 4.5).
//
// The Registry is intentionally NOT a global mutable
// singleton: every caller constructs its own via
// [DefaultRegistry], so tests can compose alternate sets
// without leaking state between table runs. The default
// constructor registers the canonical foundation-tier base
// pack (cyclo, cognitive_complexity, loc -- the three
// `pack='base'` AST-emitted kinds per impl-plan Stage 2.3).
type Registry struct {
	byKind map[string]Recipe
}

// NewRegistry returns an empty [Registry]. Call
// [Registry.Register] to add recipes; or use
// [DefaultRegistry] for the canonical Stage 2.3 base pack.
func NewRegistry() *Registry {
	return &Registry{byKind: map[string]Recipe{}}
}

// Register adds `r` to the registry keyed by `r.MetricKind()`.
// Re-registering the same `metric_kind` PANICS rather than
// silently overwriting -- the closed-set guard (architecture
// Sec 5.2.1 line 900 `metric_kind` enum) means a duplicate is
// always a programmer bug, never an intentional override.
func (reg *Registry) Register(r Recipe) {
	if r == nil {
		panic("recipes: Registry.Register received nil Recipe")
	}
	kind := r.MetricKind()
	if kind == "" {
		panic("recipes: Registry.Register received Recipe with empty MetricKind()")
	}
	if _, exists := reg.byKind[kind]; exists {
		panic(fmt.Sprintf("recipes: duplicate registration for metric_kind=%q (every metric_kind has exactly one recipe per process)", kind))
	}
	reg.byKind[kind] = r
}

// Lookup returns the registered [Recipe] for `metricKind` or
// nil when no recipe is registered. Callers MUST distinguish
// "not registered" (nil) from "registered but did not apply
// to this AST" (recipe present, `AppliesTo` returns false).
func (reg *Registry) Lookup(metricKind string) Recipe {
	if reg == nil {
		return nil
	}
	return reg.byKind[metricKind]
}

// MetricKinds returns the metric_kind values registered in
// sorted order. Deterministic ordering is part of G2: a
// snapshot of "what recipes are registered at startup" must
// be identical across two runs of the same binary, which is
// what `audit.recipe_manifest` writes (architecture Sec 1.5
// "definitional snapshot").
func (reg *Registry) MetricKinds() []string {
	if reg == nil {
		return nil
	}
	out := make([]string, 0, len(reg.byKind))
	for k := range reg.byKind {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Recipes returns the registered recipes in sorted
// `metric_kind` order. Same determinism contract as
// [Registry.MetricKinds].
func (reg *Registry) Recipes() []Recipe {
	kinds := reg.MetricKinds()
	out := make([]Recipe, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, reg.byKind[k])
	}
	return out
}

// DefaultRegistry returns a fresh registry populated with
// the canonical Stage 2.3 + 2.4 foundation-tier recipes:
//
//   - Stage 2.3 base pack: `cyclo`, `cognitive_complexity`,
//     `loc` (architecture Sec 1.4.1 rows 1-3).
//   - Stage 2.4 SOLID pack foundation: `lcom4`, `fan_in`,
//     `fan_out` (architecture Sec 1.4.1 rows 4-6).
//
// The base pack is the entirety of `pack='base'` AST-emitted
// kinds at this stage (`cycle_member` and `duplication_ratio`
// land in Stage 2.5, `modification_count_in_window` lands in
// Stage 2.6 as a materialiser, not a recipe). The SOLID
// foundation set is the three structural-coupling /
// cohesion recipes pinned by impl-plan Stage 2.4 line 221.
//
// The exact registered set is asserted by
// [TestDefaultRegistry_FoundationTierRecipes] (the renamed
// successor of `base-recipes-only-canonical-kinds`).
//
// DefaultRegistry is INTENTIONALLY side-effect-free: it does
// NOT touch any logger. The composition root (clean-coded
// `main.go`) calls [DefaultRegistryWithLog] (or the
// equivalent [Registry.LogRegistered] on a custom registry)
// to emit the startup log line required by
// implementation-plan Stage 2.3 line 201.
func DefaultRegistry() *Registry {
	reg := NewRegistry()
	// Stage 2.3 base pack.
	reg.Register(NewCycloRecipe())
	reg.Register(NewCognitiveComplexityRecipe())
	reg.Register(NewLocRecipe())
	// Stage 2.4 SOLID pack foundation (impl-plan line 221).
	reg.Register(NewLCOM4Recipe())
	reg.Register(NewFanInRecipe())
	reg.Register(NewFanOutRecipe())
	return reg
}

// DefaultRegistryWithLog is the composition-root convenience:
// construct the canonical Stage 2.3 base-pack registry AND
// emit the startup log line listing the registered recipes
// (implementation-plan Stage 2.3 line 201 -- "emit a startup
// log line listing the registered base-pack recipes").
//
// `logger` MAY be nil, in which case the registry is returned
// without logging (callers that genuinely want the silent
// path -- e.g. unit tests composing alternate registries --
// can call [DefaultRegistry] directly, but a nil here is also
// accepted as a no-op safeguard so the composition root does
// not crash if logger wiring lags). Use [DefaultRegistry] in
// tests that want pure construction.
func DefaultRegistryWithLog(logger *slog.Logger) *Registry {
	reg := DefaultRegistry()
	reg.LogRegistered(logger)
	return reg
}

// LogRegistered emits a single deterministic INFO-level
// structured log line listing every registered recipe by
// metric_kind + version + pack, in sorted order
// (implementation-plan Stage 2.3 line 201).
//
// The line carries:
//
//   - `component`: literal "recipes.registry" so a `grep -nF`
//     over CI logs lands every startup snapshot.
//   - `count`: the number of registered recipes.
//   - `metric_kinds`: the sorted slice of metric_kind strings.
//   - `metric_kind_versions`: the sorted slice of
//     "<metric_kind>:v<version>" tokens (the same set the
//     `recipe_manifest` row will eventually pin -- architecture
//     Sec 1.5 "definitional snapshot").
//   - `packs`: the SORTED DISTINCT slice of metric-pack labels
//     across the registered recipes (e.g. `["base", "solid"]`
//     once Stage 2.4 lands). Replaces the prior single-pack
//     literal -- the registry now hosts recipes from multiple
//     packs.
//   - `source`: literal "computed" -- the entire foundation
//     tier emits `source='computed'` rows (architecture Sec
//     1.4.1 column 4 is `source` on the metric ROW; every
//     row this registry's recipes emit is computed-tier).
//
// LogRegistered is a no-op when `reg == nil` or
// `logger == nil`; both cases are well-defined ("nothing to
// log" / "logger not wired yet"). The log key set is closed
// (no random or per-process state), so two startups of the
// same binary emit identical lines (G2).
func (reg *Registry) LogRegistered(logger *slog.Logger) {
	if reg == nil || logger == nil {
		return
	}
	kinds := reg.MetricKinds()
	versions := make([]string, 0, len(kinds))
	packSet := map[Pack]bool{}
	for _, k := range kinds {
		r, ok := reg.byKind[k]
		if !ok || r == nil {
			// Defence: a non-nil entry must exist for every
			// key MetricKinds returned. If it does not,
			// skip silently rather than panic in startup
			// path.
			continue
		}
		versions = append(versions, fmt.Sprintf("%s:v%d", k, r.Version()))
		packSet[r.Pack()] = true
	}
	packs := make([]string, 0, len(packSet))
	for p := range packSet {
		packs = append(packs, string(p))
	}
	sort.Strings(packs)
	logger.Info("metric recipes registered",
		slog.String("component", "recipes.registry"),
		slog.Int("count", len(kinds)),
		slog.Any("metric_kinds", kinds),
		slog.Any("metric_kind_versions", versions),
		slog.Any("packs", packs),
		slog.String("source", string(SourceComputed)),
	)
}
