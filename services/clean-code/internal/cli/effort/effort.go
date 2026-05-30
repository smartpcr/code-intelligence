// -----------------------------------------------------------------------
// <copyright file="effort.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

// Package effort declares the [Estimator] contract every
// refactor-task effort source in the CLI binary honours, plus
// the canonical task-kind / metric-input / result row shapes the
// contract operates over.
//
// # Why split from fallback.go
//
// `fallback.go` carries the deterministic linear [FallbackModel]
// (the only effort source guaranteed available in every build).
// `effort.go` (this file) carries the SHAPE every source --
// fallback OR a future ONNX-backed model -- must conform to so
// the orchestrator can swap one for the other without branching.
//
// Splitting the contract from its first implementation is what
// lets later stages drop in `OnnxModel` (Stage 3.x) behind the
// same `Estimator` value without touching `fallback.go`.
//
// # Spec anchors
//
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md`
//     Sec 4.5 ("RefactorTask" -- effort field row shape).
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md`
//     Sec 5.5 ("L5 -- Refactor Planner") -- the planner consumes
//     an [Estimator] via the orchestrator's wiring; without this
//     interface the planner could not be wired against the
//     deterministic fallback for offline CLI runs.
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/tech-spec.md`
//     Sec 9.3 ("Effort model fallback") -- pins the contract a
//     dev build follows when the ONNX runtime is missing.
//   - `docs/stories/code-intelligence-REFACTOR-GUIDE/implementation-plan.md`
//     Stage 1.3 ("Effort Estimator Fallback") -- the workstream
//     this contract belongs to.
package effort

// Estimator is the contract every refactor-task effort source
// implements. The orchestrator stamps the resulting hours onto
// each [refactor.RefactorTask] without caring whether the
// source is the deterministic [FallbackModel] or a future
// ONNX-backed model.
//
// Contract notes (binding on all implementations):
//
//   - Estimate MUST be deterministic for a given
//     `(MetricInput, TaskKind)` pair within a single binary
//     instance; the same inputs MUST yield the same output
//     bytes within and across calls. Re-runs across processes
//     are only required to match for sources whose underlying
//     model itself is deterministic (the [FallbackModel] is;
//     an ONNX model may not be if it loads non-deterministic
//     weights).
//   - An unknown [TaskKind] MUST return a non-nil error and
//     the zero-value [EstimateResult]; implementations MUST
//     NOT panic. Callers rely on this to surface "unknown
//     task kind" as a soft warning rather than a binary
//     crash.
//   - Negative or zero values in [MetricInput] are PERMITTED
//     -- the linear model clamps the final result to a
//     positive band ([ClampMinHours], [ClampMaxHours]).
//     Implementations that cannot accept negative inputs
//     (e.g. an ONNX model trained on non-negative features)
//     MUST clamp internally before evaluation rather than
//     erroring.
//   - Name MUST return a stable, lowercase, snake_case
//     identifier (e.g. "fallback", "onnx_v1"). The
//     diagnostics sink writes it into the per-task
//     `effort_source` field so a future operator can grep
//     which source produced a given hours value. The string
//     is a STABLE identifier; renaming it is a
//     cross-version migration concern.
type Estimator interface {
	// Estimate computes effort hours for the supplied input
	// + task kind. See the Estimator contract notes for the
	// error / determinism / clamping rules every
	// implementation honours.
	Estimate(input MetricInput, kind TaskKind) (EstimateResult, error)

	// Name returns the stable, lowercase, snake_case
	// identifier of this estimator source. Used by the
	// diagnostics sink to label which source produced each
	// hours value.
	Name() string
}

// FallbackEstimatorName is the [Estimator.Name] value the
// deterministic [FallbackModel] reports. Exported so the
// orchestrator (and the diagnostics sink) can compare to a
// pinned identifier rather than reaching into a fresh
// [FallbackModel] instance just to read its Name.
//
// Anchor: `tech-spec.md` Sec 9.3 ("Effort model fallback").
const FallbackEstimatorName = "fallback"

// Name returns the stable identifier of the deterministic
// [FallbackModel] -- [FallbackEstimatorName]. The method is
// declared on `*FallbackModel` here (rather than on
// `fallback.go`) to keep all Estimator-conformance surface in
// one file; this is what makes `var _ Estimator =
// (*FallbackModel)(nil)` in `effort_test.go` meaningful.
func (m *FallbackModel) Name() string { return FallbackEstimatorName }
