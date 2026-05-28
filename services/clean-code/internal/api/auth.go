package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AuthorizationHeader is the canonical HTTP header carrying the
// bearer token. RFC 6750 Sec 2 pins this name; the gateway
// rejects any other location (no query-string fallback, no
// cookie -- both are easier to log-leak than the header form).
const AuthorizationHeader = "Authorization"

// BearerScheme is the case-sensitive token-type prefix the
// gateway accepts. Per RFC 6750 Sec 2.1 the scheme MUST be
// "Bearer" -- a leading "bearer" lower-case is rejected
// (matches the conservative parsing recommended by RFC 7235
// Sec 2.1).
const BearerScheme = "Bearer"

// OIDCSubjectHeader is the trusted-channel header the gateway
// stamps on every downstream request after a successful
// bearer-token verification. Mirrors the constant of the same
// name owned by `internal/management/policy_verbs.go`
// (OIDCSubjectHeader = "X-OIDC-Subject") -- we re-declare the
// literal here so the api package does not take an import
// cycle on management. The two literals MUST stay in sync.
const OIDCSubjectHeader = "X-OIDC-Subject"

// Sentinel errors returned by [Authenticator.Authenticate]. The
// gateway maps each to its canonical HTTP status:
//
//   - [ErrMissingToken]   -- 401 with `WWW-Authenticate: Bearer`
//   - [ErrMalformedToken] -- 401 (RFC 6750 invalid_request)
//   - [ErrInvalidToken]   -- 401 (signature / format failure)
//   - [ErrExpiredToken]   -- 401 (RFC 6750 invalid_token, exp)
//   - [ErrBadAudience]    -- 403 (token valid but not for us)
//   - [ErrBadIssuer]      -- 401 (token from an unknown IdP)
//   - [ErrAuthBackend]    -- 503 (the AUTH INFRASTRUCTURE itself
//     failed -- JWKS fetch error, OIDC discovery error,
//     IdP returned a non-2xx). NOT 401: a downed IdP is
//     an operator-observable failure, not a caller-side
//     credential issue.
//
// Each error type wraps an underlying cause via [errors.Is];
// the gateway logs the underlying error server-side but emits
// only the canonical status / opaque body on the wire.
var (
	ErrMissingToken   = errors.New("api: missing bearer token")
	ErrMalformedToken = errors.New("api: malformed bearer token")
	ErrInvalidToken   = errors.New("api: invalid bearer token")
	ErrExpiredToken   = errors.New("api: bearer token expired")
	ErrBadAudience    = errors.New("api: bearer token audience does not match")
	ErrBadIssuer      = errors.New("api: bearer token issuer does not match")
	ErrAuthBackend    = errors.New("api: auth backend failure (JWKS / OIDC discovery)")
)

// Identity is the verified result of a successful bearer-token
// authentication. The gateway propagates [Identity.Subject]
// into the downstream `X-OIDC-Subject` header and the span's
// `caller_subject` attribute.
type Identity struct {
	// Subject is the verified `sub` claim. Per OIDC core
	// Sec 2 this is the IdP's stable identifier for the
	// caller and is the canonical value to log / attribute.
	Subject string
	// Audience is the verified `aud` claim list (one entry
	// for a single-audience token; multiple entries when
	// the token was issued for several relying parties).
	Audience []string
	// Issuer is the verified `iss` claim.
	Issuer string
	// IssuedAt / ExpiresAt are the verified `iat` / `exp`
	// claims. ExpiresAt MUST be non-zero on a successful
	// verification (the gateway rejects tokens without
	// `exp`).
	IssuedAt  time.Time
	ExpiresAt time.Time
	// RawClaims gives downstream code access to non-
	// standard claims (e.g. `email`, `groups`) without
	// re-parsing the JWT. Empty when the authenticator
	// elects not to expose claims.
	RawClaims map[string]json.RawMessage
}

// Authenticator verifies an HTTP bearer token and returns the
// caller's verified [Identity]. The interface intentionally
// hides the verification mechanism (JWKS-backed OIDC, static
// HMAC for tests, mTLS-bridged subject for service-to-service)
// so the composition root can swap implementations without
// touching the gateway.
//
// Authenticate MUST return one of the sentinel errors above
// (wrapped if needed) so the gateway can map to the correct
// HTTP status. Any other error is logged server-side and
// surfaced as 401 with an opaque body.
type Authenticator interface {
	Authenticate(ctx context.Context, bearerToken string) (*Identity, error)
}

// ParseBearer extracts the bearer token from an `Authorization`
// header value. Returns "" and an error sentinel when the
// header is missing, the scheme is wrong, or the token is
// blank. The function does NOT verify the token -- that is
// the [Authenticator]'s responsibility.
//
// Returned errors are sentinels from this file:
//
//   - [ErrMissingToken]   -- header is empty / whitespace.
//   - [ErrMalformedToken] -- scheme not "Bearer" or token is
//     blank after the scheme.
func ParseBearer(headerValue string) (string, error) {
	headerValue = strings.TrimSpace(headerValue)
	if headerValue == "" {
		return "", ErrMissingToken
	}
	// Case-sensitive match per RFC 6750 Sec 2.1. A
	// lowercase `bearer` is a malformed scheme.
	const prefix = BearerScheme + " "
	if !strings.HasPrefix(headerValue, prefix) {
		return "", fmt.Errorf("%w: scheme not %q", ErrMalformedToken, BearerScheme)
	}
	token := strings.TrimSpace(headerValue[len(prefix):])
	if token == "" {
		return "", fmt.Errorf("%w: empty token after %q", ErrMalformedToken, BearerScheme)
	}
	return token, nil
}

// jwtHeader is the JOSE header fragment the gateway parses
// from the bearer token. Only the fields the verifier
// inspects are decoded -- unknown header fields are ignored.
type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ,omitempty"`
	Kid string `json:"kid,omitempty"`
}

// jwtClaims is the registered-claim subset of the token
// payload the gateway requires. Non-standard claims are
// passed through via [Identity.RawClaims] from a second-pass
// decode into [map[string]json.RawMessage].
type jwtClaims struct {
	Sub string          `json:"sub"`
	Iss string          `json:"iss"`
	Aud json.RawMessage `json:"aud"`
	Exp int64           `json:"exp"`
	Iat int64           `json:"iat"`
	Nbf int64           `json:"nbf"`
}

// StaticHMACAuthenticator is a stdlib-only, HS256-signing JWT
// verifier intended for **service-internal / test-grade**
// deployments only -- e.g. the in-process gateway tests in
// this package and the local docker-compose stack. It
// satisfies the [Authenticator] contract well enough to
// exercise the gateway's 401 / 403 / 200 branches without
// requiring a JWKS endpoint or OIDC IdP.
//
// # NOT a production OIDC client
//
// Production deployments MUST use [OIDCAuthenticator] (in
// `oidc.go`), which implements the full RFC 7519 / OIDC
// Core 1.0 pipeline: RS256/RS384/RS512/ES256/ES384
// signature verification, JWKS-backed key rotation,
// optional OIDC discovery for `jwks_uri`, alg=none
// rejected, exact iss / aud match, exp / nbf with leeway,
// and a sentinel-typed `ErrAuthBackend` for JWKS-side
// infrastructure failures (which the gateway maps to 503,
// NOT to 401, so an operator does not mistake a downed IdP
// for a flood of invalid-token attempts).
//
// The deliberate "Static" + "HMAC" naming makes accidental
// production use easy to grep for in code review. The
// composition root selects between [StaticHMACAuthenticator]
// (tests / fixtures) and [OIDCAuthenticator] (production)
// at boot; the [Authenticator] interface contract is
// identical so swapping requires no gateway code changes.
type StaticHMACAuthenticator struct {
	// Secret is the shared HMAC-SHA256 key. Empty Secret
	// causes Authenticate to return [ErrInvalidToken] for
	// every call -- explicit refusal beats silently
	// trusting a zero-byte key.
	Secret []byte
	// Issuer is the expected `iss` claim. Empty means "do
	// not enforce issuer" -- acceptable for test fakes;
	// production wiring SHOULD set it.
	Issuer string
	// Audience is the expected `aud` claim. Required -- a
	// zero-value [StaticHMACAuthenticator{}] cannot
	// authenticate any token (Authenticate returns
	// [ErrBadAudience] until Audience is set). A token
	// whose `aud` does NOT contain this value returns
	// [ErrBadAudience] -- mapped to 403 by the gateway.
	Audience string
	// Now is the time-source used for `exp` / `nbf` checks.
	// Nil -> [time.Now].
	Now func() time.Time
	// Leeway is the clock-skew tolerance applied to `exp`
	// and `nbf` comparisons. Zero defaults to 60 seconds
	// (matching the OIDC core recommended default).
	Leeway time.Duration
}

// Authenticate implements [Authenticator]. The verifier
// pipeline:
//
//  1. Split the compact JWS triple `header.payload.signature`.
//     A malformed shape returns [ErrMalformedToken].
//  2. Decode the JOSE header, assert `alg == "HS256"`. Any
//     other `alg` (including "none", "RS256" on this
//     verifier, or empty) returns [ErrInvalidToken] -- this
//     is the canonical mitigation for the "alg=none"
//     downgrade attack.
//  3. Verify the HMAC-SHA256 signature over
//     `header_b64 + "." + payload_b64`. Mismatch ->
//     [ErrInvalidToken].
//  4. Decode the payload, enforce `iss` (if configured),
//     `aud` (always), `exp` (always), `nbf` (if present).
//     `sub` MUST be non-empty.
//  5. Return an [Identity] populated from the verified
//     claims.
//
// The verifier is intentionally strict about `alg` matching --
// a permissive verifier (accepting any algorithm advertised
// in the header) is an audit-finding-grade vulnerability.
func (a *StaticHMACAuthenticator) Authenticate(ctx context.Context, token string) (*Identity, error) {
	if a == nil {
		return nil, fmt.Errorf("%w: nil authenticator", ErrInvalidToken)
	}
	if len(a.Secret) == 0 {
		return nil, fmt.Errorf("%w: authenticator has no signing secret", ErrInvalidToken)
	}
	if a.Audience == "" {
		return nil, fmt.Errorf("%w: authenticator has no audience pin", ErrBadAudience)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: expected 3 dot-separated segments, got %d", ErrMalformedToken, len(parts))
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: decoding header: %v", ErrMalformedToken, err)
	}
	var hdr jwtHeader
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		return nil, fmt.Errorf("%w: parsing header: %v", ErrMalformedToken, err)
	}
	if hdr.Alg != "HS256" {
		return nil, fmt.Errorf("%w: alg %q not accepted (only HS256)", ErrInvalidToken, hdr.Alg)
	}

	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: decoding signature: %v", ErrMalformedToken, err)
	}
	signingInput := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, a.Secret)
	mac.Write([]byte(signingInput))
	expected := mac.Sum(nil)
	if !hmac.Equal(sigBytes, expected) {
		return nil, fmt.Errorf("%w: HMAC mismatch", ErrInvalidToken)
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: decoding payload: %v", ErrMalformedToken, err)
	}
	var claims jwtClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, fmt.Errorf("%w: parsing payload: %v", ErrMalformedToken, err)
	}
	rawClaims := map[string]json.RawMessage{}
	if err := json.Unmarshal(payloadBytes, &rawClaims); err != nil {
		return nil, fmt.Errorf("%w: parsing raw claims: %v", ErrMalformedToken, err)
	}

	if claims.Sub == "" {
		return nil, fmt.Errorf("%w: missing `sub` claim", ErrInvalidToken)
	}
	if a.Issuer != "" && claims.Iss != a.Issuer {
		return nil, fmt.Errorf("%w: issuer %q != %q", ErrBadIssuer, claims.Iss, a.Issuer)
	}
	audiences, err := decodeAudience(claims.Aud)
	if err != nil {
		return nil, fmt.Errorf("%w: aud claim malformed: %v", ErrMalformedToken, err)
	}
	if !audienceContains(audiences, a.Audience) {
		return nil, fmt.Errorf("%w: aud %v does not contain %q", ErrBadAudience, audiences, a.Audience)
	}

	now := a.now()
	leeway := a.leeway()
	if claims.Exp == 0 {
		return nil, fmt.Errorf("%w: missing `exp` claim", ErrInvalidToken)
	}
	exp := time.Unix(claims.Exp, 0)
	if now.After(exp.Add(leeway)) {
		return nil, fmt.Errorf("%w: exp=%s now=%s leeway=%s", ErrExpiredToken, exp.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339), leeway)
	}
	if claims.Nbf != 0 {
		nbf := time.Unix(claims.Nbf, 0)
		if now.Add(leeway).Before(nbf) {
			return nil, fmt.Errorf("%w: nbf=%s now=%s leeway=%s", ErrInvalidToken, nbf.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339), leeway)
		}
	}
	identity := &Identity{
		Subject:   claims.Sub,
		Audience:  audiences,
		Issuer:    claims.Iss,
		ExpiresAt: exp,
		RawClaims: rawClaims,
	}
	if claims.Iat != 0 {
		identity.IssuedAt = time.Unix(claims.Iat, 0)
	}
	return identity, nil
}

func (a *StaticHMACAuthenticator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *StaticHMACAuthenticator) leeway() time.Duration {
	if a.Leeway > 0 {
		return a.Leeway
	}
	return 60 * time.Second
}

// decodeAudience accepts the two RFC 7519 Sec 4.1.3 forms for
// the `aud` claim: a JSON string or a JSON array of strings.
// Any other shape is rejected with an error -- the gateway
// will not silently accept a `null` / numeric / nested aud
// because the canonical OIDC contract is "case-sensitive
// strings of the form `https://...` (URLs)".
func decodeAudience(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errors.New("aud is missing or null")
	}
	// Try string first (the common single-audience form).
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if single == "" {
			return nil, errors.New("aud is an empty string")
		}
		return []string{single}, nil
	}
	// Fall back to array of strings.
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		if len(many) == 0 {
			return nil, errors.New("aud array is empty")
		}
		for i, a := range many {
			if a == "" {
				return nil, fmt.Errorf("aud[%d] is an empty string", i)
			}
		}
		return many, nil
	}
	return nil, errors.New("aud is neither string nor array of strings")
}

// audienceContains returns true iff `want` exactly matches one
// of `auds`. Exact match only -- no prefix / substring match
// because OIDC audience claims are URLs and a substring match
// is a vector for confused-deputy attacks (e.g. an audience
// of `https://attacker.example/clean-coded` would substring-
// match `clean-coded`).
func audienceContains(auds []string, want string) bool {
	for _, a := range auds {
		if a == want {
			return true
		}
	}
	return false
}

// MintHS256TestToken mints a compact JWS for the gateway's
// HS256 verifier path. **TEST USE ONLY** -- the function is
// exported from the production package strictly so the
// composition root's smoke-test in `cmd/.../*_test.go` can
// drive the gateway end-to-end without re-implementing the
// JWT encoding. Production code MUST NOT call this -- a
// production-side token mint belongs in the IdP, not in the
// resource server.
//
// The function panics on any encoding error: it is invoked
// only from tests where panics are caught by the test
// framework.
func MintHS256TestToken(secret []byte, claims map[string]any) string {
	if claims == nil {
		claims = map[string]any{}
	}
	header := map[string]string{"alg": "HS256", "typ": "JWT"}
	hb, err := json.Marshal(header)
	if err != nil {
		panic(fmt.Sprintf("api: marshalling JWT header: %v", err))
	}
	pb, err := json.Marshal(claims)
	if err != nil {
		panic(fmt.Sprintf("api: marshalling JWT claims: %v", err))
	}
	h := base64.RawURLEncoding.EncodeToString(hb)
	p := base64.RawURLEncoding.EncodeToString(pb)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(h + "." + p))
	s := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return h + "." + p + "." + s
}
