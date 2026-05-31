@story-code-intelligence:REPO-SCANNER @phase-graphsink-storage-abstraction @stage-memory-reader-adapter @setup-compose
Feature: Memory reader adapter

  The in-memory graphsink.Reader adapter wraps the append-only
  slices in the memory Sink, applying the same per-field filters
  the SQLite reader's WHERE clauses encode so a diagram-projector
  run against `--store=memory` produces identical envelopes to one
  served by `--store=sqlite`.

  Scenario: memory-reader-parity
    Given the same fixture graph inserted into the memory sink and the SQLite sink
    When both readers run the same ListNodes, ListEdgesFrom, and ListEdgesTo queries
    Then the returned slices have identical lengths and identical kind and canonical_signature projections

  Scenario: memory-lookup-fast-path
    Given a Node inserted with signature S into the memory sink
    When LookupBySignature with kind method and signature S runs
    Then it returns the Node in O(1) via the sigIndex map
