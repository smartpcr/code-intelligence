// Package keys implements the Policy Steward's Ed25519 signing-key
// manager (Stage 5.1, implementation-plan.md lines 444-457;
// tech-spec Sec 8.4).
//
// The package owns the lifecycle of the operator's signing keys:
// generation (KMS-backed), persistence of the PUBLIC material
// (the private key never leaves the KMS), rotation with the 24h
// overlap window pinned by tech-spec Sec 8.2 row 6, and the
// `policy.keys.list_active` read verb that returns the active
// set to the Insights / runbook tooling.
//
// Key surfaces:
//
//   - [KMS] is the interface to the operator's secret manager.
//     `Generate` mints a fresh Ed25519 keypair, seals the
//     private half inside the KMS, and returns the public bytes
//     plus an opaque `KeyHandle` the service stores alongside
//     the row. `Sign` is parameterised by the handle so a
//     restart can re-sign with a key whose private material was
//     created by an earlier process. Stage 5.1 ships TWO
//     implementations: [LocalSealedKMS] (production: AES-256-GCM
//     envelope encryption of the seed under an operator-injected
//     master key) and [InMemoryKMS] (test / scaffold). A
//     vendor-specific managed-service adapter (Azure Key Vault /
//     AWS KMS / HashiCorp Vault) is OUT-OF-SCOPE for Stage 5.1
//     and is owned by a future `policy-steward-kms-adapter`
//     workstream once the deployment-target vendor is selected;
//     the [KMS] interface contract is stable, so only the
//     concrete adapter remains to land.
//
//   - [Store] persists the public-side metadata. Stage 5.1 ships
//     TWO implementations: [SQLStore] (production: PostgreSQL
//     `clean_code.policy_signing_keys` via `database/sql`) and
//     [InMemoryStore] (test). [SQLStore] maps PostgreSQL
//     SQLSTATE 23505 to [ErrDuplicateKey] and 23514 to
//     [ErrInvalidPublicKey] so callers can branch on errors.Is.
//     The Manager itself is Store-agnostic.
//
//   - [Manager] is the high-level facade. It pins the rotation
//     guard (refuse normal Rotate while still inside the
//     overlap window of the most recent key), the half-open
//     `[valid_from, valid_until)` validity interval, the
//     derived `valid_until = next.valid_from + overlap`
//     formula, a verify-after-sign defence in [Manager.Sign],
//     and a periodic cache refresher [Manager.StartRefresh] so
//     a sibling-replica rotation propagates without restart.
//
//   - [Bootstrap] probes the KMS, loads the active set, mints
//     the first key on a fresh deployment, and returns a
//     Manager plus a readiness check the composition root
//     registers against the `signing_key_cache` health-gate.
//
//   - [Build] is the composition-root factory (used by
//     `cmd/clean-coded/main.go`). It validates the supplied
//     KMS+Store config FAIL-CLOSED (an incoherent mix like
//     in-memory KMS + persisted store, or local KMS without a
//     master key, returns an error rather than silently
//     stranding signing against ghosts).
//
// Per tech-spec Sec 8.4, this package writes ONLY the public
// key + an opaque KMS handle into the [Store]. The private key
// material is generated and held inside the [KMS] implementation
// and never crosses the package boundary as exported bytes.
//
// # Read transport
//
// The `policy.keys.list_active` verb is exposed by
// `internal/management/verbs.go` as HTTP/JSON at
// `GET /v1/policy/keys/list_active` (bare JSON array per the
// Stage 5.1 brief verbatim). HTTP/JSON v1 is the SOLE ratified
// transport for Stage 5.1; a gRPC adapter is out-of-scope and
// would land as a separate workstream if a downstream consumer
// requires streaming or strong typing.
//
// # Evaluator integration
//
// `internal/evaluator/gate.go` wraps [Manager.Verify] /
// [Manager.VerifyAny]. Both signing keys verify successfully
// during the 24h overlap window because the Manager's
// `[valid_from, valid_until)` calculation accepts both rows at
// once -- no special-cased "during overlap" code path.
package keys
