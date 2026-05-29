package api

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

// audienceLiteral / issuerLiteral are pinned test-fixture
// values used across the auth-test file. They mirror the
// shape an IdP would emit (`https://issuer.example/oauth`,
// `clean-code-gateway`) so the assertions match a realistic
// JWT payload.
const (
	audienceLiteral = "clean-code-gateway"
	issuerLiteral   = "https://issuer.example/oauth"
)

// testSecret is the shared HMAC key used by every test in
// this file. 32 bytes -- matching the canonical HS256
// recommendation in RFC 7518 Sec 3.2.
var testSecret = []byte("a-very-strong-32-byte-test-secret-key!")

func newTestAuth(now time.Time) *StaticHMACAuthenticator {
	return &StaticHMACAuthenticator{
		Secret:   testSecret,
		Issuer:   issuerLiteral,
		Audience: audienceLiteral,
		Now:      func() time.Time { return now },
		Leeway:   30 * time.Second,
	}
}

// TestSentinelErrors_AreDistinct asserts that every sentinel
// is its own distinct error value -- the gateway dispatches
// on errors.Is, so accidentally aliasing two sentinels would
// silently route, say, ErrAuthBackend to the 401 branch.
func TestSentinelErrors_AreDistinct(t *testing.T) {
	t.Parallel()
	sentinels := []error{
		ErrMissingToken, ErrMalformedToken, ErrInvalidToken,
		ErrExpiredToken, ErrBadAudience, ErrBadIssuer,
		ErrAuthBackend,
	}
	for i, a := range sentinels {
		for j, b := range sentinels {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Errorf("sentinel %v unexpectedly matches %v", a, b)
			}
		}
	}
}

// TestErrAuthBackend_DistinctFromInvalidToken specifically
// guards the contract addressed by iter-2 evaluator item #5:
// JWKS-fetch failures (ErrAuthBackend) must be distinguishable
// from token-content failures (ErrInvalidToken). The gateway's
// handler relies on this distinction to route the former to
// 503 (operator-actionable) and the latter to 401 (caller-
// actionable).
func TestErrAuthBackend_DistinctFromInvalidToken(t *testing.T) {
	t.Parallel()
	if errors.Is(ErrAuthBackend, ErrInvalidToken) {
		t.Errorf("ErrAuthBackend must not classify as ErrInvalidToken")
	}
	if errors.Is(ErrInvalidToken, ErrAuthBackend) {
		t.Errorf("ErrInvalidToken must not classify as ErrAuthBackend")
	}
}

func TestParseBearer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		header  string
		wantErr error
		wantTok string
	}{
		{name: "valid", header: "Bearer abc.def.ghi", wantTok: "abc.def.ghi"},
		{name: "valid-with-leading-spaces", header: "  Bearer abc.def.ghi", wantTok: "abc.def.ghi"},
		{name: "valid-with-trailing-spaces", header: "Bearer abc.def.ghi  ", wantTok: "abc.def.ghi"},
		{name: "empty", header: "", wantErr: ErrMissingToken},
		{name: "whitespace-only", header: "   ", wantErr: ErrMissingToken},
		{name: "lowercase-scheme", header: "bearer abc.def.ghi", wantErr: ErrMalformedToken},
		{name: "missing-token", header: "Bearer ", wantErr: ErrMalformedToken},
		{name: "wrong-scheme", header: "Basic dXNlcjpwYXNz", wantErr: ErrMalformedToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tok, err := ParseBearer(tc.header)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err=%v, want errors.Is %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if tok != tc.wantTok {
				t.Errorf("token=%q, want %q", tok, tc.wantTok)
			}
		})
	}
}

func TestStaticHMACAuthenticator_Valid(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	auth := newTestAuth(now)
	token := MintHS256TestToken(testSecret, map[string]any{
		"iss":   issuerLiteral,
		"sub":   "user@example.com",
		"aud":   audienceLiteral,
		"exp":   now.Add(time.Hour).Unix(),
		"iat":   now.Unix(),
		"email": "user@example.com",
	})
	id, err := auth.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate err=%v", err)
	}
	if id.Subject != "user@example.com" {
		t.Errorf("Subject=%q, want user@example.com", id.Subject)
	}
	if len(id.Audience) != 1 || id.Audience[0] != audienceLiteral {
		t.Errorf("Audience=%v, want [%s]", id.Audience, audienceLiteral)
	}
	if id.Issuer != issuerLiteral {
		t.Errorf("Issuer=%q, want %s", id.Issuer, issuerLiteral)
	}
	if !id.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Errorf("ExpiresAt=%v, want %v", id.ExpiresAt, now.Add(time.Hour))
	}
	if _, ok := id.RawClaims["email"]; !ok {
		t.Errorf("RawClaims missing email; got keys=%v", keys(id.RawClaims))
	}
}

func TestStaticHMACAuthenticator_AudienceMismatch_403(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	auth := newTestAuth(now)
	token := MintHS256TestToken(testSecret, map[string]any{
		"iss": issuerLiteral,
		"sub": "user@example.com",
		"aud": "some-other-service",
		"exp": now.Add(time.Hour).Unix(),
	})
	_, err := auth.Authenticate(context.Background(), token)
	if !errors.Is(err, ErrBadAudience) {
		t.Fatalf("err=%v, want errors.Is ErrBadAudience", err)
	}
}

func TestStaticHMACAuthenticator_AudienceArray(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	auth := newTestAuth(now)
	// JWT `aud` claim is a JSON ARRAY containing our
	// audience among others. RFC 7519 Sec 4.1.3 form.
	token := MintHS256TestToken(testSecret, map[string]any{
		"iss": issuerLiteral,
		"sub": "user@example.com",
		"aud": []string{"some-other-service", audienceLiteral, "third-service"},
		"exp": now.Add(time.Hour).Unix(),
	})
	id, err := auth.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate err=%v", err)
	}
	if len(id.Audience) != 3 {
		t.Fatalf("Audience=%v, want 3 entries", id.Audience)
	}
}

func TestStaticHMACAuthenticator_AudienceArrayMismatch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	auth := newTestAuth(now)
	token := MintHS256TestToken(testSecret, map[string]any{
		"iss": issuerLiteral,
		"sub": "user@example.com",
		"aud": []string{"some-other-service", "third-service"},
		"exp": now.Add(time.Hour).Unix(),
	})
	_, err := auth.Authenticate(context.Background(), token)
	if !errors.Is(err, ErrBadAudience) {
		t.Fatalf("err=%v, want errors.Is ErrBadAudience", err)
	}
}

func TestStaticHMACAuthenticator_AudienceSubstringMismatch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	auth := newTestAuth(now)
	// A confused-deputy audience that SUBSTRING-contains
	// the gateway's audience MUST NOT pass -- exact match
	// only.
	token := MintHS256TestToken(testSecret, map[string]any{
		"iss": issuerLiteral,
		"sub": "user@example.com",
		"aud": "https://attacker.example/clean-code-gateway",
		"exp": now.Add(time.Hour).Unix(),
	})
	_, err := auth.Authenticate(context.Background(), token)
	if !errors.Is(err, ErrBadAudience) {
		t.Fatalf("substring aud accepted; got err=%v", err)
	}
}

func TestStaticHMACAuthenticator_BadSignature(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	auth := newTestAuth(now)
	// Mint with the WRONG secret -- the gateway's HMAC
	// verification MUST reject it as invalid.
	token := MintHS256TestToken([]byte("wrong-secret"), map[string]any{
		"iss": issuerLiteral,
		"sub": "user@example.com",
		"aud": audienceLiteral,
		"exp": now.Add(time.Hour).Unix(),
	})
	_, err := auth.Authenticate(context.Background(), token)
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err=%v, want errors.Is ErrInvalidToken", err)
	}
}

func TestStaticHMACAuthenticator_Expired(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	auth := newTestAuth(now)
	token := MintHS256TestToken(testSecret, map[string]any{
		"iss": issuerLiteral,
		"sub": "user@example.com",
		"aud": audienceLiteral,
		"exp": now.Add(-5 * time.Minute).Unix(), // 5 min in the past
	})
	_, err := auth.Authenticate(context.Background(), token)
	if !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("err=%v, want errors.Is ErrExpiredToken", err)
	}
}

func TestStaticHMACAuthenticator_LeewayTolerance(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	auth := newTestAuth(now) // 30s leeway
	// exp 10s in the past -- within the 30s leeway window.
	token := MintHS256TestToken(testSecret, map[string]any{
		"iss": issuerLiteral,
		"sub": "user@example.com",
		"aud": audienceLiteral,
		"exp": now.Add(-10 * time.Second).Unix(),
	})
	_, err := auth.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("token within leeway rejected: %v", err)
	}
}

func TestStaticHMACAuthenticator_BadIssuer(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	auth := newTestAuth(now)
	token := MintHS256TestToken(testSecret, map[string]any{
		"iss": "https://attacker.example/oauth",
		"sub": "user@example.com",
		"aud": audienceLiteral,
		"exp": now.Add(time.Hour).Unix(),
	})
	_, err := auth.Authenticate(context.Background(), token)
	if !errors.Is(err, ErrBadIssuer) {
		t.Fatalf("err=%v, want errors.Is ErrBadIssuer", err)
	}
}

func TestStaticHMACAuthenticator_MissingSubject(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	auth := newTestAuth(now)
	token := MintHS256TestToken(testSecret, map[string]any{
		"iss": issuerLiteral,
		"aud": audienceLiteral,
		"exp": now.Add(time.Hour).Unix(),
	})
	_, err := auth.Authenticate(context.Background(), token)
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err=%v, want errors.Is ErrInvalidToken", err)
	}
}

func TestStaticHMACAuthenticator_MissingExp(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	auth := newTestAuth(now)
	token := MintHS256TestToken(testSecret, map[string]any{
		"iss": issuerLiteral,
		"sub": "user@example.com",
		"aud": audienceLiteral,
	})
	_, err := auth.Authenticate(context.Background(), token)
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err=%v, want errors.Is ErrInvalidToken", err)
	}
}

func TestStaticHMACAuthenticator_RejectsAlgNone(t *testing.T) {
	t.Parallel()
	// Craft an `alg=none` token by hand. RFC 7519's "alg
	// substitution attack" mitigation requires the
	// verifier to reject any alg other than HS256.
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	header := `{"alg":"none","typ":"JWT"}`
	payload := `{"iss":"` + issuerLiteral + `","sub":"user@example.com","aud":"` + audienceLiteral + `","exp":` +
		intStr(now.Add(time.Hour).Unix()) + `}`
	hb := base64.RawURLEncoding.EncodeToString([]byte(header))
	pb := base64.RawURLEncoding.EncodeToString([]byte(payload))
	// Empty signature (alg=none convention).
	token := hb + "." + pb + "."
	auth := newTestAuth(now)
	_, err := auth.Authenticate(context.Background(), token)
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("alg=none accepted; got err=%v", err)
	}
}

func TestStaticHMACAuthenticator_MalformedShape(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	auth := newTestAuth(now)
	cases := []struct {
		name  string
		token string
	}{
		{name: "two-segments", token: "abc.def"},
		{name: "single-segment", token: "abc"},
		{name: "four-segments", token: "a.b.c.d"},
		{name: "non-base64-header", token: "!.b.c"},
		{name: "garbage-payload", token: "eyJhbGciOiJIUzI1NiJ9.!.c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := auth.Authenticate(context.Background(), tc.token)
			if err == nil {
				t.Fatalf("token %q accepted", tc.token)
			}
			if !errors.Is(err, ErrMalformedToken) && !errors.Is(err, ErrInvalidToken) {
				t.Errorf("err=%v, want Malformed or Invalid sentinel", err)
			}
		})
	}
}

func TestStaticHMACAuthenticator_NilReceiver(t *testing.T) {
	t.Parallel()
	var a *StaticHMACAuthenticator
	_, err := a.Authenticate(context.Background(), "x.y.z")
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("nil receiver: err=%v, want ErrInvalidToken", err)
	}
}

func TestStaticHMACAuthenticator_EmptySecretRefuses(t *testing.T) {
	t.Parallel()
	a := &StaticHMACAuthenticator{
		Secret:   nil,
		Issuer:   issuerLiteral,
		Audience: audienceLiteral,
	}
	_, err := a.Authenticate(context.Background(), MintHS256TestToken(testSecret, map[string]any{
		"sub": "user", "aud": audienceLiteral, "exp": time.Now().Add(time.Hour).Unix(),
	}))
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err=%v, want ErrInvalidToken", err)
	}
}

func TestStaticHMACAuthenticator_AudienceUnconfiguredRefuses(t *testing.T) {
	t.Parallel()
	a := &StaticHMACAuthenticator{
		Secret: testSecret,
		Issuer: issuerLiteral,
		// Audience deliberately unset.
	}
	_, err := a.Authenticate(context.Background(), MintHS256TestToken(testSecret, map[string]any{
		"sub": "user", "aud": "anything", "exp": time.Now().Add(time.Hour).Unix(),
	}))
	if !errors.Is(err, ErrBadAudience) {
		t.Fatalf("err=%v, want ErrBadAudience (audience unconfigured -> refuse all)", err)
	}
}

// intStr returns the base-10 string form of `i` without
// dragging `strconv` into the test imports. Used only for
// constructing the alg-none JWT payload by hand.
func intStr(i int64) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}
	if neg {
		digits = "-" + digits
	}
	return digits
}

// keys returns the sorted key list of a map -- a tiny helper
// to keep assertion messages stable across Go versions
// (map-iteration order changes per-process).
func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Simple insertion sort -- the test data sets are
	// small (<10 entries) so we avoid pulling `sort` in.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// Ensure the keys helper is exercised so the linter does not
// flag it as dead code on a future test refactor.
var _ = strings.Join
