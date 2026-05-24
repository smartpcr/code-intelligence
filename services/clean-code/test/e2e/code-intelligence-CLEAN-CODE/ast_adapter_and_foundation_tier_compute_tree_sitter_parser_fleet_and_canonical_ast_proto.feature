@story-code-intelligence:CLEAN-CODE @phase-ast-adapter-and-foundation-tier-compute @stage-tree-sitter-parser-fleet-and-canonical-ast-proto @setup-inline
Feature: Tree-sitter parser fleet and canonical AST proto
  Validates that the tree-sitter parser registry supports the four v1-pinned
  languages (Go, Python, TypeScript, Java), rejects unsupported languages per
  tech-spec Sec 8.6 v1 pin, and that the canonical AST protobuf representation
  survives round-trip serialisation without information loss.

  Scenario: parser-supports-v1-four-languages
    Given a fixture file per v1-pinned language (Go, Python, TypeScript, Java)
    When the registry returns a parser and Parse runs
    Then each returns a non-empty AstFile with the language tag set and at least one AstScope
    And attempting to register a fifth language fails the registry guard

  Scenario: proto-round-trip
    Given a parsed AstFile
    When it is serialised to protobuf wire format and deserialised
    Then the resulting struct equals the original with no information loss