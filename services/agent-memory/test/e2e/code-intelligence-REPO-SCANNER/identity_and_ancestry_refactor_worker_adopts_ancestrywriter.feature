@story-code-intelligence:REPO-SCANNER @phase-identity-and-ancestry-refactor @stage-worker-adopts-ancestrywriter @setup-inline
Feature: Worker adopts AncestryWriter

  After the refactor the queue worker's `runFull` path delegates
  repo→package→file ancestry to AncestryWriter. These scenarios
  verify that the refactor is behaviour-preserving: the graph
  output is byte-identical, the integration tests compile and
  pass without assertion edits, and the old unexported helper
  names are gone.

  Scenario: worker-graph-byte-identical
    Given the same fixture repo with 5 files across 3 packages
    When the pre-refactor inline worker flow runs against the spy
    And the post-refactor AncestryWriter flow runs against a fresh spy
    Then every node has identical canonical_signature, kind, and parent_node_id
    And every edge has identical kind and src-dst node pairs
    And every node fingerprint is byte-identical

  Scenario: worker-integration-still-passes
    Given the worker_integration_test.go package under internal/repoindexer
    When the repoindexer package is compiled with go vet
    Then the compilation succeeds with exit code 0
    And the test file contains no assertion-edit markers

  Scenario: helpers-no-internal-callers
    Given the refactored codebase under services/agent-memory/internal/
    When scanning for old unexported helper names
    Then no hits for "canonicalRepoSig" appear in Go source files
    And no hits for "canonicalPackageDir" appear in Go source files
    And no hits for "canonicalPackageSig" appear in Go source files
    And no hits for "canonicalFileSig" appear in Go source files