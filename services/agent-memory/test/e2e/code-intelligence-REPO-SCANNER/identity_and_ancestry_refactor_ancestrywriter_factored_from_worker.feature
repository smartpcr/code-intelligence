@story-code-intelligence:REPO-SCANNER @phase-identity-and-ancestry-refactor @stage-ancestrywriter-factored-from-worker @setup-inline
Feature: AncestryWriter factored from worker

  The AncestryWriter manages the repo→package→file node hierarchy during
  a scan. It is factored out of the monolithic worker so that every
  graphsink backend (Postgres, SQLite, in-memory) shares identical
  deduplication and ordering guarantees.

  Scenario: ensurerepo-and-commit-called-once
    Given a scan that walks 100 files
    When the ancestry writer runs
    Then w.EnsureRepo is called exactly 1 time
    And w.EnsureCommit is called exactly 1 time
    And InsertNode with kind "repo" is called exactly 1 time
    And EnsureRepoAndCommit completes before any EnsureFile call

  Scenario: repo-node-once
    Given a scan that walks 100 files
    When the ancestry writer runs
    Then InsertNode with kind "repo" is called exactly 1 time

  Scenario: package-deduped
    Given 50 files all under "internal/foo/"
    When EnsureFile runs per file
    Then InsertNode with kind "package" is called exactly 1 time
    And the "repo" to "package" contains edge is inserted exactly 1 time

  Scenario: file-and-contains-per-walkfile
    Given a workspace of 7 files
    When EnsureFile runs once per file
    Then 7 file nodes are inserted
    And 7 "package" to "file" contains edges are inserted

  Scenario: ensurefile-before-ensurerepo-errors
    Given a fresh AncestryWriter
    When EnsureFile is called before EnsureRepoAndCommit
    Then a non-nil error is returned
    And no nodes are inserted