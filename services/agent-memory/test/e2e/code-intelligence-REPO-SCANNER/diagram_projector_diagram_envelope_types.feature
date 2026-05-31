@story-code-intelligence:REPO-SCANNER @phase-diagram-projector @stage-diagram-envelope-types @setup-inline
Feature: Diagram envelope types

  The diagram envelope is the single JSON wire format shared by both
  diagram families (module and callchain). Its field order, nil-safe
  collections, and round-trip fidelity are pinned by architecture S4.4
  and the golden file in internal/diagram/testdata/.

  Scenario: envelope-marshal-key-order
    Given an envelope value matching the golden fixture
    When encoding/json.Marshal runs
    Then the resulting bytes match the stored golden file byte-for-byte

  Scenario: envelope-unmarshal-roundtrip
    Given marshalled bytes from the golden file
    When unmarshalled back into the struct and re-marshalled
    Then the second pass equals the first byte-for-byte
