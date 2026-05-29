@story-code-intelligence:CLEAN-CODE
@phase-audit-wal-and-reliability-hardening
@stage-otel-telemetry-across-all-surfaces
@setup-compose
Feature: OTel telemetry across all surfaces

  Validates that evaluation gate spans carry the canonical verdict tags
  and that the aggregator exposes Prometheus histogram metrics with the
  expected bucket labels.

  Scenario: gate-span-carries-verdict-tag
    Given "eval.gate" returning "warn"
    When OTLP receives the span
    Then the span carries "verdict" equal to "warn"
    And the span carries "degraded" equal to "true"
    And the span carries "degraded_reason" equal to "samples_pending"

  Scenario: prometheus-counter-shape
    Given the aggregator runs one tick
    When "/metrics" is scraped
    Then "cleancode_aggregator_tick_duration_seconds" exists with the expected bucket labels