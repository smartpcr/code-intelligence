@story-code-intelligence:REPO-SCANNER @phase-diagram-projector @stage-buildcallchain-bfs @setup-inline
Feature: BuildCallChain BFS

  The BuildCallChain projector performs a bounded BFS over
  static_calls / observed_calls edges starting from a resolved seed
  node. Each scenario populates an ephemeral SQLite database via the
  graphsink.Sink write path (EnsureRepo, InsertNode, InsertEdge) and
  then exercises BuildCallChain against the same *sqlite.Sink acting
  as the graphsink.Reader — proving the full local-fixture integration
  path documented in the implementation plan Stage 6.3.

  Scenario: bfs-callees-only
    Given a method M with 2 callees and 3 callers
    When BuildCallChain runs with seed "M", depth 1, direction "callees"
    Then the diagram contains 3 nodes and 2 edges

  Scenario: bfs-both-directions
    Given a method M with 2 callees and 3 callers
    When BuildCallChain runs with seed "M", depth 1, direction "both"
    Then the diagram contains 6 nodes and 5 edges

  Scenario: bfs-depth-two
    Given a chain A -> B -> C -> D
    When BuildCallChain runs with seed "A", depth 2, direction "callees"
    Then the diagram contains nodes "A,B,C" and not "D"

  Scenario: seed-unresolved-error
    Given a method M with 2 callees and 3 callers
    When BuildCallChain runs with seed "does-not-exist", depth 1, direction "both"
    Then it returns an error with code "seed_not_found" and a zero-value diagram
