@story-code-intelligence:REPO-SCANNER @phase-identity-and-ancestry-refactor @stage-worker-adopts-ancestrywriter @setup-inline
Feature: Worker adopts AncestryWriter

  After the refactor the queue worker's `runFull` path delegates
  repo→package→file ancestry to AncestryWriter. These scenarios
  verify that the refactor is behaviour-preserving: the graph
  output is byte-identical, the integration tests pass without
  assertion edits, and the old unexported helper names are gone.

  Scenario: worker-graph-byte-identical
    Given the same fixture repo with 5 files across 3 packages
    And worker.runFull delegates to NewAncestryWriter in the source
    When the pre-refactor inline worker flow runs against the spy
    And the post-refactor AncestryWriter flow runs against a fresh spy
    And go test runs TestWorker_fullIngest_graphIsByteIdenticalToCanonicalIdentity
    Then every node has identical canonical_signature, kind, and parent_node_id
    And every edge has identical kind and src-dst node pairs
    And every node fingerprint is byte-identical
    And the byte-identity integration test did not fail

  Scenario: worker-integration-still-passes
    Given the repoindexer test suite under internal/repoindexer
    When the repoindexer test suite runs via go test
    Then the test suite exits with code 0
    And no test functions report FAIL in the output

  Scenario: helpers-no-internal-callers
    Given the refactored codebase under services/agent-memory/internal/
    When grep -rIn scans for old unexported helper names
    Then no hits for "canonicalRepoSig" remain in the output
    And no hits for "canonicalPackageDir" remain in the output
    And no hits for "canonicalPackageSig" remain in the output
    And no hits for "canonicalFileSig" remain in the output