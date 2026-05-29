@story-code-intelligence:CLEAN-CODE @phase-evaluator-surface-and-management-surface @stage-http-json-gateway-and-oidc-auth @setup-compose
Feature: HTTP JSON gateway and OIDC auth
  Validates that the HTTP JSON gateway enforces OIDC bearer-token
  authentication on all /v1/* routes and returns proper HTTP status
  codes for missing tokens and unknown verbs.

  Scenario: oidc-rejects-missing-token
    Given an HTTP request to any "/v1/*" route without an Authorization header
    When the handler runs
    Then it returns 401
    And the response includes a "WWW-Authenticate: Bearer" header

  Scenario: unknown-verb-404
    Given a POST to "/v1/eval/unknown_verb" with a valid bearer token
    When the gateway routes the request
    Then it returns 404
    And no evaluation_run row is emitted