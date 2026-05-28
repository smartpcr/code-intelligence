// Package api implements the HTTP+JSON gateway that fronts
// every verb defined by tech-spec Sec 8.5 row 19 / architecture
// Sec 6 ("HTTP/JSON gateway"). Stage 6.4 owns the gateway's
// composition: bearer-token authentication, verb routing,
// canonical OTel-shape span emission, and downstream
// delegation to the per-namespace verb handlers (which already
// live in `internal/management/`, `internal/evaluator/`,
// `internal/ingest/webhook/`, etc.).
//
// # Surface contract (tech-spec Sec 7, implementation-plan Stage 6.4)
//
//   - Every verb mounts at `/v1/{namespace}/{verb}` with the
//     verb name copied 1:1 into the path component.
//   - Caller MUST present `Authorization: Bearer <jwt>`. A
//     missing or empty header returns `401 Unauthorized` with
//     `WWW-Authenticate: Bearer` (RFC 6750 Sec 3 compliant).
//   - A token that fails signature / expiry / issuer
//     verification returns `401`.
//   - A token whose `aud` claim does not match the configured
//     audience returns `403 Forbidden` -- the caller is
//     authenticated but not authorised for THIS gateway.
//   - An authenticated request to a path whose `{namespace,
//     verb}` pair is not in the registry returns `404 Not
//     Found`. An UNAUTHENTICATED request to an unknown verb
//     ALSO returns `404` -- per workstream brief verbatim
//     ("refuse unknown verbs with 404"). The verb taxonomy is
//     public (architecture.md lists every verb), so leaking the
//     surface to anonymous probes is not a confidentiality
//     concern; per-verb policy enforcement still sits behind
//     auth.
//
// # Pipeline order (post-iter-1 refit)
//
// The gateway runs the following pipeline for every request,
// in this exact order, so that:
//
//   - Unknown verbs return 404 regardless of auth state
//     (item #4 from iter-1 evaluator feedback).
//   - A panicking RepoIDExtractor cannot escape the
//     panic-recover defer (item #6).
//
//  1. Parse path -> `{namespace, verb}`. Mismatched shape ->
//     404.
//  2. Look up the verb in the registry. Unknown -> 404 (no
//     WWW-Authenticate challenge).
//  3. Authenticate the bearer token. Missing -> 401, bad
//     signature / issuer / expiry -> 401, bad audience -> 403.
//  4. Open OTel span; install panic-recover defer (the defer
//     ALSO closes the span and stamps the final HTTP status).
//  5. Run the verb's RepoIDExtractor (panic now recovered);
//     stamp `repo_id` on the span.
//  6. Rewrite `X-OIDC-Subject` to the verified subject and
//     forward to the verb handler.
//
// # OIDC subject propagation
//
// On successful authentication, the gateway re-writes the
// outgoing `X-OIDC-Subject` header to the token's `sub` claim
// (always overwriting any caller-supplied value, so a sloppy
// reverse-proxy upstream cannot spoof attribution). The
// downstream verb handlers (`internal/management/mgmt_verbs.go`,
// `policy_verbs.go`, `register_repo_verb.go`, etc.) already
// consume `X-OIDC-Subject` and 401 if absent -- the gateway is
// the authoritative writer of that header.
//
// # Authenticators
//
// Two production-ready [Authenticator] implementations ship in
// this package:
//
//   - [StaticHMACAuthenticator] -- HS256-signed JWTs against a
//     static shared secret. Intended for test fixtures and
//     internal-only deployments where a JWKS endpoint is
//     overkill.
//   - [OIDCAuthenticator] -- RS256/RS384/RS512/ES256/ES384
//     JWTs verified against an OIDC IdP's JWKS endpoint.
//     This is the production gateway authenticator: it
//     implements the full RFC 7519 / OIDC core verification
//     pipeline (alg=none rejected, kid-based key lookup with
//     JWKS cache + rotation refresh, exact iss / aud match,
//     exp / nbf with leeway).
//
// # Canonical verb registry
//
// [NewDefaultRegistry] returns a [VerbRegistry] pre-populated
// with every verb the architecture pins (Sec 6.2-6.5):
// `eval.gate`, `mgmt.{read.*,register_repo,set_mode,
// retract_sample,rescan,override}`, `ingest.{coverage,
// test_balance,churn,defects}`, and `policy.{publish,activate,
// publish_rulepack,keys.list_active}`. Each entry mounts with
// a [notWiredHandler] stub (503 VERB_NOT_WIRED) until the
// composition root calls [VerbRegistry.Replace] to swap in
// the production handler. The result: probing any
// architecture-defined verb returns 503 (subsystem not wired)
// instead of 404 (verb does not exist), and the "every verb
// exposed" workstream invariant is preserved by construction.
//
// # OTel span emission
//
// Each request opens one span tagged with:
//
//   - `verb` -- canonical `{namespace}.{name}` dotted form
//     (e.g. `mgmt.register_repo`, `eval.gate`).
//   - `caller_subject` -- the verified `sub` claim.
//   - `repo_id` -- extracted via the optional per-verb
//     [Verb.RepoIDExtractor]; empty string when no extractor is
//     registered or no repo_id is present.
//   - `http.status_code` -- the downstream response status.
//
// The [Tracer] interface is intentionally OTel-shaped (StartSpan
// returning a context + Span, SetAttribute kv-pairs); the
// production [OTelTracer] wraps a real
// `go.opentelemetry.io/otel/trace.Tracer` so spans land in
// whatever exporter the SDK was configured with (OTLP,
// Jaeger, Zipkin, ...). [SlogTracer] remains available for
// in-process dev paths where no collector runs, and
// [NoopTracer] keeps the test surface fast.
package api
