package wal

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"

	"github.com/gofrs/uuid"
)

// Signer is the narrow port [Writer] uses to sign a frame's
// canonical payload bytes before the frame is staged into a
// [TxBatch].
//
// The callback shape is REQUIRED so the signing key id and
// the signature are atomically bound: the signer chooses the
// active key id, calls `build` with it, and signs the byte
// slice `build` returns. Without this binding, a writer that
// minted a frame at keyID=uuid.Nil, signed it, then over-wrote
// `frame.SigningKeyID` from a real signer's response would
// produce a signature that fails recomputation. The callback
// design closes that gap: the keyID the verifier reads from
// disk is the same keyID the signer hashed into the payload.
//
// Implementations:
//
//   - Production: a thin shim around the
//     `policy/keys.Manager.SignWith` method bound at the
//     composition root. The shim's `SignFrame` returns the
//     manager's currently-active signing-key id and the
//     Ed25519 signature.
//   - Tests: [NoopSigner] -- deterministic SHA-256 stand-in
//     that never touches a KMS but still produces a
//     verifiable signature so reconciler tests can round-trip
//     without bootstrapping the real key store.
//
// `build` is called EXACTLY ONCE inside `SignFrame` and MUST
// be side-effect-free apart from the caller-owned closure
// state (typically a copy of the in-flight [AuditFrame]).
//
// Verification is intentionally NOT on this interface: the
// reconciler resolves historical keys via its own lower-level
// resolver (a frame signed yesterday must verify even after a
// rotation has retired today's key, which the live
// `policy/keys.Manager.Verify` path rejects on purpose). The
// writer only needs to Sign.
type Signer interface {
	SignFrame(ctx context.Context, build func(keyID uuid.UUID) ([]byte, error)) (keyID uuid.UUID, signature []byte, err error)
}

// NoopSigner is the test signer. Calls `build(uuid.Nil)` and
// returns a deterministic SHA-256 of the result as the
// "signature".
//
// NEVER use NoopSigner in production -- the writer's
// composition root is responsible for refusing wiring against
// it. The audit-trail signature MUST be a real Ed25519
// signature for the architecture's tamper-evidence guarantee
// (architecture Sec 7.1 lines 1434-1438).
type NoopSigner struct{}

// SignFrame implements [Signer.SignFrame] with a deterministic
// SHA-256 over the payload `build(uuid.Nil)` returns.
func (NoopSigner) SignFrame(ctx context.Context, build func(keyID uuid.UUID) ([]byte, error)) (uuid.UUID, []byte, error) {
	if err := ctx.Err(); err != nil {
		return uuid.Nil, nil, err
	}
	if build == nil {
		return uuid.Nil, nil, errors.New("wal: NoopSigner.SignFrame: build is nil")
	}
	payload, err := build(uuid.Nil)
	if err != nil {
		return uuid.Nil, nil, err
	}
	if len(payload) == 0 {
		return uuid.Nil, nil, errors.New("wal: NoopSigner.SignFrame: empty payload")
	}
	sum := sha256.Sum256(payload)
	out := make([]byte, len(sum))
	copy(out, sum[:])
	return uuid.Nil, out, nil
}

// NoopVerify recomputes the SHA-256 of payload and reports
// `nil` iff it matches `signature`. Used by reconciler-side
// tests to mirror [NoopSigner.SignFrame]; production code does
// not call this helper.
func NoopVerify(payload, signature []byte) error {
	if len(payload) == 0 {
		return errors.New("wal: NoopVerify: empty payload")
	}
	if len(signature) != sha256.Size {
		return errors.New("wal: NoopVerify: signature length mismatch")
	}
	sum := sha256.Sum256(payload)
	if subtle.ConstantTimeCompare(sum[:], signature) != 1 {
		return errors.New("wal: NoopVerify: signature mismatch")
	}
	return nil
}
