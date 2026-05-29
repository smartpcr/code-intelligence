# `eval.gate` load + SLO conformance scenario

## What this is

`eval_gate_load.js` is a [k6](https://k6.io) script that drives the
clean-code service's `eval.gate` verb at the load shape pinned by
implementation-plan Stage 10.3 ("100 repos and 50 scans/min
sustained for 30 minutes") and asserts the per-PR SLO targets
published in tech-spec Sec 8.3 lines 916-924:

| Surface     | Metric            | p50    | p95    | p99    |
| ----------- | ----------------- | ------ | ------ | ------ |
| `eval.gate` | response latency  | 200 ms | 800 ms | 2 s    |
| `eval.gate` | error rate        | n/a    | n/a    | < 1 %  |
| `eval.gate` | check-pass rate   | n/a    | n/a    | > 99 % |

A breach of any of these thresholds aborts the k6 run with a
non-zero exit code, so a CI wrapper around the lab-bare-metal
validation lane can gate purely on the exit status without
parsing the summary JSON.

## Where this runs

This scenario is NOT part of the `services/clean-code/Makefile`
CI target set. It is an OPERATOR artifact intended for the
lab-bare-metal validation lane documented in
`docs/stories/code-intelligence-CLEAN-CODE/e2e-scenarios.md`
line 13 ("`Load and SLO conformance` for the lab-bare-metal
validation"). The lab harness invokes it after the cleanroom
deploy as a release-acceptance step; do not wire it into the
unit-test or integration-test gates -- it requires a long-lived
clean-code service with 100 fixture repos pre-registered, which
is incompatible with the short-lived CI sandbox.

## Pre-requisites

1. **k6 binary**: `>= 0.50.0`. Install via
   `brew install k6` (macOS),
   `winget install k6.k6` (Windows),
   or
   [the official binary release](https://github.com/grafana/k6/releases).
2. **Running clean-code gateway**: HTTP(S) endpoint exposing
   `/v1/eval/gate` per the architecture's gateway routing
   (architecture Sec 6.1). The scenario does not bring up the
   service; the operator is responsible for deploying it via the
   normal `deploy/` Helm chart.
3. **Pre-registered fixture repos AND SHAs (CLOSED set)**: the scenario
   references a CLOSED set of `100 repos x 50 SHAs = 5_000` deterministic
   `(repo_id, sha)` pairs derived in the script's init context
   (`eval_gate_load.js`, `repos` and `shas` constants). The seeder MUST
   pre-create, for EVERY pair in this closed set:
     - the `clean_code.repo` row at the operator-pinned UUID, and
     - the `clean_code.commit` row with `scan_status='scanned'` (the
       `scan_status` column lives on `commit`, not `repo` -- see
       `migrations/0001_catalog_lifecycle.up.sql:229`), and
     - at least one `clean_code.metric_sample` row plus its matching
       `clean_code.metric_sample_active` pointer per metric_kind the
       active policy version references, so `eval.gate` does NOT fall
       through to the `samples_pending` degraded fast path.
   The deterministic pair-generation rules (re-implement in the seeder
   without sharing state with the k6 process):
     - `repo_id[i] = "00000000-0000-0000-0000-" + zeroPad12(i)` for
       `i` in `[0, 100)`.
     - `sha[i][j] = "0" * 28 + hex6(i) + hex6(j)` (40 lowercase-hex
       chars) for `j` in `[0, 50)`.
   At the brief's load (`30 min x 50/min = 1500` requests over 5_000
   pairs), each pair is sampled ~0.3 times in expectation, so the
   `(repo, sha, policy_version)` verdict cache stays cold on
   virtually every request -- the cache-cold path is the one
   tech-spec Sec 8.3 publishes the SLO for. If the seeder misses a
   pair, `eval.gate` returns `degraded: true,
   degraded_reason: "samples_pending"` and the
   `degraded is false` check fails, which rolls into the
   `checks{name:eval.gate} rate > 0.99` threshold and aborts the
   run. See **Seeding the fixtures** below for the concrete recipe.
4. **OIDC bearer token**: an operator-role token with the
   `eval.gate` scope, issued by the cluster's
   identity-provider per architecture Sec 6.1.

## Environment variables

| Variable                  | Required | Default                 | Meaning                                                    |
| ------------------------- | -------- | ----------------------- | ---------------------------------------------------------- |
| `CLEAN_CODE_GATEWAY_URL`  | no       | `http://localhost:8080` | Gateway base URL the scenario drives.                      |
| `CLEAN_CODE_OIDC_TOKEN`   | yes      | (empty -> 401 floor)    | Bearer token sent in the `Authorization` header.           |

When `CLEAN_CODE_OIDC_TOKEN` is empty the scenario intentionally
omits the `Authorization` header so the resulting 401 responses
trip the `http_req_failed{name:eval.gate} rate < 0.01` floor and
fail the run loudly. This avoids a vacuous pass when an
operator forgets to mint the token.

## Running

Smoke run against a local dev service (FAST FAIL on misconfig):

```sh
export CLEAN_CODE_GATEWAY_URL=http://localhost:8080
export CLEAN_CODE_OIDC_TOKEN="$(cat ~/.clean-code/operator.jwt)"
k6 run eval_gate_load.js
```

Lab-bare-metal acceptance run (archive the summary JSON for
release-tag review):

```sh
export CLEAN_CODE_GATEWAY_URL=https://clean-code.lab.example.com
export CLEAN_CODE_OIDC_TOKEN="$(vault read -field=token \
  secret/clean-code/load-operator)"
k6 run \
  --summary-export=eval_gate_load.$(date -u +%Y%m%dT%H%M%SZ).json \
  eval_gate_load.js
```

## Exit codes

k6 follows its standard exit-code contract:

  - `0` -- all thresholds passed.
  - `99` -- at least one threshold breached. This is the
    exit code the lab-harness gate should treat as a release
    block.
  - other non-zero -- usage error (missing script, malformed
    URL, k6 binary mismatch). Treat as operator-fix-needed,
    not a release-block.

## Seeding the fixtures

The 5_000 `(repo_id, sha)` pairs MUST exist before the run;
otherwise the gateway short-circuits with
`degraded: true, degraded_reason: "samples_pending"` (the
`writeDegraded` path in `internal/evaluator/gate_evaluate.go`)
and the per-iteration `degraded is false` check trips the
`checks{name:eval.gate} rate > 0.99` floor and aborts the run.

### Why the runtime API verbs are NOT used for pre-seeding

The clean-code service exposes two runtime verbs an operator
could in principle drive to create the fixture rows. Neither
is suitable for pre-seeding the closed 5_000-pair set the k6
scenario expects:

  - **`mgmt.register_repo`** (`POST /v1/mgmt/register_repo`,
    JSON body `{repo_url, default_branch, mode?, modes?,
    display_name?}`) creates one `clean_code.repo` row. The
    `repo_id` UUID is **minted server-side** (column DEFAULT
    `gen_random_uuid()` per `migrations/0001_catalog_lifecycle.up.sql:149`)
    and returned in the response body. The wire decoder also
    runs with `DisallowUnknownFields`
    (`internal/management/register_repo_verb.go:97-119`), so
    an operator CANNOT supply a chosen `repo_id` -- the
    request would fail with HTTP 400. This is incompatible
    with the k6 scenario's deterministic `00000000-0000-0000-0000-{NNN}`
    repo_id set (`eval_gate_load.js:148-156`).
  - **`ingest.coverage`** (`POST /v1/ingest/coverage`,
    `Content-Type: application/xml`) ingests **Cobertura XML**
    and emits `MetricSample` rows. `repo_id` and `sha` are
    read from attributes on the root `<coverage>` element
    (`internal/ingest/coverage/cobertura.go:1112-1161`); a
    JSON body is rejected as `ErrMalformedXML`. Even with a
    valid Cobertura payload, ingest also requires the
    catalog `repo` row to already exist AND an active signed
    `policy_version` (rejection happens further downstream).

### Lab fixture pre-seed (direct SQL)

The lab-bare-metal validation lane reaches the database
directly via `psql` against the `clean_code` schema. This is
the only path that can pin the deterministic UUIDs the k6
scenario draws from. Save the snippet below as `seed.sql`
and run:

```sh
psql "${CLEAN_CODE_DATABASE_URL}" \
     -v ON_ERROR_STOP=1 \
     -f seed.sql
```

The SQL is idempotent via `ON CONFLICT DO NOTHING` -- safe to
re-run on a lab refresh. It pre-seeds six rows per
`(repo_id, sha)` pair so the `eval.gate` happy path can run
end-to-end:

  - `clean_code.repo` (100 rows; pinned UUIDs)
  - `clean_code.commit` (5 000 rows; `scan_status='scanned'`
    skips the `samples_pending` fast path)
  - `clean_code.scan_run` (100 rows; one per repo, supplies
    the `producer_run_id` FK on `metric_sample`)
  - `clean_code.scope_binding` (100 rows; repo-level scope so
    every SHA reuses the same scope_id)
  - `clean_code.metric_sample` + `clean_code.metric_sample_active`
    (5 000 rows each, for `coverage_line_ratio` as the example
    metric_kind)

```sql
-- seed.sql -- pre-seed the 5_000 (repo_id, sha) pairs the
-- eval_gate_load.js scenario draws from. Idempotent.
--
-- Column lists re-check against the migrations (path:
-- services/clean-code/migrations/):
--   clean_code.repo            -- 0001_catalog_lifecycle.up.sql:147-170
--   clean_code.commit          -- 0001_catalog_lifecycle.up.sql:212-231
--   clean_code.scan_run        -- 0001_catalog_lifecycle.up.sql:337-...
--   clean_code.scope_binding   -- 0002_measurement.up.sql:186-219
--   clean_code.metric_sample   -- 0002_measurement.up.sql:257-...
--   clean_code.metric_sample_active -- 0002_measurement.up.sql:506-...
--
-- Enum value cross-refs:
--   repo_mode               -- 'embedded' (0001:75-78)
--   commit_scan_status      -- 'scanned'  (0001:87-...)
--   scan_run_kind           -- 'full'     (0001:117-...)
--   scan_run_sha_binding    -- 'single'   (0001:129-132)
--   scope_kind              -- 'repo'     (0002:142-...)
--   metric_sample_pack      -- 'ingested' (0002:103-108)
--   metric_sample_source    -- 'ingested' (0002:115-119)

BEGIN;

-- 100 repos with operator-pinned UUIDs matching the k6
-- scenario's deterministic UUID generator.
INSERT INTO clean_code.repo
    (repo_id, display_name, mode, default_branch)
SELECT
    ('00000000-0000-0000-0000-' || lpad(i::text, 12, '0'))::uuid,
    'load-fixture-' || i,
    'embedded',
    'main'
FROM generate_series(0, 99) AS s(i)
ON CONFLICT (repo_id) DO NOTHING;

-- 100 x 50 commits with scan_status='scanned'. The SHA
-- encoding mirrors eval_gate_load.js:172-190 exactly
-- (28 zero-pad + 6-hex repo index + 6-hex sha index).
INSERT INTO clean_code.commit
    (repo_id, sha, committed_at, scan_status)
SELECT
    ('00000000-0000-0000-0000-' || lpad(i::text, 12, '0'))::uuid,
    repeat('0', 28) || lpad(to_hex(i), 6, '0') || lpad(to_hex(j), 6, '0'),
    now(),
    'scanned'
FROM generate_series(0, 99) AS r(i)
CROSS JOIN generate_series(0, 49) AS s(j)
ON CONFLICT (repo_id, sha) DO NOTHING;

-- One scan_run per repo to supply the producer_run_id FK on
-- metric_sample below. scan_run.scan_run_id UUID is also
-- operator-pinned so the metric_sample INSERT can reference
-- it without a follow-up SELECT.
INSERT INTO clean_code.scan_run
    (scan_run_id, repo_id, kind, sha_binding, to_sha)
SELECT
    ('00000001-0000-0000-0000-' || lpad(i::text, 12, '0'))::uuid,
    ('00000000-0000-0000-0000-' || lpad(i::text, 12, '0'))::uuid,
    'full',
    'single',
    repeat('0', 28) || lpad(to_hex(i), 6, '0') || repeat('0', 6)
FROM generate_series(0, 99) AS s(i)
ON CONFLICT (scan_run_id) DO NOTHING;

-- One scope_binding per repo (scope_kind='repo' so all 50
-- SHAs of a repo share one scope_id; this keeps the seed
-- proportional to repo count rather than pair count).
INSERT INTO clean_code.scope_binding
    (scope_id, repo_id, scope_kind, canonical_signature, first_seen_sha)
SELECT
    ('00000002-0000-0000-0000-' || lpad(i::text, 12, '0'))::uuid,
    ('00000000-0000-0000-0000-' || lpad(i::text, 12, '0'))::uuid,
    'repo',
    'load-fixture-scope-' || i,
    repeat('0', 28) || lpad(to_hex(i), 6, '0') || repeat('0', 6)
FROM generate_series(0, 99) AS s(i)
ON CONFLICT (scope_id) DO NOTHING;

-- One metric_sample per (repo_id, sha) pair for the
-- `coverage_line_ratio` metric_kind. If the active policy
-- references additional metric_kinds, copy this block and
-- the matching metric_sample_active block below for each
-- one (canonical kinds are pinned in tech-spec Sec 4.1.1).
INSERT INTO clean_code.metric_sample
    (sample_id, repo_id, sha, scope_id, metric_kind, metric_version,
     value, pack, source, producer_run_id)
SELECT
    gen_random_uuid(),
    ('00000000-0000-0000-0000-' || lpad(i::text, 12, '0'))::uuid,
    repeat('0', 28) || lpad(to_hex(i), 6, '0') || lpad(to_hex(j), 6, '0'),
    ('00000002-0000-0000-0000-' || lpad(i::text, 12, '0'))::uuid,
    'coverage_line_ratio',
    1,
    0.95,
    'ingested',
    'ingested',
    ('00000001-0000-0000-0000-' || lpad(i::text, 12, '0'))::uuid
FROM generate_series(0, 99) AS r(i)
CROSS JOIN generate_series(0, 49) AS s(j)
ON CONFLICT DO NOTHING;

-- metric_sample_active pointer rows -- the evaluator's
-- active-row lookup reads through this table, not raw
-- metric_sample. Without these the predicate eval sees
-- "no value" and the gate verdict depends on the policy's
-- handling of missing data (commonly 'pass').
INSERT INTO clean_code.metric_sample_active
    (repo_id, sha, scope_id, metric_kind, metric_version, sample_id)
SELECT
    m.repo_id, m.sha, m.scope_id, m.metric_kind, m.metric_version,
    m.sample_id
FROM clean_code.metric_sample AS m
WHERE m.metric_kind = 'coverage_line_ratio'
  AND m.repo_id IN (
      SELECT ('00000000-0000-0000-0000-' || lpad(i::text, 12, '0'))::uuid
      FROM generate_series(0, 99) AS s(i)
  )
ON CONFLICT (repo_id, sha, scope_id, metric_kind, metric_version) DO NOTHING;

COMMIT;

-- Sanity probes after a fresh run:
--   SELECT COUNT(*) FROM clean_code.repo;            -- expect 100
--   SELECT COUNT(*) FROM clean_code.commit;          -- expect 5000
--   SELECT COUNT(*) FROM clean_code.metric_sample;   -- expect 5000
--   SELECT COUNT(*) FROM clean_code.metric_sample_active; -- expect 5000
```

### Active signed policy (operator prerequisite, NOT in `seed.sql`)

The seed above takes the catalog/measurement substrate from
empty to "`scan_status='scanned'`, MetricSample rows present".
The `eval.gate` happy path ALSO requires an active
`clean_code.policy_version` whose **Ed25519** signature
(`migrations/0003_policy_audit_refactor.up.sql:331-341`) the
evaluator can verify, and a `clean_code.policy_activation`
row referencing it
(`migrations/0003_policy_audit_refactor.up.sql:381-399`).

The lab harness must publish this once via the
`policy.publish` verb against the lab's signing key BEFORE
the load run begins. Inserting an unsigned or
test-key-signed `policy_version` row directly via SQL is
NOT a substitute -- the evaluator returns
`{verdict: 'warn', degraded: true, degraded_reason: 'policy_signature_invalid'}`
on any signature-verify failure and the `degraded is false`
check trips the `checks` floor.

If the active policy references metric_kinds beyond
`coverage_line_ratio`, repeat the `metric_sample` +
`metric_sample_active` INSERT blocks for each additional
metric_kind so the predicate evaluator finds non-null values
for every gate input.

### Runtime smoke tests via the API (informational only)

For a one-off end-to-end check OUTSIDE the load scenario --
i.e. when reproducibility against deterministic UUIDs is NOT
required -- the runtime verbs can be exercised directly:

```sh
# mgmt.register_repo -- server mints the repo_id.
curl --fail-with-body -X POST \
     "${CLEAN_CODE_GATEWAY_URL}/v1/mgmt/register_repo" \
     -H "Authorization: Bearer ${CLEAN_CODE_OIDC_TOKEN}" \
     -H "X-OIDC-Subject: smoke-operator" \
     -H "Content-Type: application/json" \
     -d '{"repo_url": "https://example.com/smoke", "default_branch": "main"}'
# Response: {"repo_id":"<uuid>","created":true,"mode":"embedded"}

# ingest.coverage -- Cobertura XML with repo_id+sha on root.
# Substitute <repo_id> with the UUID returned above.
curl --fail-with-body -X POST \
     "${CLEAN_CODE_GATEWAY_URL}/v1/ingest/coverage" \
     -H "Authorization: Bearer ${CLEAN_CODE_OIDC_TOKEN}" \
     -H "X-OIDC-Subject: smoke-operator" \
     -H "Content-Type: application/xml" \
     -d '<?xml version="1.0"?>
<coverage repo_id="<repo_id>" sha="0000000000000000000000000000000000000001">
  <packages><package><classes><class filename="src/main.go">
    <lines><line number="1" hits="1"/></lines>
  </class></classes></package></packages>
</coverage>'
```

Neither verb supports operator-pinned `repo_id` UUIDs, which
is why the load scenario's deterministic fixture set is
pre-seeded via the SQL above, not via these verbs. Re-run
the SQL seed once per lab refresh / release-tag acceptance
run; expect ~5 seconds wall clock against a healthy lab DB.

Then run the k6 scenario with `--summary-export` to archive
the run JSON for release-tag review. A successful run
reports `checks ........ 100.00%` in the summary; anything
less than `99.00%` exits with code 99.

## Why these thresholds

The `http_req_duration{name:eval.gate}` percentile pins come
directly from tech-spec Sec 8.3 lines 916-924; if those targets
change, update both the spec and the threshold block in
`eval_gate_load.js`.

The `http_req_failed` and `checks` floors are not in the spec
SLO table; they are belt-and-braces guards that prevent a
fast-failing server (e.g. 401 in 1 ms) from PASSING the
percentile thresholds while delivering zero meaningful work.
Without them the SLO assertion would be a vacuous green.

The `degraded is false` per-request check (inside the
`checks{name:eval.gate}` floor) addresses the iter-1 evaluator
finding that a degraded-only run would have vacuously passed
the latency thresholds without measuring the predicate-eval
path the SLO is published for.
