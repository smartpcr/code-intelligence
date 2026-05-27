@story-code-intelligence:CLEAN-CODE @phase-ast-adapter-and-foundation-tier-compute @stage-scope-identity-derivation-and-scopebinding-writer @setup-inline
Feature: Scope identity derivation and ScopeBinding writer
  Validates that DeriveScopeID produces deterministic, SHA-stable UUIDs from
  (repo_id, scope_kind, canonical_signature) tuples, and that the
  ScopeBinding writer performs idempotent upserts preserving first_seen_sha.

  Scenario: scope-id-determinism
    Given the same repo_id, scope_kind, canonical_signature, and first_seen_sha invoked twice
    When DeriveScopeID runs for both invocations
    Then it returns the same UUID both times

  Scenario: scope-id-stable-across-shas
    Given a signature "pkg.Foo#bar(int)" first seen at SHA A
    When the same signature appears at SHA B
    Then DeriveScopeID at SHA B returns the same scope_id as at SHA A

  Scenario: scope-binding-idempotent-write
    Given a scope_binding row already present for a scope_id
    When the writer re-inserts the same scope_id at a different SHA
    Then no error surfaces and the row count remains unchanged and first_seen_sha is preserved