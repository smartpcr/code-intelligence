package repoindexer

import (
	"context"
	"database/sql"
	"sync"
)

// recordingEventPublisher is the test-only EventPublisher used
// by the worker integration tests and the publisher unit tests.
// It captures every Publish/PublishTx call in insertion order,
// optionally returning a configured failure to exercise the
// worker's "publish failure rolls the done-transition tx back"
// branch.
//
// Lives in a `_test.go` file so the symbol is never compiled
// into the production library.
type recordingEventPublisher struct {
	mu     sync.Mutex
	calls  []Event
	err    error
	failOn map[string]error // per-kind failure override
}

func (r *recordingEventPublisher) Publish(_ context.Context, ev Event) error {
	return r.record(ev)
}

// PublishTx satisfies the EventPublisher interface for the
// atomic publish-and-done path. The recorder ignores the *sql.Tx
// argument because tests assert on the captured Event sequence
// rather than on PostgreSQL's NOTIFY queue. A non-nil
// configured `failOn`/`err` value is returned BEFORE recording
// so the tx-rollback codepath in markDoneAndPublish is exercised
// without polluting the captured-event list with the failed
// attempt.
func (r *recordingEventPublisher) PublishTx(_ context.Context, _ *sql.Tx, ev Event) error {
	return r.record(ev)
}

func (r *recordingEventPublisher) record(ev Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if perKind, ok := r.failOn[ev.Kind]; ok && perKind != nil {
		return perKind
	}
	if r.err != nil {
		return r.err
	}
	r.calls = append(r.calls, ev)
	return nil
}

func (r *recordingEventPublisher) events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.calls))
	copy(out, r.calls)
	return out
}

// recordingASTEmitter is the test-only ASTEmitter used by the
// worker integration tests. Records each EmitFile call so the
// test can assert "one EmitFile per File Node" without depending
// on a real parser.
type recordingASTEmitter struct {
	mu    sync.Mutex
	calls []EmitFileEvent
}

func (r *recordingASTEmitter) EmitFile(_ context.Context, ev EmitFileEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, ev)
	return nil
}

func (r *recordingASTEmitter) events() []EmitFileEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]EmitFileEvent, len(r.calls))
	copy(out, r.calls)
	return out
}
