// -----------------------------------------------------------------------
// <copyright file="fallback.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package effort

import "fmt"

// TaskKind identifies the category of refactoring task.
type TaskKind string

const (
	SplitClass             TaskKind = "split_class"
	ExtractMethod          TaskKind = "extract_method"
	InvertDependency       TaskKind = "invert_dependency"
	BreakCycle             TaskKind = "break_cycle"
	ConsolidateDuplication TaskKind = "consolidate_duplication"
)

// Coefficients for the deterministic linear model.
const (
	AlphaLOC      = 0.02
	BetaCyclo     = 0.10
	GammaFanIn    = 0.05
	Intercept     = 1.0
	ClampMinHours = 0.1
	ClampMaxHours = 80.0
)

// taskMultipliers maps each TaskKind to its effort multiplier.
var taskMultipliers = map[TaskKind]float64{
	SplitClass:             1.5,
	ExtractMethod:          0.7,
	InvertDependency:       1.2,
	BreakCycle:             1.0,
	ConsolidateDuplication: 0.8,
}

// MetricInput holds the raw metric values fed into the estimator.
type MetricInput struct {
	LOC    float64
	Cyclo  float64
	FanIn  float64
}

// EstimateResult holds the output of the effort estimation.
type EstimateResult struct {
	Hours float64
}

// FallbackModel is the deterministic effort estimator used when the ONNX
// model is unavailable.
type FallbackModel struct{}

// NewFallbackModel returns a ready-to-use FallbackModel.
func NewFallbackModel() *FallbackModel {
	return &FallbackModel{}
}

// Estimate computes effort hours using a deterministic linear formula:
//
//	hours = (α·LOC + β·Cyclo + γ·FanIn + Intercept) × taskMultiplier
//
// The result is clamped to [ClampMinHours, ClampMaxHours].
func (m *FallbackModel) Estimate(input MetricInput, kind TaskKind) (EstimateResult, error) {
	mult, ok := taskMultipliers[kind]
	if !ok {
		return EstimateResult{}, fmt.Errorf("unknown task kind: %s", kind)
	}

	raw := (AlphaLOC*input.LOC + BetaCyclo*input.Cyclo + GammaFanIn*input.FanIn + Intercept) * mult

	// Clamp
	if raw < ClampMinHours {
		raw = ClampMinHours
	}
	if raw > ClampMaxHours {
		raw = ClampMaxHours
	}

	return EstimateResult{Hours: raw}, nil
}