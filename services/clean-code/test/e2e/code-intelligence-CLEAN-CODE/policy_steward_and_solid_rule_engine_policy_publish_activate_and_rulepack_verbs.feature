@story-code-intelligence:CLEAN-CODE @phase-policy-steward-and-solid-rule-engine @stage-policy-publish-activate-and-rulepack-verbs @setup-compose
Feature: Policy publish, activate, and rulepack verbs
  Validates that policy_version rows are immutable, activation uses
  latest-row-wins semantics, and the gRPC surface exposes exactly
  the canonical verb set defined in the tech-spec (Sec 8.5).

  Scenario: policy-version-immutable
    Given a published policy_version row exists in the database
    When any UPDATE statement targets that row
    Then PostgreSQL returns permission denied
    And the Steward verb path has no UPDATE call

  Scenario: activation-latest-row-wins
    Given an active policy_version
    When "policy.activate" runs with a new version id
    Then a new policy_activation row appears
    And the latest row by "created_at" defines the active version
    And no "scope" parameter was accepted
    And no "deactivated_at" flag was set on the prior row

  Scenario: canonical-rulepack-verb-name
    Given the gRPC surface is available
    When listing the "policy.*" verbs
    Then exactly "policy.publish, policy.activate, policy.publish_rulepack" are registered
    And a call to "policy.rulepack.add" returns UNIMPLEMENTED
    And a call to "policy.rulepack.remove" returns UNIMPLEMENTED