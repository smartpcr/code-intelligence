package recipes

import (
	"fmt"
	"log/slog"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
)

// ProjectRecipe is the contract every metric_kind whose value
// requires the WHOLE project's `*parser.AstFile` set (not a
// single file) satisfies. The two foundation-tier recipes in
// this group are `cycle_member` (import-graph SCC detection)
// and `duplication_ratio` at `scope_kind='package'` (cross-
// file token-window comparison). Both also satisfy the per-
// file [Recipe] interface so they can sit alongside per-file
// recipes in the Compute Engine; the [ProjectRecipe]
// interface is the dispatch hook for the project-wide call
// path.
//
// Architecture: the Compute Engine (Sec 3.3) walks every
// `*AstFile` and dispatches per-file [Recipe]s, then makes a
// SECOND pass over the same slice dispatching [ProjectRecipe]s.
// Project-level recipes are pure / stateless / read-only
// (same invariants as [Recipe]); the only API difference is
// the input type.
type ProjectRecipe interface {
	// MetricKind returns the canonical metric_kind string
	// (architecture Sec 1.4.1 column 2).
	MetricKind() string
	// Version is the recipe's `version()` per Sec 8.6 line
	// 1010.
	Version() int
	// ComputeProject consumes the project's AstFile set and
	// returns the draft rows the Metric Ingestor will insert
	// into `MetricSample`. May return nil when the project
	// has no input -- there is no [Recipe.AppliesTo] gate at
	// the project level because the per-file degraded-skip
	// is the recipe's internal concern.
	ComputeProject(asts []*parser.AstFile) []MetricSampleDraft
}

// ProjectRegistry is the in-process dispatch table for
// [ProjectRecipe]s. It mirrors [Registry] but holds project-
// level rather than per-file recipes. Constructed via
// [DefaultProjectRegistry] for the canonical Stage 2.5 set
// (`cycle_member`, `duplication_ratio`).
type ProjectRegistry struct {
	byKind map[string]ProjectRecipe
}

// NewProjectRegistry returns an empty [ProjectRegistry]. Use
// [DefaultProjectRegistry] for the canonical Stage 2.5 base
// pack.
func NewProjectRegistry() *ProjectRegistry {
	return &ProjectRegistry{byKind: map[string]ProjectRecipe{}}
}

// Register adds `r` to the registry keyed by `r.MetricKind()`.
// Re-registering the same `metric_kind` PANICS rather than
// silently overwriting -- the closed-set guard (architecture
// Sec 5.2.1 line 900 `metric_kind` enum) means a duplicate is
// always a programmer bug, never an intentional override.
func (reg *ProjectRegistry) Register(r ProjectRecipe) {
	if r == nil {
		panic("recipes: ProjectRegistry.Register received nil ProjectRecipe")
	}
	kind := r.MetricKind()
	if kind == "" {
		panic("recipes: ProjectRegistry.Register received ProjectRecipe with empty MetricKind()")
	}
	if _, exists := reg.byKind[kind]; exists {
		panic(fmt.Sprintf("recipes: duplicate registration for project metric_kind=%q", kind))
	}
	reg.byKind[kind] = r
}

// Lookup returns the registered [ProjectRecipe] for
// `metricKind` or nil when no recipe is registered.
func (reg *ProjectRegistry) Lookup(metricKind string) ProjectRecipe {
	if reg == nil {
		return nil
	}
	return reg.byKind[metricKind]
}

// MetricKinds returns the sorted slice of registered
// metric_kinds for deterministic enumeration.
func (reg *ProjectRegistry) MetricKinds() []string {
	if reg == nil {
		return nil
	}
	out := make([]string, 0, len(reg.byKind))
	for k := range reg.byKind {
		out = append(out, k)
	}
	sortStringsInPlace(out)
	return out
}

// All returns every registered [ProjectRecipe] in
// deterministic (metric_kind-sorted) order.
func (reg *ProjectRegistry) All() []ProjectRecipe {
	kinds := reg.MetricKinds()
	out := make([]ProjectRecipe, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, reg.byKind[k])
	}
	return out
}

// DefaultProjectRegistry constructs and returns the canonical
// Stage 2.5 project-level base-pack registry: cycle_member
// and duplication_ratio. Both recipes also implement
// [Recipe], but their cross-file substance lives in
// [ProjectRecipe.ComputeProject]; registering them HERE (not
// in [DefaultRegistry]) keeps the per-file `DefaultRegistry`
// test assertion `{cyclo, cognitive_complexity, loc}` intact
// while still exposing the project-level dispatch path.
//
// The exact registered set is asserted by
// `TestDefaultProjectRegistry_Stage25BasePack`.
func DefaultProjectRegistry() *ProjectRegistry {
	reg := NewProjectRegistry()
	reg.Register(NewCycleMemberRecipe())
	reg.Register(NewDuplicationRatioRecipe())
	return reg
}

// DefaultProjectRegistryWithLog is the composition-root
// convenience: construct [DefaultProjectRegistry] AND emit
// the startup log line listing the registered project-level
// recipes (mirrors [DefaultRegistryWithLog] for the per-file
// registry; together they make every recipe the Compute
// Engine will dispatch visible in the boot snapshot, per
// implementation-plan Stage 2.5 line 244 + Stage 2.3 line
// 201).
//
// `logger` MAY be nil, in which case the registry is returned
// without logging. The composition root in
// `cmd/clean-coded/main.go` calls THIS variant alongside
// [DefaultRegistryWithLog] so both the per-file and the
// project-level base-pack registries appear in the startup
// log -- the operator's first signal that the Stage 2.5
// recipes are wired.
func DefaultProjectRegistryWithLog(logger *slog.Logger) *ProjectRegistry {
	reg := DefaultProjectRegistry()
	reg.LogRegistered(logger)
	return reg
}

// LogRegistered emits a single deterministic INFO-level
// structured log line listing every registered project-level
// recipe by metric_kind + version, in sorted order. The line
// shape mirrors [Registry.LogRegistered] so a `grep -nF
// "recipes.project_registry"` over CI logs lands every
// startup snapshot of the project-level registry.
//
// The line carries:
//
//   - `component`: literal "recipes.project_registry".
//   - `count`: the number of registered recipes.
//   - `metric_kinds`: the sorted slice of metric_kind strings.
//   - `metric_kind_versions`: the sorted slice of
//     "<metric_kind>:v<version>" tokens.
//   - `pack`: literal "base".
//   - `source`: literal "computed".
//
// LogRegistered is a no-op when `reg == nil` or
// `logger == nil`; both cases are well-defined ("nothing to
// log" / "logger not wired yet"). The log key set is closed
// (no random or per-process state), so two startups of the
// same binary emit identical lines (G2).
func (reg *ProjectRegistry) LogRegistered(logger *slog.Logger) {
	if reg == nil || logger == nil {
		return
	}
	kinds := reg.MetricKinds()
	versions := make([]string, 0, len(kinds))
	for _, k := range kinds {
		r, ok := reg.byKind[k]
		if !ok || r == nil {
			continue
		}
		versions = append(versions, fmt.Sprintf("%s:v%d", k, r.Version()))
	}
	logger.Info("metric project recipes registered",
		slog.String("component", "recipes.project_registry"),
		slog.Int("count", len(kinds)),
		slog.Any("metric_kinds", kinds),
		slog.Any("metric_kind_versions", versions),
		slog.String("pack", string(PackBase)),
		slog.String("source", string(SourceComputed)),
	)
}

// sortStringsInPlace is a tiny dependency-free sort to avoid
// pulling `sort` into this file's import list (the rest of
// the registry stays stdlib-minimal).
func sortStringsInPlace(s []string) {
	// Insertion sort: registries are small (foundation tier
	// has 2-5 entries) so the simplest stable algorithm is
	// fine.
	for i := 1; i < len(s); i++ {
		j := i
		for j > 0 && s[j-1] > s[j] {
			s[j-1], s[j] = s[j], s[j-1]
			j--
		}
	}
}
