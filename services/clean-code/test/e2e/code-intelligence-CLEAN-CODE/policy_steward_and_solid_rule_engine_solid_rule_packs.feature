@story-code-intelligence:CLEAN-CODE @phase-policy-steward-and-solid-rule-engine @stage-solid-rule-packs @setup-compose
Feature: SOLID rule packs
  Validates that the five SOLID rulepack files (SRP, OCP, LSP, ISP, DIP)
  load into the Policy Steward with parsed predicates, that every
  metric_kind reference is one of the seven canonical kinds, and that the
  OCP rulepack declares exactly the expected inputs.

  Background:
    Given the Policy Steward is reachable

  Scenario: solid-rulepacks-load
    Given the five SOLID rulepack files for "srp", "ocp", "lsp", "isp", "dip"
    When the Policy Steward loads them
    Then exactly 5 rule_pack rows exist with pack "solid"
    And each rule_pack maps to one of "srp", "ocp", "lsp", "isp", "dip"
    And every rule_pack has a non-empty predicate that parses without error

  Scenario: solid-rulepacks-only-canonical-kinds
    Given the loaded SOLID rule_packs
    When the steward returns the parsed metric_kind references for each predicate
    Then every metric_kind is one of "lcom4, fan_in, fan_out, depth_of_inheritance, interface_width, coupling_between_objects, modification_count_in_window"
    And no non-canonical alias appears in any predicate

  Scenario: ocp-uses-fan-in
    Given the loaded OCP rulepack
    When the steward returns its parsed input set
    Then the inputs are exactly "fan_in" and "modification_count_in_window"
    And the input "depth_of_inheritance" is not present