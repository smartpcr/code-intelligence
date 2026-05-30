//go:build e2e

// -----------------------------------------------------------------------
// <copyright file="foundations_effort_estimator_fallback_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package e2e

import (
	"fmt"
	"math"
	"os"
	"testing"

	"github.com/cucumber/godog"
	"github.com/microsoft/cleancode-service/internal/cli/effort"
)

// requireEnv skips the test when the named environment variable is unset.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("environment variable %s is not set; skipping", name)
	}
	return v
}

type effortEstimatorState struct {
	model   *effort.FallbackModel
	input   effort.MetricInput
	taskKind effort.TaskKind
	result  effort.EstimateResult
	resultA effort.EstimateResult
	resultB effort.EstimateResult
	err     error
}

func newEffortEstimatorState() *effortEstimatorState {
	return &effortEstimatorState{model: effort.NewFallbackModel()}
}

func (s *effortEstimatorState) metricInputs(loc, cyclo, fanIn float64) {
	s.input = effort.MetricInput{LOC: loc, Cyclo: cyclo, FanIn: fanIn}
}

func (s *effortEstimatorState) theTaskKindIs(kind string) {
	s.taskKind = effort.TaskKind(kind)
}

func (s *effortEstimatorState) fallbackModelEstimateRuns() {
	s.result, s.err = s.model.Estimate(s.input, s.taskKind)
}

func (s *effortEstimatorState) theResultIsHours(expected float64) error {
	if s.err != nil {
		return fmt.Errorf("estimate returned error: %w", s.err)
	}
	if math.Abs(s.result.Hours-expected) > 1e-9 {
		return fmt.Errorf("expected %.10f hours, got %.10f", expected, s.result.Hours)
	}
	return nil
}

func (s *effortEstimatorState) theResultIsExactlyHours(expected float64) error {
	if s.err != nil {
		return fmt.Errorf("estimate returned error: %w", s.err)
	}
	if s.result.Hours != expected {
		return fmt.Errorf("expected exactly %v hours, got %v", expected, s.result.Hours)
	}
	return nil
}

// Upper bound: (0.02*3950 + 1.0) * 1.5 = 80 * 1.5 = 120.0 → clamped to 80.0
func (s *effortEstimatorState) metricInputsThatComputeTo120() {
	s.input = effort.MetricInput{LOC: 3950, Cyclo: 0, FanIn: 0}
	s.taskKind = effort.SplitClass
}

// Lower bound: (0.02*(-45) + 0.05*(-1) + 1.0) * 1.0 = 0.05 → clamped to 0.1
func (s *effortEstimatorState) metricInputsThatComputeTo005() {
	s.input = effort.MetricInput{LOC: -45, Cyclo: 0, FanIn: -1}
	s.taskKind = effort.BreakCycle
}

func (s *effortEstimatorState) estimateRunsForKind(kind string) error {
	res, err := s.model.Estimate(s.input, effort.TaskKind(kind))
	if err != nil {
		return fmt.Errorf("estimate for %s: %w", kind, err)
	}
	if s.resultA.Hours == 0 {
		s.resultA = res
	} else {
		s.resultB = res
	}
	return nil
}

func (s *effortEstimatorState) splitClassDividedByExtractMethodEquals() error {
	if s.resultA.Hours == 0 || s.resultB.Hours == 0 {
		return fmt.Errorf("missing results: A=%v B=%v", s.resultA.Hours, s.resultB.Hours)
	}
	actual := s.resultA.Hours / s.resultB.Hours
	expected := 1.5 / 0.7
	if math.Abs(actual-expected) > 1e-9 {
		return fmt.Errorf("expected ratio %.10f, got %.10f", expected, actual)
	}
	return nil
}

func InitializeScenario_foundations_effort_estimator_fallback(ctx *godog.ScenarioContext) {
	s := newEffortEstimatorState()

	ctx.Step(`^metric inputs LOC=(\d+), Cyclo=(\d+), FanIn=(\d+)$`, func(loc, cyclo, fanIn int) {
		s.metricInputs(float64(loc), float64(cyclo), float64(fanIn))
	})
	ctx.Step(`^the task kind is "([^"]*)"$`, s.theTaskKindIs)
	ctx.Step(`^FallbackModel\.Estimate runs$`, s.fallbackModelEstimateRuns)
	ctx.Step(`^the result is ([\d.]+) hours$`, s.theResultIsHours)
	ctx.Step(`^the result is exactly ([\d.]+) hours$`, s.theResultIsExactlyHours)
	ctx.Step(`^metric inputs that would compute to 120\.0 hours before clamping$`, s.metricInputsThatComputeTo120)
	ctx.Step(`^metric inputs that would compute to 0\.05 hours before clamping$`, s.metricInputsThatComputeTo005)
	ctx.Step(`^FallbackModel\.Estimate runs for "([^"]*)"$`, s.estimateRunsForKind)
	ctx.Step(`^the split_class output divided by the extract_method output equals 1\.5 divided by 0\.7$`, s.splitClassDividedByExtractMethodEquals)
}

func TestE2E_foundations_effort_estimator_fallback(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_foundations_effort_estimator_fallback,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"foundations_effort_estimator_fallback.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}