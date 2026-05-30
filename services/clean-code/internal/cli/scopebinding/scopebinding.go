// -----------------------------------------------------------------------
// <copyright file="scopebinding.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package scopebinding

import "fmt"

// ScopeBinding associates a scope identifier with its source location.
type ScopeBinding struct {
	ScopeID   string
	ScopeKind string
	FilePath  string
	StartLine int
	EndLine   int
	Signature string
}

// Store is a simple in-memory store for ScopeBinding values,
// keyed by ScopeID.
type Store struct {
	bindings map[string]ScopeBinding
}

// NewStore returns a new empty Store.
func NewStore() *Store {
	return &Store{bindings: make(map[string]ScopeBinding)}
}

// Put inserts or replaces the binding for b.ScopeID.
func (s *Store) Put(b ScopeBinding) {
	s.bindings[b.ScopeID] = b
}

// Get returns the binding for the supplied scope ID and true,
// or the zero value and false when no binding exists.
func (s *Store) Get(scopeID string) (ScopeBinding, error) {
	b, ok := s.bindings[scopeID]
	if !ok {
		return ScopeBinding{}, fmt.Errorf("scope binding not found: %s", scopeID)
	}
	return b, nil
}