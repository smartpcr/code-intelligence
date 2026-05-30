// -----------------------------------------------------------------------
// <copyright file="fallback_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package effort

import (
	"math"
	"testing"
)

func TestFallbackModel_DeterministicOutput(t *testing.T) {
	m := NewFallbackModel()
	res, err := m.Estimate(MetricInput{LOC: 500, Cyclo: 20, FanIn: 8}, SplitClass)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.Abs(res.Hours-20.1) > 1e-9 {
		t.Fatalf("expected 20.1, got %v", res.Hours)
	}
}

func TestFallbackModel_ClampUpperBound(t *testing.T) {
	m := NewFallbackModel()
	// (0.02*3950 + 1.0) * 1.5 = 80 * 1.5 = 120.0 → clamped to 80.0
	res, err := m.Estimate(MetricInput{LOC: 3950, Cyclo: 0, FanIn: 0}, SplitClass)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Hours != 80.0 {
		t.Fatalf("expected 80.0, got %v", res.Hours)
	}
}

func TestFallbackModel_ClampLowerBound(t *testing.T) {
	m := NewFallbackModel()
	// (0.02*(-45) + 0.05*(-1) + 1.0) * 1.0 = 0.05 → clamped to 0.1
	res, err := m.Estimate(MetricInput{LOC: -45, Cyclo: 0, FanIn: -1}, BreakCycle)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Hours != 0.1 {
		t.Fatalf("expected 0.1, got %v", res.Hours)
	}
}

func TestFallbackModel_TaskKindMultiplierRatio(t *testing.T) {
	m := NewFallbackModel()
	input := MetricInput{LOC: 500, Cyclo: 20, FanIn: 8}
	resA, err := m.Estimate(input, SplitClass)
	if err != nil {
		t.Fatalf("unexpected error for split_class: %v", err)
	}
	resB, err := m.Estimate(input, ExtractMethod)
	if err != nil {
		t.Fatalf("unexpected error for extract_method: %v", err)
	}
	actual := resA.Hours / resB.Hours
	expected := 1.5 / 0.7
	if math.Abs(actual-expected) > 1e-9 {
		t.Fatalf("expected ratio %v, got %v", expected, actual)
	}
}

func TestFallbackModel_UnknownTaskKindReturnsError(t *testing.T) {
	m := NewFallbackModel()
	_, err := m.Estimate(MetricInput{LOC: 100}, "nonexistent_kind")
	if err == nil {
		t.Fatal("expected error for unknown task kind")
	}
}