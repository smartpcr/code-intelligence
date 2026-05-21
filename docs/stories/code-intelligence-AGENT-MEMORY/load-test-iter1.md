# Load-test calibration — iter 1

> ⚠ **Provenance:** IN-PROCESS STUB BASELINE — gen_artifact.go against httptest mgmt-mock + in-process AgentService gRPC stub; NOT the §8.3 production seal (which requires the deploy/local stack with seeded 200 k LOC fixture).
>
> See `docs/code-intelligence/agent-memory/load-test-calibration.md`
> for the operator workflow that distinguishes shape-baseline
> artifacts (CI / PR refresh) from §8.3 production-seal
> calibrations (deploy/local stack, seeded 200 k LOC fixture,
> 30-minute window).
>
> **Production-seal artifact pending.** This run does NOT
> satisfy the Stage 8.4 acceptance criterion at
> `docs/stories/code-intelligence-AGENT-MEMORY/implementation-plan.md:1479-1486`.
> The operator action that produces the production-seal
> artifact is tracked in
> `docs/stories/code-intelligence-AGENT-MEMORY/operator-action-items.md`.

> **Generator.** This file is written by the `loadtest-harness` binary
> (`services/agent-memory/cmd/loadtest-harness`). It is the **Stage 8.4**
> calibration artifact described in
> `docs/stories/code-intelligence-AGENT-MEMORY/implementation-plan.md` §8.4.
> The values below are **informational** — the operator pins post-
> calibration SLO numbers into tech-spec.md §8.3 via the §8.3 override
> route, not by editing this file.

```yaml
profile: nominal
started_at: 2026-05-20T23:25:32Z
finished_at: 2026-05-20T23:55:32Z
planned_duration: 30m0s
actual_duration: 30m0.0529511s
repo_id: ca11ca11-0000-4000-8000-000000000001
seeded_fixture_loc: 200000
random_seed: 1779319532148803500
error_budget_ratio: 0.01
budget_breaches: 0
aborted: false
completion_reason: completed
provenance: IN-PROCESS STUB BASELINE — gen_artifact.go against httptest mgmt-mock + in-process AgentService gRPC stub; NOT the §8.3 production seal (which requires the deploy/local stack with seeded 200 k LOC fixture).
```

**Status:** PASS — no verb exceeded the 0.01 error budget across 5 verbs

## Per-verb percentiles

| Verb | Requested RPS | Achieved RPS | Sent | Failed | Err % | p50 | p95 | p99 | SLO p95 | SLO p99 | SLO p95 met | SLO p99 met | Budget met |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | :---: | :---: | :---: |
| `agent.recall` | 50.000 | 49.994 | 89992 | 0 | 0.000 | 16.442ms | 18.347ms | 18.584ms | 1.500s | 4.000s | ✅ | ✅ | ✅ |
| `agent.observe` | 50.000 | 49.991 | 89987 | 0 | 0.000 | 6.522ms | 8.318ms | 8.658ms | 0.400s | 1.500s | ✅ | ✅ | ✅ |
| `agent.expand` | 20.000 | 19.999 | 35999 | 0 | 0.000 | 26.004ms | 26.340ms | 26.640ms | 1.500s | 4.000s | ✅ | ✅ | ✅ |
| `agent.summarize` | 5.000 | 5.000 | 9001 | 0 | 0.000 | 52.812ms | 53.144ms | 53.570ms | 4.000s | 10.000s | ✅ | ✅ | ✅ |
| `mgmt.ingest_spans` | 0.833 | 0.834 | 1501 | 0 | 0.000 | 5.504ms | 7.968ms | 8.401ms | 2.000s | 5.000s | ✅ | ✅ | ✅ |

## Learning-quality SLOs

Source: **labelled-query proxy** (§8.3's contract definition is a post-hoc join over
`Observation` × `RecallContextLog`; the harness measures the proxy on the
recall response payload).

- **K =** 20
- **Labelled queries evaluated:** 74944
- **`rank_of_correct_node_at_k20`:** 2.0000  (SLO ≤ 5, met: ✅)
- **`concept_hit_fraction_at_k20`:** 1.0000  (SLO ≥ 0.25, met: ✅)

## Operator notes

- default --max-inflight is 256 (was 64 in iter 0); raise when `Open-loop scheduler hygiene` reports dropped ticks AND achieved RPS lags requested RPS
- labelled-query fixture starter: `docs/stories/code-intelligence-AGENT-MEMORY/labeled-queries.sample.json` (pass via `--labeled-queries` or `LABELED_QUERIES=` make var)
- operator workflow: see `docs/code-intelligence/agent-memory/load-test-calibration.md`

