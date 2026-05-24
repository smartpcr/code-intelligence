// Package sample is a fixture file for the clean-code v1
// Go parser adapter. Stage 2.1 tests assert the parser
// extracts the canonical AST scopes from this file (file,
// package, two interfaces, one class-equivalent struct, and
// three method scopes).
package sample

import (
	"context"
	"errors"
	"fmt"
)

// ErrSampleClosed is returned by Sampler methods when the
// receiver has already been closed.
var ErrSampleClosed = errors.New("sample: already closed")

// Sampler is the interface every fixture client implements.
type Sampler interface {
	// Sample takes a context and an int seed, returning an
	// observation and possibly an error.
	Sample(ctx context.Context, seed int) (string, error)
	// Close releases sampler resources.
	Close() error
}

// Filter is a tiny single-method interface used to assert the
// parser emits ScopeKindInterface for every interface
// declaration in a file (not just the first).
type Filter interface {
	Accept(value string) bool
}

// MemorySampler is a struct implementing the Sampler interface
// using an in-memory ring buffer. The parser should emit a
// ScopeKindClass scope for this declaration with attribute
// `go_type=struct`.
type MemorySampler struct {
	closed bool
	values []string
}

// New returns a fresh MemorySampler.
func New(initial []string) *MemorySampler {
	return &MemorySampler{values: append([]string(nil), initial...)}
}

// Sample implements Sampler by returning the seed-indexed
// value modulo the buffer length.
func (m *MemorySampler) Sample(ctx context.Context, seed int) (string, error) {
	if m.closed {
		return "", ErrSampleClosed
	}
	if len(m.values) == 0 {
		return "", fmt.Errorf("sample: empty buffer")
	}
	return m.values[seed%len(m.values)], nil
}

// Close implements Sampler by flipping the closed flag.
func (m *MemorySampler) Close() error {
	m.closed = true
	return nil
}
