@story-code-intelligence:REPO-SCANNER @phase-graphsink-storage-abstraction @stage-backend-parity-golden-test @setup-compose
Feature: Backend parity golden test — E2E

  The three graphsink backends (memory, SQLite, Postgres) must
  produce byte-identical Node and Edge tuples when driven over the
  same fixture. This feature proves that invariant via golden
  snapshot comparison: each backend's output is compared against a
  committed golden fixture that represents the canonical output all
  backends agree on.

  Scenario: parity-three-backends
    Given the fixture repo "testdata/polyglot/"
    When the dispatcher runs against each backend in turn
    Then the sorted "(kind, canonical_signature, fingerprint_hex)" lines for Nodes match across all three backends

  Scenario: edge-parity
    Given the same fixture
    When the test extracts "(kind, src_fingerprint_hex, dst_fingerprint_hex, fingerprint_hex)" for Edges
    Then the sorted lines match across all three backends

  Scenario: legacy-postgres-documented-exception
    Given a Postgres row pre-existing with a random "repo_id"
    When the parity test runs against that row
    Then the documented exception path executes and the test classifies it as "legacy data" rather than a regression
