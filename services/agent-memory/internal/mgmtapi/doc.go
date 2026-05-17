// Package mgmtapi implements the Stage 7.1 Onboarding write
// verbs of the Management Surface (architecture.md §3.8, §6.2.1;
// tech-spec.md §8.5) per implementation-plan.md Stage 7.1:
//
//	POST /v1/repos                       mgmt.register
//	POST /v1/repos/{repo_id}/ingest      mgmt.ingest
//	POST /v1/repos/{repo_id}/ingest_delta mgmt.ingest_delta
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
// Behavioural invariants enforced by this package (cross-checked
// against implementation-plan.md Stage 7.1 test scenarios):
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
//     `repo_event(kind='manual')` row. The idempotency is keyed
//     on the unique index installed by migration 0006a; the
//     `repo_event` dedupe is implemented at the verb layer
//     (skip the RepoEvent INSERT when the `ingest_jobs` upsert
//     reports the row already existed).
//   - A request missing the `Authorization: Bearer …` header is
//     rejected with 401 BEFORE any database read or write
//     happens. The auth middleware runs first; the request body
//     is not even read.
//   - mgmt.ingest validates the operator-supplied SHA shape
//     BEFORE any database read. A malformed SHA returns 400
//     even when Postgres is down.
//   - mgmt.ingest does NOT fall back to the cached
//     repo.current_head_sha when the HEAD resolver fails on a
//     default-SHA call: a resolver outage surfaces as 502 and
//     no ingest_jobs row is enqueued.
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
//
// Tests:
//
//   - handler_unit_test.go: full request → auth → resolver →
//     DB pipeline driven by `go-sqlmock`, covering every test
//     scenario in the Stage 7.1 brief plus the e2e §1 idempotent
//     re-register case and the typed-error matrix.
//   - oidc_test.go: real RSA keys + httptest JWKS server,
//     covering RS256/384/512 success, alg-confusion (none /
//     HS256), expired / nbf-future / wrong iss / wrong aud /
//     bad sig / unknown kid, thundering-herd cache hits, fail-
//     closed config including the https-scheme requirement.
//   - git_resolver_test.go: every git ls-remote outcome
//     (success / tag-only-rejected / unknown-ref / git-exit-
//     nonzero / garbage / case-folding / empty-input) driven
//     by an injected runCmd hook so no real git binary is
//     ever spawned in CI.
//   - cmd/mgmt-api/main_test.go covers the composition-root
//     smoke test (config loading, env-var parsing).
package mgmtapi
