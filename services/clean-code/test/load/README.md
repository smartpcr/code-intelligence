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
3. **Pre-registered fixture repos**: the scenario references
   100 deterministic UUIDs of the form
   `00000000-0000-0000-0000-{NNN}` where `NNN` is the
   zero-padded index `000000000000`..`000000000099`. These must
   exist in the `repo` table with `scan_status='scanned'`
   AND at least one MetricSample row keyed by the SHAs the
   scenario will mint, otherwise `eval.gate` returns
   `repo_not_found` / `metrics_missing` on the fast-failure
   path and the test PASSES vacuously. See "Seeding the
   fixtures" below.
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

The 100 fixture repos must exist before the run; otherwise the
gateway short-circuits with `repo_not_found` and the SLO floor
is met on the fast-failure path. The clean-code service ships
a seeder helper (`cmd/seed-load-fixtures`) that the operator
should run once per lab refresh:

```sh
cd services/clean-code
go run ./cmd/seed-load-fixtures \
  --gateway "${CLEAN_CODE_GATEWAY_URL}" \
  --token   "${CLEAN_CODE_OIDC_TOKEN}" \
  --count   100
```

The seeder is itself a separate Stage 10 artifact; if it has
not yet landed, the operator can fall back to a manual
`mgmt.register_repo` loop using `curl`.

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
