package composition

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/audit/reconciler"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/keys"
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
// pins the sentinel mapping: keys.ErrSignatureMismatch
// MUST wrap reconciler.ErrSignatureInvalid so the
// reconciler's classifier branches into "SkippedBadSig"
// rather than "abort Run".
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

// TestNewKeysManagerWALVerifier_UnknownKeyAbortsRun
// pins the Stage 9.2 safety classification: keys.ErrUnknownKey
// MUST NOT be mapped to reconciler.ErrSigningKeyUnknown
// (which would be a per-frame skip). Instead it MUST be
// surfaced as a non-sentinel error so the reconciler
// aborts Run rather than silently dropping legitimate
// historical frames signed by a retired key. The raw
// keys.ErrUnknownKey cause MUST remain wrapped so callers
// can introspect via errors.Is.
func TestNewKeysManagerWALVerifier_UnknownKeyAbortsRun(t *testing.T) {
	t.Parallel()
	m := newTestKeysManager(t)
	ctx := context.Background()
	v := NewKeysManagerWALVerifier(m)
	bogus := uuid.Must(uuid.NewV4())
	err := v.Verify(ctx, bogus, []byte("x"), make([]byte, keys.Ed25519SignatureSize))
	if err == nil {
		t.Fatal("Verify with unknown key: err = nil; want non-nil")
	}
	if errors.Is(err, reconciler.ErrSigningKeyUnknown) {
		t.Errorf("Verify with unknown key: err = %v; MUST NOT classify as reconciler.ErrSigningKeyUnknown (would be silent skip)", err)
	}
	if errors.Is(err, reconciler.ErrSignatureInvalid) {
		t.Errorf("Verify with unknown key: err = %v; MUST NOT classify as reconciler.ErrSignatureInvalid (would be silent skip)", err)
	}
	if !errors.Is(err, keys.ErrUnknownKey) {
		t.Errorf("Verify with unknown key: err = %v; raw keys.ErrUnknownKey cause MUST remain wrapped", err)
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
	err := v.Verify(ctx, uuid.Must(uuid.NewV4()), []byte("x"), make([]byte, keys.Ed25519SignatureSize))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Verify on cancelled ctx: err = %v; want wraps context.Canceled", err)
	}
	if errors.Is(err, reconciler.ErrSigningKeyUnknown) || errors.Is(err, reconciler.ErrSignatureInvalid) {
		t.Errorf("Verify on cancelled ctx: err = %v; MUST NOT classify as skip-able sentinel", err)
	}
}

// TestNewWALReconciler_NilKeysReturnsNilNilForScaffoldMode:
// when the composition root has not wired a `*keys.Manager`,
// the factory MUST return `(nil, nil)` so the binary can
// branch on "reconciler disabled" without classifying the
// missing dependency as an error.
func TestNewWALReconciler_NilKeysReturnsNilNilForScaffoldMode(t *testing.T) {
	t.Parallel()
	r, err := NewWALReconciler(WALReconcilerConfig{})
	if err != nil {
		t.Fatalf("NewWALReconciler with nil Keys: err = %v; want nil", err)
	}
	if r != nil {
		t.Errorf("NewWALReconciler with nil Keys: got %v; want nil so binary branches on scaffold-mode", r)
	}
}

// TestNewWALReconciler_NilDBIsLoudError: with a non-nil
// Keys but a nil DB, the factory MUST refuse to construct.
// A silent "scaffold-mode" fallback here would skip the
// production-role check (clean_code_wal_reconciler).
func TestNewWALReconciler_NilDBIsLoudError(t *testing.T) {
	t.Parallel()
	m := newTestKeysManager(t)
	_, err := NewWALReconciler(WALReconcilerConfig{Keys: m, Dir: t.TempDir()})
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
	_, err := NewWALReconciler(WALReconcilerConfig{Keys: m, DB: db})
	if err == nil {
		t.Fatal("NewWALReconciler with empty Dir: want error, got nil")
	}
	if !strings.Contains(err.Error(), "Dir is required") {
		t.Fatalf("NewWALReconciler with empty Dir: err = %v; want it to mention Dir", err)
	}
}

// TestNewWALReconciler_HappyPathConstructs: all fields
// wired -> factory returns a non-nil reconciler.
func TestNewWALReconciler_HappyPathConstructs(t *testing.T) {
	t.Parallel()
	m := newTestKeysManager(t)
	db, _, cleanup := newTestDB(t)
	defer cleanup()
	r, err := NewWALReconciler(WALReconcilerConfig{
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
