package repo_indexer

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// HMACSignatureHeader is the canonical HTTP header the Repo
// Indexer webhook reads the per-request HMAC signature
// from. Matches the GitHub-style convention so a CI
// publisher can re-use the same signing code that drives
// GitHub repository webhooks (tech-spec Sec 8.5 "REST +
// HMAC-signed").
//
// # Why mirror `internal/ingest/webhook`?
//
// Both webhook surfaces (`internal/ingest/webhook` for the
// `ingest.churn` payload and this package for git
// push-event deliveries) verify HMAC-SHA256 under the same
// header. Duplicating the small primitive here keeps the
// import graph flat (the Repo Indexer is a Catalog /
// Lifecycle writer; it has no business depending on the
// Measurement-side `ingest/webhook` package). A future
// refactor MAY extract the shared verifier into
// `internal/webhook/hmac/` once a third surface joins; the
// two-copy state is the deliberate "rule of three" wait
// per the agent-memory precedent.
//
// Wire shape: `X-Hub-Signature-256: sha256=<lowercase-hex
// of HMAC-SHA256(body, shared_secret)>`.
const HMACSignatureHeader = "X-Hub-Signature-256"

// HMACSignaturePrefix is the literal that prefixes the hex
// digest in [HMACSignatureHeader]. Pinned as a constant so
// a future algorithm upgrade (e.g. `sha512=...`) is a
// one-line change.
const HMACSignaturePrefix = "sha256="

// HMAC verification sentinel errors. Surfaced as wrapped
// errors so the handler can map them to structured 401
// responses without parsing free-form text.
var (
	// ErrHMACMissingHeader is returned when the request
	// does not carry [HMACSignatureHeader] at all.
	ErrHMACMissingHeader = errors.New("repo_indexer: missing HMAC signature header")
	// ErrHMACMalformedHeader is returned when the header
	// IS present but the value is not `sha256=<hex>`.
	ErrHMACMalformedHeader = errors.New("repo_indexer: malformed HMAC signature header (want sha256=<hex>)")
	// ErrHMACSignatureMismatch is returned when the header
	// digest does not match the computed digest.
	// Constant-time comparison is used so a timing-oracle
	// attack cannot brute-force the secret byte-by-byte.
	ErrHMACSignatureMismatch = errors.New("repo_indexer: HMAC signature does not match request body")
	// ErrHMACEmptySecret is returned when the verifier is
	// invoked with an empty secret. The composition root
	// guards against this; the verifier surfaces the
	// misconfiguration as defence in depth.
	ErrHMACEmptySecret = errors.New("repo_indexer: HMAC secret is empty")
)

// VerifyHMAC returns nil iff `headerValue` is a valid
// HMAC-SHA256 signature of `body` under `secret`.
//
// Algorithm:
//
//  1. Reject empty `secret` ([ErrHMACEmptySecret]).
//  2. Strip the `sha256=` prefix; the remainder MUST be
//     hex of exactly 64 characters
//     ([ErrHMACMalformedHeader]).
//  3. Compute HMAC-SHA256(body, secret).
//  4. Constant-time-compare the computed bytes with the
//     hex-decoded header bytes ([hmac.Equal] short-
//     circuits to a constant-time compare internally).
//
// Callers MUST NOT leak the computed digest or the secret
// in any error message they surface upstream.
func VerifyHMAC(body []byte, headerValue string, secret []byte) error {
	if len(secret) == 0 {
		return ErrHMACEmptySecret
	}
	if headerValue == "" {
		return ErrHMACMissingHeader
	}
	if !strings.HasPrefix(headerValue, HMACSignaturePrefix) {
		return ErrHMACMalformedHeader
	}
	hexDigest := strings.TrimPrefix(headerValue, HMACSignaturePrefix)
	if len(hexDigest) != sha256.Size*2 { // 64 hex chars for 32 bytes
		return ErrHMACMalformedHeader
	}
	want, err := hex.DecodeString(hexDigest)
	if err != nil {
		return ErrHMACMalformedHeader
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	got := mac.Sum(nil)

	if !hmac.Equal(got, want) {
		return ErrHMACSignatureMismatch
	}
	return nil
}

// SignHMAC returns the canonical [HMACSignatureHeader]
// value (`sha256=<hex>`) for `body` under `secret`.
// Exported for tests AND for any sibling Go process that
// publishes to the webhook (e.g. an integration smoke
// job). NEVER call this in the verifier path: a publisher
// signs, a verifier checks.
func SignHMAC(body, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return HMACSignaturePrefix + hex.EncodeToString(mac.Sum(nil))
}
