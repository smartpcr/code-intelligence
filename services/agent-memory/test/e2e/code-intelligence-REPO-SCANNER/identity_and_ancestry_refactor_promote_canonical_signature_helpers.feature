@story-code-intelligence:REPO-SCANNER @phase-identity-and-ancestry-refactor @stage-promote-canonical-signature-helpers @setup-inline
Feature: Promote canonical-signature helpers

  The four canonical-signature helpers (CanonicalRepoSig, CanonicalPackageDir,
  CanonicalPackageSig, CanonicalFileSig) were promoted from unexported
  worker.go helpers to exported functions in canonical.go so that every
  graphsink backend (Postgres, SQLite, in-memory) mints byte-identical
  node identities for the same input. These E2E scenarios pin the exact
  byte output and verify the directory-normalisation edge cases that the
  acceptance criteria call out.

  Scenario: signature-byte-stable
    Given a fixed repoURL "https://example.test/repo" and relPath "pkg/foo.go"
    When the exported helpers run
    Then CanonicalRepoSig returns "https://example.test/repo"
    And CanonicalPackageDir returns "pkg"
    And CanonicalPackageSig returns "https://example.test/repo::pkg::pkg"
    And CanonicalFileSig returns "https://example.test/repo::file::pkg/foo.go"

  Scenario: package-dir-rootfile
    Given a fixed repoURL "https://example.test/repo" and relPath "main.go"
    When CanonicalPackageDir runs
    Then it returns "" (empty string == repo root)

  Scenario: package-dir-nested
    Given a fixed repoURL "https://example.test/repo" and relPath "internal/foo/bar.go"
    When CanonicalPackageDir runs
    Then it returns "internal/foo" with forward-slash separators