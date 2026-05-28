@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-shared-additive-surfaces-and-dispatcher-edits @stage-normalizehints-alias-expansion @setup-compose
Feature: normalizeHints alias expansion

  The normalizeHints function resolves language-hint aliases
  (e.g. "golang" → "go", "cs" → "csharp") and preserves
  canonical names unchanged. Extension-based routing takes
  precedence over hint-based routing when a parser is
  registered for the file's extension.

  Scenario: normalizeHints resolves new aliases
    Given LanguageHints equal to ["golang"]
    When normalizeHints runs
    Then the result contains "go"

  Scenario: Existing aliases preserved
    Given LanguageHints equal to ["typescript"]
    When normalizeHints runs
    Then the result still contains "typescript"

  Scenario: Extension precedence over hint
    Given a ".h" file with LanguageHints equal to ["cpp"]
    When selectParser runs
    Then the returned parser Language is "c"
