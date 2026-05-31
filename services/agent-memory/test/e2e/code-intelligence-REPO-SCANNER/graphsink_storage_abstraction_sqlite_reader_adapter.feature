@story-code-intelligence:REPO-SCANNER @phase-graphsink-storage-abstraction @stage-sqlite-reader-adapter @setup-compose
Feature: SQLite reader adapter

  The SQLite graphsink.Reader adapter (internal/graphsink/sqlite/reader.go)
  implements the six-method Reader interface against the same *sql.DB the
  Sink writer opens. These scenarios exercise the three acceptance criteria
  for Stage 3.6: parent-filtered ListNodes, kind-filtered ListEdgesFrom,
  and the MaxListLimit clamp on ListNodes.

  Scenario: sqlite-list-nodes-by-parent
    Given a repo with two package children in a SQLite sink
    When ListNodes with kinds package and ParentNodeID equal to the repo node runs
    Then the two packages are returned and nothing else

  Scenario: sqlite-list-edges-from
    Given a method with three outbound static_calls edges in a SQLite sink
    When ListEdgesFrom with srcNodeID and kinds static_calls runs
    Then exactly three Edges are returned

  Scenario: sqlite-maxlistlimit-clamp
    Given 15000 Nodes of kind method in a SQLite sink
    When ListNodes with kinds method and Limit 20000 runs
    Then exactly 10000 are returned and a structured log records the clamp
