// Package mgmtapi implements the Management Surface verbs of
// the agent-memory service (architecture.md §3.8, §6.2; tech-
// spec.md §8.5). The package currently ships:
//
//   - Stage 7.1 Onboarding write verbs:
//
//	POST /v1/repos                       mgmt.register
//	POST /v1/repos/{repo_id}/ingest      mgmt.ingest
//	POST /v1/repos/{repo_id}/ingest_delta mgmt.ingest_delta
//
//   - Stage 7.3 Feedback verb:
//
//	POST /v1/episodes/{parent_episode_id}/feedback   mgmt.feedback
//
//   - Stage 7.5 Operator read endpoints (mgmt.read.*):
//
//	GET  /v1/repos                          mgmt.read.repos
//	GET  /v1/episodes                       mgmt.read.episodes
//	GET  /v1/commits                        mgmt.read.commits
//	GET  /v1/observations                   mgmt.read.observations
//	GET  /v1/concepts                       mgmt.read.concepts
//	GET  /v1/concept_supports               mgmt.read.concept_supports
//	GET  /v1/context/{id}                   mgmt.read.context
//	GET  /v1/graph_node/{id}                mgmt.read.graph_node
//	GET  /v1/trace_observation/{id}         mgmt.read.trace_observation
//
// Transport pin (tech-spec §8.5): REST + JSON. AuthN pin: OIDC
// bearer token. The package exposes a [TokenVerifier] interface
// implemented by:
//
//   - [OIDCVerifier] — the production-grade JWKS-backed
//     RS256/384/512 verifier with full iss / aud / exp / nbf /
//     sub claim validation, kid-required header enforcement,
//     and a singleflight JWKS refresh cache. JWKSURL is
//     required to use https://. This is the verifier the
//     composition root in cmd/mgmt-api wires when
//     AGENT_MEMORY_OIDC_ISSUER / _AUDIENCE / _JWKS_URL are all
//     set.
//   - [StaticBearerVerifier] — a dev/test single-shared-secret
//     verifier. Selected only when AGENT_MEMORY_OIDC_DEV_TOKEN
//     is set AND the OIDC trio is absent; cmd/mgmt-api logs a
//     WARN line when this path is taken and refuses to boot if
//     neither verifier is configured.
//
// HEAD resolution is similarly split between production and
// dev:
//
//   - [GitLsRemoteResolver] — the production resolver,
//     invokes `git ls-remote --refs --heads <repo> <branch>`
//     and returns the SHA at refs/heads/<branch>. Branch-only:
//     a refs/tags/<branch> match is NOT honored, so an
//     operator who types `v1.0.0` into `default_branch` gets
//     a typed UnknownRef instead of a silent tag pin.
//   - [StaticHeadResolver] — opt-in dev resolver
//     (AGENT_MEMORY_HEAD_RESOLVER=static) for docker-compose
//     stacks where the remote isn't reachable.
//
// Routing convention -- dual-verb routes:
//
// /v1/repos and /v1/episodes accept BOTH GET (Stage 7.5 read)
// AND POST (Stage 7.1 register / Stage 7.3 feedback uses the
// sub-resource path). All other Stage 7.5 read paths are
// GET-only; all Stage 7.1 / 7.3 write sub-resource paths
// (/v1/repos/{id}/ingest, /v1/episodes/{id}/feedback) remain
// POST-only. The shared [Handler.route] dispatcher gates the
// method first and surfaces a 405 with a precise Allow header
// (e.g. "GET, POST" on dual-verb routes) before any handler
// touches the database.
//
// Behavioural invariants enforced by this package (cross-checked
// against implementation-plan.md Stage 7.x test scenarios):
//
// Stage 7.1 invariants:
//
//   - The webhook HMAC secret returned by mgmt.register is
//     revealed exactly ONCE, in the response body of the first
//     successful registration of a given repo URL. The secret is
//     never echoed by subsequent calls, never logged, and never
//     reachable by any [mgmt.read.*] verb (the
//     `repo_webhook_secret` ACLs in migration 0018 enforce that
//     at the SQL layer).
//   - mgmt.ingest_delta is idempotent on the tuple
//     (repo_id, from_sha, to_sha): N identical calls produce
//     exactly ONE `ingest_jobs` row AND exactly ONE
//     `repo_event(kind='manual')` row.
//   - mgmt.ingest validates the operator-supplied SHA shape
//     BEFORE any database read.
//   - mgmt.ingest does NOT fall back to the cached
//     repo.current_head_sha when the HEAD resolver fails on a
//     default-SHA call: a resolver outage surfaces as 502 and
//     no ingest_jobs row is enqueued.
//
// Stage 7.3 invariants:
//
//   - mgmt.feedback writes a child episode of kind 'feedback'
//     with parent_episode_id pointing at the agent episode,
//     plus an episode_update row recording the
//     (outcome, note, actor) tuple. The parent's
//     current_status (derived per §6.2.2 via mgmt.read.*)
//     reflects the latest episode_update.
//
// Stage 7.5 invariants:
//
//   - Every mgmt.read.* response carries the §6.3
//     [DegradedEnvelope] -- a FLAT JSON object that splats the
//     payload's fields at the top level and appends two
//     reserved keys: `"degraded": <bool>` always, and
//     `"degraded_reason": <string>` only when `degraded=true`
//     (the key is OMITTED on the happy path). For example
//     `mgmt.read.repos` emits
//     `{ "repos": [...], "degraded": false }` -- not a
//     `data`-wrapped envelope. Stage 7.5 always serves
//     `degraded=false`; Stage 8.1 flips the bit when the
//     embedding-store probe fails and stamps
//     `degraded_reason` with the typed cause.
//   - mgmt.read.episodes REQUIRES `?since=...`. Missing `since`
//     returns 400 `since_required`. Missing required params on
//     the other endpoints (`?episode_id=...` on
//     mgmt.read.observations, `?repo_id=...` on
//     mgmt.read.commits, `?concept_id=...` on
//     mgmt.read.concept_supports) and any malformed query
//     parameter return 400 `invalid_request` with a
//     descriptive `message` field carrying the parameter name
//     and the parse failure. Path-segment ID validation
//     failures emit per-resource codes:
//     `invalid_context_id`, `invalid_node_id`, `invalid_edge_id`,
//     and `invalid_sha`.
//   - mgmt.read.context returns the recall_context_log row PLUS
//     a tombstone-tolerant materialization of `selected_nodes[]`,
//     `selected_edges[]`, and `concept_versions[]`. Retired
//     nodes / edges surface the `retired_at_sha` field as a
//     badge -- the row itself is NOT dropped.
//   - mgmt.read.graph_node supports the optional `?sha=<git-sha>`
//     parameter per architecture §6.2 `(node_id, sha?)`.
//     Without `?sha=`, the handler returns the CURRENT view of
//     the node -- the card itself is always served (with a
//     `retired_at_sha` badge if the node has a tombstone), and
//     the neighbor list anti-joins both `edge_retirement` and
//     `node_retirement` so only currently-live edges and
//     currently-live neighbor nodes appear. With `?sha=X`, the
//     handler walks `repo_commit.parent_sha` from X back to
//     root via a recursive CTE and uses ancestor-set membership
//     (NOT timestamp ordering) to decide:
//       * 400 `invalid_sha` if the SHA shape is malformed,
//       * 404 `unknown_sha` if X is not in the node's repo's
//         commit log,
//       * 404 `node_not_at_sha` if `node.from_sha` is not an
//         ancestor of X (the node didn't exist yet at X),
//       * 200 with `retired_at_sha` badge when the tombstone's
//         `retired_at_sha` IS in X's ancestor chain AND is
//         NOT equal to X (X is a descendant of the
//         retirement boundary, so the node is dead at X),
//       * 200 with no badge otherwise (alive at X).
//     Neighbors in SHA-pinned mode apply the same ancestor-set
//     filter to each edge's `from_sha` / `retired_at_sha` AND
//     to each neighbor node's `from_sha` /
//     `node_retirement.retired_at_sha` (e2e-scenarios.md §200
//     -- §202 normative).
//   - Pagination: default `limit=200`, hard cap 1000; a
//     zero/negative/non-integer `limit` is rejected with 400
//     `invalid_request`. The trace_observation endpoint emits
//     a `next_offset` cursor when more rows remain (fetch
//     limit+1 trick).
//   - `since` accepts RFC3339 timestamps OR the rolling
//     window shorthand `<N>{d|h|m|s}` (e.g. `30d`, `12h`).
//     A bare integer or zero/negative value is rejected with
//     400 `invalid_request`.
//   - Bare prefix paths `/v1/context`, `/v1/graph_node`,
//     and `/v1/trace_observation` (no trailing `{id}` segment)
//     return 404 with the typed envelope codes
//     `context_id_required`, `node_id_required`, and
//     `edge_id_required` respectively -- never the Go
//     `http.ServeMux` 301 trailing-slash redirect: both the
//     bare and the trailing-slash routes are registered with
//     `http.ServeMux.Handle(..., handler)` so every GET hits
//     the authenticated handler and is answered with the
//     same JSON envelope shape as a known route.
//
// Error-mapping convention:
//
//   - 4xx is reserved for client-shape errors (malformed JSON,
//     missing required field, invalid SHA shape, unknown repo
//     id, missing token, unknown ref). The structured-log
//     record carries the full diagnostic; the response body
//     carries a typed [ErrorEnvelope] with a short
//     operator-friendly message and a stable `code` field.
//   - 5xx is reserved for infrastructure failures (PostgreSQL
//     outage, head-resolver outage, JWKS endpoint outage). The
//     raw driver / network error is NEVER echoed to the
//     caller — the structured log carries it, the response
//     body is a generic "internal error" / "upstream
//     unavailable" envelope.
//   - 401 / 503 from the auth middleware also emit the typed
//     [ErrorEnvelope] (not http.Error text/plain), with a
//     companion `WWW-Authenticate: Bearer …` header.
//   - 405 from [Handler.route] carries a precise Allow header
//     listing every verb the route supports (e.g. "GET, POST"
//     on dual-verb routes /v1/repos and /v1/episodes).
//
// Tests:
//
//   - handler_unit_test.go: full Stage 7.1 / 7.3 request →
//     auth → resolver → DB pipeline driven by `go-sqlmock`,
//     covering every test scenario in the brief plus the e2e
//     §1 idempotent re-register case and the typed-error
//     matrix.
//   - read_unit_test.go: Stage 7.5 sqlmock tests covering
//     each mgmt.read.* endpoint's happy + error paths, with
//     cross-cutting tests for auth gating, method dispatch,
//     and the DegradedEnvelope shape.
//   - read_integration_test.go: live PostgreSQL tests
//     (gated by AGENT_MEMORY_PG_URL) that seed a full
//     repo / commit / episode / observation / context /
//     concept / trace inventory and assert every read
//     endpoint surfaces the right rows, including retired
//     nodes (tombstone badge) and parent episodes' latest
//     current_status.
//   - oidc_test.go: real RSA keys + httptest JWKS server,
//     covering RS256/384/512 success, alg-confusion (none /
//     HS256), expired / nbf-future / wrong iss / wrong aud /
//     bad sig / unknown kid, thundering-herd cache hits,
//     fail-closed config including the https-scheme
//     requirement.
//   - git_resolver_test.go: every git ls-remote outcome
//     (success / tag-only-rejected / unknown-ref / git-exit-
//     nonzero / garbage / case-folding / empty-input) driven
//     by an injected runCmd hook so no real git binary is
//     ever spawned in CI.
//   - cmd/mgmt-api/main_test.go covers the composition-root
//     smoke test (config loading, env-var parsing).
package mgmtapi
