package repoindexer

import (
	"database/sql"
	"log/slog"
	"strings"
	"testing"

	_ "github.com/lib/pq" // driver registration for the stub *sql.DB

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
)

// TestNewWorker_panicsOnNilCorePieces guards the constructor
// contract: a nil DB, nil writer, nil materializer, or nil
// publisher is a programming error worth panicking on rather
// than surfacing at the first claim. The publisher requirement
// is load-bearing -- a nil publisher would silently disable
// the `repo.registered` / `repo.full_ingested` lifecycle events
// downstream subscribers depend on.
func TestNewWorker_panicsOnNilCorePieces(t *testing.T) {
	t.Parallel()
	mustPanic := func(name string, fn func()) {
		defer func() {
			if r := recover(); r == nil {
				t.Errorf("%s: expected panic, got none", name)
			}
		}()
		fn()
	}
	// Each sub-case panics for the FIRST nil it encounters in
	// constructor order (db -> writer -> materializer ->
	// publisher), so we cannot validate publisher-nil while
	// db is nil. Build progressively-valid options to drive
	// each panic.
	mustPanic("nil db", func() {
		_ = NewWorker(nil, nil, WorkerOptions{})
	})
	// Use a real *sql.DB shell that never connects -- the
	// constructor only requires non-nil; it does not Ping.
	stubDB, _ := sql.Open("postgres", "postgres://stub:stub@127.0.0.1:1/none")
	defer func() { _ = stubDB.Close() }()
	mustPanic("nil writer", func() {
		_ = NewWorker(stubDB, nil, WorkerOptions{})
	})
	stubWriter := graphwriter.New(stubDB, slog.Default())
	mustPanic("nil materializer", func() {
		_ = NewWorker(stubDB, stubWriter, WorkerOptions{})
	})
	mustPanic("nil publisher", func() {
		_ = NewWorker(stubDB, stubWriter, WorkerOptions{
			Materializer: &InMemoryMaterializer{Files: []InMemoryFile{{RelPath: "x", Content: []byte("x")}}},
		})
	})
}

// TestPool_rejectsZeroWorkersWhenExplicitlySet ensures the
// constructor catches a `Workers: -1` or similar misconfig
// rather than silently spawning zero goroutines and looking
// healthy while no jobs ever process.
func TestPool_rejectsNegativeWorkers(t *testing.T) {
	t.Parallel()
	// Pool requires a non-nil db + writer + materializer;
	// pass nils intentionally and confirm the workers
	// argument is checked BEFORE NewWorker would panic.
	// The order matters: we want the constructor to refuse
	// the "no workers" misconfiguration up front.
	_, err := NewPool(nil, nil, PoolConfig{Workers: -1})
	if err == nil {
		t.Fatal("expected error for negative workers; got nil")
	}
	if !strings.Contains(err.Error(), "workers must be >= 1") {
		t.Errorf("error did not mention workers bound: %v", err)
	}
}

// TestItoa_smallValues guards the tiny stdlib-free int formatter
// used by Pool worker IDs. Pinning behaviour here rather than
// relying on strconv keeps the import set tight; the helper is
// trivial enough that a couple of cases suffice.
func TestItoa_smallValues(t *testing.T) {
	t.Parallel()
	cases := map[int]string{0: "0", 1: "1", 4: "4", 10: "10", 999: "999", -7: "-7"}
	for in, want := range cases {
		if got := itoa(in); got != want {
			t.Errorf("itoa(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestNullableSHA_emptyStringRoundsToNull pins the
// `nullableSHA` shim used by the markDoneAndPublish predicate.
// Empty Go-side strings MUST collapse to a SQL NULL so the
// in-tx EXISTS lookup matches the dedupe UNIQUE INDEX
// (which is keyed on `COALESCE(from_sha, ”)` and rejects two
// rows where the COALESCE'd value collides). A bug here would
// silently let `repo.registered` fire twice for the same
// (repo, sha) tuple when one row had `from_sha = ”` and another
// had `from_sha = NULL`.
func TestNullableSHA_emptyStringRoundsToNull(t *testing.T) {
	t.Parallel()
	if got := nullableSHA(""); got != nil {
		t.Errorf("nullableSHA(\"\") = %v (%T); want nil", got, got)
	}
	if got := nullableSHA("abc123"); got != "abc123" {
		t.Errorf("nullableSHA(\"abc123\") = %v; want \"abc123\"", got)
	}
}

// TestNewWorker_defaultsMaxAttempts pins the worker's
// fall-through behaviour when WorkerOptions.MaxAttempts is left
// zero: the constructor MUST substitute `DefaultMaxAttempts`
// rather than zero (which would short-circuit the
// requeue-for-retry path on every transient publish failure
// and silently regress fix #2).
func TestNewWorker_defaultsMaxAttempts(t *testing.T) {
	t.Parallel()
	stubDB, _ := sql.Open("postgres", "postgres://stub:stub@127.0.0.1:1/none")
	defer func() { _ = stubDB.Close() }()
	stubWriter := graphwriter.New(stubDB, slog.Default())
	w := NewWorker(stubDB, stubWriter, WorkerOptions{
		Materializer: &InMemoryMaterializer{Files: []InMemoryFile{{RelPath: "x", Content: []byte("x")}}},
		Publisher:    &recordingEventPublisher{},
	})
	if w.maxAttempts != DefaultMaxAttempts {
		t.Errorf("worker.maxAttempts = %d; want DefaultMaxAttempts (%d)",
			w.maxAttempts, DefaultMaxAttempts)
	}
}
