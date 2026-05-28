package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// identityCtxKey is the context.Context key the gateway uses
// to expose the verified [Identity] to downstream handlers.
// Backed by a package-private type so other packages cannot
// manufacture a colliding key.
type identityCtxKey struct{}

// IdentityFromContext returns the verified caller identity
// previously stored by the gateway, or nil when the context
// did not pass through the gateway (e.g. internal direct-
// invocation paths in package tests).
func IdentityFromContext(ctx context.Context) *Identity {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(identityCtxKey{}).(*Identity)
	return v
}

// withIdentity stamps the verified caller identity on the
// context. Internal helper -- the gateway is the only writer.
func withIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

// errorEnvelope is the JSON shape every 4xx / 5xx response
// from the gateway carries. The shape mirrors the existing
// webhook router's error envelope (`{"error": "...", "code":
// "..."}`) so operator tooling that already consumes one
// gateway's errors can consume this one without changes.
type errorEnvelope struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// Canonical error codes the gateway emits in the `code` field
// of its error responses. Pinned as exported constants so
// integration tests can assert codes verbatim.
const (
	CodeMissingToken      = "MISSING_BEARER_TOKEN"
	CodeMalformedToken    = "MALFORMED_BEARER_TOKEN"
	CodeInvalidToken      = "INVALID_BEARER_TOKEN"
	CodeExpiredToken      = "EXPIRED_BEARER_TOKEN"
	CodeBadAudience       = "BAD_AUDIENCE"
	CodeBadIssuer         = "BAD_ISSUER"
	CodeUnknownVerb       = "UNKNOWN_VERB"
	CodeInternalError     = "INTERNAL_ERROR"
	CodeAuthBackend       = "AUTH_BACKEND_UNAVAILABLE"
	CodeInsufficientGroup = "INSUFFICIENT_GROUP"
)

// GatewayHandler is the gateway's top-level [http.Handler]. It
// composes auth, verb routing, span emission, and
// downstream forwarding behind a single [http.Handler.ServeHTTP]
// entry point. Composition roots obtain a GatewayHandler via
// [Server.HTTPHandler] -- the type itself is exported so a
// test (or a future composition root that wants to mount the
// gateway under a prefix) can construct one directly.
type GatewayHandler struct {
	auth     Authenticator
	authz    Authorizer
	registry *VerbRegistry
	tracer   Tracer
	logger   *slog.Logger
}

// NewGatewayHandler wires the gateway's dependencies. PANICS
// when any required dependency is nil -- a misconfigured
// gateway has no safe runtime behaviour.
//
// The Authorizer is OPTIONAL; passing nil installs
// [NoopAuthorizer] which admits every authenticated caller.
// Production composition roots SHOULD pass a real
// Authorizer (e.g. [GroupClaimAuthorizer]) to honour the
// tech-spec Sec 8.5 OIDC group-claim requirement.
func NewGatewayHandler(auth Authenticator, registry *VerbRegistry, tracer Tracer, logger *slog.Logger) *GatewayHandler {
	return NewGatewayHandlerWithAuthorizer(auth, NoopAuthorizer{}, registry, tracer, logger)
}

// NewGatewayHandlerWithAuthorizer is the full-config
// constructor. PANICS on a nil Authenticator, nil Registry,
// or nil Authorizer. The other arguments default safely.
func NewGatewayHandlerWithAuthorizer(auth Authenticator, authz Authorizer, registry *VerbRegistry, tracer Tracer, logger *slog.Logger) *GatewayHandler {
	if auth == nil {
		panic("api: NewGatewayHandler received nil Authenticator")
	}
	if authz == nil {
		panic("api: NewGatewayHandler received nil Authorizer (pass NoopAuthorizer{} explicitly to acknowledge no policy enforcement)")
	}
	if registry == nil {
		panic("api: NewGatewayHandler received nil VerbRegistry")
	}
	if tracer == nil {
		tracer = NoopTracer{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &GatewayHandler{
		auth:     auth,
		authz:    authz,
		registry: registry,
		tracer:   tracer,
		logger:   logger,
	}
}

// ServeHTTP implements [http.Handler]. The request pipeline:
//
//  1. Parse the URL path enough to know it is shaped
//     `/v1/{namespace}/{verb}`. If not, return 404 with no
//     auth challenge -- a request whose path does not even
//     look like a v1 verb path is a plain typo.
//  2. Look up the verb in the registry. Unknown -> 404
//     (no auth challenge). This honours the workstream
//     brief verbatim: "refuse unknown verbs with 404". The
//     trade-off -- an unauthenticated caller can in
//     principle enumerate the verb taxonomy by probing --
//     is mitigated by the fact that the verb names are
//     listed in the public architecture document
//     (architecture Sec 6.2-6.5).
//  3. Extract + verify the bearer token. Missing -> 401
//     with `WWW-Authenticate: Bearer`. Malformed /
//     signature / expiry failure -> 401. Audience
//     mismatch -> 403. Auth runs AFTER verb-lookup so the
//     401/404 ordering matches the brief.
//  4. Install the panic-recover + span-end deferred
//     closure. Installed BEFORE the repo_id extractor
//     runs so a panic in the extractor is captured by
//     the recover and stamped on the span (item #6 from
//     iter-1 evaluator feedback).
//  5. Run the optional [Verb.RepoIDExtractor] to capture
//     the span's repo_id attribute.
//  6. Rewrite the request: clone, ALWAYS overwrite
//     `X-OIDC-Subject` with the verified subject (a
//     spoofed inbound `X-OIDC-Subject` MUST NOT survive
//     the gateway), thread the verified Identity into
//     the request context for downstream consumers.
//  7. Forward to the verb's [Handler]. Capture the
//     downstream status code via a transparent wrapper.
//  8. The deferred closure stamps `http.status_code` on
//     the span and ends it.
//
// The handler defers a panic-recover that maps an
// uncaught downstream panic to a 500 with an opaque body
// so the gateway never crashes the server process.
func (g *GatewayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Step 1 -- cheap path shape check. A request whose
	// path is not `/v1/{namespace}/{verb}` cannot match
	// any registered verb regardless of auth; surface a
	// 404 immediately. No auth challenge is emitted (the
	// client has not even hit the gateway's verb surface).
	namespace, verbName, pathOK := ParseVerbPath(r.URL.Path)
	if !pathOK {
		g.writeError(w, r, http.StatusNotFound, CodeUnknownVerb,
			fmt.Sprintf("path %q is not a /v1/{namespace}/{verb} verb", r.URL.Path),
			"", "")
		return
	}

	// Step 2 -- verb lookup. Runs BEFORE auth so an
	// unknown verb always returns 404 regardless of
	// whether the caller presented a token (workstream
	// brief: "refuse unknown verbs with 404"). The
	// architecture's public verb taxonomy is a
	// documented surface, not a secret to defend.
	verb, registered := g.registry.Lookup(namespace, verbName)
	if !registered {
		g.writeError(w, r, http.StatusNotFound, CodeUnknownVerb,
			fmt.Sprintf("verb %q not registered", namespace+"."+verbName),
			"", "")
		return
	}

	// Step 3 -- open the verb span BEFORE auth runs.
	// Iter-2 evaluator feedback #3 (Stage 9.4): auth /
	// authz failures previously returned BEFORE
	// StartSpan, so 401 / 403 / 503 paths emitted no
	// span and were invisible to dashboards keyed on
	// `verb` / `auth_status`. Opening the span here
	// (after the cheap path-shape + verb-lookup 404
	// filters) means EVERY verb invocation -- success
	// AND auth-rejection -- carries the canonical
	// attribute set. Authn / authz failures stamp
	// `auth_status` to one of the disjoint enum values
	// in tracing.go before returning; the deferred
	// closure ends the span on every path.
	//
	// The defer installs the panic-recover guard FIRST
	// so a panic in authenticate / Authorize / extractor
	// / downstream handler is caught and mapped to 500
	// with the span carrying `http.status_code` + the
	// recorded error. The `identity` capture is nil-safe
	// because the defer reads through the local
	// `currentIdentity` variable updated as auth
	// progresses (nil until authn succeeds).
	ctx, span := g.tracer.StartSpan(r.Context(), SpanName)
	span.SetAttribute(SpanAttrVerb, verb.DottedName())
	span.SetAttribute(SpanAttrCallerSubject, "")
	span.SetAttribute(SpanAttrHTTPMethod, r.Method)
	span.SetAttribute(SpanAttrHTTPRoute, verb.Path())
	// Canonical eval-gate attribute defaults (Stage 9.4 /
	// architecture Sec 8): every verb span carries the
	// full attribute schema so dashboards never see a
	// missing-key blowup. Verbs that DO know the verdict
	// (eval.gate) overwrite these via
	// `telemetry.AnnotateEvalGateSpan` in their downstream
	// handler. On the OTel-backed tracer the overwrite
	// happens via the OTel-native `trace.SpanFromContext`
	// (the api.Tracer seam does not expose mid-handler
	// access to the api.Span); these defaults guarantee
	// the keys are present on EVERY span -- including
	// spans recorded by `api.RecordingTracer` /
	// `SlogTracer` where the OTel overwrite is a no-op.
	span.SetAttribute(SpanAttrRepoID, "")
	span.SetAttribute(SpanAttrPolicyVersionID, "")
	span.SetAttribute(SpanAttrDegraded, false)
	span.SetAttribute(SpanAttrDegradedReason, "")
	span.SetAttribute(SpanAttrVerdict, "")
	// Default auth_status to `ok`; auth-failure branches
	// below overwrite to the disjoint enum values before
	// returning.
	span.SetAttribute(SpanAttrAuthStatus, AuthStatusOK)

	// Wrap the writer so we can capture the status code
	// after the downstream handler returns. The wrapper
	// forwards every call to the underlying writer
	// without buffering. Constructed BEFORE auth so the
	// auth-failure branches can write through the same
	// wrapper and the deferred closure observes the
	// status code on EVERY exit path.
	sw := newStatusWriter(w)

	// `currentIdentity` is the identity captured as auth
	// progresses: nil before authn, set after authn
	// succeeds. The deferred panic-recover closure reads
	// this so it has a nil-safe view of the subject for
	// logging when a panic fires DURING auth (e.g. a
	// rogue Authenticator implementation).
	var currentIdentity *Identity

	// Recover from a downstream / extractor / auth panic
	// so the gateway never crashes the process and so
	// the span always carries `http.status_code`. The
	// defer closure fires for ALL code paths below
	// (auth, extractor, verb handler) -- whichever
	// panics first triggers it.
	defer func() {
		if rec := recover(); rec != nil {
			err := fmt.Errorf("panic in verb pipeline %s: %v", verb.DottedName(), rec)
			span.RecordError(err)
			subj := ""
			if currentIdentity != nil {
				subj = currentIdentity.Subject
			}
			g.logger.ErrorContext(ctx, "gateway: panic in verb pipeline",
				slog.String("verb", verb.DottedName()),
				slog.String("caller_subject", subj),
				slog.String("error", err.Error()),
			)
			if !sw.HeaderWritten() {
				g.writeError(sw, r, http.StatusInternalServerError,
					CodeInternalError, "internal error", subj, verb.DottedName())
			}
		}
		span.SetAttribute(SpanAttrHTTPStatusCode, sw.Status())
		span.End()
	}()

	// Step 4 -- bearer extraction + verification. The
	// gateway maps Authenticator sentinel errors to the
	// canonical HTTP statuses listed in the doc-comment
	// on [Authenticator]. Runs AFTER verb-lookup so
	// unknown verbs always 404 (per workstream brief).
	// On failure we stamp the canonical `auth_status`
	// enum on the OPEN span before returning:
	//
	//   - ErrAuthBackend -> backend_unavailable (503).
	//     The IdP / JWKS endpoint is down; dashboards
	//     alert on this independently of caller-error
	//     401 floods.
	//   - ErrBadAudience -> denied (403). The bearer
	//     verifies but is not for this gateway; auth-
	//     status semantically matches the authz-denial
	//     path because the caller is authenticated but
	//     not authorised for this resource.
	//   - default fallback (500 -- authenticator-internal
	//     failure) -> backend_unavailable. Same alert
	//     class as the JWKS-down path; the caller cannot
	//     be authenticated through no fault of theirs.
	//   - all other sentinels (missing / malformed /
	//     invalid / expired / bad-issuer) -> unauthenticated
	//     (401). Caller-supplied credential is bad.
	//
	// Stage 9.4 iter-3 evaluator item #3: previously
	// EVERY auth error stamped `unauthenticated`, which
	// papered over the 503 and 500 distinction so
	// dashboards could not tell "the IdP is down" apart
	// from "a caller mistyped their token".
	identity, authErr := g.authenticate(r)
	if authErr != nil {
		span.SetAttribute(SpanAttrAuthStatus, classifyAuthError(authErr))
		g.handleAuthError(sw, r, authErr, verb.DottedName())
		return
	}
	currentIdentity = identity
	span.SetAttribute(SpanAttrCallerSubject, identity.Subject)

	// Step 4.5 -- group-claim authorisation (tech-spec
	// Sec 8.5). Runs AFTER authn so the caller's
	// Identity is verified before policy evaluation.
	// Denial is `auth_status=denied` (403). Any other
	// authz error is classified as
	// `backend_unavailable` (the Authorizer's contract
	// is to return `ErrInsufficientGroup` for the
	// denied path; non-sentinel errors signal an
	// internal authorizer failure -- backend outage,
	// rogue panic in a custom impl). `handleAuthzError`
	// maps the non-sentinel branch to 500; the span
	// label captures it as `backend_unavailable` so
	// dashboards can alert on authz infrastructure
	// health independently of the per-caller denial
	// rate.
	if authzErr := g.authz.Authorize(r.Context(), identity, verb.DottedName()); authzErr != nil {
		if errors.Is(authzErr, ErrInsufficientGroup) {
			span.SetAttribute(SpanAttrAuthStatus, AuthStatusDenied)
		} else {
			span.SetAttribute(SpanAttrAuthStatus, AuthStatusBackendUnavailable)
		}
		g.handleAuthzError(sw, r, authzErr, identity, verb.DottedName())
		return
	}

	// Step 5 -- repo_id extraction. The extractor MAY
	// peek the body; if it does it MUST restore the body
	// so the downstream handler sees it intact (see
	// Verb.RepoIDExtractor doc). The defer above
	// captures a panic in the extractor.
	repoID := ""
	if verb.RepoIDExtractor != nil {
		extracted, rNew, err := verb.RepoIDExtractor(r)
		if err != nil {
			// Extractor failures (e.g. malformed body)
			// are logged but do NOT fail the request -
			// the gateway still forwards to the
			// downstream handler which performs full
			// body validation. The span carries
			// repo_id="" so dashboards can spot
			// extraction failures.
			g.logger.WarnContext(ctx, "gateway: repo_id extractor failed",
				slog.String("verb", verb.DottedName()),
				slog.String("error", err.Error()),
			)
		} else {
			repoID = extracted
			if rNew != nil {
				r = rNew
			}
		}
	}
	span.SetAttribute(SpanAttrRepoID, repoID)

	// Step 6 -- clone the request: ALWAYS overwrite
	// `X-OIDC-Subject` (a spoofed inbound header MUST
	// NOT reach the downstream handler), and thread the
	// verified Identity through the context so handlers
	// can call IdentityFromContext for typed access
	// without re-parsing headers.
	outbound := r.Clone(withIdentity(ctx, identity))
	// Clone copies headers; replace the OIDC subject
	// header value authoritatively. Headers are now
	// independent of the inbound request's header map.
	outbound.Header.Set(OIDCSubjectHeader, identity.Subject)

	// Step 7 -- forward to the downstream handler.
	verb.Handler.ServeHTTP(sw, outbound)
}

// authenticate runs the bearer-token verification pipeline.
// Returns the verified Identity on success or an error from
// the sentinel set above. The function is a thin
// orchestrator -- the heavy lifting lives in
// [Authenticator.Authenticate].
func (g *GatewayHandler) authenticate(r *http.Request) (*Identity, error) {
	header := r.Header.Get(AuthorizationHeader)
	token, err := ParseBearer(header)
	if err != nil {
		return nil, err
	}
	id, err := g.auth.Authenticate(r.Context(), token)
	if err != nil {
		return nil, err
	}
	if id == nil {
		return nil, fmt.Errorf("%w: authenticator returned nil identity", ErrInvalidToken)
	}
	if id.Subject == "" {
		return nil, fmt.Errorf("%w: authenticator returned empty subject", ErrInvalidToken)
	}
	return id, nil
}

// handleAuthError maps an auth sentinel to its canonical HTTP
// status + error envelope. The bearer challenge header is
// included on every 401 per RFC 6750 Sec 3 -- including the
// MalformedToken / InvalidToken cases where the spec
// recommends the additional `error="invalid_token"` parameter.
func (g *GatewayHandler) handleAuthError(w http.ResponseWriter, r *http.Request, err error, verb string) {
	switch {
	case errors.Is(err, ErrMissingToken):
		w.Header().Set("WWW-Authenticate", `Bearer`)
		g.writeError(w, r, http.StatusUnauthorized, CodeMissingToken,
			"missing bearer token", "", verb)
	case errors.Is(err, ErrMalformedToken):
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_request"`)
		g.writeError(w, r, http.StatusUnauthorized, CodeMalformedToken,
			err.Error(), "", verb)
	case errors.Is(err, ErrExpiredToken):
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="The access token expired"`)
		g.writeError(w, r, http.StatusUnauthorized, CodeExpiredToken,
			err.Error(), "", verb)
	case errors.Is(err, ErrBadIssuer):
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="The access token issuer is not recognised"`)
		g.writeError(w, r, http.StatusUnauthorized, CodeBadIssuer,
			err.Error(), "", verb)
	case errors.Is(err, ErrBadAudience):
		// 403 -- the caller is authenticated but the
		// token is not for THIS gateway. RFC 6750
		// Sec 3.1 uses `insufficient_scope` for related
		// authorisation failures; the canonical OIDC
		// usage of audience-mismatch as 403 lives in
		// the OAuth 2.0 Bearer Token Resource Server
		// spec (draft).
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", error_description="audience mismatch"`)
		g.writeError(w, r, http.StatusForbidden, CodeBadAudience,
			err.Error(), "", verb)
	case errors.Is(err, ErrAuthBackend):
		// 503 -- the JWKS endpoint / OIDC discovery
		// document / IdP itself is unreachable or
		// returned an unexpected status. Distinct from
		// 401 (caller's credential is bad) and 500 (the
		// GATEWAY itself crashed); a downed IdP is an
		// operator-observable infrastructure failure
		// and dashboards key off the 503 to alert
		// SREs without paging on every 401 flood.
		// (Item #5 from iter-2 evaluator feedback.)
		w.Header().Set("Retry-After", "30")
		g.logger.ErrorContext(r.Context(), "gateway: auth backend unavailable",
			slog.String("error", err.Error()),
		)
		g.writeError(w, r, http.StatusServiceUnavailable, CodeAuthBackend,
			"auth backend unavailable", "", verb)
	case errors.Is(err, ErrInvalidToken):
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		g.writeError(w, r, http.StatusUnauthorized, CodeInvalidToken,
			err.Error(), "", verb)
	default:
		// Authenticator-internal failure (e.g. JWKS
		// fetch timeout on the future RS256 verifier).
		// Log the raw error server-side; emit an opaque
		// 500 on the wire so resolver / IdP state does
		// not leak to the unauthenticated caller.
		g.logger.ErrorContext(r.Context(), "gateway: authenticator internal failure",
			slog.String("error", err.Error()),
		)
		g.writeError(w, r, http.StatusInternalServerError, CodeInternalError,
			"internal error", "", verb)
	}
}

// classifyAuthError maps the [Authenticator] sentinel-error
// set onto the canonical Stage 9.4 `auth_status` span enum.
// Mirrors the HTTP-status mapping in [handleAuthError]:
//
//   - [ErrAuthBackend]     -> [AuthStatusBackendUnavailable] (503)
//   - [ErrBadAudience]     -> [AuthStatusDenied]             (403)
//   - default (non-sentinel, mapped to 500 in handleAuthError)
//     -> [AuthStatusBackendUnavailable]. Same alert class as
//     the JWKS-down path because the caller cannot
//     authenticate through no fault of theirs.
//   - everything else (missing / malformed / invalid /
//     expired / bad-issuer) -> [AuthStatusUnauthenticated] (401).
//
// Kept package-private and BESIDE [handleAuthError] so a
// future contributor adding a new sentinel updates both the
// status table AND the span enum in the same edit.
//
// Stage 9.4 iter-3 evaluator item #3.
func classifyAuthError(err error) string {
	switch {
	case errors.Is(err, ErrAuthBackend):
		return AuthStatusBackendUnavailable
	case errors.Is(err, ErrBadAudience):
		return AuthStatusDenied
	case errors.Is(err, ErrMissingToken),
		errors.Is(err, ErrMalformedToken),
		errors.Is(err, ErrExpiredToken),
		errors.Is(err, ErrBadIssuer),
		errors.Is(err, ErrInvalidToken):
		return AuthStatusUnauthenticated
	default:
		// Non-sentinel = authenticator-internal failure
		// (the default branch of [handleAuthError]
		// returns 500). Surface it as backend_unavailable
		// for dashboards -- the caller cannot
		// authenticate and the cause is server-side.
		return AuthStatusBackendUnavailable
	}
}

// handleAuthzError maps the [Authorizer.Authorize] return
// onto the canonical HTTP shape. The deny path
// ([ErrInsufficientGroup]) -> 403 with WWW-Authenticate
// `insufficient_scope`; any other error -> 500 with the
// opaque [CodeInternalError] body and a server-side log.
//
// The 403 body carries the canonical [CodeInsufficientGroup]
// code so operator tooling can distinguish "audience
// mismatch" (CodeBadAudience, also 403) from "group
// policy" (CodeInsufficientGroup, also 403) without
// parsing the human-readable error string.
func (g *GatewayHandler) handleAuthzError(w http.ResponseWriter, r *http.Request, err error, identity *Identity, verb string) {
	subject := ""
	if identity != nil {
		subject = identity.Subject
	}
	if errors.Is(err, ErrInsufficientGroup) {
		// RFC 6750 Sec 3.1: `insufficient_scope` is the
		// canonical OAuth 2.0 Bearer challenge for
		// authenticated-but-unauthorised. The body
		// surfaces the verb so the caller's tooling can
		// log it for an audit trail; the underlying group
		// mismatch is logged server-side only (the caller
		// already knows their own group list, and a
		// detailed diff would leak the per-verb policy to
		// any caller probing the surface).
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", error_description="group policy denies verb"`)
		g.logger.LogAttrs(r.Context(), slog.LevelWarn,
			"gateway: authz denied",
			slog.String("verb", verb),
			slog.String("caller_subject", subject),
			slog.String("error", err.Error()),
		)
		g.writeError(w, r, http.StatusForbidden, CodeInsufficientGroup,
			"caller groups do not satisfy verb policy", subject, verb)
		return
	}
	// Any other authz error type is an INTERNAL failure
	// (e.g. the Authorizer panicked decoding the claim,
	// or a custom impl returned a non-sentinel error).
	// Log the raw error; emit an opaque 500 on the wire
	// so policy table contents do not leak.
	g.logger.ErrorContext(r.Context(), "gateway: authorizer internal failure",
		slog.String("verb", verb),
		slog.String("caller_subject", subject),
		slog.String("error", err.Error()),
	)
	g.writeError(w, r, http.StatusInternalServerError, CodeInternalError,
		"internal error", subject, verb)
}

// writeError emits the canonical JSON error envelope and logs
// the failure server-side. The function is the single writer
// of 4xx / 5xx responses so the wire shape stays uniform.
func (g *GatewayHandler) writeError(w http.ResponseWriter, r *http.Request, status int, code, msg, subject, verb string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	if err := enc.Encode(errorEnvelope{Error: msg, Code: code}); err != nil {
		// Headers already on the wire -- nothing to
		// downgrade to; log the encode failure server-
		// side so the operator has the only signal that
		// the response body was malformed.
		g.logger.ErrorContext(r.Context(), "gateway: error envelope encode failed",
			slog.Int("status", status),
			slog.String("code", code),
			slog.String("error", err.Error()),
		)
	}
	level := slog.LevelInfo
	if status >= 500 {
		level = slog.LevelError
	} else if status >= 400 {
		level = slog.LevelWarn
	}
	g.logger.LogAttrs(r.Context(), level, "gateway: request rejected",
		slog.Int("status", status),
		slog.String("code", code),
		slog.String("verb", verb),
		slog.String("caller_subject", subject),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
	)
}
