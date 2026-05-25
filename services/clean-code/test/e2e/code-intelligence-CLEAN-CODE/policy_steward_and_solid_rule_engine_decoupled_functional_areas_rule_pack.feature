@story-code-intelligence:CLEAN-CODE @phase-policy-steward-and-solid-rule-engine @stage-decoupled-functional-areas-rule-pack @setup-compose
Feature: Decoupled functional areas rule pack
  Validates that the three decoupling rulepack files load correctly
  into the Policy Steward with parsed predicates, and that the
  cycle-member rule fires on matching metric samples.

  Scenario: decoupling-loads
    Given the three decoupling rulepack files
    When the Steward loads them
    Then "pack='decoupling'" rule_packs exist with parsed predicates

  Scenario: cycles-rule-fires-on-cycle-member
    Given a metric_sample with metric_kind "cycle_member" and value 1
    When the predicate evaluates
    Then it returns true