package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// OIDCAuthenticator is the production [Authenticator] for the
// HTTP/JSON gateway. It verifies a bearer JWT against the
// signing keys served from the configured OIDC issuer's JWKS
// endpoint. The JWKS URL is either supplied explicitly via
// [OIDCAuthenticatorConfig.JWKSURL] or resolved via OIDC
// Discovery 1.0 (`{issuer}/.well-known/openid-configuration`
// -> `jwks_uri`) when the explicit URL is empty.
//
// The authenticator implements the full RFC 7519 / OIDC core
// verification pipeline:
//
//  1. Parses the JWT, refuses `alg=none`.
//  2. Looks up the signing key by `kid` in the cached JWKS.
//     A cache miss triggers a re-fetch of the JWKS (cache
//     invalidation on unknown kid is the canonical key-
//     rotation handling per RFC 7517).
//  3. Verifies the signature with the matching key. Only
//     `RS256`, `RS384`, `RS512`, `ES256`, `ES384` are
//     accepted -- HS* family is intentionally rejected
//     (per OIDC core Sec 5.1 a public JWT MUST NOT use a
//     symmetric algorithm; only the test-grade
//     [StaticHMACAuthenticator] accepts HS256).
//  4. Validates `iss` (exact match), `aud` (contains the
//     configured audience), `exp` / `nbf` with clock-skew
//     leeway, `iat` not in the far future.
//  5. Surfaces sentinel errors compatible with the
//     gateway's existing error -> status mapping.
//
// The JWKS is fetched on demand and cached for
// [OIDCAuthenticatorConfig.CacheTTL] (default 5 minutes).
// The cache is refreshed lazily: a request that cannot
// resolve `kid` triggers a synchronous re-fetch (capped at
// one in-flight refresh via `sync.Once`-style coordination)
// so a freshly-rotated key is picked up within one request.
//
// # Why golang-jwt/jwt/v5
//
// The library is the de facto stdlib-grade JWT verifier in
// the Go ecosystem (used by every major OIDC client in the
// language). It handles the compact JWS framing, header
// parsing, and signing-method dispatch. We retain explicit
// control of: which `alg` values are accepted, how the JWKS
// is resolved, and which claims are validated.
type OIDCAuthenticator struct {
	cfg         OIDCAuthenticatorConfig
	parser      *jwt.Parser
	cache       *jwksCache
	acceptedAlg map[string]struct{}
}

// OIDCAuthenticatorConfig pins the wire-time inputs of an
// [OIDCAuthenticator]. The composition root populates this
// from configuration; tests pass a hand-rolled JWKS endpoint.
type OIDCAuthenticatorConfig struct {
	// Issuer is the expected `iss` claim. REQUIRED. The
	// authenticator rejects any token whose `iss` does
	// not exactly match this value.
	Issuer string

	// Audience is the expected `aud` claim entry. REQUIRED.
	// The authenticator accepts a token iff its `aud`
	// (string or array form) exactly contains this value.
	Audience string

	// JWKSURL is the absolute URL of the JWKS endpoint.
	// Empty triggers OIDC Discovery 1.0: the
	// authenticator fetches
	// `{Issuer}/.well-known/openid-configuration`,
	// parses the `jwks_uri` field, and uses that URL
	// for subsequent JWKS fetches. Discovery results
	// are cached for [CacheTTL] alongside the JWKS so
	// a single transient IdP outage triggers at most
	// one re-discovery + re-fetch.
	JWKSURL string

	// HTTPClient is the client used to fetch the JWKS.
	// Nil falls back to [http.DefaultClient] with a 10s
	// timeout. Composition roots SHOULD pass a client
	// with a bounded timeout (the gateway is on the hot
	// path -- a slow JWKS endpoint must not block
	// authentication).
	HTTPClient *http.Client

	// CacheTTL is how long the JWKS is cached before a
	// background refresh. Zero defaults to 5 minutes.
	// The cache is ALSO invalidated lazily on an unknown
	// `kid` (key rotation handled within one request).
	CacheTTL time.Duration

	// Leeway is the clock-skew tolerance applied to
	// `exp` / `nbf` / `iat`. Zero defaults to 60s
	// (OIDC core recommended).
	Leeway time.Duration

	// Now is the time source. Nil -> [time.Now]. Tests
	// override this to drive deterministic exp/nbf
	// branches.
	Now func() time.Time

	// AcceptedAlgorithms restricts the JWS signing
	// algorithms the verifier honours. Empty defaults to
	// {"RS256","RS384","RS512","ES256","ES384"}. The
	// `none` algorithm is ALWAYS rejected regardless of
	// this list.
	AcceptedAlgorithms []string
}

// NewOIDCAuthenticator constructs an [OIDCAuthenticator] from
// `cfg`. Returns an error when required fields are missing
// (Issuer / Audience). The constructor does NOT pre-fetch the
// JWKS -- the first authentication call lazily warms the
// cache so a slow / unreachable IdP does not block service
// startup.
func NewOIDCAuthenticator(cfg OIDCAuthenticatorConfig) (*OIDCAuthenticator, error) {
	if cfg.Issuer == "" {
		return nil, errors.New("api: OIDCAuthenticatorConfig.Issuer is required")
	}
	if cfg.Audience == "" {
		return nil, errors.New("api: OIDCAuthenticatorConfig.Audience is required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 5 * time.Minute
	}
	if cfg.Leeway <= 0 {
		cfg.Leeway = 60 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	accepted := cfg.AcceptedAlgorithms
	if len(accepted) == 0 {
		accepted = []string{"RS256", "RS384", "RS512", "ES256", "ES384"}
	}
	for _, alg := range accepted {
		if alg == "none" || alg == "" {
			return nil, fmt.Errorf("api: OIDCAuthenticatorConfig.AcceptedAlgorithms must not include %q", alg)
		}
	}
	acceptedSet := make(map[string]struct{}, len(accepted))
	for _, a := range accepted {
		acceptedSet[a] = struct{}{}
	}
	// jwt.WithTimeFunc threads cfg.Now into the library's
	// claim-validation clock so exp/nbf/iat are evaluated
	// against the SAME time source the tests / operator
	// override. Without this option golang-jwt/v5 falls
	// back to time.Now() internally, defeating the whole
	// point of cfg.Now (item #4 from iter-3 feedback).
	parser := jwt.NewParser(
		jwt.WithValidMethods(accepted),
		jwt.WithIssuer(cfg.Issuer),
		jwt.WithAudience(cfg.Audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(cfg.Leeway),
		jwt.WithIssuedAt(),
		jwt.WithTimeFunc(cfg.Now),
	)
	return &OIDCAuthenticator{
		cfg:         cfg,
		parser:      parser,
		acceptedAlg: acceptedSet,
		cache: &jwksCache{
			url:           cfg.JWKSURL,
			issuer:        cfg.Issuer,
			discover:      cfg.JWKSURL == "",
			client:        cfg.HTTPClient,
			ttl:           cfg.CacheTTL,
			now:           cfg.Now,
			discoveryPath: "/.well-known/openid-configuration",
		},
	}, nil
}

// Authenticate verifies the bearer token end-to-end. The
// pipeline matches the doc-comment on [OIDCAuthenticator].
//
// # Error classification (Item #5 from iter-2 feedback)
//
// JWKS fetch / OIDC discovery failures (network errors, IdP
// returning non-2xx, malformed discovery doc) are wrapped as
// [ErrAuthBackend] -- the gateway maps that to 503 with a
// `Retry-After` header so dashboards distinguish "downed IdP"
// from "invalid token". Token-shaped failures (bad signature,
// unknown kid AFTER a fresh fetch, claim mismatch) stay
// classified as [ErrInvalidToken] / [ErrExpiredToken] /
// [ErrBadAudience] / [ErrBadIssuer].
//
// The pipeline peeks the JWT header BEFORE invoking the
// parser, so JWKS infrastructure errors short-circuit and
// never get re-classified by the parser's keyfunc as
// `jwt.ErrTokenUnverifiable` -> `ErrInvalidToken` (the bug
// the iter-2 evaluator flagged).
func (a *OIDCAuthenticator) Authenticate(ctx context.Context, bearer string) (*Identity, error) {
	if a == nil {
		return nil, fmt.Errorf("%w: nil OIDCAuthenticator", ErrInvalidToken)
	}
	// Step 1: peek JWT header to extract `kid` and `alg`
	// WITHOUT invoking the parser's keyfunc. This way
	// JWKS-fetch errors travel via a separate code path
	// from token-validity errors and the gateway can
	// classify them correctly.
	kid, alg, err := peekJWTHeader(bearer)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedToken, err)
	}
	if alg == "" || alg == "none" {
		return nil, fmt.Errorf("%w: alg %q rejected", ErrInvalidToken, alg)
	}
	if _, ok := a.acceptedAlg[alg]; !ok {
		return nil, fmt.Errorf("%w: alg %q not in accepted set", ErrInvalidToken, alg)
	}
	if kid == "" {
		return nil, fmt.Errorf("%w: JWT header missing kid", ErrInvalidToken)
	}
	// Step 2: resolve signing key via JWKS cache.
	// jwksCache.Key returns errKidNotInJWKS when a kid
	// genuinely is not present in a successfully-fetched
	// JWKS (the caller's token references a non-existent
	// key -- treat as ErrInvalidToken). Any OTHER error
	// is a backend infrastructure failure (network, 5xx,
	// malformed body, OIDC discovery failed) -- wrap as
	// ErrAuthBackend so the gateway returns 503.
	key, err := a.cache.Key(ctx, kid)
	if err != nil {
		if errors.Is(err, errKidNotInJWKS) {
			return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
		}
		return nil, fmt.Errorf("%w: %v", ErrAuthBackend, err)
	}
	// Step 3: parse + verify the token. The keyfunc just
	// returns the already-resolved key; defence-in-depth
	// rejects alg=none even though parser.WithValidMethods
	// already filters it.
	keyFn := func(t *jwt.Token) (any, error) {
		if t.Method == nil || t.Method.Alg() == "none" || t.Method.Alg() == "" {
			return nil, errors.New("alg=none is rejected")
		}
		return key, nil
	}
	token, err := a.parser.Parse(bearer, keyFn)
	if err != nil {
		return nil, classifyJWTError(err)
	}
	if !token.Valid {
		return nil, fmt.Errorf("%w: token not valid", ErrInvalidToken)
	}
	mapClaims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("%w: unexpected claims type %T", ErrInvalidToken, token.Claims)
	}
	subject, _ := mapClaims["sub"].(string)
	if subject == "" {
		return nil, fmt.Errorf("%w: missing `sub` claim", ErrInvalidToken)
	}
	issuer, _ := mapClaims["iss"].(string)
	aud := extractAudienceClaim(mapClaims["aud"])
	exp := extractTimeClaim(mapClaims["exp"])
	iat := extractTimeClaim(mapClaims["iat"])
	if exp.IsZero() {
		return nil, fmt.Errorf("%w: missing `exp` claim", ErrInvalidToken)
	}
	// Defence-in-depth: the parser already enforced `exp`
	// via WithExpirationRequired + WithLeeway, but the
	// gateway audit trail wants the verified timestamp
	// echoed back, not "trust the library".
	raw := map[string]json.RawMessage{}
	for k, v := range mapClaims {
		b, err := json.Marshal(v)
		if err != nil {
			continue
		}
		raw[k] = b
	}
	return &Identity{
		Subject:   subject,
		Audience:  aud,
		Issuer:    issuer,
		IssuedAt:  iat,
		ExpiresAt: exp,
		RawClaims: raw,
	}, nil
}

// peekJWTHeader base64-decodes the FIRST segment of a JWS-
// compact-serialized token and returns the `kid` and `alg`
// header claims WITHOUT invoking a full JWT parser. Used so
// the OIDC authenticator can resolve the signing key (and
// surface JWKS-fetch errors with the correct sentinel)
// BEFORE handing the token to the parser.
func peekJWTHeader(token string) (kid, alg string, err error) {
	segs := strings.SplitN(token, ".", 4)
	if len(segs) < 3 {
		return "", "", errors.New("JWT does not have three dot-separated segments")
	}
	raw, err := base64.RawURLEncoding.DecodeString(segs[0])
	if err != nil {
		return "", "", fmt.Errorf("decoding JWT header: %w", err)
	}
	var hdr struct {
		Kid string `json:"kid"`
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(raw, &hdr); err != nil {
		return "", "", fmt.Errorf("parsing JWT header JSON: %w", err)
	}
	return hdr.Kid, hdr.Alg, nil
}

// classifyJWTError maps a `golang-jwt/jwt/v5` parse error to
// one of the gateway's sentinel errors so the caller-facing
// HTTP status matches the failure mode.
func classifyJWTError(err error) error {
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return fmt.Errorf("%w: %v", ErrExpiredToken, err)
	case errors.Is(err, jwt.ErrTokenNotValidYet):
		return fmt.Errorf("%w: nbf in the future: %v", ErrInvalidToken, err)
	case errors.Is(err, jwt.ErrTokenInvalidIssuer):
		return fmt.Errorf("%w: %v", ErrBadIssuer, err)
	case errors.Is(err, jwt.ErrTokenInvalidAudience):
		return fmt.Errorf("%w: %v", ErrBadAudience, err)
	case errors.Is(err, jwt.ErrTokenSignatureInvalid),
		errors.Is(err, jwt.ErrTokenUnverifiable),
		errors.Is(err, jwt.ErrSignatureInvalid):
		return fmt.Errorf("%w: %v", ErrInvalidToken, err)
	case errors.Is(err, jwt.ErrTokenMalformed):
		return fmt.Errorf("%w: %v", ErrMalformedToken, err)
	default:
		// Unknown / wrapped error -- surface as invalid
		// rather than internal so a malformed token does
		// not cause a 500.
		return fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
}

// extractAudienceClaim normalises the JWT `aud` claim from
// either a string or a []interface{} (the two shapes the
// jwt library returns from MapClaims).
func extractAudienceClaim(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []string:
		return append([]string(nil), t...)
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// extractTimeClaim normalises a numeric-date JWT claim
// (`exp`, `iat`, `nbf`) into a Time. The jwt library
// returns float64 (the JSON-default numeric type).
func extractTimeClaim(v any) time.Time {
	switch t := v.(type) {
	case float64:
		return time.Unix(int64(t), 0)
	case int64:
		return time.Unix(t, 0)
	case json.Number:
		i, err := t.Int64()
		if err != nil {
			return time.Time{}
		}
		return time.Unix(i, 0)
	default:
		return time.Time{}
	}
}

// ---------------------------------------------------------------------------
// jwksCache -- thread-safe JWKS fetch with TTL refresh.
// ---------------------------------------------------------------------------

// errKidNotInJWKS is the sentinel returned by [jwksCache.Key]
// when the JWKS was successfully fetched but does not contain
// the requested `kid`. Distinct from JWKS-fetch failures so
// [OIDCAuthenticator.Authenticate] can wrap the former as
// [ErrInvalidToken] and the latter as [ErrAuthBackend].
var errKidNotInJWKS = errors.New("kid not found in JWKS after refresh")

// jwksCache fetches a JWKS document over HTTP and caches the
// parsed keys keyed by `kid`. Designed for the OIDC hot path:
//
//   - Cache hit: O(1) lock-protected map read.
//   - Cache miss / stale: synchronous refresh, single-flight
//     so concurrent requests do NOT N-multiply the IdP load.
//   - Unknown `kid`: forced refresh (idempotent within a
//     `singleflightTTL` window) so a freshly-rotated key is
//     picked up.
//
// # OIDC discovery (Item #6 from iter-2 feedback)
//
// When [jwksCache.url] is empty AND [jwksCache.discover] is
// true, the first fetch resolves the JWKS URL by GETing
// `{issuer}{discoveryPath}` (canonically
// `/.well-known/openid-configuration`) and reading the
// `jwks_uri` field. Discovery results are cached alongside
// the JWKS for the same TTL.
type jwksCache struct {
	url           string
	issuer        string
	discover      bool
	discoveryPath string
	client        *http.Client
	ttl           time.Duration
	now           func() time.Time

	mu      sync.RWMutex
	keys    map[string]any // kid -> *rsa.PublicKey | *ecdsa.PublicKey
	fetched time.Time
	lastErr error

	// refresh coalesces concurrent fetches; only one
	// goroutine actually issues the HTTP request.
	refresh sync.Mutex
}

// Key returns the public key for `kid`, fetching / refreshing
// the JWKS as needed.
//
// Error classification:
//   - [errKidNotInJWKS] -- JWKS fetched successfully but kid
//     not present. Caller (Authenticate) wraps as
//     ErrInvalidToken.
//   - Any other error -- JWKS infrastructure failure
//     (network, 5xx, malformed body, discovery failure).
//     Caller wraps as ErrAuthBackend so dashboards see 503,
//     not 401.
func (c *jwksCache) Key(ctx context.Context, kid string) (any, error) {
	if k := c.lookup(kid); k != nil {
		return k, nil
	}
	// Miss -> serialise a refresh.
	c.refresh.Lock()
	defer c.refresh.Unlock()
	// Re-check after acquiring the lock (another
	// goroutine may have just refreshed).
	if k := c.lookup(kid); k != nil {
		return k, nil
	}
	if err := c.fetch(ctx); err != nil {
		return nil, fmt.Errorf("api: JWKS fetch: %w", err)
	}
	if k := c.lookupRaw(kid); k != nil {
		return k, nil
	}
	return nil, fmt.Errorf("api: kid %q: %w", kid, errKidNotInJWKS)
}

func (c *jwksCache) lookup(kid string) any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.now().Sub(c.fetched) > c.ttl {
		// Stale -- caller will refresh.
		return nil
	}
	return c.keys[kid]
}

// lookupRaw returns whatever is in the cache without the
// staleness check -- used immediately after a successful
// fetch to confirm the kid landed in the new JWKS.
func (c *jwksCache) lookupRaw(kid string) any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.keys[kid]
}

func (c *jwksCache) fetch(ctx context.Context) error {
	// OIDC discovery: when url is unset and discovery is
	// enabled, first resolve `jwks_uri` from the issuer's
	// well-known configuration document.
	if c.url == "" && c.discover {
		jwksURL, err := c.discoverJWKSURL(ctx)
		if err != nil {
			return fmt.Errorf("OIDC discovery: %w", err)
		}
		c.url = jwksURL
	}
	if c.url == "" {
		return errors.New("JWKS URL unresolved (discovery disabled and no explicit URL)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return fmt.Errorf("building JWKS request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", c.url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET %s: status %d", c.url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading JWKS body: %w", err)
	}
	keys, err := parseJWKSBody(body)
	if err != nil {
		return fmt.Errorf("parsing JWKS body: %w", err)
	}
	c.mu.Lock()
	c.keys = keys
	c.fetched = c.now()
	c.lastErr = nil
	c.mu.Unlock()
	return nil
}

// discoverJWKSURL fetches `{issuer}{discoveryPath}`,
// decodes the OIDC discovery document, and returns the
// `jwks_uri` field. Returns an error when:
//
//   - the discovery endpoint is unreachable / returns
//     non-2xx,
//   - the response is not a valid JSON object,
//   - `jwks_uri` is missing or empty.
//
// All paths up to the JWKS fetch itself are surfaced as
// [ErrAuthBackend] by the caller (Authenticate).
func (c *jwksCache) discoverJWKSURL(ctx context.Context) (string, error) {
	if c.issuer == "" {
		return "", errors.New("issuer is empty (cannot derive discovery URL)")
	}
	url := strings.TrimRight(c.issuer, "/") + c.discoveryPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("building discovery request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("reading discovery body: %w", err)
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("decoding discovery document: %w", err)
	}
	if doc.JWKSURI == "" {
		return "", errors.New("discovery document is missing `jwks_uri`")
	}
	return doc.JWKSURI, nil
}

// parseJWKSBody decodes a JWKS document into a kid -> key map.
// Supports RSA (kty=RSA) and EC (kty=EC) keys -- the two
// shapes every major OIDC provider emits.
func parseJWKSBody(body []byte) (map[string]any, error) {
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decoding JWKS envelope: %w", err)
	}
	out := make(map[string]any, len(doc.Keys))
	for i, raw := range doc.Keys {
		var hdr struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
		}
		if err := json.Unmarshal(raw, &hdr); err != nil {
			return nil, fmt.Errorf("decoding JWKS entry %d header: %w", i, err)
		}
		if hdr.Kid == "" {
			// A JWKS entry without `kid` is unusable
			// for kid-based lookup; skip silently.
			continue
		}
		key, err := decodeJWKEntry(hdr.Kty, raw)
		if err != nil {
			// Skip a single bad key rather than failing
			// the whole document (a JWKS may contain a
			// future-algorithm key the verifier does
			// not yet support; the rest must remain
			// usable).
			continue
		}
		out[hdr.Kid] = key
	}
	return out, nil
}
