@story-code-intelligence:CLEAN-CODE @phase-policy-steward-and-solid-rule-engine @stage-policy-steward-signing-key-store @setup-compose
Feature: Policy Steward signing key store
  Validates that the policy-steward service enforces a 24-hour overlap
  window for rotated signing keys and refuses to start when the KMS
  backend is unreachable (architecture Sec 1.6 pin: policy-signing-required=v1 required).

  Scenario: overlap-window-enforced
    Given a key rotation occurred at T0
    When a payload signed by the old key arrives at T0+23h59m
    Then verification succeeds
    When a payload signed by the old key arrives at T0+24h+1s
    Then verification fails

  Scenario: kms-unavailable-blocks-start
    Given the KMS is unreachable at startup and the "policy-signing-required=v1 required" pin is active
    When the service initialises
    Then it exits non-zero with a clear error