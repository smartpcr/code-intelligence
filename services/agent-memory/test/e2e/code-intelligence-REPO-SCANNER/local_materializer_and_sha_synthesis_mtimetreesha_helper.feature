@story-code-intelligence:REPO-SCANNER @phase-local-materializer-and-sha-synthesis @stage-mtimetreesha-helper @setup-inline
Feature: MTimeTreeSHA helper

  The MTimeTreeSHA function computes a deterministic 32-character lowercase
  hex digest from the mtime/size tree rooted at a given directory. It is used
  to synthesise a content-address for non-git local scans before a Workspace
  is constructed.

  Scenario: stable-on-noop
    Given a directory tree with files "a.txt", "sub/b.txt", and "sub/c/d.txt"
    When MTimeTreeSHA is called twice with no file changes between calls
    Then both calls return the identical 32-char hex string

  Scenario: mtime-bump-changes-hash
    Given a directory tree with files "a.txt" and "b.txt"
    When one file's mtime is updated via os.Chtimes
    And MTimeTreeSHA is recomputed
    Then the returned string differs from the pre-update value

  Scenario: exclude-applied
    Given a directory tree with "src/main.go" and a ".git/" directory containing files
    When MTimeTreeSHA runs with excludes [".git"]
    Then files under ".git/" contribute zero bytes to the hash
    And removing ".git/" and re-hashing without excludes produces the identical output

  Scenario: missing-root-errors
    Given a path that does not exist
    When MTimeTreeSHA runs
    Then a non-nil error is returned and the empty string is returned for the hash
