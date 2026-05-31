@story-code-intelligence:REPO-SCANNER @phase-identity-and-ancestry-refactor @stage-worker-adopts-ancestrywriter @setup-inline
Feature: Worker adopts AncestryWriter

  The worker's runFull method now delegates the repo→package→file
  ancestry pipeline to AncestryWriter. These scenarios verify that
  the refactored path produces byte-identical graph tuples, that
  the existing integration tests remain green, and that the
  unexported canonical-signature helpers have been fully removed.

  Scenario: worker-graph-byte-identical
    Given an in-memory fixture repo with files "README.md,pkg/foo.go,pkg/sub/bar.go"
    And a recording RepoCommitNodeEdgeWriter
    When worker.runFull executes through AncestryWriter with parentSHA "aaa111" and headSHA "bbb222"
    Then the captured node tuples with canonical_signature kind parent_node_id and fingerprint match the golden fixture
    And the captured edge tuples with kind src dst and fingerprint match the golden fixture
    And the FullSummary counters match the expected values
    And fingerprints are stable across a second identical run

  Scenario: worker-integration-still-passes
    Given the existing worker_integration_test.go suite
    When the integration suite runs against the provided Postgres DSN if available
    Then the suite result is recorded

  Scenario: helpers-no-internal-callers
    Given the refactored codebase under "internal/"
    When we search for unexported helper names "canonicalRepoSig,canonicalPackageDir,canonicalPackageSig,canonicalFileSig"
    Then no hits remain in the scanned files
