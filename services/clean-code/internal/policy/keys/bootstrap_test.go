package keys

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"
)

// TestBootstrap_MintsFirstKeyOnEmptyDeployment exercises the
// composition-root path: fresh KMS + fresh Store + mintFirstKey
// = true -> Bootstrap returns a Manager with one active key
// AND a health-check that reports nil.
func TestBootstrap_MintsFirstKeyOnEmptyDeployment(t *testing.T) {
	t.Parallel()
	kms := NewInMemoryKMS(nil)
	store := NewInMemoryStore()
	clock := newFakeClock(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))

	mgr, check, err := Bootstrap(context.Background(), Config{
		KMS:   kms,
		Store: store,
		Clock: clock.Now,
	}, true)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if mgr == nil {
		t.Fatal("Bootstrap returned nil Manager")
	}
	if check == nil {
		t.Fatal("Bootstrap returned nil health check")
	}
	views, err := mgr.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("active set size = %d; want 1 after first-key mint", len(views))
	}
	if err := check(context.Background()); err != nil {
		t.Errorf("health check: %v; want nil", err)
	}
}

// TestBootstrap_ReusesExistingKeys covers the restart path:
// when the Store already has rows, Bootstrap must NOT mint a
// fresh first key.
func TestBootstrap_ReusesExistingKeys(t *testing.T) {
	t.Parallel()
	kms := NewInMemoryKMS(nil)
	store := NewInMemoryStore()
	clock := newFakeClock(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))

	// Pre-seed via a first Bootstrap.
	first, _, err := Bootstrap(context.Background(), Config{KMS: kms, Store: store, Clock: clock.Now}, true)
	if err != nil {
		t.Fatalf("first Bootstrap: %v", err)
	}
	wantKey := first.Snapshot()[0].KeyID

	// Re-Bootstrap on the same backing store. Even with
	// mintFirstKey=true the cache is non-empty so no new key
	// is minted.
	mgr2, _, err := Bootstrap(context.Background(), Config{KMS: kms, Store: store, Clock: clock.Now}, true)
	if err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
	got := mgr2.Snapshot()
	if len(got) != 1 {
		t.Fatalf("second Bootstrap saw %d cached keys; want 1 (no re-mint)", len(got))
	}
	if got[0].KeyID != wantKey {
		t.Errorf("second Bootstrap minted a NEW key %s; want reuse of %s", got[0].KeyID, wantKey)
	}
}

// TestBootstrap_KMSUnavailableBlocksStart pins the
// implementation-plan Stage 5.1 scenario
// `kms-unavailable-blocks-start`: when KMS.Ping fails Bootstrap
// returns an error wrapping ErrKMSUnavailable and the caller
// (the composition root) exits non-zero.
func TestBootstrap_KMSUnavailableBlocksStart(t *testing.T) {
	t.Parallel()
	kms := NewInMemoryKMS(nil)
	kms.FailPing(errors.New("dial tcp 127.0.0.1:8200: connection refused"))
	store := NewInMemoryStore()

	_, _, err := Bootstrap(context.Background(), Config{KMS: kms, Store: store}, true)
	if err == nil {
		t.Fatal("Bootstrap with KMS down: err = nil; want ErrKMSUnavailable")
	}
	if !errors.Is(err, ErrKMSUnavailable) {
		t.Errorf("Bootstrap with KMS down: err = %v; want errors.Is(ErrKMSUnavailable)", err)
	}
	// The wrapped error MUST mention the connection cause so
	// the operator can diagnose without re-running.
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("Bootstrap error %q does not mention root cause", err.Error())
	}
}

// TestBootstrap_HealthCheckTrippsOnKMSFailure verifies the
// returned health-check function reflects live KMS state, not
// a snapshot taken at Bootstrap time.
func TestBootstrap_HealthCheckTrippsOnKMSFailure(t *testing.T) {
	t.Parallel()
	kms := NewInMemoryKMS(nil)
	store := NewInMemoryStore()
	clock := newFakeClock(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))

	_, check, err := Bootstrap(context.Background(), Config{KMS: kms, Store: store, Clock: clock.Now}, true)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := check(context.Background()); err != nil {
		t.Fatalf("initial check: %v", err)
	}
	kms.FailPing(errors.New("kms-mock crashed"))
	if err := check(context.Background()); err == nil {
		t.Errorf("check after KMS crash: err = nil; want non-nil")
	}
}

// TestBootstrap_HealthCheckRequiresActiveKeys: if a deployment
// boots with mintFirstKey=false against an empty Store the
// health-check MUST report not-ready (no active keys).
func TestBootstrap_HealthCheckRequiresActiveKeys(t *testing.T) {
	t.Parallel()
	kms := NewInMemoryKMS(nil)
	store := NewInMemoryStore()
	clock := newFakeClock(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))

	_, check, err := Bootstrap(context.Background(), Config{KMS: kms, Store: store, Clock: clock.Now}, false)
	if err != nil {
		t.Fatalf("Bootstrap with mintFirstKey=false: %v", err)
	}
	if err := check(context.Background()); err == nil {
		t.Errorf("health check with empty cache: err = nil; want non-nil")
	}
}

// TestKMS_GenerateAndSignRoundTrip covers the in-memory KMS
// end-to-end: a freshly-generated key can sign and verify via
// the package-level verifyAgainstPublic helper. Documents the
// contract the Manager relies on.
func TestKMS_GenerateAndSignRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kms := NewInMemoryKMS(nil)
	pub, handle, err := kms.Generate(ctx)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(pub) != Ed25519PublicKeySize {
		t.Errorf("Generate returned %d-byte public key; want %d", len(pub), Ed25519PublicKeySize)
	}
	if handle == "" {
		t.Errorf("Generate returned empty handle")
	}
	payload := []byte("policy-version-blob")
	sig, err := kms.Sign(ctx, handle, payload)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if len(sig) != Ed25519SignatureSize {
		t.Errorf("Sign returned %d-byte signature; want %d", len(sig), Ed25519SignatureSize)
	}
	if err := verifyAgainstPublic(pub, payload, sig); err != nil {
		t.Errorf("verifyAgainstPublic: %v; want nil", err)
	}
}

// TestKMS_SignWithUnknownHandle returns an error rather than
// panicking. The Manager surfaces this as the wrapped
// `kms.Sign: ...` error so the bug is debuggable.
func TestKMS_SignWithUnknownHandle(t *testing.T) {
	t.Parallel()
	kms := NewInMemoryKMS(nil)
	_, err := kms.Sign(context.Background(), KeyHandle("nope"), []byte("x"))
	if err == nil {
		t.Fatal("Sign with unknown handle: err = nil; want error")
	}
}

// TestKMS_FailOverridesProduceWrappedErrors exercises every
// failure-injection knob to keep the test surface honest.
func TestKMS_FailOverridesProduceWrappedErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kms := NewInMemoryKMS(nil)

	kms.FailPing(errors.New("ping fail"))
	if err := kms.Ping(ctx); err == nil || err.Error() != "ping fail" {
		t.Errorf("Ping override: err = %v; want 'ping fail'", err)
	}
	kms.FailPing(nil)
	if err := kms.Ping(ctx); err != nil {
		t.Errorf("Ping after clearing: %v; want nil", err)
	}

	kms.FailGenerate(errors.New("gen fail"))
	if _, _, err := kms.Generate(ctx); err == nil || err.Error() != "gen fail" {
		t.Errorf("Generate override: err = %v; want 'gen fail'", err)
	}
	kms.FailGenerate(nil)

	_, handle, err := kms.Generate(ctx)
	if err != nil {
		t.Fatalf("Generate cleared: %v", err)
	}
	kms.FailSign(errors.New("sign fail"))
	if _, err := kms.Sign(ctx, handle, []byte("x")); err == nil || err.Error() != "sign fail" {
		t.Errorf("Sign override: err = %v; want 'sign fail'", err)
	}
}

// TestStore_InMemoryRejectsDuplicates pins the contract every
// Store implementation must honour: re-inserting the same
// key_id or fingerprint returns ErrDuplicateKey.
func TestStore_InMemoryRejectsDuplicates(t *testing.T) {
	t.Parallel()
	store := NewInMemoryStore()
	ctx := context.Background()
	id1, _ := uuid.NewV4()
	id2, _ := uuid.NewV4()
	pub1 := make([]byte, Ed25519PublicKeySize)
	pub1[0] = 0xAA
	pub2 := make([]byte, Ed25519PublicKeySize)
	pub2[0] = 0xBB

	r1 := KeyRecord{
		KeyID:       id1,
		Fingerprint: Fingerprint(pub1),
		PublicKey:   pub1,
		Handle:      "h1",
		ValidFrom:   time.Now(),
		Algorithm:   "ed25519",
	}
	if err := store.Insert(ctx, r1); err != nil {
		t.Fatalf("Insert r1: %v", err)
	}
	// Same KeyID -> ErrDuplicateKey.
	if err := store.Insert(ctx, r1); !errors.Is(err, ErrDuplicateKey) {
		t.Errorf("re-Insert same row: err = %v; want ErrDuplicateKey", err)
	}
	// Same fingerprint, different KeyID -> ErrDuplicateKey.
	r2 := r1
	r2.KeyID = id2
	if err := store.Insert(ctx, r2); !errors.Is(err, ErrDuplicateKey) {
		t.Errorf("Insert duplicate fingerprint: err = %v; want ErrDuplicateKey", err)
	}
	// Different KeyID + different fingerprint -> ok.
	r3 := KeyRecord{
		KeyID:       id2,
		Fingerprint: Fingerprint(pub2),
		PublicKey:   pub2,
		Handle:      "h2",
		ValidFrom:   time.Now(),
		Algorithm:   "ed25519",
	}
	if err := store.Insert(ctx, r3); err != nil {
		t.Errorf("Insert r3: %v; want nil", err)
	}
}

// TestStore_InMemoryRejectsShapeViolations exercises every
// validateRecord branch so a future caller that drops a
// validation does not silently regress.
func TestStore_InMemoryRejectsShapeViolations(t *testing.T) {
	t.Parallel()
	store := NewInMemoryStore()
	good := KeyRecord{
		KeyID:       newUUID(t),
		Fingerprint: Fingerprint(make([]byte, Ed25519PublicKeySize)),
		PublicKey:   make([]byte, Ed25519PublicKeySize),
		Handle:      "h",
		ValidFrom:   time.Now(),
		Algorithm:   "ed25519",
	}
	cases := []struct {
		name string
		mut  func(r *KeyRecord)
		want string
	}{
		{"zero KeyID", func(r *KeyRecord) { r.KeyID = uuid.Nil }, "KeyID is zero"},
		{"empty algorithm", func(r *KeyRecord) { r.Algorithm = "" }, "Algorithm is empty"},
		{"bad algorithm", func(r *KeyRecord) { r.Algorithm = "rsa-2048" }, "is not in the v1 closed set"},
		{"short public key", func(r *KeyRecord) { r.PublicKey = make([]byte, 16) }, "got 16 bytes"},
		{"empty handle", func(r *KeyRecord) { r.Handle = "" }, "Handle is empty"},
		{"short fingerprint", func(r *KeyRecord) { r.Fingerprint = "abc" }, "must be 64 lowercase hex"},
		{"non-hex fingerprint", func(r *KeyRecord) {
			r.Fingerprint = strings.Repeat("Z", 64)
		}, "non-lowercase-hex"},
		{"fingerprint-pubkey mismatch", func(r *KeyRecord) {
			// Flip one byte of PublicKey but keep the original
			// fingerprint -- the invariant must reject this so
			// the `policy.keys.list_active` projection can never
			// publish a fingerprint that diverges from the
			// actual public material.
			r.PublicKey = append([]byte(nil), r.PublicKey...)
			r.PublicKey[0] ^= 0x01
		}, "does not match SHA-256(PublicKey)"},
		{"zero ValidFrom", func(r *KeyRecord) { r.ValidFrom = time.Time{} }, "ValidFrom is zero"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := good
			// Each subtest re-builds the baseline with a unique
			// public key (so duplicate-fingerprint collisions
			// from the shared store don't shadow the intended
			// check) and a fingerprint that matches it -- THAT
			// way the mutation under test is the only invariant
			// the validator can be tripping on.
			pub := make([]byte, Ed25519PublicKeySize)
			pub[0] = byte(len(tc.name))
			pub[1] = byte(len(tc.want))
			rec.PublicKey = pub
			rec.KeyID = newUUID(t)
			rec.Fingerprint = Fingerprint(pub)
			tc.mut(&rec)
			err := store.Insert(context.Background(), rec)
			if err == nil {
				t.Fatalf("Insert(%s): err = nil; want error containing %q", tc.name, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Insert(%s): err = %q; want substring %q", tc.name, err.Error(), tc.want)
			}
		})
	}
}

func newUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV4()
	if err != nil {
		t.Fatalf("uuid.NewV4: %v", err)
	}
	return id
}
