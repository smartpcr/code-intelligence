@story-code-intelligence:REPO-SCANNER @phase-graphsink-storage-abstraction @stage-sink-interface-skeleton @setup-compose
Feature: Sink interface skeleton — compile-time satisfaction

  The graphsink package declares two interfaces (Sink and Reader) that
  decouple the scan pipeline from the storage backend. This stage proves
  the interfaces compile, stubs satisfy them, and the existing
  *graphwriter.Writer continues to satisfy the narrower
  repoindexer.RepoCommitNodeEdgeWriter without modification.

  Scenario: sink-interface-compiles
    Given an empty implementation "type stubSink struct{}" in a test file with the required methods
    When "go vet ./internal/graphsink/..." runs
    Then the implementation satisfies "graphsink.Sink"

  Scenario: reader-interface-compiles
    Given a stub Reader impl in tests including ListRepos
    When "go vet ./internal/graphsink/..." runs
    Then the stub satisfies "graphsink.Reader"

  Scenario: graphwriter-still-satisfies-narrow-writer
    Given the unchanged "*graphwriter.Writer"
    When a test assigns it to a "repoindexer.RepoCommitNodeEdgeWriter" variable
    Then the assignment compiles without modification
