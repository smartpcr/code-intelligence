@story-code-intelligence:REFACTOR-GUIDE @phase-foundations @stage-repo-context-and-scope-binding @setup-inline
Feature: Repo context and scope binding
  The CLI-local RepoContext and ScopeBinding layers mint stable
  identifiers and resolve module paths so the downstream pipeline
  (metrics, rule engine, refactor planner) receives deterministic,
  re-run-safe inputs with no database dependency.

  Scenario: stable repo id
    Given an absolute root path "/tmp/foo"
    When MintRepoID is called twice
    Then both calls return the same UUID-v5 value byte-for-byte

  Scenario: working-copy fallback
    Given a directory that is not a git repo
    When DetectHeadSHA runs
    Then it returns the literal string "working-copy" and IsGitRepo == false

  Scenario: module path from go.mod
    Given a go.mod file containing "module github.com/example/foo"
    When DetectModulePath(root, "go") runs
    Then it returns "github.com/example/foo"

  Scenario: scope binding round trip
    Given a ScopeBinding inserted with ScopeID = "scope-abc-123"
    When Get("scope-abc-123") runs
    Then it returns the same struct with FilePath, StartLine, EndLine, and Signature intact