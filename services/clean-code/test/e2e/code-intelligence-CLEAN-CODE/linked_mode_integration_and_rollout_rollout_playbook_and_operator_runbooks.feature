@story-code-intelligence:CLEAN-CODE @phase-linked-mode-integration-and-rollout @stage-rollout-playbook-and-operator-runbooks @setup-inline
Feature: Rollout playbook and operator runbooks
  Validates that the operator runbook references only canonical verb
  names and that the CHANGELOG advertises the canonical public surface.

  Scenario: runbook-references-canonical-verbs
    Given the runbook content
    When grepping for verb names
    Then only canonical names appear
    And non-canonical names "Policy.Override.Add" and "Policy.Override.Lift" are absent

  Scenario: changelog-lists-canonical-surface
    Given the changelog at "services/clean-code/CHANGELOG.md"
    When parsing the v1 entry
    Then the schema is "clean_code"
    And the verdict values are "pass|warn|block"
    And override has no "expires_at" field
    And the foundation and system metric_kind counts match the canonical lists