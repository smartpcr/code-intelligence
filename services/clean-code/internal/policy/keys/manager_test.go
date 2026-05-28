package keys

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gofrs/uuid"
)

// fakeClock is a minimal monotonic clock fixture used by every
// rotation/overlap test. now advances explicitly via Advance so
// the same test can observe the boundary at T+overlap-1ns and
// T+overlap+1ns deterministically.
type fakeClock struct {
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t} }
func (c *fakeClock) Now() time.Time       { return c.now }
func (c *fakeClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}
func (c *fakeClock) Set(t time.Time) {
	c.now = t
}

// newTestManager wires a Manager around an InMemoryKMS +
// InMemoryStore with the supplied clock. Returns the manager,
// the kms (so a test can poke FailGenerate), and the clock.
func newTestManager(t *testing.T, clock *fakeClock, overlap time.Duration) (*Manager, *InMemoryKMS, *InMemoryStore) {
	t.Helper()
	kms := NewInMemoryKMS(nil)
	store := NewInMemoryStore()
	m, err := NewManager(Config{
		KMS:     kms,
		Store:   store,
		Overlap: overlap,
		Clock:   clock.Now,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m, kms, store
}

// TestManager_RotateMintsFirstKey covers the empty-cache path:
// Rotate against an empty Store inserts exactly one row, the
// cache reflects it, and ListActive returns it with the
// sentinel valid_until.
func TestManager_RotateMintsFirstKey(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	m, _, store := newTestManager(t, clock, DefaultOverlap)
	ctx := context.Background()

	rec, err := m.Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if rec.KeyID == uuid.Nil {
		t.Fatalf("Rotate returned zero KeyID")
	}
	if rec.Algorithm != "ed25519" {
		t.Errorf("Algorithm = %q, want ed25519", rec.Algorithm)
	}
	if len(rec.PublicKey) != Ed25519PublicKeySize {
		t.Errorf("len(PublicKey) = %d, want %d", len(rec.PublicKey), Ed25519PublicKeySize)
	}
	if rec.Fingerprint != Fingerprint(rec.PublicKey) {
		t.Errorf("Fingerprint not consistent with PublicKey hash")
	}
	if !rec.ValidFrom.Equal(t0) {
		t.Errorf("ValidFrom = %v, want %v", rec.ValidFrom, t0)
	}

	rows, err := store.List(ctx)
	if err != nil {
		t.Fatalf("Store.List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("Store rows = %d, want 1", len(rows))
	}

	views, err := m.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("ListActive returned %d entries, want 1", len(views))
	}
	if !views[0].ValidUntil.Equal(SentinelValidUntil) {
		t.Errorf("first key's ValidUntil = %v, want sentinel %v", views[0].ValidUntil, SentinelValidUntil)
	}
}

// TestManager_RotateRejectsInsideOverlapWindow pins the
// guard from rubber-duck critique #2: a normal Rotate inside
// the overlap window returns ErrRotationTooSoon, leaves the
// cache unchanged, and ForceRotate succeeds at the same
// instant.
func TestManager_RotateRejectsInsideOverlapWindow(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	m, _, store := newTestManager(t, clock, DefaultOverlap)
	ctx := context.Background()

	first, err := m.Rotate(ctx)
	if err != nil {
		t.Fatalf("first Rotate: %v", err)
	}

	// Advance only 12h -- still inside the 24h overlap floor.
	clock.Advance(12 * time.Hour)
	if _, err := m.Rotate(ctx); !errors.Is(err, ErrRotationTooSoon) {
		t.Fatalf("Rotate inside overlap window: err = %v, want ErrRotationTooSoon", err)
	}
	rows, err := store.List(ctx)
	if err != nil {
		t.Fatalf("Store.List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("Store rows after rejected rotation = %d, want 1", len(rows))
	}

	// ForceRotate must bypass the guard.
	forced, err := m.ForceRotate(ctx)
	if err != nil {
		t.Fatalf("ForceRotate inside overlap window: %v", err)
	}
	if forced.KeyID == first.KeyID {
		t.Errorf("ForceRotate returned the same KeyID as the first key (%s)", first.KeyID)
	}
	rows, _ = store.List(ctx)
	if len(rows) != 2 {
		t.Fatalf("Store rows after ForceRotate = %d, want 2", len(rows))
	}
}

// TestManager_RotateAfterOverlapElapses verifies a normal
// rotation succeeds once the window has elapsed.
func TestManager_RotateAfterOverlapElapses(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	m, _, _ := newTestManager(t, clock, DefaultOverlap)
	ctx := context.Background()

	if _, err := m.Rotate(ctx); err != nil {
		t.Fatalf("first Rotate: %v", err)
	}
	clock.Advance(DefaultOverlap)
	if _, err := m.Rotate(ctx); err != nil {
		t.Fatalf("Rotate at exact overlap boundary: %v", err)
	}
}

// TestManager_OverlapWindowBoundary is the canonical
// implementation-plan Stage 5.1 scenario
// `overlap-window-enforced`:
//
//	Given a key rotation at T0,
//	When a payload signed by the old key arrives at T0+23h59m,
//	Then verification succeeds; at T0+24h+1s, verification fails.
//
// T0 in the brief = the moment the NEW key was published.
// Under the half-open interval `[valid_from, valid_until)` the
// old key's valid_until = newKey.valid_from + overlap (24h).
// The test pins three sample times: just inside, exactly at,
// and just past the boundary.
func TestManager_OverlapWindowBoundary(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	m, _, _ := newTestManager(t, clock, DefaultOverlap)
	ctx := context.Background()

	first, err := m.Rotate(ctx)
	if err != nil {
		t.Fatalf("first Rotate: %v", err)
	}
	payload := []byte("policy-version-canonical-json-blob")

	// Sign at t0 (first key is the only active one).
	signedKeyID, sig, err := m.Sign(ctx, payload)
	if err != nil {
		t.Fatalf("Sign t0: %v", err)
	}
	if signedKeyID != first.KeyID {
		t.Fatalf("Sign at t0 used key_id %s; want first key %s", signedKeyID, first.KeyID)
	}

	// Advance one second beyond the overlap floor and rotate.
	// The "rotation at T0" the scenario quotes is the
	// publication of the SECOND key; we treat the second key's
	// valid_from as the scenario's T0 for the verification
	// boundary.
	clock.Advance(DefaultOverlap + time.Second)
	second, err := m.Rotate(ctx)
	if err != nil {
		t.Fatalf("second Rotate: %v", err)
	}
	rotationT0 := second.ValidFrom

	// Inside the overlap window (T0+23h59m): old signature
	// must still verify.
	clock.Set(rotationT0.Add(23*time.Hour + 59*time.Minute))
	if err := m.Verify(ctx, first.KeyID, payload, sig); err != nil {
		t.Errorf("Verify at T0+23h59m: %v; want nil", err)
	}

	// At the exact boundary the interval is half-open so the
	// upper bound is excluded -- verification MUST fail.
	clock.Set(rotationT0.Add(DefaultOverlap))
	err = m.Verify(ctx, first.KeyID, payload, sig)
	if !errors.Is(err, ErrUnknownKey) {
		t.Errorf("Verify at exact T0+24h boundary: err = %v; want ErrUnknownKey (half-open)", err)
	}

	// One second past the overlap (T0+24h+1s): the scenario
	// explicitly requires verification to fail.
	clock.Set(rotationT0.Add(DefaultOverlap + time.Second))
	err = m.Verify(ctx, first.KeyID, payload, sig)
	if !errors.Is(err, ErrUnknownKey) {
		t.Errorf("Verify at T0+24h+1s: err = %v; want ErrUnknownKey", err)
	}
}

// TestManager_VerifyKnownGood pins the verify-good path: a
// signature minted by the same Manager validates under
// Verify(keyID, ...). Mirrors the canonical
// `eval.gate` signature-check path.
func TestManager_VerifyKnownGood(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	m, _, _ := newTestManager(t, clock, DefaultOverlap)
	ctx := context.Background()

	rec, err := m.Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	payload := []byte(`{"rule_refs":[],"threshold_refs":[],"refactor_weights":{}}`)
	keyID, sig, err := m.Sign(ctx, payload)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if keyID != rec.KeyID {
		t.Fatalf("Sign returned key_id %s; want %s", keyID, rec.KeyID)
	}
	if err := m.Verify(ctx, keyID, payload, sig); err != nil {
		t.Errorf("Verify: %v; want nil", err)
	}

	// Tamper the payload -> ErrSignatureMismatch.
	tampered := append([]byte(nil), payload...)
	tampered[0] ^= 0x01
	if err := m.Verify(ctx, keyID, tampered, sig); !errors.Is(err, ErrSignatureMismatch) {
		t.Errorf("Verify tampered payload: err = %v; want ErrSignatureMismatch", err)
	}

	// Tamper the signature -> ErrSignatureMismatch.
	tamperedSig := append([]byte(nil), sig...)
	tamperedSig[0] ^= 0x01
	if err := m.Verify(ctx, keyID, payload, tamperedSig); !errors.Is(err, ErrSignatureMismatch) {
		t.Errorf("Verify tampered signature: err = %v; want ErrSignatureMismatch", err)
	}
}

// TestManager_VerifyUnknownKeyID rejects a key_id that was
// never registered. The Evaluator Surface translates this to
// the `policy_signature_invalid` degraded short-circuit.
func TestManager_VerifyUnknownKeyID(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	m, _, _ := newTestManager(t, clock, DefaultOverlap)
	ctx := context.Background()
	if _, err := m.Rotate(ctx); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	bogus, _ := uuid.NewV4()
	if err := m.Verify(ctx, bogus, []byte("x"), make([]byte, Ed25519SignatureSize)); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("Verify with bogus key_id: err = %v; want ErrUnknownKey", err)
	}
}

// TestManager_SignSelectsNewestActiveKey demonstrates the
// "newest with valid_from <= now" tie-breaker pinned by
// rubber-duck critique #5.
func TestManager_SignSelectsNewestActiveKey(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	m, _, _ := newTestManager(t, clock, DefaultOverlap)
	ctx := context.Background()

	first, err := m.Rotate(ctx)
	if err != nil {
		t.Fatalf("first Rotate: %v", err)
	}
	// Advance well past the overlap floor and rotate again.
	clock.Advance(DefaultOverlap + time.Hour)
	second, err := m.Rotate(ctx)
	if err != nil {
		t.Fatalf("second Rotate: %v", err)
	}

	// Now both keys are active; Sign MUST pick the newer one.
	keyID, _, err := m.Sign(ctx, []byte("payload"))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if keyID != second.KeyID {
		t.Errorf("Sign picked %s; want newest key %s (first=%s)", keyID, second.KeyID, first.KeyID)
	}
}

// TestManager_VerifyAnyTriesEveryActiveKey is the
// WAL-Reconciler-style fallback: callers without a stored
// key_id can ask the Manager to brute-force across the cache.
func TestManager_VerifyAnyTriesEveryActiveKey(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	m, _, _ := newTestManager(t, clock, DefaultOverlap)
	ctx := context.Background()

	first, err := m.Rotate(ctx)
	if err != nil {
		t.Fatalf("first Rotate: %v", err)
	}
	payload := []byte("legacy-payload")
	_, sig, err := m.Sign(ctx, payload)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	clock.Advance(DefaultOverlap + time.Minute)
	if _, err := m.Rotate(ctx); err != nil {
		t.Fatalf("second Rotate: %v", err)
	}
	// During overlap both keys verify; VerifyAny should find
	// the FIRST one.
	gotKey, err := m.VerifyAny(ctx, payload, sig)
	if err != nil {
		t.Fatalf("VerifyAny: %v", err)
	}
	if gotKey != first.KeyID {
		t.Errorf("VerifyAny returned %s; want %s", gotKey, first.KeyID)
	}
}

// TestManager_ListActiveSnapshotShape pins the
// `policy.keys.list_active` projection shape: every entry
// carries key_id + fingerprint + valid_from + valid_until and
// the newest key surfaces first.
func TestManager_ListActiveSnapshotShape(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	m, _, _ := newTestManager(t, clock, DefaultOverlap)
	ctx := context.Background()

	first, err := m.Rotate(ctx)
	if err != nil {
		t.Fatalf("first Rotate: %v", err)
	}
	clock.Advance(DefaultOverlap + time.Minute)
	second, err := m.Rotate(ctx)
	if err != nil {
		t.Fatalf("second Rotate: %v", err)
	}

	views, err := m.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("ListActive returned %d entries; want 2 during overlap", len(views))
	}
	if views[0].KeyID != second.KeyID {
		t.Errorf("first list_active entry = %s; want newest key %s", views[0].KeyID, second.KeyID)
	}
	if views[1].KeyID != first.KeyID {
		t.Errorf("second list_active entry = %s; want older key %s", views[1].KeyID, first.KeyID)
	}
	// Newest key's valid_until is the sentinel; older key's
	// valid_until = newest.valid_from + overlap.
	if !views[0].ValidUntil.Equal(SentinelValidUntil) {
		t.Errorf("newest valid_until = %v; want sentinel", views[0].ValidUntil)
	}
	wantOldUntil := second.ValidFrom.Add(DefaultOverlap)
	if !views[1].ValidUntil.Equal(wantOldUntil) {
		t.Errorf("older valid_until = %v; want %v", views[1].ValidUntil, wantOldUntil)
	}
}

// TestManager_ListActiveDropsExpiredEntries verifies that
// rotating a THIRD key retires the FIRST: list_active becomes
// {third, second} after the first's overlap window with the
// second has elapsed past the third's publication.
func TestManager_ListActiveDropsExpiredEntries(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	m, _, _ := newTestManager(t, clock, DefaultOverlap)
	ctx := context.Background()

	first, err := m.Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate1: %v", err)
	}
	clock.Advance(DefaultOverlap + time.Minute)
	second, err := m.Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate2: %v", err)
	}
	clock.Advance(DefaultOverlap + time.Minute)
	third, err := m.Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate3: %v", err)
	}
	// At this moment: first.valid_until = second.valid_from + overlap.
	// We've advanced second.valid_from + (overlap + 1min) -> 1min past first's expiry.
	views, err := m.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("ListActive returned %d entries after 2 rotations; want 2 (third + second)", len(views))
	}
	ids := []uuid.UUID{views[0].KeyID, views[1].KeyID}
	wantSet := map[uuid.UUID]bool{third.KeyID: true, second.KeyID: true}
	for _, id := range ids {
		if !wantSet[id] {
			t.Errorf("ListActive contained unexpected key_id %s (first=%s, second=%s, third=%s)",
				id, first.KeyID, second.KeyID, third.KeyID)
		}
	}
}

// TestManager_SignFailsWithNoActiveKey covers the
// pathological no-key cache; reachable only via mis-bootstrap.
func TestManager_SignFailsWithNoActiveKey(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	m, _, _ := newTestManager(t, clock, DefaultOverlap)
	ctx := context.Background()

	if _, _, err := m.Sign(ctx, []byte("x")); !errors.Is(err, ErrNoActiveKey) {
		t.Errorf("Sign empty cache: err = %v; want ErrNoActiveKey", err)
	}
}

// TestManager_SignActive_BindsKeyIDIntoPayload is the
// Audit-WAL signer contract: the keyID the build callback
// receives MUST be the one SignActive uses for the KMS sign
// AND the one returned to the caller. This protects the WAL
// frame format where `signing_key_id` is hashed INTO the
// canonical payload BEFORE signing -- a signer that returned
// a different keyID would invalidate verification.
func TestManager_SignActive_BindsKeyIDIntoPayload(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	m, _, _ := newTestManager(t, clock, DefaultOverlap)
	ctx := context.Background()

	rec, err := m.Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	var observedKeyID uuid.UUID
	gotKeyID, sig, err := m.SignActive(ctx, func(keyID uuid.UUID) ([]byte, error) {
		observedKeyID = keyID
		return []byte("audit-wal-payload-" + keyID.String()), nil
	})
	if err != nil {
		t.Fatalf("SignActive: %v", err)
	}
	if observedKeyID != rec.KeyID {
		t.Errorf("build observed key_id=%s; want active=%s", observedKeyID, rec.KeyID)
	}
	if gotKeyID != rec.KeyID {
		t.Errorf("SignActive returned key_id=%s; want %s", gotKeyID, rec.KeyID)
	}
	if len(sig) != Ed25519SignatureSize {
		t.Errorf("SignActive sig length=%d; want %d", len(sig), Ed25519SignatureSize)
	}
	// Defence-in-depth: the signature MUST validate against
	// the chosen key's public bytes when computed over the
	// SAME payload bytes (i.e. those produced by build with
	// the observed keyID).
	payload := []byte("audit-wal-payload-" + observedKeyID.String())
	if err := m.Verify(ctx, gotKeyID, payload, sig); err != nil {
		t.Errorf("Verify of SignActive output: %v; want nil", err)
	}
}

// TestManager_SignActive_NoActiveKey mirrors
// [TestManager_SignFailsWithNoActiveKey] for the SignActive
// path: an empty cache MUST surface [ErrNoActiveKey].
func TestManager_SignActive_NoActiveKey(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	m, _, _ := newTestManager(t, clock, DefaultOverlap)
	ctx := context.Background()

	_, _, err := m.SignActive(ctx, func(uuid.UUID) ([]byte, error) {
		return []byte("ignored"), nil
	})
	if !errors.Is(err, ErrNoActiveKey) {
		t.Errorf("SignActive empty cache: err = %v; want ErrNoActiveKey", err)
	}
}

// TestManager_SignActive_RejectsNilBuild pins the API
// contract: a nil build callback is a programming error and
// must return a stable error immediately.
func TestManager_SignActive_RejectsNilBuild(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	m, _, _ := newTestManager(t, clock, DefaultOverlap)
	ctx := context.Background()

	if _, err := m.Rotate(ctx); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	_, _, err := m.SignActive(ctx, nil)
	if err == nil {
		t.Fatal("SignActive(nil build) = nil; want error")
	}
}

// TestManager_SignActive_PropagatesBuildError pins the
// callback-error wrapping contract: any error returned by
// build is surfaced (so the WAL writer can surface it to
// the audit-write call site, which rolls back the SQL tx).
func TestManager_SignActive_PropagatesBuildError(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	m, _, _ := newTestManager(t, clock, DefaultOverlap)
	ctx := context.Background()

	if _, err := m.Rotate(ctx); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	sentinel := errors.New("synthetic build failure")
	_, _, err := m.SignActive(ctx, func(uuid.UUID) ([]byte, error) {
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("SignActive build error: err = %v; want wraps %v", err, sentinel)
	}
}

// TestManager_NewManagerValidatesConfig pins the required-arg
// guards in NewManager.
func TestManager_NewManagerValidatesConfig(t *testing.T) {
	t.Parallel()
	kms := NewInMemoryKMS(nil)
	store := NewInMemoryStore()

	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "missing KMS",
			cfg:  Config{Store: store},
			want: "cfg.KMS is required",
		},
		{
			name: "missing Store",
			cfg:  Config{KMS: kms},
			want: "cfg.Store is required",
		},
		{
			name: "negative overlap",
			cfg:  Config{KMS: kms, Store: store, Overlap: -time.Second},
			want: "must be >= 0",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewManager(tc.cfg)
			if err == nil {
				t.Fatalf("NewManager(%s): err = nil; want error containing %q", tc.name, tc.want)
			}
			if !bytes.Contains([]byte(err.Error()), []byte(tc.want)) {
				t.Errorf("NewManager(%s): err = %q; want substring %q", tc.name, err.Error(), tc.want)
			}
		})
	}
}

// TestManager_LoadRehydratesCacheAcrossInstances simulates a
// service restart: a second Manager wired to the SAME Store +
// KMS sees every prior key via Load.
func TestManager_LoadRehydratesCacheAcrossInstances(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(t0)
	kms := NewInMemoryKMS(nil)
	store := NewInMemoryStore()
	mgrA, err := NewManager(Config{KMS: kms, Store: store, Clock: clock.Now})
	if err != nil {
		t.Fatalf("NewManager A: %v", err)
	}
	firstRec, err := mgrA.Rotate(context.Background())
	if err != nil {
		t.Fatalf("Rotate A: %v", err)
	}

	// "Restart" the manager but keep the SAME store + kms (the
	// KMS holds the sealed private key by handle).
	mgrB, err := NewManager(Config{KMS: kms, Store: store, Clock: clock.Now})
	if err != nil {
		t.Fatalf("NewManager B: %v", err)
	}
	if err := mgrB.Load(context.Background()); err != nil {
		t.Fatalf("Load B: %v", err)
	}
	keyID, sig, err := mgrB.Sign(context.Background(), []byte("x"))
	if err != nil {
		t.Fatalf("Sign B: %v", err)
	}
	if keyID != firstRec.KeyID {
		t.Errorf("Sign B used %s; want %s (key minted by mgrA)", keyID, firstRec.KeyID)
	}
	if err := mgrB.Verify(context.Background(), keyID, []byte("x"), sig); err != nil {
		t.Errorf("Verify B: %v", err)
	}
}

// TestFingerprintIsStable pins the canonical fingerprint
// computation; matches the migration's regex CHECK
// (`^[0-9a-f]{64}$`).
func TestFingerprintIsStable(t *testing.T) {
	t.Parallel()
	pub := make([]byte, Ed25519PublicKeySize)
	for i := range pub {
		pub[i] = byte(i)
	}
	got := Fingerprint(pub)
	if len(got) != 64 {
		t.Errorf("Fingerprint length = %d; want 64", len(got))
	}
	for _, r := range got {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			t.Errorf("Fingerprint character %q is not lowercase hex", r)
		}
	}
	// Determinism: same input -> same output.
	if Fingerprint(pub) != got {
		t.Errorf("Fingerprint is non-deterministic")
	}
	// Distinct input -> distinct output.
	pub2 := make([]byte, Ed25519PublicKeySize)
	pub2[0] = 0xFF
	if Fingerprint(pub2) == got {
		t.Errorf("Fingerprint collided across distinct inputs")
	}
}
