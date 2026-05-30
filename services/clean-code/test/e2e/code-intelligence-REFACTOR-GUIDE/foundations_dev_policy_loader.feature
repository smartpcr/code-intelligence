@story-code-intelligence:REFACTOR-GUIDE @phase-foundations @stage-dev-policy-loader @setup-inline
Feature: Dev-mode policy loader
  The dev-mode policy loader reads YAML rule pack files into an unsigned
  in-memory PolicyVersion for local CLI use without PostgreSQL or a signing
  key, with a build-tag-gated prod guard that prevents the bypass from
  shipping in production binaries.

  Scenario: embedded packs loaded
    Given a no-tag build
    When Loader.Load is called with LoaderSource UseEmbedded true
    Then every YAML file under the embedded rulepacks FS produced at least one rule in Bundle.Rules

  Scenario: stable policy id
    Given the embedded pack set is loaded once to obtain the rule list
    When SynthesisePolicyVersion is called on that rule list twice
    Then the two returned PolicyVersionIDs are byte-for-byte identical

  Scenario: filesystem override
    Given a temp directory with custom.yaml matching the embedded shape
    When Loader.Load is called with LoaderSource UseEmbedded false and DirPath set to the temp directory
    Then the returned Bundle.RulePacks length is 1 and the rule ids match the YAML

  Scenario: prod build refuses bypass
    Given a go build with tags prod of internal/cli/devpolicy
    When the test calls Loader.Load
    Then it returns the error "dev-mode policy bypass not available in prod build"

  Scenario: banner text exact
    Given a dev build
    When EmitBanner writes to a bytes.Buffer
    Then the buffer string equals "\u26a0  DEV MODE \u2014 unsigned policy bypass active. Not for production use.\n"