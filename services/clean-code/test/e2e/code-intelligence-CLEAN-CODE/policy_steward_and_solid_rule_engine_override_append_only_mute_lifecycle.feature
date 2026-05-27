@story-code-intelligence:CLEAN-CODE @phase-policy-steward-and-solid-rule-engine @stage-override-append-only-mute-lifecycle @setup-compose
Feature: Override append-only mute lifecycle
  Validates that the mgmt.override verb enforces append-only semantics,
  that latest-row-wins determines the active mute state, and that no
  TTL enforcement or expires_at field is supported in v1.

  Scenario: override-no-expires-field
    Given a "mgmt.override" request with "expires_at" set to "2030-01-01"
    When the verb runs
    Then it returns a validation error naming "expires_at" as unsupported in v1
    And no override row is appended

  Scenario: latest-row-wins
    Given two override rows for the same scope and rule with "mute" values "true" then "false"
    When the evaluator reads the active mute
    Then it sees mute equals "false"

  Scenario: no-ttl-enforcement
    Given an override row older than 365 days
    When time advances and no scheduled job runs
    Then the row remains the active state via latest-row-wins