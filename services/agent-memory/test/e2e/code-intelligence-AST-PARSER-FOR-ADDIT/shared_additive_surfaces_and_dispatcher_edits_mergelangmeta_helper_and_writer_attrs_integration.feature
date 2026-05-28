@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-shared-additive-surfaces-and-dispatcher-edits @stage-mergelangmeta-helper-and-writer-attrs-integration @setup-compose
Feature: mergeLangMeta helper and writer attrs integration

  The mergeLangMeta helper folds per-language metadata from
  MethodDecl.LangMeta / ClassDecl.LangMeta into attrs_json at
  write time, and the writer merges ReceiverCalls into calls_raw.
  First-class keys always win over LangMeta; nil LangMeta is a
  no-op that preserves byte-identical output for existing parsers.

  Scenario: First-class key wins
    Given a fake parser that sets LangMeta language to bogus
    When methodAttrs runs through the dispatcher
    Then the persisted attrs_json language equals the dispatchers first-class value not bogus

  Scenario: LangMeta nil is a no-op
    Given a TS method fixture with nil LangMeta
    And a Python method fixture with nil LangMeta
    When methodAttrs runs on each fixture
    Then each fixture attrs_json is byte-identical to its pre-merge baseline

  Scenario: New LangMeta key flows through
    Given a fake parser that sets MethodDecl LangMeta to receiver r and receiver_ptr true
    When methodAttrs runs through the dispatcher
    Then the persisted attrs_json receiver equals r
    And the persisted attrs_json receiver_ptr equals true

  Scenario: ReceiverCalls land in calls_raw
    Given a MethodDecl with Calls log_global and ReceiverCalls identify
    When methodAttrs runs through the dispatcher
    Then the persisted attrs_json calls_raw is the deduped ordered slice log_global identify

  Scenario: ReceiverCalls only still emits calls_raw
    Given a MethodDecl with nil Calls and ReceiverCalls Bar
    When methodAttrs runs through the dispatcher
    Then the persisted attrs_json calls_raw equals exactly the slice Bar