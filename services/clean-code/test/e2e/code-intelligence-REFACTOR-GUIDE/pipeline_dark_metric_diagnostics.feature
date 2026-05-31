@story-code-intelligence:REFACTOR-GUIDE @phase-pipeline @stage-dark-metric-diagnostics @setup-inline
Feature: Dark metric diagnostics
  Stage 2.5: the orchestrator surfaces a DarkMetric diagnostic per
  (metric_kind, language) pair when a recipe stays dark because
  today's parser fleet does not stamp the required attrs. Lit
  recipes (e.g. loc) must NOT appear. The closed-set validation
  rejects unknown attrs at init time (exit 70). The effort
  estimator fallback is recorded when the ONNX model is absent.

  Scenario: cyclo dark on Go
    Given a fixture Go file with one function whose parser does not stamp decision_blocks
    When the orchestrator runs
    Then Diagnostics.DarkMetrics includes a row with metric_kind "cyclo", language "go", missing_attrs ["decision_blocks"], affected_scope_count 1, and closure_phase "P2"

  Scenario: loc not flagged dark
    Given a fixture Go file with one function whose parser does not stamp decision_blocks
    When the orchestrator runs
    Then Diagnostics.DarkMetrics does not include any row with metric_kind "loc"

  Scenario: unknown attr fails closed
    Given a recipe registered with MetricKind "bogus_metric" not in metricAttrRequirements and a fake AppliesTo returning false
    When the orchestrator runs with the bogus recipe
    Then the dark-metric diagnostic does not include metric_kind "bogus_metric"
    And the production validateMetricAttrRequirements rejects bogus_attr with exit code 70 and stderr matching tech-spec Sec 8.7

  Scenario: effort source recorded
    Given a fixture Go file with one function whose parser does not stamp decision_blocks
    When the orchestrator runs
    Then Diagnostics.EffortSource is "fallback"
