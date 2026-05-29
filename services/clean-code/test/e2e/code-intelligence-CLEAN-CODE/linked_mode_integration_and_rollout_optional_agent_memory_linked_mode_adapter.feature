@story-code-intelligence:CLEAN-CODE @phase-linked-mode-integration-and-rollout @stage-optional-agent-memory-linked-mode-adapter @setup-inline
Feature: Optional agent memory linked mode adapter
  Validates that the cross-repo aggregator integrates agent-memory
  edges when agent-memory is reachable in linked mode, and degrades
  gracefully when the agent-memory service is unreachable.

  Scenario: linked-mode-uses-edges
    Given linked mode is enabled with a reachable agent-memory service
    When the aggregator composes "arch_debt_ratio"
    Then xrepo edges are factored into the result
    And the output has degraded equal to "false"

  Scenario: linked-mode-unreachable-degrades
    Given linked mode is enabled with an unreachable agent-memory service
    When the aggregator composes "arch_debt_ratio"
    Then the output has degraded equal to "true"
    And the output has degraded_reason equal to "xrepo_edges_unavailable"