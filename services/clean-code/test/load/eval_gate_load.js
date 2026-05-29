// Stage 10.3 -- `eval.gate` sustained-load + SLO conformance scenario.
//
// # What this script asserts
//
// The clean-code service publishes per-PR SLO targets for
// the `eval.gate` HTTP/gRPC verb in tech-spec
// `docs/stories/code-intelligence-CLEAN-CODE/tech-spec.md`
// Sec 8.3 lines 916-924:
//
//   | Surface     | Metric            | p50    | p95    | p99 |
//   |-------------|-------------------|--------|--------|-----|
//   | `eval.gate` | response latency  | 200 ms | 800 ms | 2 s |
//
// This k6 scenario drives the verb at the brief's pinned
// load shape -- "100 repos and 50 scans/min sustained for 30
// minutes" (implementation-plan Stage 10.3 line 870) -- and
// fails the run if any of the three percentiles exceeds the
// published target. `constant-arrival-rate` is the right k6
// executor for the brief because:
//
//   - The brief pins THROUGHPUT (50/min), not VU count. With
//     `constant-vus` the throughput would track the
//     server's latency (slower server -> fewer requests),
//     hiding the SLO regression we want to catch. With
//     `constant-arrival-rate` k6 holds the request rate
//     constant and lets VU pool grow up to `maxVUs` if
//     server latency causes back-pressure -- the scenario
//     fails LOUDLY (`dropped_iterations`) if even `maxVUs`
//     cannot sustain 50/min, which is itself a useful SLO
//     regression signal.
//   - `duration: '30m'` matches the brief verbatim. The 30
//     minute sustain window is long enough to flush any
//     one-shot warmup effects (connection pool, JIT, gRPC
//     channel auth) and exercise the steady-state SLO.
//
// # Repo cycling (100 repos)
//
// The 100 repos are encoded as deterministic UUIDs
// `00000000-0000-0000-0000-{NNN}` where NNN is the
// zero-padded index. A pre-test setup step
// (operator-provided -- see README.md for the seed script)
// registers these repos via `mgmt.register_repo` so the
// `eval.gate` happy path returns canonical verdicts rather
// than `repo_not_found` errors that would skew the
// latency histogram.
//
// Each request samples a repo uniformly at random; over
// 30 min * 50 req/min = 1500 requests, each repo receives
// ~15 requests in expectation -- enough to balance
// per-repo policy-cache hit/miss while keeping the test
// load deterministic.
//
// # SHA generation
//
// The brief does not pin a SHA-set shape; we generate a
// unique SHA per request (`sha-${timestamp}-${vu}-${iter}`)
// so the cache-cold path of the evaluator is exercised
// (vs reusing one SHA per repo, which would let the
// per-(repo, sha, policy_version) memoised verdict path
// short-circuit the predicate evaluation and hide the
// real SLO cost). This matches the architecture Sec 6.2
// description of `eval.gate` as "looks up active rules
// for `policy_version_id`, fetches MetricSample rows for
// the SHA, evaluates predicates" -- the cache-cold path
// is the one the SLO is published for.
//
// # Operational notes (not enforced here)
//
//   - k6 is NOT part of the CI toolchain (services/clean-code/Makefile
//     has no `make load` target). This script is an
//     OPERATOR artifact intended for the lab-bare-metal
//     validation lane documented in e2e-scenarios.md line
//     13 ("`Load and SLO conformance` for the lab-bare-metal
//     validation"). Running it requires a deployed
//     clean-code service with the 100 fixture repos
//     pre-registered; see README.md.
//   - The OPERATOR is expected to pin the SLO floor via the
//     `--summary-export` flag and archive the result JSON
//     alongside each release tag so a regression across
//     releases is reviewable. The thresholds below cause k6
//     to exit non-zero on any percentile breach, so a CI
//     wrapper (when the lab lane is built) can gate on the
//     exit code without parsing the JSON.
import http from 'k6/http';
import { check } from 'k6';

// CLEAN_CODE_GATEWAY_URL points at the gateway base URL the
// scenario should drive. Defaults to localhost:8080 for a
// developer-laptop smoke run; the lab harness overrides with
// the bare-metal LB endpoint.
const BASE = __ENV.CLEAN_CODE_GATEWAY_URL || 'http://localhost:8080';

// CLEAN_CODE_OIDC_TOKEN is the bearer token the gateway's
// OIDC middleware validates (architecture Sec 6.1 / Stage
// 4.4). The scenario MUST be run with a token issued for an
// operator role that holds the `eval.gate` scope; an empty
// or expired token would land every request as 401 and the
// latency thresholds would pass on the fast-failure path.
// We therefore omit the Authorization header when the env
// var is unset rather than send a literal "Bearer " prefix
// -- the gateway will respond 401 either way, but an
// unauthenticated request without the header surfaces the
// configuration error earlier in the run-summary's
// `http_req_failed` rate (which carries its own SLO
// threshold below).
const TOKEN = __ENV.CLEAN_CODE_OIDC_TOKEN || '';

// 100 deterministic repo UUIDs the scenario cycles through.
// Generated once at script load time (k6 runs the
// init-context code in every VU) so the per-iteration cost
// is a single random index lookup, not a UUID construction.
const REPO_COUNT = 100;
const repos = (() => {
  const out = new Array(REPO_COUNT);
  for (let i = 0; i < REPO_COUNT; i++) {
    const nnn = String(i).padStart(12, '0');
    out[i] = `00000000-0000-0000-0000-${nnn}`;
  }
  return out;
})();

// Per-PR load shape: 50 scans/min sustained for 30 minutes.
//
//   - `rate: 50, timeUnit: '1m'` is the brief's 50/min.
//   - `duration: '30m'` is the brief's sustain window.
//   - `preAllocatedVUs: 50` warms a pool large enough to
//     absorb the first burst without paying VU-spawn cost
//     mid-test. At 50 req/min ~= 0.83 req/s, a request that
//     ever runs longer than 60s starves the pool; we cap
//     growth at `maxVUs: 200` so the scenario aborts cleanly
//     (via `dropped_iterations`) rather than running away
//     when the server is overloaded.
//   - `tags: {name: 'eval.gate'}` is the lookup key the
//     threshold filter below uses; pinning it here keeps the
//     scenario aggregable with other future verbs (e.g.
//     `mgmt.read.regressions`) without a histogram-key clash.
//
// Threshold contract: any breach of the p50/p95/p99 target,
// or an error rate >= 1%, or check-failure rate >= 1% fails
// the k6 process with exit code 99 -- enough for a CI
// wrapper to gate on the exit code without parsing the
// summary JSON. The `http_req_failed` and `checks` floors
// are belt-and-braces guards that the rubber-duck flagged:
// without them, a server that returns 401 in 1ms (fast
// failure) would PASS the latency thresholds while
// delivering zero meaningful coverage -- the SLO assertion
// would be a vacuous green.
export const options = {
  scenarios: {
    eval_gate_sustained: {
      executor: 'constant-arrival-rate',
      rate: 50,
      timeUnit: '1m',
      duration: '30m',
      preAllocatedVUs: 50,
      maxVUs: 200,
      gracefulStop: '30s',
      exec: 'evalGate',
    },
  },
  thresholds: {
    // Tech-spec Sec 8.3 lines 916-924 -- the headline
    // pin. A breach of any percentile aborts the run.
    'http_req_duration{name:eval.gate}': [
      'p(50)<200',
      'p(95)<800',
      'p(99)<2000',
    ],
    // Failure-rate floor. A server returning 4xx/5xx fast
    // would otherwise let the latency thresholds pass on
    // the fast-failure path, hiding the regression.
    'http_req_failed{name:eval.gate}': ['rate<0.01'],
    // Check-result floor. The `evalGate` function asserts
    // status==200 AND verdict is one of the canonical
    // verdicts; the threshold ensures >= 99% of requests
    // satisfy both checks.
    'checks{name:eval.gate}': ['rate>0.99'],
  },
};

// evalGate is the per-VU iteration body. Each call:
//
//   - picks a repo uniformly at random,
//   - mints a unique SHA so the verdict cache cannot
//     short-circuit predicate evaluation,
//   - POSTs to `/v1/eval/gate` (the HTTP gateway pin
//     `internal/api/router.go::PathPrefix = "/v1/"` +
//     namespace/verb pattern), and
//   - asserts the response is HTTP 200 and the JSON
//     `verdict` field is one of the canonical
//     `{pass, warn, block}` values (architecture Sec 5.4.3
//     line 1237, iter-1 evaluator item 6).
//
// The `tags: {name: 'eval.gate'}` annotation is what
// makes the per-percentile and per-failure-rate threshold
// filters match this iteration; do NOT remove it.
export function evalGate() {
  const repoId = repos[Math.floor(Math.random() * REPO_COUNT)];
  // Unique SHA per request so the per-(repo, sha) memoised
  // verdict cache cannot short-circuit. `__VU` and `__ITER`
  // are k6-provided per-iteration globals.
  const sha = `sha-${Date.now()}-${__VU}-${__ITER}`;
  const body = JSON.stringify({ repo_id: repoId, sha: sha });
  const params = {
    headers: { 'Content-Type': 'application/json' },
    tags: { name: 'eval.gate' },
  };
  if (TOKEN !== '') {
    params.headers['Authorization'] = `Bearer ${TOKEN}`;
  }
  const res = http.post(`${BASE}/v1/eval/gate`, body, params);
  check(
    res,
    {
      'status is 200': (r) => r.status === 200,
      'verdict is canonical': (r) => {
        if (r.status !== 200) {
          return false;
        }
        let parsed;
        try {
          parsed = r.json();
        } catch (e) {
          return false;
        }
        const v = parsed && parsed.verdict;
        return v === 'pass' || v === 'warn' || v === 'block';
      },
    },
    { name: 'eval.gate' },
  );
}
