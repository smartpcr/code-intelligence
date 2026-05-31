@story-code-intelligence:REPO-SCANNER @phase-graphsink-storage-abstraction @stage-sqlite-sink-backend @setup-compose
Feature: SQLite sink backend — E2E

  The SQLite backend for graphsink.Sink mirrors the Postgres schema so
  a repo scanned to SQLite and later re-scanned to Postgres yields
  identical node identities. These scenarios prove bootstrap, idempotent
  inserts, CHECK-constraint enforcement, precomputed-RepoID requirement,
  and the CGO build-tag guard.

  Scenario: sqlite-bootstraps-on-open
    Given a fresh ".db" file path
    When "sqlite.Open(path)" runs
    Then the file exists
    And the schema is applied
    And sqlite_master lists the tables repo, repo_commit, node, edge

  Scenario: sqlite-idempotent-reinsert
    Given a Node already inserted
    When the same NodeInput is inserted again
    Then no new row is created and the existing row's node_id is returned

  Scenario: sqlite-enum-check-rejects-bad-kind
    Given an InsertNode call with Kind "bogus"
    When the SQLite backend runs
    Then a CHECK-constraint error is returned and no row is inserted

  Scenario: sqlite-requires-precomputed-repoid
    Given a zero-value RepoInput.RepoID
    When EnsureRepo runs against the SQLite sink
    Then a construction-time error is returned

  Scenario: sqlite-requires-cgo
    Given the "internal/graphsink/sqlite/" package
    When a CGO_ENABLED=0 build is attempted
    Then the package fails to compile with a build-tag error naming the missing "cgo" tag
