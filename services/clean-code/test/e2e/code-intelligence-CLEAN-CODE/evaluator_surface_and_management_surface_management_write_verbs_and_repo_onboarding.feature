@story-code-intelligence:CLEAN-CODE @phase-evaluator-surface-and-management-surface @stage-management-write-verbs-and-repo-onboarding @setup-compose
Feature: Management write verbs and repo onboarding
  Validates that management-surface write verbs correctly handle repo
  registration idempotency and mode-change event emission.

  Scenario: register-repo-idempotent
    Given a repo is already registered with URL "https://github.com/acme/sample-repo"
    When mgmt.register_repo is called with the same URL "https://github.com/acme/sample-repo"
    Then the existing repo_id is returned
    And no duplicate repo row appears for URL "https://github.com/acme/sample-repo"

  Scenario: set-mode-emits-event
    Given a repo registered at mode "embedded"
    When mgmt.set_mode is called with mode "linked" for that repo
    Then a repo_event with kind "mode_changed" is appended
    And subsequent mgmt.read.repo returns mode "linked"