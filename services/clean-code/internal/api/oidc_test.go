package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// rsaTestKey is a deterministic 2048-bit RSA key generated
// lazily for the OIDC test fixtures. Reused across tests so
// each test does not pay the cost of generating a fresh
// key.
var rsaTestKey *rsa.PrivateKey

func init() {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("test setup: generating RSA key: " + err.Error())
	}
	rsaTestKey = k
}

// jwksHandler builds an http.Handler that serves the public
// half of `rsaTestKey` as a JWKS document with `kid=test-kid`.
// `fetches` is incremented on every request so a test can
// assert cache behaviour.
func jwksHandler(t *testing.T, fetches *int32) http.Handler {
	t.Helper()
	pub := rsaTestKey.Public().(*rsa.PublicKey)
	jwks := map[string]any{
		"keys": []map[string]any{
			{
				"kty": "RSA",
				"kid": "test-kid",
				"use": "sig",
				"alg": "RS256",
				"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			},
		},
	}
	body, err := json.Marshal(jwks)
	if err != nil {
		t.Fatalf("marshal JWKS: %v", err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fetches != nil {
			atomic.AddInt32(fetches, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
}

// signRS256TestToken issues a JWT signed by rsaTestKey with
// the given claims and the `kid=test-kid` header (so the
// JWKS cache resolves the key).
func signRS256TestToken(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = "test-kid"
	s, err := tok.SignedString(rsaTestKey)
	if err != nil {
		t.Fatalf("signing JWT: %v", err)
	}
	return s
}

func TestOIDCAuthenticator_ValidToken(t *testing.T) {
	t.Parallel()
	jwks := httptest.NewServer(jwksHandler(t, nil))
	defer jwks.Close()
	now := time.Now()
	auth, err := NewOIDCAuthenticator(OIDCAuthenticatorConfig{
		Issuer:   "https://idp.example",
		Audience: "https://gateway.example",
		JWKSURL:  jwks.URL,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewOIDCAuthenticator: %v", err)
	}
	token := signRS256TestToken(t, jwt.MapClaims{
		"iss": "https://idp.example",
		"aud": "https://gateway.example",
		"sub": "alice@example.com",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})
	id, err := auth.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Subject != "alice@example.com" {
		t.Errorf("Subject=%q, want alice@example.com", id.Subject)
	}
	if id.Issuer != "https://idp.example" {
		t.Errorf("Issuer=%q, want https://idp.example", id.Issuer)
	}
	if len(id.Audience) != 1 || id.Audience[0] != "https://gateway.example" {
		t.Errorf("Audience=%v, want [https://gateway.example]", id.Audience)
	}
}

func TestOIDCAuthenticator_BadAudienceReturns403Sentinel(t *testing.T) {
	t.Parallel()
	jwks := httptest.NewServer(jwksHandler(t, nil))
	defer jwks.Close()
	now := time.Now()
	auth, err := NewOIDCAuthenticator(OIDCAuthenticatorConfig{
		Issuer:   "https://idp.example",
		Audience: "https://gateway.example",
		JWKSURL:  jwks.URL,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewOIDCAuthenticator: %v", err)
	}
	token := signRS256TestToken(t, jwt.MapClaims{
		"iss": "https://idp.example",
		"aud": "https://wrong.example",
		"sub": "alice@example.com",
		"exp": now.Add(time.Hour).Unix(),
	})
	_, err = auth.Authenticate(context.Background(), token)
	if !errors.Is(err, ErrBadAudience) {
		t.Fatalf("err=%v, want wraps ErrBadAudience", err)
	}
}

func TestOIDCAuthenticator_BadIssuer(t *testing.T) {
	t.Parallel()
	jwks := httptest.NewServer(jwksHandler(t, nil))
	defer jwks.Close()
	now := time.Now()
	auth, _ := NewOIDCAuthenticator(OIDCAuthenticatorConfig{
		Issuer:   "https://idp.example",
		Audience: "https://gateway.example",
		JWKSURL:  jwks.URL,
		Now:      func() time.Time { return now },
	})
	token := signRS256TestToken(t, jwt.MapClaims{
		"iss": "https://attacker.example",
		"aud": "https://gateway.example",
		"sub": "alice",
		"exp": now.Add(time.Hour).Unix(),
	})
	_, err := auth.Authenticate(context.Background(), token)
	if !errors.Is(err, ErrBadIssuer) {
		t.Fatalf("err=%v, want wraps ErrBadIssuer", err)
	}
}

func TestOIDCAuthenticator_Expired(t *testing.T) {
	t.Parallel()
	jwks := httptest.NewServer(jwksHandler(t, nil))
	defer jwks.Close()
	now := time.Now()
	auth, _ := NewOIDCAuthenticator(OIDCAuthenticatorConfig{
		Issuer:   "https://idp.example",
		Audience: "https://gateway.example",
		JWKSURL:  jwks.URL,
		Now:      func() time.Time { return now },
		Leeway:   1 * time.Second,
	})
	token := signRS256TestToken(t, jwt.MapClaims{
		"iss": "https://idp.example",
		"aud": "https://gateway.example",
		"sub": "alice",
		"exp": now.Add(-time.Hour).Unix(),
	})
	_, err := auth.Authenticate(context.Background(), token)
	if !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("err=%v, want wraps ErrExpiredToken", err)
	}
}

// TestOIDCAuthenticator_NowOverrideThreaded asserts that
// cfg.Now is plumbed into the jwt parser via
// jwt.WithTimeFunc (item #4 from iter-3 feedback). Without
// the option, golang-jwt/v5 falls back to time.Now()
// internally and a deterministic Now override has no effect
// on exp/nbf validation.
//
// Strategy: sign a token that ALREADY EXPIRED relative to
// real wall-clock time (exp = real-now - 2h), but set
// cfg.Now to a fixed past timestamp where the same token is
// STILL VALID (cfg.Now = exp - 1h). Without WithTimeFunc
// the parser sees real-now > exp and rejects; with
// WithTimeFunc the parser sees cfg.Now < exp and accepts.
func TestOIDCAuthenticator_NowOverrideThreaded(t *testing.T) {
	t.Parallel()
	jwks := httptest.NewServer(jwksHandler(t, nil))
	defer jwks.Close()
	// Pick a fixed past timestamp far enough back that
	// real-now is comfortably outside any leeway.
	frozenNow := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	auth, err := NewOIDCAuthenticator(OIDCAuthenticatorConfig{
		Issuer:   "https://idp.example",
		Audience: "https://gateway.example",
		JWKSURL:  jwks.URL,
		Now:      func() time.Time { return frozenNow },
	})
	if err != nil {
		t.Fatalf("NewOIDCAuthenticator: %v", err)
	}
	// Token issued at frozenNow, expires 1h later. Real
	// wall-clock time is well past 2024-01-01T13:00:00Z, so
	// without WithTimeFunc(cfg.Now) the parser would reject
	// this as expired.
	token := signRS256TestToken(t, jwt.MapClaims{
		"iss": "https://idp.example",
		"aud": "https://gateway.example",
		"sub": "alice",
		"iat": frozenNow.Unix(),
		"exp": frozenNow.Add(time.Hour).Unix(),
		"nbf": frozenNow.Unix(),
	})
	id, err := auth.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate with frozen Now: %v (cfg.Now is NOT threaded into jwt parser)", err)
	}
	if id.Subject != "alice" {
		t.Errorf("Subject=%q, want alice", id.Subject)
	}
}

// TestOIDCAuthenticator_NowOverrideRejectsExpiredAtFrozenNow
// asserts the inverse: with cfg.Now threaded, a token whose
// exp predates the frozen Now is still rejected as expired.
// Pinned so a future regression that drops WithTimeFunc and
// happens to use real-now (where the token might still be
// valid in some test runs) is caught.
func TestOIDCAuthenticator_NowOverrideRejectsExpiredAtFrozenNow(t *testing.T) {
	t.Parallel()
	jwks := httptest.NewServer(jwksHandler(t, nil))
	defer jwks.Close()
	frozenNow := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	auth, _ := NewOIDCAuthenticator(OIDCAuthenticatorConfig{
		Issuer:   "https://idp.example",
		Audience: "https://gateway.example",
		JWKSURL:  jwks.URL,
		Now:      func() time.Time { return frozenNow },
		Leeway:   1 * time.Second,
	})
	// Token expired 1 minute before frozenNow.
	token := signRS256TestToken(t, jwt.MapClaims{
		"iss": "https://idp.example",
		"aud": "https://gateway.example",
		"sub": "alice",
		"exp": frozenNow.Add(-time.Minute).Unix(),
	})
	_, err := auth.Authenticate(context.Background(), token)
	if !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("err=%v, want ErrExpiredToken evaluated against cfg.Now", err)
	}
}

func TestOIDCAuthenticator_AlgNoneRejected(t *testing.T) {
	t.Parallel()
	jwks := httptest.NewServer(jwksHandler(t, nil))
	defer jwks.Close()
	now := time.Now()
	auth, _ := NewOIDCAuthenticator(OIDCAuthenticatorConfig{
		Issuer:   "https://idp.example",
		Audience: "https://gateway.example",
		JWKSURL:  jwks.URL,
		Now:      func() time.Time { return now },
	})
	// Hand-craft an alg=none token.
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT","kid":"test-kid"}`))
	claims := fmt.Sprintf(`{"iss":"https://idp.example","aud":"https://gateway.example","sub":"alice","exp":%d}`, now.Add(time.Hour).Unix())
	payload := base64.RawURLEncoding.EncodeToString([]byte(claims))
	token := hdr + "." + payload + "."
	_, err := auth.Authenticate(context.Background(), token)
	if err == nil {
		t.Fatalf("alg=none token unexpectedly accepted")
	}
	// The library may classify as Unverifiable or
	// SignatureInvalid; either maps to ErrInvalidToken.
	if !errors.Is(err, ErrInvalidToken) && !errors.Is(err, ErrMalformedToken) {
		t.Errorf("err=%v, want wraps ErrInvalidToken or ErrMalformedToken", err)
	}
}

func TestOIDCAuthenticator_HS256Rejected(t *testing.T) {
	t.Parallel()
	// A symmetric HS256 token MUST NOT verify against the
	// production OIDC authenticator -- per OIDC core a
	// public JWT must use an asymmetric algorithm.
	jwks := httptest.NewServer(jwksHandler(t, nil))
	defer jwks.Close()
	now := time.Now()
	auth, _ := NewOIDCAuthenticator(OIDCAuthenticatorConfig{
		Issuer:   "https://idp.example",
		Audience: "https://gateway.example",
		JWKSURL:  jwks.URL,
		Now:      func() time.Time { return now },
	})
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": "https://idp.example",
		"aud": "https://gateway.example",
		"sub": "alice",
		"exp": now.Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = "test-kid"
	signed, _ := tok.SignedString([]byte("shared-secret"))
	_, err := auth.Authenticate(context.Background(), signed)
	if err == nil {
		t.Fatalf("HS256 token unexpectedly accepted by production OIDC verifier")
	}
}

func TestOIDCAuthenticator_KIDMissingRejected(t *testing.T) {
	t.Parallel()
	jwks := httptest.NewServer(jwksHandler(t, nil))
	defer jwks.Close()
	now := time.Now()
	auth, _ := NewOIDCAuthenticator(OIDCAuthenticatorConfig{
		Issuer:   "https://idp.example",
		Audience: "https://gateway.example",
		JWKSURL:  jwks.URL,
		Now:      func() time.Time { return now },
	})
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": "https://idp.example",
		"aud": "https://gateway.example",
		"sub": "alice",
		"exp": now.Add(time.Hour).Unix(),
	})
	// Deliberately do NOT set tok.Header["kid"].
	signed, _ := tok.SignedString(rsaTestKey)
	_, err := auth.Authenticate(context.Background(), signed)
	if err == nil {
		t.Fatalf("token without kid unexpectedly accepted")
	}
}

func TestOIDCAuthenticator_UnknownKIDTriggersRefresh(t *testing.T) {
	t.Parallel()
	// First request fetches JWKS, second request with a
	// known kid hits the cache (no extra fetch), third
	// request with an unknown kid triggers a refresh.
	var fetches int32
	jwks := httptest.NewServer(jwksHandler(t, &fetches))
	defer jwks.Close()
	now := time.Now()
	auth, _ := NewOIDCAuthenticator(OIDCAuthenticatorConfig{
		Issuer:   "https://idp.example",
		Audience: "https://gateway.example",
		JWKSURL:  jwks.URL,
		Now:      func() time.Time { return now },
	})
	good := signRS256TestToken(t, jwt.MapClaims{
		"iss": "https://idp.example",
		"aud": "https://gateway.example",
		"sub": "alice",
		"exp": now.Add(time.Hour).Unix(),
	})
	if _, err := auth.Authenticate(context.Background(), good); err != nil {
		t.Fatalf("first auth: %v", err)
	}
	if got := atomic.LoadInt32(&fetches); got != 1 {
		t.Errorf("after first auth fetches=%d, want 1", got)
	}
	if _, err := auth.Authenticate(context.Background(), good); err != nil {
		t.Fatalf("second auth: %v", err)
	}
	if got := atomic.LoadInt32(&fetches); got != 1 {
		t.Errorf("after second auth fetches=%d, want 1 (cache hit)", got)
	}
	// Forge a token with an unknown kid.
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": "https://idp.example",
		"aud": "https://gateway.example",
		"sub": "alice",
		"exp": now.Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = "unknown-kid"
	signed, _ := tok.SignedString(rsaTestKey)
	if _, err := auth.Authenticate(context.Background(), signed); err == nil {
		t.Fatalf("unknown-kid token unexpectedly accepted")
	}
	if got := atomic.LoadInt32(&fetches); got != 2 {
		t.Errorf("after unknown-kid auth fetches=%d, want 2 (refresh triggered)", got)
	}
}

func TestOIDCAuthenticator_ConstructorValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  OIDCAuthenticatorConfig
	}{
		{"missing issuer", OIDCAuthenticatorConfig{Audience: "x"}},
		{"missing audience", OIDCAuthenticatorConfig{Issuer: "x"}},
		{"alg=none in accepted", OIDCAuthenticatorConfig{Issuer: "x", Audience: "y", AcceptedAlgorithms: []string{"RS256", "none"}}},
		{"empty alg in accepted", OIDCAuthenticatorConfig{Issuer: "x", Audience: "y", AcceptedAlgorithms: []string{""}}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewOIDCAuthenticator(c.cfg)
			if err == nil {
				t.Fatalf("expected constructor error for %s", c.name)
			}
		})
	}
}

func TestDecodeJWKEntry_UnknownKtyRejected(t *testing.T) {
	t.Parallel()
	_, err := decodeJWKEntry("OKP", json.RawMessage(`{"crv":"Ed25519","x":""}`))
	if err == nil {
		t.Fatalf("OKP key unexpectedly accepted")
	}
}

func TestParseJWKSBody_SkipsBadEntries(t *testing.T) {
	t.Parallel()
	pub := rsaTestKey.Public().(*rsa.PublicKey)
	body := fmt.Sprintf(`{"keys":[
        {"kty":"RSA","kid":"good","n":%q,"e":%q},
        {"kty":"FOO","kid":"bad"},
        {"kty":"RSA","n":"AA","e":"AA"}
    ]}`,
		base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	)
	keys, err := parseJWKSBody([]byte(body))
	if err != nil {
		t.Fatalf("parseJWKSBody: %v", err)
	}
	if _, ok := keys["good"]; !ok {
		t.Errorf("good key missing")
	}
	if _, ok := keys["bad"]; ok {
		t.Errorf("bad-kty key surfaced")
	}
	if len(keys) != 1 {
		t.Errorf("len(keys)=%d, want 1", len(keys))
	}
}

func TestRSAJWKDecode_RoundTrip(t *testing.T) {
	t.Parallel()
	pub := rsaTestKey.Public().(*rsa.PublicKey)
	raw, _ := json.Marshal(map[string]string{
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	})
	decoded, err := decodeRSAJWK(raw)
	if err != nil {
		t.Fatalf("decodeRSAJWK: %v", err)
	}
	if decoded.N.Cmp(pub.N) != 0 || decoded.E != pub.E {
		t.Errorf("RSA round-trip mismatch")
	}
}

// silence the SHA helper warning while keeping the import
// chain explicit; sha256 is in the stdlib of the OIDC path.
var _ = sha256.New

// ---------------------------------------------------------------------------
// Item #5 from iter-2 feedback: JWKS / IdP infrastructure
// failures must be reported as ErrAuthBackend (the gateway
// maps this to 503 -- not 401), so SREs distinguish "the IdP
// is down" from "a caller's token is invalid".
// ---------------------------------------------------------------------------

func TestOIDCAuthenticator_JWKS500_ReturnsErrAuthBackend(t *testing.T) {
	t.Parallel()
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer jwks.Close()
	now := time.Now()
	auth, err := NewOIDCAuthenticator(OIDCAuthenticatorConfig{
		Issuer:   "https://idp.example",
		Audience: "https://gateway.example",
		JWKSURL:  jwks.URL,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewOIDCAuthenticator: %v", err)
	}
	token := signRS256TestToken(t, jwt.MapClaims{
		"iss": "https://idp.example",
		"aud": "https://gateway.example",
		"sub": "alice@example.com",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})
	_, err = auth.Authenticate(context.Background(), token)
	if err == nil {
		t.Fatalf("expected ErrAuthBackend, got nil")
	}
	if !errors.Is(err, ErrAuthBackend) {
		t.Errorf("err=%v, want errors.Is ErrAuthBackend (got class would map to 401, not 503)", err)
	}
	if errors.Is(err, ErrInvalidToken) {
		t.Errorf("err must NOT classify as ErrInvalidToken -- that masks IdP outage as caller error")
	}
}

func TestOIDCAuthenticator_JWKSNetworkError_ReturnsErrAuthBackend(t *testing.T) {
	t.Parallel()
	// Bind to a port that we immediately close so the dial
	// fails -- emulates a JWKS endpoint that is unreachable.
	jwks := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	jwks.Close() // close before any auth happens
	now := time.Now()
	auth, err := NewOIDCAuthenticator(OIDCAuthenticatorConfig{
		Issuer:   "https://idp.example",
		Audience: "https://gateway.example",
		JWKSURL:  jwks.URL,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewOIDCAuthenticator: %v", err)
	}
	token := signRS256TestToken(t, jwt.MapClaims{
		"iss": "https://idp.example",
		"aud": "https://gateway.example",
		"sub": "alice@example.com",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})
	_, err = auth.Authenticate(context.Background(), token)
	if err == nil {
		t.Fatalf("expected ErrAuthBackend on network failure")
	}
	if !errors.Is(err, ErrAuthBackend) {
		t.Errorf("err=%v, want ErrAuthBackend", err)
	}
}

func TestOIDCAuthenticator_KIDNotInJWKS_ReturnsInvalidTokenNotBackend(t *testing.T) {
	t.Parallel()
	// Distinguish from item #5: a successfully-served JWKS
	// that genuinely does not contain the kid is the
	// caller's problem (their token references a key the
	// IdP never issued) -- ErrInvalidToken, NOT ErrAuthBackend.
	var fetches int32
	jwks := httptest.NewServer(jwksHandler(t, &fetches))
	defer jwks.Close()
	now := time.Now()
	auth, err := NewOIDCAuthenticator(OIDCAuthenticatorConfig{
		Issuer:   "https://idp.example",
		Audience: "https://gateway.example",
		JWKSURL:  jwks.URL,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewOIDCAuthenticator: %v", err)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": "https://idp.example",
		"aud": "https://gateway.example",
		"sub": "alice@example.com",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = "wrong-kid"
	signed, _ := tok.SignedString(rsaTestKey)
	_, err = auth.Authenticate(context.Background(), signed)
	if err == nil {
		t.Fatalf("wrong-kid token unexpectedly accepted")
	}
	if errors.Is(err, ErrAuthBackend) {
		t.Errorf("kid-not-in-JWKS must NOT classify as ErrAuthBackend (the JWKS endpoint is healthy)")
	}
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("err=%v, want ErrInvalidToken", err)
	}
}

// ---------------------------------------------------------------------------
// Item #6 from iter-2 feedback: OIDC discovery.
// When JWKSURL is unset, the authenticator must resolve
// `jwks_uri` from {Issuer}/.well-known/openid-configuration.
// ---------------------------------------------------------------------------

// discoveryServer returns an httptest server that serves
// BOTH `/.well-known/openid-configuration` and a JWKS
// endpoint at `/jwks.json`. The discovery doc points at
// the JWKS path on the same server.
func discoveryServer(t *testing.T, jwksFetches *int32) *httptest.Server {
	t.Helper()
	pub := rsaTestKey.Public().(*rsa.PublicKey)
	jwksBody, err := json.Marshal(map[string]any{
		"keys": []map[string]any{
			{
				"kty": "RSA",
				"kid": "test-kid",
				"use": "sig",
				"alg": "RS256",
				"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal JWKS: %v", err)
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			doc := map[string]any{
				"issuer":   srv.URL,
				"jwks_uri": srv.URL + "/jwks.json",
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(doc)
		case "/jwks.json":
			if jwksFetches != nil {
				atomic.AddInt32(jwksFetches, 1)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(jwksBody)
		default:
			http.NotFound(w, r)
		}
	}))
	return srv
}

func TestOIDCAuthenticator_Discovery_ResolvesJWKSURI(t *testing.T) {
	t.Parallel()
	var fetches int32
	srv := discoveryServer(t, &fetches)
	defer srv.Close()
	now := time.Now()
	auth, err := NewOIDCAuthenticator(OIDCAuthenticatorConfig{
		Issuer:   srv.URL,
		Audience: "https://gateway.example",
		// JWKSURL deliberately empty -- triggers discovery.
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewOIDCAuthenticator: %v", err)
	}
	token := signRS256TestToken(t, jwt.MapClaims{
		"iss": srv.URL,
		"aud": "https://gateway.example",
		"sub": "alice@example.com",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})
	id, err := auth.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate via discovery: %v", err)
	}
	if id.Subject != "alice@example.com" {
		t.Errorf("Subject=%q, want alice@example.com", id.Subject)
	}
	if got := atomic.LoadInt32(&fetches); got != 1 {
		t.Errorf("JWKS fetched %d times, want exactly 1 (discovery should not double-fetch)", got)
	}
}

func TestOIDCAuthenticator_Discovery_MissingJWKSURI_ReturnsAuthBackend(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/openid-configuration" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"issuer":"x"}`)) // no jwks_uri
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	now := time.Now()
	auth, err := NewOIDCAuthenticator(OIDCAuthenticatorConfig{
		Issuer:   srv.URL,
		Audience: "https://gateway.example",
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewOIDCAuthenticator: %v", err)
	}
	token := signRS256TestToken(t, jwt.MapClaims{
		"iss": srv.URL,
		"aud": "https://gateway.example",
		"sub": "alice@example.com",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})
	_, err = auth.Authenticate(context.Background(), token)
	if err == nil {
		t.Fatalf("discovery without jwks_uri should fail")
	}
	if !errors.Is(err, ErrAuthBackend) {
		t.Errorf("err=%v, want ErrAuthBackend (discovery failure is infra, not invalid token)", err)
	}
}

func TestOIDCAuthenticator_Discovery_404_ReturnsAuthBackend(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // discovery endpoint absent
	}))
	defer srv.Close()
	now := time.Now()
	auth, err := NewOIDCAuthenticator(OIDCAuthenticatorConfig{
		Issuer:   srv.URL,
		Audience: "https://gateway.example",
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewOIDCAuthenticator: %v", err)
	}
	token := signRS256TestToken(t, jwt.MapClaims{
		"iss": srv.URL,
		"aud": "https://gateway.example",
		"sub": "alice@example.com",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})
	_, err = auth.Authenticate(context.Background(), token)
	if err == nil {
		t.Fatalf("discovery 404 should fail")
	}
	if !errors.Is(err, ErrAuthBackend) {
		t.Errorf("err=%v, want ErrAuthBackend", err)
	}
}

func TestPeekJWTHeader_ExtractsKidAndAlg(t *testing.T) {
	t.Parallel()
	token := signRS256TestToken(t, jwt.MapClaims{
		"iss": "x", "aud": "y", "sub": "z",
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	kid, alg, err := peekJWTHeader(token)
	if err != nil {
		t.Fatalf("peekJWTHeader: %v", err)
	}
	if kid != "test-kid" {
		t.Errorf("kid=%q, want test-kid", kid)
	}
	if alg != "RS256" {
		t.Errorf("alg=%q, want RS256", alg)
	}
}

func TestPeekJWTHeader_MalformedReturnsError(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",                            // empty
		"only-one-segment",            // no dots
		"two.segments",                // missing one
		"!!!notbase64!!!.x.y",         // base64 corruption
		base64.RawURLEncoding.EncodeToString([]byte("not-json")) + ".x.y", // header not JSON
	}
	for _, c := range cases {
		if _, _, err := peekJWTHeader(c); err == nil {
			t.Errorf("peekJWTHeader(%q) unexpectedly succeeded", c)
		}
	}
}
