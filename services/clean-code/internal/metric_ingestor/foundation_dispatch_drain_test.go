package metric_ingestor_test

// Stage 9.3 iter-3 -- end-to-end drain integration test.
//
// Iter-2 evaluator item #5 fired because the in-iter cross-package
// drain test in `internal/management/set_mode_drain_test.go`
// admitted directly into `coord.BeginScan` rather than going
// through the production scan source. The flip therefore drained
// a coordinator whose in-flight set the production scan path
// never touched -- a passing test that did NOT prove
// `mgmt.set_mode` drains a real scan.
//
// This test closes that gap. It wires the SAME composition the
// metric-ingestor binary's `main()` uses when
// `cfg.AstScanRoot != ""`:
//
//	dispatcher := &RegistryBackedFoundationDispatcher{
//	    AstFiles: &DirectoryAstFileSource{
//	        Coordinator: coord, // same instance the flip drains
//	        Pool:        pool,  // routes per-file parses
//	    },
//	}
//
// then drives `dispatcher.Dispatch(...)` on one goroutine and
// `coord.SetMode(...)` on a second goroutine. The slow blocking
// worker holds the per-file parse inside
// `pool.ParseInScan`, which is bracketed by the source's
// `BeginScan` / `EndScan` pair, which keeps the coordinator's
// in-flight count > 0, which blocks the flip's
// `waitForDrain`. Releasing the worker lets the parse return,
// the source returns, the dispatcher returns, the in-flight
// count drops to 0, the flip's `applyFn` runs.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/ast/isolation"
	"forge/services/clean-code/internal/ast/parser"
	"forge/services/clean-code/internal/metric_ingestor"
	"forge/services/clean-code/internal/metrics/recipes"
)

// slowWorker is an [isolation.Worker] whose Execute blocks
// until `release` is closed. Used to hold the in-flight scan
// counter at 1 so the flip's drain wait is observable.
type slowWorker struct {
	language string
	release  chan struct{}
	entered  chan struct{} // signalled once when Execute is entered
	enterOnce sync.Once
}

func (w *slowWorker) Language() string { return w.language }
func (w *slowWorker) Close() error     { return nil }
func (w *slowWorker) Execute(ctx context.Context, _ isolation.ParseRequest) (*isolation.ParseResult, error) {
	w.enterOnce.Do(func() { close(w.entered) })
	select {
	case <-w.release:
		return &isolation.ParseResult{AstFileBytes: nil}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestRegistryBackedFoundationDispatcher_DrainsModeFlipViaDirectorySource
// pins the iter-3 end-to-end drain contract. It exercises the
// EXACT composition the metric-ingestor binary wires when
// `cfg.AstScanRoot != ""` and proves the
// `mgmt.set_mode(repo_id, mode)` flip path waits for a real
// foundation-dispatch scan to drain before mutating the
// catalog. Iter-2 evaluator item 5 called out that the
// existing management cross-package drain test only exercised
// `coord.BeginScan` directly; this test admits via the
// production `RegistryBackedFoundationDispatcher` ->
// `DirectoryAstFileSource{Coordinator, Pool}` chain so a
// regression in production composition (e.g. the Coordinator
// field getting dropped from the source, or the dispatcher
// stopping at EmptyAstFileSource) surfaces here.
func TestRegistryBackedFoundationDispatcher_DrainsModeFlipViaDirectorySource(t *testing.T) {
	t.Parallel()
	repoID := mustNewV4(t)
	const sha = "0000000000000000000000000000000000000001"

	// On-disk per-commit checkout: one supported-language
	// file is enough to force the dispatcher into
	// pool.ParseInScan.
	tmp := t.TempDir()
	commitRoot := filepath.Join(tmp, repoID.String(), sha)
	if err := os.MkdirAll(commitRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", commitRoot, err)
	}
	if err := os.WriteFile(filepath.Join(commitRoot, "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Coordinator with a hydrator that returns embedded for
	// the test repo. The flip target is `linked` so we
	// exercise the REAL flip path (same-mode short-circuits
	// the drain).
	hydrator := func(_ context.Context, id uuid.UUID) (isolation.Mode, error) {
		if id != repoID {
			t.Errorf("hydrator: got unexpected repo_id=%s", id)
		}
		return isolation.ModeEmbedded, nil
	}
	coord := isolation.NewModeCoordinator(isolation.WithModeHydrator(hydrator))

	// Pool registered with a slow worker for `go`. The worker
	// blocks until `release` is closed, which keeps the
	// in-flight scan counter at 1.
	pool, err := isolation.NewPool(isolation.SubprocessConfig{Timeout: 30 * time.Second}, coord)
	if err != nil {
		t.Fatalf("isolation.NewPool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	worker := &slowWorker{
		language: "go",
		release:  make(chan struct{}),
		entered:  make(chan struct{}),
	}
	if err := pool.RegisterFactory("go", func(language string, _ isolation.SubprocessConfig) (isolation.Worker, error) {
		return worker, nil
	}); err != nil {
		t.Fatalf("pool.RegisterFactory: %v", err)
	}

	// Production-shaped composition: the source uses the
	// SAME coord + pool the flip drains against.
	source := &metric_ingestor.DirectoryAstFileSource{
		Root:        tmp,
		Parsers:     parser.DefaultRegistry(),
		Coordinator: coord,
		Pool:        pool,
	}
	// Empty recipe registry -- no drafts -> no Writer
	// dependency. We only need the dispatcher to drive the
	// source's BeginScan/parse loop.
	dispatcher := &metric_ingestor.RegistryBackedFoundationDispatcher{
		Registry: recipes.NewRegistry(),
		AstFiles: source,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Goroutine 1: drive the dispatcher. Blocks at
	// pool.ParseInScan inside source.Files() because the slow
	// worker is holding.
	dispatchDone := make(chan error, 1)
	go func() {
		dispatchDone <- dispatcher.Dispatch(ctx, metric_ingestor.ScanRunContext{
			ID:     mustNewV7(t),
			RepoID: repoID,
			Kind:   metric_ingestor.ScanRunKindFull,
			SHA:    sha,
		}, metric_ingestor.FoundationInput{SHA: sha})
	}()

	// Wait until the slow worker is actually inside
	// Execute (proves the scan token has been admitted into
	// the coordinator AND ParseInScan has dispatched to the
	// worker). Without this barrier we'd race the flip
	// against the dispatcher entering source.Files().
	select {
	case <-worker.entered:
	case <-ctx.Done():
		t.Fatalf("worker never entered Execute: %v", ctx.Err())
	}

	// At this point inFlight==1 for repoID. Drive SetMode on
	// a second goroutine; assert it does NOT complete while
	// the scan is held. The applyFn ALSO records when it ran
	// so we can prove ordering -- flip applies AFTER scan
	// ends, not before.
	var (
		flipApplied atomic.Bool
		scanEnded   atomic.Bool
	)
	flipDone := make(chan error, 1)
	flipPrev := make(chan isolation.Mode, 1)
	go func() {
		prev, _, err := coord.SetMode(ctx, repoID, isolation.ModeLinked, func(_ context.Context) error {
			flipApplied.Store(true)
			// If applyFn runs while a scan is still in
			// flight, the drain contract has been violated.
			if !scanEnded.Load() {
				t.Errorf("flip applyFn ran while scanEnded=false (drain contract violated)")
			}
			return nil
		})
		flipPrev <- prev
		flipDone <- err
	}()

	// Observe that SetMode is BLOCKED on drain. 250 ms is
	// an order of magnitude longer than the cross-goroutine
	// signalling cost; a passing iter-1 layout (no shared
	// coordinator) would complete the flip immediately
	// because its in-flight set is empty.
	select {
	case err := <-flipDone:
		t.Fatalf("SetMode returned (err=%v) before scan ended; drain barrier did not block as expected", err)
	case <-time.After(250 * time.Millisecond):
		// Expected: the flip is blocked on the in-flight
		// scan we are still holding.
	}
	if flipApplied.Load() {
		t.Fatalf("flipApplied=true before release; applyFn ran while scan was in flight")
	}

	// Release the worker. The parse returns, source.Files()
	// returns, dispatcher.Dispatch returns, EndScan fires,
	// inFlight drops to 0, flip's drain wait wakes, applyFn
	// runs, SetMode returns.
	scanEnded.Store(true) // ordered: flag flipped BEFORE pool.Execute returns to the caller
	close(worker.release)

	select {
	case err := <-dispatchDone:
		if err != nil {
			t.Fatalf("dispatcher.Dispatch: unexpected error: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("dispatcher never returned: %v", ctx.Err())
	}

	select {
	case err := <-flipDone:
		if err != nil {
			t.Fatalf("SetMode: unexpected error: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("SetMode never returned: %v", ctx.Err())
	}

	if !flipApplied.Load() {
		t.Fatalf("flipApplied=false; SetMode did not run applyFn")
	}
	prev := <-flipPrev
	if prev != isolation.ModeEmbedded {
		t.Errorf("SetMode previous mode = %q, want %q", prev, isolation.ModeEmbedded)
	}
}

// TestRegistryBackedFoundationDispatcher_HydratorErrorSurfacesFromBeginScan
// is the negative companion. It pins that an unhydrated repo
// (hydrator returns [isolation.ErrModeNotHydrated]) surfaces
// from the dispatcher path as a wrapped BeginScan error rather
// than a silent "zero files" success. The Stage 9.3 brief
// requires the coordinator to refuse cold-start admissions
// that would otherwise default to embedded and disagree with a
// persisted `linked` row; this test proves that refusal
// propagates all the way through the production composition.
func TestRegistryBackedFoundationDispatcher_HydratorErrorSurfacesFromBeginScan(t *testing.T) {
	t.Parallel()
	repoID := mustNewV4(t)
	const sha = "0000000000000000000000000000000000000002"

	tmp := t.TempDir()
	commitRoot := filepath.Join(tmp, repoID.String(), sha)
	if err := os.MkdirAll(commitRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", commitRoot, err)
	}
	if err := os.WriteFile(filepath.Join(commitRoot, "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	hydrator := func(_ context.Context, _ uuid.UUID) (isolation.Mode, error) {
		return "", isolation.ErrModeNotHydrated
	}
	coord := isolation.NewModeCoordinator(isolation.WithModeHydrator(hydrator))
	pool, err := isolation.NewPool(isolation.SubprocessConfig{Timeout: time.Second}, coord)
	if err != nil {
		t.Fatalf("isolation.NewPool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	// Register a no-op worker so the test does not depend on
	// production exec-worker child paths -- the request never
	// reaches the worker because BeginScan refuses.
	if err := pool.RegisterFactory("go", func(language string, _ isolation.SubprocessConfig) (isolation.Worker, error) {
		return &slowWorker{language: language, release: make(chan struct{}), entered: make(chan struct{})}, nil
	}); err != nil {
		t.Fatalf("pool.RegisterFactory: %v", err)
	}

	source := &metric_ingestor.DirectoryAstFileSource{
		Root:        tmp,
		Parsers:     parser.DefaultRegistry(),
		Coordinator: coord,
		Pool:        pool,
	}
	dispatcher := &metric_ingestor.RegistryBackedFoundationDispatcher{
		Registry: recipes.NewRegistry(),
		AstFiles: source,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)

	err = dispatcher.Dispatch(ctx, metric_ingestor.ScanRunContext{
		ID:     mustNewV7(t),
		RepoID: repoID,
		Kind:   metric_ingestor.ScanRunKindFull,
		SHA:    sha,
	}, metric_ingestor.FoundationInput{SHA: sha})
	if err == nil {
		t.Fatalf("dispatcher.Dispatch: want non-nil error for unhydrated repo, got nil")
	}
	if !errors.Is(err, isolation.ErrModeNotHydrated) {
		t.Fatalf("dispatcher.Dispatch err=%v, want errors.Is(err, ErrModeNotHydrated)=true", err)
	}
}
