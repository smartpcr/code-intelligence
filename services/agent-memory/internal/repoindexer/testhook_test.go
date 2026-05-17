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
//
// Stage 3.4 added EmitResult to the interface; the recording
// emitter returns an empty EmitResult by default so existing
// full-mode tests keep passing. Delta-mode tests that need a
// non-empty TouchedNodes list inject a `Result` value here.
type recordingASTEmitter struct {
	mu    sync.Mutex
	calls []EmitFileEvent
	// Result is returned verbatim from every EmitFile call so
	// delta-mode tests can stage what the dispatcher "touched"
	// for the file. The zero value yields an empty TouchedNodes
	// list which is what full-mode tests expect.
	Result EmitResult
	// ResultByPath optionally overrides Result per RelPath so a
	// single emitter can simulate distinct TouchedNodes lists
	// for distinct files in the same EmitFile sequence. The map
	// is consulted before Result; on miss Result is used.
	ResultByPath map[string]EmitResult
	// EmitErr, when non-nil, is returned as the EmitFile error
	// from the FIRST call only. Subsequent calls succeed with
	// Result. Used by tests that want to assert "the worker
	// surfaces EmitFile errors as job failures".
	EmitErr error
}

func (r *recordingASTEmitter) EmitFile(_ context.Context, ev EmitFileEvent) (EmitResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, ev)
	if r.EmitErr != nil {
		err := r.EmitErr
		r.EmitErr = nil
		return EmitResult{}, err
	}
	if r.ResultByPath != nil {
		if res, ok := r.ResultByPath[ev.RelPath]; ok {
			return res, nil
		}
	}
	return r.Result, nil
}

func (r *recordingASTEmitter) events() []EmitFileEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]EmitFileEvent, len(r.calls))
	copy(out, r.calls)
	return out
}
