package keys

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/gofrs/uuid"
)

// Store abstracts the persistence layer for [KeyRecord] rows.
// The Manager reads / writes via this interface so the same
// rotation / verify logic can target the in-memory [InMemoryStore]
// in unit tests, the SQL-backed `clean_code.policy_signing_keys`
// table in production, or a fake in higher-level tests.
//
// All implementations MUST honour these invariants:
//
//  1. List returns rows in deterministic order: `ValidFrom ASC,
//     KeyID ASC`. Stable ordering is the load-bearing assumption
//     for the Manager's "next key" computation and the
//     `policy.keys.list_active` projection.
//
//  2. Insert is append-only. The Manager NEVER asks Store to
//     UPDATE or DELETE; the DB-level [REVOKE UPDATE, DELETE]
//     grants in 0005_policy_signing_keys.up.sql enforce this at
//     the storage layer as well.
//
//  3. Insert rejects rows with duplicate `KeyID` or duplicate
//     `Fingerprint`. The migration's UNIQUE constraint on
//     fingerprint and PRIMARY KEY on key_id back this at the
//     DB level; the in-memory store enforces it explicitly.
type Store interface {
	// Insert appends rec. Returns ErrDuplicateKey if a row
	// with the same KeyID or Fingerprint already exists.
	Insert(ctx context.Context, rec KeyRecord) error

	// List returns every persisted row in `ValidFrom ASC,
	// KeyID ASC` order.
	List(ctx context.Context) ([]KeyRecord, error)
}

// ErrDuplicateKey is returned by Store.Insert when the supplied
// record collides with an existing row (by KeyID or by
// Fingerprint).
var ErrDuplicateKey = errors.New("policy/keys: duplicate signing key (key_id or fingerprint)")

// InMemoryStore is a process-local Store backed by a Go slice.
// Safe for concurrent use. Used by unit tests and by the
// `docker-compose kms-mock` harness in the e2e fixtures.
type InMemoryStore struct {
	mu   sync.RWMutex
	rows []KeyRecord
}

// NewInMemoryStore constructs a fresh empty store.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{}
}

// Insert appends rec. Returns ErrDuplicateKey if a row with
// the same KeyID or Fingerprint already exists.
func (s *InMemoryStore) Insert(ctx context.Context, rec KeyRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateRecord(rec); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.rows {
		if existing.KeyID == rec.KeyID {
			return fmt.Errorf("%w: key_id=%s", ErrDuplicateKey, rec.KeyID)
		}
		if existing.Fingerprint == rec.Fingerprint {
			return fmt.Errorf("%w: fingerprint=%s", ErrDuplicateKey, rec.Fingerprint)
		}
	}
	// Defensive copy of the public-key bytes so the caller
	// cannot mutate the slice through the persisted row.
	pubCopy := make([]byte, len(rec.PublicKey))
	copy(pubCopy, rec.PublicKey)
	rec.PublicKey = pubCopy
	s.rows = append(s.rows, rec)
	sortRecords(s.rows)
	return nil
}

// List returns every row in `ValidFrom ASC, KeyID ASC` order.
// The returned slice is a copy so callers can safely sort /
// mutate it without affecting the store.
func (s *InMemoryStore) List(ctx context.Context) ([]KeyRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]KeyRecord, len(s.rows))
	copy(out, s.rows)
	for i := range out {
		pubCopy := make([]byte, len(out[i].PublicKey))
		copy(pubCopy, out[i].PublicKey)
		out[i].PublicKey = pubCopy
	}
	return out, nil
}

// Compile-time check that InMemoryStore satisfies Store.
var _ Store = (*InMemoryStore)(nil)

// sortRecords pins the canonical `ValidFrom ASC, KeyID ASC`
// order every Store implementation must honour.
func sortRecords(rows []KeyRecord) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ValidFrom.Equal(rows[j].ValidFrom) {
			return uuidCompare(rows[i].KeyID, rows[j].KeyID) < 0
		}
		return rows[i].ValidFrom.Before(rows[j].ValidFrom)
	})
}

// uuidCompare orders two UUIDs lexicographically by their
// canonical byte representation. github.com/gofrs/uuid does not
// expose a Compare method so we use the underlying byte slice
// the type embeds.
func uuidCompare(a, b uuid.UUID) int {
	aBytes := a.Bytes()
	bBytes := b.Bytes()
	for i := 0; i < len(aBytes); i++ {
		if aBytes[i] < bBytes[i] {
			return -1
		}
		if aBytes[i] > bBytes[i] {
			return 1
		}
	}
	return 0
}

// validateRecord enforces the shape invariants every Store
// MUST refuse on Insert.
func validateRecord(rec KeyRecord) error {
	if rec.KeyID == uuid.Nil {
		return fmt.Errorf("policy/keys: KeyRecord.KeyID is zero")
	}
	if rec.Algorithm == "" {
		return fmt.Errorf("policy/keys: KeyRecord.Algorithm is empty")
	}
	if rec.Algorithm != "ed25519" {
		return fmt.Errorf("policy/keys: KeyRecord.Algorithm=%q is not in the v1 closed set {ed25519}", rec.Algorithm)
	}
	if len(rec.PublicKey) != Ed25519PublicKeySize {
		return fmt.Errorf("%w: got %d bytes, want %d", ErrInvalidPublicKey, len(rec.PublicKey), Ed25519PublicKeySize)
	}
	if rec.Handle == "" {
		return fmt.Errorf("policy/keys: KeyRecord.Handle is empty -- KMS sign cannot resolve a private key")
	}
	if len(rec.Fingerprint) != 64 {
		return fmt.Errorf("policy/keys: KeyRecord.Fingerprint must be 64 lowercase hex chars; got %d chars", len(rec.Fingerprint))
	}
	for _, r := range rec.Fingerprint {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("policy/keys: KeyRecord.Fingerprint contains non-lowercase-hex character %q", r)
		}
	}
	// Cross-check the fingerprint-pubkey invariant: the
	// `policy.keys.list_active` verb returns Fingerprint
	// AS the canonical identity of the public key, so a row
	// where Fingerprint != SHA-256(PublicKey) would silently
	// poison every downstream check that grep-matches by
	// fingerprint (the runbook, alerting, the WAL replay).
	// Catching the mismatch here keeps the invariant local to
	// the storage layer rather than requiring every consumer
	// to re-verify. Per rubber-duck critique #1 (non-blocking).
	if got := Fingerprint(rec.PublicKey); got != rec.Fingerprint {
		return fmt.Errorf("policy/keys: KeyRecord.Fingerprint=%q does not match SHA-256(PublicKey)=%q", rec.Fingerprint, got)
	}
	if rec.ValidFrom.IsZero() {
		return fmt.Errorf("policy/keys: KeyRecord.ValidFrom is zero")
	}
	return nil
}
