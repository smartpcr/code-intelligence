@story-code-intelligence:REPO-SCANNER @phase-identity-and-ancestry-refactor @stage-worker-adopts-ancestrywriter @setup-inline
Feature: Worker adopts AncestryWriter

  After the refactor the queue worker's `runFull` path delegates
  repo→package→file ancestry to AncestryWriter. These scenarios
  verify that the refactor is behaviour-preserving: the graph
  output is byte-identical, the integration tests pass without
  assertion edits, and the old unexported helper names are gone.

  Scenario: worker-graph-byte-identical
    Given the same fixture repo with 5 files across 3 packages
    When the worker-equivalent ancestry flow runs before and after the refactor
    Then the resulting node rows have identical canonical_signature and kind values
    And the resulting edge rows have identical kind and src-dst pairs
    And the resulting node fingerprints are byte-identical

  Scenario: worker-integration-still-passes
    Given the existing worker_integration_test.go file
    When the test suite is inspected after the refactor
    Then the file exists at the expected path
    And the file contains no assertion edits for graph contents
    And the file imports repoindexer and graphwriter packages

  Scenario: helpers-no-internal-callers
    Given the refactored codebase under services/agent-memory/internal/
    When scanning for old unexported helper names
    Then no hits for "canonicalRepoSig" appear in Go source files
    And no hits for "canonicalPackageDir" appear in Go source files
    And no hits for "canonicalPackageSig" appear in Go source files
    And no hits for "canonicalFileSig" appear in Go source files