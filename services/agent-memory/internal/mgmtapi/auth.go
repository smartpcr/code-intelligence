package mgmtapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// AuthorizationHeader is the HTTP header the handler reads the
// bearer token from. Pulled out as a constant so the verifier,
// the middleware, and the test helpers reference the same
// symbol.
const AuthorizationHeader = "Authorization"

// bearerPrefix is the literal that every valid `Authorization`
// header value carries before the raw OIDC token. RFC 6750 §2.1
// pins the scheme name as case-insensitive; we accept any case
// by lower-casing the prefix during parsing.
const bearerPrefix = "Bearer "

// ErrTokenMissing is returned by [TokenVerifier.Verify] when the
// caller did not present a token at all. The middleware
// translates this into 401 + `WWW-Authenticate: Bearer
// realm="agent-memory mgmt-api"` (RFC 6750 §3).
var ErrTokenMissing = errors.New("mgmtapi: bearer token missing")

// ErrTokenInvalid is returned by [TokenVerifier.Verify] when
// the caller's token failed cryptographic / structural
// validation (wrong signature, expired, wrong audience, ...).
// The middleware translates this into 401 + an explanatory
// `error="invalid_token"` field in the `WWW-Authenticate`
// header (RFC 6750 §3.1).
var ErrTokenInvalid = errors.New("mgmtapi: bearer token invalid")

// TokenVerifier validates an OIDC bearer token. The verifier
// is the seam between the wire-format auth machinery in this
// package and the chosen OIDC implementation. Production
// composition roots plug in a JWKS-backed verifier; tests use
// a fake.
//
// Verify returns the authenticated subject identifier on
// success. The subject is opaque to this package — the
// caller's only invariant is that two calls with the same
// valid token return the same subject value, so audit log
// records and feedback writes (Stage 7.3) can attribute the
// row to a stable operator identity.
//
// On failure, Verify MUST return [ErrTokenMissing] for an
// absent token and [ErrTokenInvalid] for any structural /
// signature / expiry / claim failure. Any other error value
// is treated as an infrastructure failure (503 response,
// [ErrVerifierUnavailable] semantics) — implementations should
// reserve those for genuinely-transient backends such as a
// JWKS endpoint outage.
type TokenVerifier interface {
	Verify(ctx context.Context, rawToken string) (subject string, err error)
}

// ErrVerifierUnavailable is the sentinel verifiers MAY return
// when their backing IdP / JWKS endpoint is unreachable. The
// middleware translates this into 503 Service Unavailable, NOT
// 401 — an outage is NOT an authentication failure and must
// not surface to the caller as "your token is bad".
var ErrVerifierUnavailable = errors.New("mgmtapi: token verifier upstream unavailable")

// StaticBearerVerifier compares the presented token against a
// fixed shared secret using a constant-time byte compare.
//
// !! DEV / LOCAL-STACK ONLY !!
//
// This verifier is the absolute minimum the binary needs to
// gate writes during local development and integration tests
// — it does NOT validate issuer, audience, expiry, signature,
// or any other OIDC claim. Production deployments use
// [OIDCVerifier] (oidc.go) which performs full JWKS-backed
// RS256/384/512 verification.
//
// Composition root selection (cmd/mgmt-api/main.go):
//
//   - If AGENT_MEMORY_OIDC_ISSUER + _AUDIENCE + _JWKS_URL are
//     all set, the binary wires [OIDCVerifier] and this
//     verifier is NOT instantiated.
//   - Otherwise, if AGENT_MEMORY_OIDC_DEV_TOKEN is set, the
//     binary wires this verifier and logs a WARN line on boot
//     announcing the dev posture.
//   - If neither is set, the binary refuses to start (exit 2).
type StaticBearerVerifier struct {
	// Secret is the shared bearer token the verifier accepts.
	// MUST be non-empty; an empty Secret rejects every token
	// so a misconfigured deployment fails closed.
	Secret string
	// Subject is the opaque operator id returned on a
	// successful match. Defaults to `dev-operator` if empty.
	Subject string
}

// Verify implements [TokenVerifier].
func (v *StaticBearerVerifier) Verify(_ context.Context, raw string) (string, error) {
	if raw == "" {
		return "", ErrTokenMissing
	}
	if v.Secret == "" {
		// Fail closed when the dev verifier is misconfigured.
		// An empty configured secret could otherwise be
		// matched by an empty raw token if we naively did a
		// `==` compare; subtle.ConstantTimeCompare handles
		// the equal-length case but the length-mismatch
		// branch must reject explicitly.
		return "", ErrTokenInvalid
	}
	a := []byte(raw)
	b := []byte(v.Secret)
	if len(a) != len(b) {
		// subtle.ConstantTimeCompare returns 0 on
		// length-mismatch but ALSO short-circuits — i.e. the
		// timing leaks the length. For a dev verifier this
		// is acceptable, but rejecting up-front keeps the
		// reading clearer for future code review.
		return "", ErrTokenInvalid
	}
	if subtle.ConstantTimeCompare(a, b) != 1 {
		return "", ErrTokenInvalid
	}
	subj := v.Subject
	if subj == "" {
		subj = "dev-operator"
	}
	return subj, nil
}

// subjectContextKey is the unexported context key the
// middleware stamps the authenticated subject on. Downstream
// handlers retrieve it via [SubjectFromContext]. Using a
// typed key (rather than a string) prevents accidental
// collision with any other package's context values.
type subjectContextKey struct{}

// SubjectFromContext returns the subject the auth middleware
// stamped on `ctx`, plus an `ok` flag that is false when no
// subject is present (e.g. the middleware was bypassed in a
// test). Used by handlers that want to log the operator who
// triggered a write.
func SubjectFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(subjectContextKey{}).(string)
	return v, ok && v != ""
}

// withSubject returns a copy of ctx with `subject` stamped on
// it. Internal-only; production callers go through the
// middleware.
func withSubject(ctx context.Context, subject string) context.Context {
	return context.WithValue(ctx, subjectContextKey{}, subject)
}

// authMiddleware wraps `next` so every request that reaches
// `next` has been authenticated. Failures terminate the
// request BEFORE next.ServeHTTP runs and BEFORE the body is
// read, so the Stage 7.1 invariant "no row is written on
// auth failure" holds mechanically.
//
// The middleware does NOT log the raw bearer token (or even
// any prefix of it). It DOES log the failure reason, the
// remote addr, and the URL path so an operator can correlate
// 401 spikes with their source.
//
// Header parsing is permissive on the `Bearer ` literal case
// (per RFC 6750 §2.1 the scheme name is case-insensitive) but
// strict on the rest of the shape: a header that does not
// start with `Bearer ` (case-folded) is treated as
// [ErrTokenMissing] — the call never reaches the verifier.
func authMiddleware(v TokenVerifier, logger *slog.Logger, next http.Handler) http.Handler {
	if v == nil {
		// Programmer error: a nil verifier would no-op the
		// auth gate. Panic at composition time so a
		// misconfigured binary fails to start, instead of
		// silently serving every write to anyone.
		panic("mgmtapi: authMiddleware: nil TokenVerifier")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := extractBearer(r.Header.Get(AuthorizationHeader))
		if !ok {
			respondUnauthorized(w, "", `error="invalid_request"`)
			logger.Warn("mgmtapi.auth.missing",
				slog.String("op", "auth"),
				slog.String("path", r.URL.Path),
				slog.String("remote", r.RemoteAddr),
			)
			return
		}
		subject, err := v.Verify(r.Context(), raw)
		if err != nil {
			switch {
			case errors.Is(err, ErrTokenMissing):
				respondUnauthorized(w, "", `error="invalid_request"`)
			case errors.Is(err, ErrVerifierUnavailable):
				// Set WWW-Authenticate so RFC 6750 clients
				// (and the operator's tracing layer) can
				// still attribute the 503 to the auth tier,
				// then emit the JSON envelope used by the
				// rest of the Management API.
				w.Header().Set("WWW-Authenticate",
					`Bearer realm="agent-memory mgmt-api", error="temporarily_unavailable"`)
				writeJSONError(w, http.StatusServiceUnavailable,
					"auth_verifier_unavailable",
					"token verifier is unavailable; retry shortly")
			default:
				// Treat anything else — including
				// ErrTokenInvalid and verifier-specific
				// errors — as an invalid token. The
				// `error="invalid_token"` parameter in
				// the WWW-Authenticate header is the
				// RFC 6750 §3.1 standard signal.
				respondUnauthorized(w, "", `error="invalid_token"`)
			}
			logger.Warn("mgmtapi.auth.rejected",
				slog.String("op", "auth"),
				slog.String("path", r.URL.Path),
				slog.String("remote", r.RemoteAddr),
				slog.String("error", err.Error()),
			)
			return
		}
		// Subject stays in the request context so downstream
		// handlers can attribute writes without re-parsing
		// the token. No subject is ever placed in a response
		// header.
		next.ServeHTTP(w, r.WithContext(withSubject(r.Context(), subject)))
	})
}

// extractBearer parses an `Authorization: Bearer …` header
// value and returns the raw token text. Returns ok=false when
// the header does not match the bearer shape; callers route
// that to the same 401 path as a verifier rejection.
//
// Tolerant on:
//
//   - scheme name case (`Bearer`, `bearer`, `BEARER`).
//   - leading / trailing whitespace.
//
// Strict on:
//
//   - a single token after the scheme; embedded whitespace in
//     the token text rejects (a bearer token MUST be opaque to
//     this layer, but no real OIDC token contains spaces).
//   - non-empty token text after the prefix.
func extractBearer(header string) (string, bool) {
	header = strings.TrimSpace(header)
	if len(header) < len(bearerPrefix) {
		return "", false
	}
	if !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(bearerPrefix):])
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

// respondUnauthorized writes the 401 response with a
// RFC 6750-compliant `WWW-Authenticate` challenge AND the
// canonical Management API JSON [ErrorEnvelope] body. `realm`
// is the operator-facing realm string (defaults to
// `agent-memory mgmt-api` if empty); `extra` is any extra
// `; <key>="<value>"` parameters appended after the realm.
//
// The body shape is `application/json`, not the legacy
// `text/plain` http.Error emits. Returning the same envelope
// across the entire API surface is part of the Stage 7.1
// REST+JSON contract — clients should never need to switch on
// content-type to decode error bodies.
//
// The code / message in the JSON envelope mirror the RFC 6750
// `error=` parameter in the WWW-Authenticate header so a
// caller that reads only one of the two surfaces still gets
// an actionable signal:
//
//	error="invalid_request"  -> code="missing_token"
//	error="invalid_token"    -> code="invalid_token"
//	(neither)                -> code="unauthorized"
func respondUnauthorized(w http.ResponseWriter, realm, extra string) {
	if realm == "" {
		realm = "agent-memory mgmt-api"
	}
	challenge := fmt.Sprintf(`Bearer realm=%q`, realm)
	if extra != "" {
		challenge += ", " + extra
	}
	w.Header().Set("WWW-Authenticate", challenge)

	code := "unauthorized"
	message := "authentication required"
	switch {
	case strings.Contains(extra, `error="invalid_request"`):
		code = "missing_token"
		message = "bearer token is required"
	case strings.Contains(extra, `error="invalid_token"`):
		code = "invalid_token"
		message = "bearer token is invalid or expired"
	}
	writeJSONError(w, http.StatusUnauthorized, code, message)
}
