@story-code-intelligence:REFACTOR-GUIDE @phase-foundations @stage-effort-estimator-fallback @setup-inline
Feature: Deterministic effort-estimator fallback
  The FallbackModel provides a deterministic linear effort estimate
  when the ONNX model is unavailable, using the formula
  hours = (0.02·LOC + 0.10·Cyclo + 0.05·FanIn + 1.0) × taskMultiplier,
  clamped to [0.1, 80.0].

  Scenario: deterministic output for known fixture inputs
    Given metric inputs LOC=500, Cyclo=20, FanIn=8
    And the task kind is "split_class"
    When FallbackModel.Estimate runs
    Then the result is 20.1 hours

  Scenario: clamp upper bound
    Given metric inputs that would compute to 120.0 hours before clamping
    When FallbackModel.Estimate runs
    Then the result is exactly 80.0 hours

  Scenario: clamp lower bound
    Given metric inputs that would compute to 0.05 hours before clamping
    When FallbackModel.Estimate runs
    Then the result is exactly 0.1 hours

  Scenario: task-kind multiplier ratio
    Given metric inputs LOC=500, Cyclo=20, FanIn=8
    When FallbackModel.Estimate runs for "split_class"
    And FallbackModel.Estimate runs for "extract_method"
    Then the split_class output divided by the extract_method output equals 1.5 divided by 0.7