# Changelog: `services/clean-code`

All notable changes to the clean-code service are recorded here.
Newest at the top. Stage references map to
`docs/stories/code-intelligence-CLEAN-CODE/implementation-plan.md`.

## Stage 5.3 -- Override append-only mute lifecycle

### Added

- **`mgmt.override` write verb** (`POST /v1/mgmt/override`) --
  the operator mute/unmute kill switch per architecture Sec
  6.3 line 1357 + Sec 1.5.1 row 5. Management delegates to
  the Policy Steward, which appends an `override(override_id,
  rule_id, scope_filter JSONB, mute, reason, actor_id,
  created_at)` row in the Policy / rules sub-store
  (architecture Sec 5.3.6 lines 1160-1170; tech-spec Sec 10A
  "mute lifecycle" pin). The handler returns
  `{"override_id": "..."}` -- a single id, matching the
  architecture `-> OverrideId` return type.
- `Steward.Override(ctx, OverrideRequest)` verb +
  `Steward.LatestMatchingOverride(ctx, ruleID, CandidateScope)`
  read helper (the latter is the entry point the evaluator
  (Stage 5.7) reads at gate time). The read semantic is
  **candidate-scope/glob matching**, not exact JSON equality
  (architecture Sec 5.3.6 line 1171 pin: `scope_filter matches
  the candidate scope`). Glob vocab: `*` matches any rune
  run (including empty, across dots/slashes), `?` matches one
  rune, everything else literal; the pattern is anchored
  end-to-end. Implemented in
  `internal/policy/steward/scope_glob.go` with a cached
  regexp.
- New `Store` primitives: `RuleExistsByID` (logical-FK helper
  on `Override.rule_id -> Rule.rule_id` -- a separate sibling
  to `RuleExists(rule_id, version)`), `InsertOverride`, and
  `LatestMatchingOverride`. The SQL implementation
  pre-filters with the `scope_filter->>'repo_id'` and
  `scope_filter->>'scope_kind'` JSONB extractors (so only the
  candidate's `(repo_id, scope_kind)` partition is scanned)
  and applies the glob match in Go in descending
  `(created_at, override_id)` order. **No `LIMIT`** is used:
  a newer non-matching row must not hide an older matching
  glob.
- `CandidateScope` value type + `IsValid()` predicate +
  `ErrInvalidCandidateScope` sentinel for the read path. The
  steward refuses an empty candidate (empty `repo_id`,
  unknown `scope_kind`, or whitespace-only `signature`)
  before consulting the store so the gate cannot fail-open
  by silently matching nothing.
- Sentinels: `ErrInvalidOverride` (shape validation),
  `ErrUnknownRule` (FK miss), and `ErrInvalidCandidateScope`
  (read-side validation). The first two map to HTTP 400.
- `ScopeKind` typed enum + `ScopeFilter`/`Override`/
  `OverrideRequest`/`CandidateScope` value types in the
  steward package.
- `VerbMgmtOverridePath` and `OIDCSubjectHeader` exported
  constants for the canonical mount + auth header contract.
- `noActiveSigner` null-object [Signer] in the steward
  package (iter 3). Installed by `steward.New` whenever
  `cfg.Signer == nil` so `s.signer` is never literally nil --
  `VerifyPolicyVersionSignature` calls `s.signer.VerifyAny`
  directly and would otherwise panic. The null object reports
  no active keys, so the Stage 5.2 signing verbs surface
  `ErrNoActiveSigningKey` via the existing
  `len(ListActive()) == 0` branch while
  `Steward.Override` (which doesn't consult the signer)
  keeps serving 200.
- `buildPolicyWriter(db, signer, log)` helper in
  `cmd/clean-coded/main.go` (iter 3) -- the testable
  composition seam that constructs the Steward +
  `*management.PolicyWriter` UNCONDITIONALLY (not gated on
  `cfg.KMSProvider != ""`). Pinned by
  `TestBuildPolicyWriter_ScaffoldModeProducesWriter`.

### Invariants pinned by tests

- **NO `expires_at` column / wire field.** The
  `DisallowUnknownFields` decoder rejects any caller-supplied
  `expires_at` with 400; the migration 0003 schema also has no
  such column. Pinned by
  `TestPolicyWriter_Override_RejectsExpiresAt` +
  `TestSQLStore_OverrideRoundTrip` (the SQL prep template
  mirrors the migration shape, including the
  `mute = false OR reason IS NOT NULL` CHECK constraint --
  no whitespace-trim defence at the DB level; the validator
  carries that contract).
- **NO `policy_version_id` column.** Overrides bind to rules
  (rule_id lineage), not to a specific policy version --
  architecture Sec 5.3.6 line 1166. Encoded in the `Override`
  struct (no field) and the SQL prep template (no column).
- **`actor_id`, not `created_by`.** The HTTP layer sources
  the OIDC subject from the `X-OIDC-Subject` header set by
  the auth gateway. Bodies containing `actor_id` are
  rejected with 400 to keep the trust boundary at the
  gateway. Pinned by
  `TestPolicyWriter_Override_RejectsBodyActorID`.
- **Append-only.** The `Store` interface has no
  `UpdateOverride` / `DeleteOverride`; unmute is a fresh
  INSERT with `mute=false`. Pinned by
  `TestStore_OverrideAppendOnlyInterfaceShape`.
- **Latest-row-wins read semantics with glob matching.** Both
  the in-memory store and the SQLStore order by
  `(created_at DESC, override_id DESC)` and apply the
  scope-signature glob match. The first matching row wins;
  there is no `LIMIT` short-circuit. Pinned by
  `TestSteward_Override_LatestRowWins`,
  `TestStore_LatestMatchingOverrideTieBreakOnOverrideID`,
  `TestSteward_LatestMatchingOverride_GlobMatchesSubScope`,
  `TestSteward_LatestMatchingOverride_StarMatchesEverything`,
  `TestSteward_LatestMatchingOverride_QuestionMarkMatchesOneChar`,
  `TestSteward_LatestMatchingOverride_NewerBroadOverridesOlderLiteral`,
  `TestSQLStore_OverrideLatestRowWins`,
  `TestSQLStore_OverrideGlobMatchesSubScope`,
  `TestSQLStore_OverrideGlobSkipsNonMatchingRow` (this last
  pins the no-LIMIT defence -- a newer non-matching row
  cannot mask an older matching glob).
- **No signing-key precondition (kill-switch contract).**
  Unlike Publish / Activate / PublishRulepack,
  `Steward.Override` does NOT call `checkSigningKey`. The kill
  switch must remain operable during a signing-key outage --
  the worst time to deny an emergency mute. The contract is
  enforced at three layers:

  1. **Steward layer:** `Steward.Override` bypasses
     `checkSigningKey`. Pinned by
     `TestSteward_Override_NoSigningKeyAccepted`.
  2. **HTTP handler layer:** `PolicyWriter.Override` does not
     depend on a wired signer. Pinned by
     `TestPolicyWriter_Override_AcceptsWithoutSigningKey`
     (stub-driven).
  3. **Composition-root layer (Stage 5.3 + iter 3):**
     `cmd/clean-coded/main.go` builds the Steward +
     `PolicyWriter` UNCONDITIONALLY -- not gated on
     `cfg.KMSProvider != ""`. The Steward is constructed with
     `Signer: nil`; `steward.New` installs a
     [`noActiveSigner`] null object so `s.signer` is never
     literally nil (which would have panicked
     `VerifyPolicyVersionSignature`'s direct `s.signer.VerifyAny`
     call). The null signer reports an empty active-key set,
     which makes the Stage 5.2 verbs naturally return 503 via
     the existing `len(ListActive()) == 0` branch while
     Override proceeds. Pinned by
     `TestSteward_NewRequiresStore` (the constructor now
     accepts a nil Signer),
     `TestSteward_PublishRefusesWhenSignerNil` (the null
     object still keeps the signing verbs locked),
     `TestBuildPolicyWriter_ScaffoldModeProducesWriter` (the
     wiring helper produces a non-nil writer in scaffold
     mode), and `TestRootMux_ScaffoldModeOverrideMounted_200`
     (the composition root serves 200 on
     `POST /v1/mgmt/override` with no KMS wired, while the
     same mux still returns 503 on `POST /v1/policy/publish`).
- **Reason required when `mute=true`.** The validator
  rejects empty / whitespace-only reasons with 400 before
  any persistence work; the SQL CHECK constraint
  `override_reason_required_when_muted` (which only enforces
  `mute = false OR reason IS NOT NULL`) guards the schema
  side. Pinned by
  `TestSteward_Override_RejectsMuteWithoutReason`,
  `TestSQLStore_OverrideMutedReasonNullIsRejectedByCheck`,
  and `TestSQLStore_OverrideMutedWhitespaceReasonAcceptedByCheck`
  (the latter documents that the production CHECK does NOT
  trim whitespace -- the validator carries that contract).
- **No TTL.** An override row older than any reasonable
  retention horizon (test plants 400 days in the past)
  remains the active mute when no fresher row exists.
  Pinned by `TestSteward_Override_OldRowRemainsActiveWithoutTTL`
  (tech-spec Sec 10A "v1 mute lifecycle has no TTL").
- **Read path refuses empty candidate.** The steward
  short-circuits with `ErrInvalidCandidateScope` if the
  evaluator hands it an empty `CandidateScope`. Pinned by
  `TestSteward_LatestMatchingOverride_RejectsInvalidCandidate`.

### Documentation

- `docs/runbook.md` -- new "`mgmt.override` write verb (Stage
  5.3)" section covering the POST body shape, the
  `X-OIDC-Subject` trust boundary, the append-only mute /
  unmute flow, latest-row-wins read semantics, the
  glob-matching vocab (`*` / `?` / literal, end-to-end
  anchored), no-TTL, and the kill-switch property (works
  during signing-key outage).
- `docs/rollout.md` -- Stage 5.3 entry; no new migrations
  (the `clean_code.override` table shipped in migration 0003
  during Stage 1.4), no new env vars; the gateway already
  populates `X-OIDC-Subject` for the Stage 5.2 verbs.

## Stage 5.2 -- Policy publish/activate/rulepack verbs (iter 2 follow-ups)

### Added

- `Steward.Publish` now enforces the JSON-FK contract for
  `rule_refs` and `threshold_refs` at write time (migration
  0003 lines 280/462: "FK target enforced by the writer, not
  by SQL, since the reference lives inside a JSON document").
  Unknown refs surface as the new sentinels
  `ErrUnknownRuleRef` / `ErrUnknownThresholdRef` (HTTP 400)
  and the request is rejected **before** any signing material
  is consumed (validate-before-sign).
- `Steward.ActivePolicyVersion(ctx)` -- resolves the active
  `policy_version` row via the canonical lookup
  (`LatestActivation` -> `GetPolicyVersion`). This is the
  evaluator-pickup entry point: after `policy.activate(pvB)`
  runs, this method returns `pvB` (latest-row-wins) even if
  `pvA` was activated first. Covered by
  `TestSteward_EvaluatorPicksUpActivatedVersion` (in-memory)
  and `TestSQLStore_EvaluatorPicksUpActivatedVersion` (live
  PG, skipped if `CLEAN_CODE_PG_URL` is unset).
- `Store.RuleExists` / `Store.ThresholdExists` /
  `Store.InsertThreshold` primitives backing the FK
  enforcement. `InsertThreshold` is an append-only primitive
  for tests and future bootstrap tooling -- no
  `policy.publish_threshold` verb exists in Stage 5.2.
- `validatePublishRequest` now rejects duplicate rule_refs or
  threshold_refs within a single payload (400, distinct from
  the FK-miss sentinels).

### Documentation

- `docs/runbook.md` "Policy Steward write verbs (Stage 5.2)"
  rewritten to clarify which verbs sign: only `policy.publish`
  produces a signed row (`policy_version.signature`).
  `policy.activate` and `policy.publish_rulepack` require an
  active signing key as a deployment-state precondition but
  do NOT write a signature column. Added the FK-enforced-by-
  writer contract paragraph for `rule_refs`/`threshold_refs`.

## Stage 5.2 -- Policy publish/activate/rulepack verbs

### Added

- `internal/policy/steward/` package: in-process actor that
  owns the three canonical Stage 5.2 write verbs (architecture
  Sec 6.5 + tech-spec Sec 8.5 lines 963-970):
  - `Steward.Publish` -- appends an immutable
    `clean_code.policy_version` row with an Ed25519 signature
    over canonical JSON of `(rule_refs, threshold_refs,
    refactor_weights)`. Architecture Sec 5.3.3, G5
    immutability.
  - `Steward.Activate` -- appends a
    `clean_code.policy_activation` row. NO `scope` parameter
    (architecture Sec 5.3.4 single-tenant pin); latest row by
    `created_at` wins.
  - `Steward.PublishRulepack` -- appends one `rule_pack` row
    plus N `rule` rows in a single transaction. Composite-PK
    collisions surface as `ErrDuplicateRulePack` /
    `ErrDuplicateRule`.
  - All three verbs refuse when the `keys.Manager` has no
    active key (`ErrNoActiveSigningKey`).
- `internal/policy/steward/canonicalize.go`: deterministic
  canonical-JSON encoder used as the signing input. Recursive
  sorted-key walk, `json.Number` integer-preservation, and
  nil-slice -> `[]` normalisation so the signed bytes survive
  a JSONB round-trip through PostgreSQL.
- `internal/policy/steward/verbs.go`: `Registry` pinning the
  canonical 3-verb closed set. `Lookup` returns
  `ErrUnimplementedVerb` for any non-canonical name (in
  particular the historical drafts `policy.rulepack.add`,
  `policy.rulepack.remove`, and `policy.override`).
- `internal/policy/steward/store.go`: append-only `Store`
  interface (NO `Update`/`Delete` methods at the type level --
  a compile-time witness of G3) plus a concurrent-safe
  `InMemoryStore`.
- `internal/policy/steward/sql_store.go`: production
  `SQLStore` backed by `database/sql` + `lib/pq`. Schema-
  qualified table names via `pq.QuoteIdentifier`. Transactional
  `InsertRulePackAndRules`; SQLSTATE 23505 -> `ErrDuplicate*`,
  SQLSTATE 23503 -> `ErrUnknownPolicyVersion`.
- `internal/management/policy_verbs.go`: HTTP write-side
  handlers mounting `POST /v1/policy/publish`,
  `POST /v1/policy/activate`, `POST /v1/policy/publish_rulepack`.
  `Decoder.DisallowUnknownFields()` rejects the historical
  `scope` field on activate (returns 400). Status table:
  200/400/405/409/500/503.
- `internal/management/policy_verbs.go::UnimplementedVerb`:
  returns 501 + `{error:"unimplemented_verb", verb:"..."}` for
  the banned-draft verb paths (`/v1/policy/rulepack/add`,
  `/v1/policy/rulepack/remove`, `/v1/policy/override`).
- `cmd/clean-coded/main.go` + `routes.go`: composition root
  now constructs `steward.Steward` alongside the keys cache
  (SQL-backed when `CLEAN_CODE_PG_URL` is set, in-memory
  otherwise) and mounts the new write routes + banned-verb
  501 routes onto the root mux.
- Test coverage: ~30 new tests across
  `internal/policy/steward/{store,steward,sql_store}_test.go`
  and `internal/management/policy_verbs_test.go`. SQLStore
  integration tests skip when `CLEAN_CODE_PG_URL` is unset
  and use isolated schema `clean_code_steward_test` so the
  three live-PG suites (storage migrate, keys SQLStore,
  steward SQLStore) never race.

### Notes

- `policy_version.signature` carries the Ed25519 signature
  bytes only -- the architecture canon (Sec 5.3.3) does NOT
  include a `signing_key_id` column. The evaluator verifies
  via `keys.Manager.VerifyAny`, which trials every active
  key. After a rotation overlap exceeds the cache window, an
  older `policy_version` row may fail verification; tracking
  that as Stage 6+ Evaluator work.

## Stage 5.1 -- Policy Steward signing-key store

### Added

- `internal/policy/keys/` package: Ed25519 keypair manager with
  rotation, half-open `[valid_from, valid_until)` window,
  policy signature verification (`Verify`, `VerifyAny`), and
  active-key projection (`ListActive`).
- `internal/policy/keys/sql_store.go`: production
  PostgreSQL-backed `Store` implementation using
  `database/sql` + `lib/pq`. Maps SQLSTATE `23505` to
  `ErrDuplicateKey` and `23514` to `ErrInvalidPublicKey`.
- `internal/policy/keys/local_kms.go`: production
  `LocalSealedKMS` -- envelope encryption (AES-256-GCM) of
  Ed25519 seeds under an operator-injected master key. Handle
  prefix `local-v1:`. The master key never touches PostgreSQL.
- `internal/policy/keys/build.go`: composition-root factory
  `Build(ctx, BuildConfig) -> (*BuildResult, error)` with
  fail-closed validation (local requires master key + PG;
  in-memory rejects both).
- `internal/management/` package: `Reader.ListActiveSigningKeys`
  + HTTP handler exposing
  `GET /v1/policy/keys/list_active` as a bare JSON array.
- `internal/evaluator/gate.go`: `Gate.VerifyPolicy` and
  `Gate.VerifyAnyPolicySignature` -- both consult the signing
  cache so the 24h overlap window is enforced uniformly across
  the evaluator surface.
- `cmd/clean-coded/main.go`: composition root now wires the
  signing-key cache, registers `signing_key_cache` readiness
  check, mounts the management routes, and spawns a 5-minute
  cache-refresh ticker.
- `migrations/0005_policy_signing_keys.{up,down}.sql`:
  `clean_code.policy_signing_keys` table with public-key
  fingerprint, opaque KMS handle, half-open lifecycle, and
  append-only grants (`INSERT`+`SELECT` to steward, `SELECT`
  to every other writer role).
- Config: `KMSProvider`, `KMSMasterKeyHex` fields with
  fail-closed validation.

### Changed

- `cmd/clean-coded/main.go` import paths corrected to the
  module path `github.com/microsoft/code-intelligence/services/clean-code/...`.
  Pre-existing `forge/services/...` import paths were broken.

### Operational notes

See `docs/runbook.md` for the operator-facing surface and
`docs/rollout.md` for the per-environment bootstrap +
verification steps.

### Scope boundaries (ratified for Stage 5.1)

These were originally floated as open operator questions and
are now PINNED so Stage 5.1 ships with a closed contract.
Future workstreams own the deferred work:

- **Transport: HTTP/JSON v1, sole ratified surface.** A gRPC
  adapter is out-of-scope. If a downstream consumer ever needs
  streaming / strong-typed verbs, a `management-grpc-adapter`
  workstream would land it alongside HTTP with regression
  tests pinning both transports to the same wire shape.
- **KMS backend: `LocalSealedKMS` (AES-256-GCM envelope) is
  the only Stage 5.1 production impl.** A managed-service
  adapter (Azure Key Vault / AWS KMS / HashiCorp Vault) is
  owned by a future `policy-steward-kms-adapter` workstream
  once the deployment-target vendor is selected. The `KMS`
  interface contract is stable, so the future adapter only
  needs to land its concrete implementation -- Manager /
  Store / rotation / evaluator integration / read verb all
  continue to work unchanged.
