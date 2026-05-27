package keys

import (
	"errors"
	"time"

	"github.com/gofrs/uuid"
)

// Ed25519PublicKeySize is the wire-level size of an Ed25519
// public key (RFC 8032). The migration's CHECK constraint
// (`octet_length(public_key) = 32`) and the Manager's runtime
// validation both reference this constant so a future
// algorithm bump must edit ONE place.
const Ed25519PublicKeySize = 32

// Ed25519SignatureSize is the wire-level size of an Ed25519
// signature (RFC 8032). Defined alongside the public-key size
// for symmetry; the Manager's Sign / Verify shape never quotes
// `64` as a magic number.
const Ed25519SignatureSize = 64

// SentinelValidUntil is the far-future placeholder the
// `policy.keys.list_active` verb returns for the newest key
// (the one with no successor). Picked at the end of the
// PostgreSQL timestamptz year range so the JSON serialisation
// is unambiguous and a downstream consumer can pattern-match
// on the sentinel without ambiguity.
//
// Documented in the Stage 5.1 runbook so an operator who sees
// "9999-12-31..." in a list_active response understands it
// means "no successor key has been published yet" rather than
// "the key expires in 7973 years".
var SentinelValidUntil = time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)

// KeyHandle is the opaque identifier the KMS returned when the
// keypair was generated. The Manager stores it alongside the
// public material so a restart can resolve the same KMS-sealed
// private key.
//
// The shape is intentionally `string` and treated as opaque
// reference material rather than a secret. Real-world handles
// are typically Key Vault key URIs (`https://kv.example.com/
// keys/policy-steward/v3`) or env-var names; tests use a
// short random label.
type KeyHandle string

// KeyRecord is the persisted shape of a signing key row. It
// MIRRORS the `clean_code.policy_signing_keys` table columns
// minus the derived `valid_until` (computed at read time).
type KeyRecord struct {
	// KeyID is the row PK and the value of the Stage 5.1
	// `signing_key_id` field that the Evaluator Surface
	// records on each finding.
	KeyID uuid.UUID
	// Fingerprint is the SHA-256 of PublicKey, hex-encoded
	// lowercase. 64 chars. The `policy.keys.list_active`
	// verb returns this verbatim.
	Fingerprint string
	// PublicKey is the Ed25519 public key bytes (32 bytes).
	// The Manager validates the length on every load /
	// insert; the DB enforces the same shape via CHECK.
	PublicKey []byte
	// Handle is the opaque KMS reference for resolving the
	// sealed private key at Sign time.
	Handle KeyHandle
	// ValidFrom is the wall-clock moment the key entered
	// service. Derived `valid_until` is computed against the
	// NEXT row's ValidFrom + overlap.
	ValidFrom time.Time
	// Algorithm pins the closed-set algorithm label. v1
	// always `"ed25519"`.
	Algorithm string
}

// ActiveKeyView is the shape the `policy.keys.list_active`
// read verb returns. Matches the brief verbatim: `[{key_id,
// fingerprint, valid_from, valid_until}]`. The full
// [KeyRecord] (public-key bytes, KMS handle) is intentionally
// NOT exposed -- the verb is for inventory / fingerprint
// reporting, not key distribution.
type ActiveKeyView struct {
	KeyID       uuid.UUID `json:"key_id"`
	Fingerprint string    `json:"fingerprint"`
	ValidFrom   time.Time `json:"valid_from"`
	ValidUntil  time.Time `json:"valid_until"`
}

// Sentinel errors returned by Manager / Bootstrap. Defined as
// exported sentinels so callers can branch via `errors.Is`
// rather than string-matching the message.
var (
	// ErrNoActiveKey is returned by Manager.Sign when no key
	// in the cache is currently active (every key is either
	// in the future or past its valid_until). Reachable only
	// in pathological clock skew / mis-bootstrap states; the
	// composition root treats it as a fatal startup error
	// per the `policy-signing-required=v1 required` pin.
	ErrNoActiveKey = errors.New("policy/keys: no active signing key")

	// ErrUnknownKey is returned by Manager.Verify when the
	// supplied key_id is not in the active set (either never
	// registered or already retired). The Evaluator Surface
	// translates this into the
	// `policy_signature_invalid` degraded short-circuit per
	// architecture Sec 8.2.
	ErrUnknownKey = errors.New("policy/keys: unknown or retired signing key")

	// ErrSignatureMismatch is returned by Manager.Verify when
	// the signature is well-formed but does not validate
	// against the named key's public bytes.
	ErrSignatureMismatch = errors.New("policy/keys: signature does not validate against key")

	// ErrRotationTooSoon is returned by Manager.Rotate when
	// the caller attempts a normal rotation while the most
	// recent key is still inside its overlap window. The
	// `ForceRotate` overload bypasses this for the
	// tech-spec Sec 9.3 compromise / emergency path.
	ErrRotationTooSoon = errors.New("policy/keys: rotation rejected; overlap window has not elapsed (use ForceRotate for compromise rotations)")

	// ErrInvalidPublicKey is returned when the KMS supplies a
	// public key whose byte length is not [Ed25519PublicKeySize].
	// Reachable only via a buggy or malicious KMS shim.
	ErrInvalidPublicKey = errors.New("policy/keys: KMS returned a non-Ed25519-shaped public key")

	// ErrKMSUnavailable is returned by Bootstrap when the KMS
	// fails to respond to the startup health-check. The
	// composition root surfaces this as a non-zero exit per
	// implementation-plan Stage 5.1 scenario
	// `kms-unavailable-blocks-start`.
	ErrKMSUnavailable = errors.New("policy/keys: KMS is unreachable; refusing to start under `policy-signing-required=v1 required`")
)
