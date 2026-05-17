package mgmtapi

// Unit tests for the OIDC verifier. Builds a complete RSA
// signing path (key generation, JWT compose, JWKS document
// served via httptest.Server) so the verifier exercises real
// signature + claim validation against test-process-local
// cryptographic input.

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testIssuer   = "https://idp.example.test/realms/agent-memory"
	testAudience = "agent-memory-mgmt-api"
)

// jwtTestKey wraps the RSA private key plus its JWK
// serialisation (n / e base64url).
type jwtTestKey struct {
	kid   string
	alg   string // "RS256" / "RS384" / "RS512"
	priv  *rsa.PrivateKey
	jwksN string
	jwksE string
}

func newTestKey(t *testing.T, kid, alg string) *jwtTestKey {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	eBytes := []byte{}
	for e := priv.E; e > 0; e >>= 8 {
		eBytes = append([]byte{byte(e & 0xff)}, eBytes...)
	}
	return &jwtTestKey{
		kid:   kid,
		alg:   alg,
		priv:  priv,
		jwksN: base64.RawURLEncoding.EncodeToString(priv.N.Bytes()),
		jwksE: base64.RawURLEncoding.EncodeToString(eBytes),
	}
}

// jwksDocument builds the JSON body a JWKS endpoint would
// serve for the supplied keys.
func jwksDocument(keys ...*jwtTestKey) []byte {
	entries := make([]map[string]string, 0, len(keys))
	for _, k := range keys {
		entries = append(entries, map[string]string{
			"kid": k.kid,
			"kty": "RSA",
			"alg": k.alg,
			"use": "sig",
			"n":   k.jwksN,
			"e":   k.jwksE,
		})
	}
	body, _ := json.Marshal(map[string]any{"keys": entries})
	return body
}

func hashForAlg(t *testing.T, alg string) crypto.Hash {
	t.Helper()
	switch alg {
	case "RS256":
		return crypto.SHA256
	case "RS384":
		return crypto.SHA384
	case "RS512":
		return crypto.SHA512
	default:
		t.Fatalf("hashForAlg: unsupported alg %q", alg)
		return 0
	}
}

func digestForAlg(t *testing.T, alg string, data []byte) []byte {
	t.Helper()
	switch alg {
	case "RS256":
		d := sha256.Sum256(data)
		return d[:]
	case "RS384":
		d := sha512.Sum384(data)
		return d[:]
	case "RS512":
		d := sha512.Sum512(data)
		return d[:]
	default:
		t.Fatalf("digestForAlg: unsupported alg %q", alg)
		return nil
	}
}

// signJWT composes a compact JWS for the supplied claims.
// `header` is merged with the default `{alg, typ:JWT, kid}`
// triple; pass a non-nil header to override (e.g. to inject
// `alg:none`).
func signJWT(t *testing.T, key *jwtTestKey, headerOverride map[string]any, claims map[string]any) string {
	t.Helper()
	header := map[string]any{
		"alg": key.alg,
		"typ": "JWT",
		"kid": key.kid,
	}
	for k, v := range headerOverride {
		header[k] = v
	}
	return composeJWT(t, key.priv, key.alg, header, claims)
}

// composeJWT signs a JWT with full control over the header.
// Used by tests that want an exact header shape (e.g.
// `alg:none`, missing kid, wrong alg vs key).
func composeJWT(t *testing.T, priv *rsa.PrivateKey, signAlg string, header, claims map[string]any) string {
	t.Helper()
	hb, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	pb, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	h64 := base64.RawURLEncoding.EncodeToString(hb)
	p64 := base64.RawURLEncoding.EncodeToString(pb)
	signingInput := h64 + "." + p64
	if signAlg == "" || signAlg == "none" {
		// Tests that want an unsigned token append an empty
		// signature segment.
		return signingInput + "."
	}
	digest := digestForAlg(t, signAlg, []byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, hashForAlg(t, signAlg), digest)
	if err != nil {
		t.Fatalf("rsa.SignPKCS1v15: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// runJWKSServer spins up an httptest server that serves
// `body` as the JWKS document. `hits`, if non-nil, increments
// per request.
func runJWKSServer(t *testing.T, body []byte, hits *int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits != nil {
			atomic.AddInt64(hits, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// validClaims returns a JWT claim set with iss / aud / exp /
// nbf / iat / sub all set so a baseline verify succeeds.
// Tests mutate the returned map to exercise specific failures.
func validClaims() map[string]any {
	now := time.Now().Unix()
	return map[string]any{
		"iss": testIssuer,
		"aud": testAudience,
		"sub": "operator-42",
		"iat": now,
		"nbf": now - 1,
		"exp": now + 600,
	}
}

func TestOIDCVerifier_validRS256Token_returnsSubject(t *testing.T) {
	t.Parallel()
	key := newTestKey(t, "kid-1", "RS256")
	srv := runJWKSServer(t, jwksDocument(key), nil)
	v, err := newOIDCVerifierInsecure(testIssuer, testAudience, srv.URL)
	if err != nil {
		t.Fatalf("NewOIDCVerifier: %v", err)
	}

	tok := signJWT(t, key, nil, validClaims())
	subj, err := v.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if subj != "operator-42" {
		t.Errorf("subject = %q, want operator-42", subj)
	}
}

func TestOIDCVerifier_validRS384Token(t *testing.T) {
	t.Parallel()
	key := newTestKey(t, "kid-1", "RS384")
	srv := runJWKSServer(t, jwksDocument(key), nil)
	v, _ := newOIDCVerifierInsecure(testIssuer, testAudience, srv.URL)

	tok := signJWT(t, key, nil, validClaims())
	if _, err := v.Verify(context.Background(), tok); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestOIDCVerifier_validRS512Token(t *testing.T) {
	t.Parallel()
	key := newTestKey(t, "kid-1", "RS512")
	srv := runJWKSServer(t, jwksDocument(key), nil)
	v, _ := newOIDCVerifierInsecure(testIssuer, testAudience, srv.URL)

	tok := signJWT(t, key, nil, validClaims())
	if _, err := v.Verify(context.Background(), tok); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestOIDCVerifier_rejectsAlgNone(t *testing.T) {
	t.Parallel()
	key := newTestKey(t, "kid-1", "RS256")
	srv := runJWKSServer(t, jwksDocument(key), nil)
	v, _ := newOIDCVerifierInsecure(testIssuer, testAudience, srv.URL)

	tok := composeJWT(t, key.priv, "none",
		map[string]any{"alg": "none", "typ": "JWT", "kid": key.kid},
		validClaims())

	_, err := v.Verify(context.Background(), tok)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
	if !strings.Contains(err.Error(), "alg") {
		t.Errorf("err = %v, want substring 'alg'", err)
	}
}

func TestOIDCVerifier_rejectsHS256(t *testing.T) {
	t.Parallel()
	// `alg:HS256` is the canonical "key confusion" attack —
	// a server that accepts HS256 with the public key as
	// the HMAC key is exploitable. The verifier must reject
	// before reading the signature.
	key := newTestKey(t, "kid-1", "RS256")
	srv := runJWKSServer(t, jwksDocument(key), nil)
	v, _ := newOIDCVerifierInsecure(testIssuer, testAudience, srv.URL)

	hb, _ := json.Marshal(map[string]any{"alg": "HS256", "typ": "JWT", "kid": key.kid})
	pb, _ := json.Marshal(validClaims())
	tok := base64.RawURLEncoding.EncodeToString(hb) + "." +
		base64.RawURLEncoding.EncodeToString(pb) + ".garbage-sig"

	_, err := v.Verify(context.Background(), tok)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}

func TestOIDCVerifier_rejectsWrongIssuer(t *testing.T) {
	t.Parallel()
	key := newTestKey(t, "kid-1", "RS256")
	srv := runJWKSServer(t, jwksDocument(key), nil)
	v, _ := newOIDCVerifierInsecure(testIssuer, testAudience, srv.URL)

	c := validClaims()
	c["iss"] = "https://attacker.example/realms/x"
	tok := signJWT(t, key, nil, c)

	_, err := v.Verify(context.Background(), tok)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
	if !strings.Contains(err.Error(), "iss") {
		t.Errorf("err = %v, want substring 'iss'", err)
	}
}

func TestOIDCVerifier_rejectsWrongAudience(t *testing.T) {
	t.Parallel()
	key := newTestKey(t, "kid-1", "RS256")
	srv := runJWKSServer(t, jwksDocument(key), nil)
	v, _ := newOIDCVerifierInsecure(testIssuer, testAudience, srv.URL)

	c := validClaims()
	c["aud"] = "some-other-api"
	tok := signJWT(t, key, nil, c)

	_, err := v.Verify(context.Background(), tok)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}

func TestOIDCVerifier_acceptsArrayAudience(t *testing.T) {
	t.Parallel()
	key := newTestKey(t, "kid-1", "RS256")
	srv := runJWKSServer(t, jwksDocument(key), nil)
	v, _ := newOIDCVerifierInsecure(testIssuer, testAudience, srv.URL)

	c := validClaims()
	c["aud"] = []string{"other", testAudience, "another"}
	tok := signJWT(t, key, nil, c)

	if _, err := v.Verify(context.Background(), tok); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestOIDCVerifier_rejectsExpired(t *testing.T) {
	t.Parallel()
	key := newTestKey(t, "kid-1", "RS256")
	srv := runJWKSServer(t, jwksDocument(key), nil)
	v, _ := newOIDCVerifierInsecure(testIssuer, testAudience, srv.URL)

	c := validClaims()
	c["exp"] = time.Now().Add(-2 * time.Hour).Unix()
	tok := signJWT(t, key, nil, c)

	_, err := v.Verify(context.Background(), tok)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("err = %v, want substring 'expired'", err)
	}
}

func TestOIDCVerifier_rejectsNbfFuture(t *testing.T) {
	t.Parallel()
	key := newTestKey(t, "kid-1", "RS256")
	srv := runJWKSServer(t, jwksDocument(key), nil)
	v, _ := newOIDCVerifierInsecure(testIssuer, testAudience, srv.URL)

	c := validClaims()
	c["nbf"] = time.Now().Add(1 * time.Hour).Unix()
	tok := signJWT(t, key, nil, c)

	_, err := v.Verify(context.Background(), tok)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}

func TestOIDCVerifier_rejectsBadSignature(t *testing.T) {
	t.Parallel()
	key := newTestKey(t, "kid-1", "RS256")
	srv := runJWKSServer(t, jwksDocument(key), nil)
	v, _ := newOIDCVerifierInsecure(testIssuer, testAudience, srv.URL)

	tok := signJWT(t, key, nil, validClaims())
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token parts = %d", len(parts))
	}
	rawSig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	rawSig[0] ^= 0xff
	parts[2] = base64.RawURLEncoding.EncodeToString(rawSig)
	tampered := strings.Join(parts, ".")

	_, err := v.Verify(context.Background(), tampered)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}

func TestOIDCVerifier_rejectsUnknownKid(t *testing.T) {
	t.Parallel()
	signingKey := newTestKey(t, "actual-kid", "RS256")
	jwksKey := newTestKey(t, "other-kid", "RS256")
	srv := runJWKSServer(t, jwksDocument(jwksKey), nil)
	v, _ := newOIDCVerifierInsecure(testIssuer, testAudience, srv.URL)

	tok := signJWT(t, signingKey, nil, validClaims())

	_, err := v.Verify(context.Background(), tok)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
	if !strings.Contains(err.Error(), "kid") {
		t.Errorf("err = %v, want substring 'kid'", err)
	}
}

func TestOIDCVerifier_rejectsMissingKidHeader(t *testing.T) {
	t.Parallel()
	key := newTestKey(t, "kid-1", "RS256")
	srv := runJWKSServer(t, jwksDocument(key), nil)
	v, _ := newOIDCVerifierInsecure(testIssuer, testAudience, srv.URL)

	// Compose with NO kid claim in the header.
	tok := composeJWT(t, key.priv, "RS256",
		map[string]any{"alg": "RS256", "typ": "JWT"},
		validClaims())

	_, err := v.Verify(context.Background(), tok)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
	if !strings.Contains(err.Error(), "kid") {
		t.Errorf("err = %v, want substring 'kid'", err)
	}
}

func TestOIDCVerifier_jwksOutage_returns503Sentinel(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "outage", http.StatusInternalServerError)
	}))
	defer srv.Close()
	v, _ := newOIDCVerifierInsecure(testIssuer, testAudience, srv.URL)
	key := newTestKey(t, "kid-1", "RS256")
	tok := signJWT(t, key, nil, validClaims())

	_, err := v.Verify(context.Background(), tok)
	if !errors.Is(err, ErrVerifierUnavailable) {
		t.Fatalf("err = %v, want ErrVerifierUnavailable", err)
	}
}

func TestOIDCVerifier_thunderingHerd_collapseToOneFetch(t *testing.T) {
	t.Parallel()
	key := newTestKey(t, "kid-1", "RS256")
	var hits int64
	srv := runJWKSServer(t, jwksDocument(key), &hits)
	v, _ := newOIDCVerifierInsecure(testIssuer, testAudience, srv.URL)
	tok := signJWT(t, key, nil, validClaims())

	// First call primes the cache.
	if _, err := v.Verify(context.Background(), tok); err != nil {
		t.Fatalf("Verify (priming): %v", err)
	}
	primedHits := atomic.LoadInt64(&hits)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := v.Verify(context.Background(), tok); err != nil {
				t.Errorf("Verify (concurrent): %v", err)
			}
		}()
	}
	wg.Wait()
	final := atomic.LoadInt64(&hits)
	if final != primedHits {
		t.Errorf("hits = %d, want %d (cache should serve subsequent calls)", final, primedHits)
	}
}

func TestOIDCVerifier_missingToken_returnsErrTokenMissing(t *testing.T) {
	t.Parallel()
	v, _ := newOIDCVerifierInsecure(testIssuer, testAudience, "http://localhost:9/jwks")
	if _, err := v.Verify(context.Background(), ""); !errors.Is(err, ErrTokenMissing) {
		t.Fatalf("err = %v, want ErrTokenMissing", err)
	}
}

func TestOIDCVerifier_rejectsMissingSubject(t *testing.T) {
	t.Parallel()
	key := newTestKey(t, "kid-1", "RS256")
	srv := runJWKSServer(t, jwksDocument(key), nil)
	v, _ := newOIDCVerifierInsecure(testIssuer, testAudience, srv.URL)

	c := validClaims()
	delete(c, "sub")
	tok := signJWT(t, key, nil, c)

	_, err := v.Verify(context.Background(), tok)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}

func TestOIDCVerifier_rejectsMissingAudience(t *testing.T) {
	t.Parallel()
	key := newTestKey(t, "kid-1", "RS256")
	srv := runJWKSServer(t, jwksDocument(key), nil)
	v, _ := newOIDCVerifierInsecure(testIssuer, testAudience, srv.URL)

	c := validClaims()
	delete(c, "aud")
	tok := signJWT(t, key, nil, c)

	_, err := v.Verify(context.Background(), tok)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}

func TestOIDCVerifier_rejectsMalformedToken(t *testing.T) {
	t.Parallel()
	v, _ := newOIDCVerifierInsecure(testIssuer, testAudience, "http://localhost:9/jwks")
	cases := []string{
		"not.a.jwt.too.many.parts",
		"only-two.parts",
		"one-part",
		"...",
		"not_base64!!.payload.sig",
	}
	for _, c := range cases {
		_, err := v.Verify(context.Background(), c)
		if !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("err for %q = %v, want ErrTokenInvalid", c, err)
		}
	}
}

func TestNewOIDCVerifier_failClosedOnEmptyConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		iss         string
		aud         string
		jwks        string
		wantSubstr  string
	}{
		{"empty_issuer", "", testAudience, "https://x/jwks", "Issuer"},
		{"empty_audience", testIssuer, "", "https://x/jwks", "Audience"},
		{"empty_jwks", testIssuer, testAudience, "", "JWKSURL"},
		{"whitespace_issuer", "   ", testAudience, "https://x/jwks", "Issuer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewOIDCVerifier(tc.iss, tc.aud, tc.jwks)
			if err == nil {
				t.Fatalf("NewOIDCVerifier returned nil error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("err = %v, want substring %q", err, tc.wantSubstr)
			}
		})
	}
}

// TestNewOIDCVerifier_requiresHTTPSScheme -- the production
// constructor MUST reject a JWKS URL served over plaintext.
// A plaintext JWKS endpoint lets a network-positioned attacker
// substitute signing keys and forge tokens; the operator
// MUST front the IdP with TLS before booting the API.
func TestNewOIDCVerifier_requiresHTTPSScheme(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		url  string
	}{
		{"http", "http://issuer.example.com/.well-known/jwks.json"},
		{"file", "file:///etc/jwks.json"},
		{"ftp", "ftp://example.com/jwks.json"},
		{"no_scheme", "issuer.example.com/jwks"},
		{"empty_host", "https:///jwks"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewOIDCVerifier(testIssuer, testAudience, tc.url)
			if err == nil {
				t.Fatalf("NewOIDCVerifier(%q) returned nil error, want HTTPS-required error", tc.url)
			}
			if !strings.Contains(err.Error(), "JWKSURL") &&
				!strings.Contains(err.Error(), "https") &&
				!strings.Contains(err.Error(), "host") {
				t.Errorf("err = %v, want substring 'JWKSURL'/'https'/'host'", err)
			}
		})
	}
}

// TestNewOIDCVerifier_acceptsHTTPS -- the production
// constructor accepts a well-formed https URL.
func TestNewOIDCVerifier_acceptsHTTPS(t *testing.T) {
	t.Parallel()
	v, err := NewOIDCVerifier(testIssuer, testAudience, "https://issuer.example.com/.well-known/jwks.json")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if v == nil {
		t.Fatalf("verifier = nil")
	}
}

// TestNewOIDCVerifierInsecure_isTestOnly -- the unexported
// test helper does NOT enforce https. Documented contract:
// production code MUST never call this. We assert it's
// reachable from within the package (this test) AND
// implicitly assert it's unreachable from cmd/ via the Go
// export rules (lowercase identifier).
func TestNewOIDCVerifierInsecure_skipsSchemeCheck(t *testing.T) {
	t.Parallel()
	v, err := newOIDCVerifierInsecure(testIssuer, testAudience, "http://example.com/jwks")
	if err != nil {
		t.Fatalf("err = %v, want nil (test helper must accept http)", err)
	}
	if v == nil {
		t.Fatalf("verifier = nil")
	}
}

func TestJwkToRSA_rejectsBadFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		k    jwksKey
	}{
		{"non_rsa_kty", jwksKey{Kid: "k", Kty: "EC", N: "AA", E: "AQAB"}},
		{"use_not_sig", jwksKey{Kid: "k", Kty: "RSA", Use: "enc", N: "AA", E: "AQAB"}},
		{"empty_kid", jwksKey{Kid: "", Kty: "RSA", N: "AA", E: "AQAB"}},
		{"empty_n", jwksKey{Kid: "k", Kty: "RSA", N: "", E: "AQAB"}},
		{"empty_e", jwksKey{Kid: "k", Kty: "RSA", N: "AA", E: ""}},
		{"bad_n_b64", jwksKey{Kid: "k", Kty: "RSA", N: "!!!", E: "AQAB"}},
		{"bad_e_b64", jwksKey{Kid: "k", Kty: "RSA", N: "AA", E: "!!!"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := jwkToRSA(tc.k); err == nil {
				t.Errorf("jwkToRSA accepted bad input for %s", tc.name)
			}
		})
	}
}

func TestJwkToRSA_acceptsCanonical(t *testing.T) {
	t.Parallel()
	key := newTestKey(t, "kid-1", "RS256")
	pub, err := jwkToRSA(jwksKey{
		Kid: key.kid, Kty: "RSA", Use: "sig",
		N: key.jwksN, E: key.jwksE,
	})
	if err != nil {
		t.Fatalf("jwkToRSA: %v", err)
	}
	if pub.N.Cmp(key.priv.N) != 0 {
		t.Errorf("N mismatch")
	}
	if pub.E != key.priv.E {
		t.Errorf("E = %d, want %d", pub.E, key.priv.E)
	}
}