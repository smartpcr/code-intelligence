@story-code-intelligence:CLEAN-CODE @phase-refactor-planner @stage-composite-hotspot-scoring @setup-compose
Feature: Composite hotspot scoring
  The refactor-planner computes a composite hotspot score from
  per-metric z-scores, finding counts, and configurable weights.
  Each hot_spot row records the policy_version_id that was active
  at scoring time.

  Scenario: hotspot-score-formula
    Given known z-scores "1.0, 2.0, 0.5" and finding_count 3 with weights "1, 1, 1, 1"
    When score is computed
    Then it equals 6.50

  Scenario: hotspot-pins-policy-version
    Given the active policy_version_id "pv-42"
    When a hot_spot row is written
    Then hot_spot.policy_version_id is "pv-42"