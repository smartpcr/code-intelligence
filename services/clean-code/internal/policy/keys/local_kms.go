package keys

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// LocalKMSMasterKeyLen is the required byte-length of the
// AES-256 master key the [LocalSealedKMS] uses to wrap Ed25519
// seeds. Operators are expected to source the master key from
// their secrets manager and inject it via the
// CLEAN_CODE_KMS_MASTER_KEY_HEX env var (64 lowercase hex chars).
const LocalKMSMasterKeyLen = 32

// localKMSHandlePrefix is the canonical prefix every
// [KeyHandle] produced by [LocalSealedKMS] carries. Lets a
// downstream reader spot "this row was sealed by the local
// envelope KMS" via `strings.HasPrefix` without parsing the
// blob, and lets a future KMS impl pick a different prefix
// for its own handle namespace.
const localKMSHandlePrefix = "local-v1:"

// localKMSAAD is the AES-GCM Additional Authenticated Data the
// [LocalSealedKMS] binds every sealed blob to. AEAD AAD is
// cryptographically tied to the ciphertext: an attacker who
// substitutes a blob from a different sealing context (e.g. a
// different service's wrapped material that happens to use the
// same master key) will see Open fail because the AAD won't
// match. Per rubber-duck critique #3.
var localKMSAAD = []byte("clean-code/policy-steward-signing-key:local-v1")

// ErrLocalKMSMasterKey is returned by [NewLocalSealedKMS] when
// the supplied master key is missing, the wrong length, or not
// hex-encoded. The composition root translates this into a
// fatal startup error so a mis-configured deployment never
// runs with a degraded sealing layer.
var ErrLocalKMSMasterKey = errors.New("policy/keys: LocalSealedKMS master key invalid")

// LocalSealedKMS is the v1 production-capable KMS shipping
// with the Stage 5.1 deliverable. It implements [KMS] by
// wrapping the Ed25519 seed with AES-256-GCM under a master
// key the operator holds out-of-band (never on disk, never in
// the DB). The wrapped blob is the [KeyHandle] persisted in
// the `clean_code.policy_signing_keys.key_handle` column.
//
// Threat model:
//
//   - The master key NEVER touches PostgreSQL. Even a full
//     dump of the `policy_signing_keys` table on its own
//     reveals no usable private material; the attacker would
//     also need the master key.
//
//   - AEAD additional data ([localKMSAAD]) binds the sealed
//     blob to this service's sealing context. Cross-service
//     material reuse fails at Open.
//
//   - Tamper detection: any single bit flip in the blob is
//     detected by the AES-GCM tag.
//
//   - Forward secrecy: NOT provided. Rotating the master key
//     requires re-sealing every row. Stage 5.2 ships master
//     key rotation; until then a master-key compromise
//     requires re-rotating every signing key via
//     [Manager.ForceRotate].
//
// LocalSealedKMS is safe for concurrent use. Sign is
// allocation-free in the steady state (Open + the signature
// itself).
type LocalSealedKMS struct {
	aead cipher.AEAD
	rng  io.Reader

	mu       sync.RWMutex
	failPing error
}

// NewLocalSealedKMS constructs a [LocalSealedKMS] from a 64-
// char hex-encoded 32-byte AES master key. The caller is
// expected to source masterKeyHex from `internal/config`
// (CLEAN_CODE_KMS_MASTER_KEY_HEX) which never echoes the
// value into a log.
//
// Returns [ErrLocalKMSMasterKey] (wrapped) when the input is
// the wrong shape -- the composition root surfaces this as a
// fatal startup error.
func NewLocalSealedKMS(masterKeyHex string) (*LocalSealedKMS, error) {
	masterKeyHex = strings.TrimSpace(masterKeyHex)
	if masterKeyHex == "" {
		return nil, fmt.Errorf("%w: master key is empty", ErrLocalKMSMasterKey)
	}
	raw, err := hex.DecodeString(masterKeyHex)
	if err != nil {
		return nil, fmt.Errorf("%w: master key not hex: %v", ErrLocalKMSMasterKey, err)
	}
	if len(raw) != LocalKMSMasterKeyLen {
		return nil, fmt.Errorf("%w: master key length=%d, want %d (AES-256)",
			ErrLocalKMSMasterKey, len(raw), LocalKMSMasterKeyLen)
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, fmt.Errorf("policy/keys: LocalSealedKMS: aes.NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("policy/keys: LocalSealedKMS: cipher.NewGCM: %w", err)
	}
	// Zero the raw master-key bytes once they're inside the
	// AES key schedule. The AEAD keeps an internal copy; the
	// caller's hex string still lives in their config struct.
	for i := range raw {
		raw[i] = 0
	}
	return &LocalSealedKMS{aead: aead, rng: rand.Reader}, nil
}

// SetRNG injects an alternative random reader. Tests use a
// deterministic reader to produce reproducible keypairs;
// production paths leave the default `crypto/rand.Reader` in
// place.
func (k *LocalSealedKMS) SetRNG(r io.Reader) {
	if r == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rng = r
}

// FailPing forces Ping to return err. Pass nil to clear. The
// composition root never calls this; tests use it to exercise
// the `kms-unavailable-blocks-start` scenario.
func (k *LocalSealedKMS) FailPing(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.failPing = err
}

// Ping satisfies [KMS.Ping]. The AEAD itself can't fail at
// runtime (construction did the real validation), so Ping is
// effectively a sentinel hook for tests. In production the
// composition root invokes Ping via [Bootstrap] once at
// startup.
func (k *LocalSealedKMS) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.failPing
}

// Generate satisfies [KMS.Generate]. Mints a fresh Ed25519
// keypair, AES-GCM-seals the 32-byte seed under
// [localKMSAAD], and returns the public bytes plus the handle
// = `local-v1:` || base64(nonce || sealed). The returned
// handle is safe to persist verbatim in the
// `policy_signing_keys.key_handle` column.
func (k *LocalSealedKMS) Generate(ctx context.Context) ([]byte, KeyHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	k.mu.RLock()
	rng := k.rng
	k.mu.RUnlock()
	if rng == nil {
		rng = rand.Reader
	}
	pub, priv, err := ed25519.GenerateKey(rng)
	if err != nil {
		return nil, "", fmt.Errorf("policy/keys: LocalSealedKMS.Generate: ed25519: %w", err)
	}
	seed := priv.Seed()
	defer zeroize(seed)

	nonce := make([]byte, k.aead.NonceSize())
	if _, err := io.ReadFull(rng, nonce); err != nil {
		return nil, "", fmt.Errorf("policy/keys: LocalSealedKMS.Generate: nonce: %w", err)
	}
	sealed := k.aead.Seal(nil, nonce, seed, localKMSAAD)

	blob := make([]byte, 0, len(nonce)+len(sealed))
	blob = append(blob, nonce...)
	blob = append(blob, sealed...)
	handle := KeyHandle(localKMSHandlePrefix + base64.StdEncoding.EncodeToString(blob))

	pubCopy := make([]byte, len(pub))
	copy(pubCopy, pub)
	return pubCopy, handle, nil
}

// Sign satisfies [KMS.Sign]. Parses handle, decrypts the
// sealed seed under [localKMSAAD], reconstructs the Ed25519
// private key via `ed25519.NewKeyFromSeed`, signs payload,
// and zeros the unsealed seed before returning. Returns a
// 64-byte Ed25519 signature on success.
func (k *LocalSealedKMS) Sign(ctx context.Context, handle KeyHandle, payload []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body := strings.TrimPrefix(string(handle), localKMSHandlePrefix)
	if body == string(handle) {
		return nil, fmt.Errorf("policy/keys: LocalSealedKMS.Sign: handle %q is not in the %q namespace",
			string(handle), localKMSHandlePrefix)
	}
	blob, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return nil, fmt.Errorf("policy/keys: LocalSealedKMS.Sign: handle base64: %w", err)
	}
	nonceSize := k.aead.NonceSize()
	if len(blob) < nonceSize+k.aead.Overhead() {
		return nil, fmt.Errorf("policy/keys: LocalSealedKMS.Sign: blob too short (%d bytes, want >= %d)",
			len(blob), nonceSize+k.aead.Overhead())
	}
	nonce, sealed := blob[:nonceSize], blob[nonceSize:]
	seed, err := k.aead.Open(nil, nonce, sealed, localKMSAAD)
	if err != nil {
		return nil, fmt.Errorf("policy/keys: LocalSealedKMS.Sign: AEAD open: %w", err)
	}
	defer zeroize(seed)
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("policy/keys: LocalSealedKMS.Sign: unsealed seed length=%d, want %d",
			len(seed), ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	sig := ed25519.Sign(priv, payload)
	// Best-effort wipe of the derived private bytes.
	for i := range priv {
		priv[i] = 0
	}
	return sig, nil
}

// zeroize overwrites buf with zeros. Used to clear unsealed
// seed bytes before they leave scope. Go does not guarantee
// the compiler can't optimise this away, but for the local
// KMS shipping in v1 this defence is in line with what is
// reasonable for an envelope-encryption scheme.
func zeroize(buf []byte) {
	for i := range buf {
		buf[i] = 0
	}
}

// Compile-time check that LocalSealedKMS satisfies KMS.
var _ KMS = (*LocalSealedKMS)(nil)
