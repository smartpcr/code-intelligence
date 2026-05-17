package mgmtapi

// Real OIDC bearer-token verifier for the Stage 7.1
// Management API. Implements RS256 / RS384 / RS512 signature
// verification against a JWKS endpoint plus full claim
// validation (iss / aud / exp / nbf / sub) per RFC 7519 and
// the OpenID Connect Core spec.
//
// This is the production-grade verifier the Management API
// uses when AGENT_MEMORY_OIDC_ISSUER / AGENT_MEMORY_OIDC_AUDIENCE
// / AGENT_MEMORY_OIDC_JWKS_URL are configured. The dev-only
// StaticBearerVerifier in auth.go remains opt-in via
// AGENT_MEMORY_OIDC_DEV_TOKEN — but composition root rejects a
// boot where neither verifier is wired.
//
// Scope:
//   - Algorithms: RS256, RS384, RS512 only. ES256 / HS256 / none
//     deliberately rejected; the architecture pin's
//     authentication contract uses asymmetric tokens issued by
//     a trusted IdP, and HS-family algorithms (shared secret)
//     would silently downgrade to a single-secret deployment.
//   - JWKS fetching: HTTPS GET against the configured JWKS URL,
//     cached for CacheTTL (default 5m), refresh deduplicated
//     via a singleflight-style mutex so a burst of unknown-kid
//     tokens cannot hammer the IdP.
//   - kid required: tokens without a `kid` header field are
//     rejected. Standard OIDC IdPs (Okta, Auth0, AzureAD,
//     Google, Keycloak) always emit a kid; a missing kid is a
//     signal of either misconfiguration or a forged token.
//   - Defensive JWK parsing: rejects `kty != "RSA"`,
//     `use != "sig"` (when present), keys whose `e` is even /
//     <= 1 / oversized, keys whose `n` decodes to a non-positive
//     modulus.
//
// Error mapping:
//   - Network / 5xx / unparseable JWKS / no usable keys in JWKS
//     -> ErrVerifierUnavailable (handler -> 503).
//   - Missing token -> ErrTokenMissing (handler -> 401
//     invalid_request).
//   - Bad alg, kid not found after a fresh JWKS refresh, bad
//     signature, claim mismatch, expired, malformed -> wrapped
//     ErrTokenInvalid (handler -> 401 invalid_token).
//
// Concurrency:
//   - Verify is safe for concurrent calls. The keys map is
//     replaced atomically under a write lock; readers hold the
//     read lock long enough to copy the *rsa.PublicKey
//     reference, then release before doing cryptographic work.
//   - refreshMu serialises JWKS network fetches so a burst of
//     uncached kids triggers exactly one fetch.

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultJWKSCacheTTL is the cache lifetime of a JWKS
// document. 5 minutes is the same default coreos/go-oidc uses
// — short enough that a key rotation propagates within an
// operator-visible time window, long enough that a JWKS
// endpoint outage doesn't immediately blow up token
// verification across the fleet.
const DefaultJWKSCacheTTL = 5 * time.Minute

// DefaultJWKSRefreshFloor is the minimum interval between two
// JWKS refresh attempts triggered by unknown-kid cache misses.
// Without this, a flood of tokens with random kids would
// cause every miss to hit the IdP, which is exactly the
// thundering-herd vector the singleflight-style refreshMu is
// supposed to prevent. 30s is short enough that a legitimate
// key rotation propagates quickly and long enough that
// adversarial traffic can't pin the JWKS endpoint at 100%.
const DefaultJWKSRefreshFloor = 30 * time.Second

// DefaultJWKSMaxBytes caps the JWKS response body size so a
// hostile / broken endpoint cannot starve the process with a
// multi-gigabyte JSON blob.
const DefaultJWKSMaxBytes int64 = 256 << 10 // 256 KiB

// OIDCVerifier is a JWKS-backed JWT verifier for RS256 /
// RS384 / RS512 tokens. Construct one per process; reuse
// across requests. Zero-value fields take their respective
// defaults (see DefaultJWKSCacheTTL etc).
//
// All fields are read-only after construction. The internal
// JWKS cache lives behind a RWMutex so Verify is safe for
// concurrent use.
type OIDCVerifier struct {
	// Issuer is the expected `iss` claim. REQUIRED;
	// constructing a verifier with an empty Issuer panics
	// (an empty Issuer accepting any iss is a wide-open
	// auth bypass).
	Issuer string

	// Audience is the expected `aud` claim. REQUIRED for
	// the same reason as Issuer; empty Audience refuses to
	// match any token at all (fail-closed).
	Audience string

	// JWKSURL is the absolute https URL of the IdP's JWKS
	// endpoint (typically `<issuer>/.well-known/jwks.json`).
	// REQUIRED.
	JWKSURL string

	// HTTPClient is the client used to fetch the JWKS. nil
	// means a default client with a 10s timeout.
	HTTPClient *http.Client

	// Clock is the time source for ALL time-dependent
	// verifier decisions: exp / nbf claim validation, JWKS
	// cache TTL freshness, and the unknown-kid refresh
	// floor. nil means time.Now. Tests that need
	// deterministic behaviour around cache expiry or token
	// expiration set this to a controlled clock; all three
	// subsystems then observe the same notion of "now",
	// preventing split-clock skew between token validation
	// and cache freshness.
	Clock func() time.Time

	// Leeway is the tolerance for clock skew when checking
	// exp / nbf. Zero means no tolerance.
	Leeway time.Duration

	// CacheTTL is how long a fetched JWKS document is
	// trusted before a refresh is attempted. Zero means
	// DefaultJWKSCacheTTL.
	CacheTTL time.Duration

	// RefreshFloor is the minimum interval between two
	// unknown-kid-triggered JWKS refresh attempts. Zero
	// means DefaultJWKSRefreshFloor.
	RefreshFloor time.Duration

	// MaxJWKSBytes caps the JWKS response body. Zero means
	// DefaultJWKSMaxBytes; negative disables the cap (NOT
	// recommended in production).
	MaxJWKSBytes int64

	// AllowedAlgs whitelist. Empty means RS256+RS384+RS512.
	// Anything outside this set is rejected up-front.
	AllowedAlgs []string

	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey // kid -> RSA public key
	fetchedAt time.Time

	refreshMu sync.Mutex // serialises refreshJWKS network calls
}

// NewOIDCVerifier constructs an OIDCVerifier and fails closed
// on any missing or unsafe required field. Use this constructor
// rather than the zero value so a misconfigured deployment
// fails to start (exit 2 in the cmd binary) instead of
// silently accepting wide-open tokens.
//
// JWKS URL scheme is REQUIRED to be `https://`. An IdP
// served over plaintext would let a network-positioned
// attacker swap signing keys and forge tokens; we refuse to
// boot rather than provide that footgun. Tests that need to
// serve JWKS from net/http/httptest (which is plaintext) call
// the unexported newOIDCVerifierInsecure helper instead.
func NewOIDCVerifier(issuer, audience, jwksURL string) (*OIDCVerifier, error) {
	v, err := newOIDCVerifierCommon(issuer, audience, jwksURL)
	if err != nil {
		return nil, err
	}
	parsed, perr := url.Parse(strings.TrimSpace(jwksURL))
	if perr != nil {
		return nil, fmt.Errorf("mgmtapi: OIDCVerifier: JWKSURL %q is not a valid URL: %v", jwksURL, perr)
	}
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("mgmtapi: OIDCVerifier: JWKSURL must use https:// (got scheme %q); plaintext JWKS endpoints expose token verification to MITM key substitution", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("mgmtapi: OIDCVerifier: JWKSURL %q has no host", jwksURL)
	}
	return v, nil
}

// newOIDCVerifierInsecure constructs an OIDCVerifier WITHOUT
// the JWKS-must-be-https check. Test-only — production
// callers use NewOIDCVerifier above. The function is
// unexported so the cmd/ composition root cannot accidentally
// reach it.
func newOIDCVerifierInsecure(issuer, audience, jwksURL string) (*OIDCVerifier, error) {
	return newOIDCVerifierCommon(issuer, audience, jwksURL)
}

// newOIDCVerifierCommon is the shared field-validation half
// that both the production and test-only constructors run.
func newOIDCVerifierCommon(issuer, audience, jwksURL string) (*OIDCVerifier, error) {
	if strings.TrimSpace(issuer) == "" {
		return nil, errors.New("mgmtapi: OIDCVerifier: Issuer is required")
	}
	if strings.TrimSpace(audience) == "" {
		return nil, errors.New("mgmtapi: OIDCVerifier: Audience is required")
	}
	if strings.TrimSpace(jwksURL) == "" {
		return nil, errors.New("mgmtapi: OIDCVerifier: JWKSURL is required")
	}
	return &OIDCVerifier{
		Issuer:   issuer,
		Audience: audience,
		JWKSURL:  jwksURL,
		keys:     map[string]*rsa.PublicKey{},
	}, nil
}

// jwksDoc is the wire shape of an OIDC JWKS document. We
// only consume the `keys` array; the IdP may emit additional
// top-level fields which we deliberately ignore.
type jwksDoc struct {
	Keys []jwksKey `json:"keys"`
}

// jwksKey is the wire shape of a single JWK. Only RSA `kty`
// is consumed; everything else is skipped silently.
type jwksKey struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// jwtHeader is the wire shape of the JWT header (first
// base64url segment).
type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

// jwtClaims is the wire shape of the JWT payload (second
// base64url segment). `aud` is captured as a json.RawMessage
// because OIDC permits it to be either a string or an array
// of strings.
type jwtClaims struct {
	Iss string          `json:"iss"`
	Sub string          `json:"sub"`
	Aud json.RawMessage `json:"aud"`
	Exp int64           `json:"exp"`
	Nbf int64           `json:"nbf"`
	Iat int64           `json:"iat"`
}

// Verify implements [TokenVerifier]. See the package comment
// for the full error-mapping contract.
func (v *OIDCVerifier) Verify(ctx context.Context, raw string) (string, error) {
	if raw == "" {
		return "", ErrTokenMissing
	}
	// Quick structural check before any base64 work.
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("%w: malformed compact JWT", ErrTokenInvalid)
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("%w: header base64: %v", ErrTokenInvalid, err)
	}
	var hdr jwtHeader
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		return "", fmt.Errorf("%w: header json: %v", ErrTokenInvalid, err)
	}
	if hdr.Alg == "" || strings.EqualFold(hdr.Alg, "none") {
		return "", fmt.Errorf("%w: alg %q rejected", ErrTokenInvalid, hdr.Alg)
	}
	hash, ok := v.algorithmHash(hdr.Alg)
	if !ok {
		return "", fmt.Errorf("%w: unsupported alg %q", ErrTokenInvalid, hdr.Alg)
	}
	if hdr.Kid == "" {
		return "", fmt.Errorf("%w: kid header is required", ErrTokenInvalid)
	}
	if len(hdr.Kid) > 256 {
		// Defensive bound: limit kid length so it can never
		// be used to inflate the JWKS cache key space.
		return "", fmt.Errorf("%w: kid header too long", ErrTokenInvalid)
	}

	key, err := v.lookupKey(ctx, hdr.Kid)
	if err != nil {
		return "", err
	}

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("%w: signature base64: %v", ErrTokenInvalid, err)
	}
	signingInput := []byte(parts[0] + "." + parts[1])
	digest, err := hashBytes(hash, signingInput)
	if err != nil {
		return "", fmt.Errorf("%w: hash compute: %v", ErrTokenInvalid, err)
	}
	if err := rsa.VerifyPKCS1v15(key, hash, digest, signature); err != nil {
		return "", fmt.Errorf("%w: signature invalid", ErrTokenInvalid)
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("%w: payload base64: %v", ErrTokenInvalid, err)
	}
	var claims jwtClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return "", fmt.Errorf("%w: payload json: %v", ErrTokenInvalid, err)
	}

	now := v.now()

	if claims.Iss == "" || claims.Iss != v.Issuer {
		return "", fmt.Errorf("%w: iss %q does not match expected issuer", ErrTokenInvalid, claims.Iss)
	}
	if claims.Sub == "" {
		return "", fmt.Errorf("%w: sub claim required", ErrTokenInvalid)
	}
	if claims.Exp <= 0 {
		return "", fmt.Errorf("%w: exp claim required", ErrTokenInvalid)
	}
	exp := time.Unix(claims.Exp, 0)
	if now.After(exp.Add(v.Leeway)) {
		return "", fmt.Errorf("%w: token expired at %s", ErrTokenInvalid, exp.UTC().Format(time.RFC3339))
	}
	if claims.Nbf > 0 {
		nbf := time.Unix(claims.Nbf, 0)
		if now.Add(v.Leeway).Before(nbf) {
			return "", fmt.Errorf("%w: token not yet valid (nbf=%s)", ErrTokenInvalid, nbf.UTC().Format(time.RFC3339))
		}
	}
	if err := matchAudience(claims.Aud, v.Audience); err != nil {
		return "", err
	}
	return claims.Sub, nil
}

// algorithmHash returns the crypto.Hash that corresponds to
// the JWT `alg` header, plus an ok flag. The whitelist is
// either v.AllowedAlgs or the package default
// (RS256/RS384/RS512). Anything else returns ok=false and the
// caller maps to ErrTokenInvalid.
func (v *OIDCVerifier) algorithmHash(alg string) (crypto.Hash, bool) {
	allowed := v.AllowedAlgs
	if len(allowed) == 0 {
		allowed = []string{"RS256", "RS384", "RS512"}
	}
	for _, a := range allowed {
		if a == alg {
			switch a {
			case "RS256":
				return crypto.SHA256, true
			case "RS384":
				return crypto.SHA384, true
			case "RS512":
				return crypto.SHA512, true
			}
		}
	}
	return 0, false
}

// hashBytes hashes data using h. Pulled into a helper so
// Verify stays linear and the algorithm switch lives in
// exactly one place.
func hashBytes(h crypto.Hash, data []byte) ([]byte, error) {
	switch h {
	case crypto.SHA256:
		s := sha256.Sum256(data)
		return s[:], nil
	case crypto.SHA384:
		s := sha512.Sum384(data)
		return s[:], nil
	case crypto.SHA512:
		s := sha512.Sum512(data)
		return s[:], nil
	default:
		return nil, fmt.Errorf("hashBytes: unsupported hash %v", h)
	}
}

// matchAudience checks that the configured Audience is present
// in the token's `aud` claim. RFC 7519 permits `aud` to be
// either a single string or an array of strings; we accept
// both shapes.
func matchAudience(rawAud json.RawMessage, want string) error {
	if len(rawAud) == 0 {
		return fmt.Errorf("%w: aud claim required", ErrTokenInvalid)
	}
	var single string
	if err := json.Unmarshal(rawAud, &single); err == nil {
		if single == want {
			return nil
		}
		return fmt.Errorf("%w: aud %q does not include configured audience", ErrTokenInvalid, single)
	}
	var multi []string
	if err := json.Unmarshal(rawAud, &multi); err != nil {
		return fmt.Errorf("%w: aud claim has unexpected shape", ErrTokenInvalid)
	}
	for _, a := range multi {
		if a == want {
			return nil
		}
	}
	return fmt.Errorf("%w: aud %v does not include configured audience", ErrTokenInvalid, multi)
}

// lookupKey resolves the RSA public key for `kid`. Hits the
// cache first; on miss, attempts a refresh subject to the
// refresh floor. A successful refresh that still does not
// surface `kid` returns ErrTokenInvalid (the kid is genuinely
// unknown to the IdP, not a transient outage).
func (v *OIDCVerifier) lookupKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	key, ok := v.keys[kid]
	fetched := v.fetchedAt
	v.mu.RUnlock()
	if ok && v.cacheFresh(fetched) {
		return key, nil
	}

	// Cache miss OR stale. Take the refresh lock so a burst
	// of concurrent unknown-kid requests collapses into one
	// JWKS fetch.
	v.refreshMu.Lock()
	defer v.refreshMu.Unlock()

	// Re-check after acquiring the lock — another goroutine
	// may have already refreshed.
	v.mu.RLock()
	key, ok = v.keys[kid]
	fetched = v.fetchedAt
	v.mu.RUnlock()
	if ok && v.cacheFresh(fetched) {
		return key, nil
	}

	// Throttle: if we refreshed very recently and the kid
	// still isn't there, treat as an invalid token rather
	// than retry the network. This is the negative-cache
	// guard against random-kid traffic. We compare against
	// the `fetched` snapshot captured above under v.mu (not
	// a fresh read of v.fetchedAt) so the throttle decision
	// stays consistent with the cache-freshness re-check
	// and avoids an unlocked field read.
	if !fetched.IsZero() && v.now().Sub(fetched) < v.refreshFloor() {
		if ok {
			return key, nil
		}
		return nil, fmt.Errorf("%w: kid %q not in JWKS (recent refresh)", ErrTokenInvalid, kid)
	}

	if err := v.refreshJWKS(ctx); err != nil {
		return nil, err
	}
	v.mu.RLock()
	key, ok = v.keys[kid]
	v.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: kid %q not in JWKS", ErrTokenInvalid, kid)
	}
	return key, nil
}

// now returns the verifier's notion of the current time. It
// honours v.Clock when set, otherwise falls back to time.Now.
// Centralising this fallback ensures every time-dependent
// decision in the verifier (token exp / nbf, JWKS cache TTL,
// unknown-kid refresh floor) observes a single, consistent
// clock — tests with a frozen v.Clock can therefore exercise
// cache-expiry behaviour deterministically, and any deployment
// where v.Clock diverges from wall time (clock-skew tests,
// fault injection) keeps token validation and cache freshness
// in agreement.
func (v *OIDCVerifier) now() time.Time {
	if v.Clock != nil {
		return v.Clock()
	}
	return time.Now()
}

// cacheFresh reports whether a JWKS document fetched at
// `fetched` is still within the cache TTL window. Uses
// v.now() so a custom v.Clock controls cache expiry as well
// as token claim validation.
func (v *OIDCVerifier) cacheFresh(fetched time.Time) bool {
	if fetched.IsZero() {
		return false
	}
	ttl := v.CacheTTL
	if ttl <= 0 {
		ttl = DefaultJWKSCacheTTL
	}
	return v.now().Sub(fetched) < ttl
}

// refreshFloor returns the minimum interval between two
// unknown-kid-triggered refreshes.
func (v *OIDCVerifier) refreshFloor() time.Duration {
	if v.RefreshFloor > 0 {
		return v.RefreshFloor
	}
	return DefaultJWKSRefreshFloor
}

// refreshJWKS fetches and parses the JWKS document at
// v.JWKSURL. On success replaces v.keys atomically. On any
// failure returns ErrVerifierUnavailable (network / 5xx /
// parse / empty), leaving any previously-cached keys in place.
func (v *OIDCVerifier) refreshJWKS(ctx context.Context) error {
	client := v.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.JWKSURL, nil)
	if err != nil {
		return fmt.Errorf("%w: build jwks request: %v", ErrVerifierUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: fetch jwks: %v", ErrVerifierUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: jwks endpoint returned %d", ErrVerifierUnavailable, resp.StatusCode)
	}
	maxBytes := v.MaxJWKSBytes
	if maxBytes == 0 {
		maxBytes = DefaultJWKSMaxBytes
	}
	var reader io.Reader = resp.Body
	if maxBytes > 0 {
		reader = io.LimitReader(resp.Body, maxBytes+1)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("%w: read jwks: %v", ErrVerifierUnavailable, err)
	}
	if maxBytes > 0 && int64(len(body)) > maxBytes {
		return fmt.Errorf("%w: jwks body exceeded %d bytes", ErrVerifierUnavailable, maxBytes)
	}
	var doc jwksDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("%w: parse jwks: %v", ErrVerifierUnavailable, err)
	}
	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		pub, err := jwkToRSA(k)
		if err != nil {
			// Skip non-RSA / malformed entries silently
			// — a JWKS may include EC, oct, or unusable
			// keys alongside the RSA signing keys.
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return fmt.Errorf("%w: jwks contained no usable RSA signing keys", ErrVerifierUnavailable)
	}
	now := v.now()
	v.mu.Lock()
	v.keys = keys
	v.fetchedAt = now
	v.mu.Unlock()
	return nil
}

// jwkToRSA constructs an *rsa.PublicKey from a JWK. Rejects:
//   - kty != "RSA"
//   - use present and not "sig"
//   - empty kid (we look up by kid; a kidless entry would be
//     accessible only by exhaustive enumeration)
//   - empty / unparseable n / e
//   - non-positive modulus
//   - exponent <= 1, even (only odd exponents are usable for
//     PKCS#1 v1.5 signature verification), or larger than fits
//     in an int
func jwkToRSA(k jwksKey) (*rsa.PublicKey, error) {
	if k.Kty != "RSA" {
		return nil, fmt.Errorf("jwkToRSA: kty %q is not RSA", k.Kty)
	}
	if k.Use != "" && k.Use != "sig" {
		return nil, fmt.Errorf("jwkToRSA: use %q is not sig", k.Use)
	}
	if strings.TrimSpace(k.Kid) == "" {
		return nil, errors.New("jwkToRSA: kid required")
	}
	if k.N == "" || k.E == "" {
		return nil, errors.New("jwkToRSA: n / e required")
	}
	nb, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("jwkToRSA: decode n: %w", err)
	}
	eb, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("jwkToRSA: decode e: %w", err)
	}
	n := new(big.Int).SetBytes(nb)
	if n.Sign() <= 0 {
		return nil, errors.New("jwkToRSA: n is non-positive")
	}
	// Decode the exponent as a big-endian unsigned integer.
	// The typical value is 65537 which fits in 24 bits.
	if len(eb) == 0 || len(eb) > 8 {
		return nil, fmt.Errorf("jwkToRSA: e has unexpected length %d", len(eb))
	}
	var e uint64
	for _, b := range eb {
		e = e<<8 | uint64(b)
	}
	if e <= 1 {
		return nil, fmt.Errorf("jwkToRSA: e=%d is too small", e)
	}
	if e%2 == 0 {
		return nil, fmt.Errorf("jwkToRSA: e=%d is even", e)
	}
	if e > 1<<31-1 {
		return nil, fmt.Errorf("jwkToRSA: e=%d does not fit in int", e)
	}
	return &rsa.PublicKey{N: n, E: int(e)}, nil
}
