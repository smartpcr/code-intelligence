# Changelog: `services/clean-code`

All notable changes to the clean-code service are recorded here.
Newest at the top. Stage references map to
`docs/stories/code-intelligence-CLEAN-CODE/implementation-plan.md`.

## Stage 5.6 -- Decoupled functional areas rule pack

### Added

- **`policy/rulepacks/decoupling/cycles.yaml`** -- canonical
  `pack_id='decoupling.cycles'` rule_pack file declaring one
  rule (`decoupling.cycle_member`, severity `block`) that
  fires when a scope's `cycle_member` sample is `> 0`. Matches
  the implementation-plan Stage 5.6 line 533 brief "block when
  any module in a watched scope is in a cycle". The
  `decoupling-loads` and `cycles-rule-fires-on-cycle-member`
  test scenarios are pinned by
  `policy/rulepacks/decoupling/{decoupling_test.go,bootstrap_test.go}`.
- **`policy/rulepacks/decoupling/coupling.yaml`** --
  `pack_id='decoupling.coupling'` rulepack with three rules
  (`decoupling.fan_in_high`, `decoupling.fan_out_high`,
  `decoupling.cbo_high`) covering exactly the three-metric
  closed set `{fan_in, fan_out, coupling_between_objects}`
  per the Stage 5.6 brief. Each predicate is a single
  `threshold('<uuid>')` atom that resolves -- via the
  canonical [`decoupling.Resolver`] -- to a
  [`steward.Threshold`] row seeded by
  [`decoupling.SeedThresholds`] (v1 defaults: 20 / 20 / 12).
  Operators re-tune by inserting a new Threshold row and
  republishing this rulepack at `version=2` against the new
  UUID -- the implementation-plan Stage 5.6 line 534 contract
  "with thresholds from `Threshold` rows".
- **`policy/rulepacks/decoupling/duplication.yaml`** --
  `pack_id='decoupling.duplication'` rulepack with one rule
  (`decoupling.duplication_ratio_high`) whose predicate is a
  single `threshold('<uuid>')` atom that resolves to a
  `duplication_ratio` Threshold row (file scope, op=gt,
  value=0.20).
- **`policy/rulepacks/decoupling/thresholds.go`** -- canonical
  source of truth for the four decoupling threshold UUIDs.
  Each ID is a v5 UUID derived from a single
  [`decoupling.Namespace`] seed (mirrors the
  `internal/ast/scope/identity.go` pattern). Exports
  [`SeedThresholds(ctx, store)`] (inserts the four rows into
  `clean_code.threshold` via [`steward.Store.InsertThreshold`],
  stamping each row's `CreatedAt` with a single `time.Now().UTC()`
  capture so the four rows share one atomic seeding instant in
  the audit log)
  and [`Resolver()`] (returns a [`dsl.MapResolver`] for the
  Rule Engine and the canon-guard tests).
- **`policy/rulepacks/decoupling/bootstrap.go`** --
  [`decoupling.Bootstrap(ctx, *steward.Steward, steward.Store)`]
  -- the canonical startup hook the composition root calls
  (see `cmd/clean-coded/main.go::run()` where the call
  lives, gated on `signer != nil` so scaffold-mode boots
  are skipped). Seeds the four Threshold rows + invokes
  `steward.PublishRulepack` for each loaded YAML, treating
  [`steward.ErrDuplicateRulePack`] / [`ErrDuplicateRule`] as
  the idempotent "already bootstrapped" outcome.
- **`cmd/clean-coded/main.go`** -- the production composition
  root now invokes `decoupling.Bootstrap` after building the
  Policy Steward, gated on `signer != nil` (scaffold-mode
  boots without a wired signing key are skipped because
  `policy.publish_rulepack`'s precondition would refuse them).
  Bootstrap result counters (inserted thresholds / published
  packs / published rules) are emitted as structured-log
  fields on the `decoupling rulepacks bootstrapped` startup
  line. The helper `buildPolicyWriter` is extended to return
  the inner `*steward.Steward` and `steward.Store` alongside
  the existing `*management.PolicyWriter` so the Bootstrap
  call attaches to the SAME steward instance the HTTP
  surface serves.
- **`policy/rulepacks/decoupling/loader.go`** -- embed.FS-backed
  [`LoadAll()`] that strict-decodes the three YAMLs via
  `yaml.v3 KnownFields(true)` and validates the per-file and
  cross-file invariants (family prefix, PK uniqueness, etc.).
- **`policy/rulepacks/decoupling/{decoupling,bootstrap}_test.go`**
  + `walk.go` + `cmd/clean-coded/bootstrap_test.go` -- 27 tests pinning:
  - **canonical UUIDs match the YAML literals**
    (`TestCanonicalThresholdIDs_MatchYAML`,
    `TestNamespace_Pinned`),
  - all three files declare `pack_id='decoupling.<subname>'`,
  - every `predicate_dsl` parses cleanly via [`dsl.Parse`],
  - every rule's metric_kind reference (whether literal OR
    threshold-bound via the canonical [`Resolver`]) is in
    [`dsl.CanonicalMetricKinds`] -- this is the
    implementation-plan brief's "test asserting each predicate
    references only canonical metric_kinds",
  - every `severity_default` is in `{info, warn, block}`,
  - no `(rule_id, version)` or `(pack_id, version)` pair is
    duplicated across files,
  - `cycles.yaml` rule fires on a
    `(metric_kind='cycle_member', value=1)` sample and does
    NOT fire on `value=0`,
  - `coupling.yaml` covers exactly the
    `{fan_in, fan_out, coupling_between_objects}` set
    (sourced via resolver-bound metric_kind),
  - `duplication.yaml` references only `duplication_ratio`,
  - **end-to-end bootstrap**
    (`TestBootstrap_PublishesThreePacksAndFiveRules`)
    constructs a real wired [`steward.Steward`] (in-memory KMS
    + Store + minted signing key), invokes
    [`decoupling.Bootstrap`], and asserts (a) all 4 Threshold
    rows present, (b) all 3 RulePack rows present via
    `store.GetRulePack`, (c) all 5 Rule rows present via
    `store.ListRulesForPack`, (d) every persisted predicate
    parses cleanly. This realises the `decoupling-loads` e2e
    scenario's "`pack='decoupling'` rule_packs exist with
    parsed predicates" clause end-to-end.
  - **composition-root wiring**
    (`cmd/clean-coded/bootstrap_test.go::TestBuildPolicyWriter_WiresStewardAndStoreForBootstrap`)
    calls the production `buildPolicyWriter` helper, then
    `decoupling.Bootstrap` on the returned (steward, store)
    tuple -- exactly the call shape `run()` performs.
    Proves the wiring landing point: a follow-up edit that
    refactors `buildPolicyWriter`'s signature so Bootstrap
    can no longer find the inner Steward fails this test.
  - **CreatedAt stamping**
    (`TestSeedThresholds_StampsCreatedAt`) -- each persisted
    Threshold row carries a non-zero, UTC, recent timestamp.
    Guards against the silent-bug failure mode where
    `InsertThreshold` (which writes `t.CreatedAt.UTC()`
    verbatim) persists `0001-01-01` on every row.
  - **idempotency**
    (`TestBootstrap_IsIdempotent`, `TestSeedThresholds_Idempotent`),
  - **predicate semantics** end-to-end: the coupling
    `fan_in_high` rule fires for value=21 (above 20) and does
    NOT fire at the boundary value=20
    (`TestBootstrap_CouplingRuleFiresOnFanInAbove20`);
    duplication fires for ratio=0.25 and does NOT fire at
    0.20 (`TestBootstrap_DuplicationRuleFiresOnRatioAbove20pct`),
  - signing-key precondition is honoured
    (`TestBootstrap_RefusesWithoutSigningKey`).

### Notes

- The four canonical decoupling threshold UUIDs are
  v5-derived from the namespace seed
  `"clean-code/policy/rulepacks/decoupling/v1"`. Bumping the
  trailing `/v1` to `/v2` is how operators rotate the entire
  canonical threshold set without colliding with the v1 IDs.
- Architecture Sec 1.4.1 pins `fan_in` and `fan_out` to the
  canonical scope set `{method, class, file}`; the v1
  decoupling rulepack ships one Threshold row per metric_kind
  at `scope_kind=class` (the SOLID-family join point).
  Operators extend coverage to method or file scope by
  seeding additional Threshold rows and republishing the
  coupling rulepack at `version=2`. The class-scope-only v1
  shape is documented inline in `coupling.yaml`.
- Threshold rows are seeded via the
  `steward.Store.InsertThreshold` primitive (migration tooling
  or per-deploy operator scripts). There is no `policy.*`
  canonical write verb that mutates the Threshold catalogue
  in v1; the `coupling.yaml` header documents this contract
  inline rather than naming a hypothetical admin verb.

## Stage 2.2 -- iter 4 follow-ups (evaluator feedback resolution)

### Fixed

- **`internal/ast/scope/identity.go`: doc comment on `var
  Namespace` corrected.** Iter-3's comment said the namespace
  was derived from `[uuid.NamespaceDNS]` while the code
  correctly used `uuid.NamespaceURL`; evaluator iter-3 #1
  flagged the mismatch because a future schema-bump reviewer
  could trust the wrong word. Replaced with the accurate
  `[uuid.NamespaceURL]` description plus an explicit
  `(Iter 3's doc comment incorrectly named ...)` paragraph so
  the prior-iter wrong claim is captured in the file's own
  history. No code, namespace UUID, or test changed -- the
  `TestNamespace_Pinned` literal still asserts
  `5fa5937c-c012-5190-b7bd-0bd48f41de65` and still passes.
- Grep-verified no `"DNS namespace"` prose remains in
  `services/clean-code`; the only `NamespaceDNS` occurrences
  are now (a) the new corrective paragraph in `identity.go`
  and (b) the two `identity_test.go` doc lines that
  intentionally call out `[uuid.NamespaceURL] vs
  [uuid.NamespaceDNS]` as the kind of wrong-source edit the
  golden test catches.

## Stage 2.2 -- iter 3 follow-ups (evaluator feedback resolution)

### Changed

- **`storage.ScopeBindingWriter.insertFreshOn` now writes
  `created_at` as an EXPLICIT column** (filled by the inline
  `NOW()` SQL literal, not the DB DEFAULT). The brief lists
  `created_at` as a writer-owned column; iter-1 / iter-2
  silently relied on the table DEFAULT, leaving the column
  value undocumented at the writer's call site. The change is:
  - `scopeBindingInsertColumns` now lists 8 columns (was 7),
    explicitly ending in `created_at`.
  - `scopeBindingColumnCount` (the bound-PARAMETER count per
    row) stays at 7; `NOW()` is a server-side SQL literal that
    consumes no `$N` slot.
  - `verifyRow` test helper SELECTs `created_at` and asserts
    it is populated AND within a narrow wall-clock window
    (catches column-shift bugs that would put e.g. the epoch
    in this slot).
  - New `TestScopeBindingWriter_CreatedAtPopulated` live PG
    test pins the explicit emit + the G3 immutability
    contract (a second observation does NOT mutate
    `created_at`).
  - The decision to use inline `NOW()` (rather than a
    Go-side `time.Now()` parameter) is documented on
    `scopeBindingInsertColumns`: the server's wall clock is
    authoritative, saves one `$N` slot per row (matters for
    the bound-parameter chunk-size budget), and keeps the
    value observable in the INSERT's SQL text rather than
    deferred to a DEFAULT clause an evaluator must
    cross-reference. (Addresses evaluator iter-2 #1.)

- **`internal/ast/scope/identity_test.go::TestNamespace_Pinned`
  now compares against a LITERAL UUID string** (the new
  `pinnedNamespaceUUID = "5fa5937c-c012-5190-b7bd-0bd48f41de65"`),
  not a value recomputed from `scope.NamespaceURL` at test
  time. Iter-2's `want := uuid.NewV5(uuid.NamespaceURL,
  scope.NamespaceURL).String()` was tautological -- editing
  `NamespaceURL` would update BOTH `scope.Namespace` and
  `want` simultaneously and the assertion would still pass
  even though every existing `scope_id` had silently drifted.
  The literal pin makes namespace drift fail loudly. A
  belt-and-braces re-derivation assertion catches the case
  where the literal and the in-source inputs diverge (a
  schema bump that needs operator review). (Addresses
  evaluator iter-2 #2.)

- **`storage.ScopeBindingWriter` lookup + insert paths now
  CHUNK over PostgreSQL's 65535-parameter ceiling.** Iter-2
  built one SQL statement per `Write()` call regardless of
  batch size; the writer's own doc comments referenced
  "single-repo scans of 10k scopes" as the worst-case
  contention case, which at 7 params/row would have
  overshot the ceiling at 9362 rows (and at 3 params/lookup
  tuple, would have overshot at 21,845 keys). The change is:
  - New `scopeBindingLookupChunkSize = 16384` (3 params/tuple
    -> 49,152 params/statement) and
    `scopeBindingInsertChunkSize = 8192` (7 params/row ->
    57,344 params/statement). Both sit below the
    65535-parameter ceiling with headroom for a future
    column addition.
  - `lookupExistingOn` now splits `keys` into chunks of
    `scopeBindingLookupChunkSize` and merges results into a
    single map; the caller does not see chunk boundaries.
    The single-chunk helper is extracted as
    `lookupExistingChunk` so the chunk loop is the only
    place that owns the chunk-size policy.
  - `insertFreshOn` now splits `rows` into chunks of
    `scopeBindingInsertChunkSize` and runs INSERTs serially
    on the supplied querier (so all statements share the
    same session and the advisory lock the caller holds in
    the locked-INSERT path). Sum of RETURNING counts across
    chunks is returned. Single-chunk helper extracted as
    `insertFreshChunk`.
  - `insertFreshChunk` has a pre-flight
    `len(rows) * scopeBindingColumnCount > pgMaxBindParameters`
    guard so a future chunk-size raise that overshoots the
    ceiling surfaces a precise pre-flight error rather than
    a confusing driver-emitted "got N parameters, expected
    at most 65535".
  - Chunk-size vars are package-level `var` (not `const`) so
    live tests can drop them to small values and exercise
    multi-chunk fan-out without staging tens of thousands
    of rows per test.
  - New `TestScopeBindingWriter_ChunkingBoundary` live PG
    test temporarily drops insert chunk size to 37 and
    lookup chunk size to 29 (both PRIME so chunk boundaries
    don't accidentally align), writes 300 distinct
    candidates, and asserts: (a) every candidate's scope_id
    matches `scope.DeriveScopeID` (no chunk-boundary
    drift), (b) exactly 300 rows land, (c) a second Write
    of the same candidates resolves entirely from the
    multi-chunk LOOKUP path with zero new INSERTs.
  - New `TestScopeBindingWriter_ChunkBoundaryParamCeilingGuard`
    unit test (no live PG) hands `insertFreshChunk` 9363
    rows directly and asserts the in-helper guard surfaces
    the precise "exceeds PostgreSQL bound-parameter
    ceiling" message. (Addresses evaluator iter-2 #3.)

## Stage 2.2 -- iter 2 follow-ups (evaluator feedback resolution)

### Changed

- **`scope.BuildInterface` discriminator** -- emits `::class::`
  (NOT `::interface::`) so the canonical signature is
  BYTE-IDENTICAL to agent-memory's `classSignature` for the
  same `(relPath, qualifiedName)`. agent-memory's
  `services/agent-memory/internal/repoindexer/ast/dispatcher.go`
  uses `classSignature` for "a Class / Interface node" without
  distinguishing them at the signature layer; linked-mode
  `agent_memory_node_id` resolution depends on this parity.
  Class and interface are still independently distinguished by
  the `scope_kind` discriminator, which is part of the
  `scope_id` UUIDv5 pre-image -- so a class and an interface
  with the same qualifiedName get the SAME `canonical_signature`
  string but DIFFERENT `scope_id`s. (Reverses iter-1's
  "self-consistent `::interface::`" decision; addresses
  evaluator iter-1 #1.)

- **`scope.BuildBlock` ordinal validation** -- the guard is now
  `ordinal < 0` (was `ordinal <= 0`). Block ordinals are
  0-based per agent-memory's `Block.Ordinal` doc ("0-based
  position of this Block within its enclosing Method's Block
  list") and `blockSignature` emits `#block_0_<kind>` for the
  first block. Rejecting `0` would have broken parity for the
  first emitted block of every method. (Addresses evaluator
  iter-1 #2.)

- **`storage.ScopeBindingWriter.Write` -- intra-batch dedupe
  (G2 #3 fix)** -- candidates sharing
  `(repo_id, scope_kind, canonical_signature)` are grouped
  BEFORE deriving any `scope_id`. The FIRST occurrence in
  input order wins: its CurrentSHA becomes the group's
  `first_seen_sha`, its derived `scope_id` is broadcast to
  every sibling slot, and only ONE row is INSERTed. Without
  this fix two candidates with the same natural key but
  different CurrentSHAs would derive DIFFERENT `scope_id`s
  (first_seen_sha is part of the UUIDv5 pre-image) and both
  land via the `(repo_id, scope_kind, canonical_signature,
  first_seen_sha)` UNIQUE -- two rows for one logical scope.
  Pinned by `TestScopeBindingWriter_BatchSameKeyDifferentSHAs`
  (live PG). Sibling SHA divergences increment
  `SHADivergences` for producer observability. (Addresses
  evaluator iter-1 #3.)

- **`storage.ScopeBindingWriter.Write` -- concurrent-writer
  race (G2 #4 fix)** -- the fresh-INSERT path now runs inside
  a transaction that holds a transaction-scoped
  `pg_advisory_xact_lock(int4, int4)` per unique `repo_id` in
  the batch (namespaced under int32 `0x434C4353` ("CLCS") so
  the writer's lock space is isolated from any other component
  sharing the PostgreSQL instance). The natural-key SELECT is
  RE-RUN inside the lock so a racer that committed between the
  unlocked fast-path SELECT and the lock acquisition is
  observed and reused, NOT re-INSERTed. Lock keys are sorted
  before acquisition (single `unnest`-driven SELECT round-trip)
  so two writers with overlapping repo sets cannot deadlock.
  Per-repo (NOT per-natural-key) granularity is exhaustion-
  proof against `max_locks_per_transaction` at large batch
  sizes -- a single-repo scan of 10k scopes acquires ONE lock.
  Steady-state warm-read fast path: when the unlocked initial
  SELECT finds every key, the writer returns WITHOUT opening a
  transaction. Pinned by
  `TestScopeBindingWriter_ConcurrentRaceDifferentSHAs` with
  8 concurrent goroutines on a shared `*sql.DB` (live PG).
  (Addresses evaluator iter-1 #4.)

- **Helper refactor -- `lookupExistingOn` / `insertFreshOn`
  take a `querier` interface** -- both helpers now accept
  either `*sql.DB` (unlocked fast path) or `*sql.Tx` (locked
  transaction body). The `*sql.Tx` is the load-bearing
  argument for the race fix: a `*sql.DB`-based call inside a
  locked transaction would borrow a different pooled
  connection and the advisory lock (backend-local per session)
  would be invisible to it, silently bypassing the fix.

### Removed

- `storage.ErrConflictingFirstSeenSHA` -- declared but never
  returned. Producer-side SHA divergence is exposed via the
  `ScopeBindingWriteResult.SHADivergences` counter; the
  unreached error symbol was misleading. The natural-key
  UNIQUE 23505 path now surfaces with a more accurate message
  ("a bypass-the-writer write path landed first") because
  with the advisory lock in place the only way to reach it is
  for a producer outside this writer to INSERT.

### Deferred

- **Production wiring (evaluator iter-1 #5).** Implementation
  plan line 183 calls for the writer to be wired behind the
  Metric Ingestor. The Metric Ingestor itself is built in
  Stage 3.2 (implementation-plan.md line 284 -- "Metric
  Ingestor and ScanRun state machine"); the `internal/metric_ingestor/`
  package does not exist in this stage's scope. Stage 3.2 will
  call `storage.NewScopeBindingWriter` from the per-scan
  ingest path. No production caller can be added within Stage
  2.2 without speculatively scaffolding the Metric Ingestor
  out of stage order.

## Stage 2.2 -- Scope identity derivation and ScopeBinding writer

### Added

- **`internal/ast/scope/` package** -- owns the deterministic
  identity and canonical-signature derivation for every
  `scope_binding` row the service writes (architecture Sec
  5.2.3 lines 1039-1050):
  - `Kind` typed string + the closed seven-value enum
    (`repo|package|file|class|interface|method|block`) with
    `IsValid()` predicate matching the `clean_code.scope_kind`
    PostgreSQL ENUM byte-for-byte (so a `Kind` value rides as
    a `text` parameter cast to the enum server-side).
  - `NormalizeSignature(s)` -- mirrors
    `services/agent-memory/internal/repoindexer/ast/whitespace.go`
    byte-for-byte (strip line+block comments, collapse Unicode
    whitespace runs to a single ASCII space, strip space
    adjacent to `,()[]{}<>:;`, trim) so a formatter-only commit
    produces a byte-identical signature -- the architecture
    §9.7 / §9.9 stability mitigation.
  - Per-kind builders `BuildRepo`, `BuildPackage`, `BuildFile`,
    `BuildClass`, `BuildInterface`, `BuildMethod`, `BuildBlock`
    -- emit the canonical-signature strings using the same
    recipe agent-memory uses for its `Node.canonical_signature`
    so the cross-service `agent_memory_node_id` link is stable
    when clean-code runs in `linked` mode. Paths (`dir`,
    `relPath`) are NOT normalised; only `qualifiedName` and
    joined `params` ride through the normaliser.
  - `DeriveScopeID(repoID, kind, canonicalSignature, firstSeenSHA)`
    -- deterministic UUIDv5 over `(repoID, kind, signature,
    firstSeenSHA)` with NUL framing between fields, derived
    under a pinned package-level `Namespace` UUID (itself a
    UUIDv5 of `NamespaceURL` constant
    `https://github.com/microsoft/code-intelligence/clean-code/scope#v1`).
    SHA is NOT part of identity (G2): callers reuse the
    persisted `first_seen_sha` across SHAs so the same
    logical scope keeps the same `scope_id`. The
    `TestNamespace_Pinned` golden test fails loudly if the
    namespace ever drifts.
  - Sentinel errors `ErrZeroRepoID`, `ErrInvalidKind`,
    `ErrEmptyField`, `ErrEmbeddedNUL` for the validation
    surface; NUL rejection is mandatory because NUL is the
    framing delimiter in the DeriveScopeID pre-image.

- **`internal/storage/scope_binding_writer.go`** --
  `ScopeBindingWriter` performing batched, idempotent writes
  into `<schema>.scope_binding`:
  - `NewScopeBindingWriter(db)` / `NewScopeBindingWriterWithSchema(db, schema)`
    constructor pair (matches the steward / keys SQLStore
    convention; production reaches the former on the canonical
    `clean_code` schema, tests reach the latter on the
    isolated `clean_code_scope_test` schema).
  - `Write(ctx, []ScopeBindingCandidate) -> ScopeBindingWriteResult`:
    (1) validate every candidate (kind / signature / SHA /
    NUL-byte / valid JSON guards) up-front so a bad input
    cannot half-land; (2) SELECT existing rows by natural
    key `(repo_id, scope_kind, canonical_signature)` so any
    pre-existing `first_seen_sha` is reused (the LOAD-BEARING
    G2 enforcement -- a buggy caller passing the current SHA
    in place of the cached first_seen_sha does NOT mint a
    second row); (3) derive `scope_id` via
    `scope.DeriveScopeID` for every fresh candidate using
    its `CurrentSHA` as first_seen_sha; (4) batched
    `INSERT ... VALUES ... ON CONFLICT (scope_id) DO NOTHING
    RETURNING scope_id` for the fresh set, with the
    `scope_kind` placeholder cast to the schema-qualified
    `<schema>.scope_kind` enum so the test schema and the
    production schema both work.
  - `ScopeBindingWriteResult` reports `Rows` (parallel to
    input), `Inserted` (RETURNING count -- excludes
    concurrent-writer races), `ReusedExisting` (natural-key
    lookups that hit), and `SHADivergences` (informational
    count of candidates whose `CurrentSHA` differed from the
    persisted `first_seen_sha`; the writer always reuses the
    persisted value).
  - `pgSQLStateUniqueViolation = "23505"` mapped to a wrapped
    error annotating the violated constraint so a concurrent
    writer race (which can only happen when two pipelines
    pass DIFFERENT `CurrentSHA`s for a brand-new tuple) is
    distinguishable from a real bug.

### Invariants pinned by tests

- **G2 stability across SHAs.** A natural-key tuple first
  observed at SHA A and observed again at SHA B resolves to
  the SAME `scope_id` AND the persisted `first_seen_sha`
  remains A. Pinned by `TestScopeBindingWriter_G2StableAcrossSHAs`
  (live PG, skipped if `CLEAN_CODE_PG_URL` unset).
- **Namespace UUID is locked.** Changing the
  `NamespaceURL` constant (or the source namespace) would
  silently drift every existing `scope_id`; the golden test
  `TestNamespace_Pinned` re-derives the namespace from the
  pinned URL and fails loudly on any mismatch.
- **Closed-set scope_kind enum.** Adding a `scope_kind` value
  requires also adding it to the PostgreSQL ENUM AND to the
  architecture doc; the in-process `Kind.IsValid()` predicate
  AND `TestKind_IsValid_ClosedSet` keep the three in lockstep.
- **All seven kinds produce distinct scope_ids.** The same
  `(repo_id, signature, first_seen_sha)` fed into every Kind
  yields seven distinct UUIDs -- pinned by
  `TestDeriveScopeID_AllKindsDistinct`.
- **NUL bytes are reserved framing.** Every signature builder
  AND `DeriveScopeID` itself rejects strings containing the
  NUL byte with `ErrEmbeddedNUL`. Pinned by
  `TestBuilders_RejectNUL` and `TestDeriveScopeID_Validation`.
- **Idempotency.** Calling `Write` twice with the same batch
  yields the same `Rows` on both calls and `Inserted=0` on the
  second. Pinned by `TestScopeBindingWriter_Idempotent` (live PG).
- **Duplicate batch entries collapse.** A batch containing the
  same natural key twice produces ONE INSERT (both result rows
  carry the same `scope_id`). Pinned by
  `TestScopeBindingWriter_BatchWithDuplicates` (live PG).
- **agent-memory canonical-signature parity.**
  `TestNormalizeSignature_AgentMemoryParity` pins every example
  from agent-memory's `whitespace.go` doc comment so a drift
  surfaces immediately.

### Changed

- `go.mod`: module path corrected from `forge/services/clean-code`
  back to `github.com/microsoft/code-intelligence/services/clean-code`.
  Every existing internal package (`internal/policy/keys`,
  `internal/policy/steward`, `internal/management`,
  `internal/evaluator`, `cmd/clean-coded`, etc.) imports from
  the `github.com/microsoft/...` path; the prior `forge/...`
  rename in commit `30394c7` broke `go build` and every test
  ran against a stale-cache binary. Fixing this was required
  to land the Stage 2.2 changes (the new `internal/ast/scope`
  package imports `gofrs/uuid` and is consumed by
  `internal/storage`); without the fix nothing in the service
  compiled.

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
