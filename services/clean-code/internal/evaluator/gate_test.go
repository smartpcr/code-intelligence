package evaluator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/keys"
)

// buildManagerWithClock returns an in-memory Manager backed by
// a settable clock so tests can step forward across the 24h
// overlap boundary deterministically.
func buildManagerWithClock(t *testing.T, now *time.Time) *keys.Manager {
	t.Helper()
	clock := func() time.Time { return *now }
	cfg := keys.Config{
		KMS:     keys.NewInMemoryKMS(nil),
		Store:   keys.NewInMemoryStore(),
		Overlap: 24 * time.Hour,
		Clock:   clock,
	}
	m, err := keys.NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return m
}

// signWith mints a fresh key at the given time (returns the
// key_id) and signs payload using m.Sign at that same time.
// Returns the signing key_id and the produced signature.
func mintAt(t *testing.T, m *keys.Manager, now *time.Time, at time.Time) uuid.UUID {
	t.Helper()
	*now = at
	rec, err := m.ForceRotate(context.Background())
	if err != nil {
		t.Fatalf("ForceRotate at %s: %v", at, err)
	}
	return rec.KeyID
}

func signAt(t *testing.T, m *keys.Manager, now *time.Time, at time.Time, payload []byte) (uuid.UUID, []byte) {
	t.Helper()
	*now = at
	kid, sig, err := m.Sign(context.Background(), payload)
	if err != nil {
		t.Fatalf("Sign at %s: %v", at, err)
	}
	return kid, sig
}

func TestGate_NilManagerReturnsUnwired(t *testing.T) {
	t.Parallel()
	g := NewGate(nil)
	err := g.VerifyPolicy(context.Background(), PolicySignature{
		KeyID:     uuid.Must(uuid.NewV4()),
		Payload:   []byte("p"),
		Signature: []byte("s"),
	})
	if !errors.Is(err, ErrGateUnwired) {
		t.Fatalf("err=%v; want ErrGateUnwired", err)
	}
}

func TestGate_RejectsZeroKeyID(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	m := buildManagerWithClock(t, &now)
	mintAt(t, m, &now, now)
	g := NewGate(m)

	err := g.VerifyPolicy(context.Background(), PolicySignature{
		KeyID:     uuid.Nil,
		Payload:   []byte("payload"),
		Signature: []byte("sig"),
	})
	if !errors.Is(err, ErrPolicySignatureInvalid) {
		t.Fatalf("err=%v; want ErrPolicySignatureInvalid", err)
	}
}

func TestGate_RejectsEmptyPayloadAndSignature(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	m := buildManagerWithClock(t, &now)
	kid := mintAt(t, m, &now, now)
	g := NewGate(m)

	cases := []struct {
		name string
		sig  PolicySignature
	}{
		{"empty payload", PolicySignature{KeyID: kid, Payload: nil, Signature: []byte("x")}},
		{"empty signature", PolicySignature{KeyID: kid, Payload: []byte("x"), Signature: nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := g.VerifyPolicy(context.Background(), tc.sig); !errors.Is(err, ErrPolicySignatureInvalid) {
				t.Errorf("err=%v; want ErrPolicySignatureInvalid", err)
			}
		})
	}
}

func TestGate_VerifyValidSignature(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	m := buildManagerWithClock(t, &now)
	mintAt(t, m, &now, now)
	g := NewGate(m)

	payload := []byte("hello world")
	kid, sig := signAt(t, m, &now, now, payload)

	if err := g.VerifyPolicy(context.Background(), PolicySignature{
		KeyID: kid, Payload: payload, Signature: sig,
	}); err != nil {
		t.Fatalf("VerifyPolicy: %v", err)
	}
}

// TestGate_OverlapWindowBothKeysAccepted is the Stage 5.1
// linchpin requirement: during the 24h overlap window after a
// rotation, signatures from BOTH the new and the old key MUST
// verify. The brief calls this out verbatim:
//
//   `two key_ids may co-exist during overlap, both accepted by
//   the evaluator`.
func TestGate_OverlapWindowBothKeysAccepted(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	m := buildManagerWithClock(t, &now)
	g := NewGate(m)

	// T0: mint K0 and sign payload P0.
	mintAt(t, m, &now, now)
	payloadOld := []byte("old-bundle")
	kidOld, sigOld := signAt(t, m, &now, now, payloadOld)

	// T+12h: mint K1 (well inside K0's 24h overlap), sign P1
	// with K1. Both K0 and K1 should be active.
	rotateAt := now.Add(12 * time.Hour)
	kidNew := mintAt(t, m, &now, rotateAt)
	payloadNew := []byte("new-bundle")
	kidSigNew, sigNew := signAt(t, m, &now, rotateAt.Add(time.Minute), payloadNew)
	if kidSigNew != kidNew {
		t.Fatalf("sign-time keyID=%s but ForceRotate returned %s", kidSigNew, kidNew)
	}
	if kidNew == kidOld {
		t.Fatal("rotation produced the same key_id; rotation is broken")
	}

	// Inside the overlap, BOTH key_ids verify.
	if err := g.VerifyPolicy(context.Background(), PolicySignature{
		KeyID: kidOld, Payload: payloadOld, Signature: sigOld,
	}); err != nil {
		t.Errorf("overlap: old key signature should still verify; got err=%v", err)
	}
	if err := g.VerifyPolicy(context.Background(), PolicySignature{
		KeyID: kidNew, Payload: payloadNew, Signature: sigNew,
	}); err != nil {
		t.Errorf("overlap: new key signature should verify; got err=%v", err)
	}
}

// TestGate_PostOverlapOldKeyRejected: once the overlap closes
// (24h after the SECOND key's valid_from), the old key's
// `[valid_from, valid_until)` window has elapsed; verification
// MUST fail.
func TestGate_PostOverlapOldKeyRejected(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	m := buildManagerWithClock(t, &now)
	g := NewGate(m)

	mintAt(t, m, &now, now)
	payloadOld := []byte("old")
	kidOld, sigOld := signAt(t, m, &now, now, payloadOld)

	// At T+12h mint K1. K0.valid_until = K1.valid_from + 24h
	// = T+36h.
	rotateAt := now.Add(12 * time.Hour)
	mintAt(t, m, &now, rotateAt)

	// Advance the clock past K0's valid_until (T+36h + 1s).
	now = rotateAt.Add(24*time.Hour + time.Second)

	err := g.VerifyPolicy(context.Background(), PolicySignature{
		KeyID: kidOld, Payload: payloadOld, Signature: sigOld,
	})
	if !errors.Is(err, ErrPolicySignatureInvalid) {
		t.Fatalf("post-overlap: err=%v; want ErrPolicySignatureInvalid", err)
	}
}

func TestGate_TamperedPayloadRejected(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	m := buildManagerWithClock(t, &now)
	mintAt(t, m, &now, now)
	g := NewGate(m)

	payload := []byte("hello world")
	kid, sig := signAt(t, m, &now, now, payload)

	tampered := []byte("hello world!")
	err := g.VerifyPolicy(context.Background(), PolicySignature{
		KeyID: kid, Payload: tampered, Signature: sig,
	})
	if !errors.Is(err, ErrPolicySignatureInvalid) {
		t.Fatalf("tampered payload: err=%v; want ErrPolicySignatureInvalid", err)
	}
}

func TestGate_UnknownKeyIDRejected(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	m := buildManagerWithClock(t, &now)
	mintAt(t, m, &now, now)
	g := NewGate(m)

	payload := []byte("p")
	_, sig := signAt(t, m, &now, now, payload)

	unknown := uuid.Must(uuid.NewV4())
	err := g.VerifyPolicy(context.Background(), PolicySignature{
		KeyID: unknown, Payload: payload, Signature: sig,
	})
	if !errors.Is(err, ErrPolicySignatureInvalid) {
		t.Fatalf("unknown key_id: err=%v; want ErrPolicySignatureInvalid", err)
	}
}

func TestGate_VerifyAnyDuringOverlap(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	m := buildManagerWithClock(t, &now)
	g := NewGate(m)

	mintAt(t, m, &now, now)
	pOld := []byte("old")
	kidOld, sigOld := signAt(t, m, &now, now, pOld)

	rotateAt := now.Add(12 * time.Hour)
	kidNew := mintAt(t, m, &now, rotateAt)
	pNew := []byte("new")
	_, sigNew := signAt(t, m, &now, rotateAt.Add(time.Minute), pNew)

	gotOld, err := g.VerifyAnyPolicySignature(context.Background(), pOld, sigOld)
	if err != nil {
		t.Fatalf("VerifyAny(old): %v", err)
	}
	if gotOld != kidOld {
		t.Errorf("VerifyAny(old) returned %s; want %s", gotOld, kidOld)
	}
	gotNew, err := g.VerifyAnyPolicySignature(context.Background(), pNew, sigNew)
	if err != nil {
		t.Fatalf("VerifyAny(new): %v", err)
	}
	if gotNew != kidNew {
		t.Errorf("VerifyAny(new) returned %s; want %s", gotNew, kidNew)
	}
}

func TestGate_VerifyAnyUnwired(t *testing.T) {
	t.Parallel()
	g := NewGate(nil)
	_, err := g.VerifyAnyPolicySignature(context.Background(), []byte("p"), []byte("s"))
	if !errors.Is(err, ErrGateUnwired) {
		t.Fatalf("err=%v; want ErrGateUnwired", err)
	}
}
