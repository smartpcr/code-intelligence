@story-code-intelligence:REPO-SCANNER @phase-graphsink-storage-abstraction @stage-postgres-sink-adapter @setup-compose
Feature: Postgres sink adapter — E2E

  Scenario: postgres-forwarding
    Given a sqlmock-backed "*graphwriter.Writer"
    When each Sink method runs
    Then the corresponding writer method is invoked exactly once with the same arguments

  Scenario: write-contract-violation-propagates
    Given a SQL error with SQLSTATE 42501
    When InsertNode runs
    Then the returned error is a typed WriteContractViolation and the user-facing message includes the role hint

  Scenario: lookupbysignature-uses-filter
    Given an existing Node with kind "method" and canonical signature "sig://TestLookup" in a real Postgres
    When LookupBySignature runs with repoID, "method", "sig://TestLookup"
    Then it returns the same Node that ListNodes with CanonicalSignature filter returns

  Scenario: postgres-adapter-no-database-sql-import
    Given the "internal/graphsink/postgres/" package source
    When "go list -deps" runs against the package
    Then "database/sql" does NOT appear in the dependency list

  Scenario: listrepos-forwards-to-graphreader
    Given a fake "*graphreader.Reader" that records calls
    When the postgres adapter's ListRepos(ctx, opts) runs
    Then exactly one delegated call is recorded with the same args and the returned []graphreader.RepoSummary is returned unmodified

  Scenario: graphreader-listrepos-matches-mgmtapi
    Given the same fixture rows seeded for both graphreader.Reader.ListRepos and mgmtapi.handleListRepos
    When graphreader.Reader.ListRepos and mgmtapi.handleListRepos both run
    Then the two return identical ordered RepoSummary-equivalent slices
