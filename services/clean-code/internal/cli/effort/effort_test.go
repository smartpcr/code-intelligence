// -----------------------------------------------------------------------
// <copyright file="effort_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

// Tests for the [Estimator] contract declared in `effort.go`.
//
// These tests pin three properties downstream stages (the
// orchestrator's effort-stamping pass, the diagnostics sink's
// per-task `effort_source` label) rely on:
//
//  1. `*FallbackModel` SATISFIES the [Estimator] interface --
//     a compile-time assertion (the `var _` line below) makes
//     the conformance loud at build time so an accidental
//     signature drift fails CI before any test runs.
//  2. The [FallbackModel.Name] string is the stable identifier
//     [FallbackEstimatorName] -- renaming either side is a
//     cross-version migration concern, so the test fails on
//     drift in either direction.
//  3. The [Estimator] contract's "unknown TaskKind returns
//     non-nil error AND zero-value result" rule is honoured
//     by the only implementation in this package; this guards
//     against a future implementation that panics on an
//     unknown kind.
package effort

import "testing"

// Compile-time assertion: `*FallbackModel` MUST satisfy the
// [Estimator] interface. A signature drift on either side
// (a removed method, a changed parameter type) fails the
// build BEFORE any test runs, which is louder than a
// per-method assertion inside a `t.Run`.
var _ Estimator = (*FallbackModel)(nil)

// TestFallbackModel_NameIsStableIdentifier pins that the
// dev-build estimator reports the [FallbackEstimatorName]
// constant verbatim. The diagnostics sink writes this string
// into the per-task `effort_source` field; a rename here
// without a corresponding doc + downstream consumer update
// would silently break log parsing on the operator side.
func TestFallbackModel_NameIsStableIdentifier(t *testing.T) {
	t.Parallel()

	got := NewFallbackModel().Name()
	if got != FallbackEstimatorName {
		t.Errorf("FallbackModel.Name() = %q, want %q (FallbackEstimatorName)",
			got, FallbackEstimatorName)
	}
	if got != "fallback" {
		t.Errorf("FallbackModel.Name() = %q, want literal %q -- "+
			"changing the identifier is a cross-version migration concern",
			got, "fallback")
	}
}

// TestFallbackModel_UnknownTaskKindIsErrorNotPanic exercises
// the [Estimator] contract: an unknown [TaskKind] MUST return
// a non-nil error and the zero-value [EstimateResult]; the
// implementation MUST NOT panic. This is what lets the
// orchestrator surface "unknown task kind" as a soft warning
// rather than crashing the binary.
func TestFallbackModel_UnknownTaskKindIsErrorNotPanic(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Estimate panicked on unknown TaskKind: %v", r)
		}
	}()

	res, err := NewFallbackModel().Estimate(MetricInput{LOC: 1}, TaskKind("definitely_not_a_real_kind"))
	if err == nil {
		t.Fatal("Estimate(unknown TaskKind) returned nil error; want non-nil per Estimator contract")
	}
	if res != (EstimateResult{}) {
		t.Errorf("Estimate(unknown TaskKind) returned non-zero EstimateResult %+v; want zero value per Estimator contract", res)
	}
}

// TestFallbackModel_EstimatorIsDeterministicWithinProcess pins
// the within-process determinism half of the [Estimator]
// contract: identical (MetricInput, TaskKind) pairs MUST
// produce byte-identical [EstimateResult] values within a
// single binary instance. The cross-process guarantee is
// satisfied for the deterministic [FallbackModel] but is not
// promised by the contract for all future implementations
// (an ONNX-backed model may load non-deterministic weights),
// so this test deliberately scopes the assertion to within
// one process.
func TestFallbackModel_EstimatorIsDeterministicWithinProcess(t *testing.T) {
	t.Parallel()

	m := NewFallbackModel()
	input := MetricInput{LOC: 250, Cyclo: 12, FanIn: 4}
	first, err := m.Estimate(input, ExtractMethod)
	if err != nil {
		t.Fatalf("first Estimate: %v", err)
	}
	for i := 0; i < 8; i++ {
		got, err := m.Estimate(input, ExtractMethod)
		if err != nil {
			t.Fatalf("Estimate call %d: %v", i, err)
		}
		if got != first {
			t.Fatalf("Estimate call %d not deterministic: got %+v, want %+v", i, got, first)
		}
	}
}
