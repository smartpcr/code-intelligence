package composition

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/audit/reconciler"
	"forge/services/clean-code/internal/policy/keys"
)

// newTestKeysManager constructs a Manager backed by the
// in-memory KMS, loaded and rotated to a non-empty cache.
func newTestKeysManager(t *testing.T) *keys.Manager {
	t.Helper()
	kms := keys.NewInMemoryKMS(nil)
	store := keys.NewInMemoryStore()
	m, err := keys.NewManager(keys.Config{KMS: kms, Store: store, Overlap: keys.DefaultOverlap})
	if err != nil {
		t.Fatalf("keys.NewManager: %v", err)
	}
	ctx := context.Background()
	if err := m.Load(ctx); err != nil {
		t.Fatalf("Manager.Load: %v", err)
	}
	if _, err := m.Rotate(ctx); err != nil {
		t.Fatalf("Manager.Rotate: %v", err)
	}
	return m
}

// newTestDB returns a sqlmock-backed *sql.DB and a cleanup
// closure.
func newTestDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return db, mock, func() { db.Close() }
}

// stageHistoricalKey is a helper that inserts a fresh
// ed25519 keypair into `store` as a historical signing-key
// row. Returns the keyID and the private key so the test
// can sign payloads with the same key the verifier will
// resolve from `store`.
func stageHistoricalKey(t *testing.T, store keys.Store, validFrom time.Time) (uuid.UUID, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	keyID, err := uuid.NewV4()
	if err != nil {
		t.Fatalf("uuid.NewV4: %v", err)
	}
	sum := sha256.Sum256(pub)
	fp := hex.EncodeToString(sum[:])
	rec := keys.KeyRecord{
		KeyID:       keyID,
		Fingerprint: fp,
		PublicKey:   []byte(pub),
		Handle:      keys.KeyHandle("test-handle-" + keyID.String()),
		ValidFrom:   validFrom,
		Algorithm:   "ed25519",
	}
	if err := store.Insert(context.Background(), rec); err != nil {
		t.Fatalf("store.Insert: %v", err)
	}
	return keyID, priv
}

// TestNewKeysManagerWALVerifier_NilManagerReturnsNil pins
// the scaffold-mode branch: when the composition root has
// not wired a [keys.Manager], the adapter returns nil so
// the caller can branch on "reconciler disabled". This
// matches the [NewKeysManagerWALSigner] convention.
func TestNewKeysManagerWALVerifier_NilManagerReturnsNil(t *testing.T) {
	t.Parallel()
	if got := NewKeysManagerWALVerifier(nil); got != nil {
		t.Errorf("NewKeysManagerWALVerifier(nil) = %v; want nil so caller branches on scaffold-mode", got)
	}
}

// TestNewKeysManagerWALVerifier_VerifiesActiveKeySignature
// is the integration assertion for the production WAL
// verification path: a frame signed via
// [NewKeysManagerWALSigner] MUST verify through
// [NewKeysManagerWALVerifier].
func TestNewKeysManagerWALVerifier_VerifiesActiveKeySignature(t *testing.T) {
	t.Parallel()
	m := newTestKeysManager(t)
	ctx := context.Background()

	// Sign a tiny payload via the manager's active key,
	// then verify it via the adapter.
	payload := []byte("audit-wal-v1\n{}")
	signer := NewKeysManagerWALSigner(m)
	keyID, sig, err := signer.SignFrame(ctx, func(uuid.UUID) ([]byte, error) { return payload, nil })
	if err != nil {
		t.Fatalf("signer.SignFrame: %v", err)
	}

	v := NewKeysManagerWALVerifier(m)
	if v == nil {
		t.Fatal("NewKeysManagerWALVerifier(non-nil) = nil; want non-nil")
	}
	if err := v.Verify(ctx, keyID, payload, sig); err != nil {
		t.Errorf("verifier.Verify: %v; want nil (production sig MUST verify)", err)
	}
}

// TestNewKeysManagerWALVerifier_TamperedSignatureClassifiesAsInvalid
// pins the sentinel mapping: a frame whose signature does
// NOT match the historical public key MUST wrap
// reconciler.ErrSignatureInvalid so the reconciler's
// classifier branches into "SkippedBadSig" rather than
// "abort Run".
func TestNewKeysManagerWALVerifier_TamperedSignatureClassifiesAsInvalid(t *testing.T) {
	t.Parallel()
	m := newTestKeysManager(t)
	ctx := context.Background()

	payload := []byte("audit-wal-v1\n{}")
	signer := NewKeysManagerWALSigner(m)
	keyID, sig, err := signer.SignFrame(ctx, func(uuid.UUID) ([]byte, error) { return payload, nil })
	if err != nil {
		t.Fatalf("signer.SignFrame: %v", err)
	}
	// Flip the last signature byte.
	sig[len(sig)-1] ^= 0x01

	v := NewKeysManagerWALVerifier(m)
	err = v.Verify(ctx, keyID, payload, sig)
	if !errors.Is(err, reconciler.ErrSignatureInvalid) {
		t.Fatalf("Verify tampered sig: err = %v; want wraps reconciler.ErrSignatureInvalid", err)
	}
}

// TestNewKeysManagerWALVerifier_UnknownKeyClassifiesAsSigningKeyUnknown
// pins the Stage 9.2 iter-2 semantic change for the
// historical-keys verifier: a frame whose `signing_key_id`
// is NOT in the historical snapshot (truly unregistered)
// MUST classify as a per-frame skip via
// [reconciler.ErrSigningKeyUnknown] -- NOT abort Run.
//
// Rationale: the iter-1 fail-loud "unknown -> abort" was a
// temporary stopgap while the verifier still consulted the
// live [keys.Manager.Verify] active-window check (which
// rejects retired keys as ErrUnknownKey, conflating them
// with truly-unknown keys). The Stage 9.2 production
// historical-keys verifier consults the FULL
// `policy_signing_keys` table including retired rows -- so
// "key not in snapshot" now unambiguously means "truly
// unregistered, attacker forgery or stale snapshot", which
// is exactly what ErrSigningKeyUnknown classifies.
func TestNewKeysManagerWALVerifier_UnknownKeyClassifiesAsSigningKeyUnknown(t *testing.T) {
	t.Parallel()
	m := newTestKeysManager(t)
	ctx := context.Background()
	v := NewKeysManagerWALVerifier(m)
	bogus := uuid.Must(uuid.NewV4())
	err := v.Verify(ctx, bogus, []byte("x"), make([]byte, ed25519.SignatureSize))
	if err == nil {
		t.Fatal("Verify with unknown key: err = nil; want non-nil")
	}
	if !errors.Is(err, reconciler.ErrSigningKeyUnknown) {
		t.Errorf("Verify with unknown key: err = %v; want wraps reconciler.ErrSigningKeyUnknown so the reconciler skip-and-counts", err)
	}
}

// TestNewKeysManagerWALVerifier_ContextCancelled pins that
// a cancelled ctx propagates as a NON-sentinel error so the
// reconciler treats it as transient and aborts Run rather
// than silently classifying it as a bad signature.
func TestNewKeysManagerWALVerifier_ContextCancelled(t *testing.T) {
	t.Parallel()
	m := newTestKeysManager(t)
	v := NewKeysManagerWALVerifier(m)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := v.Verify(ctx, uuid.Must(uuid.NewV4()), []byte("x"), make([]byte, ed25519.SignatureSize))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Verify on cancelled ctx: err = %v; want wraps context.Canceled", err)
	}
	if errors.Is(err, reconciler.ErrSigningKeyUnknown) || errors.Is(err, reconciler.ErrSignatureInvalid) {
		t.Errorf("Verify on cancelled ctx: err = %v; MUST NOT classify as skip-able sentinel", err)
	}
}

// TestNewHistoricalKeysWALVerifier_NilStoreReturnsNil pins
// the scaffold-mode branch for the explicit Store factory:
// nil store -> (nil, nil) so the WALReconcilerConfig path
// can branch on "reconciler disabled" identically to the
// Manager-based path.
func TestNewHistoricalKeysWALVerifier_NilStoreReturnsNil(t *testing.T) {
	t.Parallel()
	v, err := NewHistoricalKeysWALVerifier(context.Background(), nil)
	if err != nil {
		t.Fatalf("NewHistoricalKeysWALVerifier(nil): err = %v; want nil", err)
	}
	if v != nil {
		t.Errorf("NewHistoricalKeysWALVerifier(nil): got %v; want nil", v)
	}
}

// TestNewHistoricalKeysWALVerifier_VerifiesRetiredKeySignature
// is the load-bearing Stage 9.2 contract pinned by
// `internal/audit/reconciler/types.go`'s Verifier interface
// doc: "a frame signed yesterday by a now-retired key must
// still verify on replay".
//
// Setup: insert TWO keys into a fresh InMemoryStore --
// one with `ValidFrom` 30 days ago (the "retired" key) and
// one with `ValidFrom` now (the active key). The retired
// key's `valid_until` (derived from the next row's
// ValidFrom + overlap) is well in the past by Manager
// standards, so [keys.Manager.Verify] would reject it with
// ErrUnknownKey. The historical verifier MUST NOT impose
// that check: it MUST accept a signature produced by the
// retired key's private half because the row is still in
// the historical signing-key table.
func TestNewHistoricalKeysWALVerifier_VerifiesRetiredKeySignature(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := keys.NewInMemoryStore()

	// Retired key: ValidFrom 30 days ago.
	retiredKeyID, retiredPriv := stageHistoricalKey(t, store, time.Now().Add(-30*24*time.Hour))
	// Active key: ValidFrom now. The retired key's
	// derived valid_until = active.ValidFrom + overlap is
	// 24h+ in the past, so the Manager's window check
	// would reject the retired key. The historical
	// verifier MUST bypass that check.
	stageHistoricalKey(t, store, time.Now())

	v, err := NewHistoricalKeysWALVerifier(ctx, store)
	if err != nil {
		t.Fatalf("NewHistoricalKeysWALVerifier: %v", err)
	}

	payload := []byte("audit-wal-v1\n{retired-but-still-trusted}")
	sig := ed25519.Sign(retiredPriv, payload)

	if verr := v.Verify(ctx, retiredKeyID, payload, sig); verr != nil {
		t.Errorf("Verify retired-key sig: %v; want nil (historical verifier MUST accept retired-window keys per types.go Verifier contract)", verr)
	}
}

// TestNewHistoricalKeysWALVerifier_UnknownKeySentinelSkip
// pins the production sentinel mapping for the explicit
// Store factory path: a key_id not present in the
// snapshot (truly unregistered) -> ErrSigningKeyUnknown.
func TestNewHistoricalKeysWALVerifier_UnknownKeySentinelSkip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := keys.NewInMemoryStore()
	stageHistoricalKey(t, store, time.Now())

	v, err := NewHistoricalKeysWALVerifier(ctx, store)
	if err != nil {
		t.Fatalf("NewHistoricalKeysWALVerifier: %v", err)
	}

	bogus := uuid.Must(uuid.NewV4())
	verr := v.Verify(ctx, bogus, []byte("x"), make([]byte, ed25519.SignatureSize))
	if !errors.Is(verr, reconciler.ErrSigningKeyUnknown) {
		t.Errorf("Verify with unknown key_id: err = %v; want wraps reconciler.ErrSigningKeyUnknown", verr)
	}
}

// TestNewHistoricalKeysWALVerifier_TamperedSignatureSentinelSkip
// pins the production sentinel mapping for the
// known-key-but-bad-signature path: ed25519.Verify=false
// -> ErrSignatureInvalid.
func TestNewHistoricalKeysWALVerifier_TamperedSignatureSentinelSkip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := keys.NewInMemoryStore()
	keyID, priv := stageHistoricalKey(t, store, time.Now())

	v, err := NewHistoricalKeysWALVerifier(ctx, store)
	if err != nil {
		t.Fatalf("NewHistoricalKeysWALVerifier: %v", err)
	}

	payload := []byte("legit payload")
	sig := ed25519.Sign(priv, payload)
	sig[0] ^= 0xFF // flip a byte

	verr := v.Verify(ctx, keyID, payload, sig)
	if !errors.Is(verr, reconciler.ErrSignatureInvalid) {
		t.Errorf("Verify tampered sig: err = %v; want wraps reconciler.ErrSignatureInvalid", verr)
	}
}

// TestNewHistoricalKeysWALVerifier_StoreListErrorPropagates
// pins that a Store.List failure during snapshot
// construction is reported as a non-sentinel error so the
// caller (NewWALReconciler) can treat it as transient
// infra and refuse to construct the reconciler.
func TestNewHistoricalKeysWALVerifier_StoreListErrorPropagates(t *testing.T) {
	t.Parallel()
	listErr := errors.New("simulated DB outage")
	store := errorStore{err: listErr}
	v, err := NewHistoricalKeysWALVerifier(context.Background(), store)
	if err == nil {
		t.Fatal("NewHistoricalKeysWALVerifier with failing Store: err = nil; want non-nil")
	}
	if v != nil {
		t.Errorf("NewHistoricalKeysWALVerifier with failing Store: v = %v; want nil", v)
	}
	if !errors.Is(err, listErr) {
		t.Errorf("error chain: got %v; want wraps %v", err, listErr)
	}
}

// errorStore is a `keys.Store` whose List always returns
// the canned error -- used to exercise the snapshot-fetch
// failure path in the historical-keys factory.
type errorStore struct{ err error }

func (e errorStore) Insert(context.Context, keys.KeyRecord) error {
	return e.err
}

func (e errorStore) List(context.Context) ([]keys.KeyRecord, error) {
	return nil, e.err
}

// TestNewWALReconciler_NilKeysAndNilStoreReturnsNilNilForScaffoldMode:
// when the composition root has wired NEITHER a
// `*keys.Manager` NOR a `keys.Store`, the factory MUST
// return `(nil, nil)` so the binary can branch on
// "reconciler disabled" without classifying the missing
// dependency as an error.
func TestNewWALReconciler_NilKeysAndNilStoreReturnsNilNilForScaffoldMode(t *testing.T) {
	t.Parallel()
	r, err := NewWALReconciler(context.Background(), WALReconcilerConfig{})
	if err != nil {
		t.Fatalf("NewWALReconciler with nil Keys + nil KeyStore: err = %v; want nil", err)
	}
	if r != nil {
		t.Errorf("NewWALReconciler with nil Keys + nil KeyStore: got %v; want nil so binary branches on scaffold-mode", r)
	}
}

// TestNewWALReconciler_NilDBIsLoudError: with a non-nil
// Keys but a nil DB, the factory MUST refuse to construct.
// A silent "scaffold-mode" fallback here would skip the
// production-role check (clean_code_wal_reconciler).
func TestNewWALReconciler_NilDBIsLoudError(t *testing.T) {
	t.Parallel()
	m := newTestKeysManager(t)
	_, err := NewWALReconciler(context.Background(), WALReconcilerConfig{Keys: m, Dir: t.TempDir()})
	if err == nil {
		t.Fatal("NewWALReconciler with nil DB: want error, got nil")
	}
	if !strings.Contains(err.Error(), "DB is nil") {
		t.Fatalf("NewWALReconciler with nil DB: err = %v; want it to mention nil DB", err)
	}
}

// TestNewWALReconciler_EmptyDirIsLoudError: with non-nil
// Keys + non-nil DB but no Dir, the factory MUST refuse.
func TestNewWALReconciler_EmptyDirIsLoudError(t *testing.T) {
	t.Parallel()
	m := newTestKeysManager(t)
	db, _, cleanup := newTestDB(t)
	defer cleanup()
	_, err := NewWALReconciler(context.Background(), WALReconcilerConfig{Keys: m, DB: db})
	if err == nil {
		t.Fatal("NewWALReconciler with empty Dir: want error, got nil")
	}
	if !strings.Contains(err.Error(), "Dir is required") {
		t.Fatalf("NewWALReconciler with empty Dir: err = %v; want it to mention Dir", err)
	}
}

// TestNewWALReconciler_HappyPathConstructsWithKeysManager:
// all fields wired with the Manager-based path -> factory
// returns a non-nil reconciler.
func TestNewWALReconciler_HappyPathConstructsWithKeysManager(t *testing.T) {
	t.Parallel()
	m := newTestKeysManager(t)
	db, _, cleanup := newTestDB(t)
	defer cleanup()
	r, err := NewWALReconciler(context.Background(), WALReconcilerConfig{
		Keys: m,
		DB:   db,
		Dir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewWALReconciler: %v", err)
	}
	if r == nil {
		t.Fatal("NewWALReconciler happy path: got nil reconciler")
	}
}

// TestNewWALReconciler_HappyPathConstructsWithKeyStore:
// the production path -- explicit `KeyStore` (a
// `keys.Store` built on the reconciler's DB pool) instead
// of a `*keys.Manager`. Factory returns a non-nil
// reconciler. This is the path
// `cmd/clean-code-eval-gate/main.go` and
// `cmd/clean-code-gateway/main.go` exercise.
func TestNewWALReconciler_HappyPathConstructsWithKeyStore(t *testing.T) {
	t.Parallel()
	store := keys.NewInMemoryStore()
	stageHistoricalKey(t, store, time.Now())
	db, _, cleanup := newTestDB(t)
	defer cleanup()
	r, err := NewWALReconciler(context.Background(), WALReconcilerConfig{
		KeyStore: store,
		DB:       db,
		Dir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewWALReconciler with KeyStore: %v", err)
	}
	if r == nil {
		t.Fatal("NewWALReconciler with KeyStore: got nil reconciler")
	}
}

// TestNewWALReconciler_KeyStoreFailurePropagates: when the
// snapshot fetch fails (transient DB outage during
// startup), the factory MUST refuse to construct the
// reconciler so the binary fail-fasts rather than serving
// traffic with a half-wired audit path.
func TestNewWALReconciler_KeyStoreFailurePropagates(t *testing.T) {
	t.Parallel()
	listErr := errors.New("PG connection refused")
	db, _, cleanup := newTestDB(t)
	defer cleanup()
	_, err := NewWALReconciler(context.Background(), WALReconcilerConfig{
		KeyStore: errorStore{err: listErr},
		DB:       db,
		Dir:      t.TempDir(),
	})
	if err == nil {
		t.Fatal("NewWALReconciler with failing KeyStore: err = nil; want non-nil")
	}
	if !errors.Is(err, listErr) {
		t.Errorf("error chain: got %v; want wraps %v", err, listErr)
	}
}
