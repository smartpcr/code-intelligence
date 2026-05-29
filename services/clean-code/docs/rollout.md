## `services/clean-code` rollout playbook

How to roll a new build of the clean-code service into a
production-tier environment. The instructions assume Postgres
14+ is already running and the `clean_code_*` roles have been
created via the `0004_roles.up.sql` migration.

## Stage 10.2: Aged mute insights report

This subsection captures the rollout sequence for the
aged-mute Insights report -- the `mgmt.read.insights.aged_mutes`
projection added in
`internal/management/insights/aged_mutes.go` and wired into
`internal/management/reader.go` via
`WithAgedMutes(*insights.AgedMutes)`. The unit of rollout
is the binary that hosts the management HTTP surface
(`cmd/clean-code-metric-ingestor` today, future
`cmd/clean-code-gateway`); no schema migration ships with
this stage -- the report is read-only over the existing
`override` table.

### Pre-flight (no infra change)

1. The Stage 5.3 `mgmt.override` write verb MUST already be
   live in the target environment -- the report's input is
   the SAME `override` table that verb appends to. If
   `mgmt.override` has never been called for a tenant the
   report returns an empty list, which is the correct
   answer (nothing to flag).
2. NO migration runs. The report is a pure read over the
   existing `override` table; no new column, no new index,
   no new role.
3. NO feature flag is required. The Reader option
   `WithAgedMutes(nil)` is permitted and leaves the verb
   returning `ErrBackendUnavailable` -- the same
   "backend not wired" shape as `mgmt.read.cross_repo`
   when its `MetricsBackend` is missing. Rolling forward
   without wiring is safe.

### Wiring

1. In the binary's composition root, construct the production
   adapter that bridges `policy/steward.Store.ListAllOverrides`
   to `insights.OverrideReader`. The adapter ships in this
   stage as
   `management.OverrideReaderFromStore{Store: stewardStore}`
   (`internal/management/insights_override_adapter.go`) -- a
   field-for-field value mapper held out of the `insights`
   package to preserve its zero-`internal/*`-deps invariant.
2. Construct the projection with the default 90-day
   threshold and the production system clock by passing
   `nil` for the clock argument:
   `agedMutes := insights.NewAgedMutes(overrideAdapter, nil)`.
   The two-argument constructor lets bring-up tests inject a
   `FixedClock`; production callers ALWAYS pass `nil`.
3. Pass `management.WithAgedMutes(agedMutes)` to the
   existing `management.NewReader(...)` call alongside the
   Stage 7.3 `WithInsightsFreshness(...)` option.
4. Deploy. The verb is live as soon as the binary boots.

### Verb contract

| field | type | source |
|---|---|---|
| `mode` | string | repository-resolved mode (echoes `mgmt.read.cross_repo`) |
| `threshold_days` | int | effective threshold (caller's `threshold_days` or 90) |
| `aged_mutes[].rule_id` | string | `override.rule_id` |
| `aged_mutes[].scope_kind` | string | `override.scope_filter.scope_kind` |
| `aged_mutes[].scope_signature_glob` | string | `override.scope_filter.scope_signature_glob` |
| `aged_mutes[].repo_id` | string | `override.scope_filter.repo_id` |
| `aged_mutes[].override_id` | string | `override.override_id` |
| `aged_mutes[].created_at` | RFC3339 | `override.created_at` |
| `aged_mutes[].age_days` | int | `floor((now - created_at) / 24h)` |
| `aged_mutes[].reason` | string | `override.reason` (free-form, may be empty) |
| `aged_mutes[].actor` | string | `override.actor_id` |

### Verification checklist

- [ ] `GET /v1/mgmt/read/insights/aged_mutes` (when mounted)
      returns HTTP 200 with `aged_mutes: []` against an
      empty `override` table.
- [ ] Append `override(mute=true)` with
      `created_at = now() - 100 days`. The next read returns
      a one-row `aged_mutes` array containing that override.
- [ ] Append `override(mute=false)` with the SAME
      `(rule_id, scope_filter)` and a LATER `created_at`.
      The next read returns `aged_mutes: []` -- the unmute
      row drops the pair off the report.
- [ ] Call with `?threshold_days=30` against a 100-day-old
      and a 20-day-old mute. ONLY the 100-day row surfaces;
      the 20-day row is filtered out.
- [ ] Call with `?threshold_days=0`. The default 90-day
      threshold applies (non-positive override is clamped
      to default).

### Rollback

The verb is read-only -- rollback is a binary downgrade.
NO data migration runs forward, so NO down migration runs
backward. The `override` table is unchanged.

### Out of scope for this stage

- **TTL enforcement.** v1 has NO TTL enforcement in code
  (iter 1 evaluator item 5). The report is a SURFACE for
  operators; the rule engine continues to honor every
  mute regardless of age. A future stage may add an
  enforcement policy that consumes this projection.
- **HTTP route mount.** Like `mgmt.read.cross_repo` and
  `mgmt.read.portfolio`, the Reader method ships here;
  the HTTP route lands in a downstream gateway stage that
  owns `Routes()` in `verbs.go`. The Reader method is the
  contract surface that gateway stage consumes.

## Stage 8.3: ML effort-model loader and version pinning

This subsection captures the rollout sequence for the
`internal/refactor/effort_model.go` loader -- the
external-artefact + version-pinning interlock that
replaces the Stage 8.2 `refactor_task.effort_hours = 0.0`
placeholder. The unit of rollout is the
`clean-code-refactor-planner` binary; no schema migration
is required (the effort-model version is recovered
transitively through `hot_spot.policy_version_id`, NOT a
new column on `refactor_plan` / `refactor_task`).

### Required env vars

Set on every `clean-code-refactor-planner` instance:

```
CLEAN_CODE_REFACTOR_EFFORT_SOURCE="ML model from historical commits"
CLEAN_CODE_REFACTOR_EFFORT_MODEL_URI="file:///var/lib/clean-code/models/effort-v1.2.0.json"
```

- `CLEAN_CODE_REFACTOR_EFFORT_SOURCE` -- defaults to the ML
  pin per architecture Sec 1.6. Setting it explicitly is
  optional in v0 but recommended so the operator-pin
  surface is auditable. The other accepted value is
  `"none"` (opt-out -- planner runs without an
  estimator; not for production).
- `CLEAN_CODE_REFACTOR_EFFORT_MODEL_URI` -- REQUIRED when
  source is the ML pin. v0 accepts local-disk URIs only:
  bare paths (`/abs/path/model.json`, `C:\path\model.json`,
  `C:/path/model.json`, `./rel/path.json`); `file://` URIs
  (`file:///abs/path` on POSIX, `file:///C:/path` on
  Windows). Schemes other than `file://` are refused at
  startup.

### Artefact deployment

The JSON artefact MUST be present on disk at the URI BEFORE
the binary starts. Recommended layout:

```
/var/lib/clean-code/models/effort-v1.2.0.json     <- this build
/var/lib/clean-code/models/effort-v1.1.3.json     <- previous build (kept for one rollout cycle)
```

Use distinct filenames per `version` (the artefact's
internal pin) so an in-flight rollback can re-point the env
var without overwriting the new artefact.

Distribute through your existing config / secret
distribution channel (Ansible / Kubernetes ConfigMap mount
/ Vault sidecar -- pick whatever the rest of the
deployment uses). The loader does NOT integrate with
anything other than the local filesystem in v0.

### Rollout sequence (ZERO-DOWNTIME, version-pinned)

The version-pinning interlock means the model artefact and
the active policy MUST move together. Roll in this order:

1. **Stage the new model artefact** on every planner host
   under a NEW filename (do NOT overwrite the old one).
   Verify with `cat`.
2. **Publish the new `policy_version`** through
   `cmd/clean-code-gateway` (Stage 5.2 verbs). The
   `refactor_weights.effort_model_version` field MUST
   equal the new artefact's `version` string verbatim.
   Activate the new `policy_version` via
   `mgmt.activate_policy` (Stage 5.2). The active
   activation is what `PolicyReader.ActivePolicyVersion`
   returns; the planner reads it once per `Plan` call.
3. **Roll the planner binaries** with the new env var
   pointing at the new artefact path. As each instance
   restarts:
   - On the new instance: load + validate succeeds; the
     instance starts serving and emits estimates against
     the new policy. If the active policy still references
     the OLD model version, `Plan` returns
     `ErrEffortModelVersionMismatch` and NO `refactor_plan`
     or `refactor_task` rows are written -- so a misordered
     rollout (step 3 before step 2) is harmless but stalls
     planning until corrected.
   - On the old instances: still loaded with the OLD
     artefact, they also return `ErrEffortModelVersionMismatch`
     for the new active policy (same write-nothing safety
     net). Old instances drain naturally as their
     deployment slot recycles.
4. **Remove the old artefact file** after all planner hosts
   have rolled. Optional -- safe to keep for one cycle in
   case of rollback.

### Rollback

Two-step rollback (mirror of rollout):

1. Re-activate the previous `policy_version` via
   `mgmt.activate_policy`.
2. Roll planner binaries back with the env var pointing at
   the previous artefact path (which you kept on disk).

NO `refactor_plan` or `refactor_task` rows need to be
deleted; the rolled-forward rows are valid relative to the
policy they were emitted under, and the
`policy_version_id` they reference (via the `hot_spot` row)
preserves their reproducibility.

### Smoke checks post-rollout

- `clean-code-refactor-planner` process is running on every
  host (`systemctl status` or pod readiness).
- Structured log line on startup names the loaded
  artefact's `version` (level `info`, message
  `effort_model_loaded`).
- A single round of planning emits non-zero
  `refactor_task.effort_hours` values:

  ```sql
  SELECT plan_id, kind, effort_hours
  FROM clean_code.refactor_task
  WHERE created_at > NOW() - INTERVAL '5 minutes'
  ORDER BY created_at DESC
  LIMIT 20;
  ```

  Every `effort_hours` should be `> 0` (or exactly `0`
  only when the formula legitimately clamps -- e.g.
  `intercept` set so that
  `base + score_coef*0 + intercept <= 0` for a particular
  kind/score combination).
- No `ErrEffortModelVersionMismatch` lines in the planner
  logs after step 3 completes.

### Failure-mode checklist

| symptom | cause | remediation |
|---|---|---|
| planner exits 1 with `ErrEffortModelURIRequired` | env var unset | set `CLEAN_CODE_REFACTOR_EFFORT_MODEL_URI`. |
| planner exits 1 with `ErrEffortModelUnsupportedScheme` | URI uses `http://` / `s3://` / etc. | switch to `file://` or a bare path; v0 supports local only. |
| planner exits 1 with `ErrEffortModelMalformed` | JSON has unknown top-level field or invalid syntax | re-validate the artefact (`jq '.' < artefact.json`); compare keys to the schema in `runbook.md`. |
| planner exits 1 with `ErrEffortModelMissingKindBase` | `kind_base_hours` lacks one of the five canonical kinds | add the missing kind to the artefact. |
| every `Plan` call returns `ErrEffortModelVersionMismatch` | active policy's `effort_model_version` != loaded artefact's `Version` | re-activate the matching `policy_version` OR re-deploy planner with the matching artefact. |
| `effort_hours` always exactly `0` | wired `ZeroEffortEstimator` OR source = `"none"` | check env: `CLEAN_CODE_REFACTOR_EFFORT_SOURCE` must be the ML pin; URI must point at a real artefact. |

## Stage 9.2: Audit WAL Reconciler (replay-only)

This subsection captures the rollout sequence for the
`internal/audit/reconciler/` package -- the replay-only
restart sweep that closes the Stage 9.1 durability loop.
Architecture Sec 7.10 / iter 1 evaluator item 11.

### What's new

1. **`internal/audit/reconciler/` package** -- on service
   restart, walks `data/wal/audit/`, verifies every
   frame's signature, and replays MISSING rows into the
   three Audit tables via
   `INSERT ... ON CONFLICT (<pk>) DO NOTHING`.
2. **`composition.NewWALReconciler(ctx, WALReconcilerConfig)`**
   factory + `composition.NewHistoricalKeysWALVerifier`
   adapter -- the production wiring point for the
   reconciler. Returns `(nil, nil)` for scaffold-mode
   (nil `KeyStore`) so the binary branches on
   "reconciler disabled" deliberately. The verifier
   pins a `[]keys.KeyRecord` snapshot from the
   `clean_code.policy_signing_keys` table at
   construction time, so RETIRED-but-known keys still
   verify legitimate historical frames.
3. **Binary on-restart wiring (this stage)** -- both
   `cmd/clean-code-eval-gate` and
   `cmd/clean-code-gateway` now run the reconciler as a
   BLOCKING startup step BEFORE the HTTP listener
   accepts traffic. New env var
   `CLEAN_CODE_WAL_RECONCILER_DSN` is REQUIRED whenever
   signing keys are configured.
4. **Four pinned invariants** (architecture Sec 7.10):
   - NEVER inserts a row whose `(table, row_pk)` already
     exists.
   - NEVER deletes a row.
   - NEVER modifies a non-Audit table.
   - PRESERVES `evaluation_run.caller` verbatim from the
     original frame.
5. **Phased replay** -- pass 1 every `evaluation_run`
   frame, pass 2 every `evaluation_verdict` + `finding`
   frame. FK ordering is honoured even on a corrupted
   partition that has reordered frames.

### What is NOT in Stage 9.2

- **Retention sweep** -- the reconciler does not delete
  WAL partition files. Disk-capacity planning still
  rests on the operator (see Stage 9.1 rollout).
- **Continuous-replay / interval timer** -- the
  reconciler runs ONCE at startup and exits. A
  long-lived process repeating the sweep on a cadence
  is a future enhancement and is out of scope here.

### Pre-rollout: confirm role grants

Verify `clean_code_wal_reconciler` exists with the right
posture:

```sql
\du clean_code_wal_reconciler
-- Expect: the role exists, attribute set is `NOLOGIN` (the
--         role is assumed via `SET ROLE` after the
--         CLEAN_CODE_WAL_RECONCILER_DSN's auth user logs in
--         -- see "DSN connection pattern" below).

SELECT grantee, privilege_type, table_name
  FROM information_schema.role_table_grants
 WHERE grantee = 'clean_code_wal_reconciler'
   AND table_schema = 'clean_code'
 ORDER BY table_name, privilege_type;
-- Expect: INSERT, SELECT on each of evaluation_run,
--         evaluation_verdict, finding. UPDATE and DELETE
--         MUST be absent. SELECT on
--         policy_signing_keys must be present (the
--         historical-keys verifier reads this table at
--         construction time).
```

If `UPDATE` or `DELETE` appears for this role on ANY of
the three Audit tables, STOP -- the role posture is
broken and the reconciler's "never deletes, never
modifies a non-Audit table" invariants degrade to
"depend on the application layer". Re-apply migration
`0004_roles.up.sql`. If SELECT on
`clean_code.policy_signing_keys` is missing, re-apply
migration `0005_policy_signing_keys.up.sql`.

### DSN connection pattern (NOLOGIN + `SET ROLE`)

`clean_code_wal_reconciler` is created as `NOLOGIN` by
migration `0004_roles.up.sql:191` -- the same posture
the existing `clean_code_evaluator` and
`clean_code_solid_batch` roles use. The DSN therefore
cannot connect AS `clean_code_wal_reconciler`
directly; instead, the DSN's auth user is a
deployment-scoped LOGIN role (typically the same
`clean_code_app` / `clean_code_runtime` login the
gateway already uses) that has been granted
membership in `clean_code_wal_reconciler` via
`GRANT clean_code_wal_reconciler TO <login-role>`,
and the connection assumes the role via either:

- **`options='-c role=clean_code_wal_reconciler'`** in
  the libpq connect string -- the cleanest pattern:
  PostgreSQL issues an implicit `SET ROLE` on
  connection establishment, so every statement the
  reconciler runs is attributed to
  `clean_code_wal_reconciler` in `pg_stat_activity`
  and PG audit logs without any application-side
  bookkeeping. This is what the gateway's
  `runWALReconciler` pool expects.
- **`SET ROLE clean_code_wal_reconciler`** issued as
  the first statement on every checked-out
  connection -- equivalent semantics, but requires
  the application to remember to do it. The Stage 9.2
  binary wiring uses the `options=` form so this is
  not the active pattern.

Operators MUST grant the membership BEFORE the first
post-9.2 restart:

```sql
GRANT clean_code_wal_reconciler TO clean_code_app;
-- (replace `clean_code_app` with whatever LOGIN role
-- your existing CLEAN_CODE_PG_URL / EVALUATOR_PG_URL
-- DSNs already authenticate as)
```

Failing to grant the membership produces a
`permission denied to set role
"clean_code_wal_reconciler"` error at reconciler boot
-- the binary will refuse to start, which is the
correct fail-loud behaviour.

### Cutover (Stage 9.2 -- blocking on-restart sweep)

1. Confirm the env vars on every replica's start
   command / systemd unit / Kubernetes Deployment:
   - `CLEAN_CODE_AUDIT_WAL_DIR` -- the same directory
     the Stage 9.1 writer is using (default
     `data/wal/audit`).
   - `CLEAN_CODE_WAL_RECONCILER_DSN` -- PostgreSQL DSN
     whose auth user is a LOGIN role that has been
     granted `clean_code_wal_reconciler` membership,
     with `options=-c role=clean_code_wal_reconciler`
     (or equivalent `SET ROLE`) so statements are
     attributed to the reconciler role. This DSN is
     REQUIRED whenever the existing signing-key env
     var `CLEAN_CODE_KMS_PROVIDER` is set (see
     `cmd/clean-code-eval-gate/main.go:190-214` and
     `cmd/clean-code-gateway/main.go:230-231` for the
     production check). If the DSN is absent while
     `CLEAN_CODE_KMS_PROVIDER` is set, boot is
     refused.
2. Restart the affected replica. Watch the boot logs
   for the reconciler stanza. Expectations:
   - First the log line indicating reconciler start
     (`WAL reconciler: starting blocking on-restart
     sweep ...`).
   - Then the Stats summary on completion:
     - `Replayed.Total() = N` where `N` is the count of
       WAL-on-disk-but-PG-missing rows (usually 0 in a
       clean shutdown; positive after a fsync-fail
       incident).
     - `SkippedExisting.Total() = M` where `M` is the
       count of WAL-on-disk-AND-PG-present rows
       (usually the entire WAL contents minus N).
     - `SkippedBadSig.Total() = 0` -- ANY non-zero
       value pages on-call.
     - `SkippedBadShape.Total() = 0` -- ANY non-zero
       value pages on-call. (Post-signature
       schema-drift / RowPK-mismatch failures abort
       `Run` instead -- see step 4.)
     - `Warnings` empty (or 1 trailing-partial entry if
       the last partition was mid-write at shutdown).
3. Wait for the binary to advance past the reconciler
   stanza into the rest of the boot sequence (rule
   engine wiring, HTTP listener). If the binary exits
   instead, see step 4.
4. **If the binary exits with an error mentioning
   `decode failed AFTER valid signature`,
   `ErrRowPKMismatch`, or `KeyStore.List` /
   snapshot-fetch failures:** STOP. Do NOT retry blindly.
   The first two indicate writer-side schema drift OR
   signing-key compromise; quarantine the partition,
   page on-call, and audit recent Policy Steward
   key-rotation events. The third indicates the
   reconciler could not load the historical-keys
   snapshot (DSN unreachable or role grant missing) --
   verify `\dp clean_code.policy_signing_keys` shows
   SELECT for `clean_code_wal_reconciler`. See
   `docs/runbook.md` Stage 9.2 -> "Operator checklist"
   for the full triage flow.

### Roll-back

Stage 9.2 is replay-only and additive. There is nothing
to roll back at the data layer -- the reconciler's
inserts are no-ops on existing rows, and a botched run
leaves the WAL bytes durable for retry. To "roll back"
the rollout itself: unset
`CLEAN_CODE_WAL_RECONCILER_DSN` AND the signing-key env
vars on the affected replica. Boot will then skip the
reconciler (scaffold-mode branch). Any WAL frames
already on disk remain durable for a future run.

## Stage 9.1: Audit WAL frame writer

This subsection captures the rollout sequence for the
`internal/audit/wal/` package -- the per-process WAL writer
scoped EXCLUSIVELY to the three Audit tables
(`evaluation_run`, `evaluation_verdict`, `finding`).
Architecture Sec 7.10 / tech-spec Sec 4.13.

### What's new

1. **`internal/audit/wal/` package** -- `Writer`, `TxBatch`,
   `Signer`, `AuditFrame`, `ReadAll`/`ReadPartition`,
   `MaxFrameSize = 1<<20`, and the
   `ErrTrailingPartialFrame` / `ErrFrameSizeExceeded`
   sentinel taxonomy.
2. **WAL writer REQUIRED at both audit-write Stores** --
   `rule_engine.NewSQLStore` and
   `evaluator.NewSQLDegradedRunStore` BOTH error if
   `WalWriter` is nil (`internal/rule_engine/sql_store.go`
   constructor, `internal/evaluator/sql_degraded_store.go`
   constructor). This is the row+WAL atomicity guard
   demanded by the brief: there is NO SQL-only fallback
   for Audit-table writes. The two writers stage WAL
   frames in a per-tx batch and call `batch.Commit(ctx)`
   BEFORE `tx.Commit()`; a WAL fsync failure rolls back
   the SQL transaction.
3. **Production composition wires the writer** --
   `cmd/clean-code-eval-gate/main.go` and
   `cmd/clean-code-gateway/main.go` read
   `CLEAN_CODE_AUDIT_WAL_DIR` (default `data/wal/audit`)
   at startup, construct a `wal.Writer`, and pass it to
   `composition.BuildEvalGate` /
   `evaluator.NewProductionGate` /
   `rule_engine.NewSQLStore`. Both
   `composition.EvalGateConfig` and
   `evaluator.ProductionGateConfig` REQUIRE the field
   and error at construction time when it is nil
   (`TestBuildEvalGate_RejectsNilWalWriter`,
   `TestSQLDegradedRunStore_RejectsNilWalWriter`).
4. **Conformance enforcement** -- `test/conformance/wal_scope_test.go`
   pins the closed allow-list of importers.
5. **Integration tests prove frame emission** --
   `internal/rule_engine/sql_store_wal_test.go` and
   `internal/evaluator/sql_degraded_store_wal_test.go`
   exercise sqlmock + real `wal.Writer`, asserting the
   happy-path frame triple, the signer-failure rollback
   contract, AND the write/fsync-failure rollback contract
   (`TestSQLStore_AppendEvaluation_WALFlushFailureRollsBackSQL`,
   `TestSQLDegradedRunStore_AppendDegradedRun_WALFlushFailureRollsBackSQL`).
6. **Production signer is `policy/keys`-backed when KMS is
   wired** -- `cmd/clean-code-eval-gate/main.go` and
   `cmd/clean-code-gateway/main.go` construct the writer's
   signer via `composition.NewKeysManagerWALSigner(*keys.Manager)`
   when the signing keys manager is non-nil. The 2-phase
   callback contract (keyID emitted into the canonical
   payload BEFORE signing) is satisfied by
   `keys.Manager.SignActive(ctx, build)`. Frames signed by
   the KMS path carry a non-zero `signing_key_id` and a
   real Ed25519 signature verifiable via `keys.Manager.Verify`.
7. **No kill-switch** -- there is NO `CLEAN_CODE_AUDIT_WAL_DISABLED`
   env var, NO "audit WAL off" feature flag, and NO SQL-only
   fallback path for Audit-table writes. Unsetting
   `CLEAN_CODE_AUDIT_WAL_DIR` does NOT disable the writer; it
   falls through to the default `data/wal/audit` directory.
   The two Audit-store constructors error on nil `WalWriter`
   and the composition root refuses to build the eval-gate
   without one.

### What is NOT in Stage 9.1

The Stage 9.2 brief covers the **reconciler** (replay of
speculative frames against PostgreSQL keyed on
`(table, row_pk)`, signature verification via the policy KMS
handle, quarantine of unverifiable frames). Stage 9.1 stops
at the writer; reconciler enablement is deferred.

### Pre-rollout: provision the WAL volume

1. Pick a dedicated mount for `data/wal/audit/` with at least
   2 GiB free (per-process). The writer creates the
   directory at startup with mode `0o755` -- the parent
   must be writable by the binary's user.
2. Set the env knob (composition root reads it during
   wiring): `CLEAN_CODE_AUDIT_WAL_DIR=/srv/clean-code/data/wal/audit`.
3. Mount the volume with `data=ordered` (ext4) or
   `data_journal=writeback` is NOT sufficient -- the WAL
   contract depends on `fsync(2)` durability semantics
   typical of `data=ordered` and `xfs` defaults.

### Pre-rollout: confirm WAL signer scope

Stage 9.1 supports TWO signer wirings, chosen at startup
based on whether the `policy/keys` signing manager was
constructed (driven by the binary's KMS/keystore env
variables -- see `cmd/clean-code-eval-gate/main.go` and
`cmd/clean-code-gateway/main.go`):

1. **Production (KMS wired)** -- the binary calls
   `composition.NewKeysManagerWALSigner(*keys.Manager)`
   which adapts `keys.Manager.SignActive(ctx, build)` to
   the writer's 2-phase `wal.Signer.SignFrame(ctx, build)`
   callback contract. The keyID is emitted into the
   canonical payload BEFORE signing, so the on-disk
   `signing_key_id` field is bound to the bytes the KMS
   actually signed. Frames are verifiable via
   `keys.Manager.Verify(keyID, payload, sig)`. The Stage
   9.2 reconciler will use this verifier to gate replay
   eligibility.
2. **Scaffold (KMS unset)** -- the binary falls back to
   `wal.NoopSigner{}` (SHA-256 stand-in, zero
   `signing_key_id`) and emits a loud `WARN` log at
   startup. This is intended for short-lived dev/test
   bring-up while KMS provisioning catches up; operators
   MUST NOT treat scaffold-mode frames as
   cryptographically authoritative. Audit any
   scaffold-mode partition files before the Stage 9.2
   reconciler ships -- they will land in the
   quarantine queue.

The frame format itself is stable across both wirings
(`signing_key_id` + `signature` fields are populated in
both paths), so partition files written under scaffold
mode remain readable by the production reader and the
Stage 9.2 reconciler.

### Pre-rollout: confirm role grants

Audit table INSERTs are made by the existing
`clean_code_evaluator` (degraded path) and
`clean_code_solid_batch` (happy path) roles. Stage 9.1 does
NOT add a new role; the WAL writes are local-disk only.
A future migration may add `clean_code_wal_reconciler` for
the Stage 9.2 replay path.

### Cutover

1. **Deploy the new binary** with the audit WAL volume
   mounted and `CLEAN_CODE_AUDIT_WAL_DIR` set.
   `cmd/clean-code-eval-gate/main.go` and
   `cmd/clean-code-gateway/main.go` read the env var
   (default `data/wal/audit` relative to the binary's
   cwd if unset) and pass the constructed `wal.Writer`
   into `composition.BuildEvalGate`,
   `evaluator.NewProductionGate`, and
   `rule_engine.NewSQLStore`. A misconfigured
   `WalWriter` causes the binary to fail to start --
   this is intentional, the row+WAL atomicity guarantee
   has no SQL-only fallback.
2. **Tail the WAL volume** for the first 5 minutes of
   production traffic. Expect at most one partition file
   per UTC day. Per-frame size is well under the 1 MiB cap
   for typical finding rows; a `frame_size_exceeded`
   error in the binary logs indicates a malformed audit
   row (the SQL transaction will have rolled back, so
   PostgreSQL is uncorrupted).
3. **Confirm SQL writes still succeed** -- WAL fsync
   failures cause the SQL transaction to roll back. If
   the WAL volume is misconfigured (read-only mount,
   wrong owner), audit writes will hard-fail with
   `WAL flush before SQL commit` errors. Roll back the
   deploy and re-verify the volume.

### Roll-back

Audit-table writes REQUIRE a WAL writer in Stage 9.1 --
the two store constructors error on nil `WalWriter`,
and the composition roots refuse to build the eval-gate
without it. There is NO env-var kill-switch and no
SQL-only fallback path. To roll back:

1. Re-deploy the prior binary (the one without the
   WAL-required constructor guard) -- the partition
   files written by the new build remain readable by
   the Stage 9.2 reconciler when it ships.
2. If the WAL volume is genuinely broken (read-only
   mount, wrong owner) and the prior binary is
   unavailable, point `CLEAN_CODE_AUDIT_WAL_DIR` at a
   writable scratch directory to unblock startup; the
   Stage 9.2 reconciler will pick up the rerouted
   partition files.

## Stage 8.2: Refactor plan and task generation

This subsection captures the rollout sequence for the Stage
8.2 extension to `internal/refactor/`: the `TaskPlanner`
orchestrator emitting `refactor_plan` and `refactor_task`
rows. Stage 8.2 INTRODUCES the new one-shot K8s Job binary
`cmd/clean-code-refactor-planner` (NOT a cadence loop -- one
invocation per `(repo_id, sha)`), which composes the
Stage 8.1 `refactor.Planner.Plan` pass and the Stage 8.2
`refactor.TaskPlanner.PlanFromSnapshot` pass into a single
race-safe two-pass run pinned to one `policy_version_id`.
See "Step 2: Roll out the binary" below for the env-var
contract and the two-pass execution flow.

### What's new

1. **`internal/refactor/task_planner.go`** -- the Stage 8.2
   contract surface: `TaskPlanner`, `RefactorPlan`,
   `RefactorTask`, `TaskKind` + the closed five-value
   canonical enum, `FindingDetailReader`,
   `RefactorPlanTaskWriter`, in-memory + SQL
   implementations.
2. **`steward.RefactorWeights.TopN`** -- new optional
   `int` field with `json:"top_n,omitempty"`. Zero =
   no truncation; positive = top-N truncation; negative
   rejected at publish time. Legacy policies (no `top_n`
   key in the JSONB blob) deserialize to `TopN = 0` and
   carry the no-truncation semantics, so NO migration
   step is required for existing rows.
3. **`refactor_task_kind` ENUM** -- already created by
   migration `0003_policy_audit_refactor.up.sql:140`
   during Stage 5.1. Stage 8.2 only EMITS the five values
   the migration already declares; no new migration step is
   required.

### Pre-rollout: confirm role grants (no change needed)

The DB role `clean_code_refactor_planner` already has
`INSERT, SELECT` on `refactor_plan` and `refactor_task` per
migration `0004_roles.up.sql:482-509`. Stage 8.2 emits
to those same tables, so NO new role grant is required.
Verify with:

```sql
SELECT grantee, table_name, privilege_type
  FROM information_schema.role_table_grants
 WHERE table_schema = 'clean_code'
   AND table_name IN ('refactor_plan', 'refactor_task')
   AND grantee = 'clean_code_refactor_planner'
 ORDER BY table_name, privilege_type;
```

Expect exactly four rows: `(INSERT, refactor_plan)`,
`(SELECT, refactor_plan)`, `(INSERT, refactor_task)`,
`(SELECT, refactor_task)`. NO `UPDATE`, NO `DELETE` --
both tables are append-only.

### Step 1: Republish active policies with `top_n`

Stage 8.2 reads `policy_version.refactor_weights.top_n`. For
a brand-new deployment, the default is `0` (no truncation)
and operators may skip this step. For a production tier
already running Stage 8.1, decide whether plan coverage
should be truncated:

- Small repos / pilot environments: keep `top_n: 0`. Every
  scored hot_spot lands in the plan; useful for surfacing
  the long tail.
- Large repos / triage-mode: set `top_n: 25` (suggested
  default). The plan covers the 25 highest-score hot_spots
  per `(repo_id, sha)`; the long tail still persists in
  `hot_spot` for future re-planning.

Republish via the steward `policy.publish` + `policy.activate`
verbs; the migration is signature-stable because `top_n` is
a JSONB key with `omitempty` (so the legacy-row canonical
bytes are unchanged when `top_n == 0`).

### Step 2: Roll out the binary

Stage 8.2 ships the NEW `cmd/clean-code-refactor-planner`
binary, a one-shot Kubernetes Job (NOT a cadence loop) that
runs ONCE per `(repo_id, sha)` and exits. The operator wires
it into the existing scan-completion pipeline so a fresh
scan triggers a fresh refactor pass.

#### Required environment

| env var                                  | required | purpose                                                                                  |
| ---------------------------------------- | -------- | ---------------------------------------------------------------------------------------- |
| `CLEAN_CODE_PG_URL`                      | yes      | libpq DSN to the clean_code database. Connects under the `clean_code_refactor_planner` role. |
| `CLEAN_CODE_REFACTOR_PLANNER_REPO_ID`    | yes      | UUID of the repo to plan. Zero / malformed / missing fail fast at startup.               |
| `CLEAN_CODE_REFACTOR_PLANNER_SHA`        | yes      | Commit SHA to plan against. Empty / whitespace fail fast at startup.                     |
| `CLEAN_CODE_DISABLE_REFACTOR_PLANNER`    | no       | Truthy (`1`/`true`/`yes`/`on`) skips both passes and serves `/healthz` only. Default false. Used during staging rollouts that lack the hot_spot / refactor_plan / refactor_task schema. |
| `PORT`                                   | no       | Health/metrics listener port. Default `8080`.                                            |

The K8s Job spec MUST gate uniqueness on `(repo_id, sha)`
(single-writer assumption -- see runbook "Single-writer
assumption"). Two concurrent jobs against the same
`(repo_id, sha)` could race the
`WHERE created_at = (SELECT MAX(created_at) ...)` Stage 8.2
latest-batch lookup and emit a torn plan.

#### Two-pass execution flow

Each invocation of the binary performs TWO passes in order
(both pinned to the SAME `policy_version_id`):

1. Stage 8.1 `refactor.Planner.Plan(ctx, repo_id, sha)` --
   reads the active policy + metric_sample + finding rows,
   scores composite hot_spots, and writes the full
   `clean_code.hot_spot` batch.
2. Stage 8.2 `refactor.TaskPlanner.PlanFromSnapshot(ctx,
   repo_id, sha, planRes.Snapshot)` -- reads the top-N rows
   back from `clean_code.hot_spot` (latest batch by
   `created_at`, filtered to the same `policy_version_id`),
   reads qualifying finding details for those scopes, and
   writes ONE `refactor_plan` row + N `refactor_task` rows
   in a SINGLE transaction.

The `PlanFromSnapshot` entrypoint (rather than the
standalone `TaskPlanner.Plan`) closes the policy-activate
race the rubber-duck design review surfaced: a concurrent
`policy.activate` between the two passes cannot produce a
torn plan whose hot_spots were scored by PV-A and whose
top-N truncation came from PV-B.

### Step 3: Smoke-test post-rollout

Trigger a planner run against a known (repo_id, sha) and
verify both tables landed atomically:

```sql
SELECT plan_id,
       jsonb_array_length(hotspot_ids) AS coverage,
       (SELECT count(*)
          FROM clean_code.refactor_task t
         WHERE t.plan_id = p.plan_id) AS task_count
  FROM clean_code.refactor_plan p
 WHERE p.repo_id = $1 AND p.sha = $2
 ORDER BY p.created_at DESC
 LIMIT 1;
```

Expect:

- `coverage` ≤ `top_n` (or = full hot_spot count when
  `top_n == 0`).
- `task_count` ≥ 0 (zero is legal for metric-only signals).
- The task kinds you SELECT back are members of
  `('split_class','extract_method','invert_dependency',
  'break_cycle','consolidate_duplication')` -- the ENUM
  type itself rejects any other value.

### Rollback

Revert the binary. The Stage 8.2 rows that already landed
remain (the tables are append-only). To remove them
operationally, take an audit-tracked retraction via the
forthcoming `refactor.retract` verb (not in Stage 8.2 --
Stage 8.4 owns it).

#### Emergency stop -- halt all new plan / task emission

The CORRECT way to immediately halt new plan + task emission
is one of:

1. **Operator opt-out (recommended).** Set
   `CLEAN_CODE_DISABLE_REFACTOR_PLANNER=true` on the K8s Job
   spec and roll forward. The binary skips both Stage 8.1
   and Stage 8.2 passes, serves `/healthz` only, and exits 0
   when the Job is terminated. No new hot_spot, refactor_plan,
   or refactor_task rows land.
2. **Deactivate the active policy.** Issue `policy.activate`
   with no active row (or revoke the current activation) so
   `steward.ActivePolicyVersion` returns `(false, nil)`. The
   Stage 8.1 pass surfaces `ErrNoActivePolicy`, the binary
   logs the warning and exits cleanly; Stage 8.2 is skipped.
3. **Suspend the K8s CronJob / event consumer.** If the
   binary is triggered by a scan-completion CronJob,
   suspending the CronJob halts future runs without touching
   the policy or the binary.

> **DO NOT** rely on setting
> `policy_version.refactor_weights.top_n = 0` as an
> emergency stop. Per the documented semantics, `top_n == 0`
> means "no truncation -- plan covers EVERY scored hot_spot",
> not "no plan". Republishing with `top_n = 0` would INCREASE
> the per-(repo, sha) plan / task volume, the opposite of an
> emergency stop. The valid use of `top_n = 0` is the
> deliberate "include every hot_spot in the plan" choice
> documented in Step 1.


## Stage 7.1: Cross-Repo Aggregator cadence loop

This subsection captures the rollout sequence for the
`internal/aggregator/` package (architecture Sec 3.10 /
Sec 5.2.4 - Sec 5.2.6, tech-spec Sec 8.2
`aggregator_cadence=15min`). Stage 7.1 introduces ONE new
binary path under `cmd/clean-code-aggregator/` and ONE new
process per environment.

### What's new

1. **`internal/aggregator/` package** -- new package carrying
   `Aggregator` (Tick logic), `Loop` (cadence driver),
   `PGSampleSource` (read path), `PGSnapshotWriter` (write
   path). All three snapshot tables are populated by a SINGLE
   process per environment.
2. **`CLEAN_CODE_AGGREGATOR_CADENCE`** (Go duration, default
   `15m`) and **`CLEAN_CODE_DISABLE_AGGREGATOR`** (bool,
   default `false`) -- new env knobs in
   `internal/config/config.go`.

### Pre-rollout: confirm role grants

Run as a DB superuser BEFORE rolling out the binary. The
filter on `privilege_type IN ('INSERT','UPDATE','DELETE')` is
LOAD-BEARING -- migration `0004_roles.up.sql:227-260` grants
broad `SELECT` on the snapshot tables to nearly every role
(reader access for Insights / Evaluator / Refactor Planner /
etc.), so an UNFILTERED query would return many roles and the
STOP guidance below would be unactionable. Only INSERT /
UPDATE / DELETE on the snapshot tables identifies the
sole-writer surface this rollout depends on, and matches the
post-rollout validation query in `runbook.md` "Snapshot table
writer identity".

```sql
SELECT grantee, privilege_type
  FROM information_schema.role_table_grants
 WHERE table_schema = 'clean_code'
   AND table_name IN ('repo_metric_snapshot',
                      'cross_repo_percentile',
                      'portfolio_snapshot')
   AND privilege_type IN ('INSERT', 'UPDATE', 'DELETE')
 ORDER BY table_name, grantee, privilege_type;
```

Expected output: EXACTLY three rows, one per snapshot table,
all of the form
`('clean_code_xrepo_aggregator', 'INSERT')`. No `UPDATE` or
`DELETE` row should appear (migration `0004_roles.up.sql`
lines 416-418 explicitly `REVOKE UPDATE, DELETE` on all three
tables, per the G6 row-immutability invariant), and no other
`grantee` should appear with INSERT/UPDATE/DELETE on these
three tables. If ANY of those conditions are violated,
**STOP** and consult Phase 1.5 sub-store grant policy
(migration `0004_roles.up.sql`).

### Wiring step (composition root)

The `cmd/clean-code-aggregator/main.go` composition root constructs:

```go
src, _ := aggregator.NewPGSampleSource(db)
w, _   := aggregator.NewPGSnapshotWriter(db)
agg, _ := aggregator.NewAggregator(src, w)
loop   := aggregator.NewLoop(agg,
    aggregator.WithLoopCadence(cfg.AggregatorCadence),
    aggregator.WithLoopLogger(log),
)
go func() { _ = loop.Run(ctx) }()
```

Connect to the database AS the `clean_code_xrepo_aggregator`
role (NOT `clean_code_management` or `clean_code_ingestor`).
Per migration `0004_roles.up.sql:392-418` the role's grant
matrix is:

| Table                    | Aggregator grants    | Notes                                                                |
| ------------------------ | -------------------- | -------------------------------------------------------------------- |
| `metric_sample`          | `INSERT, SELECT`     | System-tier carve-out (tech-spec Sec 7.2 lines 1348-1364): the       |
|                          |                      | aggregator writes `pack='system' AND source='derived'` rows for      |
|                          |                      | composed metrics (xrepo-dep-depth, arch-debt-ratio, etc.). The       |
|                          |                      | `pack='system'` filter is enforced by the application-layer writer   |
|                          |                      | (architecture Sec 3.10), NOT by table-level ACL.                     |
| `metric_retraction`      | `INSERT, SELECT`     | Retraction writer for system-tier rows the aggregator emitted.       |
| `metric_sample_active`   | `INSERT, SELECT, UPDATE` | Pointer-update for system-tier active rows; only the aggregator  |
|                          |                      | and the metric_ingestor have `UPDATE` here.                          |
| `repo_metric_snapshot`   | `INSERT, SELECT`     | **SOLE writer** -- no other role has `INSERT`. `UPDATE` and          |
|                          |                      | `DELETE` are explicitly `REVOKE`d (G6 row-immutability invariant).   |
| `cross_repo_percentile`  | `INSERT, SELECT`     | Same: SOLE writer, UPDATE/DELETE revoked.                            |
| `portfolio_snapshot`     | `INSERT, SELECT`     | Same: SOLE writer, UPDATE/DELETE revoked.                            |

What this means for code drift:

  - An accidental `INSERT INTO metric_sample` from the aggregator
    binary WILL succeed at the PG ACL layer; the application-layer
    `pack='system'` filter is the gate that protects the
    metric_ingestor's foundation-tier write surface
    (`pack IN ('base','solid','ingested')`; `'foundation'` is the
    TIER label that contains those three packs, not a pack value
    itself -- see tech-spec Sec 7.2 lines 1212-1248). Code review
    + the `aggregator-is-sole-writer` role isolation test
    (`internal/storage/roles_test.go`) are the safety nets.
  - An accidental `INSERT INTO repo_metric_snapshot` from ANY OTHER
    role (`clean_code_management`, `clean_code_metric_ingestor`,
    `clean_code_repo_indexer`, etc.) WILL fail at the PG ACL layer
    -- the sole-writer invariant is enforced by the role grants,
    not by an application-layer check. Same for
    `cross_repo_percentile` and `portfolio_snapshot`.
  - An accidental `UPDATE` or `DELETE` from the aggregator role
    against any of the three snapshot tables WILL fail at the PG
    ACL layer (G6 row immutability is grant-enforced).

### Single-replica invariant

Run EXACTLY ONE aggregator process per environment. A second
replica producing snapshot rows at the same cadence would
double the row count and confuse downstream readers that
JOIN to `MAX(built_at)`. The architecture G1 sub-store grant
ensures correctness at the role layer (only one role can
write), but the single-replica invariant must be enforced at
the orchestration layer (Kubernetes `Deployment.replicas=1`
with `strategy.type: Recreate`, or equivalent).

### Smoke validation

After the first tick (within `aggregator_cadence`, default
15 min):

```sql
-- Confirm at least one tick landed:
SELECT COUNT(*), MAX(built_at)
  FROM clean_code.repo_metric_snapshot;
SELECT COUNT(*), MAX(built_at)
  FROM clean_code.cross_repo_percentile;
SELECT COUNT(*), MAX(built_at)
  FROM clean_code.portfolio_snapshot;
```

All three `MAX(built_at)` values MUST be EQUAL (within the
single-transaction write window of one tick). A divergence
indicates either a manual write to one of the three tables
(policy violation) or a partial-COMMIT bug in the writer
(file a regression and roll back).

### Rollback

To roll back the aggregator without losing already-written
snapshot rows: set `CLEAN_CODE_DISABLE_AGGREGATOR=true`,
restart the binary, and confirm `loop: stopped` appears in
the structured log. Snapshot rows already in the three
tables remain (they are append-only history).

## Stage 7.3: Insights percentile freshness banner

This subsection captures the rollout sequence for the
percentile-freshness banner attached to the Management
latest-dashboard read verbs `mgmt.read.cross_repo` and
`mgmt.read.portfolio` (architecture Sec 6.3, tech-spec
Sec 8.2 `freshness_window_seconds=3600`). Stage 7.3
introduces NO new binary, NO new env var, NO migration --
the banner is a behavior-pinning iteration of the existing
Reader code path in
`internal/management/insights/freshness.go` +
`internal/management/reader.go`.

### What's new

1. **`internal/management/insights/freshness.go`** -- new
   sub-package owning the `Freshness`,
   `Status`, `Clock`, `SystemClock` types plus the
   exported `FreshnessWindowSeconds = 3600` constant and
   the canonical
   `DegradedReasonPercentileStale = "percentile_stale"`
   string.
2. **`management.WithInsightsFreshness(*insights.Freshness)`**
   Reader option -- the composition-root wire-up. When
   absent, the Reader auto-defaults to
   `insights.NewPercentileFreshness()` so a wiring slip
   cannot silently render stale snapshots as fresh
   (defence-in-depth contract pinned by
   `TestReader_ReadCrossRepo_*` /
   `TestReader_ReadPortfolio_*`).
3. **`management.WithoutFreshness()`** -- explicit opt-out
   for unit-test seams and developer-mode replay
   harnesses. NOT a production runtime knob and NOT a
   rollback lever; production composition roots MUST NOT
   call it (see runbook "Auto-default wiring").
4. **`internal/evaluator` gate-side rejection** --
   `verdict.IsValidForGate()`,
   `Gate.writeDegraded`, and
   `SQLDegradedRunStore.AppendDegradedRun` all return
   `ErrInvalidGateDegradedReason` when handed
   `percentile_stale`. This is the canonical realisation
   of the Stage 6.1 e2e scenario
   `percentile-stale-not-on-gate`.

### Pre-rollout: confirm aggregator is ticking

The freshness banner is meaningful only when the cross-repo
aggregator (Stage 7.1) is producing snapshots within the
3600s window. Confirm BEFORE rolling out a build that
mounts the Reader:

```sql
SELECT
  metric_kind,
  scope_kind,
  EXTRACT(EPOCH FROM (now() - MAX(built_at))) AS age_seconds,
  MAX(built_at) AS latest_built_at
FROM clean_code.cross_repo_percentile
GROUP BY metric_kind, scope_kind
ORDER BY age_seconds DESC;
```

Every row's `age_seconds` MUST be `< 3600` on a healthy
cluster. A row with `age_seconds > 3600` means the
post-rollout read will return
`degraded_reason='percentile_stale'` -- triage the
aggregator (Stage 7.1 runbook) BEFORE rolling out the
reader if the banner would mislead the dashboard.

### Wiring step (composition root)

**State of the read surface today.** The
`mgmt.read.cross_repo` and `mgmt.read.portfolio` HTTP
handlers are **not yet mounted in any production binary**
on this branch. A literal grep confirms it:

```sh
$ grep -rn 'management.NewReader' services/clean-code/cmd/
(no production hits -- only test-file constructions and
 doc comments)

$ grep -rn 'ReadCrossRepo\|ReadPortfolio' services/clean-code/cmd/
(empty)
```

Stage 7.3 itself ships ONLY the Reader-side library
behaviour (the freshness sub-package and the
`management.Reader` wire-up) plus the eval.gate-side
rejection carve-out. The HTTP exposure of the read verbs
(routing, auth, role grants) is a follow-on stage; that
stage is the one that introduces a live composition
root. Until then, the canonical wire shape is the one
pinned by the package tests:

- `internal/management/reader_test.go:609` --
  `NewReader(nil, WithMetricsBackend(fb), WithInsightsFreshness(fresh))`
- `internal/management/reader_test.go:716-733` -- the
  defence-in-depth proof that `NewReader(...)` **without**
  `WithInsightsFreshness` auto-defaults to
  `insights.NewPercentileFreshness()`.

**Recommended wire-up when the read-surface stage lands.**
The follow-on stage should mount the read verbs from a
**sibling** helper to `mountMgmtRoutes` -- not by
extending `mountMgmtRoutes` itself, because the latter
runs under the `clean_code_management` Postgres role (see
`migrations/0004_roles.up.sql`) and intentionally splits
its `*sql.DB` handles between the ingestor-role
write path and the mgmt-role audit log.

The Reader's actual constructor signature (file
`internal/management/reader.go:514`) is:

```go
func NewReader(signingKeys *keys.Manager, opts ...ReaderOption) *Reader
```

-- the first positional argument is the Stage 5.1
`*keys.Manager`, NOT a `*PGRepoStore`. The
`MetricsBackend` (Stage 6.3) and `*insights.Freshness`
(Stage 7.3) are wired via the variadic options.
`NewPGMetricsBackend` (file
`internal/management/pg_metrics_backend.go:106`)
returns `(*PGMetricsBackend, error)` -- two values; the
error MUST be propagated. The Reader exposes
domain-level methods `ReadCrossRepo(ctx, metricKind,
scopeKind) (*CrossRepoResponse, error)` and
`ReadPortfolio(ctx, metricKind) (*PortfolioResponse, error)`
-- the read-surface stage owns the thin HTTP handler
shims that bridge `*http.Request -> Reader.ReadCrossRepo`
and write the JSON response.

A complete, compile-correct future wire-up sketch:

```go
// Future file (NOT shipped by Stage 7.3):
//   cmd/clean-code-mgmt-read/main.go OR a
//   mountMgmtReadRoutes sibling of mountMgmtRoutes
//   in cmd/clean-code-metric-ingestor/main.go.

import (
    "database/sql"
    "fmt"
    "net/http"

    "github.com/smartpcr/code-intelligence/services/clean-code/internal/management"
    "github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/keys"
)

func mountMgmtReadRoutes(
    mux *http.ServeMux,
    mgmtDB *sql.DB,
    km *keys.Manager, // Stage 5.1 signing-key manager
) error {
    if mgmtDB == nil {
        return fmt.Errorf("mountMgmtReadRoutes: mgmtDB is nil")
    }
    backend, err := management.NewPGMetricsBackend(mgmtDB)
    if err != nil {
        return fmt.Errorf("mountMgmtReadRoutes: backend: %w", err)
    }
    reader := management.NewReader(km,
        management.WithMetricsBackend(backend),
        // WithInsightsFreshness is OMITTED on purpose --
        // NewReader auto-defaults to
        // insights.NewPercentileFreshness() so a wiring
        // slip cannot silently render stale percentiles
        // as fresh (see reader.go:521 auto-default
        // branch).
    )
    // The thin HTTP shims that adapt *http.Request to
    // reader.ReadCrossRepo / reader.ReadPortfolio are
    // owned by the follow-on read-surface stage. They
    // are conventionally named handleReadCrossRepo /
    // handleReadPortfolio and live next to the
    // existing mgmt_verbs.go handlers. Stage 7.3 does
    // NOT ship those shims (the Reader exposes only the
    // domain-level methods today; see
    // reader.go:745 ReadCrossRepo and
    // reader.go:792 ReadPortfolio).
    _ = reader
    return nil
}
```

What Stage 7.3 **DOES** pin verbatim about the
construction shape (the parts the follow-on stage MUST
honour to keep the freshness-banner contract intact):

1. `NewReader`'s first positional arg is the signing-key
   manager (`*keys.Manager`). Tests stub this with
   `nil`; production wires the Stage 5.1 manager.
2. `WithMetricsBackend(backend)` wires the metrics
   backend (Stage 6.3 PG-backed implementation,
   Stage 6.3 in-memory test fake, or any future
   `MetricsBackend` implementation).
3. Omitting `WithInsightsFreshness` is SAFE and
   PREFERRED in production -- the auto-default at
   `reader.go:521` wires the canonical 3600s window.
4. Explicitly passing `WithoutFreshness()` SUPPRESSES
   the freshness banner. It exists ONLY for unit-test
   seams; a code review on a production composition
   root calling `WithoutFreshness()` SHOULD block the
   merge.

The Reader-construction shape is verified by the
package tests at `internal/management/reader_test.go:609`
(`NewReader(nil, WithMetricsBackend(fb), WithInsightsFreshness(fresh))`)
and `:716-733` (the defence-in-depth proof that
`NewReader(...)` **without** `WithInsightsFreshness`
auto-defaults).

To **explicitly** suppress the freshness banner -- which
should only happen in a unit-test seam, NEVER in a
production composition root -- the caller MUST opt out
with `management.WithoutFreshness()`. A code review
flagged on a production composition root calling
`WithoutFreshness()` SHOULD block the merge.

### No new environment variables

Stage 7.3 introduces no env vars. The freshness window is
the package-level constant
`insights.FreshnessWindowSeconds = 3600`; changing it
requires a tech-spec amendment (the literal is
explicitly pinned by `TestFreshnessWindowSecondsLiteral`
in `freshness_test.go`).

### No Postgres impact

No migration. No new role grant. The banner reads the
existing `cross_repo_percentile.built_at` and
`portfolio_snapshot.built_at` columns populated by the
Stage 7.1 aggregator -- both are non-nullable timestamps
already covered by the Stage 1.2 catalog migration set.

### Smoke validation

After deploying a build that mounts the Reader with the
banner wired:

1. `/healthz` returns 200.
2. Issue a `GET /v1/mgmt/read/cross_repo` for any
   `(metric_kind, scope_kind)` that the aggregator has
   composed. Expect `200` and a JSON body whose
   `"degraded"` field is `false`. `"degraded_reason"`
   MUST be ABSENT from the body (the field has
   `omitempty` and the empty string is omitted on the
   fresh path).
3. SIMULATE a stale read in a staging environment by
   temporarily setting
   `CLEAN_CODE_DISABLE_AGGREGATOR=true` on the
   aggregator deployment and waiting > 1 hour past the
   last tick. Re-issue the same `mgmt.read.cross_repo`
   call. Expect `200` with body
   `"degraded": true, "degraded_reason":
   "percentile_stale"`. Restore the aggregator and
   confirm the next call returns to `degraded=false`
   WITHOUT a binary restart.
4. SMOKE the gate-side rejection by attempting to write a
   degraded run with `degraded_reason="percentile_stale"`:

   ```sql
   SELECT clean_code.f_assert_gate_can_persist_reason(
     'percentile_stale'::clean_code.degraded_reason
   );
   ```

   If your deployment ships such a probe; otherwise the
   contract is asserted at the Go layer by the
   `verdict_test.go` suite which runs as part of
   `make test`.

### Rollback

The banner is purely additive on the response envelope. To
roll back the wiring without a code change, redeploy the
previous build (the banner field is `omitempty` on
fresh-path JSON, so any consumer that ignores
`degraded`/`degraded_reason` was already compatible).

If a real defect in the freshness comparison surfaces in
production:

1. The correct first response is to fix the AGGREGATOR
   (the most common cause of `percentile_stale` in
   practice -- see runbook "Operator triage on
   `percentile_stale`").
2. ONLY if the comparison itself is mis-classifying fresh
   rows as stale (a real implementation bug), file a
   regression and consider a hotfix that pins the wider
   `time.Duration` window via a custom
   `WithInsightsFreshness(&insights.Freshness{Window:
   2*time.Hour, Clock: insights.SystemClock{}})`
   composition. Do NOT reach for
   `management.WithoutFreshness()` as the rollback
   path -- suppressing the signal does not fix the bug
   and removes the dashboard's only staleness indicator.

### Backout sequence (rare)

If a Stage 7.3 hotfix turns out to be worse than the bug
it was fixing, revert the change in the composition root
(restore the previous `WithInsightsFreshness(...)` line)
and redeploy. The Reader's auto-default contract
guarantees a fresh-by-default behaviour even with the
option omitted -- there is no DB cleanup required and the
existing snapshot rows remain untouched.

## Stage 6.2: management write verbs and repo onboarding

This subsection captures the rollout sequence for the
Stage 6.2 verbs `mgmt.register_repo(repo_url, default_branch,
mode?)` and `mgmt.set_mode(repo_id, mode)`. It does NOT
introduce a new binary -- the existing
`clean-code-metric-ingestor` binary's HTTP server (the
binary that already hosts the Stage 3.4 management verbs
`mgmt.retract_sample` / `mgmt.rescan`) gains two new routes
via the unified `MgmtSurfaceRoutes(mgmt, policy)` composition
function. See `cmd/clean-code-metric-ingestor/main.go:mountMgmtRoutes`
for the production wiring.

### What's new at the HTTP layer

1. **`POST /v1/mgmt/register_repo`** (new). Writes the
   `clean_code.repo` row PLUS a
   `repo_event(kind='registered')`. Idempotent on `repo_url`.
2. **`POST /v1/mgmt/set_mode`** (new). Updates `repo.mode`
   PLUS appends `repo_event(kind='mode_changed')`. No-op when
   the new mode equals the existing mode.
3. **`MgmtSurfaceRoutes(mgmt, policy)`** (new). A top-level
   composition function that mounts all six canonical
   `mgmt.*` write verbs (`mgmt.override, mgmt.register_repo,
   mgmt.set_mode, mgmt.retract_sample, mgmt.rescan`) onto a
   single `*http.ServeMux`. Each path is gated on its
   backing writer being non-nil so the wire surface NEVER
   advertises an endpoint it cannot serve. Existing Stage
   3.4 callers that already mount `MgmtWriter.Routes()` and
   `Handler.Routes()` continue to work unchanged -- the
   new paths are conditionally mounted on those muxes too,
   so existing composition roots pick up the new verbs
   without re-routing.

### Wiring step (composition root)

The composition root (`cmd/clean-code-metric-ingestor/main.go`,
`mountMgmtRoutes`) wires a Postgres-backed `RepoStore` into
`MgmtWriter` via `WithMgmtWriterRepoStore`. The PG store
runs against the SAME `*sql.DB` as the existing audit-log
appender (both run under the `clean_code_management` Postgres
role and therefore inherit the column-level grants enumerated
in `migrations/0001_catalog_lifecycle.up.sql`).

```go
appender := management.NewPGRepoEventAppender(mgmtDB) // Stage 3.4
repoStore := management.NewPGRepoStore(mgmtDB)        // Stage 6.2 iter 2
writer := management.NewMgmtWriter(
    sampleResolver,
    retractDispatcher,
    rescanEnqueuer,
    appender,
    management.WithMgmtWriterRepoStore(repoStore),
)
policyWriter := management.NewPolicyWriter(...) // Stage 5.3
mux := management.MgmtSurfaceRoutes(writer, policyWriter)
```

A composition root that does NOT need overrides MAY pass
`nil` for the `policy` argument; the function will omit
`/v1/mgmt/override` from the mounted set. Likewise, omitting
the `WithMgmtWriterRepoStore` option (as iter 1 did) causes
`register_repo` / `set_mode` to be omitted from the mounted
set entirely; this is the safety-net path for legacy
composition roots that have not yet adopted Stage 6.2.

### `NewPGRepoStore` transactional shape (operator detail)

The `PGRepoStore` implementation in
`internal/management/pg_repo_store.go` opens ONE Postgres
transaction per write verb and commits BOTH the row mutation
AND the matching `repo_event` in the same transaction. This
is the production realisation of the Stage 6.2 atomicity
invariant documented in the runbook.

- `RegisterRepo` opens a transaction with
  `BEGIN`, takes
  `SELECT pg_advisory_xact_lock(hashtext($repo_url::text))`,
  performs a `SELECT repo_id, mode FROM clean_code.repo
  WHERE repo_url = $1` lookup INSIDE the lock, and either
  returns the existing `repo_id` (idempotent path,
  `created:false`, NO `registered` event written) or
  INSERTs both the `clean_code.repo` row AND the
  `repo_event(kind='registered')` payload before
  COMMITting. The advisory lock is xact-scoped so it
  auto-releases on COMMIT or ROLLBACK; different `repo_url`
  hashes do not block each other.

- `SetRepoMode` opens a transaction, takes
  `SELECT mode FROM clean_code.repo WHERE repo_id = $1 FOR
  UPDATE`, computes the transition, and either commits a
  no-op (same mode -- NO `mode_changed` event appended)
  or runs `UPDATE clean_code.repo SET mode=$2 WHERE
  repo_id=$1` AND `INSERT INTO clean_code.repo_event`
  before COMMITting. Unknown `repo_id` rolls back and
  returns the `ErrRepoStoreUnknownRepo` sentinel which the
  HTTP verb maps to HTTP 404.

If the event-append step fails inside either transaction,
the row mutation is rolled back. Operators can verify by
SELECTing on the `clean_code.repo_event` audit log: every
`registered`/`mode_changed` row implies a corresponding
catalog state.

### No new environment variables

Stage 6.2 introduces no new env vars. The PG store reads from
the same `*sql.DB` handle as Stage 3.4's appender (the
`CLEAN_CODE_MGMT_PG_URL` connection string the binary already
consumes).

### Postgres impact

No schema migration is required. Stage 6.2 reuses the
existing tables introduced by earlier migrations:

- `clean_code.repo` (Stage 1 migration `0001_catalog_lifecycle`,
  with the `repo_url` column added by migration
  `0006_repo_url`). The write-once `repo_url` invariant on the
  column is honoured by `mgmt.register_repo` (which writes
  the row exactly once) and respected by `mgmt.set_mode`
  (which only updates `repo.mode`).
- `clean_code.repo_event` (same migration). The
  `repo_event_kind` enum already includes both `registered`
  and `mode_changed` per Sec 5.1.4 lines 877-884.

### Rollback

Roll back by un-wiring the `RepoStore` from the composition
root and redeploying. The two new paths return HTTP 503,
existing mgmt verbs continue to serve, and no schema
migration needs to be reverted. Already-written `repo` rows
and `repo_event` rows remain in Postgres -- they are valid
audit history under the existing schema, so there is no
"data to clean up" before a partial rollback.

### Smoke test sequence

After deploying the new binary:

1. `POST /v1/mgmt/register_repo` with a new `repo_url`. Assert
   HTTP 200, `created:true`, and a `repo_id` in the
   response body. Query Postgres for the new row and the
   matching `repo_event(kind='registered')`.
2. `POST /v1/mgmt/register_repo` with the SAME `repo_url`.
   Assert HTTP 200, `created:false`, same `repo_id`. Query
   Postgres: exactly ONE `repo_event(kind='registered')` for
   that `repo_id` (idempotency invariant).
3. `POST /v1/mgmt/set_mode` with the `repo_id` from step 1
   and `mode:"linked"`. Assert HTTP 200, `changed:true`,
   `previous_mode:"embedded"`, `mode:"linked"`. Query
   Postgres: `repo.mode = 'linked'` AND one new
   `repo_event(kind='mode_changed')`.
4. Repeat step 3 with `mode:"linked"` (no-op). Assert HTTP
   200, `changed:false`. Query Postgres: no new
   `repo_event` row.
5. `POST /v1/mgmt/set_mode` with a random UUID. Assert HTTP
   404.

## Stage 6.1: `eval.gate` verb and synchronous SOLID delegation

This subsection captures the rollout sequence for the
Stage 6.1 verb `eval.gate(repo_id, sha, scope?)`. It does
NOT introduce a new binary -- the `clean-code-eval-gate`
binary delivered in Stage 5.7 iter 4 is reused, with two
behavioural changes documented below.

### Behavioural changes vs. Stage 5.7 iter 4

1. **Active-policy resolution is now mandatory.** The
   `/v1/eval/gate` route NO LONGER accepts a caller-supplied
   `policy_version_id`. The gate resolves the active
   `policy_version_id` itself via the latest
   `clean_code.policy_activation` row. A caller-supplied
   `policy_version_id` is REJECTED with HTTP 400 pointing
   the caller at the new `/v1/eval/replay` route.
2. **New admin route `/v1/eval/replay`.** Same JSON contract
   as the prior Stage 5.7 `/v1/eval/gate` body
   (`{repo_id, sha, policy_version_id, scope?}`). Provided
   so batch tooling and reconciler replays can still pin
   evaluations to a specific policy. MUST be exposed only
   on an admin-only network path.
3. **HTTP 409 on no active policy.** A `/v1/eval/gate` call
   against a repo whose policy has never been activated
   returns HTTP 409 (was 500 in Stage 5.7). No audit row is
   written -- `evaluation_run.policy_version_id` is NOT NULL.

### No new environment variables

Stage 6.1 reuses the Stage 5.7 iter 4 env vars
(`CLEAN_CODE_EVALUATOR_PG_URL`, `CLEAN_CODE_PG_URL`) without
change.

### Migration sequence

1. Ensure `clean_code.policy_activation` has at least one
   row per repo that should be gateable. Activation is the
   canonical `policy.activate` write verb on the Policy
   Steward (`POST /v1/policy/activate`); see the Stage 5.2
   `policy.activate` runbook section for the body schema
   and the `## Stage 5.2: Policy publish/activate/rulepack
   verbs` rollout section below for the deploy sequence.
2. Verify the persisted `policy_version.signature` is
   correct for the active policy bytes; the
   `policy_signature_invalid` degraded path is the gate's
   own self-healing trap, not a happy-path expectation.
3. Deploy the Stage 6.1 build of `clean-code-eval-gate`.
4. **Cutover smoke test**: call
   `POST /v1/eval/gate {repo_id, sha}` against a SHA whose
   `scan_status='scanned'`. Expect HTTP 200 with
   `degraded=false` and a non-empty `evaluation_run_id`.
5. **Bypass-regression smoke test**: call
   `POST /v1/eval/gate {repo_id, sha, policy_version_id: "<any uuid>"}`.
   Expect HTTP 400 with the error pointing at
   `/v1/eval/replay`. If you get HTTP 200, the bypass
   rejection has regressed; roll back.

### Rollback

If Stage 6.1 misbehaves in production, roll back the
`clean-code-eval-gate` binary to the Stage 5.7 iter 4 build.
The audit-row schema is unchanged
(`evaluation_run`/`evaluation_verdict`/`finding`), so no
DB migration rollback is required. Reconcilers that depend
on `/v1/eval/replay` will receive HTTP 404 against the
Stage 5.7 binary -- pause those reconcilers until the
Stage 6.1 binary is re-deployed.

### Observability checklist

- `clean_code_eval_gate_requests_total{route, status}` --
  one counter per (route, HTTP status) pair. Expect
  steady-state traffic on `/v1/eval/gate` with status
  200; spikes on 400 indicate caller-side bypass attempts.
- `clean_code_eval_gate_degraded_total{reason}` -- one
  counter per `degraded_reason`. `samples_pending` should
  trend down as the ingestor catches up;
  `policy_signature_invalid` should be near zero -- any
  non-zero rate indicates a steward signing-key issue.

## Stage 4.1 iter 3: per-verb durable idempotency key + Finalize same-terminal contract

Iter 2 shipped the durable seam with a `(kind, payload_hash)`
partial unique index. Iter 3 keys the durable uniqueness
on `(verb, payload_hash)` instead. Two distinct verbs that
share the same kind (e.g. `churn` + `defects` -- both
`external_per_row`) MUST receive independent scan_run rows
even when their canonical bodies hash identically.
Migration 0009 is rewritten end-to-end.

### Pre-roll checklist (iter 3 -- in order)

- [ ] **If iter 2's migration was applied on a dev DB**, the
      iter-3 up migration DROPs the iter-2
      `scan_run_payload_hash_kind_uniq` index automatically.
      No operator action required other than running the
      iter-3 file via `psql -f` (autocommit). Production has
      not applied iter 2 because Stage 4.1 has not gone live;
      this branch is the iter-3 shape from day one for prod.

- [ ] Apply migration 0009 (iter-3 shape) BEFORE deploying
      the new binary. The file is
      `services/clean-code/migrations/0009_scan_run_payload_hash_unique.up.sql`.
      It uses `DROP / CREATE INDEX CONCURRENTLY` and
      `ALTER TABLE ADD COLUMN`, which CANNOT all run inside
      a single transaction:

      ```bash
      psql "$CLEAN_CODE_PG_URL" \
          -v ON_ERROR_STOP=1 \
          -f services/clean-code/migrations/0009_scan_run_payload_hash_unique.up.sql
      ```

      Do NOT route this migration through a wrapper that
      opens a transaction. Verify the new index is
      `valid=true` post-apply:

      ```sql
      SELECT indexname, indisvalid
      FROM   pg_indexes ix JOIN pg_class c ON c.relname = ix.indexname
                            JOIN pg_index i ON i.indexrelid = c.oid
      WHERE  ix.indexname = 'scan_run_payload_hash_verb_uniq';
      ```

      An `invalid` index means the concurrent build failed
      (likely a pre-existing duplicate on the new `(verb,
      payload_hash)` shape); `DROP INDEX CONCURRENTLY` and
      investigate before retrying. Also verify the new CHECK
      constraint is present:

      ```sql
      SELECT conname FROM pg_constraint
      WHERE  conname = 'scan_run_verb_payload_hash_consistent';
      ```

- [ ] Confirm there are NO duplicate
      `(verb, payload_hash) WHERE payload_hash IS NOT NULL`
      rows in `scan_run` BEFORE migration 0009 -- otherwise
      the CONCURRENTLY build will mark the new index
      `invalid`:

      ```sql
      SELECT verb, payload_hash, count(*)
      FROM   scan_run
      WHERE  payload_hash IS NOT NULL
      GROUP  BY verb, payload_hash
      HAVING count(*) > 1;
      ```

      Zero rows is the expected state on a fresh
      external-ingest rollout. If a dev DB has rows from
      iter-2's `(kind, payload_hash)` shape, the iter-3
      backfill UPDATE assigns them `verb = '__legacy_' ||
      kind`, which avoids violating the new CHECK
      constraint but does NOT auto-resolve duplicates --
      operators with rows from a prior dev environment must
      manually deduplicate before applying 0009.

### Iter-3 verification

After the new binary is live, verify the per-verb
idempotency at the durable layer. Stage 4.1 only mounts
the `churn` verb in `RouterConfig.Verbs`
(`cmd/clean-code-metric-ingestor/main.go`); the `coverage`
/ `test_balance` / `defects` verbs land in Stages
4.2 / 4.3 / 4.5 and will register against the same Router
seam. Until those stages land, an unmounted verb returns
`404 / VERB_NOT_FOUND`, so the live operator smoke test in
Stage 4.1 verifies only `churn`:

```bash
# Stage 4.1 mounted verb -- replay invariant:
RESP_A=$(curl -s -X POST .../v1/ingest/churn --data @body.json \
            -H "X-Hub-Signature-256: sha256=..." \
            -H "X-Signing-Key-Id: $CLEAN_CODE_WEBHOOK_SIGNING_KEY_ID")
RESP_B=$(curl -s -X POST .../v1/ingest/churn --data @body.json \
            -H "X-Hub-Signature-256: sha256=..." \
            -H "X-Signing-Key-Id: $CLEAN_CODE_WEBHOOK_SIGNING_KEY_ID")
# Second response MUST carry replayed=true with the same
# scan_run_id as the first:
[[ "$(jq -r .scan_run_id <<< "$RESP_A")" == "$(jq -r .scan_run_id <<< "$RESP_B")" ]] \
    || { echo "FAIL: replay returned a different scan_run_id"; exit 1; }
[[ "$(jq -r .replayed <<< "$RESP_B")" == "true" ]] \
    || { echo "FAIL: second response missing replayed=true"; exit 1; }
```

The per-verb idempotency boundary itself (two distinct
verbs sharing a payload_hash MUST receive independent
scan_run_ids) is verified by unit test
`TestInMemoryScanRunRepository_OpenExternal_DifferentVerbs_SamePayload_GetIndependentRuns`
and at the DB layer by migration 0009's partial unique
index `scan_run_payload_hash_verb_uniq`. A live HTTP
verification will be added in Stage 4.5 when `defects` is
mounted alongside `churn`.

### Rollback (iter 3)

`migrations/0009_scan_run_payload_hash_unique.down.sql`:

```sql
DROP INDEX CONCURRENTLY IF EXISTS clean_code.scan_run_payload_hash_verb_uniq;
ALTER TABLE clean_code.scan_run DROP CONSTRAINT IF EXISTS scan_run_verb_payload_hash_consistent;
ALTER TABLE clean_code.scan_run DROP COLUMN IF EXISTS verb;
```

Apply via `psql -f` (autocommit). Rolling back leaves the
external-ingest Router functional but NOT
durably-idempotent across replicas / restarts.

## Stage 4.1 iter 2: durable `scan_run(payload_hash)` idempotency + Router mount

*Note: iter-2's `(kind, payload_hash)` uniqueness shape
has been superseded by iter-3's `(verb, payload_hash)`
shape. The iter-2 checklist below is preserved as
historical record; the iter-3 checklist above is the
canonical playbook for the migration step.*

Iter 1 shipped the transport / HMAC / in-process idempotency
layers. Iter 2 closes the durability gap: a second POST with
the same canonical body now resolves through the database
even across replica or restart boundaries, and the Router is
wired into the running service.

### Pre-roll checklist (iter 2 -- in order)

- [ ] Apply migration 0009 BEFORE deploying the new binary.
      The file is
      `services/clean-code/migrations/0009_scan_run_payload_hash_unique.up.sql`.
      It uses `CREATE UNIQUE INDEX CONCURRENTLY`, which CANNOT
      run inside a transaction:

      ```bash
      psql "$CLEAN_CODE_PG_URL" \
          -v ON_ERROR_STOP=1 \
          -f services/clean-code/migrations/0009_scan_run_payload_hash_unique.up.sql
      ```

      Do NOT route this migration through a wrapper that opens
      a transaction. Verify the index is `valid=true` post-apply:

      ```sql
      SELECT indexname, indisvalid
      FROM   pg_indexes ix JOIN pg_class c ON c.relname = ix.indexname
                            JOIN pg_index i ON i.indexrelid = c.oid
      WHERE  ix.indexname = 'scan_run_payload_hash_kind_uniq';
      ```

      An `invalid` index means the concurrent build failed (likely
      a pre-existing duplicate); `DROP INDEX CONCURRENTLY` and
      investigate before retrying.

- [ ] Confirm there are NO duplicate
      `(kind, payload_hash) WHERE payload_hash IS NOT NULL`
      rows in `scan_run` BEFORE migration 0009 -- otherwise the
      CONCURRENTLY build will mark the index `invalid`:

      ```sql
      SELECT kind, payload_hash, count(*)
      FROM   scan_run
      WHERE  payload_hash IS NOT NULL
      GROUP  BY kind, payload_hash
      HAVING count(*) > 1;
      ```

      Zero rows is the expected state on a fresh
      external-ingest rollout.

- [ ] Set the three env vars in the deployment secret store:

      ```
      CLEAN_CODE_ENABLE_EXTERNAL_INGEST_WEBHOOK=true
      CLEAN_CODE_WEBHOOK_SIGNING_KEY_ID=<opaque ASCII id>
      CLEAN_CODE_WEBHOOK_HMAC_SECRET=<32+ random bytes, base64 or hex>
      ```

      Each is required when the flag is `true`; startup-time
      interlocks reject partial wiring.

- [ ] Roll the new `clean-code-metric-ingestor` binary.
      Confirm the startup log line
      `mounted external-ingest webhook router` (INFO, with
      structured fields `path=/v1/ingest/`,
      `signing_key_id=<your id>`, and `verbs=[churn]` --
      emitted from `cmd/clean-code-metric-ingestor/main.go:
      mountIngestRouter`) and the existing
      `clean-code-metric-ingestor http listening`.

- [ ] Smoke-test the durable replay using
      `scripts/smoke/ingest-replay.sh` (or curl): POST the
      same canonical body twice and confirm the second
      response carries `replayed=true` AND the same
      `scan_run_id` as the first.

### Rollback (iter 2)

If the Router misbehaves and the legacy `/v1/ingest/churn`
path is sufficient:

1. Set `CLEAN_CODE_ENABLE_EXTERNAL_INGEST_WEBHOOK=false` and
   restart. The Router unmounts; the legacy churn handler
   continues serving.
2. (Optional) Drop the partial unique index if it's blocking
   an unrelated `scan_run` insertion path:

   ```bash
   psql "$CLEAN_CODE_PG_URL" -v ON_ERROR_STOP=1 \
       -f services/clean-code/migrations/0009_scan_run_payload_hash_unique.down.sql
   ```

   This is safe: the constraint is the ONLY thing depending
   on the index, and re-applying 0009 later is idempotent
   (`IF NOT EXISTS`). After the drop, retries across replicas
   degrade to "may insert duplicate `scan_run` rows on race"
   -- the Router still works, just not durably-idempotent.

## Stage 4.1: external ingest webhook Router + HMAC

The generic `/v1/ingest/{verb}` Router lands in
`internal/ingest/webhook/router.go` and is the production
surface for all four `ingest.*` verbs. Stage 4.1 ships the
transport, HMAC, and idempotency layers; per-verb dispatch
(coverage / test_balance / churn / defects) is wired
incrementally in Stages 4.2-4.5.

### Pre-roll checklist

- [ ] Generate ONE signing key id (e.g. UUID, or
  `kv-prod-2026-q1`) and ONE 32-byte secret. Distribute the
  secret to publishers via the deployment's secret manager,
  NOT source control.
- [ ] Seed `webhook.StaticSecretResolver` with the `(id,
  secret)` pair at composition-root startup. The Router
  panics on a nil resolver or a registration with an empty
  secret -- a misconfigured deployment fails LOUDLY rather
  than serving unauthenticated traffic.
- [ ] CI publishers MUST compute and send BOTH headers per
  request:
  - `X-Signing-Key-Id: <the id agreed above>`
  - `X-Hub-Signature-256: sha256=<lowercase-hex HMAC-SHA256(body, secret)>`
- [ ] CI publishers SHOULD treat any `replayed=true` in the
  200 envelope as success (no-op retry); retries against
  network errors are safe and idempotent.

### Operator key rotation (24h overlap)

1. `resolver.Add(newID, newSecret)` -- both ids verify.
2. Update publishers in waves.
3. After 24h: `resolver.Remove(oldID)`. The old id now
   yields `HMAC_UNKNOWN_KEY_ID` -- monitor the
   `HMAC verification failed` warning logs for stragglers.

## Stage 5.7 (iter 4): least-privilege Audit writes, durable catchup, and production gate wiring

This subsection captures the operator-facing changes that
shipped with Stage 5.7 iter 4. The base Stage 5.7 rollout
narrative (worker composition, observability, rollback) is
unchanged and remains canonical below.

### New environment variables

| Var | Default | Purpose |
|---|---|---|
| `CLEAN_CODE_SOLID_BATCH_PG_URL` | unset (FALLS BACK to `CLEAN_CODE_PG_URL` with WARN) | Separate DSN authenticated as `clean_code_solid_batch` for the rule-engine Audit writes (`INSERT INTO evaluation_run`, `evaluation_verdict`, `finding`). Required under production least-privilege; dev/test compose-as-superuser can omit it. |
| `CLEAN_CODE_EVALUATOR_PG_URL` | unset (FALLS BACK to `CLEAN_CODE_PG_URL`) | DSN authenticated as `clean_code_evaluator` for the new `clean-code-eval-gate` binary. Used for the degraded-path Audit writes and for the `commit.scan_status` readiness reads. |

### New cmd binary: `clean-code-eval-gate`

The `evaluator.Gate.Evaluate` surface is now hosted by its own
binary at `services/clean-code/cmd/clean-code-eval-gate`. It
exposes:

- `/healthz` -- standard liveness check.
- `POST /v1/eval/gate` -- accepts `{repo_id, sha, policy_version_id, scope?}` and returns `{evaluation_run_id, evaluation_verdict_id, finding_ids[], verdict, degraded, degraded_reason?}`.

The handler resolves the policy via the Steward, re-verifies
the persisted signature against THAT policy version's
canonical bytes, checks `commit.scan_status='scanned'`, and
delegates to `rule_engine.Engine.RunSync` on the happy path.
On the two degraded paths (`policy_signature_invalid`,
`samples_pending`) the gate writes ONE `evaluation_run` +
ONE `evaluation_verdict` (zero findings, `degraded=true`,
`verdict='warn'`) under the `clean_code_evaluator` grant
and returns HTTP 200 with `degraded=true`.

The Verdict on BOTH degraded paths is `warn` (Stage 5.7
evaluator feedback #1 / architecture Sec 3.7 lines 566-575
+ operator pin `gate-degraded-policy=warn` from Sec 1.6):
the gate never blocks on a degraded path, but it also does
not silently pass.

### Durable catchup loop

The post-scan dispatcher's buffered channel
(`scanEvents`, capacity 64) is best-effort. Per Stage 5.7
evaluator feedback #6, `cmd/clean-code-metric-ingestor`
now:

1. Bounds the emit by `scanEventEmitTimeout` (5s). A
   saturated buffer surfaces as a latency spike + log line,
   NOT a silent permanent drop.
2. Runs `rule_engine.Worker.Catchup(...)` on startup AND
   every 5 minutes against `SQLPendingScanReader` which
   selects `commit` rows with `scan_status='scanned'` that
   have NO matching `evaluation_run` row (caller=
   `batch_refresh`, degraded=false) for the active policy
   version (NOT EXISTS anti-join -- see Stage 5.7
   evaluator iter-4 feedback #1). The reader orders by
   `(committed_at, repo_id, sha)` (Stage 5.7 evaluator
   iter-5 feedback #1: `clean_code.commit` has
   `committed_at`, not `created_at`) and is paged via a
   keyset cursor over the same tuple
   (`(committed_at, repo_id, sha) > ($t, $r, $s)`) at
   `LIMIT 100` per call to avoid an evaluation storm on
   policy switch. Each invocation pins the active policy
   version once at the TOP and routes the work through
   `Worker.processWithPolicy` so a mid-run policy switch
   cannot cause pages-for-P1 to be persisted-against-P2.
3. `Worker.Catchup` advances the cursor by the LAST row of
   every page regardless of per-event success or failure
   (Stage 5.7 evaluator iter-5 feedback #2: cursor
   advancement guarantees that a persistent poison row at
   the head no longer starves later valid SHAs within the
   same invocation). The loop terminates when the reader
   returns an empty page OR a short page (`len < limit`);
   any rows that errored within the invocation are logged
   and retried fresh on the next 5-minute tick.

Grep for these log lines:
- `rule_engine: scan event channel saturated after 5s -- event WILL BE REPROCESSED BY CATCHUP` -- emit timeout tripped; catchup will pick the SHA up on its next tick.
- `rule_engine.worker.Catchup (startup) processed=N events` -- backlog drained at boot.
- `rule_engine.worker.Catchup (periodic) processed=N events` -- 5-minute tick drained N events.

### Cross-replica run dedup (`Store.LookupRecentCanonicalRun`)

Per Stage 5.7 evaluator iter-5 feedback #3 (and the iter-6
feedback #2 closure), the engine performs an additional
Store-level lookup BEFORE writing a fresh canonical
`(evaluation_run, evaluation_verdict, findings)` triple.
The lookup runs INSIDE the `pg_advisory_xact_lock`
envelope, so a replica that just committed its canonical
row is observed by the second replica's RC-isolated SELECT
before that replica begins mutating Audit rows. The
implementation joins `evaluation_verdict ON
evaluation_run.evaluation_run_id =
verdict.evaluation_run_id` and filters `verdict.degraded =
false` so a degraded short-circuit is never returned as
canonical.

**Both callers are covered (iter-7).** Migration 0008
adds a nullable `evaluation_run.scope_id uuid` column +
composite index `evaluation_run_dedup_idx`. The lookup
filters with the null-safe `IS NOT DISTINCT FROM`
operator, so a scoped eval_gate row NEVER matches an
unscoped eval_gate call (or vice versa); the engine
consults the Store-level lookup for both
`caller='batch_refresh'` and `caller='eval_gate'` with
the call's `scopeID`, and parallel calls across replicas
produce a single canonical run+verdict pair. The previous
iter-6 limitation (eval_gate fell back to the in-process
cache only) is closed.

Operationally, the second replica that observed the
first's row returns the SAME `(evaluation_run_id,
evaluation_verdict_id, []finding_id)` to its caller, so
HTTP responses from `eval.gate.Evaluate` are stable
across replicas for the same `(repo_id, sha,
policy_version_id, scope_id)` tuple within the run-dedup
TTL.

### Active-row reads for metric samples

`SQLStore.ListMetricSamples` (used by both happy and
synchronous paths) now JOINs through
`clean_code.metric_sample_active` (the active-row pointer
table) so retracted / inactive samples cannot trigger
findings. The query also hydrates `pack`, `source`,
`degraded`, and `degraded_reason` so DSL predicates over
those canonical fields evaluate correctly under production
data. See Stage 5.7 evaluator feedback #3 + #4.

### Per-scope predicate evaluation (SOLID composites)

The rule engine now evaluates predicates per SCOPE (not
per sample) via the new `dsl.Predicate.EvalAtScope`
contract. This is what enables SOLID composite recipes
like SRP's `threshold(lcom4) AND threshold(interface_width)`
to fire at a class scope when the class has BOTH a
high-LCOM4 sample AND a wide-interface sample, even though
no single sample carries both metric_kinds. Per-sample
correlated predicates such as
`metric_kind == 'lcom4' AND value > 5` still take the
single-sample-witness path so they cannot cross witnesses
across two unrelated samples. See Stage 5.7 evaluator
feedback #7.

## Stage 5.7: SOLID Rule Engine batch worker and synchronous mode

### Pre-requisites

- **Stage 5.2 active policy:** the Rule Engine consumes the
  latest active `policy_version`. Confirm the
  `clean_code.policy_activation` table has at least one row
  (`SELECT activation_id, policy_version_id, created_at FROM
  clean_code.policy_activation ORDER BY created_at DESC
  LIMIT 1;`) before enabling the engine.
- **Migration 0003 applied:** `evaluation_run`,
  `evaluation_verdict`, `finding` tables and the Phase 1.5
  grants on those tables to `clean_code_solid_batch`,
  `clean_code_evaluator`, and `clean_code_wal_reconciler`
  MUST exist. Verify with:

  ```bash
  psql -c "SELECT grantee, privilege_type
           FROM information_schema.role_table_grants
           WHERE table_name IN ('evaluation_run','evaluation_verdict','finding')
             AND grantee LIKE 'clean_code_%'
           ORDER BY table_name, grantee;"
  ```

  Expect THREE grantees (`clean_code_evaluator`,
  `clean_code_solid_batch`, `clean_code_wal_reconciler`) per
  table, each with `INSERT`.

### One-time bootstrap (per environment)

1. **Apply migration `0008_evaluation_run_scope_id`.** Stage
   5.7 iter 7 introduces ONE schema change on top of the base
   Audit schema from migration 0003: a nullable
   `clean_code.evaluation_run.scope_id uuid` column plus the
   composite index `evaluation_run_dedup_idx(repo_id, sha,
   policy_version_id, caller, scope_id, created_at DESC)`. The
   column is REQUIRED by the engine's Store-level cross-
   replica dedup lookup (`Store.LookupRecentCanonicalRun`)
   for both `caller='eval_gate'` and `caller='batch_refresh'`;
   without it, parallel `eval.gate` calls landing on
   different replicas can mint duplicate run+verdict rows.
   The migration is forward-only and safe to apply against a
   live database. Lock semantics (iter-8 evaluator feedback
   #3):

   - The `ADD COLUMN scope_id uuid NULL` is additive and
     non-defaulting, so PostgreSQL completes it as a fast
     `ACCESS EXCLUSIVE` catalogue update -- no table
     rewrite, no data scan.
   - The composite index is built with
     `CREATE INDEX CONCURRENTLY IF NOT EXISTS
     evaluation_run_dedup_idx ...`, which acquires
     `SHARE UPDATE EXCLUSIVE` rather than `ACCESS EXCLUSIVE`
     on `clean_code.evaluation_run`. The Rule Engine's
     happy-path INSERTs and the eval-gate's degraded-path
     INSERTs continue to run while the index materialises.
   - CONCURRENTLY cannot run inside a transaction, so the
     migration file deliberately omits any `BEGIN/COMMIT`
     envelope. Apply via `psql -f
     migrations/0008_evaluation_run_scope_id.up.sql`
     (autocommit; the default for `psql -f` is already
     `--single-transaction=off`). Wrapping the apply
     command in `psql --single-transaction` will FAIL with
     "CREATE INDEX CONCURRENTLY cannot run inside a
     transaction block".
   - **End-to-end idempotency** (iter-9 evaluator
     feedback #1): both DDL statements in the up migration
     carry `IF NOT EXISTS` (`ADD COLUMN IF NOT EXISTS
     scope_id uuid NULL` on the table, `CREATE INDEX
     CONCURRENTLY IF NOT EXISTS evaluation_run_dedup_idx`
     on the index), so re-running the WHOLE file against
     a partially-applied state never errors -- in
     particular, if a previous attempt added the column
     but the CONCURRENTLY index build was interrupted
     (leaving the index in PostgreSQL's `INVALID` state),
     the operator runs `DROP INDEX CONCURRENTLY IF EXISTS
     clean_code.evaluation_run_dedup_idx` (no
     `--single-transaction`) and re-runs `psql -f
     migrations/0008_evaluation_run_scope_id.up.sql`;
     the ALTER step is a no-op on the second pass, the
     CREATE INDEX picks up cleanly, and the migration
     completes. (`COMMENT ON COLUMN` is naturally
     idempotent -- it overwrites the existing comment
     with the same text.)

   Verify after apply:

   ```bash
   psql -c "SELECT column_name, is_nullable
            FROM information_schema.columns
            WHERE table_schema='clean_code'
              AND table_name='evaluation_run'
              AND column_name='scope_id';"
   # expect: scope_id | YES

   psql -c "SELECT indexname, indexdef FROM pg_indexes
            WHERE schemaname='clean_code'
              AND tablename='evaluation_run'
              AND indexname='evaluation_run_dedup_idx';"
   # expect: evaluation_run_dedup_idx

   psql -c "SELECT indisvalid FROM pg_index
            WHERE indexrelid='clean_code.evaluation_run_dedup_idx'::regclass;"
   # expect: t  -- 'f' means the CONCURRENTLY build was
   # interrupted; drop the INVALID index and re-run the
   # migration before considering the bootstrap complete.
   ```

   Rollback path: `migrations/0008_evaluation_run_scope_id.down.sql`
   drops the index (also CONCURRENTLY) then the column.
   The engine treats a missing `scope_id` column as a hard
   error at startup (the SQL lookup references `scope_id
   IS NOT DISTINCT FROM $5::uuid`), so the rollback must
   be paired with rolling Stage 5.7 iter 7 binaries back
   to an iter-6 build that does not expect the column.

2. **Verify the Rule Engine wires.** The composition root
   for the batch path is the
   `cmd/clean-code-metric-ingestor/main.go` entrypoint --
   the same binary that serves `/v1/ingestor/process` and
   manages the ScanRun state machine. Stage 5.7 adds an
   in-process [rule_engine.Worker] driven by a
   buffered post-scan dispatcher channel:

   ```go
   stewardStore, _ := steward.NewSQLStore(db)
   stew, _        := steward.New(steward.Config{Store: stewardStore})

   ruleStore, _ := rule_engine.NewSQLStore(rule_engine.SQLStoreConfig{
       DB:      db,
       Steward: stewardStore,
   })
   engine, _ := rule_engine.New(rule_engine.Config{Store: ruleStore})

   events := make(chan rule_engine.ScanEvent, 64) // buffered
   worker, _ := rule_engine.NewWorker(rule_engine.WorkerConfig{
       Engine:     engine,
       Activation: rule_engine.NewStewardActivation(stew),
       Events:     events,
       Logger:     slog.Default(),
   })
   go worker.Run(ctx)
   ```

   The composition is performed at process start by
   `startRuleEngineWorker`; the HTTP handler
   `handleProcess` emits a non-blocking `ScanEvent` on
   every transition to `scan_status='scanned'`. A
   `CLEAN_CODE_RULE_ENGINE_DISABLED=1` env var skips the
   wiring entirely (no-op fallback) for environments that
   want to defer the cut-over.

   The engine is shared between the worker (`RunBatch`,
   `caller='batch_refresh'`) and `eval.gate.Evaluate`
   (`RunSync`, `caller='eval_gate'`); both paths are
   concurrent-safe. The
   `rule_engine.NewStewardActivation` adapter is the
   canonical bridge between
   `steward.Steward.ActivePolicyVersion(...)` and the
   worker's `PolicyActivationReader` port -- passing
   `stew` (a `*steward.Steward`) directly to
   `WorkerConfig.Activation` will NOT compile.

### Observability

Grep the structured log stream for these lines per event:

- `rule_engine.worker: RunBatch completed` -- one INFO per
  successful batch run. Carries `evaluation_run_id`,
  `verdict`, `findings_count`.
- `rule_engine.worker: skip -- no active policy` -- INFO,
  expected only during fresh-deploy steady state.
- `rule_engine.worker: active policy lookup failed` -- ERROR
  on activation reader failure; the worker proceeds to the
  next event.
- `rule_engine.worker: RunBatch failed` -- ERROR on engine
  failure (broken predicate, missing rule, etc.). The worker
  proceeds to the next event so one broken policy does not
  bring down the post-scan pipeline.

### Rollback

The Rule Engine has no persisted state of its own beyond
the canonical Audit rows. To roll back:

1. Stop the service.
2. Optionally clear the freshly-written audit rows for the
   in-flight SHA via the `audit.purge_run` operator verb
   (Stage 5.6 -- if shipped) or by issuing a manual
   `DELETE FROM clean_code.finding WHERE evaluation_run_id
   IN (...); DELETE FROM clean_code.evaluation_verdict WHERE
   evaluation_run_id IN (...); DELETE FROM
   clean_code.evaluation_run WHERE evaluation_run_id IN
   (...)` against the offending IDs.
3. Deploy the previous binary; the prior Stage's evaluator
   (signature-verify-only) will resume as the canonical
   verdict writer on the gate-degraded paths.

### Feature flags

None for Stage 5.7. The Rule Engine becomes the canonical
writer of `evaluation_verdict` on the rule-pass paths as
soon as the binary lands; there is no operator switch to
defer the cut-over. The gate-degraded short-circuit paths
(signature-invalid, samples_pending) continue to write
their own zero-findings verdict pair via the
`clean_code_evaluator` role.

## Stage 3.2: Metric Ingestor and ScanRun state machine

### One-time bootstrap (per environment)

1. **Apply migration `0006_repo_url`** against the shared
   Postgres instance. The migration:
   * adds the `repo_url` column to `clean_code.repo`;
   * installs the WRITE-ONCE trigger
     `tg_repo_url_write_once` and the backing PL/pgSQL
     function;
   * grants `INSERT (repo_url)` + `UPDATE (repo_url)` on
     `clean_code.repo` to `clean_code_management` ONLY
     (see `0006_repo_url.up.sql:127-141`). The Repo Indexer
     is **not** granted INSERT/UPDATE on `repo_url`; the
     column is Management-owned. The `clean_code_metric_ingestor`
     role's `UPDATE (scan_status)` on `clean_code.commit`
     was granted by the earlier `0004_roles.up.sql`
     migration -- 0006 does NOT touch `commit.scan_status`
     grants.

   Verify with:

   ```bash
   psql "$CLEAN_CODE_PG_URL" -c "\d clean_code.repo" \
     | grep -E "repo_url|tg_repo_url_write_once"
   psql "$CLEAN_CODE_PG_URL" -c "\dt+ clean_code.commit"
   ```

   Expect the `repo_url` column on `repo`, the
   `tg_repo_url_write_once` trigger on `repo`, and the
   `clean_code.commit` table to be present (its
   `scan_status` column was added by an earlier 0003-era
   migration).

2. **Back-fill `repo_url` for existing repos** (skip if
   this is a fresh environment). Today's
   `internal/management/register_repo.go` helper is the
   only WRITE-ONCE-safe path; the Stage 1.2 follow-up
   workstream `ws-...-stage-mgmt-register-repo-repo-url`
   adds the HTTP verb. The helper uses
   `INSERT INTO clean_code.repo (...) VALUES (...) ON
   CONFLICT (repo_id) DO NOTHING` (see
   `register_repo.go:204-213`), so re-running it against
   an already-registered `repo_id` is a no-op. Operators
   that need to back-fill `repo_url` for a row that was
   inserted WITHOUT one (legacy data) must `UPDATE
   clean_code.repo SET repo_url = $1 WHERE repo_id = $2
   AND repo_url IS NULL` -- the WRITE-ONCE trigger allows
   the NULL→non-NULL transition but rejects any subsequent
   change. The `COALESCE(EXCLUDED.repo_url, repo.repo_url)`
   shape used in the e2e fixture is a test-only helper and
   does NOT appear in the production helper.

3. **Set the env vars** on the long-running process. The
   metric ingestor inherits its config from the
   service-wide knobs -- there is intentionally no
   `CLEAN_CODE_METRIC_INGESTOR_*` namespace today.

   | Env var                              | Value                                                                  |
   | ------------------------------------ | ---------------------------------------------------------------------- |
   | `CLEAN_CODE_PG_URL`                  | postgres connection string with `clean_code_metric_ingestor` role auth |
   | `CLEAN_CODE_AST_SCAN_ROOT`           | `/var/lib/clean-code/checkouts` (or wherever your indexer puts them)   |
   | `CLEAN_CODE_PERIODIC_SWEEP_CADENCE`  | Sweeper tick interval (Go duration; default is the value in `config.go`) |
   | `CLEAN_CODE_SCAN_TIMEOUT`            | Per-scan timeout passed to `WithStateMachineTimeout`                   |

   Production scan pods MUST set BOTH `CLEAN_CODE_PG_URL`
   and `CLEAN_CODE_AST_SCAN_ROOT`. The composition root
   (`cmd/clean-code-metric-ingestor/main.go:837-891`)
   selects the dispatcher based on the pairing:

   - **Both set** → `RegistryBackedFoundationDispatcher`
     is wired with a
     `DirectoryAstFileSource{Coordinator, Pool}` rooted at
     `CLEAN_CODE_AST_SCAN_ROOT` and threaded with the
     shared `iso.coord` / `iso.pool` (so mode-flip drains
     observe the SAME in-flight counter the scan path
     increments). The sweeper is launched separately by
     `buildSweepLoop` at `main.go:160`.
   - **`CLEAN_CODE_PG_URL` set, `CLEAN_CODE_AST_SCAN_ROOT`
     unset** → the foundation dispatcher falls back to
     `NoopFoundationRecipeDispatcher{Logger: logger}`
     (`main.go:860`, `:887-890`) and the binary boots
     normally. Stage 9.3 iter-3 intentionally reverted the
     iter-4 fail-fast contract so that a webhook-only
     metric-ingestor pod (one that serves `mgmt.*` and
     `/v1/ingest/*` without owning the on-disk checkout
     layout) still starts. A single info log
     `foundation dispatcher = noop
     (CLEAN_CODE_AST_SCAN_ROOT unset)` is emitted at
     startup; operators deploying a SCAN pod (rather than
     a webhook pod) MUST verify this log line is absent
     and that `isolation_pool_languages` appears in the
     `wired production foundation dispatcher` info log
     instead.
   - **`CLEAN_CODE_PG_URL` unset** → the binary refuses
     to start. `cmd/clean-code-metric-ingestor/main.go:102-104`
     does `log.Fatalf("%s is required", config.EnvPGURL)`
     before any listener is bound, so there is no
     `/healthz` to probe and no in-memory fallback. The
     metric-ingestor binary deliberately does not support
     a PG-less scaffold mode; operators who want one
     should fall back to unit tests against
     `metric_ingestor.NewInMemoryScanRunStore`.

### Per-rollout verification

After deploying a new build, confirm:

1. `/healthz` returns 200 within 5s of pod-ready.
2. **There is NO `/readyz` endpoint on this binary.**
   `cmd/clean-code-metric-ingestor` mounts only `/healthz`
   (always), `/metrics` (always), the optional legacy
   `/v1/ingestor/*` routes when `EnableLegacyDemoAPI=true`,
   the mgmt verbs (`mgmt.set_mode`, `mgmt.retract_sample`,
   `mgmt.rescan`, `mgmt.register_repo`) via
   `mountMgmtRoutes`, and the external-ingest router via
   `mountIngestRouter` -- nothing registers `/readyz` or
   calls `AddReadyCheck`. (Code inspection of
   `cmd/clean-code-metric-ingestor/main.go` finds no
   references to `readyz`, `AddReadyCheck`,
   `healthHandler`, or `signing_key_cache`.) The
   `AstSourceAvailability` / `WithStateMachineSourceProbe`
   option exists in `internal/metric_ingestor` and is
   exercised by unit tests, but the metric-ingestor binary
   does NOT wire it today (`NewStateMachine` is only
   called from tests). For this rollout, gate on `/healthz`
   returning 200, then on the startup log lines
   `wired production foundation dispatcher
   isolation_pool_languages=[...]` (scan pod) or
   `foundation dispatcher = noop (CLEAN_CODE_AST_SCAN_ROOT
   unset)` (webhook pod). A dedicated `/readyz` aggregating
   PG-ping + signing-key + AST-source probes is a
   follow-up workstream.
3. The first sweeper tick (after
   `CLEAN_CODE_PERIODIC_SWEEP_CADENCE`) fires and emits a
   structured log line of the form:

   ```
   metric_ingestor.sweeper start kind=full sha_binding=single
   ```

4. Query PG to confirm BOTH state machines moved together.
   The `clean_code.commit` table is keyed by
   `(repo_id, sha)` -- there is no `commit_id` column.
   `clean_code.scan_run` carries `repo_id` plus `to_sha`
   (non-null for `sha_binding='single'` per the
   `scan_run_sha_binding_consistent` CHECK constraint in
   `0001_catalog_lifecycle.up.sql`):

   ```sql
   SELECT c.repo_id, c.sha, c.scan_status,
          r.scan_run_id, r.status
   FROM clean_code.commit c
   LEFT JOIN clean_code.scan_run r
          ON r.repo_id = c.repo_id
         AND r.to_sha  = c.sha
         AND r.sha_binding = 'single'
   WHERE c.repo_id = '<the repo you watched>'
     AND c.sha     = '<the SHA you watched>';
   ```

   Expect `(commit.scan_status, scan_run.status)` to be
   one of `('scanning','running')` mid-sweep,
   `('scanned','succeeded')` on success, or
   `('failed','failed')` on failure -- never any other
   pairing.

5. The audit log shows `commit.scan_status` UPDATEs by
   `clean_code_metric_ingestor` ONLY -- no other role. If
   you see another role writing to that column, the Phase
   1.5 role grants regressed and the deploy MUST be rolled
   back.

### Key constraints to watch

- **`repo_url` is WRITE-ONCE.** Any attempt to UPDATE the
  column to a different value raises SQLSTATE 23514 from
  the `tg_repo_url_write_once()` trigger, with the literal
  message template (per
  `migrations/0006_repo_url.up.sql:179`)
  `clean_code.repo.repo_url is WRITE-ONCE: cannot change
  from %L to %L for repo_id %L` -- the `%L` placeholders
  are filled by `format()` with the old URL, the new URL,
  and the affected `repo_id`. If your back-fill script
  trips this, you have a real source-of-truth ambiguity
  (the same repo registered twice with different URLs) --
  resolve at the policy layer, do NOT disable the trigger.
- **Canonical state alphabet.** Anything other than
  `pending`, `scanning`, `scanned`, `failed` on
  `commit.scan_status` or anything other than `running`,
  `succeeded`, `failed` on `scan_run.status` is a bug. The
  state constants live in
  `internal/metric_ingestor/state.go` and are pinned by
  the conformance suite under
  `test/conformance/canonical_states_test.go` (when
  present).

### Backout

Stage 3.2 is additive on top of Stage 3.1's commit lifecycle:

1. **Stop the sweep loop first.** Unset
   `CLEAN_CODE_AST_SCAN_ROOT` (or rotate it to a path the
   pod cannot read) and restart the pod. The next
   `Sweeper` tick will refuse to claim a pending commit
   because the AST source probe is unhealthy.
2. **Wait for any `'scanning'` commits to finalize.**
   Query
   `SELECT COUNT(*) FROM clean_code.commit WHERE scan_status='scanning';`
   and wait for it to drop to zero (typically <30s; the
   sweep's PG transaction is short and bounded by
   `CLEAN_CODE_SCAN_TIMEOUT`).
3. **Optionally replay migration 0006 down.** The down
   migration drops the trigger and function BEFORE
   dropping the `repo_url` column, and revokes the
   targeted role grants. Existing `scan_run` rows and
   `commit.scan_status` values stay populated -- the
   Stage 3.1 lifecycle keeps working without 0006 in
   place. There is NO destructive backout of recorded
   metric_sample rows.
4. Restart the pod with the previous build; the AST
   source probe will report "not wired" and the sweeper
   will tick without claiming any pending commits until
   the next deploy.

There is no application-layer rollback of recorded
metric samples or finalized scan_runs; those are
append-only audit history by design.

## Stage 5.1: Policy Steward signing-key store

### One-time bootstrap (per environment)

1. **Generate the AES-256 master key** for the LocalSealedKMS:

   ```bash
   openssl rand -hex 32
   ```

   This is 64 hex chars. Store it in your secret manager (Key
   Vault, AWS Secrets Manager, Vault, ...) as
   `clean-code/kms/master-key`. **DO NOT commit it to source.**
   **DO NOT log it.**

2. **Apply migration `0005_policy_signing_keys`** against the
   shared Postgres instance. The migration grants:
   * `INSERT, SELECT` to `clean_code_policy_steward` (sole
     writer);
   * `SELECT` to every other writer role;
   * REVOKEs `UPDATE, DELETE` from every grantee (append-only).

   The table is created on `clean_code` schema and is
   independent of every other 0001-0004 migration.

3. **Set the env vars** on the long-running process:

   | Env var                           | Value                                                |
   | --------------------------------- | ---------------------------------------------------- |
   | `CLEAN_CODE_KMS_PROVIDER`         | `local`                                              |
   | `CLEAN_CODE_KMS_MASTER_KEY_HEX`   | the 64-hex master key from step 1                    |
   | `CLEAN_CODE_PG_URL`               | postgres connection string (with steward role auth)  |

   Production deploys MUST set all three. Scaffold-mode
   (`KMS_PROVIDER=""`) is dev-loop only and leaves the signing
   cache disabled.

### Per-rollout verification

After deploying a new build, confirm:

1. `/healthz` returns 200 within 5s of pod-ready.
2. `/readyz` flips to 200 within 30s -- the `signing_key_cache`
   readiness check passes once the KMS responds and at least
   one key is loaded.
3. `GET /v1/policy/keys/list_active` returns a single-entry
   array on first boot:

   ```bash
   curl -fsS http://$POD:8080/v1/policy/keys/list_active | jq .
   ```

4. The audit log shows `policy_signing_keys` writes by
   `clean_code_policy_steward` only -- no other role.

### Key compromise drill

If a private key is suspected compromised:

1. Page the on-call holder of `clean-code/kms/master-key`.
2. Run the `keys.compromise` runbook step (TBD in Stage 5.2).
   This calls `Manager.ForceRotate`, bypassing the 24h overlap
   guard.
3. Revoke the compromised key by inserting a new row and
   verifying the old row's `derived valid_until` falls inside
   the past (verification will fail at
   `now >= compromised_key.valid_until`).
4. Audit-log the rotation event with severity `high` and
   `kind=key_compromise`.

### Migration rollback

`0005_policy_signing_keys.down.sql` drops the table and its
index. **Never run on a production environment that has
already minted production keys** -- doing so destroys the
public-key record and every signed bundle becomes
unverifiable. The DOWN is reserved for clean-room dev-loop
re-bootstraps.

## Stage 5.2: Policy publish/activate/rulepack verbs

### Pre-requisites

This stage builds on the Stage 5.1 signing-key cache. Before
rolling out 5.2, confirm Stage 5.1's bootstrap (master key,
migration 0005, env vars, `signing_key_cache` readiness)
already lands green per the section above. The publish verbs
refuse to write when no active signing key is loaded
(`ErrNoActiveSigningKey` -> 503).

### One-time bootstrap (per environment)

1. **Apply migration `0003_policy_audit_refactor`** against
   the shared Postgres instance. It creates the canonical
   `clean_code.{policy_version, policy_activation, rule_pack,
   rule}` tables plus the `clean_code.rule_severity` ENUM and
   the supporting append-only column constraints.

2. **Confirm the role grants** from `0004_roles.up.sql` are in
   place: the steward role must have `INSERT, SELECT` on all
   four tables and MUST NOT have `UPDATE` or `DELETE`. The
   policy/rules sub-store is append-only per architecture G3.

3. **No additional env vars** are required beyond Stage 5.1.
   The composition root constructs the steward against the
   same `*sql.DB` handle the keys subsystem uses; when
   `CLEAN_CODE_PG_URL` is set the steward writes to PostgreSQL,
   otherwise it falls back to the in-memory store
   (development only).

### Per-rollout verification

After deploying a new build, confirm:

1. `/readyz` returns 200 within 30s (Stage 5.1 cache loaded).

2. **Bootstrap a rulepack BEFORE the first `policy.publish`.**
   `Steward.Publish` enforces the JSON-FK contract on
   `rule_refs` -- a publish naming a `(rule_id, version)` that
   has not yet been registered returns **400** with
   `error:"unknown_rule_ref"`. On a fresh environment you MUST
   publish the rulepack(s) you intend to reference first:

   ```bash
   curl -fsS -X POST http://$POD:8080/v1/policy/publish_rulepack \
     -H 'Content-Type: application/json' \
     -d '{
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
     }' | jq .
   ```

   Expect 200 with the inserted `rule_pack` + `rules` echoed.
   A 409 means the pack was already published in a prior
   rollout -- safe to skip and move on. A 503 means the
   signing-key cache isn't loaded; triage via the Stage 5.1
   runbook before re-running.

3. A canary `policy.publish` succeeds (this references the
   rule registered in step 2):

   ```bash
   curl -fsS -X POST http://$POD:8080/v1/policy/publish \
     -H 'Content-Type: application/json' \
     -d '{
       "name": "canary",
       "rule_refs": [{"rule_id": "solid.srp.lcom4_high", "version": 1}],
       "threshold_refs": [],
       "refactor_weights": {
         "alpha": 0.4, "beta": 0.3, "gamma": 0.2, "delta": 0.1,
         "effort_model_version": "v1.0",
         "window_days": 90
       }
     }' | jq .
   ```

   Expect 200 + a `policy_version_id`, `signature`, and
   `created_at`. A 400 with `error:"unknown_rule_ref"` here
   means step 2 was skipped (or the `rule_id`/`version` in
   this body does not match what step 2 registered). A 503
   indicates the signing-key cache isn't loaded -- triage via
   the Stage 5.1 runbook.

4. A `policy.activate` against the returned id succeeds:

   ```bash
   curl -fsS -X POST http://$POD:8080/v1/policy/activate \
     -H 'Content-Type: application/json' \
     -d '{"policy_version_id":"<uuid from step 3>","activated_by":"rollout"}' | jq .
   ```

5. The banned-verb paths return 501 (NOT 404 -- the route is
   intentionally mounted as a "verb is rejected" signal):

   ```bash
   curl -i -X POST http://$POD:8080/v1/policy/rulepack/add -d '{}'
   # HTTP/1.1 501 Not Implemented
   ```

6. The audit log shows `policy_version`, `policy_activation`,
   `rule_pack`, and `rule` writes by the
   `clean_code_policy_steward` role only -- no other role
   should appear in INSERT statements against these tables.

### Backout

Stage 5.2 is purely additive (new routes + new tables in
migration 0003). To back out:

1. Stop calling the new verbs from the operator dashboard
   client.
2. Restart the pod with the previous build; the new tables
   stay populated (append-only) but are no longer written.

There is no DOWN-migration step required for backout because
the existing append-only rows do not block the prior build.

## Stage 5.5: SOLID rulepack bootstrap

### What changes at rollout

`cmd/clean-coded/main.go` calls `solid.Bootstrap(ctx, steward)`
after `decoupling.Bootstrap`, publishing **5 SOLID rulepacks /
9 rules** via the same Steward verbs Stage 5.2 exposes
externally. Bootstrap is idempotent: re-runs against an
already-populated store report `PublishedPacks == 0` (see
`policy/rulepacks/solid/bootstrap.go`).

Inventory (matches the table in `docs/runbook.md` Stage 5.5):

| Pack         | Rules                                                                       |
| ------------ | --------------------------------------------------------------------------- |
| `solid.srp`  | `solid.srp.lcom4_high`, `solid.srp.interface_width_high`                    |
| `solid.ocp`  | `solid.ocp.fan_in_high`, `solid.ocp.modification_count_high`                |
| `solid.lsp`  | `solid.lsp.depth_of_inheritance_high`, `solid.lsp.override_violation`       |
| `solid.isp`  | `solid.isp.interface_width_high`                                            |
| `solid.dip`  | `solid.dip.fan_out_high`, `solid.dip.coupling_between_objects_high`         |

### Stage 2.4 producer dependency (carry-forward follow-up)

`solid.lsp.override_violation` reads `metric_kind='lsp_violation'`
at `scope_kind='method'`. That row is emitted by the **Stage 2.4
`recipes/lsp_violation.go` recipe** (architecture Sec 1.4.1
row 13, Sec 3.5.1.c dual encoding, implementation-plan Stage 2.4
line 221).

**The recipe is scheduled but not implemented yet.** Until
Stage 2.4 lands, the rule publishes cleanly but fires zero
violations because there are no input rows. The other 8 SOLID
rules are independent of Stage 2.4's LSP step and operate on
metric_kinds (`lcom4`, `fan_in`, `fan_out`,
`depth_of_inheritance`, `interface_width`,
`coupling_between_objects`, `modification_count_in_window`)
that Stage 2.4 foundation recipes and Stage 2.6 materialiser
already produce.

Tracking handoff (this dependency is recorded in three places
so it cannot be quietly dropped):

1. `docs/stories/code-intelligence-CLEAN-CODE/architecture.md`
   Sec 1.4.1 row 13 -- canonical catalogue declares the
   metric_kind.
2. `docs/stories/code-intelligence-CLEAN-CODE/implementation-plan.md`
   Stage 2.4 step "Implement `recipes/lsp_violation.go`"
   (line 221) plus the two e2e scoring scenarios
   `lsp-violation-strengthens-precondition` and
   `lsp-violation-compatible-override` (lines 232-233).
3. This rollout note + `docs/runbook.md` Stage 5.5
   (operator-facing data-starved-state guidance).

### Per-rollout verification

After deploying a build that includes Stage 5.5:

```bash
curl -fsS http://$POD:8080/v1/policy/rulepack/list_published \
  | jq '[.packs[] | select(.pack_id | startswith("solid."))] | length'
# 5
```

```bash
psql "$CLEAN_CODE_PG_URL" -c \
  "SELECT pack_id, count(*) FROM clean_code.rule
   WHERE pack_id LIKE 'solid.%' GROUP BY pack_id ORDER BY pack_id;"
#  solid.dip | 2
#  solid.isp | 1
#  solid.lsp | 2
#  solid.ocp | 2
#  solid.srp | 2
```

A row count of `0` for `solid.lsp` after deploy means
bootstrap failed -- check `/readyz` for the signing-key cache
(Stage 5.1) and the Steward writer (Stage 5.2). A count of
`2` confirms both LSP rules published;
`solid.lsp.override_violation` will sit data-starved until
Stage 2.4 ships.

### Backout

Stage 5.5 is purely additive (new `rule_pack` + `rule` rows
under the existing migration-0003 schema). To back out:

1. Restart the pod with a build that does not call
   `solid.Bootstrap`; the new rows remain (append-only) but
   no new SOLID rules are published.
2. Any policy already activated that referenced a `solid.*`
   rule continues to evaluate against the persisted `rule`
   row (the references are JSON-FK by `(rule_id, version)`,
   not Go-link by symbol).

There is no DOWN-migration required for backout. The
`solid.*` rows stay populated.


## Stage 5.3: `mgmt.override` write verb

### Migration

**None required.** The `clean_code.override` table (with the
`override_reason_required_when_muted` CHECK constraint and the
`override_rule_created_idx (rule_id, created_at DESC)` index)
shipped in migration 0003 during Stage 1.4. Stage 5.3 is the
first stage to actually write rows; no schema change is needed.

### Env vars

**None.** The verb reuses the existing `CLEAN_CODE_PG_URL`
(append target) and the auth gateway's existing
`X-OIDC-Subject` header contract that Stage 5.2 introduced. No
KMS key is required -- the override row carries no signature
column (kill-switch contract: the verb must work during a
signing-key outage).

### Per-rollout verification

After deploying a new build:

1. `/healthz` returns 200.
2. `/readyz` returns 200 (steward wired).
3. **Smoke-test the verb** with a known `rule_id` (the one the
   smoke-test rulepack registers in Stage 5.2):

   ```bash
   curl -X POST \
        -H 'Content-Type: application/json' \
        -H 'X-OIDC-Subject: rollout-smoke@operator.local' \
        --data '{"rule_id":"solid.srp.lcom4_high",
                 "scope_filter":{"repo_id":"smoke","scope_kind":"repo","scope_signature_glob":"*"},
                 "mute":true,"reason":"rollout smoke"}' \
        https://clean-coded.example.com/v1/mgmt/override
   ```

   Expect 200 + `{"override_id":"..."}`. A 503 indicates the
   steward is not wired; a 400 with "expires_at" or "actor_id"
   in the error body indicates a client typing the body the
   wrong way; a 401 indicates the gateway is not setting
   `X-OIDC-Subject`.

4. **Unmute the smoke row** with `mute=false` so the smoke
   rule is not silenced for real evaluator traffic. The
   evaluator (Stage 5.7) will read the LATEST row and see the
   row is back to unmuted.

5. The audit log shows `override` writes by the
   `clean_code_policy_steward` role only.

### Backout

Stage 5.3 is purely additive (new route + first writes against
the migration-0003 `override` table). To back out:

1. Stop calling `mgmt.override` from the operator dashboard.
2. Restart the pod with the previous build; the new route is
   gone but the `override` rows remain. The evaluator (Stage
   5.7) is not yet shipped, so the rows are inert.

There is no DOWN-migration required for backout. The
`override` table stays populated.
