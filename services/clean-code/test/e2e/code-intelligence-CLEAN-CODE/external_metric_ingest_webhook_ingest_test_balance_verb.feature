@story-code-intelligence:CLEAN-CODE @phase-external-metric-ingest-webhook @stage-ingest-test-balance-verb @setup-compose
Feature: Ingest test_balance verb
  Validates that the test_balance ingest verb computes pass_first_try_ratio
  from JSON uploads, rejects non-JSON payloads, and clamps the ratio to [0,1].

  Scenario: test-balance-emits-only-pass-first-try-ratio
    Given a running webhook service connected to PostgreSQL
    And the database is migrated and repo-d is seeded
    When a JSON test_balance payload is uploaded with scopes
      | scope_id | attempt_count | pass_count |
      | mod-a    | 10            | 8          |
      | mod-b    | 5             | 5          |
    Then each scope_id has exactly one metric_sample with metric_kind "pass_first_try_ratio"
    And no metric_sample rows exist with metric_kind "test_count" or "duration"

  Scenario: test-balance-rejects-junit-xml
    Given a running webhook service connected to PostgreSQL
    When a JUnit-XML body is POSTed to "/v1/ingest/test_balance"
    Then the response status code is 415

  Scenario: ratio-clamped-zero-to-one
    Given a running webhook service connected to PostgreSQL
    And the database is migrated and repo-d is seeded
    When a JSON test_balance payload is uploaded with scopes
      | scope_id | attempt_count | pass_count |
      | mod-c    | 4             | 3          |
      | mod-d    | 0             | 0          |
    Then the emitted pass_first_try_ratio for "mod-c" is between 0 and 1
    And no metric_sample row is written for scope_id "mod-d"