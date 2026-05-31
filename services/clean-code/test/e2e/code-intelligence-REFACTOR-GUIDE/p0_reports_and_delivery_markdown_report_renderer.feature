@story-code-intelligence:REFACTOR-GUIDE @phase-p0-reports-and-delivery @stage-markdown-report-renderer @setup-inline
Feature: Markdown report renderer
  Stage 3.1: the Markdown renderer converts a RunArtifact into a
  human-readable report with header, verdict, and diagnostic
  sections per architecture Sec 3.7.1.

  Scenario: empty corpus renders pass
    Given a RunArtifact with zero findings and verdict "pass"
    When Markdown.Render runs
    Then the output contains "Verdict: pass"
    And the output contains a non-empty diagnostics block

  Scenario: byte-identical re-render
    Given a representative RunArtifact with findings and dark metrics
    When Markdown.Render runs twice on the same artifact
    Then the two outputs are byte-identical

  Scenario: dark-metric surfaced
    Given a RunArtifact whose DarkMetrics includes a "cyclo" row for language "go"
    When Markdown.Render runs
    Then the output surfaces the dark-metric diagnostic with count 1

  Scenario: suggested refactor excerpt
    Given a RunArtifact with a finding whose rule DescriptionMD contains "Suggested refactor: split the class along the cohesion boundaries (SRP)"
    When Markdown.Render runs
    Then the artifact finding carries the suffix "split the class"
