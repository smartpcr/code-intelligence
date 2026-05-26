package webhook

import (
	"errors"
	"fmt"
	"sync"
)

// SigningKeyIDHeader is the canonical HTTP header the
// `/v1/ingest/{verb}` Router reads the publisher's
// `signing_key_id` from. The header value names the
// per-deployment HMAC secret the verifier should resolve
// before checking [HMACSignatureHeader].
//
// The header is REQUIRED whenever the Router is constructed
// with a non-nil [SecretResolver]; a missing value surfaces
// as [ErrSigningKeyIDMissing] (mapped to a structured 401 in
// the Router).
//
// Wire shape: `X-Signing-Key-Id: <opaque ASCII identifier>`.
// The identifier is treated as a token the publisher and the
// deployment agree on out-of-band -- the Router does NOT
// parse it, only looks it up in the [SecretResolver].
//
// # Why a header, not a body field
//
// The publisher signs the body BEFORE the server inspects
// any of it. The server needs the `signing_key_id` to
// resolve which secret to verify against -- so the id MUST
// be reachable from request metadata (headers) and not from
// the body itself, otherwise the server would have to parse
// the body before authenticating it (the contract probe
// vector the iter-6 audit closed).
const SigningKeyIDHeader = "X-Signing-Key-Id"

// MaxSigningKeyIDLength caps the [SigningKeyIDHeader] value
// the verifier will accept. 128 ASCII bytes is generous
// (UUIDs are 36; key-ring labels like `kv-prod-2026-q1` are
// well under 32). A larger value is treated as malformed so a
// malicious caller cannot use the header to amplify
// log-volume or trigger pathological map-lookup paths.
const MaxSigningKeyIDLength = 128

// SecretResolver maps a publisher-supplied `signing_key_id`
// to the per-deployment HMAC-SHA256 secret the Router uses to
// verify the request body.
//
// v1 is single-tenant per tech-spec Sec 4.14 (one logical org
// per deployment); there is NO `tenant_id` field on any
// resolver method or any backing table. A multi-tenant v2
// migration uses per-schema isolation (tech-spec Sec 10A
// "multi-tenant v2 shape" pin lines 1690-1696) -- the
// resolver shape does NOT pre-reserve a tenant column.
//
// # Sentinel error contract
//
// Implementations MUST return [ErrUnknownSigningKeyID] when
// the supplied identifier does not match any active
// per-deployment secret. The Router maps the sentinel onto a
// 401 + `HMAC_UNKNOWN_KEY_ID` response so the operator
// runbook can pattern-match on the code without parsing
// prose. Any other error type is treated as an internal
// resolver failure (500 / `INTERNAL_ERROR`).
//
// # Concurrency
//
// Implementations MUST be safe for concurrent invocation;
// the Router fires a Resolve call for every inbound request
// without external serialisation.
type SecretResolver interface {
	// Resolve returns the HMAC secret bound to `keyID`. A
	// nil error implies a non-empty secret; a non-nil error
	// MUST signal one of:
	//   - [ErrUnknownSigningKeyID] when no row matches
	//   - any other error when the lookup itself failed
	//     (e.g. a future PG-backed resolver loses its
	//     connection).
	Resolve(keyID string) ([]byte, error)
}

// Sentinel errors returned by [SecretResolver] implementations
// and the Router's HMAC pipeline.
var (
	// ErrSigningKeyIDMissing is returned when the inbound
	// request does not carry [SigningKeyIDHeader] (or
	// carries an empty value). Mapped to a 401 +
	// `HMAC_MISSING_KEY_ID` response.
	ErrSigningKeyIDMissing = errors.New("webhook: missing " + SigningKeyIDHeader + " header")

	// ErrSigningKeyIDMalformed is returned when the header
	// value exceeds [MaxSigningKeyIDLength] or contains
	// control characters. Mapped to a 401 +
	// `HMAC_MALFORMED_KEY_ID` response.
	ErrSigningKeyIDMalformed = errors.New("webhook: malformed " + SigningKeyIDHeader + " header")

	// ErrUnknownSigningKeyID is returned by a [SecretResolver]
	// when the supplied identifier does not match any active
	// per-deployment secret. Mapped to a 401 +
	// `HMAC_UNKNOWN_KEY_ID` response so an attacker probing
	// arbitrary key IDs cannot distinguish "unknown id" from
	// "bad signature" via status-code alone (the Router
	// keeps the response body the same shape; only the code
	// string differs in service-side logs).
	ErrUnknownSigningKeyID = errors.New("webhook: unknown signing_key_id")

	// ErrEmptyResolvedSecret is returned by [StaticSecretResolver]
	// when an entry is registered with a zero-length secret.
	// The Router treats this as an internal resolver failure
	// (500) because the misconfiguration is the operator's
	// problem, not the caller's.
	ErrEmptyResolvedSecret = errors.New("webhook: resolved HMAC secret is empty (resolver misconfiguration)")
)

// StaticSecretResolver is the v1 single-tenant
// [SecretResolver] implementation. It holds an in-memory map
// from `signing_key_id` to HMAC secret bytes; the operator
// seeds the map at startup from the deployment's secret
// manager (Key Vault / environment variable / etc.). The
// resolver is the in-process projection of the secret
// manager, NEVER the source of truth.
//
// # Why static (and what the v2 path is)
//
// v1 single-tenant has a small, well-known set of active
// signing keys -- typically ONE (the production secret) plus
// optionally ONE more during a rotation overlap. A
// dependency on a database or a remote vault for every
// inbound webhook request would be a needless availability
// hazard. The v2 multi-tenant migration replaces this
// resolver with a tenant-aware lookup (per-schema isolation
// per tech-spec Sec 10A); the SecretResolver INTERFACE is
// the seam that survives the migration -- callers do not
// depend on `StaticSecretResolver` directly.
//
// # Rotation
//
// Operators rotate by adding a row for the new id, then (24h
// later per tech-spec Sec 8.2 row 6) removing the old id.
// Both ids verify successfully during the overlap. Add/Remove
// are concurrency-safe; the Router and the rotation tool
// share one resolver instance.
type StaticSecretResolver struct {
	mu      sync.RWMutex
	secrets map[string][]byte
}

// NewStaticSecretResolver constructs a [StaticSecretResolver]
// seeded with `initial`. The map keys are signing-key
// identifiers; values are the HMAC secrets. PANICS when any
// secret in `initial` is zero-length (operator misconfig that
// should fail loudly at composition time, never at
// request-handling time).
//
// Pass an empty map (or nil) for a resolver that initially
// rejects every key; tests use the [Add] method to populate.
func NewStaticSecretResolver(initial map[string][]byte) *StaticSecretResolver {
	out := &StaticSecretResolver{secrets: make(map[string][]byte, len(initial))}
	for id, secret := range initial {
		if id == "" {
			panic("webhook: NewStaticSecretResolver received an empty signing_key_id")
		}
		if len(secret) == 0 {
			panic(fmt.Sprintf("webhook: NewStaticSecretResolver received empty secret for signing_key_id %q", id))
		}
		// Copy the slice so a caller mutating the input map
		// cannot poison the resolver post-construction.
		copySecret := make([]byte, len(secret))
		copy(copySecret, secret)
		out.secrets[id] = copySecret
	}
	return out
}

// Resolve implements [SecretResolver].
func (r *StaticSecretResolver) Resolve(keyID string) ([]byte, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	secret, ok := r.secrets[keyID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownSigningKeyID, keyID)
	}
	if len(secret) == 0 {
		// Defence-in-depth: NewStaticSecretResolver panics on
		// empty input, but a direct field touch (in tests)
		// could bypass that.
		return nil, ErrEmptyResolvedSecret
	}
	// Hand back a copy so the caller cannot mutate the
	// stored secret in place.
	out := make([]byte, len(secret))
	copy(out, secret)
	return out, nil
}

// Add registers `secret` under `keyID`. Overwrites any
// previous entry for the same id (the rotation tool calls
// this with a fresh secret when rolling the production key).
// PANICS on empty inputs for the same reasons as
// [NewStaticSecretResolver].
func (r *StaticSecretResolver) Add(keyID string, secret []byte) {
	if keyID == "" {
		panic("webhook: StaticSecretResolver.Add received an empty signing_key_id")
	}
	if len(secret) == 0 {
		panic(fmt.Sprintf("webhook: StaticSecretResolver.Add received empty secret for signing_key_id %q", keyID))
	}
	copySecret := make([]byte, len(secret))
	copy(copySecret, secret)
	r.mu.Lock()
	r.secrets[keyID] = copySecret
	r.mu.Unlock()
}

// Remove drops the entry for `keyID`. No-op if the id was
// never registered. Returns true iff a row was removed (the
// rotation tool surfaces this so an operator typo
// (`Remove("kv-old-2025")` when the id is `kv-old-2024`) is
// loud rather than silent).
func (r *StaticSecretResolver) Remove(keyID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.secrets[keyID]; !ok {
		return false
	}
	delete(r.secrets, keyID)
	return true
}

// Len returns the number of registered key ids. Exported so
// the composition root can assert "at least one key
// configured" at startup.
func (r *StaticSecretResolver) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.secrets)
}

// ValidateSigningKeyID returns nil iff `keyID` satisfies the
// shape contract documented on [SigningKeyIDHeader]. Used by
// the Router BEFORE the resolver is consulted so a malformed
// header is rejected with [ErrSigningKeyIDMalformed] and the
// resolver only sees well-formed inputs.
//
// Rules:
//
//   - Must be non-empty ([ErrSigningKeyIDMissing]).
//   - Length must be <= [MaxSigningKeyIDLength].
//   - Every byte must be printable ASCII (0x20..0x7E).
//
// The ASCII restriction blocks header-injection vectors
// (CR/LF, NUL, tabs) without overconstraining the
// label-shape the operator can use.
func ValidateSigningKeyID(keyID string) error {
	if keyID == "" {
		return ErrSigningKeyIDMissing
	}
	if len(keyID) > MaxSigningKeyIDLength {
		return fmt.Errorf("%w: length %d exceeds %d", ErrSigningKeyIDMalformed, len(keyID), MaxSigningKeyIDLength)
	}
	for i := 0; i < len(keyID); i++ {
		c := keyID[i]
		if c < 0x20 || c > 0x7E {
			return fmt.Errorf("%w: contains non-printable byte at offset %d", ErrSigningKeyIDMalformed, i)
		}
	}
	return nil
}
