@story-code-intelligence:REFACTOR-GUIDE @phase-p0-reports-and-delivery @stage-json-findings-artifact @setup-inline
Feature: JSON findings artifact
  Stage 3.2: the JSON renderer converts a RunArtifact into a
  machine-readable findings sidecar per architecture Sec 3.7.2.
  Every emitted document is byte-stable, round-trips through
  json.Unmarshal, and carries the canonical schema version stamp.

  Scenario: round-trip via Unmarshal
    Given a fully-populated RunArtifact for JSON rendering
    When JSON.Render runs
    And the output is unmarshalled back into a RunArtifact
    Then all fields of the unmarshalled artifact match the original

  Scenario: byte-stable across runs
    Given a fully-populated RunArtifact for JSON rendering
    When JSON.Render runs twice on the same artifact
    Then the two JSON outputs are byte-identical

  Scenario: schema version stamped
    Given a non-empty RunArtifact without an explicit schema version
    When JSON.Render runs
    Then the output JSON contains a schemaVersion field set to "v1.2026.05"
