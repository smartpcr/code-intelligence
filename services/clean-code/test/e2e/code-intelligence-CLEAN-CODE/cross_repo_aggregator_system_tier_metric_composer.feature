@story-code-intelligence:CLEAN-CODE @phase-cross-repo-aggregator @stage-system-tier-metric-composer @setup-inline
Feature: System tier metric composer
  The SystemTierComposer materialises exactly the SEVEN canonical
  system-tier metric_sample rows per architecture Sec 1.4.2. It
  never silently drops a row; missing inputs produce a degraded
  row with the appropriate reason.

  Scenario: system-tier-only-canonical-kinds
    Given the system_tier composer at runtime
    When listing the metric_kinds it will write
    Then the set is exactly "xrepo_dep_depth, arch_debt_ratio, velocity_trend, arch_fitness, blast_radius, xservice_test_reliability, knowledge_index"
    And no metric_kind matches "p50.system" or "p90.system" or "p95.system" or "p99.system"

  Scenario: embedded-mode-writes-degraded-row
    Given the aggregator in embedded mode with no xrepo edges
    When it composes "arch_debt_ratio" for an affected scope
    Then a metric_sample row is written with metric_kind "arch_debt_ratio" and pack "system" and degraded true and degraded_reason "xrepo_edges_unavailable"
    And the degraded counter labelled reason "xrepo_edges_unavailable" increments

  Scenario: samples-pending-writes-degraded-row
    Given missing foundation "cyclo" samples for a scope at a given SHA
    When the aggregator composes "velocity_trend" at that SHA
    Then a metric_sample row is written with metric_kind "velocity_trend" and pack "system" and degraded true and degraded_reason "samples_pending"
    And the value may be NULL