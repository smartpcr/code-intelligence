@story-code-intelligence:REFACTOR-GUIDE @phase-p0-reports-and-delivery @stage-report-and-version-subcommands @setup-inline
Feature: Report and Version Subcommands
  The cleanc binary exposes report re-render and version introspection
  sub-commands, plus a prod-build gate that refuses dev-mode policy bypass.

  Scenario: report re-render
    Given a findings.json previously written by an analyze run
    When cleanc report findings.json --out replay.md runs
    Then replay.md is byte-identical to the markdown that the analyze run emitted

  Scenario: schema mismatch refused
    Given a findings.json whose schemaVersion is "v0.0.0"
    When cleanc report findings.json runs
    Then exit code is 64 and stderr names both schema versions

  Scenario: version format
    Given a built binary
    When cleanc version runs
    Then stdout matches the regex "^cleanc \d+\.\d+\.\d+ \(build-tag=.*\) \(parsers=.+\) \(rule-packs=.+\)$"

  Scenario: prod build excludes bypass
    Given the internal/cli/devpolicy package
    When go test -tags prod ./internal/cli/devpolicy/... runs
    Then the prod-gated test passes and asserts ErrDevModeUnavailable with message "dev-mode policy bypass not available in prod build"
