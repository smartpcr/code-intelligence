@story-code-intelligence:REFACTOR-GUIDE @phase-foundations @stage-dev-policy-loader @setup-inline
Feature: Dev-mode policy loader
  The devpolicy package loads canonical steward shapes from the
  binary-baked rulepacks fs.FS (the default) OR an operator-supplied
  --policy <path> filesystem override, synthesises an UNSIGNED
  PolicyVersion whose PolicyVersionID is stable per (loaded packs,
  effort model), and exposes a build-tag-paired surface where the
  prod build refuses the bypass at the earliest reachable layer.
  Both surfaces of the build-tag-paired contract are exercised in
  this feature: the dev surface in the first three scenarios and
  the prod surface in the fourth. The fifth scenario pins the
  operator-facing banner byte-for-byte so a future edit cannot
  silently drift the constraint C10 string.

  Scenario: embedded packs loaded in dev build
    Given a build WITHOUT the prod tag
    And the LoaderSource has UseEmbedded set to true
    When devpolicy.NewLoader().Load runs against the embedded fs.FS
    Then the returned Bundle.Rules contains at least one rule for every YAML under services/clean-code/policy/rulepacks/{solid,decoupling}/
    And the returned Bundle.PolicyVersion.Signature is nil

  Scenario: stable policy id across runs
    Given the same embedded pack set walked twice in succession
    When synthesisePolicyVersion is invoked both times with the same effort_model_version
    Then the returned PolicyVersionID is byte-for-byte identical between the two runs
    And the second Bundle.Rules slice is a permutation-stable copy of the first

  Scenario: filesystem override accepts --policy <path>
    Given a temporary directory containing a custom.yaml file that matches the embedded rulepack schema
    And the LoaderSource has UseEmbedded false and DirPath set to that temp directory
    When devpolicy.NewLoader().Load runs in a dev build
    Then the returned Bundle.RulePacks length is 1
    And the rule ids in Bundle.Rules match the ids declared in custom.yaml

  Scenario: prod build refuses the dev-mode bypass at the loader layer
    Given a binary built with go build -tags prod of the devpolicy package
    When the test invokes devpolicy.NewLoader().Load with any LoaderSource
    Then the returned error satisfies errors.Is(err, devpolicy.ErrDevModeUnavailable)
    And the error string contains the literal phrase "dev-mode policy bypass not available in prod build"
    And the returned Bundle has empty Rules and RulePacks slices

  Scenario: banner text matches constraint C10 byte-for-byte
    Given a dev build
    When devpolicy.EmitBanner writes to a bytes.Buffer
    Then the buffer contents exactly equal the C10 banner string followed by a single newline
    And the constant devpolicy.BannerText equals "WARNING: dev-mode policy is unsigned. Do NOT use cleanc output as the source of truth for a production gate."
