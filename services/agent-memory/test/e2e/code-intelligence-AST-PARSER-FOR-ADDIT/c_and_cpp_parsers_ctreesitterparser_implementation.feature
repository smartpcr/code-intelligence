@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-c-and-cpp-parsers @stage-ctreesitterparser-implementation @setup-inline @stub
Feature: C tree-sitter parser implementation (STUB)

  The cTreeSitterParser stage will add a tree-sitter-backed C parser
  that emits ClassDecl nodes for structs and MethodDecl nodes for
  functions, plus Imports for #include directives and same-file
  call edges (per the story brief §1 "Per language: C — functions,
  structs, includes, function calls").

  This feature file is landed by the Go-parser stage (iter 14, in
  response to iter-13 evaluator items 1 and 2) as a STUB so the
  workstream's declared changed-file set matches the worktree. The
  sibling stage workstream
  `stage-3.1-ctreesitterparser-implementation` REPLACES this feature
  in place with full walker scenarios when its branch merges to
  `feature/memory`. Until then, the scenarios below pin only the
  stub contract that `services/agent-memory/internal/repoindexer/ast/parser_treesitter_c.go`
  exposes (LanguageParser surface + empty ParseResult).

  Scenario: Stub Language and Extensions contract
    Given the C tree-sitter parser is constructed
    Then the C parser Language is "c"
    And the C parser Extensions include ".c"
    And the C parser Extensions include ".h"

  Scenario: Stub Parse returns no extracted nodes for a translation unit
    Given C source for stub:
      """
      #include <stdio.h>
      struct widget { int field; };
      void run(void) { printf("ok\n"); }
      """
    When the source is parsed with the C tree-sitter parser
    Then the stub C ParseResult Classes is empty
    And the stub C ParseResult Methods is empty
    And the stub C ParseResult Imports is empty
