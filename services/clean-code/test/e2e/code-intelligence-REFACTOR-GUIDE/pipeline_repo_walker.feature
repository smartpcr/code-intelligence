@story-code-intelligence:REFACTOR-GUIDE @phase-pipeline @stage-repo-walker @setup-inline
Feature: Repo walker
  The L1 filesystem walker converts a repo root into a deterministic stream
  of WalkedFile rows and WalkSkip diagnostics. It honours the hard-coded
  skip-directory list, .gitignore patterns, a per-file size cap, and returns
  a clear sentinel when the root path does not exist.

  Scenario: skip list honoured
    Given a fixture repo containing "node_modules/foo.js"
    When the walker traverses
    Then "node_modules/foo.js" does not appear in WalkedFile
    And a WalkSkip with reason "directory_skip" is emitted for "node_modules"

  Scenario: gitignore honoured
    Given a fixture repo whose ".gitignore" lists "secret.txt"
    And the fixture repo contains "secret.txt"
    When the walker traverses
    Then "secret.txt" produces a WalkSkip with reason "gitignore"
    And zero WalkedFile rows exist for "secret.txt"

  Scenario: size cap
    Given a fixture repo with a 3 MiB ".go" file named "huge.go"
    When the walker traverses
    Then it emits a WalkSkip with reason "size_cap" for "huge.go"
    And the file bytes are never read

  Scenario: missing root
    Given a path that does not exist
    When Walk runs
    Then the error channel yields ErrRootNotFound
    And the file channel closes with zero rows

  Scenario: deterministic ordering
    Given a fixture repo with files "b.go;a.go;c.go"
    When the walker traverses twice
    Then both runs emit WalkedFile in identical order "a.go;b.go;c.go"
