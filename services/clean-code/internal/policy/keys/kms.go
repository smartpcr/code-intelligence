package keys

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
)

// KMS is the abstraction over the operator's secret manager
// (Azure Key Vault, AWS KMS, GCP KMS, an in-cluster Vault, ...).
// Per tech-spec Sec 8.4 the deployment's secret manager owns
// the Ed25519 PRIVATE key; the Policy Steward never holds the
// private bytes in memory longer than the [KMS.Sign] call.
//
// The interface deliberately does NOT expose a "give me the
// private key" verb. Sign / Generate are the only paths through
// which private material is exercised, and both are scoped to
// the KMS implementation -- a swap to a hardware HSM is then a
// matter of replacing the KMS impl, not refactoring the Manager.
type KMS interface {
	// Ping is the startup readiness probe. Bootstrap invokes
	// it ONCE before any other KMS verb; a non-nil error
	// surfaces as [ErrKMSUnavailable] and the composition
	// root exits non-zero per implementation-plan Stage 5.1
	// scenario `kms-unavailable-blocks-start`.
	Ping(ctx context.Context) error

	// Generate mints a fresh Ed25519 keypair inside the KMS,
	// seals the private half, and returns the public bytes
	// (32 bytes) plus an opaque [KeyHandle] the service stores
	// alongside the row. The handle is the ONLY durable
	// reference the service holds; on a subsequent boot the
	// handle is loaded from the [Store] and passed back to
	// [KMS.Sign].
	Generate(ctx context.Context) (publicKey []byte, handle KeyHandle, err error)

	// Sign signs payload under the private key identified by
	// handle. Implementations MUST return an Ed25519
	// signature (64 bytes) or a wrapped error; partial /
	// truncated signatures are a contract violation.
	Sign(ctx context.Context, handle KeyHandle, payload []byte) ([]byte, error)
}

// InMemoryKMS is the test-only KMS implementation. It generates
// keypairs with `crypto/ed25519` and stores the private halves
// in a process-local map keyed by [KeyHandle]. Suitable for
// unit tests, the docker-compose `kms-mock` shim that ships
// with the e2e harness, and for `go test ./...` runs against
// the bootstrap path.
//
// NOT suitable for production -- the private keys live in
// plaintext heap memory and are gone on process exit.
type InMemoryKMS struct {
	// rng overrides the crypto/rand reader. Tests use a
	// seeded deterministic reader to produce reproducible
	// keypairs; production paths leave it nil and the impl
	// falls back to `crypto/rand.Reader`.
	rng io.Reader

	// failGenerate, when set, makes Generate return the
	// provided error verbatim. Tests use this to exercise
	// the bootstrap failure path without an out-of-band KMS
	// shim.
	failGenerate error

	// failSign, when set, makes Sign return the provided
	// error. Symmetrical to failGenerate; used by tests that
	// want to assert Sign-time fault behaviour.
	failSign error

	// failPing, when set, makes Ping return the provided
	// error. The implementation-plan Stage 5.1 scenario
	// `kms-unavailable-blocks-start` test uses this to assert
	// Bootstrap surfaces [ErrKMSUnavailable].
	failPing error

	mu      sync.RWMutex
	private map[KeyHandle]ed25519.PrivateKey
}

// NewInMemoryKMS constructs a fresh in-memory KMS. The optional
// rng argument lets a test inject a deterministic random reader
// (use `crypto/rand.Reader` -- or nil, same thing -- in
// production code paths).
func NewInMemoryKMS(rng io.Reader) *InMemoryKMS {
	return &InMemoryKMS{
		rng:     rng,
		private: make(map[KeyHandle]ed25519.PrivateKey),
	}
}

// FailGenerate configures the next (and subsequent) Generate
// calls to return err verbatim. Pass nil to clear the override.
func (k *InMemoryKMS) FailGenerate(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.failGenerate = err
}

// FailSign configures the next (and subsequent) Sign calls to
// return err verbatim. Pass nil to clear the override.
func (k *InMemoryKMS) FailSign(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.failSign = err
}

// FailPing configures Ping to return err verbatim. Pass nil to
// clear the override.
func (k *InMemoryKMS) FailPing(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.failPing = err
}

// Ping satisfies [KMS.Ping]. Returns the override set by
// FailPing (if any) or nil.
func (k *InMemoryKMS) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.failPing
}

// Generate satisfies [KMS.Generate]. Mints an Ed25519 keypair
// via crypto/ed25519 and registers the private half under a
// randomly-chosen [KeyHandle].
func (k *InMemoryKMS) Generate(ctx context.Context) ([]byte, KeyHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	k.mu.Lock()
	override := k.failGenerate
	k.mu.Unlock()
	if override != nil {
		return nil, "", override
	}

	reader := k.rng
	if reader == nil {
		reader = rand.Reader
	}
	pub, priv, err := ed25519.GenerateKey(reader)
	if err != nil {
		return nil, "", fmt.Errorf("policy/keys: ed25519.GenerateKey: %w", err)
	}
	handle, err := newRandomHandle(reader)
	if err != nil {
		return nil, "", err
	}

	k.mu.Lock()
	k.private[handle] = priv
	k.mu.Unlock()

	// Copy the public bytes so a caller mutating the slice
	// cannot corrupt the KMS-internal view (the in-memory KMS
	// has no other defence against this; it's cheap and
	// matches what a real KMS would do at the wire boundary).
	pubCopy := make([]byte, len(pub))
	copy(pubCopy, pub)
	return pubCopy, handle, nil
}

// Sign satisfies [KMS.Sign]. Returns an Ed25519 signature
// (64 bytes) over payload using the private key bound to
// handle.
func (k *InMemoryKMS) Sign(ctx context.Context, handle KeyHandle, payload []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	k.mu.RLock()
	override := k.failSign
	priv, ok := k.private[handle]
	k.mu.RUnlock()
	if override != nil {
		return nil, override
	}
	if !ok {
		return nil, fmt.Errorf("policy/keys: in-memory KMS has no private key for handle %q", string(handle))
	}
	sig := ed25519.Sign(priv, payload)
	return sig, nil
}

// HasHandle reports whether the KMS currently holds a private
// key for handle. Test-only helper.
func (k *InMemoryKMS) HasHandle(handle KeyHandle) bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	_, ok := k.private[handle]
	return ok
}

// newRandomHandle generates a short opaque [KeyHandle]. 16
// hex chars (8 bytes of entropy) is plenty -- handles are not
// secrets; the entropy is just there to avoid collision-by-
// accident in a long-running process.
func newRandomHandle(reader io.Reader) (KeyHandle, error) {
	var raw [8]byte
	if _, err := io.ReadFull(reader, raw[:]); err != nil {
		return "", fmt.Errorf("policy/keys: random handle: %w", err)
	}
	return KeyHandle("kms-mem-" + hex.EncodeToString(raw[:])), nil
}

// verifyAgainstPublic is a small helper used by Manager.Verify
// to constant-time-compare the Ed25519 signature. Exposed at
// the package level because tests in `manager_test.go` use it
// to mint sentinel comparisons.
func verifyAgainstPublic(public []byte, payload []byte, signature []byte) error {
	if len(public) != Ed25519PublicKeySize {
		return ErrInvalidPublicKey
	}
	if len(signature) != Ed25519SignatureSize {
		return ErrSignatureMismatch
	}
	if !ed25519.Verify(ed25519.PublicKey(public), payload, signature) {
		return ErrSignatureMismatch
	}
	return nil
}

// Compile-time check that InMemoryKMS satisfies KMS. Catches a
// signature drift at build time rather than at the first test
// invocation.
var _ KMS = (*InMemoryKMS)(nil)
