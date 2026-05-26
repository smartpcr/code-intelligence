## `services/clean-code` rollout playbook

How to roll a new build of the clean-code service into a
production-tier environment. The instructions assume Postgres
14+ is already running and the `clean_code_*` roles have been
created via the `0004_roles.up.sql` migration.

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

   Production deploys MUST set BOTH `CLEAN_CODE_PG_URL`
   and `CLEAN_CODE_AST_SCAN_ROOT`. The composition root
   (`cmd/clean-coded/main.go:402-448`) is strict about the
   pairing:

   - **Both set** → `PGScanRunStore` +
     `DirectoryAstFileSource` are wired and the sweeper
     launches.
   - **`CLEAN_CODE_PG_URL` set, `CLEAN_CODE_AST_SCAN_ROOT`
     unset** → the process **fails to start** with a
     non-zero exit and the actionable boot error
     "CLEAN_CODE_AST_SCAN_ROOT is REQUIRED when
     CLEAN_CODE_PG_URL is configured" (see
     `main.go:438-448`). It does NOT silently fall back to
     `EmptyAstFileSource`; iter-4 made this fail-fast
     after an evaluator finding that a production process
     could otherwise start against live PG without ever
     processing pending commits.
   - **`CLEAN_CODE_PG_URL` unset** → scaffold mode. The
     composition root logs `metric ingestor sweep loop
     NOT STARTED (scaffold mode: CLEAN_CODE_AST_SCAN_ROOT
     unset)` (`main.go:454-460`), closes `sweepDone`
     immediately, and does NOT launch the sweeper. The
     HTTP surface still serves; nothing claims commits.
     This is dev-loop only.

### Per-rollout verification

After deploying a new build, confirm:

1. `/healthz` returns 200 within 5s of pod-ready.
2. `/readyz` flips to 200 within 30s. Note: Stage 3.2 does
   NOT register an `ast_source` ready-check; the AST
   source availability is enforced inside the state
   machine's pre-flight (`WithStateMachineSourceProbe`),
   not on the `/readyz` surface. The only `/readyz` probe
   registered today is `signing_key_cache` (Policy
   Steward, Stage 5.1).
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
