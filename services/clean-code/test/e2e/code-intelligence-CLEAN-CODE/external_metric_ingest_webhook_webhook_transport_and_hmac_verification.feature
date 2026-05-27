@story-code-intelligence:CLEAN-CODE @phase-external-metric-ingest-webhook @stage-webhook-transport-and-hmac-verification @setup-compose
Feature: Webhook Transport and HMAC Verification

  Validates that the webhook endpoint enforces HMAC-SHA256 signature
  verification and deduplicates replay payloads by payload hash.

  Scenario: invalid-signature-rejected
    Given a running webhook service connected to PostgreSQL
    And the database is migrated and repo-d is seeded
    When a webhook POST is sent with an invalid HMAC header
    Then the response status code is 401
    And no scan_run row exists for that payload

  Scenario: replay-returns-cached-scan-run
    Given a running webhook service connected to PostgreSQL
    And the database is migrated and repo-d is seeded
    When a valid webhook POST is sent for SHA "dddd0001"
    Then the response status code is 2xx and a scan_run_id is returned
    When the same payload body is POSTed again with a valid signature
    Then the response returns the original scan_run_id
    And only one scan_run row exists for that payload hash