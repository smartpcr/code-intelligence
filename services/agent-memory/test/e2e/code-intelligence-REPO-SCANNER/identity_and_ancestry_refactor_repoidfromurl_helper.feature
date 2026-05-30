@story-code-intelligence:REPO-SCANNER @phase-identity-and-ancestry-refactor @stage-repoidfromurl-helper @setup-inline
Feature: RepoIDFromURL helper determinism and validation

  The RepoIDFromURL helper derives a deterministic RFC 4122 v5 UUID from
  a repository URL using a pinned namespace constant. The derived RepoID
  must be byte-identical across calls, processes, and graphsink backends
  (Postgres, SQLite, in-memory) so that node/edge fingerprints agree
  regardless of storage target.

  Scenario: deterministic-for-same-url
    Given the same input URL "https://github.com/foo/bar"
    When RepoIDFromURL is called twice with that URL
    Then the returned RepoID is byte-identical across both calls
    And a separate child process computing RepoIDFromURL for the same URL returns the same value

  Scenario: different-urls-diverge
    Given two different URLs "https://github.com/foo/bar" and "https://github.com/foo/baz"
    When both are hashed with RepoIDFromURL
    Then the returned RepoID values differ

  Scenario: empty-url-rejected
    Given an empty string as the URL
    When RepoIDFromURL is called with the empty string
    Then a non-nil error is returned
    And the RepoID is the zero value