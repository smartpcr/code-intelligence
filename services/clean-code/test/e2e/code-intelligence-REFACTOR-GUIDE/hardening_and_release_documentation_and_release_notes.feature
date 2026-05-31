@story-code-intelligence:REFACTOR-GUIDE @phase-hardening-and-release @stage-documentation-and-release-notes @setup-inline
Feature: Documentation and Release Notes
  User-facing documentation gates the release: the README must have a
  `cleanc CLI` section, the usage doc must reference every shipped flag,
  the prompt-format spec must carry the current version string, and the
  CHANGELOG must mention `cleanc` under an Unreleased heading.

  Scenario: README has cleanc section
    Given the updated "services/clean-code/README.md"
    When grep -F "## cleanc CLI" runs against it
    Then it returns at least one match

  Scenario: usage doc references flags
    Given the file "docs/cleanc/USAGE.md"
    When grep -F "--emit-prompts" runs against it
    Then it returns at least one match

  Scenario: prompt format doc has version
    Given the file "docs/cleanc/PROMPT-FORMAT.md"
    When grep -F "v1.2026.05" runs against it
    Then it returns at least one match

  Scenario: changelog updated
    Given the file "CHANGELOG.md"
    When grep -F "cleanc" runs against it
    Then it returns at least one match under an "## Unreleased" heading
