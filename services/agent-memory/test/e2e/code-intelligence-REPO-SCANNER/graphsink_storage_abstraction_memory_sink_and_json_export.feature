@story-code-intelligence:REPO-SCANNER @phase-graphsink-storage-abstraction @stage-memory-sink-and-json-export @setup-compose
Feature: Memory sink and JSON export

  The in-memory graphsink.Sink backend backs the
  `codeintel scan --store=memory --export graph.json` one-shot path.
  It stores nodes and edges in append-only slices with a fingerprint-keyed
  idempotent re-emit cache, and writes a JSON export on Close whose
  top-level keys are pinned to `repo`, `nodes`, `edges` in that order.

  Scenario: memory-idempotent
    Given the same NodeInput inserted twice into a memory sink
    When InsertNode runs both times
    Then the second call returns the cached id and the slice length is 1

  Scenario: json-export-key-order
    Given a scan that produces repo, nodes, and edges in a memory sink with an export path
    When Close writes the export
    Then the top-level keys are "repo", "nodes", "edges" in that order verified by streaming-decode

  Scenario: roundtrip-via-loadexport
    Given a memory-sink scan written to disk via Close
    When LoadExport re-reads the export file
    Then the rehydrated Reader returns the same Node and Edge counts as the original scan
