package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// walTestDir creates a fresh temp dir + a Metrics for the WAL
// to record into.  Returns the dir + the metrics; both are
// torn down by t.Cleanup.
func walTestDir(t *testing.T) (string, *Metrics) {
	t.Helper()
	dir := t.TempDir()
	m := &Metrics{}
	return dir, m
}

// makeInput builds a minimal EpisodeAppendInput for WAL
// roundtrip assertions.  Each call increments a counter so
// the test can assert arrival order.
func makeInput(id int) EpisodeAppendInput {
	return EpisodeAppendInput{
		EpisodeID:      fmt.Sprintf("ep-%04d", id),
		EpisodeGroupID: "group-1",
		RepoID:         "repo-1",
		SessionID:      "sess-1",
		TraceID:        fmt.Sprintf("trace-%d", id),
		Kind:           "agent",
		ContextID:      "ctx-1",
		ActionJSON:     json.RawMessage(`{"action":"step"}`),
		Outcome:        "success",
		CreatedAt:      time.Date(2025, 11, 12, 23, 59, id, 0, time.UTC),
		Observations: []ObservationAppendInput{
			{
				ObservationID: fmt.Sprintf("obs-%04d", id),
				Role:          "node_hit",
				NodeID:        "node-1",
				Weight:        0.5,
				CreatedAt:     time.Date(2025, 11, 12, 23, 59, id, 0, time.UTC),
			},
		},
	}
}

// recordingWriter is a unit-test EpisodeAppender used to
// observe what Drain feeds into it (and, optionally, to fail
// on a particular call number).
type recordingWriter struct {
	mu     sync.Mutex
	calls  []EpisodeAppendInput
	failOn int // 1-indexed; 0 disables
}

func (r *recordingWriter) Append(_ context.Context, in EpisodeAppendInput) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, in)
	if r.failOn != 0 && len(r.calls) == r.failOn {
		return errors.New("recordingWriter: simulated failure")
	}
	return nil
}

func (r *recordingWriter) snapshot() []EpisodeAppendInput {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]EpisodeAppendInput, len(r.calls))
	copy(out, r.calls)
	return out
}

// -- Roundtrip -------------------------------------------------------

// Enqueue 3, Drain all → writer sees them in ARRIVAL ORDER
// and the offset advances to EOF.
func TestFileWAL_Roundtrip(t *testing.T) {
	dir, m := walTestDir(t)
	wal, err := NewFileWAL(dir, FileWALOptions{Metrics: m})
	if err != nil {
		t.Fatalf("NewFileWAL: %v", err)
	}
	defer wal.Close()

	for i := 1; i <= 3; i++ {
		if err := wal.Enqueue(context.Background(), makeInput(i)); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	if got := wal.Depth(); got != 3 {
		t.Fatalf("depth after 3 enqueues: got %d want 3", got)
	}
	if m.WALDepth() != 3 {
		t.Fatalf("metric depth after 3 enqueues: got %d want 3", m.WALDepth())
	}

	w := &recordingWriter{}
	drained, err := wal.Drain(context.Background(), w, 10)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if drained != 3 {
		t.Fatalf("drained: got %d want 3", drained)
	}
	calls := w.snapshot()
	if len(calls) != 3 {
		t.Fatalf("writer calls: got %d want 3", len(calls))
	}
	for i, c := range calls {
		want := fmt.Sprintf("ep-%04d", i+1)
		if c.EpisodeID != want {
			t.Fatalf("arrival order violated at %d: got %q want %q", i, c.EpisodeID, want)
		}
	}
	if got := wal.Depth(); got != 0 {
		t.Fatalf("depth after drain: got %d want 0", got)
	}
	if m.WALDepth() != 0 {
		t.Fatalf("metric depth after drain: got %d want 0", m.WALDepth())
	}
}

// Drain stops at the first writer error, the failed entry
// remains at the head, and depth reflects the unread count.
func TestFileWAL_DrainStopsOnWriterError(t *testing.T) {
	dir, m := walTestDir(t)
	wal, err := NewFileWAL(dir, FileWALOptions{Metrics: m})
	if err != nil {
		t.Fatalf("NewFileWAL: %v", err)
	}
	defer wal.Close()

	for i := 1; i <= 3; i++ {
		if err := wal.Enqueue(context.Background(), makeInput(i)); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	w := &recordingWriter{failOn: 2}
	drained, err := wal.Drain(context.Background(), w, 10)
	if err == nil {
		t.Fatalf("expected drain to error, got nil")
	}
	if drained != 1 {
		t.Fatalf("drained: got %d want 1 (first call succeeded, second failed)", drained)
	}
	// Depth = 2 (the failed entry + the un-attempted third).
	if got := wal.Depth(); got != 2 {
		t.Fatalf("depth after partial drain: got %d want 2", got)
	}

	// Retry with a healthy writer.  The remaining 2 entries
	// flow through, starting with the previously-failed one.
	w2 := &recordingWriter{}
	drained, err = wal.Drain(context.Background(), w2, 10)
	if err != nil {
		t.Fatalf("retry Drain: %v", err)
	}
	if drained != 2 {
		t.Fatalf("retry drained: got %d want 2", drained)
	}
	got := w2.snapshot()
	if len(got) != 2 || got[0].EpisodeID != "ep-0002" || got[1].EpisodeID != "ep-0003" {
		t.Fatalf("retry order violated: %+v", got)
	}
	if wal.Depth() != 0 {
		t.Fatalf("depth after second drain: got %d want 0", wal.Depth())
	}
}

// Drain respects the batch cap so a large backlog cannot
// starve the rest of the flusher loop.
func TestFileWAL_DrainBatchCap(t *testing.T) {
	dir, _ := walTestDir(t)
	wal, err := NewFileWAL(dir, FileWALOptions{})
	if err != nil {
		t.Fatalf("NewFileWAL: %v", err)
	}
	defer wal.Close()

	for i := 1; i <= 5; i++ {
		if err := wal.Enqueue(context.Background(), makeInput(i)); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	w := &recordingWriter{}
	drained, err := wal.Drain(context.Background(), w, 2)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if drained != 2 {
		t.Fatalf("drained: got %d want 2 (capped)", drained)
	}
	if wal.Depth() != 3 {
		t.Fatalf("depth after capped drain: got %d want 3", wal.Depth())
	}
}

// -- Crash recovery --------------------------------------------------

// Restart simulation: enqueue some, close, reopen with a new
// FileWAL pointing at the same dir → depth is rebuilt from
// disk and a drain proceeds in arrival order.
func TestFileWAL_RestartRecoversDepth(t *testing.T) {
	dir, m := walTestDir(t)
	wal, err := NewFileWAL(dir, FileWALOptions{Metrics: m})
	if err != nil {
		t.Fatalf("NewFileWAL: %v", err)
	}
	for i := 1; i <= 4; i++ {
		if err := wal.Enqueue(context.Background(), makeInput(i)); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	// Drain 2 only, then close.
	w1 := &recordingWriter{}
	if _, err := wal.Drain(context.Background(), w1, 2); err != nil {
		t.Fatalf("Drain (partial): %v", err)
	}
	if wal.Depth() != 2 {
		t.Fatalf("pre-close depth: got %d want 2", wal.Depth())
	}
	if err := wal.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen against the same dir with a NEW Metrics so we
	// can verify it gets seeded on construction.
	m2 := &Metrics{}
	wal2, err := NewFileWAL(dir, FileWALOptions{Metrics: m2})
	if err != nil {
		t.Fatalf("reopen NewFileWAL: %v", err)
	}
	defer wal2.Close()
	if wal2.Depth() != 2 {
		t.Fatalf("recovered depth: got %d want 2", wal2.Depth())
	}
	if m2.WALDepth() != 2 {
		t.Fatalf("recovered metric depth: got %d want 2", m2.WALDepth())
	}

	w2 := &recordingWriter{}
	drained, err := wal2.Drain(context.Background(), w2, 10)
	if err != nil {
		t.Fatalf("post-recovery Drain: %v", err)
	}
	if drained != 2 {
		t.Fatalf("post-recovery drained: got %d want 2", drained)
	}
	got := w2.snapshot()
	if len(got) != 2 || got[0].EpisodeID != "ep-0003" || got[1].EpisodeID != "ep-0004" {
		t.Fatalf("recovery arrival order: %+v", got)
	}
}

// A partial trailing line (write that crashed mid-Enqueue)
// is NOT replayed and does NOT advance the offset; once the
// rest of the line lands (a future complete Enqueue) it is
// processed normally.
func TestFileWAL_PartialTrailingLineIgnored(t *testing.T) {
	dir, _ := walTestDir(t)
	wal, err := NewFileWAL(dir, FileWALOptions{})
	if err != nil {
		t.Fatalf("NewFileWAL: %v", err)
	}
	if err := wal.Enqueue(context.Background(), makeInput(1)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := wal.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Simulate a partial trailing line by appending bytes
	// without a terminating newline.
	f, err := os.OpenFile(filepath.Join(dir, walFileName), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open data: %v", err)
	}
	if _, err := f.Write([]byte(`{"EpisodeID":"truncated"`)); err != nil {
		t.Fatalf("partial write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close data: %v", err)
	}

	wal2, err := NewFileWAL(dir, FileWALOptions{})
	if err != nil {
		t.Fatalf("reopen NewFileWAL: %v", err)
	}
	defer wal2.Close()
	if wal2.Depth() != 1 {
		t.Fatalf("recovered depth must ignore partial trailing line: got %d want 1", wal2.Depth())
	}
	w := &recordingWriter{}
	drained, err := wal2.Drain(context.Background(), w, 10)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if drained != 1 {
		t.Fatalf("drained: got %d want 1", drained)
	}
}

// A corrupted (un-parsable) line advances the offset rather
// than stalling the drain forever.
func TestFileWAL_CorruptLineSkipped(t *testing.T) {
	dir, _ := walTestDir(t)
	wal, err := NewFileWAL(dir, FileWALOptions{})
	if err != nil {
		t.Fatalf("NewFileWAL: %v", err)
	}
	defer wal.Close()
	if err := wal.Enqueue(context.Background(), makeInput(1)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Append a corrupted line directly.  Easiest way to
	// simulate corruption mid-stream — a real-world cause
	// would be a buggy enqueue or a partial disk corruption.
	f, err := os.OpenFile(filepath.Join(dir, walFileName), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open data: %v", err)
	}
	if _, err := f.Write([]byte("garbage that is not json\n")); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	_ = f.Close()
	if err := wal.Enqueue(context.Background(), makeInput(2)); err != nil {
		t.Fatalf("Enqueue 2: %v", err)
	}

	w := &recordingWriter{}
	drained, err := wal.Drain(context.Background(), w, 10)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// Only the 2 valid entries reach the writer; the
	// corrupt line is skipped (NOT counted in `drained`).
	if drained != 2 {
		t.Fatalf("drained valid: got %d want 2", drained)
	}
	got := w.snapshot()
	if len(got) != 2 || got[0].EpisodeID != "ep-0001" || got[1].EpisodeID != "ep-0002" {
		t.Fatalf("post-corrupt order: %+v", got)
	}
}

// -- Process-exclusivity lock ----------------------------------------

// A second NewFileWAL against the same dir while the first is
// still open returns ErrWALAlreadyOpen.  Closing the first
// frees the lock so a subsequent open succeeds.
func TestFileWAL_LockExclusion(t *testing.T) {
	dir, _ := walTestDir(t)
	wal, err := NewFileWAL(dir, FileWALOptions{})
	if err != nil {
		t.Fatalf("first NewFileWAL: %v", err)
	}
	_, err2 := NewFileWAL(dir, FileWALOptions{})
	if !errors.Is(err2, ErrWALAlreadyOpen) {
		t.Fatalf("expected ErrWALAlreadyOpen, got %v", err2)
	}
	if err := wal.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wal2, err := NewFileWAL(dir, FileWALOptions{})
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	_ = wal2.Close()
}

// -- Background flusher ----------------------------------------------

// StartFlusher drains a backlog within a reasonable time
// window after each Enqueue (the enqueue signals the flusher).
func TestFileWAL_FlusherDrainsOnSignal(t *testing.T) {
	dir, _ := walTestDir(t)
	wal, err := NewFileWAL(dir, FileWALOptions{})
	if err != nil {
		t.Fatalf("NewFileWAL: %v", err)
	}
	defer wal.Close()

	var drained atomic.Int64
	w := EpisodeAppenderFunc(func(_ context.Context, _ EpisodeAppendInput) error {
		drained.Add(1)
		return nil
	})
	// Slow tick — we want to prove the per-enqueue signal
	// drives the drain, not the ticker.
	wal.StartFlusher(w, 30*time.Second)

	for i := 1; i <= 3; i++ {
		if err := wal.Enqueue(context.Background(), makeInput(i)); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	// Wait up to 5s for the flusher to drain all 3.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if drained.Load() == 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := drained.Load(); got != 3 {
		t.Fatalf("flusher did not drain backlog: got %d want 3", got)
	}
	if wal.Depth() != 0 {
		t.Fatalf("post-flush depth: got %d want 0", wal.Depth())
	}
}

// Stop is idempotent and safe to call before StartFlusher.
func TestFileWAL_StopIdempotent(t *testing.T) {
	dir, _ := walTestDir(t)
	wal, err := NewFileWAL(dir, FileWALOptions{})
	if err != nil {
		t.Fatalf("NewFileWAL: %v", err)
	}
	wal.Stop() // before StartFlusher
	wal.Stop() // again
	if err := wal.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// -- Drain degraded-normalize (evaluator iter-1 item #1) -------------

// Defence in depth: even if a pre-iter-2 WAL backlog entry
// (or a hand-crafted JSON line) lacks `degraded=true` /
// `degraded_reason='episodic_log_unavailable'`, the Drain
// path normalizes the replayed payload before handing it to
// the writer. Without this, replayed Episode rows would
// land with the schema defaults (degraded=false,
// degraded_reason=NULL) and the §7.5 contract would silently
// drop on the floor. The WAL exists only to fulfil the
// episodic-log-unavailable fallback, so the normalization
// is safe.
func TestFileWAL_DrainNormalizesDegraded(t *testing.T) {
	dir, m := walTestDir(t)

	// Hand-write a JSON line that LACKS the Degraded field
	// entirely (simulating a backlog entry written by an
	// older agent build, before iter-2 added Degraded).
	dataPath := filepath.Join(dir, "observe.wal")
	legacyLine := `{"EpisodeID":"ep-legacy","EpisodeGroupID":"g","RepoID":"r","SessionID":"s","TraceID":"t","Kind":"agent","ContextID":"","ActionJSON":{"a":1},"SignalJSON":null,"Outcome":"success","CreatedAt":"2025-11-12T23:59:00Z","Observations":[]}` + "\n"
	if err := os.WriteFile(dataPath, []byte(legacyLine), 0o600); err != nil {
		t.Fatalf("seed legacy line: %v", err)
	}

	wal, err := NewFileWAL(dir, FileWALOptions{Metrics: m})
	if err != nil {
		t.Fatalf("NewFileWAL: %v", err)
	}
	defer wal.Close()

	w := &recordingWriter{}
	drained, err := wal.Drain(context.Background(), w, 10)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if drained != 1 {
		t.Fatalf("drained: got %d want 1", drained)
	}
	calls := w.snapshot()
	if len(calls) != 1 {
		t.Fatalf("writer calls: got %d want 1", len(calls))
	}
	if !calls[0].Degraded {
		t.Fatalf("Drain must normalize Degraded=true on replay, got %+v", calls[0])
	}
	if calls[0].DegradedReason != "episodic_log_unavailable" {
		t.Fatalf("Drain must normalize DegradedReason='episodic_log_unavailable' on replay, got %q",
			calls[0].DegradedReason)
	}
}
