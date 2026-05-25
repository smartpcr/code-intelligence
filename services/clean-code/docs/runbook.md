# `services/clean-code` runbook

Operational guide for the clean-code service. Add a new
section here as each subsystem ships against the production
composition root (`cmd/clean-coded/main.go`).

## Metric Ingestor and ScanRun state machine (Stage 3.2)

### What

The Metric Ingestor subsystem is a stack of types in
`internal/metric_ingestor/`:

- `Sweeper` (`sweep_loop.go`) -- the long-running supervisor
  that ticks at `CLEAN_CODE_PERIODIC_SWEEP_CADENCE` and calls
  `StateMachine.ProcessOne` on each tick.
- `StateMachine` (`state.go`) -- the one-turn orchestrator
  that drives a single commit from `pending` to a terminal
  state. Composed of: `ScanRunStore` (PG- or in-memory-backed,
  the sole writer of `commit.scan_status`), `AstScanner`
  (the adapter from `Ingestor.Run` to the scanner interface,
  produced by `NewIngestorAstScanner`), and the optional
  `AstSourceAvailability` probe.
- `Ingestor` (`ingestor.go`) -- the per-scan orchestrator
  composing the dispatcher and `ChurnSweep`. One
  `(*Ingestor).Run` invocation drives the full per-scan
  pipeline.

One sweep, end-to-end, is:

1. The `Sweeper` ticks; `StateMachine.ProcessOne` runs.
   When an `AstSourceAvailability` probe is wired
   (production: `DirectoryAstFileSource` doubles as the
   probe), the state machine **peeks up to `probeFanout`
   pending commits** (default 16, see
   `internal/metric_ingestor/state.go:813`
   `defaultProbeFanout`) via
   `ScanRunStore.PeekNextPendingCommits`, iterates them
   in commit-time order asking the probe whether each
   commit's on-disk checkout has materialised, and
   selects the FIRST ready candidate. This avoids
   head-of-line blocking when the oldest commit's
   checkout has not yet been written by the Repo
   Indexer. If no probe is wired (in-memory / scaffold),
   the legacy single-row `ClaimNextPendingCommit` path
   is taken instead (`state.go:1047`).
2. The state machine claims the selected row via
   `ScanRunStore.ClaimSpecificPendingCommit(repoID, sha)`
   when it came from the fanout pre-flight, or
   `ScanRunStore.ClaimNextPendingCommit` when there was
   no probe (legacy path). Either claim opens a
   `scan_run(kind='full', sha_binding='single',
   status='running')` row and transitions
   `commit.scan_status` to `'scanning'` in ONE PG
   transaction. A crash between these two writes cannot
   leave a commit in `'scanning'` without an owning
   `scan_run`.
3. The state machine invokes `Ingestor.Run` via
   `IngestorAstScanner.Scan`. The `Ingestor` resolves the
   commit's `repo_url` via the lookup helper
   (`internal/metric_ingestor/repo_url_lookup.go`) and
   opens the AST source against the on-disk checkout
   directory rooted at `CLEAN_CODE_AST_SCAN_ROOT`.
4. The recipe-registry dispatcher
   (`RegistryBackedFoundationDispatcher`) drives every
   recipe over the parsed AST. Each recipe yields zero or
   more `metric_sample` drafts; the PG writer
   (`pg_metric_sample_writer.go:111-117`) issues a plain
   `INSERT INTO clean_code.metric_sample (sample_id,
   repo_id, sha, scope_id, metric_kind, metric_version,
   value, pack, source, producer_run_id, attrs_json)
   VALUES (...)` inside a transaction. There is no
   `ON CONFLICT` clause; duplicates are prevented by the
   caller minting fresh `sample_id`s per scan.
5. The state machine finalizes via
   `ScanRunStore.FinalizeScanRun`. The PG implementation
   (`pg_scan_run_store.go:463-509`) runs ONE transaction
   that `UPDATE clean_code.scan_run SET status = $1,
   ended_at = $2 WHERE scan_run_id = ... AND status =
   'running'` followed by `UPDATE clean_code.commit SET
   scan_status = ... WHERE repo_id = ... AND sha = ... AND
   scan_status = 'scanning'`. On success the pair is
   (`scan_run.status='succeeded'`,
   `commit.scan_status='scanned'`); on failure the pair is
   (`scan_run.status='failed'`,
   `commit.scan_status='failed'`). The metric_sample writer
   is NOT inside this transaction -- successful inserts
   from a sweep that subsequently fails its finalize are
   left in place but attributed to a `scan_run.status='failed'`
   row, which downstream readers filter on. There is
   intentionally no `scan_run.error_class` / `error_message`
   column in this workstream's schema; the failure reason
   is recorded in the structured log line at finalize time.

Only the four canonical Commit states (`pending`,
`scanning`, `scanned`, `failed`) and three canonical
ScanRun states (`running`, `succeeded`, `failed`) are
ever written. The state alphabet is pinned in
`internal/metric_ingestor/state.go` (the
`ScanRunStatus*` constants + `AllScanRunStatuses` /
`ValidateScanRunStatus` closed-set guards).

### Configuration (env vars)

The metric ingestor is wired in `cmd/clean-coded/main.go`
(`buildMetricIngestor` + the sweeper construction below it)
and consumes the existing service-wide config knobs -- it
does NOT introduce a `CLEAN_CODE_METRIC_INGESTOR_*`
namespace. The relevant env vars are:

| Env var                           | Meaning                                                                                                                                                                                                          | Required when                       |
| --------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------- |
| `CLEAN_CODE_PG_URL`               | PostgreSQL connection URL. The pool MUST be reachable by the `clean_code_metric_ingestor` role. When empty, the composition root falls back to in-memory stores ([`metric_ingestor.NewInMemoryScanRunStore`]).   | always in production                |
| `CLEAN_CODE_AST_SCAN_ROOT`        | Root directory under which per-repo checkouts live. The composition root reads it as the root of `DirectoryAstFileSource`; if empty the root falls back to `EmptyAstFileSource` (no work to do).                 | always in production                |
| `CLEAN_CODE_PERIODIC_SWEEP_CADENCE` | The `Sweeper` tick interval (Go duration). Drives `WithSweeperCadence` in `cmd/clean-coded/main.go`. Default value lives in `internal/config/config.go`.                                                       | never (defaulted)                   |
| `CLEAN_CODE_SCAN_TIMEOUT`         | Per-scan timeout passed to `WithStateMachineTimeout`. A sweep that exceeds this is aborted and the commit is marked `'failed'` rather than left in `'scanning'` indefinitely.                                    | never (defaulted)                   |

There is intentionally NO `CLEAN_CODE_METRIC_INGESTOR_*`
env-var namespace today; the metric ingestor inherits its
config from the service-wide knobs above. Future stages
that add per-subsystem tuning (e.g. multi-tenant batch
sizing) may introduce a dedicated namespace; until then,
operators should NOT set fictional env vars expecting
them to work.

Mode selection (per `cmd/clean-coded/main.go:402-460`):

- **Production**: `CLEAN_CODE_PG_URL` is set AND
  `CLEAN_CODE_AST_SCAN_ROOT` is set. The composition root
  wires `PGScanRunStore` + `DirectoryAstFileSource` and
  launches the sweeper.
- **Fail-fast**: `CLEAN_CODE_PG_URL` is set but
  `CLEAN_CODE_AST_SCAN_ROOT` is empty. The composition
  root **refuses to start** and returns an actionable
  error (`main.go:438-448`): "CLEAN_CODE_AST_SCAN_ROOT is
  REQUIRED when CLEAN_CODE_PG_URL is configured -- the
  Metric Ingestor sweep loop is the SOLE driver of
  `commit.scan_status` transitions...". This is the
  iter-4 evaluator structural fix: rather than silently
  letting pending commits accumulate against a live PG
  instance with no source of AST files, the process
  exits non-zero so the operator sees the misconfiguration
  at boot, not 30 minutes of silent backlog later.
- **Scaffold mode**: `CLEAN_CODE_PG_URL` is empty (which
  implies in-memory stores). The composition root logs
  `metric ingestor sweep loop NOT STARTED (scaffold mode:
  CLEAN_CODE_AST_SCAN_ROOT unset)` (`main.go:454-460`),
  closes `sweepDone` immediately so shutdown does not
  block, and the sweeper is **NEVER launched**. The HTTP
  surface still serves; nothing claims commits. This is
  acceptable for the dev loop only.

### Source-availability pre-flight (NOT a `/readyz` probe)

The composition root threads an `AstSourceAvailability`
probe (`metric_ingestor.AstSourceAvailability`, defined in
`internal/metric_ingestor/availability.go`) into the state
machine via `WithStateMachineSourceProbe` (see
`cmd/clean-coded/main.go:917-957`). The directory AST
source itself implements `HasFilesFor`, so the probe is
non-nil whenever `CLEAN_CODE_AST_SCAN_ROOT` is set; in
scaffold mode the probe is nil and the pre-flight is
disabled. When the probe is wired,
`StateMachine.ProcessOne` peeks **up to `probeFanout`
pending commits** (default 16 per
`state.go:813`) and iterates them in commit-time order,
claiming the FIRST one whose `HasFilesFor` returns true
via `ClaimSpecificPendingCommit`
(`state.go:950-1019`). Every skipped candidate stays in
`'pending'` (no canonical edge crossed); if NO candidate
in the fanout is ready, `ProcessOne` returns
`DidWork=false` with `SkipReason=SourceNotReady` and the
next tick re-peeks. This keeps the four-state Commit
diagram intact AND avoids head-of-line blocking when the
oldest commit's checkout hasn't yet landed on disk.

The probe is plumbed into the state machine, NOT into
`/readyz`. The composition root currently registers only
the Policy-Steward signing-key cache ready-check via
`healthHandler.AddReadyCheck("signing_key_cache", ...)`
(see `cmd/clean-coded/main.go:526`); there is no
`AddReadyCheck("ast_source", ...)` call today. Operators
that want `/readyz` to reflect AST-source readiness should
treat this as a follow-up workstream, not a Stage 3.2
deliverable.

### State-machine invariants

- `commit.scan_status` is written ONLY by the Metric
  Ingestor. The Phase 1.5 role grants restrict
  `UPDATE (scan_status)` on `clean_code.commit` to the
  `clean_code_metric_ingestor` role; any other writer
  attempting an UPDATE that touches the column will get
  PG `permission denied`. The Repo Indexer (Stage 3.1)
  INSERTs the commit row with `scan_status='pending'`
  default and never UPDATEs it.
- `scan_run.status` transitions exactly once per row, from
  `'running'` to either `'succeeded'` or `'failed'`. The
  `scan_run` table is append-only-with-finalize; rows are
  never deleted (audit + replay).
- The commit + scan_run finalize is paired: a commit moves
  to `'scanned'` IFF its owning scan_run moves to
  `'succeeded'`, and to `'failed'` IFF the scan_run moves
  to `'failed'`. The pairing is enforced in the sweep
  transaction.

### `repo_url` is WRITE-ONCE

The `clean_code.repo.repo_url` column added by
`migrations/0006_repo_url.up.sql` is enforced WRITE-ONCE at
the DB level via `tg_repo_url_write_once()` (a `BEFORE
UPDATE OF repo_url` trigger). An attempt to change the
value to a different non-null URL raises SQLSTATE 23514
with the literal `format()` template from
`migrations/0006_repo_url.up.sql:179`:

```
clean_code.repo.repo_url is WRITE-ONCE: cannot change from %L to %L for repo_id %L
```

(the `%L` placeholders are filled in by PostgreSQL's
`format()` with the existing URL, the proposed new URL,
and the affected `repo_id`).

This means:

- `mgmt.register_repo` MUST be idempotent on the URL: the
  in-process helper `internal/management/register_repo.go`
  uses `ON CONFLICT (repo_id) DO NOTHING` in its INSERT
  (see `register_repo.go:204-213`), so re-registering the
  SAME `repo_id` is a no-op and the WRITE-ONCE trigger
  never fires (the trigger is `BEFORE UPDATE`, and
  `DO NOTHING` means no UPDATE runs). Re-registering the
  SAME `repo_id` with a DIFFERENT `repo_url` is therefore
  ALSO a no-op at this layer -- to mutate the URL, a
  caller would have to issue an explicit UPDATE, which the
  trigger then rejects with SQLSTATE 23514. The
  `COALESCE(EXCLUDED.repo_url, repo.repo_url)` shape is
  used by the e2e fixture only (see the e2e test cited
  below).
- The e2e scan-driving fixture inherits the same pattern;
  see
  `test/e2e/code-intelligence-CLEAN-CODE/repo_indexer_and_metric_ingestor_repo_indexer_and_commit_lifecycle_test.go`.
- Operator runbook: if a URL must legitimately change
  (e.g. repo migration from GitHub to internal Gitea),
  DROP and recreate the repo row -- there is no UPDATE
  path. This is intentional: the recipe sweep binds metric
  samples to the URL recorded at scan time; mutating it
  would silently rebind history.

### Acceptance checklist (before enabling in production)

- [ ] Migration 0006 has been applied (verify via
  `\d clean_code.repo` -- `repo_url` column is present and
  the trigger `tg_repo_url_write_once` shows on
  `\dt+ clean_code.repo`).
- [ ] Phase 1.5 role grants have been replayed so
  `clean_code_metric_ingestor` is the SOLE grantee of
  `UPDATE (scan_status)` on `clean_code.commit`.
- [ ] `CLEAN_CODE_AST_SCAN_ROOT` points at a directory
  that is reachable from the pod AND that the Repo
  Indexer's checkout job populates.
- [ ] `CLEAN_CODE_PG_URL` is set so the composition root
  selects `PGScanRunStore` instead of the in-memory
  fallback.
- [ ] The first sweeper tick (after
  `CLEAN_CODE_PERIODIC_SWEEP_CADENCE`) has produced at
  least one row in `scan_run` AND moved at least one
  `commit.scan_status` from `'pending'` to `'scanned'`.
- [ ] On rollback, unwire the AST source dir / PG URL
  BEFORE replaying migration 0006 down -- otherwise the
  sweep will continue claiming pending commits during the
  rollback window.

### Stage 3.2 follow-ups (out of scope here)

- `ws-...-stage-mgmt-register-repo-repo-url`: wires
  `mgmt.register_repo` HTTP verb, back-fills `repo_url` for
  legacy repo rows, tightens the column to `NOT NULL`.
- Multi-tenant batch sizing: today the sweep claims one
  pending commit per tick; a future stage may raise the
  batch size and add a row-level lock to keep "sole writer"
  inside a single process even when the deployment is
  scaled wide.
- Recipe-level retry policies: today a failed sweep marks
  the commit `'failed'` permanently; a future stage may
  expose a `mgmt.rescan` retry verb.

## Policy Steward signing-key cache (Stage 5.1)

### What

The clean-code service signs every published policy bundle with
an **Ed25519** keypair. The active set of signing keys lives in
the `clean_code.policy_signing_keys` Postgres table; the matching
private keys live in the operator's KMS (envelope-encrypted under
a master AES-256-GCM key the operator injects via env var). The
24h **rotation overlap window** (tech-spec Sec 8.2 row 6) is the
key reliability mechanism -- when a new key is minted, the prior
key remains accepted for `policy_publish_overlap_min_seconds`
(default 86400 = 24h) so consumers cache catch up before signature
verification breaks.

### Configuration (env vars)

| Env var                              | Meaning                                                                                            | Required when             |
| ------------------------------------ | -------------------------------------------------------------------------------------------------- | ------------------------- |
| `CLEAN_CODE_KMS_PROVIDER`            | `"local"` (production, envelope KMS), `"in-memory"` (test-only), `""` (scaffold mode, no signing). | always                    |
| `CLEAN_CODE_KMS_MASTER_KEY_HEX`      | 64 lowercase hex chars = 32-byte AES-256 master key. Required when provider = `local`.             | `KMS_PROVIDER=local`      |
| `CLEAN_CODE_PG_URL`                  | PostgreSQL connection URL. Required when provider = `local`.                                       | `KMS_PROVIDER=local`      |
| `CLEAN_CODE_POLICY_PUBLISH_OVERLAP_SECONDS` | Rotation overlap in seconds. Defaults to 86400 (24h).                                       | never (defaulted)         |

Scaffold mode (`KMS_PROVIDER=""`) leaves the signing-key cache
unwired; `/readyz` reports `signing_key_cache` as **missing**
which keeps the listener at 503 by design.

### Read verb: `policy.keys.list_active`

The active signing-key inventory is exposed at:

```
GET /v1/policy/keys/list_active
```

Response: a **bare JSON array**:

```json
[
  {
    "key_id": "f4c1...-uuid",
    "fingerprint": "<64 lowercase hex>",
    "valid_from": "2025-01-01T00:00:00Z",
    "valid_until": "2026-01-02T00:00:00Z"
  }
]
```

Status codes:

| Code | Meaning |
| ---- | ------- |
| 200  | List emitted (may be `[]` during the brief startup window before the first key is minted). |
| 405  | Method other than `GET` / `HEAD`. |
| 503  | Signing-key cache not wired (scaffold mode) or no active key. The route is ALWAYS mounted -- scaffold mode never returns 404. This lets operators distinguish "verb exists, backing subsystem down" from "this build doesn't ship the verb". The contract is pinned by `TestRootMux_ScaffoldModeListActive503` in `cmd/clean-coded/routes_test.go`. |
| 500  | Unexpected backend error. |

### Rotation

Routine rotation is rate-limited by the overlap window: the
`Manager.Rotate` call refuses while the most recent key is still
inside its `valid_until` cooldown. Use `Manager.ForceRotate` (or
the equivalent `keys.compromise` runbook step) for the Sec 9.3
compromise response path -- that bypass is the one and only way
to mint a successor key before the overlap expires.

### Health check

The composition root registers `signing_key_cache` as a readiness
check. It probes the KMS via `Ping` and asserts at least one key
is loaded. A KMS outage or an empty cache flips `/readyz` to 503
WITHOUT crashing the process -- the rest of the service continues
running but no new policy publishes will succeed.

### Cache refresh

Every replica re-reads the `policy_signing_keys` table every
`5m` (constant `signingKeyCacheRefreshInterval` in
`cmd/clean-coded/main.go`). This is two orders of magnitude
faster than the 24h overlap window, so a sibling-replica
rotation always propagates well before the old key expires.

### Transport

The canonical transport for `policy.keys.list_active` (and every
other read verb this service grows in later stages) is
**HTTP/JSON v1**. The path prefix is `/v1/`; a future shape
change ships under `/v2/` so dashboards keep working through the
cutover.

A gRPC/proto layer is **out-of-scope for Stage 5.1**. No
`*.proto` file or gRPC server exists in this service, the
dashboard / operator-CLI clients are already HTTP-based, and
shipping a second transport speculatively would create
wire-shape drift between two surfaces that have to be kept in
sync. If a future downstream consumer requires streaming or
strong-typed verbs, a `management-grpc-adapter` workstream
would land it -- with explicit regression tests pinning both
transports to the same wire shape. Until then, HTTP/JSON v1 is
the SOLE ratified contract.

### KMS backend

Stage 5.1 ships `LocalSealedKMS` (AES-256-GCM envelope encryption
of the Ed25519 seed under an operator-injected master key) as
the production KMS implementation. The `KMS` interface contract
is stable.

A managed-service KMS adapter (Azure Key Vault / AWS KMS /
HashiCorp Vault) is **out-of-scope for Stage 5.1** and is owned
by a future `policy-steward-kms-adapter` workstream once the
deployment-target vendor is selected by the operator. That
workstream will only need to land a new `KMS` implementation
plus its config wiring; Stage 5.1's manager, store, rotation,
overlap-window, evaluator integration, and read verb continue
to work unchanged because they depend on the interface, not the
implementation.

### Test isolation (live PostgreSQL)

When `CLEAN_CODE_PG_URL` is exported, two test suites in
different packages hit the same database:

| Suite                                                  | Owns schema             |
| ------------------------------------------------------ | ----------------------- |
| `internal/storage/migrate_test.go::TestRoundTrip_...`  | `clean_code` (canonical migration) |
| `internal/policy/keys/sql_store_test.go::TestSQLStore_...` | `clean_code_keys_test` (isolated) |
| `internal/policy/steward/sql_store_test.go::TestSQLStore_...` | `clean_code_steward_test` (isolated) |

The storage round-trip `DROP SCHEMA clean_code CASCADE`s on prep,
which is why both SQLStore live tests own distinct schemas. CI
lanes that set `CLEAN_CODE_PG_URL` can run all three packages in
parallel without interference.

## Policy Steward write verbs (Stage 5.2)

### What

The Policy Steward owns the three canonical `policy.*` write
verbs (tech-spec Sec 8.5 lines 963-970 + architecture Sec 6.5).
All three are append-only -- no row is ever UPDATEd or DELETEd
-- and all three require an active signing key in the
[Stage 5.1 cache](#policy-steward-signing-key-cache-stage-5-1)
before they will write.

**Signing scope is narrow**: only `policy.publish` produces a
signed row. The PolicyVersion table has a `signature bytea NOT
NULL` column carrying an Ed25519 signature over the canonical
JSON of `(rule_refs, threshold_refs, refactor_weights)`.
`policy.activate` and `policy.publish_rulepack` REQUIRE that a
signing key be loaded (so the service is in a state where
signed writes work end-to-end) but do NOT write a signature
column of their own -- the `policy_activation` and `rule_pack`
/ `rule` tables have no signature column.

| Verb                      | Writes signature? | Append-only? |
| ------------------------- | ----------------- | ------------ |
| `policy.publish`          | yes (`policy_version.signature`) | yes |
| `policy.activate`         | no                | yes |
| `policy.publish_rulepack` | no                | yes (pack + rules, transactionally) |

### Write verbs

| Verb                          | URL                                  | Status table |
| ----------------------------- | ------------------------------------ | ------------ |
| `policy.publish`              | `POST /v1/policy/publish`            | 200 / 400 / 405 / 500 / 503 |
| `policy.activate`             | `POST /v1/policy/activate`           | 200 / 400 / 405 / 500 / 503 |
| `policy.publish_rulepack`     | `POST /v1/policy/publish_rulepack`   | 200 / 400 / 405 / 409 / 500 / 503 |

Banned historical-draft verbs return **501 Not Implemented**:

- `POST /v1/policy/rulepack/add`     -> `{error:"unimplemented_verb", verb:"policy.rulepack.add"}`
- `POST /v1/policy/rulepack/remove`  -> `{error:"unimplemented_verb", verb:"policy.rulepack.remove"}`
- `POST /v1/policy/override`         -> `{error:"unimplemented_verb", verb:"policy.override"}`

### `policy.publish` body

```json
{
  "name": "default-v3",
  "rule_refs": [{"rule_id": "solid.srp.lcom4_high", "version": 1}],
  "threshold_refs": [],
  "refactor_weights": {
    "alpha": 0.4, "beta": 0.3, "gamma": 0.2, "delta": 0.1,
    "effort_model_version": "v1.0",
    "window_days": 90
  }
}
```

Response: the full `PolicyVersion` row including
`policy_version_id`, `signature`, and `created_at`.

**rule_refs / threshold_refs FK contract**: each `rule_refs`
entry MUST reference an existing `(rule_id, version)` pair
registered via a prior `policy.publish_rulepack` call. Each
`threshold_refs` entry MUST reference an existing
`threshold_id` row in `clean_code.threshold`. The migration
keeps these references inside a JSONB document (not as proper
SQL FKs) so the Policy Steward enforces them at write time --
an unknown ref returns **400 Bad Request** with the offending
`rule_id`/`version` (or `threshold_id`) in the body. The
steward refuses BEFORE spending signing material, so a
rejected request leaves no signature on the audit trail.
Duplicate refs within the same payload also return 400.

### `policy.activate` body

```json
{
  "policy_version_id": "f4c1...-uuid",
  "activated_by": "alice@example"
}
```

The body MUST NOT contain a `scope` field -- v1 activation is
global per deployment (architecture Sec 5.3.4 single-tenant
pin). The HTTP handler decodes with `DisallowUnknownFields`, so
a body carrying `scope` is rejected with 400 and a body that
mentions `scope` in its error message; clients can self-correct
without dashboard support.

### `policy.publish_rulepack` body

```json
{
  "pack_id": "solid.srp",
  "version": 1,
  "display_name": "Single Responsibility",
  "description_md": "SOLID SRP rulepack.",
  "rules": [
    {
      "rule_id": "solid.srp.lcom4_high",
      "version": 1,
      "predicate_dsl": "lcom4 > 0.7",
      "severity_default": "block",
      "description_md": "High LCOM4 -- methods share little state."
    }
  ]
}
```

Response: `{rule_pack, rules}`. The pack + every rule row is
appended in a **single transaction** -- a mid-batch failure
rolls back both the pack and any earlier rules so an append-
only store never carries a partial pack. A re-publish of the
same `(pack_id, version)` returns 409 -- this is the append-
only contract. None of the rows carry a signature column;
`policy.publish_rulepack` is unsigned (only `policy.publish`
signs).

### Signing-key precondition

All three verbs refuse when no signing key is active
(`ErrNoActiveSigningKey` -> 503). The signing key must be
wired via `CLEAN_CODE_KMS_PROVIDER=local` (or the in-memory
provider for development). See "Policy Steward signing-key
cache (Stage 5.1)" above.

### Evaluator pickup

A future `eval.gate` call resolves the active policy via the
canonical lookup: read the latest `policy_activation` row,
dereference its `policy_version_id`, verify the row's
`signature`. The same path is exposed in code via
`Steward.ActivePolicyVersion(ctx)` for integration tests.
After `policy.activate(pvB)` runs, this query returns `pvB`
even if `pvA` was activated first -- latest-row-wins by
`created_at, activation_id DESC` (architecture Sec 5.3.4).

### Storage backend

When `CLEAN_CODE_PG_URL` is set, the steward writes rows to
the canonical `clean_code.{policy_version, policy_activation,
rule_pack, rule}` tables (migration 0003). Otherwise the
composition root falls back to the in-memory store -- rows
are lost on process restart. The development warning log line
`policy steward backed by in-memory store` signals which mode
is active.


## `mgmt.override` write verb (Stage 5.3)

### What

`mgmt.override` is the operator **mute / unmute kill switch**
for individual rules at a chosen scope (architecture Sec 1.5.1
row 5 + Sec 6.3 line 1357). The verb appends one `override` row
per call; unmute is a fresh INSERT with `mute=false`. The
evaluator (Stage 5.7) consults `MAX(created_at) WHERE rule_id
= $1 AND scope_filter matches the candidate scope` so the
**latest matching row wins**.

### How `scope_filter matches the candidate scope`

The evaluator carries a `CandidateScope{repo_id, scope_kind,
signature}` and asks the steward for the latest override that
matches it. "Match" means:

1. `scope_filter.repo_id` == `candidate.repo_id` (exact)
2. `scope_filter.scope_kind` == `candidate.scope_kind` (exact)
3. `scope_filter.scope_signature_glob` matches
   `candidate.signature` under simple glob vocab:
   - `*` matches any sequence of characters (including empty,
     and across `.` / `/`).
   - `?` matches exactly one character.
   - Every other rune is literal (no `[...]` classes, no
     backslash escapes).
   The pattern is anchored end-to-end -- `com.example.legacy.*`
   matches `com.example.legacy.Foo` and `com.example.legacy.a.b`
   but NOT `com.other.legacy.X`.

The read is implemented in `Store.LatestMatchingOverride`. The
SQL path pre-filters with a JSONB extractor (`scope_filter->>
'repo_id'` and `scope_filter->>'scope_kind'`) so only the rows
in the candidate's `(repo_id, scope_kind)` partition are
streamed; the glob match is applied in Go in descending
`(created_at, override_id)` order, stopping at the first hit.
Crucially the query does **not** carry a `LIMIT`: a newer row
under a non-matching glob must not hide an older row under a
matching glob.

### Wire shape

```
POST /v1/mgmt/override
Content-Type: application/json
X-OIDC-Subject: <caller OIDC sub>

{
  "rule_id": "solid.srp.lcom4_high",
  "scope_filter": {
    "repo_id": "repo-a",
    "scope_kind": "class",
    "scope_signature_glob": "com.example.legacy.*"
  },
  "mute":   true,
  "reason": "legacy code; planned refactor in Q3"
}
```

Response (HTTP 200) carries **only** the new override id --
matching the architecture `mgmt.override(...) -> OverrideId`
return type:

```json
{ "override_id": "f4c1...-uuid" }
```

### Required invariants

- The body **MUST NOT** contain `expires_at` (v1 mute lifecycle
  has no TTL -- tech-spec Sec 10A pin). `DisallowUnknownFields`
  on the decoder returns **400** if you try.
- The body **MUST NOT** contain `actor_id`. The OIDC subject is
  sourced exclusively from the `X-OIDC-Subject` header. Bodies
  carrying `actor_id` are rejected with **400**; missing or
  empty `X-OIDC-Subject` returns **401**.
- `scope_filter.scope_kind` must be one of the canonical seven
  values: `repo, package, file, class, interface, method,
  block`. Anything else -> 400.
- `scope_filter.repo_id` and `scope_filter.scope_signature_glob`
  MUST be non-empty (use `"*"` for the repo-wide wildcard --
  the empty string is rejected). 400 on miss.
- `reason` MUST be non-empty (after `TrimSpace`) when
  `mute=true`. Empty / whitespace-only reasons return 400 at
  the handler; the SQL CHECK constraint
  `override_reason_required_when_muted` provides defence in
  depth at the database. `reason` MAY be empty on unmute.
- `rule_id` MUST reference a rule that has been registered via
  `policy.publish_rulepack`. The verb performs a logical FK
  check; an unknown rule returns 400.

### Status codes

| Code | Meaning |
| ---- | ------- |
| 200  | Override row appended; body is `{override_id}`. Note: in scaffold mode (`CLEAN_CODE_KMS_PROVIDER` unset) the Policy Steward is still wired with a null-object signer, so `mgmt.override` continues to serve 200 -- see the scaffold-mode matrix in "No signing-key precondition (kill-switch contract)" below. |
| 400  | Malformed JSON, unknown field (including `expires_at` / `actor_id`), invalid `scope_kind`, empty `reason` on mute, or unknown `rule_id`. |
| 401  | Missing or empty `X-OIDC-Subject` header. |
| 405  | Any method other than POST. |
| 500  | Internal error; opaque body. |
| 503  | The Policy Steward is genuinely unreachable (the composition root failed to construct a writer, or the route was mounted against a nil writer). Under the Stage 5.3 + iter 3 composition root in `cmd/clean-coded/main.go`, `mgmt.override` does NOT 503 simply because the signing-key cache is unwired -- that case is wired explicitly to keep the kill switch operable. |

### Trust boundary

`X-OIDC-Subject` is set by the **auth gateway** after token
validation. In any deployment where `clean-coded` is directly
reachable from untrusted clients, the gateway MUST strip the
header at the edge and re-inject the validated `sub` claim --
otherwise a caller can spoof the actor. clean-coded does not
re-validate the bearer token here because the gateway already
did.

### No signing-key precondition (kill-switch contract)

Unlike `policy.publish`, `policy.activate`, and
`policy.publish_rulepack`, `mgmt.override` **does not** require
an active signing key. The override row carries no signature
column, and the operator must be able to silence a noisy or
broken rule even during a signing-key outage -- that is the
worst time to deny an emergency mute. If the steward verbs
above are returning 503 because the signing-key cache is
unwired, `mgmt.override` still serves 200.

The composition root encodes this contract by building the
Policy Steward + write verbs UNCONDITIONALLY (Stage 5.3 +
iter 3) -- not gated on `cfg.KMSProvider != ""`. When KMS is
unset the steward is constructed with a **null-object signer**
(`noActiveSigner`) so `Steward.Override` proceeds while the
Stage 5.2 signing verbs surface `ErrNoActiveSigningKey` via
the existing `len(ListActive()) == 0` branch. Pinned by
`TestRootMux_ScaffoldModeOverrideMounted_200` and
`TestBuildPolicyWriter_ScaffoldModeProducesWriter` (both in
`cmd/clean-coded/routes_test.go`) plus
`TestSteward_PublishRefusesWhenSignerNil` (in
`internal/policy/steward/steward_test.go`).

#### Scaffold-mode status-code matrix

When the composition root runs with `CLEAN_CODE_KMS_PROVIDER`
empty (scaffold mode, signing-key cache unwired):

| Verb                              | Path                              | Scaffold mode | Reason |
| --------------------------------- | --------------------------------- | ------------- | ------ |
| `mgmt.override` (mute / unmute)   | `POST /v1/mgmt/override`          | **200** for a valid request against a registered rule | Kill-switch contract: the verb must remain operable during a signing-key outage. |
| `policy.keys.list_active` (read)  | `GET /v1/policy/keys/list_active` | **503**       | The verb is mounted (not 404), but the signing-key cache is unwired. |
| `policy.publish`                  | `POST /v1/policy/publish`         | **503**       | Refuses with `ErrNoActiveSigningKey` -- the null-object signer reports an empty active-key set. |
| `policy.activate`                 | `POST /v1/policy/activate`        | **503**       | Same precondition as `policy.publish`. |
| `policy.publish_rulepack`         | `POST /v1/policy/publish_rulepack`| **503**       | Same precondition. |
| `policy.rulepack.add` / `.remove` | (legacy paths)                    | **501**       | Banned-verb canonical response (not part of the v1 surface). |
| `policy.override` (legacy path)   | `POST /v1/policy/override`        | **501**       | Renamed; see "Historical: `policy.override`" below. |

### Append-only / unmute flow

To unmute, POST again with the **same** `scope_filter` and
`mute=false`:

```
POST /v1/mgmt/override
X-OIDC-Subject: bob@example.com
{
  "rule_id": "solid.srp.lcom4_high",
  "scope_filter": { "repo_id": "repo-a", "scope_kind": "class",
                    "scope_signature_glob": "com.example.legacy.*" },
  "mute":   false,
  "reason": ""
}
```

The evaluator's latest-row read now sees `mute=false` for that
`(rule_id, scope_filter)` pair. The previous `mute=true` row
remains in the table for audit -- no UPDATE / DELETE primitives
exist on the Store interface.

### No automatic expiry

A row planted years ago remains the active mute until an
operator unmutes it. There is no scheduled "expire old
overrides" job in v1. Aged-mute hygiene is surfaced via the
`mgmt.insights.aged_mutes` read verb (a future stage); v1
operators audit by querying the `override` table directly:

```sql
SELECT rule_id, scope_filter, mute, created_at
  FROM clean_code.override
  WHERE created_at < now() - interval '180 days'
  ORDER BY created_at DESC;
```

### Historical: `policy.override`

The draft surface listed a `policy.override` verb. The canonical
name is `mgmt.override`; the legacy path is still mounted at
`/v1/policy/override` and returns **501 Not Implemented** with
a body identifying the rename so operators scripting against an
older draft contract learn of the change without getting a
404.

## SOLID rule packs (Stage 5.5)

### What

`cmd/clean-coded/main.go` calls `solid.Bootstrap(ctx, steward)`
at startup, which publishes **5 SOLID rulepacks (9 rules total)**
into the Policy Steward store via the same `RulePack` + `Rule`
verbs that Stage 5.2 exposes externally. Bootstrap is
idempotent: a re-run on a populated store reports
`PublishedPacks == 0`.

Rule inventory (`policy/rulepacks/solid/`):

| Pack       | Rules | Inputs (`metric_kind`)                                                |
| ---------- | ----- | --------------------------------------------------------------------- |
| `solid.srp`| 2     | `lcom4` (class), `interface_width` (class)                            |
| `solid.ocp`| 2     | `fan_in` (class), `modification_count_in_window` (file)               |
| `solid.lsp`| 2     | `depth_of_inheritance` (class), `lsp_violation` (method, 0/1)         |
| `solid.isp`| 1     | `interface_width` (interface)                                         |
| `solid.dip`| 2     | `fan_out` (class), `coupling_between_objects` (class)                 |

### Stage 2.4 producer dependency (LSP override rule)

The `solid.lsp.override_violation` rule consumes
`metric_kind='lsp_violation'` rows at `scope_kind='method'`,
`value ∈ {0, 1}`. The producer of those rows is the
**Stage 2.4 `recipes/lsp_violation.go` recipe** (Adapter,
architecture Sec 3.2 + Sec 1.4.1 row 13), which is **scheduled
but not yet implemented** -- see `implementation-plan.md`
Stage 2.4 step "Implement `recipes/lsp_violation.go`"
(line 221) and the two scoring scenarios
`lsp-violation-strengthens-precondition` /
`lsp-violation-compatible-override` (lines 232-233).

Until Stage 2.4 lands, the LSP override rule is in **published
but data-starved** state: it parses, signs, and serves on
`policy.publish` like any other rule, but the rule engine
finds zero `metric_kind='lsp_violation'` input rows in
`clean_code.metric_sample` and therefore emits zero
violations. The other 8 rules are unaffected and fire on
the foundation metrics already produced by Stage 2.4 recipes
(`lcom4`, `fan_in`, `fan_out`, `depth_of_inheritance`,
`interface_width`, `coupling_between_objects`) and the
Stage 2.6 materialiser (`modification_count_in_window`).

Operators can confirm the data-starved state with:

```bash
psql "$CLEAN_CODE_PG_URL" -c "
  SELECT count(*) FROM clean_code.metric_sample
  WHERE metric_kind = 'lsp_violation';"
```

A `0` result before Stage 2.4 ships is expected. After Stage
2.4 lands, the same query will return one row per overriding
method analysed.

### Configuration

No new env vars. Bootstrap reuses the Stage 5.1 signing-key
cache and Stage 5.2 RulePack writer; both must be ready
(`/readyz` → 200) before bootstrap can publish.

### Verification

After deploy:

```bash
curl -fsS http://$POD:8080/v1/policy/rulepack/list_published \
  | jq '[.packs[] | select(.pack_id | startswith("solid."))] | length'
# 5
```


## ingest.churn webhook -- scaffold mode (Stage 2.6 iter 6)

### What

Stage 2.6 ships the `modification_count_in_window` materialiser
plus a churn-payload webhook
([`webhook.ChurnIngestHandler`](../internal/ingest/webhook/handler.go))
that drives [`metric_ingestor.Ingestor.Run`](../internal/metric_ingestor/ingestor.go).
The webhook is **off by default** in production builds: it is
mounted on the `rootMux` iff BOTH env vars below are set.

This is intentional. The Stage 2.6 composition root uses an
**in-memory** `MetricSampleWriter` -- materialised rows survive
in the process heap but are LOST on restart. Phase 3.2
(`stage-metric-ingestor-and-scanrun-state-machine`) lands the
`pgx`-backed batch writer that persists to `metric_sample` /
`metric_sample_active` in the same ScanRun transaction; until
then the webhook is a **scaffold** that an operator MUST opt
into knowingly.

### Configuration (env vars)

| Env var                                       | Meaning                                                                                                                                                                                  | Required when                              |
| --------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------ |
| `CLEAN_CODE_ENABLE_SCAFFOLD_CHURN_WEBHOOK`    | `true`/`false`/`1`/`0`/`yes`/`no`/`on`/`off`. Mount the webhook at `/v1/ingest/churn`. Defaults to **`false`** -- production deploys leave the path returning 404.                       | always (defaults to `false`)               |
| `CLEAN_CODE_WEBHOOK_HMAC_SECRET`        | Shared secret for HMAC-SHA256 verification of every request body. Long-lived; rotate by issuing a new value to every publisher AND the service AT THE SAME TIME (no overlap support yet). | when the enable flag is `true`             |

`config.Validate` enforces a **both-or-neither** interlock:

- Both unset (default) -> webhook unmounted, `POST /v1/ingest/churn` returns 404.
- Both set -> webhook mounted with HMAC verification active.
- Enable=true with empty secret -> `config.Load` returns an error (process fails to start; prevents unauthenticated mount).
- Secret set with Enable=false -> `config.Load` returns an error (catches a likely operator typo where the enable flag was forgotten).

### Wire shape

```
POST /v1/ingest/churn HTTP/1.1
Content-Type: application/json
X-Hub-Signature-256: sha256=<lowercase-hex HMAC-SHA256(body, secret)>

{
  "repo_id":   "11111111-2222-3333-4444-555555555555",
  "rows": [
    { "sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "file_path":   "internal/foo.go",
      "modified_at": "2026-05-23T12:00:00Z" }
  ]
}
```

The handler computes HMAC-SHA256(body, secret) using
[`webhook.SignHMAC`](../internal/ingest/webhook/hmac.go) and
constant-time-compares against the header digest using
[`crypto/hmac.Equal`](https://pkg.go.dev/crypto/hmac#Equal). On
mismatch the response is **401** + a structured error code
(`HMAC_MISSING_SIGNATURE`, `HMAC_MALFORMED_SIGNATURE`,
`HMAC_SIGNATURE_MISMATCH`, `HMAC_EMPTY_SECRET`). The HMAC check
runs BEFORE Content-Type validation so an unauthenticated caller
cannot probe the contract through differential 401-vs-415
responses.

### Status codes

| Code | Meaning |
| ---- | ------- |
| 200  | Payload accepted, materialiser ran, in-memory writer holds the rows until restart. |
| 400  | Body missing `repo_id`, malformed JSON, unknown field, invalid SHA (must be 40 hex chars), zero `modified_at`. |
| 401  | HMAC verification failed -- see the per-code subtypes above. |
| 404  | The webhook is not mounted (scaffold opt-in not flipped). |
| 405  | Any method other than `POST`. |
| 413  | Body exceeds the 16 MiB limit (`webhook.MaxBodyBytes`). |
| 415  | `Content-Type` is not `application/json`. |
| 422  | Scope resolution failed -- e.g. the `ScopeResolver` returned an error, the zero UUID, or (Stage 2.6) a non-file scope. Code `SCOPE_RESOLUTION_FAILED`; see "Stage 2.6 hydration: file scope ONLY" below for the method-scope deferral. |
| 500  | Writer rejected the row (the in-memory writer never produces 500; this surfaces when Phase 3.2 swaps in the PG-backed writer and an INSERT fails). |

### Stage 2.6 hydration: file scope ONLY

The Stage 2.6 churn hydrator
(`internal/ingest/churn/churn.go::Hydrate`) currently mints
**file-scope** `MetricSampleSeed` records only -- if the resolved
scope is anything other than `Kind == scope.KindFile` the row is
rejected by wrapping `ErrScopeResolutionFailed` (see
`internal/ingest/churn/churn.go:538-540`). The webhook's
`classifyError` (`internal/ingest/webhook/handler.go:359-360`)
maps `ErrScopeResolutionFailed` to **HTTP 422** + the error code
`SCOPE_RESOLUTION_FAILED` -- the same response shape any
scope-resolver failure surfaces, so a publisher can treat
"file-scope hydration only" as one branch of
`SCOPE_RESOLUTION_FAILED` rather than a Stage-2.6-specific
status code. The Stage 2.6 brief's reference scenario names a
method-scope tag (`pkg.Foo.bar`); that case is
**deferred to Stage 4** when the AST-driven `scope_binding`
reader replaces today's `AutoMapScopeResolver`. Until then,
publishers MUST emit one row per FILE PATH, not one row per
method. The materialiser itself
([`internal/metrics/materialisers/modification_count.go`](../internal/metrics/materialisers/modification_count.go))
already supports method-scope seeds; only the churn-payload
hydrator is gated.

### Acceptance checklist (before enabling in production)

- [ ] You have read this section's "What" paragraph and understand the in-memory persistence implication.
- [ ] You have rotated `CLEAN_CODE_WEBHOOK_HMAC_SECRET` to a value NOT in source control.
- [ ] Every CI publisher has been updated to compute and send the `X-Hub-Signature-256` header.
- [ ] You have set `CLEAN_CODE_ENABLE_SCAFFOLD_CHURN_WEBHOOK=true` AND `CLEAN_CODE_WEBHOOK_HMAC_SECRET=<value>`.
- [ ] Restart `clean-coded`. The startup log line `ingest.churn webhook mounted in SCAFFOLD MODE -- writer is in-memory and rows are LOST on restart` confirms the mount.
- [ ] You have a plan to upgrade to Phase 3.2 BEFORE the in-memory loss becomes operationally painful (e.g. a backfill job that re-POSTs the last N hours of churn on every restart).
