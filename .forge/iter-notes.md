# Iter notes -- Stage 6.4 HTTP+JSON gateway + OIDC auth (iter 3)

## Files touched this iter
- `services/clean-code/internal/api/auth.go` -- added `ErrAuthBackend` sentinel + updated `StaticHMACAuthenticator` doc-block (removed stale "OUT OF SCOPE" claim; points to `OIDCAuthenticator`).
- `services/clean-code/internal/api/handlers.go` -- added `CodeAuthBackend` const + `case errors.Is(err, ErrAuthBackend)` branch in `handleAuthError` (503 + `Retry-After: 30` + structured log).
- `services/clean-code/internal/api/oidc.go` -- refactored `Authenticate` to peek JWT header BEFORE parser; `cache.Key` failures now wrap as `ErrAuthBackend`, `errKidNotInJWKS` (genuine bad kid) wraps as `ErrInvalidToken`; added OIDC discovery (`{issuer}/.well-known/openid-configuration` -> `jwks_uri`); removed hardcoded `/.well-known/jwks.json` default; added `acceptedAlg` set + `peekJWTHeader` helper.
- `services/clean-code/internal/api/repo_id.go` -- (iter-2 pre-touch carried forward) `chainedBody` `io.MultiReader`-based body restore preserves oversized bodies intact; `NestedJSONBodyRepoIDExtractor(maxBytes, path...)` walks nested JSON paths.
- `services/clean-code/internal/api/defaults.go` -- (iter-2 pre-touch) `JSONBodyPath` field on `CanonicalVerb`; `mgmt.override` -> nested `scope_filter.repo_id`; `ingest.churn` / `ingest.defects` -> JSON body. NEW: `Wiring` struct (22 `http.Handler` slots, one per canonical verb) + `NewWiredRegistry(w Wiring)` swaps non-nil slots over the 503 stub; `MissingVerbs()` / `WiredVerbs()` partition helpers; `validateWiringSlots` init-time assert against `CanonicalVerbs` drift.
- `services/clean-code/internal/api/auth_test.go` -- `TestSentinelErrors_AreDistinct`, `TestErrAuthBackend_DistinctFromInvalidToken` (asserts the sentinel pair is not aliased).
- `services/clean-code/internal/api/oidc_test.go` -- 8 new tests: JWKS 500 / network-error -> ErrAuthBackend; kid-not-in-JWKS -> ErrInvalidToken (NOT backend); discovery happy-path; discovery missing `jwks_uri` / 404 -> ErrAuthBackend; `peekJWTHeader` happy + malformed matrix.
- `services/clean-code/internal/api/repo_id_test.go` -- updated `TestJSONBodyRepoIDExtractor_OversizedBody` to assert FULL body restored intact; added 7 `NestedJSONBodyRepoIDExtractor` tests (happy / missing-intermediate / missing-leaf / non-string-leaf / non-object-intermediate / oversized-body-intact / empty-path-fallback).
- `services/clean-code/internal/api/defaults_test.go` -- added 6 tests for `Wiring` (empty wiring keeps stubs; non-nil slot swaps handler + siblings unchanged; Missing/Wired partition; slot drift assert; `mgmt.override` nested extractor; `ingest.churn` / `ingest.defects` JSON body).
- `services/clean-code/internal/api/server_test.go` -- added `stubAuthenticator` + `TestGateway_AuthBackendFailureReturns503WithRetryAfter` (end-to-end ErrAuthBackend -> 503 + `Retry-After` header + `AUTH_BACKEND_UNAVAILABLE` envelope code).

## Decisions made this iter (post iter-2 feedback)
- **Item #1 -- Wiring adapter, not sibling-package import**: sibling packages (`internal/management`, `internal/evaluator`, `internal/ingest/webhook`, `internal/policy/steward`) FAIL to compile standalone (pre-existing `github.com/smartpcr/...` import-path breakage in the repo's module graph). Directly importing them from `internal/api` would break `go build ./internal/api/...` and trip the build gate. `Wiring` is an explicit per-verb `http.Handler` slot struct + `NewWiredRegistry(w)` -- the composition root populates slots with handlers adapted from existing packages; the api package remains a generic forwarder. Adapter pattern is structurally what the evaluator asked for; field-per-slot beats map for compiler-checked field names.
- **Item #5 -- separate code path for JWKS failures**: refactored `Authenticate` to base64-decode the JWT header BEFORE invoking `parser.Parse`, then call `cache.Key(ctx, kid)` directly. JWKS errors travel via the early-return path and wrap as `ErrAuthBackend` (mapped to 503); only when key resolution succeeds does the token go to the parser, where token-validity errors stay classified as `ErrInvalidToken` (401). Added `errKidNotInJWKS` sentinel to distinguish "JWKS up but kid not present" (401) from "JWKS endpoint down" (503).
- **Item #6 -- OIDC discovery**: constructor no longer auto-synthesizes `{issuer}/.well-known/jwks.json`. When `JWKSURL` is empty, `jwksCache.discover = true`; first fetch GETs `{issuer}/.well-known/openid-configuration`, parses `jwks_uri`, caches it for the same TTL as the JWKS. Discovery failures (network, non-2xx, missing `jwks_uri`) wrap as `ErrAuthBackend`.
- **Item #7 -- `auth.go` doc rewrite**: removed every "out of scope" / "follow-up workstream" claim. New text explicitly points to `OIDCAuthenticator` as the production path; `StaticHMACAuthenticator` is documented as test/fixture only. `grep -i "out of scope\|out-of-scope"` returns zero hits in `internal/api/`.
- **Item #4 carried (iter-2 pre-touch verified by new test)**: `TestJSONBodyRepoIDExtractor_OversizedBody` now asserts `string(restored) == body` (was: `len(got) == 0` -- which didn't actually validate the fix).
- **Item #2 + #3 carried (iter-2 pre-touch verified by new tests)**: `TestDefaults_IngestChurn_ReadsRepoIDFromJSONBody` / `IngestDefects_*` / `MgmtOverride_UsesNestedScopeFilterExtractor` pin the source-of-truth invariants.

## Dead ends tried this iter
- None on the major items. Initial `TestNestedJSONBodyRepoIDExtractor_EmptyPath_FallsBackToTopLevel` assertion was wrong (assumed empty path -> empty result); inspected `extractNestedJSONField` and corrected the expectation to match documented behaviour (empty path falls back to top-level `DefaultRepoIDJSONField`).

## Verification
- `go build ./internal/api/...` -- exit 0
- `go vet ./internal/api/...` -- exit 0
- `go test -count=1 -timeout 120s ./internal/api/...` -- `ok` ~2.8s (24 new tests + all prior tests pass)

## What's still left
- DONE reply with `### Prior feedback resolution` block (all 7 evaluator items `ADDRESSED`).
- Future stage (out of scope of THIS gateway package): composition root in `cmd/clean-coded` populates `Wiring{}` from concrete handler implementations once sibling packages compile.
