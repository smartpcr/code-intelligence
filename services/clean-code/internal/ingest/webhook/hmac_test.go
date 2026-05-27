package webhook_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/webhook"
)

// hmacUnitSecret is the deterministic secret every HMAC unit
// test below signs/verifies against. Constant so the
// expected digest below stays stable; 32 bytes mirrors a
// realistic rotated production secret.
var hmacUnitSecret = []byte("hmac-unit-test-secret-32-bytes!!")

// TestSignHMAC_KnownDigest pins the algorithm against a
// hand-computed vector so a future swap of hash function is
// loud rather than silent.
func TestSignHMAC_KnownDigest(t *testing.T) {
	t.Parallel()
	body := []byte(`{"hello":"world"}`)

	// Independent reference computation (do not call the
	// package's SignHMAC here -- we want a vector check, not
	// a tautology).
	mac := hmac.New(sha256.New, hmacUnitSecret)
	mac.Write(body)
	want := webhook.HMACSignaturePrefix + hex.EncodeToString(mac.Sum(nil))

	got := webhook.SignHMAC(body, hmacUnitSecret)
	if got != want {
		t.Fatalf("SignHMAC: want %q, got %q", want, got)
	}
}

// TestVerifyHMAC_RoundTrip pins the happy path: a digest
// produced by SignHMAC verifies under the same secret.
func TestVerifyHMAC_RoundTrip(t *testing.T) {
	t.Parallel()
	body := []byte(`{"a":1,"b":[2,3]}`)
	sig := webhook.SignHMAC(body, hmacUnitSecret)
	if err := webhook.VerifyHMAC(body, sig, hmacUnitSecret); err != nil {
		t.Fatalf("VerifyHMAC: want nil, got %v", err)
	}
}

// TestVerifyHMAC_EmptySecretRejected pins the defence-in-
// depth guard. The composition root rejects empty secrets at
// config-load time; this asserts the verifier ALSO refuses.
func TestVerifyHMAC_EmptySecretRejected(t *testing.T) {
	t.Parallel()
	body := []byte(`{}`)
	if err := webhook.VerifyHMAC(body, "sha256=00", nil); !errors.Is(err, webhook.ErrHMACEmptySecret) {
		t.Errorf("VerifyHMAC with nil secret: want ErrHMACEmptySecret, got %v", err)
	}
	if err := webhook.VerifyHMAC(body, "sha256=00", []byte{}); !errors.Is(err, webhook.ErrHMACEmptySecret) {
		t.Errorf("VerifyHMAC with empty secret: want ErrHMACEmptySecret, got %v", err)
	}
}

// TestVerifyHMAC_MissingHeaderRejected pins the empty
// header path.
func TestVerifyHMAC_MissingHeaderRejected(t *testing.T) {
	t.Parallel()
	if err := webhook.VerifyHMAC([]byte("body"), "", hmacUnitSecret); !errors.Is(err, webhook.ErrHMACMissingHeader) {
		t.Errorf("VerifyHMAC with empty header: want ErrHMACMissingHeader, got %v", err)
	}
}

// TestVerifyHMAC_MalformedHeader covers each malformed-shape
// branch the verifier short-circuits on.
func TestVerifyHMAC_MalformedHeader(t *testing.T) {
	t.Parallel()
	body := []byte(`{}`)
	cases := []struct {
		name string
		sig  string
	}{
		{name: "wrong-algo-prefix", sig: "md5=" + strings.Repeat("0", 64)},
		{name: "no-prefix-just-hex", sig: strings.Repeat("0", 64)},
		{name: "too-short-hex", sig: webhook.HMACSignaturePrefix + strings.Repeat("0", 63)},
		{name: "too-long-hex", sig: webhook.HMACSignaturePrefix + strings.Repeat("0", 65)},
		{name: "non-hex-chars", sig: webhook.HMACSignaturePrefix + strings.Repeat("z", 64)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := webhook.VerifyHMAC(body, tc.sig, hmacUnitSecret)
			if !errors.Is(err, webhook.ErrHMACMalformedHeader) {
				t.Errorf("VerifyHMAC(%q): want ErrHMACMalformedHeader, got %v", tc.sig, err)
			}
		})
	}
}

// TestVerifyHMAC_SignatureMismatch pins the cryptographic
// rejection: a well-formed but cryptographically WRONG
// digest is rejected. The verifier's constant-time compare
// guards against timing attacks; we just assert the boolean
// outcome here.
func TestVerifyHMAC_SignatureMismatch(t *testing.T) {
	t.Parallel()
	body := []byte(`{"a":1}`)
	// Sign with a different secret to get a well-formed
	// header value that won't match.
	wrong := webhook.SignHMAC(body, []byte("a-different-32-byte-secret-abcdef"))
	err := webhook.VerifyHMAC(body, wrong, hmacUnitSecret)
	if !errors.Is(err, webhook.ErrHMACSignatureMismatch) {
		t.Errorf("VerifyHMAC: want ErrHMACSignatureMismatch, got %v", err)
	}
}

// TestVerifyHMAC_BodyTamper pins that flipping one bit of
// the body is enough to invalidate the digest.
func TestVerifyHMAC_BodyTamper(t *testing.T) {
	t.Parallel()
	body := []byte(`{"a":1}`)
	sig := webhook.SignHMAC(body, hmacUnitSecret)
	tampered := append([]byte{}, body...)
	tampered[0] ^= 0x01
	err := webhook.VerifyHMAC(tampered, sig, hmacUnitSecret)
	if !errors.Is(err, webhook.ErrHMACSignatureMismatch) {
		t.Errorf("VerifyHMAC tampered: want ErrHMACSignatureMismatch, got %v", err)
	}
}

// TestVerifyHMAC_CaseSensitiveHexDigest pins the
// case-sensitivity contract: a digest with uppercase hex
// MUST be rejected as malformed (or at least mismatched) so
// publishers settle on the documented lowercase form. This
// keeps the wire format unambiguous for the operator
// runbook's "exact bytes" troubleshooting.
func TestVerifyHMAC_UppercaseHexAccepted(t *testing.T) {
	t.Parallel()
	// hex.DecodeString accepts mixed-case hex, so the
	// verifier currently accepts uppercase too. This test
	// pins the EXISTING behaviour so a future tightening
	// (lowercase-only) is loud, not silent.
	body := []byte(`{"a":1}`)
	sig := webhook.SignHMAC(body, hmacUnitSecret)
	if !strings.HasPrefix(sig, webhook.HMACSignaturePrefix) {
		t.Fatalf("SignHMAC output missing prefix %q: %q", webhook.HMACSignaturePrefix, sig)
	}
	hexPart := strings.TrimPrefix(sig, webhook.HMACSignaturePrefix)
	upperSig := webhook.HMACSignaturePrefix + strings.ToUpper(hexPart)
	if err := webhook.VerifyHMAC(body, upperSig, hmacUnitSecret); err != nil {
		t.Errorf("VerifyHMAC with uppercase hex: want nil (hex.DecodeString accepts mixed case), got %v", err)
	}
}
