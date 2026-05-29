@story-code-intelligence:CLEAN-CODE @phase-linked-mode-integration-and-rollout @stage-aged-mute-insights-report @setup-inline
Feature: Aged mute insights report
  Validates that the aged-mutes report lists overrides older than a
  threshold, that those overrides remain active (no automatic flip),
  and that unmuting a scope/rule removes it from subsequent reports.

  Scenario: aged-mute-listed-not-enforced
    Given an override with mute equal to "true" and created_at "100" days ago
    When the aged-mutes report runs
    Then the override appears in the report response
    And it remains the active mute with value "true"

  Scenario: unmute-removes-from-report
    Given an aged mute override for a known scope and rule
    When the operator appends an override with mute equal to "false" via mgmt.override
    Then the next aged-mutes report omits the scope and rule pair