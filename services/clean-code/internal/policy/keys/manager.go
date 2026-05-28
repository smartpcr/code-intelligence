package keys

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/gofrs/uuid"
)

// DefaultOverlap is the canonical key-rotation overlap pinned
// by tech-spec Sec 8.2 row 6: 86400 seconds (24h). The Manager
// requires the duration to be > 0; the composition root in
// `cmd/clean-coded/main.go` sources the value from
// `config.PolicyPublishOverlapSeconds` so an operator can pin
// a longer overlap via env without touching this code.
const DefaultOverlap = 24 * time.Hour

// Config bundles the Manager's wiring. Every field is optional
// in the sense that NewManager falls back to a sensible default
// when the field is zero -- except for KMS and Store, both of
// which are required (NewManager returns an error if either is
// nil).
type Config struct {
	// KMS is the operator's secret manager. Required.
	KMS KMS
	// Store persists the public-side rows. Required.
	Store Store
	// Overlap is the key-rotation overlap window. Defaults to
	// [DefaultOverlap] (24h) when zero. Must be > 0.
	Overlap time.Duration
	// Clock returns the current wall-clock time. Defaults to
	// time.Now when nil. Tests inject a controllable clock to
	// exercise the overlap-window boundary deterministically.
	Clock func() time.Time
	// UUIDGen returns a fresh UUID. Defaults to
	// `uuid.NewV4`. Tests inject a deterministic generator to
	// pin assertion-friendly key_ids.
	UUIDGen func() (uuid.UUID, error)
}

// Manager owns the in-memory signing-key cache and exposes the
// Sign / Verify / Rotate / ListActive surface the rest of the
// service consumes. The Manager is safe for concurrent use:
// every read serialises through a RWMutex and every mutation
// holds the write lock.
//
// Lifecycle:
//
//  1. NewManager wires the dependencies.
//
//  2. Load fetches the current Store snapshot into the in-memory
//     cache. Bootstrap calls Load before returning to the
//     caller so the `signing_key_cache` health-gate flips to
//     "ready" without further intervention.
//
//  3. Sign / Verify / ListActive operate against the cache and
//     never touch the Store on the hot path. Rotate appends a
//     new row to the Store AND refreshes the cache in the same
//     critical section.
type Manager struct {
	kms     KMS
	store   Store
	overlap time.Duration
	clock   func() time.Time
	newID   func() (uuid.UUID, error)

	mu    sync.RWMutex
	cache []KeyRecord
}

// NewManager constructs a Manager. Returns an error if cfg.KMS
// or cfg.Store is nil or cfg.Overlap is negative. Does NOT
// touch the KMS or the Store -- callers must follow up with
// Load / Bootstrap to populate the cache.
func NewManager(cfg Config) (*Manager, error) {
	if cfg.KMS == nil {
		return nil, fmt.Errorf("policy/keys: NewManager: cfg.KMS is required")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("policy/keys: NewManager: cfg.Store is required")
	}
	if cfg.Overlap < 0 {
		return nil, fmt.Errorf("policy/keys: NewManager: cfg.Overlap=%s must be >= 0", cfg.Overlap)
	}
	overlap := cfg.Overlap
	if overlap == 0 {
		overlap = DefaultOverlap
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	newID := cfg.UUIDGen
	if newID == nil {
		newID = uuid.NewV4
	}
	return &Manager{
		kms:     cfg.KMS,
		store:   cfg.Store,
		overlap: overlap,
		clock:   clock,
		newID:   newID,
	}, nil
}

// Overlap returns the configured rotation overlap window. Read
// by tests and the runbook tooling.
func (m *Manager) Overlap() time.Duration {
	return m.overlap
}

// Load refreshes the in-memory cache from the Store. Safe to
// call multiple times; later Loads atomically replace the
// cached snapshot.
func (m *Manager) Load(ctx context.Context) error {
	rows, err := m.store.List(ctx)
	if err != nil {
		return fmt.Errorf("policy/keys: store.List: %w", err)
	}
	sortRecords(rows)
	m.mu.Lock()
	m.cache = rows
	m.mu.Unlock()
	return nil
}

// Rotate mints a new Ed25519 keypair via the KMS, appends a
// row to the Store, and refreshes the cache. Refuses the
// rotation when the most-recent key is still inside its
// overlap window (returns [ErrRotationTooSoon]); use
// [Manager.ForceRotate] for compromise / emergency rotations
// per tech-spec Sec 9.3.
//
// Returns the new KeyRecord (copy) on success.
func (m *Manager) Rotate(ctx context.Context) (KeyRecord, error) {
	return m.rotate(ctx, false)
}

// ForceRotate behaves like Rotate but skips the overlap-window
// guard. Reserved for the tech-spec Sec 9.3 "signing key
// compromise" path where the operator needs to publish a new
// key immediately even if the previous one is still inside its
// 24h window.
func (m *Manager) ForceRotate(ctx context.Context) (KeyRecord, error) {
	return m.rotate(ctx, true)
}

func (m *Manager) rotate(ctx context.Context, force bool) (KeyRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.clock()
	if !force {
		if newest, ok := newestRecord(m.cache); ok {
			minNextRotation := newest.ValidFrom.Add(m.overlap)
			if now.Before(minNextRotation) {
				return KeyRecord{}, fmt.Errorf(
					"%w: most recent key %s entered service at %s; next rotation eligible at %s",
					ErrRotationTooSoon,
					newest.KeyID,
					newest.ValidFrom.UTC().Format(time.RFC3339),
					minNextRotation.UTC().Format(time.RFC3339),
				)
			}
		}
	}

	pub, handle, err := m.kms.Generate(ctx)
	if err != nil {
		return KeyRecord{}, fmt.Errorf("policy/keys: kms.Generate: %w", err)
	}
	if len(pub) != Ed25519PublicKeySize {
		return KeyRecord{}, fmt.Errorf("%w: got %d bytes", ErrInvalidPublicKey, len(pub))
	}
	id, err := m.newID()
	if err != nil {
		return KeyRecord{}, fmt.Errorf("policy/keys: uuid generation: %w", err)
	}
	rec := KeyRecord{
		KeyID:       id,
		Fingerprint: Fingerprint(pub),
		PublicKey:   pub,
		Handle:      handle,
		ValidFrom:   now,
		Algorithm:   "ed25519",
	}
	if err := m.store.Insert(ctx, rec); err != nil {
		return KeyRecord{}, fmt.Errorf("policy/keys: store.Insert: %w", err)
	}
	// Refresh the cache by re-listing from the store. This
	// keeps the in-memory view authoritative even when the
	// backing store mutates rows on insert (e.g. defaulting
	// `valid_from` from the DB's `now()`).
	rows, err := m.store.List(ctx)
	if err != nil {
		return KeyRecord{}, fmt.Errorf("policy/keys: store.List post-insert: %w", err)
	}
	sortRecords(rows)
	m.cache = rows
	// Return the canonical row from the refreshed cache so the
	// caller sees the same ValidFrom the cache will use for
	// overlap arithmetic.
	for _, r := range rows {
		if r.KeyID == id {
			return copyRecord(r), nil
		}
	}
	return copyRecord(rec), nil
}

// Sign signs payload under the newest key whose
// `valid_from <= now`. Returns the KeyID used and the Ed25519
// signature.
//
// In the steady state this is the most recent key in the
// cache. During a rotation that races against an unsynchronised
// app-clock the previous key is used until the new key's
// `valid_from` is reached -- see rubber-duck critique #5 for
// the rationale.
//
// Defence-in-depth: after the KMS produces a signature, we
// verify it against the chosen row's public key BEFORE
// returning. A `kms.Sign` that returns a signature which does
// NOT validate against the public_key persisted in the row
// indicates one of (a) DB corruption, (b) a handle/pubkey swap
// (e.g. a row mis-edit), (c) a KMS bug. Catching the mismatch
// here is far cheaper than every downstream verifier
// catching it later -- per rubber-duck critique #3.
func (m *Manager) Sign(ctx context.Context, payload []byte) (uuid.UUID, []byte, error) {
	m.mu.RLock()
	cache := append([]KeyRecord(nil), m.cache...)
	overlap := m.overlap
	m.mu.RUnlock()

	now := m.clock()
	chosen, ok := signingKey(cache, now, overlap)
	if !ok {
		return uuid.Nil, nil, ErrNoActiveKey
	}
	sig, err := m.kms.Sign(ctx, chosen.Handle, payload)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("policy/keys: kms.Sign for key_id=%s: %w", chosen.KeyID, err)
	}
	if len(sig) != Ed25519SignatureSize {
		return uuid.Nil, nil, fmt.Errorf("policy/keys: kms.Sign returned %d bytes; want %d", len(sig), Ed25519SignatureSize)
	}
	if err := verifyAgainstPublic(chosen.PublicKey, payload, sig); err != nil {
		return uuid.Nil, nil, fmt.Errorf("policy/keys: kms.Sign result does not validate against key_id=%s public key: %w", chosen.KeyID, err)
	}
	return chosen.KeyID, sig, nil
}

// SignActive is the 2-phase signing path the Audit WAL writer
// uses to bind the signing-key id INTO the canonical signing
// payload before producing the signature. It picks the same
// active key [Manager.Sign] would (newest with
// `valid_from <= now`), calls `build(keyID)` to obtain the
// caller-shaped payload bytes, then runs the KMS signature +
// verify-against-public defence-in-depth chain Sign uses.
//
// Why a separate method (vs. composing
// [Manager.Sign] + a "peek the active key" accessor):
//
//   - The keyID and the signature MUST come from the same
//     critical-section view of the cache. A peek-then-sign
//     pair would race against an in-flight [Manager.Rotate]
//     and produce a frame whose `signing_key_id` field does
//     not match the key that produced its signature.
//   - The Audit WAL frame format hashes the keyID INTO the
//     bytes the signer signs (see
//     `internal/audit/wal.AuditFrame.SigningPayload`). A
//     verifier that re-derives the payload from the on-disk
//     frame must see the same keyID the signer hashed in;
//     SignActive's callback contract gives the caller
//     exactly one shot at building those bytes against the
//     correct keyID.
//
// Returns:
//
//   - [ErrNoActiveKey] when the cache holds no key whose
//     valid_from <= now (mis-bootstrap).
//   - ctx errors when the caller cancels.
//   - the first error returned by `build`; wrapped so
//     `errors.Is(err, sentinel)` works.
//   - KMS / verify errors with the same wrapping shape Sign
//     uses, so the error chain stays consistent.
func (m *Manager) SignActive(ctx context.Context, build func(keyID uuid.UUID) ([]byte, error)) (uuid.UUID, []byte, error) {
	if err := ctx.Err(); err != nil {
		return uuid.Nil, nil, err
	}
	if build == nil {
		return uuid.Nil, nil, errors.New("policy/keys: Manager.SignActive: build is nil")
	}
	m.mu.RLock()
	cache := append([]KeyRecord(nil), m.cache...)
	overlap := m.overlap
	m.mu.RUnlock()

	now := m.clock()
	chosen, ok := signingKey(cache, now, overlap)
	if !ok {
		return uuid.Nil, nil, ErrNoActiveKey
	}
	payload, err := build(chosen.KeyID)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("policy/keys: Manager.SignActive: build: %w", err)
	}
	if len(payload) == 0 {
		return uuid.Nil, nil, errors.New("policy/keys: Manager.SignActive: build returned empty payload")
	}
	sig, err := m.kms.Sign(ctx, chosen.Handle, payload)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("policy/keys: Manager.SignActive: kms.Sign for key_id=%s: %w", chosen.KeyID, err)
	}
	if len(sig) != Ed25519SignatureSize {
		return uuid.Nil, nil, fmt.Errorf("policy/keys: Manager.SignActive: kms.Sign returned %d bytes; want %d", len(sig), Ed25519SignatureSize)
	}
	if err := verifyAgainstPublic(chosen.PublicKey, payload, sig); err != nil {
		return uuid.Nil, nil, fmt.Errorf("policy/keys: Manager.SignActive: signature does not validate against key_id=%s public key: %w", chosen.KeyID, err)
	}
	return chosen.KeyID, sig, nil
}

// Verify checks that signature was produced by keyID over
// payload AND that keyID is currently inside its active
// `[valid_from, valid_until)` window. Returns one of:
//
//   - nil on success;
//   - [ErrUnknownKey] if keyID is not in the cache or is
//     outside its active window;
//   - [ErrSignatureMismatch] if the signature does not validate
//     against the key's public bytes.
func (m *Manager) Verify(ctx context.Context, keyID uuid.UUID, payload []byte, signature []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.RLock()
	cache := append([]KeyRecord(nil), m.cache...)
	overlap := m.overlap
	m.mu.RUnlock()
	now := m.clock()
	for i, rec := range cache {
		if rec.KeyID != keyID {
			continue
		}
		validFrom := rec.ValidFrom
		validUntil := computeValidUntil(cache, i, overlap)
		if now.Before(validFrom) {
			return fmt.Errorf("%w: key_id=%s is not yet active (valid_from=%s, now=%s)",
				ErrUnknownKey, keyID, validFrom.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
		}
		if !now.Before(validUntil) {
			return fmt.Errorf("%w: key_id=%s expired (valid_until=%s, now=%s)",
				ErrUnknownKey, keyID, validUntil.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
		}
		return verifyAgainstPublic(rec.PublicKey, payload, signature)
	}
	return fmt.Errorf("%w: key_id=%s", ErrUnknownKey, keyID)
}

// VerifyAny is the fallback path that tries every active key
// in the cache and returns the first match. Used by tests and
// by the WAL Reconciler when replaying audit rows whose
// signing-key metadata was not preserved alongside the
// signature. Production hot paths SHOULD use [Manager.Verify]
// with an explicit key_id.
//
// Returns the matching KeyID on success; [ErrUnknownKey] if no
// active key validates the signature.
func (m *Manager) VerifyAny(ctx context.Context, payload []byte, signature []byte) (uuid.UUID, error) {
	if err := ctx.Err(); err != nil {
		return uuid.Nil, err
	}
	if len(signature) != Ed25519SignatureSize {
		return uuid.Nil, ErrSignatureMismatch
	}
	m.mu.RLock()
	cache := append([]KeyRecord(nil), m.cache...)
	overlap := m.overlap
	m.mu.RUnlock()
	now := m.clock()
	for i, rec := range cache {
		validFrom := rec.ValidFrom
		validUntil := computeValidUntil(cache, i, overlap)
		if now.Before(validFrom) || !now.Before(validUntil) {
			continue
		}
		if err := verifyAgainstPublic(rec.PublicKey, payload, signature); err == nil {
			return rec.KeyID, nil
		}
	}
	return uuid.Nil, ErrUnknownKey
}

// ListActive returns the `policy.keys.list_active` projection:
// `[{key_id, fingerprint, valid_from, valid_until}]` for every
// key currently inside `[valid_from, valid_until)`. Result is
// sorted by `ValidFrom DESC, KeyID DESC` so the newest key
// appears first (matches the index defined in the migration).
func (m *Manager) ListActive(ctx context.Context) ([]ActiveKeyView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	cache := append([]KeyRecord(nil), m.cache...)
	overlap := m.overlap
	m.mu.RUnlock()
	now := m.clock()
	out := make([]ActiveKeyView, 0, len(cache))
	for i, rec := range cache {
		validUntil := computeValidUntil(cache, i, overlap)
		if now.Before(rec.ValidFrom) {
			continue
		}
		if !now.Before(validUntil) {
			continue
		}
		out = append(out, ActiveKeyView{
			KeyID:       rec.KeyID,
			Fingerprint: rec.Fingerprint,
			ValidFrom:   rec.ValidFrom,
			ValidUntil:  validUntil,
		})
	}
	// Sort newest-first so the runbook / dashboard always sees
	// the active key at the top.
	sort.Slice(out, func(i, j int) bool {
		if out[i].ValidFrom.Equal(out[j].ValidFrom) {
			return uuidCompare(out[i].KeyID, out[j].KeyID) > 0
		}
		return out[i].ValidFrom.After(out[j].ValidFrom)
	})
	return out, nil
}

// Snapshot returns a copy of the current in-memory cache. Used
// by tests and by the WAL Reconciler's audit re-verification
// path. The returned slice is owned by the caller.
func (m *Manager) Snapshot() []KeyRecord {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]KeyRecord, len(m.cache))
	for i, r := range m.cache {
		out[i] = copyRecord(r)
	}
	return out
}

// computeValidUntil returns the derived upper bound for the
// row at index i in cache. `cache` must already be sorted by
// `ValidFrom ASC, KeyID ASC`.
//
// Formula: `cache[i+1].ValidFrom + overlap`, or [SentinelValidUntil]
// when no successor exists. This is the canonical "next key's
// valid_from + overlap" formula from tech-spec Sec 8.4.
func computeValidUntil(cache []KeyRecord, i int, overlap time.Duration) time.Time {
	if i+1 < len(cache) {
		return cache[i+1].ValidFrom.Add(overlap)
	}
	return SentinelValidUntil
}

// signingKey picks the row Sign should use. Per rubber-duck
// critique #5 we pick the NEWEST key whose `ValidFrom <= now`
// (so a clock-skewed future-key row does not become the
// signing target). `cache` must be sorted ascending by
// ValidFrom.
func signingKey(cache []KeyRecord, now time.Time, overlap time.Duration) (KeyRecord, bool) {
	_ = overlap
	for i := len(cache) - 1; i >= 0; i-- {
		if !cache[i].ValidFrom.After(now) {
			return cache[i], true
		}
	}
	return KeyRecord{}, false
}

// newestRecord returns the most recent row in cache (highest
// `ValidFrom`, tie-break by `KeyID`). Used by Rotate's
// overlap guard.
func newestRecord(cache []KeyRecord) (KeyRecord, bool) {
	if len(cache) == 0 {
		return KeyRecord{}, false
	}
	return cache[len(cache)-1], true
}

// copyRecord deep-copies a KeyRecord (the only mutable field is
// the public-key byte slice).
func copyRecord(r KeyRecord) KeyRecord {
	out := r
	if r.PublicKey != nil {
		out.PublicKey = append([]byte(nil), r.PublicKey...)
	}
	return out
}

// Fingerprint computes the canonical SHA-256 fingerprint over
// an Ed25519 public key. Returned as lowercase hex matching
// the migration's CHECK constraint.
func Fingerprint(publicKey []byte) string {
	sum := sha256.Sum256(publicKey)
	return hex.EncodeToString(sum[:])
}

// StartRefresh spawns a goroutine that calls [Manager.Load]
// every `interval` until ctx is canceled. Returns immediately;
// the caller may discard the returned cancel function if ctx
// is the right cancellation handle.
//
// Why a periodic refresh is necessary: in a multi-replica
// deployment, replica A may rotate a new key into the SQL
// store while replica B's in-memory cache still only sees the
// old key. Without a refresh, replica B will reject signatures
// produced by replica A's new key as ErrUnknownKey until the
// process restarts. The refresh closes that window to at most
// `interval`. The 24h overlap guarantees both keys remain
// valid for long enough that the refresh races safely.
//
// Pass `interval == 0` to disable refresh (the caller can also
// just not call StartRefresh at all -- this is for symmetry
// with cfg-driven wiring that may default to zero).
//
// Each refresh failure is reported via the `onError` callback
// when non-nil; otherwise it is silently retried on the next
// tick. The callback fires asynchronously from the ticker
// goroutine and MUST be safe for concurrent invocation.
func (m *Manager) StartRefresh(ctx context.Context, interval time.Duration, onError func(error)) (cancel func()) {
	if interval <= 0 {
		return func() {}
	}
	refreshCtx, cancelFn := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-refreshCtx.Done():
				return
			case <-ticker.C:
				if err := m.Load(refreshCtx); err != nil && onError != nil {
					onError(err)
				}
			}
		}
	}()
	return cancelFn
}

// Bootstrap is the composition-root entry point. Steps:
//
//  1. Probe the KMS. On error returns a wrapped
//     [ErrKMSUnavailable] so the composition root exits
//     non-zero per `policy-signing-required=v1 required`.
//
//  2. Load the active set into the cache.
//
//  3. If the cache is empty AND `mintFirstKey` is true, mint
//     the first signing key via [Manager.ForceRotate]
//     (ForceRotate bypasses the overlap guard which is
//     vacuously true on an empty cache anyway, but ForceRotate
//     also documents the "first key" intent).
//
// Returns the Manager and a function suitable for registration
// with `internal/health.Handler.AddReadyCheck("signing_key_cache",
// fn)`. The check returns nil while the cache is non-empty AND
// the KMS responds to Ping; non-nil otherwise.
func Bootstrap(ctx context.Context, cfg Config, mintFirstKey bool) (*Manager, func(context.Context) error, error) {
	m, err := NewManager(cfg)
	if err != nil {
		return nil, nil, err
	}
	if err := m.kms.Ping(ctx); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrKMSUnavailable, err)
	}
	if err := m.Load(ctx); err != nil {
		return nil, nil, fmt.Errorf("policy/keys: initial Load: %w", err)
	}
	if mintFirstKey {
		m.mu.RLock()
		empty := len(m.cache) == 0
		m.mu.RUnlock()
		if empty {
			if _, err := m.ForceRotate(ctx); err != nil {
				return nil, nil, fmt.Errorf("policy/keys: mint first key: %w", err)
			}
		}
	}
	check := func(probeCtx context.Context) error {
		if err := m.kms.Ping(probeCtx); err != nil {
			return fmt.Errorf("policy/keys: KMS ping: %w", err)
		}
		views, err := m.ListActive(probeCtx)
		if err != nil {
			return fmt.Errorf("policy/keys: list_active: %w", err)
		}
		if len(views) == 0 {
			return errors.New("policy/keys: no active signing key in cache")
		}
		return nil
	}
	return m, check, nil
}
