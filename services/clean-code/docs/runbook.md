# `services/clean-code` runbook

Operational guide for the clean-code service. Add a new
section here as each subsystem ships against the production
composition root (`cmd/clean-code-metric-ingestor/main.go`,
which hosts the management + ingest HTTP surfaces; future
binaries under `cmd/clean-code-*` ship their own routes).

## Stage 9.4 -- OTel telemetry + Prometheus `/metrics` (all binaries)

Every `clean-code-*` binary now exports OpenTelemetry traces
via OTLP gRPC and exposes a Prometheus text-exposition
`/metrics` endpoint on its root HTTP mux. The span
attribute schema and metric names are the architecture-Sec-8
canonical surface for dashboards.

### Env vars

| env var | required | default | semantics |
|---|---|---|---|
| `CLEAN_CODE_OTEL_ENDPOINT` | no | `localhost:4317` (the local-dev OTel collector) | OTLP gRPC collector address. Override with the production collector hostname in deployment (e.g. `otel-collector.svc.cluster.local:4317`). Setting it to the empty string disables telemetry: the SDK uses its built-in noop tracer, `AnnotateEvalGateSpan` becomes a no-op via the `span.IsRecording()` short-circuit, and Prometheus collectors continue to record observations regardless (no scrape impact). Localhost / 127.0.0.1 endpoints default to plaintext gRPC; production endpoints SHOULD set telemetry options' `Insecure=false` so TLS is enforced. |
| `CLEAN_CODE_PROMETHEUS_ADDR` | no | binary-default | bind address for the HTTP listener exposing `/metrics`. Already established at Stage 3.5; this stage extends the collectors mounted on the existing handler. |

### Span taxonomy

Every verb span carries:

| attribute | type | source | notes |
|---|---|---|---|
| `verb` | string | `internal/api` gateway | canonical dotted verb (`mgmt.register_repo`, `eval.gate`, ...). |
| `repo_id` | string | request body parse | empty when the verb does not take a repo or the body has not been parsed yet. |
| `caller_subject` | string | verified bearer token `sub` claim | populated by the auth middleware. |
| `policy_version_id` | string | `evaluator.EvaluateResult.PolicyVersionID` | empty string when the verb does not bind to a policy_version (gateway default for non-eval verbs). |
| `degraded` | bool | `EvaluateResult.Degraded` | `false` default on every verb span at gateway entry. |
| `degraded_reason` | string | `EvaluateResult.DegradedReason` | empty string default; closed enum is `{samples_pending, policy_signature_invalid, xrepo_edges_unavailable}` per architecture Sec 6.1. |
| `verdict` | string | `EvaluateResult.Verdict` | empty string default; closed enum is `{pass, warn, block}`. |

The eval-gate-specific four (`policy_version_id`, `degraded`,
`degraded_reason`, `verdict`) are stamped via
`telemetry.AnnotateEvalGateSpan` from within
`composition.writeEvalResponse` BEFORE the operational-state
branch ladder, so even `ErrNoActivePolicy` (409) and
`INTERNAL_ERROR` (500) responses carry the partial state the
evaluator computed before failing.

### Prometheus collectors mounted per binary

| binary | collectors on `/metrics` |
|---|---|
| `clean-code-gateway` | `WALReplayMetrics`, `RuleEngineMetrics` |
| `clean-code-aggregator` | `AggregatorTickMetrics` |
| `clean-code-eval-gate` | `RuleEngineMetrics`, `WALReplayMetrics` |
| `clean-code-metric-ingestor` | `StaleScanRunSweepMetrics` (existing Stage 3.5 surface) |
| `clean-code-refactor-planner` | placeholder `/metrics` -- no observable counters currently exported. |

Metric names:

| name | type | semantic |
|---|---|---|
| `cleancode_aggregator_tick_duration_seconds` | histogram | Cross-Repo Aggregator tick wall-clock (Stage 7.1). |
| `cleancode_wal_replay_duration_seconds` | histogram | Audit WAL Reconciler `Run` wall-clock (Stage 9.2). |
| `cleancode_rule_engine_evaluations_total` | counter | total rule-engine evaluations completed (Stage 5.7). Dedup-cache hits (within `RunDedupTTL=30s` for same `(repoID, sha, policy_version_id, scope_id, caller)`) are NOT counted -- the counter tracks real evaluator work, not duplicate calls. |
| `cleancode_rule_engine_evaluations_by_verdict_total` | counter | per-verdict total; labels are the canonical `{pass, warn, block}` plus an `unknown` bucket that traps adapter bugs smuggling non-canonical verdicts. |

Default histogram buckets are
`[0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300]`
seconds. Compute `rate(cleancode_rule_engine_evaluations_total[5m])`
for the architecture-Sec-8 "evaluations/sec" SLO panel.

### Operator playbook

1. Stand up an OTel collector reachable from the binary's
   network (the canonical local-dev path is
   `deploy/local/otel/config.yaml` listening on
   `localhost:4317` plaintext).
2. Set `CLEAN_CODE_OTEL_ENDPOINT` on the binary's env. The
   binary will log `telemetry: OTel SDK initialised` at
   boot when the endpoint is non-empty.
3. Set `CLEAN_CODE_PROMETHEUS_ADDR` if the bind address
   differs from the binary default. Scrape the configured
   `/metrics` URL from Prometheus or the cluster-tier
   collector.
4. Confirm spans land in the OTel backend by issuing one
   `eval.gate` request and querying for
   `verdict in ("pass","warn","block")` -- empty result set
   means the binary's endpoint pin is wrong or the
   collector is dropping the OTLP-gRPC connection.

### Failure modes

- **Empty `CLEAN_CODE_OTEL_ENDPOINT`** -- intentional
  "telemetry disabled" path. Binary stays functional;
  spans become noop; Prometheus collectors continue to
  observe.
- **Collector unreachable** -- the OTel SDK's batch span
  processor enqueues spans regardless of connectivity and
  retries on the next flush. A sustained outage drops
  spans silently after the in-memory queue overflows (the
  SDK default is 2048 spans). Operators see queue-overflow
  warnings in the SDK's stdout. Prometheus is unaffected.
- **Service version not pinned (`ServiceVersion=""`)** --
  the resource attribute defaults to `"dev"`. Set it via
  the binary's build-time `-ldflags` injection so
  dashboards can chart per-build regressions.

## Stage 8.3 -- ML effort-model loader (clean-code-refactor-planner)

The `clean-code-refactor-planner` binary now loads an
external ML effort-model artefact at startup and stamps a
per-task hour estimate onto every emitted `refactor_task`,
replacing the Stage 8.2 `0.0` placeholder.

### Operator pins (env vars)

| env var | required | default | semantics |
|---|---|---|---|
| `CLEAN_CODE_REFACTOR_EFFORT_SOURCE` | no | `ML model from historical commits` | operator pin per architecture Sec 1.6. Closed set today: `"ML model from historical commits"` (REQUIRES a model artefact) or `"none"` (opt-out -- planner runs without an estimator and emits `effort_hours = 0.0`). Any other value refuses startup with the `ErrEffortModelSourceUnknown` sentinel. |
| `CLEAN_CODE_REFACTOR_EFFORT_MODEL_URI` | YES when source = `"ML model from historical commits"` | `""` | local-disk URI of the JSON-on-disk effort-model artefact. Accepted forms: bare local path (`/abs/path/model.json`, `C:\path\model.json`, `C:/path/model.json`, `./rel/path.json`); `file://` URI (`file:///abs/path` on POSIX, `file:///C:/path` on Windows). Schemes other than `file://` are refused. |

### Artefact format

JSON object:

```json
{
  "version": "v1.2.0-trained-2026-04-15",
  "kind_base_hours": {
    "split_class": 6.0,
    "extract_method": 1.5,
    "invert_dependency": 4.0,
    "break_cycle": 8.0,
    "consolidate_duplication": 3.0
  },
  "score_coef": 0.25,
  "intercept": 0.0
}
```

Strict-schema invariants enforced by `LoadModelFromFile`:

- `version` is non-empty after trim (the load-bearing pin
  per architecture Sec 5.3.3).
- Every value in
  `{split_class, extract_method, invert_dependency, break_cycle, consolidate_duplication}`
  has a finite, non-negative entry in `kind_base_hours`.
  Missing entries refuse load with `ErrEffortModelMissingKindBase`.
- `score_coef`, `intercept`, and every entry in
  `kind_base_hours` are IEEE-finite (no NaN / no Inf).
  Otherwise refuse with `ErrEffortModelNonFiniteCoefficient`.
- Unknown top-level fields refuse load with
  `ErrEffortModelMalformed` (the decoder uses
  `json.Decoder.DisallowUnknownFields`).

Per-task formula (deterministic, same input -> bit-identical
output):

    hours = max(0,
                kind_base_hours[task.kind]
              + score_coef * hot_spot.score
              + intercept)

### Version-pinning chain (architecture Sec 5.3.3)

The effort-model version is NOT duplicated on
`refactor_plan` or `refactor_task`. Reproducing an
estimate requires walking:

    refactor_task
      -> refactor_plan                                    (via refactor_task.plan_id)
      -> refactor_plan.hotspot_ids[0]
      -> hot_spot.policy_version_id                       (architecture Sec 5.5.1)
      -> policy_version.refactor_weights.effort_model_version

The loaded artefact's `version` MUST equal
`policy_version.refactor_weights.effort_model_version` for
every active policy snapshot the planner serves.
`EffortModel.Estimate` returns the
`ErrEffortModelVersionMismatch` sentinel when the snapshot's
`Weights.EffortModelVersion` differs from the loaded
`Version`; the error aborts the WHOLE batch (no plan row,
no task row lands) rather than silently emitting
`effort_hours = 0`.

### Startup failure modes

| condition | exit | log surface |
|---|---|---|
| `CLEAN_CODE_REFACTOR_EFFORT_SOURCE` set to anything other than the two pins | 1 | `ErrEffortModelSourceUnknown` |
| source = ML pin AND `CLEAN_CODE_REFACTOR_EFFORT_MODEL_URI=""` | 1 | `ErrEffortModelURIRequired` (names both env vars) |
| URI uses a scheme other than `file://` or a bare local path | 1 | `ErrEffortModelUnsupportedScheme` |
| artefact file missing | 1 | wrapped `os.ErrNotExist` |
| artefact JSON malformed / has unknown top-level fields | 1 | `ErrEffortModelMalformed` |
| artefact missing a canonical kind base or has non-finite coefficients | 1 | `ErrEffortModelMissingKindBase` / `ErrEffortModelNonFiniteCoefficient` |
| `version` empty after trim | 1 | `ErrEffortModelVersionEmpty` |

When source = `"none"` and URI is empty, the binary starts
normally; the composition root emits a `warn` log line
indicating the estimator is disabled and the planner will
emit `effort_hours = 0.0` for every task. Production
deployments should NOT use this path.

### Runtime failure modes (per planner call)

| condition | behaviour |
|---|---|
| `EffortModel.Version != snap.Weights.EffortModelVersion` | `Plan` returns `ErrEffortModelVersionMismatch`; NO plan or task rows are written. |
| `task.Kind` not in `EffortModel.KindBaseHours` (e.g. a custom rule mapper bypassed `ValidateTaskKind`) | `Plan` returns `ErrEffortModelMissingKindBase`; NO plan or task rows are written. |
| `hot_spot.score` is +/-Inf (defensive; Stage 8.1 never produces this) | `Plan` returns `ErrEffortModelNonFiniteCoefficient`; NO plan or task rows are written. |
| computed `hours < 0` (e.g. `intercept` set sufficiently negative) | clamped to `0`; estimate is still emitted. |

### Operator playbook

1. **Train / update model.** Publish a new artefact at the
   URI; bump `version` to a new unique string (e.g.
   `v1.3.0-trained-2026-06-01`).
2. **Publish a new policy version** through the Policy
   Steward with `refactor_weights.effort_model_version` set
   to the new artefact's `version`.
3. **Roll the planner.** Deploy `clean-code-refactor-planner`
   binaries pointing at the new URI. Old planners (still
   loaded with the previous artefact) will refuse to run
   the new policy at all (`ErrEffortModelVersionMismatch`);
   freshly-rolled planners will run the new policy and
   refuse the old (same sentinel). The blast radius is
   self-contained: a planner that finds a mismatched
   snapshot writes NOTHING, so partial-rollout state is
   never persisted.

### Code locations

- `internal/refactor/effort_model.go` -- loader, validator,
  estimator, URI resolver, sentinel errors.
- `internal/refactor/task_planner.go` --
  `WithEffortEstimator` option + the per-task estimator call
  inside `PlanFromSnapshot`.
- `internal/config/config.go` -- `EnvRefactorEffortModelURI`
  constant and `Config.RefactorEffortModelURI` field. NOT
  validated in `Config.Validate()`; the model interlock is
  binary-local to `clean-code-refactor-planner`.
- `cmd/clean-code-refactor-planner/main.go` -- composition
  root: calls `refactor.LoadFromConfig(cfg)` immediately
  after `config.Load()`; exits 1 on loader error; wires
  `refactor.WithEffortEstimator(model)` only when the
  loader returned a non-nil model.

## Stage 9.1 -- Audit WAL frame writer

The `internal/audit/wal/` package is the write-ahead log for
the three Audit tables (`evaluation_run`,
`evaluation_verdict`, `finding`). Every successful Audit
INSERT mints a signed `AuditFrame`, fsyncs it to a per-day
partition file under `data/wal/audit/YYYY-MM-DD.wal`, and
ONLY THEN commits the SQL transaction (architecture
Sec 7.10 / tech-spec Sec 4.13). Catalog, Measurement,
Policy, and Refactor writes DO NOT route through this WAL.

### Disk layout

- Root: `data/wal/audit/` (configured via
  `wal.WriterConfig.Dir`).
- Files: `YYYY-MM-DD.wal` (UTC), one per writer-clock day.
- Format: newline-delimited JSON (one frame per line, max
  1 MiB per frame).
- Per-frame fields: `frame_id`, `table`, `op`, `row_pk`,
  `row_json`, `written_at`, `signing_key_id`, `signature`.

The directory must be writable by the binary's user (the
writer creates it with `0o755` on first start). Partition
files are append-only -- the writer NEVER rewrites or
truncates an existing file.

### Operator checklist

1. **Disk capacity** -- partition files grow without an
   in-package rotation policy in Stage 9.1. Plan for
   ~1 KiB per Audit row (one `evaluation_run` + one
   `evaluation_verdict` + N `finding` rows per
   evaluation). Sizing: the post-Stage-9.2 reconciler will
   add a retention sweep; until then operators must
   monitor disk free space on the WAL volume.
2. **Fsync failures** -- a write that reaches the kernel
   but fails `fsync(2)` returns an error to the audit
   writer; the SQL transaction MUST roll back. The frame
   bytes MAY be readable on disk (the writer does not
   truncate after failure -- racy across sibling
   processes). The Stage 9.2 reconciler treats the readable
   frame as a "speculative" replay candidate keyed on
   `(table, row_pk)`. Today (Stage 9.1, no reconciler)
   operators must accept that a fsync error leaves an
   un-replayed frame on disk; raise an incident if `df`
   shows the WAL volume below 5%.
3. **Signature failures** -- a frame whose
   `signing_key_id` cannot be resolved to a public key at
   reconciler time will be quarantined (NOT applied). The
   policy KMS handle MUST be online when the reconciler
   runs in Stage 9.2; operators must coordinate KMS
   maintenance windows with the reconciler's run window.
4. **Crash recovery** -- a process crash mid-write may
   leave a partial trailing frame in the current partition
   file. The reader returns the sentinel
   `ErrTrailingPartialFrame` AND the complete frames
   decoded so far; the Stage 9.2 reconciler quarantines
   the tail bytes and continues from the next clean
   record. No operator action is required at Stage 9.1.

### Composition wiring

`Wal.Writer` is wired in the composition root of both
`cmd/clean-code-eval-gate/main.go` and
`cmd/clean-code-gateway/main.go`, and threaded into the
two audit-write Stores via
`rule_engine.SQLStoreConfig.WalWriter` and
`evaluator.SQLDegradedRunStoreConfig.WalWriter`. **Both
fields are REQUIRED** -- the two constructors error on
nil, and the composition-root configs
(`composition.EvalGateConfig.WalWriter`,
`evaluator.ProductionGateConfig.WalWriter`) likewise
error on nil. There is NO SQL-only fallback for Audit
INSERTs in Stage 9.1.

**No kill-switch.** There is no `CLEAN_CODE_AUDIT_WAL_DISABLED`
env var, no feature flag, no "audit WAL off" branch. The
two Audit-store constructors hard-error on a nil writer,
and unsetting `CLEAN_CODE_AUDIT_WAL_DIR` does NOT disable
the writer -- it falls through to the default
`data/wal/audit` directory. If the WAL volume is broken,
audit writes hard-fail with `WAL flush before SQL commit`
errors and the entire `evaluation_run` /
`evaluation_verdict` / `finding` triple is rolled back.

The env var `CLEAN_CODE_AUDIT_WAL_DIR` (default
`data/wal/audit`) selects the on-disk root. Both binaries
construct a `wal.NewWriter` with one of two signer
wirings, chosen at startup:

- **Production (KMS wired)** -- the binary calls
  `composition.NewKeysManagerWALSigner(*keys.Manager)`
  to adapt `keys.Manager.SignActive` to the writer's
  2-phase `wal.Signer.SignFrame` callback. Frames carry a
  non-zero `signing_key_id` and a real Ed25519 signature
  verifiable via `keys.Manager.Verify`.
- **Scaffold (KMS unset)** -- the binary falls back to
  `wal.NoopSigner{}` (SHA-256 stand-in, zero
  `signing_key_id`) and logs a loud `WARN` at startup.
  Intended for short-lived dev/test bring-up only.

Integration tests --
`internal/rule_engine/sql_store_wal_test.go` and
`internal/evaluator/sql_degraded_store_wal_test.go` --
exercise sqlmock + a real `wal.Writer` to prove every
successful `evaluation_run`, `evaluation_verdict`, and
`finding` INSERT pairs with a fsynced WAL frame. Both
tests cover the signer-failure rollback case AND the
write/fsync-failure rollback case (the per-day partition
path is pre-created as a directory to force a real disk
write failure at `os.OpenFile`). Stage 9.2 layers the
reconciler on top.

## Stage 9.2 -- Audit WAL Reconciler (replay-only)

The `internal/audit/reconciler/` package is the
replay-only restart sweep that walks `data/wal/audit/`,
verifies every frame's signature, and re-inserts MISSING
rows into the three Audit tables. **The reconciler never
modifies a non-Audit table, never deletes a row, never
overwrites an existing row, and preserves
`evaluation_run.caller` verbatim from the original frame.**
Stage 9.1's WAL writer pairs with this worker to close the
"WAL frame on disk, SQL row absent" gap caused by a fsync
error or a process crash between the WAL flush and the SQL
commit.

### Replay-only contract (architecture Sec 7.10)

1. Every Audit re-insert is issued as
   `INSERT INTO clean_code.<table> (...) VALUES (...)
   ON CONFLICT (<pk>) DO NOTHING`. A row whose
   `(table, row_pk)` already exists in PostgreSQL is
   classified as `OutcomeSkippedExisting` and left
   byte-identical.
2. There is NO `DELETE` / `UPDATE` statement anywhere in
   `internal/audit/reconciler/`. The package's PG role,
   `clean_code_wal_reconciler`, has `INSERT, SELECT`
   granted and `UPDATE, DELETE` revoked at the migration
   layer (migration `0004_roles.up.sql`). A bug in this
   package CANNOT escalate to a destructive write.
3. Only the three Audit tables are referenced; the table
   names live as package-level constants and the
   per-frame `replayOne` dispatcher uses an explicit
   `switch` on `wal.AuditFrame.Table`. A frame whose
   table is outside the closed set
   (`evaluation_run` / `evaluation_verdict` / `finding`)
   surfaces `ErrUnknownTable` and aborts the run --
   defence-in-depth on top of `wal.AuditFrame.Validate`.
4. `evaluation_run.caller` is bound verbatim from the
   parsed frame to the SQL parameter. There is NO
   substitution branch -- if the original writer recorded
   `caller='batch_refresh'`, the reconciler replays the
   row with `caller='batch_refresh'`, regardless of which
   process started the reconciler.

### Phased replay (FK ordering)

The reconciler walks every frame TWICE per `Run`:

1. **Pass 1** -- every `evaluation_run` frame, in WAL
   order. By the end of this pass every WAL-known
   `evaluation_run_id` exists in PostgreSQL.
2. **Pass 2** -- every `evaluation_verdict` and `finding`
   frame, in WAL order. The FK constraint
   `evaluation_verdict.evaluation_run_id ->
   evaluation_run.evaluation_run_id` (and the matching
   one on `finding`) is honoured EVEN IF a corrupted
   partition has reordered frames out of writer-order
   (which the writer never does, but the partition file
   is on durable disk and an external corruption pass
   cannot be ruled out).

### Verifier classification

`reconciler.Verifier` distinguishes three failure modes:

- **Durable-broken (skip + count)** -- the frame's
  signature did not validate
  (`reconciler.ErrSignatureInvalid`). The reconciler
  bumps `Stats.SkippedBadSig` and continues with the next
  frame. The operator MUST manually quarantine the
  affected partition bytes (see "Operator checklist"
  below) -- a counted skip is NOT a self-healing recovery.
- **Signing key not in trusted snapshot (skip + count)** --
  the frame's `signing_key_id` is not present in the
  historical-keys snapshot taken from
  `clean_code.policy_signing_keys` at reconciler
  construction time
  (`reconciler.ErrSigningKeyUnknown`). The reconciler
  bumps `Stats.SkippedBadSig` and continues. Note that
  "unknown" here means "not in the trusted snapshot", not
  "cryptographically unsignable": an attacker who minted
  a valid Ed25519 keypair can still produce a
  cryptographically-valid signature, but the historical
  verifier refuses to trust it.
- **Transient infrastructure (abort `Run`)** -- any other
  error from the verifier (DB outage during the initial
  snapshot fetch, ctx cancellation). The reconciler
  returns the error from `Run` so an operator can address
  the root cause before retrying. Silently skipping every
  frame on a verifier outage would erase the durability
  guarantee Stage 9.1 set up.

### Stats schema

`reconciler.Run` returns a `Stats` value with per-table
counters (`Replayed`, `SkippedExisting`, `SkippedBadSig`,
`SkippedBadShape`) and a `Warnings []string` channel for
non-fatal signals from `wal.ReadAll`
(`ErrTrailingPartialFrame`, `ErrFrameSizeExceeded`). The
post-Stage-9.4 OTel pipeline publishes these as
Prometheus counters with a `table` label; operators can
correlate skipped-row counts with disk artifacts.

`SkippedBadShape` covers PRE-signature structural
failures only -- a frame whose `wal.AuditFrame.Validate`
or `SigningPayload` rejected it before signature
verification could even run. POST-signature
disagreements (decode rejection under
`DisallowUnknownFields`, `frame.RowPK` disagreeing with
`row_json.<pk>`) are loud `Run` aborts -- they signal
writer-side schema drift or a durability-coordinate
corruption and warrant immediate operator triage rather
than silent skip.

### Operator checklist

1. **Trailing-partial-frame warning** -- benign for the
   reconciler (every complete frame preceding it
   replayed). Quarantine the tail bytes after the run by
   `cp data/wal/audit/<date>.wal /var/quarantine/`,
   truncating the original at the last complete-frame
   newline. Stage 9.4 will surface a Prometheus counter
   for the warning; until then `grep` the binary's stdout
   for "ErrTrailingPartialFrame".
2. **Frame-size-exceeded warning** -- pages an on-call.
   A single frame > 1 MiB is either a Stage 9.1 writer
   bug (no in-package check should let one through) or a
   forged frame. Quarantine the entire partition file
   and STOP further reconciliation runs until the source
   is identified.
3. **`SkippedBadSig > 0`** -- the affected frames carry
   either a tampered signature, a payload modified
   after signing, or were signed by a key whose UUID is
   not present in the historical-keys snapshot taken
   from `clean_code.policy_signing_keys` at reconciler
   startup. (Retired-but-known keys ARE in the
   snapshot and DO verify successfully -- the
   "unknown key" classification means the UUID is not
   in the trusted snapshot at all.) The reconciler does
   NOT replay these. Investigate the partition file by
   hand: re-parse the JSON, identify the
   `signing_key_id` and `row_pk`, and decide whether
   the underlying business event landed in PostgreSQL
   via a separate path or needs manual entry by a
   Policy Steward.
4. **`SkippedBadShape > 0`** -- the affected frames
   failed `wal.AuditFrame.Validate` or `SigningPayload`
   BEFORE signature verification. None of these can
   happen via the Stage 9.1 writer; investigate as a
   tamper / on-disk corruption signal. (Post-signature
   schema drift / RowPK mismatch are abort-Run, not
   counted here.)
5. **Run returns an error mentioning `decode failed AFTER
   valid signature` or `ErrRowPKMismatch`** -- the
   reconciler saw a frame whose `row_json` decoded
   incorrectly OR whose `RowPK` disagreed with
   `row_json.<pk>` AFTER the signature verified. This is
   writer-side schema drift OR a signing-key compromise.
   STOP further reconciliation runs, page the on-call,
   quarantine the partition, audit recent Policy Steward
   key-rotation events, and confirm the writer's audit
   columns match the current PG schema before retrying.
6. **Run returns an error mentioning
   `KeyStore.List` or snapshot construction** -- the
   historical-keys verifier could not build its
   trusted snapshot at construction time, typically
   because the `clean_code_wal_reconciler` role
   cannot SELECT from `clean_code.policy_signing_keys`
   (migration `0005_policy_signing_keys.up.sql` grants
   this) or the
   PostgreSQL pool is unreachable. The reconciler
   refuses to operate without a snapshot rather than
   silently classifying every frame as `SkippedBadSig`.
   Check the role grants (`\du` in psql, then
   `\dp clean_code.policy_signing_keys`) and the DSN
   reachability before restarting.
7. **Run returns any other error** -- the WAL reconciler
   did not complete. The most common cause is verifier
   transient error (KMS unreachable). Verify
   `policy.keys.list_active` returns the expected key,
   restart the reconciler. If the error mentions the
   Replayer (`reconciler: replayOne: ReplayRun ...`),
   check PostgreSQL connectivity and that the
   `clean_code_wal_reconciler` role still has
   `INSERT, SELECT` on the three Audit tables (it has by
   default; a manual `REVOKE` would break this).

### Composition wiring

The composition factory is
`composition.NewWALReconciler(ctx, WALReconcilerConfig)`
returning a `*reconciler.Reconciler`. The factory:

- Requires a `keys.Store` (`KeyStore` field) returning
  `(nil, nil)` when the store is nil so the binary
  branches "reconciler disabled" deliberately. The
  callers in `cmd/clean-code-eval-gate` and
  `cmd/clean-code-gateway` construct the store via
  `keys.NewSQLStore(reconcilerDB)`.
- Requires a non-nil `*sql.DB` authenticated as
  `clean_code_wal_reconciler` (migration 0004 grants
  INSERT+SELECT on the three Audit tables; UPDATE /
  DELETE are revoked service-wide; migration 0005
  additionally grants SELECT on
  `clean_code.policy_signing_keys` so the
  historical-keys verifier can build its snapshot).
- Requires a non-empty `Dir` -- production wiring threads
  `CLEAN_CODE_AUDIT_WAL_DIR` (default `data/wal/audit`)
  through, matching the writer's directory.
- Constructs the production `reconciler.SQLReplayer` and
  a `composition.NewHistoricalKeysWALVerifier`-backed
  Verifier; passes both into `reconciler.NewReconciler`.

A `*keys.Manager` is also accepted via the convenience
`Keys` field; the factory then takes the snapshot from
`Manager.HistoricalKeys()` (the manager must have
called `Load` first, which the production composition
helpers already do).

The verifier classification matrix:

| Verifier returned | Reconciler classification |
|---|---|
| `nil` | replay row |
| `reconciler.ErrSignatureInvalid` (sig wrong length OR `ed25519.Verify == false`) | SkippedBadSig |
| `reconciler.ErrSigningKeyUnknown` (UUID not in trusted snapshot) | SkippedBadSig |
| ctx error | propagated -> abort `Run` |
| any other error | propagated -> abort `Run` |

### Binary wiring (on-restart blocking step)

Both binaries (`cmd/clean-code-eval-gate/main.go` and
`cmd/clean-code-gateway/main.go`) run the reconciler as
a BLOCKING startup step BEFORE the HTTP listener
accepts traffic, so a missing reconciliation cannot
serve stale gate decisions or emit unreplayed audit
frames.

Required env var:

- `CLEAN_CODE_WAL_RECONCILER_DSN` -- PostgreSQL DSN
  whose auth user is a LOGIN role that has been
  granted `clean_code_wal_reconciler` membership
  (which is `NOLOGIN` per migration
  `0004_roles.up.sql:191`), with
  `options=-c role=clean_code_wal_reconciler` (or
  equivalent `SET ROLE`) so every statement is
  attributed to the reconciler role in
  `pg_stat_activity` and PG audit logs. See
  `docs/rollout.md` Stage 9.2 -> "DSN connection
  pattern (NOLOGIN + SET ROLE)" for the operator
  flow.

  The reconciler opens its OWN dedicated pool from
  this DSN; reusing the gateway's evaluator /
  solid_batch pools is NOT permitted. Note that
  those roles ALSO carry `INSERT, SELECT` on the
  three Audit tables (migration
  `0004_roles.up.sql:455-465` -- the three writers
  share one append-only path by design) and ALSO
  carry `SELECT` on `clean_code.policy_signing_keys`
  (migration `0005_policy_signing_keys.up.sql:169-171`),
  so the prohibition is NOT a grant-matrix gap.
  Instead the dedicated DSN exists for three
  operational reasons:

  1. **Activity attribution.** With
     `options=-c role=clean_code_wal_reconciler`,
     reconciler writes show up under a distinct role
     in `pg_stat_activity` and PG audit logs.
     Mixing them with evaluator / solid_batch
     activity would erase the
     `EvaluationRun.caller='reconciler'` <->
     PG-role correspondence operators rely on for
     post-hoc forensics.
  2. **Future grant tightening.** Keeping the
     reconciler on a separate role makes it safe to
     tighten its grants in future migrations
     (e.g. narrow INSERT to specific columns,
     restrict by row-level security) without
     touching the hot-path evaluator / solid_batch
     grants.
  3. **Connection budgeting.** The reconciler holds
     a connection for the entire on-restart sweep.
     Borrowing from the evaluator / solid_batch
     pools would steal capacity from the hot
     serving paths during boot -- precisely when
     those paths need to come up cleanly. A
     dedicated pool sized for the one-shot sweep
     avoids that contention.

Boot matrix:

- `signingKeys == nil` (scaffold mode, no KMS wiring) ->
  reconciler is skipped; the binary logs
  "WAL reconciler: skipped (no signing keys configured)"
  and proceeds. This matches Stage 9.1's writer, which
  also skips frame signing in scaffold mode.
- `signingKeys != nil` AND
  `CLEAN_CODE_WAL_RECONCILER_DSN` UNSET -> boot is
  REFUSED (`log.Fatalf` in eval-gate, error return in
  gateway). A silent skip would mean pending WAL
  frames sit on disk unreplayed forever; that is the
  exact failure mode Stage 9.2 exists to prevent.
- `signingKeys != nil` AND DSN SET -> reconciler runs
  to completion, logs `Stats`, and yields to the rest
  of boot. On `Run` error the binary aborts so the
  operator can investigate before traffic is served.

## Stage 8.2 -- Refactor plan and task generation

This section captures the operator-facing contract of the
Stage 8.2 add-ons to `internal/refactor/`: the `TaskPlanner`
orchestrator that emits `refactor_plan` rows + per-hot_spot
`refactor_task` rows from the top-N hot_spots at one
`(repo_id, sha)`.

### Process layout

Stage 8.2 runs inside `cmd/clean-code-refactor-planner/main.go`
as a one-shot K8s Job (NOT a cadence loop). The operator
schedules ONE pod per (repo_id, sha) — typically tied to a
scan completion event — and the binary:

1. Loads `CLEAN_CODE_PG_URL` plus the per-job env vars
   `CLEAN_CODE_REFACTOR_PLANNER_REPO_ID` (uuid) and
   `CLEAN_CODE_REFACTOR_PLANNER_SHA`.
2. Opens a libpq handle (retries up to 30s for slow-starting DBs).
3. Calls Stage 8.1 `Planner.Plan(ctx, repoID, sha)` — which
   reads + scores + writes the `clean_code.hot_spot` batch.
4. Calls Stage 8.2
   `TaskPlanner.PlanFromSnapshot(ctx, repoID, sha, planRes.Snapshot)`
   — which READS the latest top-N hot_spot rows (the ones
   the previous step just wrote) and emits the
   `refactor_plan` + `refactor_task` rows under one
   transaction.
5. Exits 0 on success; non-zero on any unhandled error.

`TaskPlanner` is wired with FOUR dependencies — `PolicyReader`,
`HotSpotReader` (NEW in iter 2: `WHERE created_at = (SELECT
MAX(created_at) ...)` latest-batch lookup against
`clean_code.hot_spot`), `FindingDetailReader` (production:
`SQLFindingDetailReader` against `clean_code.finding`), and
`RefactorPlanTaskWriter` (production:
`SQLRefactorPlanTaskWriter` against
`refactor_plan` + `refactor_task` in one transaction). The
DB role `clean_code_refactor_planner` is already granted
`INSERT, SELECT` on the three target tables (migration
`0004_roles.up.sql:482-509`) AND now ALSO needs `SELECT` on
`clean_code.hot_spot` for the new latest-batch read (the
existing grant set covers this — `hot_spot` SELECT was
already in scope because Stage 8.1 needed to upsert and read
it back, but operators should grep their role audit to
confirm a custom role spec inherits the same row).

**Single-writer assumption:** the latest-batch query uses
`WHERE created_at = (SELECT MAX(created_at) ...)`. This is
correct under the architecture's "one
clean-code-refactor-planner pod per (repo, sha) at a time"
invariant. If two pods race the same (repo, sha) tuple they
will both insert hot_spot rows at near-identical timestamps
and the MAX(created_at) row may end up being a mix of the
two batches — operators MUST gate the K8s Job spec with a
`uniqueness key = (repo_id, sha)` constraint
(architecture Sec 5.5.1).

### Race-safe wiring via `PlanFromSnapshot`

Stage 8.2 deliberately uses
`TaskPlanner.PlanFromSnapshot(ctx, repoID, sha, snap)` rather
than `TaskPlanner.Plan`. The standalone `Plan` re-reads the
active policy, which would race a concurrent
`policy.activate` between Stage 8.1 and Stage 8.2 — the
returned policy_version_id could differ from the one stamped
on every `hot_spot` row just persisted. Passing the Stage 8.1
`PlanResult.Snapshot` closes the race at the type level.

### Knobs (`policy_version.refactor_weights`)

- `top_n` (Stage 8.2 NEW, optional int) -- maximum number
  of hot_spots a single `refactor_plan` row covers. Zero
  means no truncation (plan covers every scored hot_spot);
  positive truncates to the top-N by composite score
  DESC; negative is rejected at publish time. The hot_spot
  table is always written in full (architecture Sec 5.5.1
  append-only) -- `top_n` only affects plan coverage and
  emitted tasks.
- `effort_model_version` -- Stage 8.3 effort-model pin.
  Stage 8.2 emits `refactor_task.effort_hours = 0.0` as the
  unestimated placeholder; Stage 8.3 backfills.
- All Stage 8.1 weights (`alpha`, `beta`, `gamma`,
  `delta`, `window_days`) flow through to Stage 8.2
  unchanged: the Stage 8.1 `Planner.Plan` pass uses them
  to score and persist the `hot_spot` batch; the Stage 8.2
  `TaskPlanner.PlanFromSnapshot` pass inherits the same
  `PolicySnapshot` and consults `Weights.TopN` only.

### What the planner writes

For each `cmd/clean-code-refactor-planner` invocation
(one-shot K8s Job per `(repo_id, sha)`):

1. The Stage 8.1 `refactor.Planner.Plan` pass writes ALL
   scored hot_spots via `HotSpotWriter`
   (`clean_code.hot_spot`), in canonical sort order
   (Score DESC, ScopeID ASC). Stage 8.1 is the SOLE writer
   of `clean_code.hot_spot`.
2. The Stage 8.2 `refactor.TaskPlanner.PlanFromSnapshot`
   pass then READS the LATEST top-N rows back from
   `clean_code.hot_spot` (via the new `HotSpotReader` →
   `SQLHotSpotReader.LatestHotSpotsByScore`, pinned by
   `policy_version_id = $snap.PolicyVersionID`) -- it does
   NOT recompute scores and does NOT write `hot_spot`.
3. ONE `refactor_plan` row persists via
   `RefactorPlanTaskWriter.WriteRefactorPlanAndTasks`
   (atomic transaction with the tasks below) covering the
   top-N hot_spots in `hotspot_ids JSONB`. Carries NO
   `policy_version_id` column -- recover the policy via
   any referenced hot_spot row.
4. ZERO OR MORE `refactor_task` rows in the same transaction,
   one per unique `(scope_id, rule_id)` qualifying finding
   pair. A hot_spot with NO qualifying findings IS still
   listed in `plan.hotspot_ids` but emits ZERO tasks (the
   planner refuses to fabricate a synthetic rule_id).

### `task.kind` canonical enum

The five values per architecture Sec 5.5.3 line 1274:

| kind                       | typical trigger                                    |
| -------------------------- | -------------------------------------------------- |
| `split_class`              | SOLID SRP / ISP violation (split class / interface)|
| `extract_method`           | SOLID OCP / LSP / unmapped rule fallback           |
| `invert_dependency`        | SOLID DIP / high CBO / high fan_in / high fan_out  |
| `break_cycle`              | `decoupling.cycle_member` / `decoupling.cycles.*`  |
| `consolidate_duplication`  | `decoupling.duplication*`                          |

The iter-3 alias set
`extract_function | introduce_interface |
reduce_inheritance | reduce_coupling | reduce_lcom |
reduce_duplication` is REJECTED. A rule pack that emits a
finding whose rule_id maps to an alias kind (via a custom
`WithRuleKindMapper`) ABORTS the whole `TaskPlanner.Plan`
batch with `ErrRejectedTaskKindAlias`; no plan row, no
task row lands. Operators see the rejection in the
planner's structured-log error wrap.

### Failure modes / recovery

| symptom                                            | likely cause                                                                   | action                                                                                       |
| -------------------------------------------------- | ------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------- |
| `ErrNoActivePolicy`                                | No `policy_activation` row at planner runtime                                  | Activate a policy via the steward verb; planner is idempotent and will produce on next loop |
| `ErrInvalidTopN`                                   | Composition root injected a snapshot with `TopN < 0`                           | Republish the policy with a non-negative `top_n`; legitimate publishes are blocked already   |
| `ErrRejectedTaskKindAlias` / `ErrUnknownTaskKind`  | Custom rule mapper returned a non-canonical kind                               | Inspect the failing rule_id in the error wrap; remove the override or map to a canonical kind|
| `write plan+tasks: ... transaction rollback`       | Connection drop mid-batch                                                      | Re-run the planner -- the transaction guarantees no orphan plan landed                       |

### Effort-model version recovery

Stage 8.2 emits `effort_hours = 0.0`. To recover the
effort-model version a task was scored against:

```
refactor_task.task_id
  -> refactor_task.plan_id
  -> refactor_plan.hotspot_ids[0]
  -> hot_spot.policy_version_id
  -> policy_version.refactor_weights.effort_model_version
```

Stage 8.3 will replace the placeholder; the traversal path
above stays canonical.


## Stage 7.1 -- Cross-Repo Aggregator cadence loop

This section captures the operator-facing contract of the
`internal/aggregator/` package -- the cadence-driven worker
that materialises `repo_metric_snapshot`,
`cross_repo_percentile`, and `portfolio_snapshot` from the
active `metric_sample` rows (architecture Sec 3.10 /
Sec 5.2.4 - Sec 5.2.6, tech-spec Sec 8.2
`aggregator_cadence`).

### Process layout

The aggregator runs in its own loop (`internal/aggregator/Loop`)
inside the `cmd/clean-code-aggregator/main.go` binary, separate
from the metric_ingestor and management binaries. Exactly
ONE aggregator process should run per environment -- the
INSERT-only writes are not coordinated across replicas (a
second writer would produce duplicate snapshot rows sharing
the same `built_at`). The PG role `clean_code_xrepo_aggregator`
is the only role with `INSERT, SELECT` on the three snapshot
tables; `UPDATE` and `DELETE` are explicitly `REVOKE`d
(migration `0004_roles.up.sql` lines 395-397 / 416-418).
On the read side the same role ALSO has `INSERT, SELECT`
on `metric_sample` and `metric_retraction` plus
`INSERT, SELECT, UPDATE` on `metric_sample_active` per the
system-tier MetricSample writer carve-out (tech-spec Sec 7.2
lines 1348-1364); the `pack='system' AND source='derived'`
discriminator that keeps the aggregator off the
metric_ingestor's pack='ingested' surface is enforced at the
application layer, NOT at the PG ACL layer.

### Knobs (`internal/config/config.go`)

- **`CLEAN_CODE_AGGREGATOR_CADENCE`** (Go duration; default
  `15m` per tech-spec Sec 8.2). Period between aggregator
  ticks. Must be `> 0`.
- **`CLEAN_CODE_DISABLE_AGGREGATOR`** (bool; default `false`).
  Skips composition-root wiring of the aggregator loop. Used
  during the Stage 7 rollout when the binary ships but
  operators want to gate the worker until snapshot reader
  consumers exist.

### What it writes per tick

On every tick the aggregator:

1. Captures a single `built_at = clock.Now().UTC()` value.
2. Reads ACTIVE samples via the canonical join
   `metric_sample_active msa JOIN metric_sample ms JOIN
   scope_binding sb LEFT JOIN metric_retraction mr
   WHERE mr.sample_id IS NULL AND ms.value IS NOT NULL`.
   Degraded `value IS NULL` rows are filtered at the SQL
   layer; NaN / +-Inf are filtered in Go.
3. Buckets observations by `(repo_id, metric_kind,
   scope_kind)` -> writes ONE `repo_metric_snapshot` row per
   bucket carrying `count`, `mean`, `p50`, `p90`, `p99`.
4. Groups observations by `(metric_kind, scope_kind)` -> writes
   ONE `cross_repo_percentile` row per cohort with cross-repo
   `p50`/`p90`/`p99` computed over the FLAT observation-value
   set across ALL contributing repos (architecture Sec 3.10
   line 644: "the full per-metric percentile vector across all
   repos" -- large repos with more observations therefore carry
   proportionally more weight in the cohort percentile by
   design) + `histogram_json = {"entries":[{repo_id,count,mean,
   p50,p90,p99}, ...]}` sorted by `repo_id`. The unweighted
   per-repo view (size-equal weighting per repo) is what the
   `histogram_json` entries + the sibling
   `portfolio_snapshot.aggregate_json.unweighted_mean` field
   surface for the operator portfolio UI.
5. Writes ONE `portfolio_snapshot` row per `(metric_kind,
   scope_kind)` with `aggregate_json = {total_observations,
   repo_count, weighted_mean, unweighted_mean, p50, p90,
   p99, per_repo[{repo_id,count,mean}, ...]}`.
6. INSERTs all rows in a SINGLE transaction so a tick either
   lands completely or not at all. Snapshot rows are
   append-only: readers always JOIN to `MAX(built_at) BY
   (metric_kind, scope_kind)` or analogous.

### Operator-visible counters

`aggregator.Report` (returned from `Loop.runOnce`'s
structured log) carries:

- `built_at` -- the timestamp shared by every row this tick
- `observations_read` -- active rows pulled this tick
- `cohorts_aggregated` -- distinct `(metric_kind, scope_kind)`
  cohorts seen
- `repo_metric_snapshot_rows`, `cross_repo_percentile_rows`,
  `portfolio_snapshot_rows` -- INSERT counts per table

A healthy tick log line at INFO level looks like:

```
level=INFO msg="aggregator loop: Tick succeeded"
  built_at=2025-09-14T12:00:00Z observations_read=3712
  cohorts_aggregated=12 repo_metric_snapshot_rows=180
  cross_repo_percentile_rows=12 portfolio_snapshot_rows=12
```

### Failure handling

A Tick error does NOT terminate the loop. The error is
logged at WARN and the loop sleeps `errorBackoff` (default:
equal to cadence) before the next attempt. The only way the
loop exits is ctx cancellation (SIGTERM). Operators should
alert on:

- `observations_read=0` for > 2 ticks in a row when the
  ingester is known healthy (suggests a JOIN regression or
  `metric_sample_active` pointer-swap bug)
- repeated WARN "Tick failed" lines -- inspect the wrapped
  `err` for the PG error code

### Snapshot table writer identity

Per architecture G1 / Phase 1.5 sub-store grants, the
aggregator is the SOLE writer for `repo_metric_snapshot`,
`cross_repo_percentile`, `portfolio_snapshot`. Any other
process writing to those tables is a policy violation.
Validate by running:

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

Expected: `clean_code_xrepo_aggregator` has INSERT only;
no other role has INSERT/UPDATE/DELETE.

## Stage 7.3 -- Insights percentile freshness banner

This section captures the operator-facing contract of the
percentile-freshness banner attached to the Management
latest-dashboard read verbs `mgmt.read.cross_repo` and
`mgmt.read.portfolio` (Stage 6.3). The banner is the
Insights-surface envelope decoration documented at
architecture Sec 7.5 / tech-spec Sec 8.2
`freshness_window_seconds`; the implementation lives in
`internal/management/insights/freshness.go` and is wired
into `internal/management/reader.go` via the
`WithInsightsFreshness(*insights.Freshness)` option. The
production constructor is
`insights.NewPercentileFreshness()` (window =
`FreshnessWindowSeconds = 3600s`, clock = `SystemClock`).

### What the banner does

On EVERY call to `mgmt.read.cross_repo(metric_kind,
scope_kind)` or `mgmt.read.portfolio(metric_kind)` the
Reader:

1. Resolves the latest `cross_repo_percentile` (resp.
   `portfolio_snapshot`) row through the configured
   `MetricsBackend` (architecture Sec 6.3).
2. Passes the row's `built_at` to
   `insights.Freshness.Evaluate(builtAt)`.
3. If `now() - built_at > 3600s`, stamps the response
   envelope's `degraded=true` and
   `degraded_reason="percentile_stale"`. Otherwise the
   envelope carries `degraded=false` and the
   `degraded_reason` field is omitted from JSON.

`mgmt.read.portfolio` aggregates the WORST-CASE across the
fetched rows -- `Degraded=true` iff ANY row's `built_at`
is stale, and `OldestBuiltAt` echoes the oldest row's
`built_at` so an operator can attribute the staleness
verdict to a specific snapshot.

### Wire shape on a stale read

A response from `GET /v1/mgmt/read/cross_repo?metric_kind=
arch_debt_ratio&scope_kind=repo` against a stale snapshot
looks like:

```json
{
  "row": {
    "metric_kind":   "arch_debt_ratio",
    "scope_kind":    "repo",
    "p50":           0.18,
    "p90":           0.41,
    "p99":           0.72,
    "histogram_json": "{...}",
    "built_at":      "2026-05-27T17:02:11Z"
  },
  "degraded":        true,
  "degraded_reason": "percentile_stale",
  "built_at":        "2026-05-27T17:02:11Z"
}
```

A FRESH response omits `degraded_reason` and emits
`"degraded": false`.

### Boundary semantics

`Freshness.Evaluate` uses a strict-greater-than comparison
(`age > Window`) so a row whose age EQUALS the window is
treated as FRESH; one second past the window flips it to
stale. The boundary contract is pinned by
`TestFreshness_BoundaryAtExactWindowIsFresh` and
`TestFreshness_OneSecondPastBoundaryIsStale`
(`internal/management/insights/freshness_test.go`).

Edge cases the verb handles WITHOUT operator intervention:

- **Empty `built_at` (`time.Time{}` zero value)** -- some
  backends return this when the underlying table is empty.
  Evaluate treats it as STALE so an unpopulated dashboard
  cannot silently render as "fresh".
- **Future `built_at` (writer clock ahead of reader clock)**
  -- treated as FRESH; the resulting negative age never
  satisfies `> Window`. The Insights surface does not
  police clock drift.
- **Nil `Clock`** -- a `Freshness{Clock: nil}` falls back
  to `SystemClock` rather than panicking, so a
  composition-root wiring bug cannot crash the hot read
  path.

### INSIGHTS-ONLY: NOT a gate signal

`percentile_stale` is the canonical "Insights-only"
degraded reason. The `eval.gate` verb's degraded-reason
taxonomy (architecture Sec 8.2) is the closed four-value
set `{samples_pending, policy_signature_invalid,
xrepo_edges_unavailable, ast_subprocess_unavailable}`; the
gate's writer REJECTS `percentile_stale` with the sentinel
`ErrInvalidGateDegradedReason` BEFORE any SQL is issued
(`internal/evaluator/verdict.go`,
`internal/evaluator/gate_evaluate.go:writeDegraded`,
`internal/evaluator/sql_degraded_store.go`). The carve-out
is pinned by
`TestDegradedReason_IsValidForGate_RejectsPercentileStale`,
`TestGate_writeDegraded_RejectsPercentileStaleReason`, and
`TestSQLDegradedRunStore_RejectsPercentileStaleReasonBeforeSQL`
(verdict_test.go) plus the Stage 6.1 e2e scenario
`percentile-stale-not-on-gate`.

Operational implication: a dashboard showing
`degraded_reason="percentile_stale"` MUST NOT be treated as
a block/warn input to a deploy gate. It is a *dashboard
staleness* signal -- the underlying ACTIVE
`metric_sample` rows are still consumable by `eval.gate`
even when the percentile cohort summary has not been
refreshed within the hour.

### Operator triage on `percentile_stale`

A persistent `degraded_reason="percentile_stale"` typically
indicates the cross-repo aggregator loop is not ticking.
Triage steps:

1. Inspect the Stage 7.1 aggregator binary's structured
   log for the "aggregator loop: Tick succeeded" line.
   Absence for > 1 hour is the canonical trigger.
2. Confirm `CLEAN_CODE_DISABLE_AGGREGATOR` is not set to
   `true` on the aggregator deployment (Stage 7.1).
3. Confirm exactly ONE aggregator replica is running
   (Stage 7.1 single-replica invariant).
4. Query the table directly:

   ```sql
   SELECT metric_kind, scope_kind, MAX(built_at) AS latest
     FROM clean_code.cross_repo_percentile
    GROUP BY metric_kind, scope_kind
    ORDER BY latest;
   ```

   The `latest` column lets you attribute the
   `percentile_stale` verdict to the specific cohort whose
   `built_at` is oldest -- the same cohort
   `OldestBuiltAt` echoes for `mgmt.read.portfolio`.

The banner DOES NOT auto-clear: once the aggregator
resumes ticking, the next snapshot insert advances
`built_at` and the verb's next read returns
`degraded=false` without operator intervention.

### Auto-default wiring (defence-in-depth)

A composition root that calls `management.NewReader(...)`
WITHOUT `WithInsightsFreshness` AUTOMATICALLY receives the
production-canonical `insights.NewPercentileFreshness()`
(window = 3600s, clock = `SystemClock`). This auto-default
exists so a wiring slip cannot silently render a stale
snapshot as fresh. A composition root that genuinely needs
to suppress the banner -- e.g. a developer-mode replay
harness or a unit test seam -- MUST opt out explicitly via
`management.WithoutFreshness()`.

`WithoutFreshness()` is a DEVELOPER/TEST SEAM, NOT a
production rollback knob. As of Stage 7.3 there is no
production composition root that calls
`management.NewReader(...)` -- a literal grep over
`services/clean-code/cmd/` confirms this (see the rollout
guide's "State of the read surface today" subsection).
When the follow-on read-surface stage lands and introduces
the first production Reader-wiring binary (a sibling
helper to the existing
`cmd/clean-code-metric-ingestor/main.go:mountMgmtRoutes`
write-verb mount, or a new `cmd/clean-code-mgmt-read/`),
that binary MUST NOT call `WithoutFreshness()`. A PR that
adds `WithoutFreshness()` to a production composition
root MUST be reviewed as a release-blocking change and
the operator on call MUST be paged before it merges. If
the banner is firing during an incident, the correct
response is to fix the aggregator (Stage 7.1 triage
above), not to suppress the signal.

## Stage 6.2 -- `mgmt.register_repo` and `mgmt.set_mode`

This section captures the operator-facing contract of the
Stage 6.2 management write verbs
`mgmt.register_repo(repo_url, default_branch, mode?)` and
`mgmt.set_mode(repo_id, mode)`. See
`internal/management/register_repo_verb.go`,
`internal/management/set_mode_verb.go`, and
`internal/management/mgmt_surface.go` for the HTTP surface.

### Canonical verb paths and mount

The two Stage 6.2 verbs share the existing
`clean-code-metric-ingestor` binary's HTTP server (the
binary that already hosts the Stage 3.4 management verbs
`mgmt.retract_sample` / `mgmt.rescan`). The unified
canonical `mgmt.*` surface is composed by
`MgmtSurfaceRoutes(mgmt *MgmtWriter, policy *PolicyWriter)`,
which mounts every write verb in the canonical set
(`mgmt.override, mgmt.register_repo, mgmt.set_mode,
mgmt.retract_sample, mgmt.rescan`) onto a single
`*http.ServeMux`. Each path is gated on its backing writer
being non-nil so the wire surface NEVER advertises an
endpoint it cannot serve. See
`cmd/clean-code-metric-ingestor/main.go:mountMgmtRoutes`
for the production composition root.

- **`POST /v1/mgmt/register_repo`**. Body:
  `{ "repo_url": "<https URL>", "default_branch": "<name>",
     "mode": "embedded"|"linked", "modes": "embedded"|"linked",
     "display_name": "<str?>" }`.
  The wire accepts EITHER `mode` (the singular column name)
  OR `modes` (the brief's plural-form parameter) as a JSON
  string -- specifying both fields in the same request
  returns HTTP 400. When BOTH are omitted, mode defaults to
  the canonical `embedded` per architecture Sec 1.6
  `ast-mode-default`. `display_name` defaults to the
  path-tail of `repo_url` when omitted (e.g.
  `https://github.com/org/repo` -> `repo`; `.git` suffix
  stripped; SCP-style `git@github.com:org/repo.git` handled)
  so the `repo.display_name NOT NULL` column always has a
  value.
- **`POST /v1/mgmt/set_mode`**. Body:
  `{ "repo_id": "<uuid>", "mode": "embedded"|"linked" }`.

### Authentication and actor attribution

Both verbs REQUIRE `X-OIDC-Subject: <subject>` on every
request. The subject is the OIDC-authenticated principal; it
is stamped into the resulting `repo_event.payload.actor` as
`"operator:<subject>"`. A missing or empty header returns
HTTP 401 BEFORE any persistence happens.

### Atomicity invariant (audit log integrity)

Both verbs write BOTH the catalog row AND the matching
`repo_event` atomically. The `RepoStore` implementation owns
that boundary. In production the
`management.NewPGRepoStore(mgmtDB)` implementation opens a
single Postgres transaction per verb: `register_repo` takes
a per-URL `pg_advisory_xact_lock(hashtext($repo_url::text))`
so concurrent registrations of the same URL serialise on the
write path (different URLs do not block each other); the
SELECT-by-URL lookup then runs inside the lock, so the
"check then INSERT" sequence is race-free even though the
`clean_code.repo` table has no UNIQUE constraint on
`repo_url`. `set_mode` takes `SELECT mode ... FOR UPDATE`
on the `repo` row, computes the transition (no-op vs
`embedded` <-> `linked`), and writes the matching
`mode_changed` event in the same transaction. Either both
the row mutation AND the event are durably committed, or
neither is. Operators can rely on the invariant "every
`repo_event` has a matching `repo`-row state and vice versa"
for incident triage.

### `mgmt.register_repo` semantics

- **Happy path (200, `created:true`).** A new row is inserted
  into `clean_code.repo` with the requested
  `(repo_url, default_branch, mode, display_name)`. A
  `repo_event(kind='registered',
  payload={repo_url, default_branch, mode, display_name,
  actor})` is appended. Response:
  `{ "repo_id": "<uuid>", "created": true,
     "mode": "embedded"|"linked" }`.

- **Idempotent on `repo_url` (200, `created:false`).** A
  second call with the same `repo_url` returns the existing
  `repo_id` and sets `created:false`. NO second `registered`
  event is appended. The existing row's `mode` is echoed back
  even if the new request asked for a different mode --
  callers MUST use `mgmt.set_mode` to change the mode of an
  existing repo. This idempotency is required because the
  `clean_code.repo` schema does NOT enforce a unique
  constraint on `repo_url`; the store layer is the gate.

### `mgmt.set_mode` semantics

- **Transition (200, `changed:true`).** The row's `mode` is
  updated and a `repo_event(kind='mode_changed',
  payload={mode, previous_mode, actor})` is appended.
  Response: `{ "repo_id": "<uuid>", "mode": "<new>",
  "previous_mode": "<old>", "changed": true }`.
- **No-op (200, `changed:false`).** A call that re-asserts
  the existing mode (e.g. `mode:"embedded"` against a repo
  already at `embedded`) returns 200 with `changed:false`
  and appends NO `mode_changed` event. `mode_changed`
  records a TRANSITION, not a re-assertion. This is a
  deliberate audit-log hygiene rule: every `mode_changed`
  row in the audit log implies a real transition.

### Status code matrix

| Code | When                                                            | Verbs affected         |
| ---- | --------------------------------------------------------------- | ---------------------- |
| 200  | Happy path (incl. idempotent register and no-op set_mode)       | register_repo, set_mode |
| 400  | Empty / whitespace `repo_url`; empty `default_branch`; invalid `mode` / `modes` (anything outside `{embedded, linked}`); BOTH `mode` AND `modes` supplied in the same request (`ErrMgmtRegisterRepoBothModeAndModes`); malformed JSON; unknown body field; zero / malformed `repo_id` | both |
| 401  | Missing or empty `X-OIDC-Subject`                               | both                   |
| 404  | `set_mode` against an unknown `repo_id`                         | set_mode               |
| 405  | Non-POST method                                                 | both                   |
| 500  | Unexpected RepoStore error (NOT a known sentinel)               | both                   |
| 503  | `repoStore` was not wired into the writer                       | both                   |

### Operator triage

- **HTTP 503 from either verb.** The composition root did
  NOT wire a `RepoStore` into `MgmtWriter` via
  `WithMgmtWriterRepoStore`. Inspect the binary's
  composition (`cmd/clean-code-metric-ingestor/main.go`,
  `mountMgmtRoutes`) and restart with the store wired.
  Until then, the two paths return 503 and the rest of the
  mgmt surface (retract_sample, rescan, override) is
  UNAFFECTED.
- **HTTP 400 with body mentioning BOTH `mode` and `modes`.**
  A caller sent both wire fields in the same request --
  this is rejected to prevent silent precedence ambiguity.
  The caller MUST pick exactly one: the singular `mode`
  (matches the `repo.mode` column and the `mgmt.set_mode`
  verb) OR the plural `modes` (matches the brief signature
  `register_repo(repo_url, default_branch, modes)`). Both
  forms are accepted; the wire is forgiving so operators
  who copy-paste from the brief and operators who copy-paste
  from the column docs both work without a 400.
- **HTTP 400 with `unknown field "actor"`.** A caller is
  trying to forge the actor identity. Actor is sourced
  ONLY from `X-OIDC-Subject` -- the body cannot override it.
- **Mismatched `repo_event` and `repo` row.** This is a hard
  invariant violation -- see the "Atomicity invariant"
  section. If you observe one without the other in
  Postgres, capture the rows, file a P1, and inspect the
  store implementation; the in-memory implementation
  rolls back on appender failure and the production
  Postgres-backed implementation does the write in a
  single transaction.
- **Multiple `registered` events for the same `repo_id`.**
  This is also a hard invariant violation -- the
  `register_repo` verb is idempotent on `repo_url` and
  appends `registered` only when `created:true`. Inspect
  the audit log for non-canonical writers
  (`repo_event.actor NOT LIKE 'operator:%'`).

## Stage 6.1 -- `eval.gate` verb and synchronous SOLID delegation

This section captures the operator-facing contract of the
Stage 6.1 verb `eval.gate(repo_id, sha, scope?)`. See
`cmd/clean-code-eval-gate/main.go` for the HTTP surface and
`internal/evaluator/gate_evaluate.go` for the verb itself.

### Two HTTP routes -- canonical vs. admin

The `clean-code-eval-gate` binary exposes TWO routes; each
has a distinct contract.

- **`POST /v1/eval/gate`** (canonical). Body:
  `{ "repo_id": "<uuid>", "sha": "<hex>", "scope": "<uuid?>" }`.
  Per architecture Sec 3.7 lines 548-570, the verb's signature
  is `eval.gate(repo_id, sha, scope?)` -- it does NOT take a
  `policy_version_id`. Step (1) of the brief mandates that
  the gate resolve the active `policy_version_id` itself via
  the latest `clean_code.policy_activation` row. A
  caller-supplied `policy_version_id` is REJECTED with HTTP
  400 and an error body pointing the caller at
  `/v1/eval/replay`. This guards against a rogue client
  pinning findings to an inactive (or revoked) policy and
  bypassing the steward's activation governance.

- **`POST /v1/eval/replay`** (NON-CANONICAL admin). Body:
  `{ "repo_id": "<uuid>", "sha": "<hex>", "policy_version_id": "<uuid>", "scope": "<uuid?>" }`.
  Required for batch tooling, reconciler replay, and dry-runs
  against a candidate policy that has not yet been activated.
  Callers MUST be authenticated with an admin grant; bind
  this route to an admin-only network path (e.g. internal
  subnet, mTLS-gated) so unauthenticated clients cannot pin
  evaluations to arbitrary policies.

### HTTP response shape (both routes)

Both routes return the same JSON shape on success:

```json
{
  "evaluation_run_id":     "<uuid>",
  "evaluation_verdict_id": "<uuid>",
  "finding_ids":           ["<uuid>", ...],
  "verdict":               "pass" | "warn" | "block",
  "degraded":              false | true,
  "degraded_reason":       "" | "policy_signature_invalid" | "samples_pending"
}
```

Verdict is the canonical closed enum `pass | warn | block`
(architecture Sec 5.4.2). `degraded_reason` is restricted to
the eval.gate closed set: `samples_pending`,
`policy_signature_invalid`, or `xrepo_edges_unavailable`.
The Insights-only `percentile_stale` is REJECTED at the
gate's writer boundary.

### HTTP status code matrix

| Status | Meaning |
| --- | --- |
| 200 OK | Happy path (engine-written run+verdict+findings) OR degraded short-circuit (`degraded=true`, `verdict=warn`, zero findings). |
| 400 Bad Request | Invalid `repo_id`, invalid/missing `sha`, malformed JSON, OR `policy_version_id` smuggled into `/v1/eval/gate`. |
| 405 Method Not Allowed | Non-POST. |
| 409 Conflict | `ErrNoActivePolicy` -- no `clean_code.policy_activation` row exists yet (fresh-deploy steady state). Activate a policy via the canonical `policy.activate` verb (`POST /v1/policy/activate` on the Policy Steward) before invoking `eval.gate`. |
| 500 Internal Server Error | Storage outage, broken adapter, non-canonical verdict from the engine. Inspect logs. |

### Sequence per architecture Sec 3.7

1. Resolve active `policy_version_id` via the latest
   `clean_code.policy_activation` row. No row → HTTP 409
   (NOT a degraded path: `evaluation_run.policy_version_id`
   is non-nullable so no audit row can be written without
   a pvid).
2. Fetch the resolved `policy_version` row; verify the
   persisted signature against the canonical bytes of THIS
   policy_version. On mismatch take the
   **`policy_signature_invalid` degraded short-circuit**:
   ONE `evaluation_run(caller='eval_gate', ...)` + ONE
   `evaluation_verdict(verdict='warn', degraded=true,
   degraded_reason='policy_signature_invalid')` in one
   transaction. Zero findings, NO Rule Engine invocation
   (architecture Sec 1.6 `policy-signing-required` -- the
   gate never blocks but always records the audit trail).
3. Check Phase 2 sample readiness via
   `clean_code.commit.scan_status`. If not `'scanned'`
   take the **`samples_pending` degraded short-circuit**:
   ONE run + ONE verdict (same shape as above but
   `degraded_reason='samples_pending'`), zero findings,
   no Rule Engine invocation.
4. **Clean path**: delegate to
   `rule_engine.RunSync(ctx, repoID, sha, scope, pvid)`.
   The engine writes ONE `evaluation_run` + ONE
   `evaluation_verdict` + N `finding` rows in ONE
   transaction and returns their IDs. eval.gate does NOT
   write its own `evaluation_verdict` row on the clean
   path; the engine's verdict (severity-rollup: `block`
   if any unmuted finding has `severity='block'`; `warn`
   if any has `severity='warn'`; `pass` otherwise) is
   canonical.

### Operator triage

- **HTTP 409 from `/v1/eval/gate`**: no active policy. Bind
  one via the canonical `policy.activate` verb -- e.g.
  `curl -fsS -X POST http://<steward>:8080/v1/policy/activate -d '{"policy_version_id":"<uuid>","actor_id":"<uuid>"}'`
  (see the Stage 5.2 `policy.activate` section below for the
  full body schema). No audit row is written for the 409
  case; do NOT expect a `caller='eval_gate'` row in
  `evaluation_run`.
- **HTTP 200 with `degraded=true, degraded_reason='policy_signature_invalid'`**:
  the persisted signature did not verify. Check the steward
  signing key (was it rotated without re-signing the
  policy?). The gate already wrote the audit row; replay
  with the corrected signature via `/v1/eval/replay` once
  the policy is re-signed.
- **HTTP 200 with `degraded=true, degraded_reason='samples_pending'`**:
  the SHA is not yet `scan_status='scanned'`. Inspect the
  underlying state with a direct table read --
  `SELECT scan_status FROM clean_code.commit WHERE repo_id = '<uuid>' AND sha = '<hex>'`
  -- and wait for the metric ingestor to catch up
  (`commit.scan_status` is written ONLY by the Metric
  Ingestor's terminal-transition writer, documented in
  the Stage 3.2 section below). The gate already wrote
  the audit row so the caller has a record of "decision
  made under samples-pending."
- **HTTP 200 with `verdict='block'`, no degraded**: a SOLID
  rule fired with `severity='block'`. Inspect
  `evaluation_verdict.evaluation_run_id` → `finding` rows
  for the rule_ids that blocked.
- **HTTP 500**: storage outage OR the engine returned a
  non-canonical verdict (`fail`/`gated`). Both are
  programming errors; check logs.

## Stage 5.7 iter 4 -- production wiring updates

This subsection captures the operator-facing changes that
shipped with Stage 5.7 iter 4. The base Stage 5.7 section
below remains canonical for the engine and worker behaviour.

### Two new environment variables

- `CLEAN_CODE_SOLID_BATCH_PG_URL`: DSN authenticated as
  `clean_code_solid_batch` for rule-engine Audit writes.
  When unset the composition root falls back to the main
  `CLEAN_CODE_PG_URL` handle with a WARN log. Required under
  production least-privilege.
- `CLEAN_CODE_EVALUATOR_PG_URL`: DSN authenticated as
  `clean_code_evaluator` for the new `clean-code-eval-gate`
  binary. Used for the degraded-path Audit writes and for
  the `commit.scan_status` readiness reads. Falls back to
  `CLEAN_CODE_PG_URL` when unset.

### New binary: `clean-code-eval-gate`

The production gate composition root now lives under
`cmd/clean-code-eval-gate`. It exposes `POST /v1/eval/gate`
and returns the canonical
`{evaluation_run_id, evaluation_verdict_id, finding_ids[], verdict, degraded, degraded_reason?}`
shape. The Verdict on BOTH degraded paths
(`policy_signature_invalid`, `samples_pending`) is `warn`
per architecture Sec 3.7 + operator pin
`gate-degraded-policy=warn`.

### Durable catchup loop

`cmd/clean-code-metric-ingestor` now:

1. Bounds the `scanEvents` channel emit by 5s. Saturation
   surfaces as a latency spike + log line, NOT a silent
   permanent drop.
2. Runs `rule_engine.Worker.Catchup` on startup AND every
   5 minutes against `SQLPendingScanReader`. The reader
   selects `commit.scan_status='scanned'` rows missing an
   `evaluation_run` for the active policy and pages at
   100 rows per call.

### Active-row metric_sample reads

`SQLStore.ListMetricSamples` now JOINs through
`clean_code.metric_sample_active` so retracted / inactive
samples cannot trigger findings. The query also hydrates
`pack`, `source`, `degraded`, and `degraded_reason` so DSL
predicates over those canonical fields evaluate correctly.

### Per-scope predicate evaluation

The rule engine evaluates predicates per SCOPE via the new
`dsl.Predicate.EvalAtScope` contract. This enables SOLID
composite recipes such as SRP's
`threshold(lcom4) AND threshold(interface_width)` to fire
at a class scope when the class has BOTH a high-LCOM4
sample AND a wide-interface sample, even though no single
sample carries both metric_kinds.

## SOLID Rule Engine batch worker and synchronous mode (Stage 5.7)

### What

The Rule Engine subsystem is a small stack of types in
`internal/rule_engine/`:

- `Engine` (`engine.go`) -- the in-process actor that
  consumes the active `policy_version`, compiles each rule's
  predicate via `dsl.Cache`, evaluates the predicate over
  `metric_sample` rows for the target SHA, computes the
  `new`/`newly_failing`/`unchanged`/`resolved` delta for
  every emitted finding, and writes ONE `evaluation_run` +
  ONE `evaluation_verdict` + N `finding` rows in a single
  `Store.AppendEvaluation` transaction.
- `Worker` (`worker.go`) -- the long-running batch-refresh
  driver consuming `ScanEvent{RepoID, SHA}` from a channel
  (the post-scan dispatcher). The worker resolves the active
  `policy_version_id` via the `PolicyActivationReader` port,
  then calls `Engine.RunBatch(ctx, repo, sha,
  policy_version_id)` for each event.
- `Store` (`store.go`) -- the atomic-write boundary. The
  production implementation will issue
  `BEGIN; INSERT evaluation_run; INSERT evaluation_verdict;
  INSERT findings...; COMMIT;` as one Postgres transaction.
  Tests use the `InMemoryStore` (`inmem_store.go`) drop-in.

### Two callable modes (canonical signatures)

**Synchronous mode -- `eval.gate` invokes the engine in the
same call:**

```go
result, err := engine.RunSync(ctx, repoID, sha, scopeID, policyVersionID)
// returns: result.EvaluationRunID, result.EvaluationVerdictID, result.FindingIDs
```

`caller='eval_gate'`. The gate uses the three returned IDs
to attach its HTTP response to the canonical audit rows.

**Batch-refresh mode -- the post-scan dispatcher invokes
the engine after a SHA's metric samples land:**

```go
result, err := engine.RunBatch(ctx, repoID, sha, policyVersionID)
```

`caller='batch_refresh'`. The dispatcher emits one
`ScanEvent` per newly-scanned SHA; `Worker.Run` drains the
channel and calls `RunBatch` per event.

Both modes write the SAME row set in the SAME transaction.
The engine -- not eval.gate -- is the canonical writer of
`evaluation_verdict` whenever the rule pass is invoked.

### Writer-ownership grid (Phase 1.5 grants)

| Path                      | Run + Verdict writer       | Finding writer            |
|---------------------------|----------------------------|---------------------------|
| Synchronous rule pass     | Rule Engine (`eval_gate`)  | Rule Engine               |
| Batch refresh             | Rule Engine (`batch_refresh`) | Rule Engine            |
| Gate degraded (sig-invalid, samples_pending) | `clean_code_evaluator` | (none -- 0 findings) |
| WAL replay                | `clean_code_wal_reconciler` | `clean_code_wal_reconciler` |

The three Audit tables (`evaluation_run`,
`evaluation_verdict`, `finding`) are granted INSERT in
parallel to `clean_code_solid_batch` (the engine's batch
worker), `clean_code_evaluator` (the gate degraded paths),
and `clean_code_wal_reconciler` (replay only) per tech-spec
Sec 7.2 lines 1256-1261.

### Operating the batch worker

- One worker per service instance is sufficient. The
  engine's per-`(repo, sha)` `sync.Mutex` serialises
  read-modify-write windows across concurrent gate calls;
  multiple workers competing for the same event stream
  would only add coordination overhead.
- The worker logs at INFO on every successful
  `Engine.RunBatch` with the freshly-written
  `evaluation_run_id`, the rollup `verdict`, and the
  `findings_count`. Grep for `rule_engine.worker:` to surface
  per-event lines.
- On a transient activation-lookup error the worker logs at
  ERROR and proceeds to the next event. Repeated
  ERROR-level lines from one operator typically point at a
  stale `policy_activation` table; resolve by reactivating
  the latest published `policy_version` via
  `policy.activate`.
- On a missing active policy (no `policy_activation` row
  yet) the worker logs at INFO and skips the event. This is
  the expected fresh-deploy steady state.

### Manually invoking `RunSync` for diagnosis

When a developer wants to reproduce a gate decision
offline (e.g. to debug a "why did this SHA block?"
incident), they can shell into the service and call
`RunSync` directly via the wired `Engine`. The engine
**deduplicates** invocations within the configured TTL
window (default **30 seconds**, exported as
`rule_engine.DefaultRunDedupTTL` and overridable via
`Config.RunDedupTTL`; see the public `Engine.RunSync`
+ `Store.LookupRecentCanonicalRun` -- the cache uses
the private `Engine.runDedupTTL` field threaded
through to the Store lookup). A diagnostic call with
the same `(repo_id, sha, policy_version_id, scope_id,
caller)` tuple as a recent run returns the existing
canonical run+verdict IDs rather than mint a fresh
audit row. This is the production cross-replica
dedup contract from migration 0008 +
architecture §5.4.2; the runbook MUST NOT contradict it.

To force a fresh diagnostic row when the operator wants
one (e.g. to see findings against an updated rule set or
to reproduce after fixing a sample-ingestion bug),
choose ONE of the following:

- **Vary `policy_version_id`** -- publish a new
  `policy_version` (or activate a different existing
  one) and pass its ID; this is the canonical way to
  evaluate a SHA against a different rule set.
- **Vary `scope_id`** -- a per-scope diagnostic
  evaluation against the same SHA writes a distinct
  audit row because the dedup tuple includes
  `scope_id` (null-safe via `IS NOT DISTINCT FROM`).
- **Wait out the TTL** -- after `runDedupTTL` has
  elapsed since the most recent canonical row, the
  next `RunSync` call mints a new run.

Diagnostic rows are written under the canonical
`clean_code_solid_batch` grant (the Rule Engine's own
writer ownership), with `caller='eval_gate'` or
`caller='batch_refresh'` matching whichever entry path
the developer triggered. If the diagnostic finds new
findings worth muting from regression counts, use the
overrides UI to mark the resulting `evaluation_run_id`
as a "manual repro" so the Insights surface excludes it
from rollups.

### Mute semantics

An active `override` row with `mute=true` whose
`scope_filter` matches a candidate scope causes the engine
to **emit no finding row** for that scope+rule pairing on
the current SHA (per Stage 5.7 brief scenario
`muted-scope-skipped`). This deviates from architecture
Sec 5.3.6's "preserve as info" wording; the chosen
behaviour is documented in `engine.go`.

If `mute=false` (the unmute case), the engine emits the
finding row as normal -- the override has no effect on a
non-muted scope.

### Resolved findings

When a prior SHA's `(scope, rule)` tuple produced a
`severity=block` finding and the current SHA's predicate
does NOT fire (sample absent, value below threshold, or
the engine returns no row), the engine emits a
`delta=resolved`, `severity=info` row at the current SHA.
Resolved rows are EXCLUDED from the verdict severity
rollup so a clean SHA receives `verdict=pass` even though
a resolved-finding row was written.

The engine emits AT MOST one resolved row per tuple per
SHA; subsequent SHAs where the tuple remains absent do
NOT emit a duplicate resolved row (the engine consults
`LatestPriorFinding` and short-circuits when the latest
prior row already has `delta=resolved` or a non-block
severity).

### Severity rollup

The verdict for a run is `MAX(severity)` over the firing
findings, with `pass < warn < block`:

| Findings                          | Verdict |
|-----------------------------------|---------|
| none                              | pass    |
| only `info`                       | pass    |
| at least one `warn`, no `block`   | warn    |
| at least one `block`              | block   |

`delta=resolved` rows are excluded from the rollup.

### Production composition root

The Rule Engine is wired by
`cmd/clean-code-metric-ingestor/main.go` at process start
(function `startRuleEngineWorker`). The wiring fans
together:

| Layer                          | Type                              | Role                                                                  |
|--------------------------------|-----------------------------------|-----------------------------------------------------------------------|
| `*sql.DB` (libpq)              | `database/sql`                    | The canonical Postgres handle (env `CLEAN_CODE_PG_URL`).              |
| `*steward.SQLStore`            | `internal/policy/steward`         | Reader for `policy_version`, override matcher.                        |
| `*steward.Steward`             | `internal/policy/steward`         | Exposes `ActivePolicyVersion(ctx)` -- the single source of truth.     |
| `*rule_engine.SQLStore`        | `internal/rule_engine`            | Audit-table writer + metric_sample / commit reader.                   |
| `*rule_engine.Engine`          | `internal/rule_engine`            | In-process actor for `RunSync` + `RunBatch`.                          |
| `chan rule_engine.ScanEvent`   | std chan (buffered, cap=64)       | Post-scan dispatcher channel; emit is non-blocking on the HTTP path.  |
| `*rule_engine.Worker`          | `internal/rule_engine`            | Consumes ScanEvent + drives `Engine.RunBatch`.                        |
| `NewStewardActivation(stew)`   | `internal/rule_engine`            | Adapter projecting `ActivePolicyVersion` → `ActivePolicyVersionID`.    |

The `handleProcess` HTTP handler emits exactly one
`ScanEvent` on every successful transition to
`scan_status='scanned'`. The emit uses a `select` with
a 5-second bounded `time.After` branch
(`scanEventEmitTimeout`): if the worker is stalled and
the channel remains full for the full 5 seconds, the emit
logs the line
`rule_engine: scan event channel saturated after 5s -- event WILL BE REPROCESSED BY CATCHUP repo_id=<id> sha=<sha> ...`
and returns. The HTTP request still succeeds because the
durable catchup loop (next paragraph) replays any
`scan_status='scanned'` SHA that lacks a
`(caller='batch_refresh', degraded=false)` evaluation_run
within ~5 minutes -- saturation surfaces as a latency
spike + log line, NOT silent permanent loss. Operators
can tune channel capacity by recompiling with a different
`scanEventCapacity` constant (v1 ships with 64); the
catchup loop is the durability backstop and SHOULD NOT
be tuned away.

The composition root also runs
`rule_engine.Worker.Catchup` at startup and on a
5-minute ticker. Catchup pages over commits where
`scan_status='scanned'` AND no canonical batch-refresh
evaluation_run exists yet under the active policy
(`NOT EXISTS` anti-join on
`caller='batch_refresh' AND degraded=false`); each page
processes through the same `Engine.RunBatch` path as the
live channel. The reader orders by
`(committed_at, repo_id, sha)` (Stage 5.7 evaluator iter-5
feedback #1: `clean_code.commit.committed_at` is the
canonical timestamp column -- earlier iters incorrectly
used `created_at`, which does not exist) and pages via a
keyset cursor over the same tuple
(`(committed_at, repo_id, sha) > ($t, $r, $s)`). Catchup
pins the active policy version at the TOP of each
invocation so a policy switch mid-run does not deadlock
the anti-join, and advances the cursor by the LAST row of
every page regardless of per-event success or failure --
a persistent poison row at the head no longer starves
later valid SHAs within the same invocation (Stage 5.7
evaluator iter-5 feedback #2). The loop terminates when a
page comes back empty or short (`len < limit`); per-event
errors are logged and retried fresh on the next tick.

### Cross-replica canonical-run dedup

The engine performs an additional Store-level lookup
(`Store.LookupRecentCanonicalRun`) before writing a fresh
canonical `(evaluation_run, evaluation_verdict, findings)`
triple. The lookup runs INSIDE the
`pg_advisory_xact_lock` envelope, so when two replicas
race for the same `(repo, sha, policy_version)` the second
replica's RC-isolated SELECT observes the first replica's
just-committed canonical row and returns the SAME IDs
without minting a duplicate audit row (Stage 5.7 evaluator
iter-5 feedback #3 + iter-6 feedback #2).

**Both callers are covered (iter-7).** Migration 0008
(`evaluation_run_scope_id`) adds a nullable
`evaluation_run.scope_id uuid` column plus the
`evaluation_run_dedup_idx` composite index on
`(repo_id, sha, policy_version_id, caller, scope_id,
created_at DESC)`. The lookup filters with the null-safe
`IS NOT DISTINCT FROM` operator so a scoped eval_gate row
NEVER matches an unscoped call (or vice versa); the engine
now consults the Store-level lookup for both
`caller='batch_refresh'` and `caller='eval_gate'` with the
call's `scopeID`. The previous iter-6 limitation (eval_gate
fell back to in-process cache only) is closed.

**PendingScanCursor visibility caveat (open question
resolution).** The 5-minute catchup ticker uses a keyset
cursor over `(committed_at, repo_id, sha)`. A SHA inserted
into `commit` AFTER the catchup loop's current invocation
started but BEFORE the cursor reached its
`committed_at` may not be visible to THAT invocation; the
NEXT tick (default 5 minutes) re-issues `PendingScans`
from cursor=nil and will pick it up. This is acceptable
under the durability contract: the live event channel
emits the SHA immediately, so the catchup loop is
strictly a safety net.

The wiring is opt-out via
`CLEAN_CODE_RULE_ENGINE_DISABLED=1`. When the env var is
set the worker is NOT composed and the post-scan emit is
a no-op (`scanEvents == nil`), so the binary continues
serving `/v1/ingestor/process` unchanged.

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

## `mgmt.retract_sample` and `mgmt.rescan` (Stage 3.4)

### What

Two operator-facing HTTP verbs that drive the
Measurement-sub-store retraction and rescan flows. Both are
mounted on the Management surface (`internal/management/mgmt_verbs.go`)
and delegate the actual sub-store writes to the Metric
Ingestor -- the Management surface NEVER writes Measurement
rows directly per architecture Sec 6.3.

- `POST /v1/mgmt/retract_sample` -- canonical name
  `mgmt.retract_sample`. Body: `{"sample_id":"<uuid>","reason":"<free-form>"}`.
  Actor is sourced from the `X-OIDC-Subject` header (NOT the
  body -- a caller cannot spoof attribution).
- `POST /v1/mgmt/rescan` -- canonical name `mgmt.rescan`.
  Body: `{"repo_id":"<uuid>","sha":"<commit-sha>"}`. Actor is
  sourced from `X-OIDC-Subject`.

### Retract flow

The handler executes the architecture-pinned sequence:

1. **Validate body shape.** `DisallowUnknownFields` rejects
   any caller attempt to include `actor` in the body
   (status 400). The trust boundary is the auth gateway, not
   the JSON.
2. **Resolve `sample_id` -> `(repo_id, sha)`.** A missing
   sample returns 404 BEFORE any `repo_event` row is
   appended -- an intent log entry for a non-existent sample
   would be misleading audit noise.
3. **Append `repo_event(kind='retract_intent', payload={sample_id, reason})`.**
   `retract_intent` is the canonical RepoEvent.kind value per
   architecture Sec 5.1.4 line 883 (the canonical enum is
   `registered | retired | retract_intent | mode_changed`).
   This is the operator-intent audit row -- it lands BEFORE
   the Measurement-sub-store writes so the operator's intent
   survives even if the dispatcher fails.
4. **Dispatch to `metric_ingestor.RetractDispatcher`.** The
   dispatcher opens a `scan_run(kind='retract', sha_binding='single',
   status='running', to_sha=<sample.sha>)`, appends a
   `metric_retraction(retraction_id, sample_id, reason, appended_by, created_at)`
   row in the Measurement sub-store, and transitions the
   scan_run to `succeeded` (or `failed`).
5. **Return** the persisted retraction row + the scan_run_id.

`appended_by` is stamped `operator:<X-OIDC-Subject>` per
architecture Sec 5.2.2 line 1033.

### Retract idempotency

A second `mgmt.retract_sample` call for the
already-retracted sample is a no-op:

- The dispatcher (`metric_ingestor.RetractDispatcher.Dispatch`)
  consults `RetractionStore.Lookup(sample_id)` BEFORE
  opening a fresh `scan_run(kind='retract')`. If the
  lookup finds an existing retraction the dispatcher
  short-circuits and returns
  `{Retraction=existing, ScanRunID=uuid.Nil, Inserted=false}`
  -- NO scan_run row is opened (iter 2 fix; prior to iter 2
  the dispatcher opened a duplicate scan_run row that
  immediately transitioned to `succeeded`).
- The dispatcher's `RetractionStore.Append` is also
  idempotent at the storage layer on `sample_id`
  (the schema's `UNIQUE` on `metric_retraction.sample_id`
  enforces this at the DB layer; the in-memory store
  mirrors it; the `PGRetractionStore` uses
  `INSERT ... ON CONFLICT (sample_id) DO NOTHING
  RETURNING ...` with a fallback `Lookup` so the PG
  contract matches the in-memory contract bit-for-bit).
  This belt-and-braces second guard catches the rare
  race where TWO concurrent dispatches Lookup-miss and
  both call Append.
- **Race-loser path**: When the Lookup-first probe finds
  no existing row but the Append fails on the UNIQUE
  collision (a concurrent retract raced ahead), the
  dispatcher finalises its already-opened scan_run as
  `succeeded` (preserving an honest audit trail of the
  attempt) and returns
  `{Retraction=existing, ScanRunID=<real-id>, Inserted=false}`
  -- NOT `uuid.Nil`. The race-loser scan_run row IS
  visible in the table because the operator's intent
  did reach the dispatcher; only the
  `metric_retraction` write was suppressed.
- The wire response on the sequential idempotent path
  carries the EXISTING `retraction_id`,
  `inserted=false`, and `scan_run_id == uuid.Nil` -- no
  new scan_run was opened, so there is none to point at.
- The `repo_event` intent log is APPEND-ONLY -- a retry
  appends a second `retract_intent` row even though only
  one `metric_retraction` exists. The Sec 5.1.4 intent log
  is the operator-intent record, not the applied-state
  record.

### `metric_sample_active` is NOT deleted

Per tech-spec Sec 7.2 line 1248 the `DELETE` privilege on
`metric_sample_active` is REVOKEd from every writer role.
The retract flow does NOT delete the pointer row; SHA-pinned
readers (`mgmt.read.metric_sample`, `mgmt.read.metric_samples`,
`eval.gate`) filter the retracted sample out by joining
through `metric_retraction`:

```sql
SELECT msa.sample_id
FROM clean_code.metric_sample_active msa
LEFT JOIN clean_code.metric_retraction mr ON mr.sample_id = msa.sample_id
WHERE msa.repo_id = $1
  AND msa.sha     = $2
  AND mr.sample_id IS NULL  -- exclude tombstoned samples
```

The production implementation of this anti-join for the
`eval.gate` rule-engine reader lives in
`internal/rule_engine/sql_store.go` --
`listMetricSamplesQuery`. The live PG test
`TestSQLStore_ListMetricSamples_FiltersRetracted` in
`internal/rule_engine/sql_store_test.go` pins the
behaviour against an isolated PG schema: a retracted
sample is filtered out even when the active pointer
remains in place.

### Rescan flow

The rescan verb is intentionally NOT idempotent: an
operator who clicks "rescan" twice expects TWO `scan_run`
rows because they want the recipe loop to run twice. The
handler executes:

1. **Validate body shape.** Same `DisallowUnknownFields`
   rule as retract.
2. **NO `repo_event` row is appended.** The canonical
   `RepoEvent.kind` enum at architecture Sec 5.1.4 has only
   four values (`registered`, `retired`, `retract_intent`,
   `mode_changed`) -- no `rescan_intent`. The rescan verb is
   a service-internal request; its audit trail lives in the
   structured log line plus the `scan_run` row itself.
3. **Delegate to `metric_ingestor.RescanEnqueuer`.** The
   enqueuer opens a `scan_run(kind='full', sha_binding='single',
   status='running', to_sha=<sha>)` row and returns the
   freshly-minted `scan_run_id`. The foundation-tier state
   machine drains the row via its standard claim path and
   finalises it once the recipe loop completes.

### Status codes (both verbs)

- **200** -- happy path. Body is the canonical wire-response
  shape (`retract_sample`: `retraction_id`, `sample_id`,
  `reason`, `appended_by`, `created_at`, `scan_run_id`,
  `inserted`; `rescan`: `scan_run_id`, `repo_id`, `sha`,
  `requested_by`, `opened_at`).
- **400** -- malformed JSON, unknown body field, missing
  required field, zero UUID, blank `reason` / `sha`.
- **401** -- missing or blank `X-OIDC-Subject` header.
- **404** (retract only) -- `sample_id` does not exist in
  `metric_sample`.
- **405** -- any method other than POST.
- **500** -- internal error; opaque body (`internal error`).
  The underlying driver / chain is logged server-side under
  `management write verb failed`.
- **503** -- one of the verb's backing subsystems
  (resolver / dispatcher / repo_event appender / enqueuer)
  is not wired. Mirrors the "verb exists, backing subsystem
  is down" contract pinned by Stage 5.1.

### Trust boundary

The body's `sample_id` / `reason` / `repo_id` / `sha` are
caller-supplied. The actor stamped on
`metric_retraction.appended_by` and the rescan structured
log line comes from `X-OIDC-Subject` (set by the auth
gateway). A body containing `actor` is REJECTED with 400 --
`DisallowUnknownFields` is the enforcement mechanism.

### Composition root wiring (Stage 3.4 iter 3 — role boundary)

Production wiring lives in
`cmd/clean-code-metric-ingestor/main.go`
(`mountMgmtRoutes`). The function takes **two** `*sql.DB`
handles so the binary honours the role grants from
`migrations/0004_roles.up.sql`:

| Handle       | Role                            | Tables (writes)                                            | Migration line |
| ------------ | ------------------------------- | ---------------------------------------------------------- | -------------- |
| `ingestorDB` | `clean_code_metric_ingestor`    | `scan_run` (INSERT/UPDATE), `metric_retraction` (INSERT)   | 348, 374       |
| `mgmtDB`     | `clean_code_management`         | `repo_event` (INSERT)                                      | 313            |

The `mgmtDB` handle is opened from the new
`CLEAN_CODE_MGMT_PG_URL` env var (the canonical Go field is
`config.Config.ManagementPostgresURL`). When that env var is
unset, the binary refuses to mount the `mgmt.*` write verbs
**unless** the operator has opted into shared-role mode via
`CLEAN_CODE_ALLOW_SHARED_PG_ROLE=true` (dev / `docker compose`
E2E only — production deployments **MUST** set role-distinct
DSNs).

```go
ingestorDB, _ := openAndPingDB(cfg.PostgresURL, "ingestor")
mgmtDB, _, _  := openMgmtDB(cfg, ingestorDB)
// mgmtDB is either:
//   - a SECOND *sql.DB opened from CLEAN_CODE_MGMT_PG_URL, OR
//   - the SAME pointer as ingestorDB if
//     CLEAN_CODE_ALLOW_SHARED_PG_ROLE=true (dev/E2E only).
// If CLEAN_CODE_MGMT_PG_URL is unset AND the shared-role
// opt-in is false, openMgmtDB FAILS FAST -- the binary
// will not boot.

retractStore, _        := metric_ingestor.NewPGRetractionStore(ingestorDB)
retractScanRunStore, _ := metric_ingestor.NewPGRetractScanRunStore(ingestorDB)
rescanStore, _         := metric_ingestor.NewPGRescanScanRunStore(ingestorDB)
repoEventAppender, _   := management.NewPGRepoEventAppender(mgmtDB) // mgmt role

dispatcher := metric_ingestor.NewRetractDispatcher(retractScanRunStore, retractStore, retractStore)
enqueuer   := metric_ingestor.NewRescanEnqueuer(rescanStore)

writer := management.NewMgmtWriter(
    retractStore, // PGRetractionStore satisfies SampleResolver
    management.AdaptMetricIngestorRetractDispatcher(dispatcher),
    management.AdaptMetricIngestorRescanEnqueuer(enqueuer),
    repoEventAppender,
)
mux.HandleFunc(management.VerbMgmtRetractSamplePath, writer.RetractSample)
mux.HandleFunc(management.VerbMgmtRescanPath, writer.Rescan)
```

### Operator env var reference (Stage 3.4)

| Env var                              | Default     | Required when                                        | Effect                                                                                       |
| ------------------------------------ | ----------- | ---------------------------------------------------- | -------------------------------------------------------------------------------------------- |
| `CLEAN_CODE_PG_URL`                  | unset       | Always (the binary refuses to boot without it)       | libpq DSN with `clean_code_metric_ingestor` credentials                                      |
| `CLEAN_CODE_MGMT_PG_URL`             | unset       | When `CLEAN_CODE_ALLOW_SHARED_PG_ROLE` is NOT truthy | libpq DSN with `clean_code_management` credentials (used for `PGRepoEventAppender` ONLY)     |
| `CLEAN_CODE_ALLOW_SHARED_PG_ROLE`    | `false`     | NEVER in production                                  | Dev/E2E opt-in to re-use `CLEAN_CODE_PG_URL` for both roles. Logs a startup WARN.            |
| `CLEAN_CODE_DISABLE_STALE_SWEEP`     | `false`     | Legacy `001_init.sql` environments only              | Skips the Stage 3.5 stale-sweep goroutine                                                    |
| `CLEAN_CODE_ENABLE_LEGACY_DEMO_API`  | `false`     | Legacy E2E only                                      | Mounts `/v1/ingestor/process` + `/v1/ingestor/scan-run`                                      |

When BOTH the keys-reader and the mgmt-writer are wired,
prefer the combined constructor that mounts both
surfaces on a single `http.ServeMux`:

```go
handler := management.NewHandlerWithWriter(reader, writer)
srv := &http.Server{Handler: handler.Routes()}
```

The `NewHandler(reader)` overload (no writer) is the
scaffold-mode bring-up posture: `policy.keys.list_active`
is mounted, the mgmt verb paths are NOT mounted (parent
mux returns 404 -- the service does NOT advertise an
endpoint it cannot serve).

Any nil dependency passed to `NewMgmtWriter` disables
ONLY the verb that depends on it (the affected verb
returns 503); the other verb keeps serving. This is the
same scaffold-mode posture Stage 5.1 established for
`policy.keys.list_active`.

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

## Stage 4.1 -- `/v1/ingest/{verb}` Router and HMAC verification

The Stage 4.1 webhook lives at `internal/ingest/webhook/router.go`
and serves the **generic** `/v1/ingest/{verb}` path that all four
`ingest.*` verbs (`coverage`, `test_balance`, `churn`, `defects`)
will mount on. The legacy `/v1/ingest/churn` mount (above) stays
for backwards compatibility; the Router is the canonical
production surface going forward.

### Wire shape

```
POST /v1/ingest/{verb} HTTP/1.1
Content-Type: <per-verb media type, e.g. application/json>
X-Signing-Key-Id: <opaque ASCII id agreed out-of-band>
X-Hub-Signature-256: sha256=<lowercase-hex HMAC-SHA256(body, secret)>

<verb-specific body>
```

The Router resolves the per-deployment HMAC secret via
`SecretResolver.Resolve(X-Signing-Key-Id)`. v1 is
**single-tenant** per tech-spec Sec 4.14 (one logical org per
deployment); there is NO `tenant_id` field on the resolver,
the response envelope, or the idempotency record. Multi-tenant
v2 will use per-schema isolation (tech-spec Sec 10A lines
1690-1696), NOT row-level tenant columns -- the Router's
seam-shape survives that migration without API change.

### Order of operations (security-critical)

1. **Method check**: non-`POST` -> `405 / METHOD_NOT_ALLOWED`.
2. **Body size cap**: 16 MiB -> `413 / PAYLOAD_TOO_LARGE`.
3. **HMAC verification** (BEFORE any per-verb inspection):
   - `ValidateSigningKeyID(X-Signing-Key-Id)` rejects missing /
     malformed headers (`HMAC_MISSING_KEY_ID`,
     `HMAC_MALFORMED_KEY_ID`).
   - `SecretResolver.Resolve(...)` rejects unknown ids
     (`HMAC_UNKNOWN_KEY_ID`).
   - `VerifyHMAC(body, X-Hub-Signature-256, secret)` rejects
     missing / malformed / mismatched signatures
     (`HMAC_MISSING_SIGNATURE`, `HMAC_MALFORMED_SIGNATURE`,
     `HMAC_SIGNATURE_MISMATCH`).
   - Every failure returns `401`. The HMAC step runs BEFORE
     verb / content-type inspection so an unauthenticated
     caller cannot probe the per-verb contract via 401-vs-415
     differentials.
4. **Verb lookup**: unregistered verb -> `404 / VERB_NOT_FOUND`.
5. **Content-Type pin** (per-verb): a mismatch ->
   `415 / UNSUPPORTED_MEDIA_TYPE`.
6. **Idempotency claim**: `payload_hash = sha256(body)`. A
   prior commit for the same `(verb, payload_hash)` returns
   the cached `scan_run_id` with `replayed=true` and does NOT
   re-execute the verb handler. Atomic claim/commit/abort
   guarantees exactly one execution under concurrent retries.

### Success envelope

```json
{
  "scan_run_id": "<uuidv7>",
  "verb": "<churn|coverage|test_balance|defects>",
  "scan_run_kind": "<external_per_row|external_single>",
  "payload_hash": "<sha256 lowercase hex>",
  "foundation_dispatched": false,
  "replayed": false,
  "detail": { /* verb-specific counters */ }
}
```

A replay returns the SAME `scan_run_id` with `replayed=true`.

### Operator rotation (signing key)

`StaticSecretResolver` supports the tech-spec Sec 8.2 24-hour
overlap:

1. Publish a new `(signing_key_id, secret)` pair via
   `resolver.Add(newID, newSecret)`. Both old and new keys
   verify during the overlap window.
2. Update CI publishers to use the new id.
3. After the 24-hour overlap, `resolver.Remove(oldID)`. The
   old id now returns `HMAC_UNKNOWN_KEY_ID`.

A Phase 3.2 PG-backed resolver will source rows from a
secret-rotation table; the rotation flow stays the same.

### Iter 2 -- Durable `scan_run(payload_hash)` idempotency + Router mount

> **Superseded by iter 3.** Iter 2 keyed the partial
> unique index on `(kind, payload_hash)`. Iter 3 rekeyed
> it on `(verb, payload_hash)` (index name
> `scan_run_payload_hash_verb_uniq`) -- see the iter-3
> section below for the canonical migration shape and
> operator playbook. The iter-2 prose is preserved as
> historical record only; do NOT use the SQL snippets in
> this section directly.

Iter 1 used the in-process `IdempotencyStore` as the source of
truth for replay. Iter 2 inverts that: the durable
`scan_run(kind, payload_hash)` row is now the authority; the
in-process store is a fast same-process replay cache.

#### Required migration -- 0009 (apply BEFORE enabling the webhook)

`migrations/0009_scan_run_payload_hash_unique.up.sql` creates a
partial unique index:

```sql
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS
    scan_run_payload_hash_kind_uniq
    ON scan_run (kind, payload_hash)
    WHERE payload_hash IS NOT NULL;
```

Operational rules:

- `CREATE INDEX CONCURRENTLY` cannot run inside a transaction.
  Apply via `psql -f` (autocommit), NOT through the migration
  runner if that runner wraps each file in `BEGIN/COMMIT`.
- The `WHERE payload_hash IS NOT NULL` clause restricts the
  constraint to external-ingest rows; foundation-tier rows
  (`atomic`, `aggregated_repo`, etc.) keep
  `payload_hash NULL` and remain unaffected.
- Rollback is `0009_scan_run_payload_hash_unique.down.sql`:
  `DROP INDEX CONCURRENTLY IF EXISTS`. Safe to roll back, but
  retries across replicas/restarts will degrade to "duplicate
  scan_run rows on race" -- the Router still works, just not
  durably-idempotent.

#### Two-tier idempotency

| Tier | Backend | Scope | What it catches |
|------|---------|-------|------------------|
| In-process | `webhook.InMemoryIdempotencyStore` | one replica | concurrent retries on the SAME process |
| Durable | `webhook.PGScanRunRepository` | cluster-wide, survives restart | retries across replicas + after restart |

On each request the Router consults BOTH:

1. Compute `payload_hash = sha256(body)`.
2. **In-process claim** -- `IdempotencyStore.Claim` returns a
   prior committed envelope verbatim (single round-trip; no
   DB cost on the hot replay path).
3. **Metadata extract** -- `VerbHandler.ExtractMetadata(body)`
   pulls the verb-specific `(RepoID, SHA)` out of the canonical
   body (required for the durable INSERT shape). Decode errors
   here surface as `400 / BAD_REQUEST` or `422 / UNPROCESSABLE_ENTITY`
   WITHOUT burning a `scan_run` row.
4. **Durable open** -- `ScanRunRepository.OpenExternal(...)`
   performs an `INSERT ... ON CONFLICT DO NOTHING RETURNING
   scan_run_id` against the partial unique index. On conflict
   the store transparently re-`SELECT`s the prior row. The
   result carries `AlreadyExisted` + `ExistingStatus`.
5. **Replay branch** -- if `AlreadyExisted=true`, the Router
   emits the durable replay envelope (`scan_run_id` =
   the prior row, `replayed=true`) WITHOUT calling the verb
   handler. The in-process cache is also populated so further
   replays on the same process collapse to step (2).
6. **Fresh execution** -- the verb handler runs against the
   fresh `scan_run_id`, then the Router calls
   `ScanRunRepository.Finalize(succeeded|failed)` and commits
   the in-process cache.

#### Replay semantics

- A second POST with the same canonical body returns the
  ORIGINAL `scan_run_id` and `replayed=true`. The verb handler
  is NOT re-executed; no new `scan_run` row is inserted.
- This holds across **process restarts** AND across
  **replicas** -- the durable INSERT-ON-CONFLICT is the
  source of truth.
- Pinned by:
  - `TestRouter_DurableReplay_AcrossSimulatedRestart` (two
    Router instances sharing one `ScanRunRepository`).
  - `TestRouter_ReplayReturnsCachedScanRun_NoReExecution`
    (in-process happy path).
  - `TestRouter_VerbFailure_FinalizesScanRunAsFailed`
    (failure path; durable row finalizes as `failed`).

#### Sticky-failed replay (publisher contract)

When a verb handler returns an error, the Router finalizes
the durable `scan_run` row as `failed`. A subsequent POST
with the **same canonical body** resolves through the partial
unique index and returns the failed row's `scan_run_id` with
`replayed=true`. **The handler is NOT retried.** Publishers
MUST change the canonical body to retry (e.g. bump a request
nonce or correlation id inside the body). This matches GitHub
webhook conventions and preserves the audit chain. A future
iter MAY add a retry-window-controlled
`ON CONFLICT DO UPDATE` recycle path for failed rows -- not
in v1.

#### Running-status replay (sibling-replica race)

When the durable row is found in `running` state (a sibling
replica is mid-execution), the Router currently returns the
running `scan_run_id` with `replayed=true`. The publisher's
poll-until-terminal loop on `GET /v1/scan_runs/{id}` is the
canonical way to wait for the verdict. A future iter MAY add
a Router-side poll-or-409 surface.

#### Composition-root wiring (operator-visible env vars)

`cmd/clean-code-metric-ingestor/main.go` mounts the Router
behind two new env vars in addition to the existing
`CLEAN_CODE_WEBHOOK_HMAC_SECRET`:

| Variable | Required | Default | Notes |
|----------|----------|---------|-------|
| `CLEAN_CODE_ENABLE_EXTERNAL_INGEST_WEBHOOK` | yes (for mount) | `false` | When unset the Router is NOT mounted; the legacy `/v1/ingest/churn` path remains. |
| `CLEAN_CODE_WEBHOOK_SIGNING_KEY_ID` | yes (when enabled) | -- | Opaque ASCII key id paired with `CLEAN_CODE_WEBHOOK_HMAC_SECRET`. Publishers send this in the `X-Signing-Key-Id` header. |
| `CLEAN_CODE_WEBHOOK_HMAC_SECRET` | yes (when enabled) | -- | Per-deployment secret. |

Startup-time interlocks reject:

- `EnableExternalIngestWebhook=true` with either of the
  other two unset.
- `WebhookSigningKeyID` set with
  `EnableExternalIngestWebhook=false` (wiring drift -- the
  id would never be consulted).

To enable in production:

1. Apply migration 0009 via `psql -f` (see above).
2. Set the three env vars in the operator's deployment
   secret store.
3. Restart `clean-code-metric-ingestor` -- the startup log
   line `mounted external-ingest webhook router` (emitted by
   `cmd/clean-code-metric-ingestor/main.go: mountIngestRouter`
   at INFO level) with structured fields `path`,
   `signing_key_id`, and `verbs` confirms the mount. Sample
   field shape (slog text encoder, secret values redacted):
   `path=/v1/ingest/ signing_key_id=<id> verbs=[churn]`.

#### Observability

Stage 4.1 emits five structured-log lines from the Router
itself (`internal/ingest/webhook/router.go`) plus one
startup line from the composition root
(`cmd/clean-code-metric-ingestor/main.go`); the underlying
durable-claim primitives (`metric_ingestor.PGExternalScanRunStore`,
`webhook.PGScanRunRepository`) do NOT carry an `slog`
seam in v1 -- their behaviour surfaces through the
Router's lines plus the catalog `scan_run` table itself.

| Event | Level | slog message (verbatim) | Fields (verbatim) | Emitter (file:func) |
|-------|-------|-------------------------|-------------------|---------------------|
| Mount confirmation (startup) | INFO | `mounted external-ingest webhook router` | `path`, `signing_key_id`, `verbs` | `cmd/clean-code-metric-ingestor/main.go: mountIngestRouter` |
| Fresh successful POST | INFO | `ingest webhook: success` | `verb`, `scan_run_id`, `payload_hash`, `scan_run_kind` | `router.go: Router.ServeHTTP` (after Commit) |
| In-process replay (same-replica cache hit, fast path) | INFO | `ingest webhook: replay (cached scan_run_id, in-process)` | `verb`, `scan_run_id`, `payload_hash` | `router.go: Router.replayResponse` |
| Durable replay (cross-process / cross-replica) | INFO | `ingest webhook: replay (durable scan_run_id, cross-process)` | `verb`, `scan_run_id`, `existing_status`, `payload_hash` | `router.go: Router.emitDurableReplay` |
| HMAC short-circuit (any 401 path) | WARN | `ingest webhook: HMAC verification failed` | `verb`, `code` (one of the `HMAC_*` codes below), `err`, `remote_addr` | `router.go: Router.logHMACFailure` |
| Internal failure (resolver, scan_run-open, verb-handler, idempotency-commit, marshal, etc.) | WARN | `ingest webhook: internal failure` | `verb`, `kind` (stage tag), `err`, `remote_addr` | `router.go: Router.logInternal` |

The `code` field on `HMAC verification failed` and the
`code` field on the 401 JSON error envelope (`ErrorBody.Code`)
draw from the SAME closed set:

| Code | Trigger | Source |
|------|---------|--------|
| `HMAC_MISSING_KEY_ID` | `X-Signing-Key-Id` header absent | `router.go: classifyKeyIDError` |
| `HMAC_MALFORMED_KEY_ID` | header fails [ValidateSigningKeyID] | `router.go: classifyKeyIDError` |
| `HMAC_UNKNOWN_KEY_ID` | header valid but resolver has no entry | `router.go: Router.ServeHTTP` ([ErrUnknownSigningKeyID] branch) |
| `HMAC_MISSING_SIGNATURE` | `X-Hub-Signature-256` header absent | `handler.go: classifyHMACError` ([ErrHMACMissingHeader]) |
| `HMAC_MALFORMED_SIGNATURE` | header not `sha256=<64 hex>` | `handler.go: classifyHMACError` ([ErrHMACMalformedHeader]) |
| `HMAC_SIGNATURE_MISMATCH` | digest != recomputed body digest | `handler.go: classifyHMACError` ([ErrHMACSignatureMismatch]) |
| `HMAC_EMPTY_SECRET` | resolver returned an empty secret (server-side misconfig) | `handler.go: classifyHMACError` ([ErrHMACEmptySecret]) |
| `HMAC_INVALID` | any other HMAC-layer error (catch-all default) | `handler.go: classifyHMACError` (default arm) |

Operator notes:

- `grep -F "ingest webhook: HMAC verification failed"` against
  the service log returns every 401 from the Router; the
  `code` field on the same line names the failure mode.
  `grep -F "HMAC_SIGNATURE_MISMATCH"` is the canonical query
  for "publisher signed the wrong body".
- `grep -F "ingest webhook: internal failure"` against the
  service log returns every Router-internal 500. The `kind`
  field is the stage tag (e.g. `resolver-internal-failure`,
  `scan-run-open-failure`, `idempotency-commit-failure`,
  `extract-metadata-failure`) so operators can disambiguate
  which Router stage failed without re-reading the
  pipeline code.
- The `existing_status` field on a durable replay names
  the prior terminal state of the `scan_run` row
  (`succeeded` / `failed` / `running`). A
  `running` value means a sibling replica is still
  mid-execution -- publishers should poll
  `GET /v1/scan_runs/{id}` for the terminal verdict.
- The Router NEVER logs the `signing_key_id` value, the
  HMAC secret, the supplied signature, or the computed
  digest -- those would all leak side-channel information
  useful for brute-force or replay attacks. The 401 JSON
  envelope is similarly safe (it carries only the
  `HMAC_*` code, no secret material).
- DB-tier observability for the durable seam is the
  `scan_run` catalog itself: a `SELECT verb,
  payload_hash, status, started_at, ended_at FROM
  clean_code.scan_run WHERE payload_hash IS NOT NULL
  ORDER BY started_at DESC LIMIT 50` returns the same
  view that operators ran against `scan_run` for
  foundation-tier rows in Stage 3. A future iter MAY
  add INFO logs at the `PGExternalScanRunStore` layer
  if operator feedback shows the catalog query is
  insufficient; until then the catalog table IS the
  authoritative observability surface.


### Iter 3 -- Per-verb idempotency key + Finalize same-terminal contract

Iter 2 keyed the durable partial unique index on
(kind, payload_hash). That key is too coarse: `churn`
and `defects` are both `kind = external_per_row`, and
`coverage` and `test_balance` are both
`kind = external_single`. A publisher posting the same
canonical body to two different verbs would have collapsed
onto a single `scan_run` row -- corrupting the per-verb
audit chain. Iter 3 keys the durable uniqueness on
`(verb, payload_hash)` instead.

#### Migration 0009 -- rewritten shape

The iter-3 `migrations/0009_scan_run_payload_hash_unique.up.sql`:

1. `DROP INDEX CONCURRENTLY IF EXISTS clean_code.scan_run_payload_hash_kind_uniq` --
   removes the iter-2 index if any dev DB applied it.
2. `ALTER TABLE clean_code.scan_run ADD COLUMN IF NOT EXISTS verb text` --
   nullable; metadata-only on PG 11+.
3. Defensive backfill:
   `UPDATE clean_code.scan_run SET verb = '__legacy_' || kind WHERE payload_hash IS NOT NULL AND verb IS NULL` --
   ensures any iter-2 dev rows satisfy the new CHECK
   constraint without operator intervention. Production has
   zero external `scan_run` rows yet.
4. `ALTER TABLE clean_code.scan_run ADD CONSTRAINT scan_run_verb_payload_hash_consistent CHECK ((verb IS NULL) = (payload_hash IS NULL))` --
   pins the invariant that `verb` and `payload_hash`
   are always set together for external rows and always
   null together for foundation-tier rows.
5. `CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS scan_run_payload_hash_verb_uniq ON clean_code.scan_run (verb, payload_hash) WHERE payload_hash IS NOT NULL` --
   the new per-verb idempotency key.

Operational rules unchanged: `CREATE INDEX CONCURRENTLY`
cannot run inside a transaction; apply via `psql -f`
(autocommit). The rollback file
`0009_scan_run_payload_hash_unique.down.sql` is updated
to drop the new index, the CHECK constraint, and the
`verb` column (in that order).

#### Per-verb closed set + verb-kind matrix

`PGExternalScanRunStore` (`internal/metric_ingestor`)
validates the verb closed-set BEFORE any DB roundtrip:

| Verb            | Kind                 |
|-----------------|----------------------|
| `coverage`    | `external_single`  |
| `test_balance`| `external_single`  |
| `churn`       | `external_per_row` |
| `defects`     | `external_per_row` |

An unknown verb returns `ErrExternalScanRunUnsupportedVerb`;
a verb-kind mismatch (e.g. `Verb: "churn", Kind: "external_single"`)
returns a validation error naming both fields. These
guards close a wiring-bug class where a caller could
silently write a verb row under the wrong kind enum.

#### `ScanRunRepository.Finalize` same-terminal contract

The interface contract: a double-finalize where the row
is ALREADY in the requested terminal status MUST return
nil (this is the benign sibling-replica double-finalize).
Only a finalize that observes a DIFFERENT terminal
status (or a missing row) returns an error.

The PG adapter (`webhook.PGScanRunRepository.Finalize`)
implements this by, on `ErrConcurrentFinalize`:

1. Calling `LookupExternalScanRunStatusByID(scan_run_id)`.
2. If the existing status == requested status -> nil.
3. If the existing status != requested status -> wrapped
   `ErrConcurrentFinalize` naming the mismatch (the
   operator log line tells the SRE which two terminal
   statuses raced).
4. If the row is unexpectedly missing (stale-sweep DELETE
   between FinalizeExternalScanRun and the lookup) ->
   wrapped error naming the missing row.

Pinned by three adapter-layer tests
(`TestPGScanRunRepository_Finalize_ConcurrentSameTerminal_ReturnsNil`,
`_DifferentTerminal_ReturnsError`,
`_RowMissing_ReturnsError`) and two in-memory tests
(`TestInMemoryScanRunRepository_Finalize_SameTerminal_ReturnsNil`,
`_DifferentTerminal_ReturnsError`).

#### Composition root + config interlock tests

Iter 3 adds direct test coverage for the iter-2 wiring:

- `internal/config/config_test.go` -- 5 tests pin the
  three-variable interlock (all-set OK, partial-set
  rejected each way, off-by-default).
- `cmd/clean-code-metric-ingestor/main_test.go` -- 6
  tests pin the mount: disabled-no-mount, enabled-nil-DB,
  enabled-empty-signing-key-id, enabled-empty-hmac-secret,
  enabled-canonical-path (asserts a POST without a valid
  signature returns 401 NOT 404, proving the Router is
  mounted AND the HMAC verifier runs in front of the DB
  roundtrip), and disabled-with-secrets-still-not-mounted.
