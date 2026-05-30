// Package effort implements the cleanc CLI's deterministic
// effort-estimator fallback (story REFACTOR-GUIDE, Phase
// Foundations, Stage 1.3).
//
// # Why this exists
//
// The production Refactor Planner (Stage 8.3 of the CLEAN-CODE
// story) loads an ONNX model whose semantic version is pinned
// by `PolicyVersion.RefactorWeights.EffortModelVersion`. The
// CLI runs on a developer laptop where the ONNX artefact is
// almost never present; when the loader returns an error the
// CLI substitutes the deterministic formula pinned in the
// REFACTOR-GUIDE tech-spec Section 8.5:
//
//	base_hours = 0.02*loc + 0.10*cyclo + 0.05*fan_in + 1.0
//	adjusted  = base_hours * task_kind_factor[TaskKind]
//	clamped   = max(0.1, min(80.0, adjusted))
//	result    = round_half_up(clamped, 1)
//
// `task_kind_factor` (REFACTOR-GUIDE tech-spec Sec 8.5 table
// 965-971):
//
//   - split_class             1.5
//   - invert_dependency       1.3
//   - break_cycle             1.4
//   - extract_method          0.7
//   - consolidate_duplication 1.0
//
// # Why a separate package (not refactor.FallbackEffortModel)
//
// The `refactor` package already ships [refactor.ZeroEffortModel],
// [refactor.HeuristicEffortModel], and [refactor.MLEffortModel]
// for the CLEAN-CODE service. Adding a fourth implementation
// there would couple the service's effort-source vocabulary to
// the CLI's new operator-pin `cli-effort-fallback-formula`
// (REFACTOR-GUIDE architecture Sec 1.3). Keeping the fallback
// in `internal/cli/effort` preserves the service's
// "ML-or-placeholder" world view while letting the CLI's
// composition root wire a richer fallback via the existing
// [refactor.WithEffortModel] option seam
// (`services/clean-code/internal/refactor/task_planner.go:719-740`).
//
// # Inputs
//
// The estimator consumes raw (loc, cyclo, fan_in) values per
// scope. The CLI orchestrator builds a one-time index of the
// same `[]refactor.InMemoryMetricSample` rows it already
// loaded into the planner's [refactor.InMemoryMetricSampleReader]
// and exposes it as an [EffortInputProvider]. This satisfies
// tech-spec Sec 8.5 lines 973-975 ("inputs come from the same
// in-memory `MetricSampleReader` rows the Planner used")
// without re-querying the reader at `Estimate` time.
//
// [refactor.HotSpot.Breakdown] is deliberately NOT an input
// source; it only carries z-scores (ComplexityZ / ChurnZ /
// CouplingZ per
// `services/clean-code/internal/refactor/hotspot.go:264-284`),
// not the raw loc / fan_in numbers the formula needs.
package effort

import (
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/refactor"
)

// FormulaVersion stamps the deterministic fallback formula
// pinned in REFACTOR-GUIDE tech-spec Sec 8.5. Bump when the
// coefficients or clamp window change so audit logs can
// attribute estimates to the right revision of the formula.
// Matches the [refactor.PolicySnapshot.Weights.EffortModelVersion]
// value the dev-mode policy loader (Stage 1.4) writes when no
// ONNX model is configured (REFACTOR-GUIDE architecture
// Section 4.5 table row `RefactorWeights.EffortModelVersion`).
const FormulaVersion = "fallback-2026.05"

// EffortSource enumerates the provenance tags every CLI
// `effort_hours` value carries. Consumed by the Phase 4
// `RefactorPromptRecord.effort_source` writer (REFACTOR-GUIDE
// architecture Sec 4.6 table row `effort_source`) and by the
// `--diagnostics` JSON's `effort_mode` field
// (REFACTOR-GUIDE tech-spec C15).
type EffortSource = string

const (
	// EffortSourceML is the stamp the orchestrator uses when
	// the production ONNX model loaded successfully. The CLI
	// itself never produces this tag; the constant lives here
	// so call sites import a single closed enum.
	EffortSourceML EffortSource = "ml"

	// EffortSourceFallback is the stamp produced by every
	// [FallbackModel.Estimate] call. Pinned by REFACTOR-GUIDE
	// tech-spec C15 as the exact string `"fallback"`.
	EffortSourceFallback EffortSource = "fallback"
)

// EffortInputProvider resolves a refactor task's `ScopeID` to
// the raw (loc, cyclo, fan_in) inputs the fallback formula
// consumes. Implementations are expected to build the index
// ONCE (e.g. by walking the same `[]refactor.InMemoryMetricSample`
// rows the planner loaded into [refactor.InMemoryMetricSampleReader])
// and serve per-call lookups in O(1).
//
// Contract:
//
//   - The boolean `ok` reports whether the provider has ANY
//     row for the given `scopeID`. A scope that has no metric
//     samples at all (e.g. an orphan id from a stale fixture)
//     yields `ok=false`; in that case the formula treats every
//     input as `0` and the result is the per-kind floor
//     `1.0 * factor` clamped to `[0.1, 80.0]`.
//   - When `ok=true` the (loc, cyclo, fanIn) tuple carries
//     EVERY input the provider has. Individual dark metrics
//     (e.g. `cyclo` not emitted because the parser does not
//     stamp `decision_blocks` for the file's language) are
//     returned as `0` — the corresponding term then contributes
//     `0` to the formula, exactly as REFACTOR-GUIDE tech-spec
//     Sec 8.5 final paragraph requires.
//   - Implementations MUST be goroutine-safe: [refactor.TaskPlanner]
//     does not serialise [refactor.EffortModel.Estimate] calls.
type EffortInputProvider func(scopeID uuid.UUID) (loc, cyclo, fanIn float64, ok bool)

// Option configures a [FallbackModel] at construction time.
// Functional-options pattern mirrors [refactor.TaskOption]
// so the two effort estimators feel symmetric to a caller
// composing both into one binary.
type Option func(*FallbackModel)

// WithInputSource installs the [EffortInputProvider] the
// estimator consults at each [FallbackModel.Estimate] call.
// A nil provider is tolerated: the estimator then behaves as
// if every scope had `ok=false`, i.e. all inputs zero. This
// is the documented "loaded the model but no metrics ingested"
// behaviour (REFACTOR-GUIDE e2e-scenarios.md "Dark-metric
// inputs contribute zero" scenario).
func WithInputSource(p EffortInputProvider) Option {
	return func(m *FallbackModel) {
		m.provider = p
	}
}

// WithSourceTag overrides the [EffortSource] string the
// [FallbackModel.Mode] accessor returns. Defaults to
// [EffortSourceFallback]. The override is intended for
// orchestrators that compose multiple fallback estimators
// behind one label; production CLI deployments leave the
// default in place so the Phase 4 prompt emitter stamps
// `"fallback"` per REFACTOR-GUIDE tech-spec C15.
//
// An empty tag is ignored (the default stays); a non-empty
// tag is taken verbatim. Per-character validation is the
// caller's responsibility — the prompt-record schema
// (REFACTOR-GUIDE architecture Sec 4.6 row `effort_source`)
// pins the closed set `{"ml", "fallback"}` and the orchestrator
// is the natural enforcement point.
func WithSourceTag(tag string) Option {
	return func(m *FallbackModel) {
		if tag != "" {
			m.sourceTag = tag
		}
	}
}

// WithLogWriter is a test seam used by the package's own
// tests to capture the first-invocation WARNING line. Not
// part of the public composition-root surface; the production
// callers use [New]'s `logger` argument.
//
// The seam exists because [New] resolves a nil logger to
// [slog.Default] (a global), and unit tests need a
// per-test sink without mutating package state.
func withLogWriter(w io.Writer) Option {
	return func(m *FallbackModel) {
		if w != nil {
			m.logger = slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelWarn}))
		}
	}
}

// FallbackModel is the deterministic effort estimator the
// cleanc CLI substitutes when the production ONNX model is
// absent. Implements [refactor.EffortModel].
//
// Thread safety: every field is read-only after [New] returns
// except for [sync.Once] (which is internally serialised) so
// concurrent [FallbackModel.Estimate] calls are safe without
// further locking. The injected [EffortInputProvider] is
// expected to be goroutine-safe (see its doc).
type FallbackModel struct {
	logger    *slog.Logger
	provider  EffortInputProvider
	sourceTag string
	warnOnce  sync.Once
}

// compile-time check: FallbackModel satisfies refactor.EffortModel.
var _ refactor.EffortModel = (*FallbackModel)(nil)

// New constructs a [FallbackModel] with the supplied logger
// and zero or more [Option]s. A nil logger resolves to
// [slog.Default]; a nil-or-omitted [WithInputSource] yields a
// model whose [FallbackModel.Estimate] treats every scope as
// having no inputs (all-zero formula); a nil-or-omitted
// [WithSourceTag] leaves [FallbackModel.Mode] returning
// [EffortSourceFallback].
//
// The return type is the concrete `*FallbackModel` so the
// orchestrator's diagnostics writer can call
// [FallbackModel.Mode] without a type assertion. The pointer
// still satisfies [refactor.EffortModel] via the compile-time
// assertion above, so callers wiring the model into
// [refactor.WithEffortModel] do not need to spell out the
// interface type at the call site.
func New(logger *slog.Logger, opts ...Option) *FallbackModel {
	if logger == nil {
		logger = slog.Default()
	}
	m := &FallbackModel{
		logger:    logger,
		sourceTag: EffortSourceFallback,
	}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt(m)
	}
	return m
}

// Mode returns the provenance tag this estimator stamps on
// every produced `effort_hours`. Defaults to
// [EffortSourceFallback]; configurable via [WithSourceTag] for
// orchestrators that route multiple fallback estimators behind
// one label. The orchestrator's diagnostics writer surfaces
// this string in every `RunArtifact` (REFACTOR-GUIDE tech-spec
// C15) and the Phase 4 prompt emitter writes it into
// `RefactorPromptRecord.effort_source` (REFACTOR-GUIDE
// architecture Sec 4.6 row `effort_source`).
func (m *FallbackModel) Mode() string {
	return m.sourceTag
}

// FormulaVersion returns the version string the estimator
// stamps in its first-invocation WARNING log line. Exposed so
// the orchestrator's diagnostics writer can record the
// formula revision alongside `effort_source`.
func (m *FallbackModel) FormulaVersion() string {
	return FormulaVersion
}

// Estimate implements [refactor.EffortModel]. Returns the
// per-task `effort_hours` value the deterministic formula
// computes. Never returns a non-finite or out-of-range value
// (the clamp guarantees `[0.1, 80.0]`). The only error path
// is an invalid [refactor.TaskKind] — every other branch
// produces a sanitised finite estimate, so the planner's
// "abort the batch on EffortModel error" contract
// (`services/clean-code/internal/refactor/effort_model.go:130-142`)
// is honoured ONLY when a non-canonical task kind reaches the
// emitter (which `refactor.ValidateTaskKind` upstream of this
// call already prevents).
//
// The first call across the model's lifetime emits ONE
// WARNING line so the operator sees in their CLI output that
// the heuristic is in play rather than a trained ML model
// (REFACTOR-GUIDE architecture Sec 3.5 + Sec 3.9). The
// [sync.Once] gate keeps the log volume tied to "estimator
// constructed and used at least once" rather than "called
// N times for N tasks".
func (m *FallbackModel) Estimate(task refactor.RefactorTask, _ refactor.HotSpot, _ refactor.PolicySnapshot) (float64, error) {
	if err := refactor.ValidateTaskKind(task.Kind); err != nil {
		return 0, fmt.Errorf("effort.FallbackModel.Estimate: %w", err)
	}
	m.warnOnce.Do(m.emitFirstInvocationWarning)

	loc, cyclo, fanIn := m.lookupInputs(task.ScopeID)
	base := 0.02*loc + 0.10*cyclo + 0.05*fanIn + 1.0
	adjusted := base * taskKindFactor(task.Kind)
	clamped := clamp(adjusted, fallbackMinHours, fallbackMaxHours)
	return roundHalfUpOneDecimal(clamped), nil
}

// lookupInputs runs the configured [EffortInputProvider] for
// the given scope and sanitises the returned values. A nil
// provider, or one that signals `ok=false`, yields three zeros
// — exactly what REFACTOR-GUIDE tech-spec Sec 8.5 final
// paragraph mandates for missing inputs.
func (m *FallbackModel) lookupInputs(scopeID uuid.UUID) (loc, cyclo, fanIn float64) {
	if m.provider == nil {
		return 0, 0, 0
	}
	l, c, f, ok := m.provider(scopeID)
	if !ok {
		return 0, 0, 0
	}
	return sanitiseInput(l), sanitiseInput(c), sanitiseInput(f)
}

// emitFirstInvocationWarning logs the canonical WARNING line
// the impl-plan Stage 1.3 step 4 pins. The literal substring
// `"deterministic fallback formula"` is verified by the
// REFACTOR-GUIDE e2e-scenarios.md "Effort fallback advertises
// its mode in diagnostics" feature.
func (m *FallbackModel) emitFirstInvocationWarning() {
	m.logger.Warn(
		"effort estimator using deterministic fallback formula "+FormulaVersion+" (no ONNX model loaded)",
		slog.String("formula_version", FormulaVersion),
		slog.String("effort_source", EffortSourceFallback),
	)
}

// -----------------------------------------------------------------------------
// Constants + helpers
// -----------------------------------------------------------------------------

// fallbackMinHours / fallbackMaxHours pin the clamp window
// (REFACTOR-GUIDE tech-spec Sec 8.5 line 959). Pinned as
// named constants so a future spec revision moves the bound
// at exactly one place.
const (
	fallbackMinHours = 0.1
	fallbackMaxHours = 80.0
)

// taskKindFactor returns the per-kind multiplier pinned in
// REFACTOR-GUIDE tech-spec Sec 8.5 table 965-971. Unknown
// kinds yield `1.0` defensively, but the caller is expected
// to have run [refactor.ValidateTaskKind] FIRST so this branch
// is unreachable in production paths. A switch (vs a map)
// makes the table immutable at the language level and lets
// the compiler check exhaustiveness via go vet's
// future-typed-switch heuristic.
func taskKindFactor(k refactor.TaskKind) float64 {
	switch k {
	case refactor.TaskKindSplitClass:
		return 1.5
	case refactor.TaskKindInvertDependency:
		return 1.3
	case refactor.TaskKindBreakCycle:
		return 1.4
	case refactor.TaskKindExtractMethod:
		return 0.7
	case refactor.TaskKindConsolidateDuplication:
		return 1.0
	default:
		return 1.0
	}
}

// sanitiseInput coerces non-finite or negative inputs to
// zero. The Planner's [refactor.MetricSampleReader] already
// rejects non-finite `metric_sample.value`s upstream of this
// call, but the [EffortInputProvider] is an unconstrained
// caller-supplied func — defending here keeps the estimator's
// "never returns NaN" invariant locally provable rather than
// relying on a global property of the upstream pipeline.
func sanitiseInput(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}

// clamp returns `v` clipped to the closed interval `[lo, hi]`.
// Tiny helper rather than inlining `math.Max(lo, math.Min(hi, v))`
// because the latter inverts the bounds (lo on min, hi on
// max) when typed by a hurried reader, and the fallback's
// bounds are policy-critical.
func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// roundHalfUpOneDecimal rounds `v` to one decimal place using
// the "round half away from zero" rule (REFACTOR-GUIDE
// tech-spec Sec 8.5 line 960: `round_half_up`). For the
// non-negative inputs the clamp produces this is equivalent
// to `floor(v*10 + 0.5)/10`; the explicit negative branch is
// belt-and-braces for a hypothetical future spec that lifts
// the lower clamp.
func roundHalfUpOneDecimal(v float64) float64 {
	if v >= 0 {
		return math.Floor(v*10+0.5) / 10
	}
	return -math.Floor((-v)*10+0.5) / 10
}
