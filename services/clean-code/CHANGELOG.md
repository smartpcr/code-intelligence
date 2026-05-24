# Changelog: `services/clean-code`

All notable changes to the clean-code service are recorded here.
Newest at the top. Stage references map to
`docs/stories/code-intelligence-CLEAN-CODE/implementation-plan.md`.

## Stage 5.4 -- Predicate DSL evaluator

### Iteration 3 -- canonical metric_kind set alignment

- **`CanonicalMetricKinds` ingested-tier fix**:
  `services/clean-code/internal/policy/dsl/sample.go` now lists
  all three v1 ingested metric_kinds -- `coverage_line_ratio`,
  `coverage_branch_ratio`, and `pass_first_try_ratio` --
  matching implementation-plan.md line 31 ("Canonical 3
  ingested metric_kinds") and tech-spec.md lines 302-304. The
  prior set was missing the two coverage ratios, which would
  have caused the parser canon-guard and `Threshold.Validate`
  to reject otherwise-valid coverage-policy DSL predicates and
  Threshold rows.
- Regression tests added in a new `threshold_test.go`:
  - `TestThreshold_Validate_AcceptsAllCanonicalMetricKinds`
    iterates the entire closed set and confirms every member
    survives `Threshold.Validate()`. Acts as the structural
    regression guard against future drift between the canonical
    map and the planning artifacts.
  - `TestThreshold_Validate_RejectsLegacyCoverageAliases` pins
    the counter-invariant: `coverage_line` / `coverage_branch`
    bare names (the implementation-plan's negative-clause names)
    remain rejected.
  - `TestCompile_CoverageRatioThresholdEndToEnd` exercises the
    realistic coverage-policy shape end-to-end via Compile +
    Bind + Eval against a `Sample`.
  - `TestCompile_RejectsLegacyCoverageAlias_AtParseTime` mirrors
    the parser canon-guard rejection through the full Compile
    surface.
- Parser test additions in `parser_test.go`:
  three new well-formed cases (`coverage_line_ratio_compare`,
  `coverage_branch_ratio_compare`, `coverage_ratio_or_chain`)
  and two new malformed cases pinning rejection of the bare
  legacy aliases (`unknown_metric_kind_coverage_line_alias`,
  `unknown_metric_kind_coverage_branch_alias`).

### Iteration 2 -- evaluator feedback resolution

- **Parser**: `parseAtom` now uniformly looks ahead one token after
  parsing an operand: comparison-op → parse a comparison; no
  comparison-op + bool-literal operand → accept as standalone atom;
  otherwise → `ErrParse`. The prior iter consumed standalone
  `true` / `false` BEFORE attempting a comparison, so
  `false == degraded` and `true == false` parsed as trailing junk.
  Standalone bool-typed FIELDS (e.g. `degraded`) remain rejected
  per the documented grammar (`atom ::= ... | bool_literal`, not
  `| bool_operand`).
- **Threshold identity check**: `Bind` now verifies that
  `Threshold.ThresholdID == requested UUID` after `resolver.Lookup`,
  rejecting a stale or mis-keyed resolver before it can bind the
  wrong row. Diagnostic surfaces `mismatched threshold_id <uuid>`
  with line/column.
- **Cache singleflight**: `Cache.GetOrCompile` no longer holds the
  cache-wide write mutex across `Compile` / `resolver.Lookup`. The
  miss path installs a placeholder `cacheEntry{ready chan struct{}}`
  under the mutex, then releases the mutex BEFORE calling `Compile`.
  Concurrent callers for the same key wait on `<-entry.ready`;
  concurrent callers for DIFFERENT keys proceed in parallel.
  Compile panics are recovered, stored on the entry, and re-raised
  by waiters so a buggy DSL doesn't return silent `(nil, nil)`.
- Added regression tests:
  `TestParser_BoolLiteralSymmetric`,
  `TestParser_RejectStandaloneBoolField`,
  `TestEval_BoolLiteralOnLeft`,
  `TestBind_RejectsMismatchedThresholdID`,
  `TestCache_SlowMissDoesNotStallUnrelatedHits`,
  `TestCache_SingleFlightSameKey`. Coverage now 84.8%.

### Added

- New package `internal/policy/dsl/` -- parser + evaluator for
  the per-`Rule.predicate_dsl` predicate language described in
  architecture Sec 5.3.1 / 5.3.2. Surface:

  - `dsl.Parse(src) (Node, error)` -- lex + parse + type-check
    + closed-set canon-guard. Returns a `*dsl.Error` with
    [Position] line/column on any failure (the Stage 5.4
    acceptance criterion line 500 shape).

  - `dsl.Bind(node, src, resolver) (*Predicate, error)` --
    resolves every `threshold('<uuid>')` atom against a
    `ThresholdResolver`. Returns an `ErrBind`-kinded error
    when a UUID is malformed or not in the policy's
    `ThresholdRefs`. Multi-target Unwrap exposes the
    underlying resolver error (e.g. `ErrUnknownThreshold`).

  - `dsl.Compile(src, resolver) (*Predicate, error)` -- the
    canonical Parse + Bind helper.

  - `Predicate.Eval(sample dsl.Sample) (bool, error)` -- pure
    evaluation over a denormalised `Sample` (the MetricSample
    columns plus the joined `ScopeBinding.scope_kind`).
    Determinism is the `dsl-deterministic` Stage 5.4 test
    scenario.

  - `dsl.Cache` -- `sync.RWMutex`-backed memo keyed by
    `(policy_version_id, source string)`. Hot path is one
    RLock + two map lookups + a closed-channel receive. Miss
    path installs a placeholder `cacheEntry{ready}` under
    the cache mutex and then RELEASES the mutex before
    calling `Compile`, so concurrent compiles on different
    keys never block each other and concurrent callers on
    the same key de-duplicate via the `ready` channel
    (singleflight). `Invalidate(policy_version_id)` drops a
    retired policy's entries.

- Grammar (BNF in `doc.go`):

  ```
  predicate      ::= or_expr
  or_expr        ::= and_expr ( "OR" and_expr )*
  and_expr       ::= not_expr ( "AND" not_expr )*
  not_expr       ::= "NOT" not_expr | atom
  atom           ::= "(" predicate ")" | threshold_call | comparison | bool_literal
  threshold_call ::= "threshold" "(" string_literal ")"
  comparison     ::= operand cmp_op operand
  cmp_op         ::= "==" | "!=" | ">" | ">=" | "<" | "<="
  operand        ::= field | string_literal | number_literal | bool_literal
  field          ::= "metric_kind" | "scope_kind" | "value"
                  |  "pack" | "source" | "degraded"
  ```

  Precedence (tightest to loosest): atoms < `NOT` < `AND` <
  `OR`. Parens override.

- Closed-set canon-guards on string literals -- the
  `dsl-rejects-unknown-metric-kind` test scenario:
  - `metric_kind` -- architecture Sec 1.4 catalogue.
  - `scope_kind` -- migration 0002 `clean_code.scope_kind` enum.
  - `pack` -- migration 0002 `clean_code.metric_sample_pack` enum.
  - `source` -- migration 0002 `clean_code.metric_sample_source` enum.

  A predicate like `metric_kind == 'lines_of_code'` is rejected
  at `Parse` time with `ErrSemantic` and a `Position` pointing
  at the offending literal.

### Notes for downstream stages

- Stage 5.5 / 5.6 rule-pack YAMLs MUST cite metric_kinds from
  the canonical sets in `sample.go`. Adding a new metric_kind
  requires (1) an entry in `CanonicalMetricKinds`, (2) a
  Catalog row in `clean_code.metric_kind` (migration 0001),
  and (3) a Compute Engine recipe.
- Stage 5.7 SOLID Rule Engine consumes `dsl.Cache.GetOrCompile`
  per `(active policy_version_id, rule.predicate_dsl)` and
  iterates `Predicate.Eval` over the SHA's MetricSample rows.

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
