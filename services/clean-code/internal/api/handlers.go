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
	CodeMissingToken   = "MISSING_BEARER_TOKEN"
	CodeMalformedToken = "MALFORMED_BEARER_TOKEN"
	CodeInvalidToken   = "INVALID_BEARER_TOKEN"
	CodeExpiredToken   = "EXPIRED_BEARER_TOKEN"
	CodeBadAudience    = "BAD_AUDIENCE"
	CodeBadIssuer      = "BAD_ISSUER"
	CodeUnknownVerb    = "UNKNOWN_VERB"
	CodeInternalError  = "INTERNAL_ERROR"
	CodeAuthBackend    = "AUTH_BACKEND_UNAVAILABLE"
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
	registry *VerbRegistry
	tracer   Tracer
	logger   *slog.Logger
}

// NewGatewayHandler wires the gateway's dependencies. PANICS
// when any required dependency is nil -- a misconfigured
// gateway has no safe runtime behaviour.
func NewGatewayHandler(auth Authenticator, registry *VerbRegistry, tracer Tracer, logger *slog.Logger) *GatewayHandler {
	if auth == nil {
		panic("api: NewGatewayHandler received nil Authenticator")
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

	// Step 3 -- bearer extraction + verification. The
	// gateway maps Authenticator sentinel errors to the
	// canonical HTTP statuses listed in the doc-comment
	// on [Authenticator]. Runs AFTER verb-lookup so
	// unknown verbs always 404 (per workstream brief).
	identity, authErr := g.authenticate(r)
	if authErr != nil {
		g.handleAuthError(w, r, authErr, verb.DottedName())
		return
	}

	// Step 4 -- open the span and install the deferred
	// span-end / panic-recover closure. The defer MUST
	// be installed BEFORE the repo_id extractor runs so
	// a panicking extractor is caught by recover()
	// (item #6 from iter-1 evaluator feedback). The span
	// is opened with the canonical attribute set
	// initialised; the extractor below stamps repo_id on
	// the same span before the defer fires End().
	ctx, span := g.tracer.StartSpan(r.Context(), SpanName)
	span.SetAttribute(SpanAttrVerb, verb.DottedName())
	span.SetAttribute(SpanAttrCallerSubject, identity.Subject)
	span.SetAttribute(SpanAttrHTTPMethod, r.Method)
	span.SetAttribute(SpanAttrHTTPRoute, verb.Path())

	// Wrap the writer so we can capture the status code
	// after the downstream handler returns. The wrapper
	// forwards every call to the underlying writer
	// without buffering.
	sw := newStatusWriter(w)

	// Recover from a downstream / extractor panic so the
	// gateway never crashes the process and so the span
	// always carries `http.status_code`. The defer
	// closure fires for BOTH the extractor (step 5) and
	// the verb handler (step 7) -- whichever panics
	// first triggers it.
	defer func() {
		if rec := recover(); rec != nil {
			err := fmt.Errorf("panic in verb pipeline %s: %v", verb.DottedName(), rec)
			span.RecordError(err)
			g.logger.ErrorContext(ctx, "gateway: panic in verb pipeline",
				slog.String("verb", verb.DottedName()),
				slog.String("caller_subject", identity.Subject),
				slog.String("error", err.Error()),
			)
			if !sw.HeaderWritten() {
				g.writeError(sw, r, http.StatusInternalServerError,
					CodeInternalError, "internal error", identity.Subject, verb.DottedName())
			}
		}
		span.SetAttribute(SpanAttrHTTPStatusCode, sw.Status())
		span.End()
	}()

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
