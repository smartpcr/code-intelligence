@story-code-intelligence:REFACTOR-GUIDE @phase-p1-structured-prompt-emitter @stage-prompt-record-and-source-snippet-extractor @setup-inline
Feature: Prompt record and source snippet extractor
  The suggest package owns RefactorPromptRecord (the JSONL wire shape) and
  ExtractSnippet (raw-byte snippet extraction capped at a configurable
  line limit). These scenarios verify truncation semantics, raw-byte
  fidelity, and metric-evidence population per implementation-plan
  Sec 4.1 and architecture.md Sec 4.6.

  Scenario: snippet capped
    Given a 500-line fixture file
    And maxLines is set to 200
    When ExtractSnippet runs over lines 1 to 500
    Then the returned string has exactly 200 lines
    And truncated is true
    And the last line is "... [truncated 301 lines]"

  Scenario: snippet not truncated for small scope
    Given a 50-line fixture file
    And maxLines is set to 200
    When ExtractSnippet runs over lines 1 to 50
    Then truncated is false
    And the snippet contains exactly 50 lines

  Scenario: raw bytes
    Given a fixture file containing a literal tab followed by a multi-byte UTF-8 sequence
    When ExtractSnippet runs over the full file range
    Then the returned snippet preserves the exact byte sequence

  Scenario: metric evidence join
    Given a RefactorPromptRecord with one metric evidence entry for metric_kind "loc" value 2000 threshold 1500 op ">="
    Then metric_evidence contains exactly 1 entry
    And the entry has metric_kind "loc" value 2000 threshold 1500 op ">="
