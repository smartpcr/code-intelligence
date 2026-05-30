// -----------------------------------------------------------------------
// <copyright file="scopebinding.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package scopebinding

import (
	"fmt"
	"sync"

	"github.com/gofrs/uuid"
)

// scopeNamespace is the UUID v5 namespace used by MintScopeID.
var scopeNamespace = uuid.Must(uuid.FromString("6ba7b810-9dad-11d1-80b4-00c04fd430c8"))

// ScopeBinding associates a scope identifier with its source location.
type ScopeBinding struct {
	ScopeID   uuid.UUID
	ScopeKind string
	FilePath  string
	StartLine int
	EndLine   int
	Signature string
	Language  string
}

// ---------------------------------------------------------------------------
// MintScopeID / TryMintScopeID — deterministic UUID v5 derivation
// ---------------------------------------------------------------------------

// MintScopeID returns a deterministic UUID v5 derived from the supplied
// (repoID, scopeKind, canonicalSignature, firstSeenSHA) tuple.
// It panics when repoID is uuid.Nil.
func MintScopeID(repoID uuid.UUID, scopeKind, canonicalSignature, firstSeenSHA string) uuid.UUID {
	id, err := TryMintScopeID(repoID, scopeKind, canonicalSignature, firstSeenSHA)
	if err != nil {
		panic(err)
	}
	return id
}

// TryMintScopeID is the error-returning variant of MintScopeID.
func TryMintScopeID(repoID uuid.UUID, scopeKind, canonicalSignature, firstSeenSHA string) (uuid.UUID, error) {
	if repoID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("scopebinding: repoID must not be uuid.Nil")
	}
	name := fmt.Sprintf("%s:%s:%s:%s", repoID, scopeKind, canonicalSignature, firstSeenSHA)
	return uuid.NewV5(scopeNamespace, name), nil
}

// ---------------------------------------------------------------------------
// Table — concurrent-safe binding store keyed by uuid.UUID
// ---------------------------------------------------------------------------

// Table is a concurrent-safe in-memory store for ScopeBinding values,
// keyed by ScopeID.
type Table struct {
	m   sync.Map
	len int64
	mu  sync.Mutex
}

// NewTable returns a new empty Table.
func NewTable() *Table {
	return &Table{}
}

// Insert adds or replaces the binding for b.ScopeID.
// Inserting a binding with a zero ScopeID is a no-op.
func (t *Table) Insert(b ScopeBinding) {
	if b.ScopeID == uuid.Nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, loaded := t.m.LoadOrStore(b.ScopeID, b); loaded {
		t.m.Store(b.ScopeID, b)
	} else {
		t.len++
	}
}

// Get returns the binding for the supplied scope ID and true,
// or the zero value and false when no binding exists.
func (t *Table) Get(scopeID uuid.UUID) (ScopeBinding, bool) {
	v, ok := t.m.Load(scopeID)
	if !ok {
		return ScopeBinding{}, false
	}
	return v.(ScopeBinding), true
}

// Len returns the number of bindings in the table.
func (t *Table) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return int(t.len)
}

// ---------------------------------------------------------------------------
// Store — simple non-concurrent store keyed by string scope ID
// ---------------------------------------------------------------------------

// Store is a simple in-memory store for ScopeBinding values,
// keyed by ScopeID (as string).
type Store struct {
	bindings map[string]ScopeBinding
}

// NewStore returns a new empty Store.
func NewStore() *Store {
	return &Store{bindings: make(map[string]ScopeBinding)}
}

// Put inserts or replaces the binding for b.ScopeID.
func (s *Store) Put(b ScopeBinding) {
	s.bindings[b.ScopeID.String()] = b
}

// GetByString returns the binding for the supplied scope ID string and an error
// if not found.
func (s *Store) GetByString(scopeID string) (ScopeBinding, error) {
	b, ok := s.bindings[scopeID]
	if !ok {
		return ScopeBinding{}, fmt.Errorf("scope binding not found: %s", scopeID)
	}
	return b, nil
}