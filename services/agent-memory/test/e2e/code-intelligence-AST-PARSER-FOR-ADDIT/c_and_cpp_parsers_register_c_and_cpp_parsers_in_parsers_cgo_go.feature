@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-c-and-cpp-parsers @stage-register-c-and-cpp-parsers-in-parsers-cgo-go @setup-inline
Feature: Register C and C++ parsers in parsers_cgo.go

  Validates that the dispatcher routes .c, .cpp, and .h extensions
  to the correct tree-sitter parsers when CGO is enabled.

  Scenario: .c routes to C
    Given the dispatcher under CGO=on
    When selectParser runs for "foo.c"
    Then the selected parser Language is "c"

  Scenario: .cpp routes to C++
    Given the dispatcher under CGO=on
    When selectParser runs for "foo.cpp"
    Then the selected parser Language is "cpp"

  Scenario: .h routes to C unconditionally
    Given the dispatcher under CGO=on
    When selectParser runs for "foo.h" with no hints
    Then the selected parser Language is "c"
    When selectParser runs for "foo.h" with hints "cpp"
    Then the selected parser Language is "c"