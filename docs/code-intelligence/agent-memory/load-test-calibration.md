# Load-test calibration — operator workflow

> **Scope.** Operator workflow for running the Stage 8.4
> calibration harness against the agent-memory service. The
> implementation lives at
> `services/agent-memory/cmd/loadtest-harness` and writes a
> machine-readable calibration artifact at
> `docs/stories/code-intelligence-AGENT-MEMORY/load-test-iter1.md`.
> The artifact is overwritten on every run; this file (the
> operator workflow) is committed and stable.

## Quick start

> **⚠ READ FIRST — production-seal vs dev-preflight.** The
> Stage 8.4 acceptance criterion calls for a 30-minute
> calibration against a **real** 200 k LOC code corpus
> ingested through the agent-memory mgmt-api wire path.
> `make seed-fixture-200k` is a SYNTHETIC structural
> stand-in for that corpus — useful for CI / dev preflight
> but NOT a production-seal seed. The two options below are
> structurally distinct; the resulting artifact's provenance
> banner stamps which path produced it so a reviewer can
> tell the two apart at a glance. **Choose deliberately.**

### Option A — Production-seal calibration (operator-provided real corpus)

```bash
# A1. Bring up the deploy/local stack (Docker + Postgres + Qdrant +
#     agent-memory mgmt-api on :8444 + agent-api on :8443).
cd deploy/local && docker-compose up -d

# A2. Ingest a REAL 200 k LOC code corpus into the agent-memory
#     graph against the deploy/local stack. There is NO Makefile
#     target for this step — it is the operator's responsibility
#     to choose the corpus source. Acceptable shapes:
#       - OTel-agent feed pointed at a built repo (≥ 200 k LOC).
#       - IDE-extension trace export from a real engineer session.
#       - Repo-walker batch (e.g. tree-sitter over a checkout).
#     Each MUST land via the same mgmt.ingest_spans surface the
#     harness later exercises, against the chosen REPO_ID.
#
#     DO NOT substitute `make seed-fixture-200k` here — it is
#     Option B and produces a SYNTHETIC seed, not a real corpus.
export FIXTURE_REPO_ID=<uuid-for-real-corpus>

# A3. 30-minute production-seal calibration. Paths supplied via
#     LABELED_QUERIES / ARTIFACT are accepted as EITHER repo-rooted
#     (the form you naturally type from the repo root) OR as
#     `../../…` paths relative to `services/agent-memory/`. The
#     Makefile's `resolve_path` helper tries both forms so
#     `make -C services/agent-memory` does not silently miss the
#     file.
make -C services/agent-memory loadtest-calibration \
    REPO_ID=$FIXTURE_REPO_ID \
    SEEDED_LOC=200000 \
    LABELED_QUERIES=docs/stories/code-intelligence-AGENT-MEMORY/labeled-queries.sample.json \
    PROVENANCE="DEPLOY/LOCAL STACK NOMINAL CALIBRATION — $(date -u +%F); seeded <corpus>/<sha>"

# A4. Inspect the rewritten artifact (always under docs/stories/…,
#     NOT services/agent-memory/docs/stories/…; the Makefile pins
#     --artifact to the repo-rooted path via `resolve_path`).
${EDITOR:-less} docs/stories/code-intelligence-AGENT-MEMORY/load-test-iter1.md
# Verify the provenance banner reads "DEPLOY/LOCAL STACK NOMINAL
# CALIBRATION — …", NOT "IN-PROCESS STUB BASELINE — …".

# A5. (Optional) Pin post-calibration numbers into tech-spec.md §8.3
#     via the §8.3 override route. Do NOT edit tech-spec.md directly.
```

### Option B — Developer preflight (synthetic stand-in; NOT a production seal)

```bash
# B1. Bring up the deploy/local stack OR use the in-process
#     mgmt/agent stubs (the harness embeds both for CI use).
cd deploy/local && docker-compose up -d   # optional for Option B

# B2. Synthesise ~200 000 OTel spans through the mgmt-api wire
#     path. Structural stand-in only — the spans are synthetic and
#     do NOT represent a real code corpus.
make -C services/agent-memory seed-fixture-200k MGMT_TARGET=http://localhost:8444
export FIXTURE_REPO_ID=ca11ca11-0000-4000-8000-000000000001  # default; override per-fixture

# B3. Calibration run (the smoke profile finishes in < 1 s; the
#     nominal profile takes 30 minutes against the synthetic seed
#     and is rarely useful at this duration without a real corpus).
make -C services/agent-memory loadtest-calibration-smoke \
    REPO_ID=$FIXTURE_REPO_ID \
    LABELED_QUERIES=docs/stories/code-intelligence-AGENT-MEMORY/labeled-queries.sample.json

# B4. Inspect the rewritten artifact. The provenance banner will
#     correctly read "IN-PROCESS STUB BASELINE — …" (or
#     "SYNTHETIC SEED — …" if you point at a real mgmt) so this
#     output is unambiguously NOT a production seal.
${EDITOR:-less} docs/stories/code-intelligence-AGENT-MEMORY/load-test-iter1.md
```

> **Path-resolution contract.** When you `make -C services/agent-memory …`
> from the repo root, GNU make changes the cwd to
> `services/agent-memory/` before running the recipe. Any
> relative path you pass on the command line is resolved against
> that post-`-C` cwd, NOT the repo root you typed in. The
> Makefile's `resolve_path` helper (defined immediately above the
> `loadtest-calibration` target) tries both interpretations and
> returns the first existing path so the natural repo-rooted form
> you see in this quick-start works without modification. The
> `ARTIFACT` default already lives at `../../docs/stories/…` so
> the writer always targets the repo's committed artifact, not a
> phantom under `services/agent-memory/docs/`.
>
> **About `make seed-fixture-200k` (Option B's seed step).** This
> target uses the load-test harness itself in mgmt-only burst mode
> to push ~200 000 synthetic OTel spans through the mgmt-api wire
> path. It is a STRUCTURAL stand-in (matches the expected edge /
> node count order of magnitude with synthetic payload) for
> development and CI use; operators running the §8.3 production
> seal MUST replace it with a real corpus ingest (Option A above).
> The provenance banner on the resulting calibration artifact
> stamps the seed-source so a reviewer can tell synthetic from
> real at a glance. See
> `docs/stories/code-intelligence-AGENT-MEMORY/operator-action-items.md`
> for the tracked production-seal-pending operator action.

## Flag reference

| Flag                | Default                                  | Purpose                                                                                                |
| ------------------- | ---------------------------------------- | ------------------------------------------------------------------------------------------------------ |
| `--profile`         | `nominal`                                | `nominal` (30 min, §8.3 envelope) or `smoke` (200 ms, sub-second CI check)                             |
| `--duration`        | profile-default                          | Override the run duration                                                                              |
| `--repo-id`         | `ca11ca11-0000-4000-8000-000000000001`   | Fixture repository id; MUST be UUID-formatted (mgmt-api `internal/mgmtapi.handler.reUUID` rejects non-UUIDs) |
| `--seed`            | `0` (wall clock)                         | PRNG seed; echoed onto the artifact for replay                                                         |
| `--max-inflight`    | `256`                                    | Per-verb open-loop in-flight cap. `256 = ceil(50 RPS × 4 s p99) × headroom`. Raise it when dropped-tick numbers appear |
| `--spans-per-batch` | `50`                                     | mgmt.ingest_spans batch size (cap 1000 per §8.3)                                                       |
| `--seeded-loc`      | `0`                                      | Informational; stamped on the artifact                                                                 |
| `--labeled-queries` | empty                                    | Path to a JSON array of `{query, expected_node_id, expected_concept_ids[]}` entries; populates learning-quality numerics |
| `--mgmt-target`     | `http://localhost:8444`                  | Management API base URL                                                                                |
| `--mgmt-ingest-path`| `/v1/spans`                              | POST path on the mgmt-api                                                                              |
| `--agent-target`    | `localhost:8443`                         | AgentService gRPC target                                                                               |
| `--no-tls`          | `false`                                  | Dial AgentService without TLS (deploy/local only)                                                      |
| `--no-tracer`       | `false`                                  | Skip `obs.SetupTracer` (useful in CI where the OTLP endpoint is absent)                                |
| `--skip-agent`      | `false`                                  | Skip the agent.* scenarios (mgmt-only run)                                                             |
| `--skip-mgmt`      | `false`                                  | Skip the mgmt.ingest_spans scenario (agent-only run)                                                   |
| `--metrics-addr`    | empty                                    | Bind address for the harness `/metrics` surface (empty = disabled)                                     |

## Labelled-query fixture

The §8.3 learning-quality SLOs (`rank_of_correct_node_at_k20`,
`concept_hit_fraction_at_k20`) are defined as a post-hoc join
over `Observation × RecallContextLog × Episode`. The harness
measures a **labelled-query proxy** off the recall response
payload — strictly the same shape, computed inline rather than
in the join. The artifact tags every learning-quality number
with the `labelled-query proxy` provenance so a reviewer never
mistakes the proxy for the contract measurement.

A starter fixture lives at
`docs/stories/code-intelligence-AGENT-MEMORY/labeled-queries.sample.json`.
Each entry has the shape:

```json
{
  "query": "natural-language query the agent surfaces",
  "expected_node_id": "func:<repo>/<package>.<Symbol>",
  "expected_concept_ids": ["concept:<repo>/<group>/<name>"]
}
```

Either `expected_node_id` OR `expected_concept_ids` may be empty
to opt out of that specific measurement for the query; both
empty makes the query a pure load driver (no learning-quality
contribution).

When `--labeled-queries` is omitted the artifact's
learning-quality rows read `n/a (no labelled queries supplied)`
and both SLOs report `met: ❌`. Supply the flag for any
calibration run that needs to satisfy the §8.3 numeric
acceptance.

## Exit codes

| Code | Meaning                                                                                          |
| ---: | ------------------------------------------------------------------------------------------------ |
| `0`  | All verbs within the 1 % error budget; artifact written successfully.                            |
| `1`  | Artifact was written but at least one verb exceeded the error budget. Inspect the artifact's "Error-budget breaches" section. |
| `2`  | Harness construction or run failed (config invalid, dial failure, etc); see stderr.              |
| `3`  | Run aborted before planned duration elapsed (SIGINT / SIGTERM / parent-context deadline). The partial artifact is still written so the operator can inspect what was captured; CI MUST NOT pin a baseline from an exit-3 run. |

Note: exit code precedence is `aborted (3)` > `budget breach (1)` > `OK (0)`.
A partial-window run with a synthetic breach is reported as
`aborted` because partial percentile/budget numbers are not a
valid baseline.

## /metrics surface

When `--metrics-addr` is set the binary exposes a Prometheus
endpoint serving the per-verb histogram family
(`loadtest_harness_request_duration_seconds`). Every sample is
observed with a `verb="agent.recall"|"agent.observe"|"agent.expand"|"agent.summarize"|"mgmt.ingest_spans"`
label.

**This surface is an SLO-gate signal, not a percentile-parity
source for the markdown artifact.** Read it for:

- "Did p95 cross 1.5 s?" / SLO-line-crossing alerts in
  Grafana / Prometheus alertmanager.
- Per-verb shape diffing between two stacks at the bucket
  boundaries.

Do NOT read it for the artifact's exact `p50` / `p95` / `p99`
numbers — they will disagree (see the bucket-quantization
section below).

```promql
# per-verb p95 across all five verbs — APPROXIMATE (one-bucket
# resolution) but exact at the §8.3 SLO threshold boundary
histogram_quantile(
  0.95,
  sum(rate(loadtest_harness_request_duration_seconds_bucket[5m])) by (verb, le)
)

# single verb, e.g. recall p99
histogram_quantile(
  0.99,
  sum(rate(loadtest_harness_request_duration_seconds_bucket{verb="agent.recall"}[5m])) by (le)
)
```

**Bucket quantization vs the markdown artifact.**
The /metrics histogram does NOT reproduce the markdown
artifact's `p50`/`p95`/`p99` numbers. The artifact uses the
nearest-rank percentile method over the raw sample slice
(`idx = ceil(p*n) - 1`), so its percentiles are at
single-sample resolution. The /metrics histogram is bucketed
at SLO-aligned boundaries (`0.05, 0.1, 0.2, 0.4, 0.8, 1.5, 2,
4, 5, 10, 30`), so `histogram_quantile()` returns linearly-
interpolated values within whichever bucket contains the
percentile rank.

The two surfaces AGREE on whether a percentile crosses an
§8.3 SLO threshold (the bucket boundaries land exactly at the
thresholds for that purpose), and AGREE ON NOTHING ELSE.

| Need                                       | Read                                                             |
| ------------------------------------------ | ---------------------------------------------------------------- |
| "Did p95 cross 1.5 s?" (SLO gate)          | EITHER — both surfaces agree at bucket boundaries.               |
| Exact millisecond p50/p95/p99 baseline     | The markdown artifact at `docs/stories/.../load-test-iter1.md`.  |
| Live trend / SRE Grafana per-verb signal   | The `/metrics` Prometheus scrape via the query above.            |

## Regenerating the persisted artifact without a deploy/local stack

When a real stack is not available (e.g. PR CI), refresh the
persisted artifact via the in-process helper. The canonical
entry point is the make target — `make -C` pins the working
directory to `services/agent-memory/` (which is where `go.mod`
lives) so the command works from ANY directory in the worktree:

```bash
# From ANY directory in the worktree (this is the recommended form):
make -C services/agent-memory loadtest-gen-artifact

# Fast local iteration (2-minute window, NOT for committing):
make -C services/agent-memory loadtest-gen-artifact DURATION=2m

# If you've already cd'd into services/agent-memory/:
make loadtest-gen-artifact
```

The make target defaults to a **30-minute** duration to match
the §8.3 nominal envelope, so the committed artifact's
`planned_duration` field is the production-seal length. Use
the 2-minute fallback only for local fast-iteration; do NOT
commit a sub-30m artifact (the §8.3 acceptance criterion is
the 30-minute window).

> **Why a make target and not `go run` directly?**
> `go run ./services/agent-memory/cmd/loadtest-harness/gen_artifact.go`
> from the repo root FAILS — Go module resolution scans the cwd
> for `go.mod`, and the repo has no top-level go.mod (the module
> root is `services/agent-memory/`). The make target chdir's
> via `-C` before invoking `go run`, sidestepping the failure
> mode. If you must invoke the helper directly, `cd` into
> `services/agent-memory/` first.

The helper spins up an in-process `httptest` mgmt-api mock and an
in-process AgentService gRPC stub, then invokes the harness at the
§8.3 **nominal** load envelope (50 RPS recall + 50 RPS observe +
20 RPS expand + 5 RPS summarize + ~0.833 RPS mgmt.ingest_spans)
for the supplied duration and writes the resulting artifact to
`docs/stories/code-intelligence-AGENT-MEMORY/load-test-iter1.md`.
The helper passes a default `--provenance` banner that stamps
the artifact as `IN-PROCESS STUB BASELINE` so a reviewer can see
the artifact's source at a glance without reading this doc.

Pass `--profile smoke` to fall back to the lighter sub-minute
sanity check when iterating locally. Pass
`GEN_PROVENANCE="DEPLOY/LOCAL STACK NOMINAL CALIBRATION — …"`
to the make target (or `--provenance "…"` to the binary directly)
when running against a real stack so the persisted artifact is
correctly labelled as a §8.3 production-seal candidate.

**In-process baseline vs §8.3 production-seal calibration.**
The in-process gen_artifact path is NOT a substitute for the
§8.3 production-seal calibration. The numbers it records
reflect the harness's own per-call overhead plus deterministic
synthetic stub delays — useful as a "the harness runs cleanly
and writes a well-formed artifact" gate, not as a tech-spec
SLO baseline. The §8.3 production seal still requires (this
is Quick start **Option A** above, restated):

1. The deploy/local stack brought up
   (`cd deploy/local && docker-compose up -d`).
2. **A REAL 200 k LOC corpus** ingested via the operator's
   choice of OTel-agent feed / IDE-extension trace export /
   repo-walker batch landing through the mgmt-api's
   `mgmt.ingest_spans` surface against a chosen `REPO_ID`.
   **DO NOT substitute `make -C services/agent-memory
   seed-fixture-200k` for this step** — that target is the
   Quick start Option B developer preflight and synthesises
   structural stand-in spans, not a real code corpus; the
   resulting artifact will be (correctly) stamped
   `SYNTHETIC SEED` and will NOT satisfy the §8.3 seal.
3. A 30-minute nominal-profile run via
   `make -C services/agent-memory loadtest-calibration`
   with `PROVENANCE="DEPLOY/LOCAL STACK NOMINAL CALIBRATION — …"`
   stamped explicitly (the production-seal vocabulary the
   artifact renderer recognises to suppress the
   `Production-seal artifact pending` callout).
4. Operator review of the resulting artifact and pinning of
   post-calibration §8.3 SLO numbers via the §8.3 override
   route. Verification: the artifact's provenance banner
   reads `DEPLOY/LOCAL STACK NOMINAL CALIBRATION — …`
   instead of `IN-PROCESS STUB BASELINE — …` or
   `SYNTHETIC SEED — …`, and the `Production-seal artifact
   pending` callout is absent.

The pending production-seal action is tracked in
`docs/stories/code-intelligence-AGENT-MEMORY/operator-action-items.md`.

The provenance banner at the top of the persisted artifact is
how a reviewer distinguishes one from the other; this doc and
the artifact's banner are the source of truth.

## Reproducibility

Every artifact stamps the `random_seed` it ran with. Replay a
flaky run by re-invoking the binary with `--seed <value>` and
the same `--duration`. The seed governs:

- Per-verb tick rotation order
- LabeledQuery rotation for `agent.recall`
- Span / context / episode id generation in the harness's
  deterministic encoders

The mgmt scenario's OTel trace/span ids are derived from the
per-tick seed via the
`ca110000ca110000<seed-hex>:<seed-hex>` encoding — the upper
half of every traceId is a non-zero "ca11..." sentinel so the
mgmt-api's `normalizeOTelID` (`internal/mgmtapi/spans.go`)
never rejects the batch with an "all-zero trace id" error.

## Repo-id contract

The harness's mgmt scenario forwards `--repo-id` into:

- the HTTP header `X-Mgmt-Repo-ID` (mirrors
  `internal/mgmtapi.MgmtRepoIDHeader`); and
- the `mgmt.repo_id` OTLP resource attribute on each
  `resourceSpans` batch entry (mirrors
  `internal/mgmtapi.MgmtRepoIDResourceAttr`).

Both are validated against the mgmt-api's UUID regex
(`internal/mgmtapi/handler.reUUID`). A non-UUID value causes
the mgmt scenario's verbs to fail with HTTP 400 and bumps the
error-ratio above the 1 % budget. The default value
(`ca11ca11-0000-4000-8000-000000000001`) is a stable UUID-shaped
placeholder; production-seal runs (Quick start Option A)
override it with the fixture's real repo-id — the operator
chooses one when ingesting the real corpus (via OTel-agent
feed / IDE-extension export / repo-walker batch) and reuses
the same value at the `loadtest-calibration` step. Developer-
preflight runs (Quick start Option B) may use the default or
override via `make -C services/agent-memory seed-fixture-200k
REPO_ID=<dev-fixture-uuid>` — that command is the **Option B
synthetic stand-in**, NOT the production-seal seed path.
