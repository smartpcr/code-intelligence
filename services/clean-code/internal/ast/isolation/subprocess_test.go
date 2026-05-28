// Package isolation -- Stage 9.3 test surface.
//
// This file is the brief's required deliverable:
// `internal/ast/isolation/subprocess_test.go`. It pins the two
// scenarios from implementation-plan Stage 9.3:
//
//   - subprocess-oom-returns-error  (line 811)
//     "Given a parser subprocess that allocates beyond its
//      rlimit, When the parse runs, Then the host process
//      receives a typed `ParserOOM` error and the host remains
//      running (no panic)."
//   - mode-flip-drains-scans         (line 812)
//     "Given two in-flight scans for `repo_id=r1`, When
//      `mgmt.set_mode(r1, 'linked')` is called, Then both
//      scans complete under the old mode and the next scan
//      starts under `linked`."
//
// Plus rubber-duck-flagged race / cancel / error-path tests
// so the coordinator's drain contract holds under adverse
// orderings.

package isolation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/uuid"
)

// ---------------------------------------------------------------------
// TestMain -- re-exec hook for the real-subprocess OOM test.
// ---------------------------------------------------------------------

// TestMain switches the binary into "parser child" mode when
// the parent sets the [ChildEnvVar]. The child registers a
// handler that allocates a huge byte slice (designed to bust
// the rlimit) before any other work; the parent observes the
// resulting crash and asserts the typed [ErrParserOOM]
// mapping.
//
// When ChildEnvVar is not set, TestMain runs the test suite
// normally.
func TestMain(m *testing.M) {
	if IsChildProcess() {
		RegisterChildHandler(childTestHandler)
		RunChild() // never returns
	}
	os.Exit(m.Run())
}

// childTestHandler is the handler the re-execed test binary
// runs against a parse request. The behaviour is driven by a
// second env-var so the parent can ask the child to OOM /
// crash / sleep on demand.
func childTestHandler(_ context.Context, req ParseRequest) (*ParseResult, error) {
	switch os.Getenv("__ISOLATION_PARSER_CHILD_BEHAVIOUR") {
	case "oom":
		// Allocate a single large slice that exceeds the
		// rlimit. On Linux the parent passes
		// ChildMemoryLimitEnvVar=64 MiB; allocating 2 GiB
		// fails -- Go's runtime exits with code 2 and
		// emits "fatal error: runtime: out of memory" to
		// stderr (which the parent maps to ErrParserOOM
		// via classifyExitFailure's stderr-text matcher).
		size := 2 << 30 // 2 GiB
		// Touch the slice so the Go runtime actually
		// commits the pages (otherwise lazy allocation
		// might let us escape the rlimit on some kernels).
		buf := make([]byte, size)
		for i := 0; i < len(buf); i += 4096 {
			buf[i] = 1
		}
		// If somehow we got here without dying, fail
		// loudly so the parent test surfaces the bug.
		return nil, fmt.Errorf("child: allocation of %d bytes unexpectedly succeeded", size)
	case "crash":
		panic("child: deliberate crash for crash test")
	case "echo":
		return &ParseResult{
			AstFileBytes:   append([]byte("echo:"), req.Content...),
			DegradedReason: "",
		}, nil
	default:
		// Unknown behaviour -> error so the test surfaces it.
		return nil, fmt.Errorf("child: unknown __ISOLATION_PARSER_CHILD_BEHAVIOUR=%q", os.Getenv("__ISOLATION_PARSER_CHILD_BEHAVIOUR"))
	}
}

// ---------------------------------------------------------------------
// Fake worker used by the host-side tests.
// ---------------------------------------------------------------------

// fakeWorker is a hand-built [Worker] that surfaces canned
// outcomes for the host-side error-mapping tests. No subprocess
// involved -- the fake exists so the OOM / timeout / crash
// branches are exercised on every platform (including Windows,
// where rlimit is a no-op).
type fakeWorker struct {
	language string

	// behaviour pins the response the worker returns. Set
	// per-test.
	behaviour atomic.Pointer[fakeBehaviour]

	executes atomic.Int64
}

type fakeBehaviour struct {
	// returnErr is the error the worker surfaces. If nil
	// the worker returns returnResult.
	returnErr error
	// sleep is the delay before responding. Used by the
	// timeout test to ensure the parent's ctx wins.
	sleep time.Duration
	// returnResult is the success payload.
	returnResult *ParseResult
}

func (w *fakeWorker) Language() string { return w.language }
func (w *fakeWorker) Close() error     { return nil }

func (w *fakeWorker) Execute(ctx context.Context, req ParseRequest) (*ParseResult, error) {
	w.executes.Add(1)
	bptr := w.behaviour.Load()
	b := fakeBehaviour{}
	if bptr != nil {
		b = *bptr
	}
	if b.sleep > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(b.sleep):
		}
	}
	if b.returnErr != nil {
		return nil, b.returnErr
	}
	if b.returnResult != nil {
		return b.returnResult, nil
	}
	return &ParseResult{AstFileBytes: []byte("ok:" + req.Path)}, nil
}

// setBehaviour atomically swaps the fake's planned response.
func (w *fakeWorker) setBehaviour(b fakeBehaviour) {
	w.behaviour.Store(&b)
}

// fakeWorkerFactory builds a [WorkerFactory] that hands out
// the same fakeWorker every time.
func fakeWorkerFactory(w *fakeWorker) WorkerFactory {
	return func(language string, _ SubprocessConfig) (Worker, error) {
		if w.language == "" {
			w.language = language
		}
		return w, nil
	}
}

// ---------------------------------------------------------------------
// Pool-level error-mapping tests (host-side, fake worker).
// ---------------------------------------------------------------------

// TestPool_FakeWorker_OOMReturnsTypedError pins the host-side
// half of impl-plan scenario "subprocess-oom-returns-error":
// when a worker surfaces ErrParserOOM (or a *ParserCrashError
// wrapping it), the host process MUST receive a typed error
// AND remain operable -- a subsequent Parse on the same pool
// MUST succeed.
func TestPool_FakeWorker_OOMReturnsTypedError(t *testing.T) {
	t.Parallel()
	repoID := mustUUID(t)
	coord := NewModeCoordinator()
	if err := coord.HydrateMode(repoID, ModeEmbedded); err != nil {
		t.Fatalf("HydrateMode: %v", err)
	}

	pool, err := NewPool(SubprocessConfig{Timeout: time.Second}, coord)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	fake := &fakeWorker{language: "go"}
	if err := pool.RegisterFactory("go", fakeWorkerFactory(fake)); err != nil {
		t.Fatalf("RegisterFactory: %v", err)
	}

	// First call: fake surfaces ErrParserOOM. Verify the
	// host receives a typed error.
	fake.setBehaviour(fakeBehaviour{returnErr: ErrParserOOM})
	_, err = pool.Parse(context.Background(), repoID, ParseRequest{
		Language: "go", Path: "broken.go", Content: []byte("package broken"),
	})
	if err == nil {
		t.Fatalf("Pool.Parse: expected ErrParserOOM, got nil")
	}
	if !errors.Is(err, ErrParserOOM) {
		t.Fatalf("Pool.Parse error: errors.Is(err, ErrParserOOM)=false; err=%v", err)
	}
	var pce *ParserCrashError
	if !errors.As(err, &pce) {
		t.Fatalf("Pool.Parse error: errors.As(err, &ParserCrashError)=false; err=%v", err)
	}
	if pce.Language != "go" || pce.Path != "broken.go" {
		t.Fatalf("ParserCrashError context lost: language=%q path=%q; want language=go path=broken.go", pce.Language, pce.Path)
	}

	// Second call: fake returns success. The host process is
	// still alive (the test runner itself is the host); the
	// pool's worker registry is still functional. This pins
	// the brief's "does NOT crash the host" half.
	fake.setBehaviour(fakeBehaviour{returnResult: &ParseResult{AstFileBytes: []byte("ok")}})
	res, err := pool.Parse(context.Background(), repoID, ParseRequest{
		Language: "go", Path: "good.go", Content: []byte("package good"),
	})
	if err != nil {
		t.Fatalf("Pool.Parse (post-OOM): %v", err)
	}
	if got := string(res.AstFileBytes); got != "ok" {
		t.Fatalf("Pool.Parse (post-OOM) result: got %q, want %q", got, "ok")
	}
}

// TestPool_FakeWorker_TimeoutReturnsTypedError verifies the
// [SubprocessConfig.Timeout] enforcement: a worker that
// sleeps past the deadline triggers ctx.DeadlineExceeded on
// the per-call ctx, which the pool maps to ErrParserTimeout.
func TestPool_FakeWorker_TimeoutReturnsTypedError(t *testing.T) {
	t.Parallel()
	repoID := mustUUID(t)
	coord := NewModeCoordinator()
	if err := coord.HydrateMode(repoID, ModeEmbedded); err != nil {
		t.Fatalf("HydrateMode: %v", err)
	}

	pool, err := NewPool(SubprocessConfig{Timeout: 50 * time.Millisecond}, coord)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	fake := &fakeWorker{language: "go"}
	if err := pool.RegisterFactory("go", fakeWorkerFactory(fake)); err != nil {
		t.Fatalf("RegisterFactory: %v", err)
	}
	// Fake sleeps for 5x the pool timeout.
	fake.setBehaviour(fakeBehaviour{sleep: 250 * time.Millisecond})

	_, err = pool.Parse(context.Background(), repoID, ParseRequest{
		Language: "go", Path: "slow.go", Content: []byte("package slow"),
	})
	if err == nil {
		t.Fatalf("Pool.Parse: expected ErrParserTimeout, got nil")
	}
	if !errors.Is(err, ErrParserTimeout) {
		t.Fatalf("Pool.Parse error: errors.Is(err, ErrParserTimeout)=false; err=%v", err)
	}
}

// TestPool_FakeWorker_CallerCancelDistinguishedFromTimeout
// verifies the classifier distinguishes caller-cancel (returns
// context.Canceled verbatim) from per-call timeout (returns
// ErrParserTimeout). The two map to different operator-side
// back-off strategies.
func TestPool_FakeWorker_CallerCancelDistinguishedFromTimeout(t *testing.T) {
	t.Parallel()
	repoID := mustUUID(t)
	coord := NewModeCoordinator()
	if err := coord.HydrateMode(repoID, ModeEmbedded); err != nil {
		t.Fatalf("HydrateMode: %v", err)
	}

	pool, err := NewPool(SubprocessConfig{Timeout: 5 * time.Second}, coord)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	fake := &fakeWorker{language: "go"}
	if err := pool.RegisterFactory("go", fakeWorkerFactory(fake)); err != nil {
		t.Fatalf("RegisterFactory: %v", err)
	}
	fake.setBehaviour(fakeBehaviour{sleep: 500 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err = pool.Parse(ctx, repoID, ParseRequest{
		Language: "go", Path: "x.go", Content: []byte("package x"),
	})
	if err == nil {
		t.Fatalf("Pool.Parse: expected context.Canceled, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Pool.Parse error: errors.Is(err, context.Canceled)=false; err=%v", err)
	}
	if errors.Is(err, ErrParserTimeout) {
		t.Fatalf("Pool.Parse error: caller-cancel was mis-classified as ErrParserTimeout; err=%v", err)
	}
}

// TestPool_FakeWorker_GenericErrorWrappedAsCrash verifies that
// a worker error with no typed sentinel still bubbles up as a
// typed [*ParserCrashError] with [ErrParserCrash] so callers
// never see an unwrapped raw error.
func TestPool_FakeWorker_GenericErrorWrappedAsCrash(t *testing.T) {
	t.Parallel()
	repoID := mustUUID(t)
	coord := NewModeCoordinator()
	if err := coord.HydrateMode(repoID, ModeEmbedded); err != nil {
		t.Fatalf("HydrateMode: %v", err)
	}
	pool, err := NewPool(SubprocessConfig{Timeout: time.Second}, coord)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	fake := &fakeWorker{language: "go"}
	if err := pool.RegisterFactory("go", fakeWorkerFactory(fake)); err != nil {
		t.Fatalf("RegisterFactory: %v", err)
	}
	fake.setBehaviour(fakeBehaviour{returnErr: errors.New("unexpected wire error")})
	_, err = pool.Parse(context.Background(), repoID, ParseRequest{Language: "go", Path: "x.go", Content: []byte("package x")})
	if err == nil {
		t.Fatalf("Pool.Parse: expected ErrParserCrash, got nil")
	}
	if !errors.Is(err, ErrParserCrash) {
		t.Fatalf("Pool.Parse error: errors.Is(err, ErrParserCrash)=false; err=%v", err)
	}
}

// ---------------------------------------------------------------------
// Real-subprocess OOM test (re-execs the test binary).
// ---------------------------------------------------------------------

// TestPool_RealSubprocess_OOMReturnsTypedError is the
// end-to-end version of the impl-plan scenario
// "subprocess-oom-returns-error". The parent spawns the
// test binary as a parser child with a 64 MiB rlimit; the
// child attempts a 2 GiB allocation; the parent verifies it
// receives ErrParserOOM and the parent process is still
// responsive afterwards.
//
// Skipped on non-Linux platforms (rlimit semantics differ on
// macOS / are absent on Windows; production target is Linux
// per architecture Sec 9.2). On non-Linux the fake-worker
// tests above cover the host-side mapping.
func TestPool_RealSubprocess_OOMReturnsTypedError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("real-subprocess OOM uses RLIMIT_AS; %s lacks portable rlimit support", runtime.GOOS)
	}
	if testing.Short() {
		t.Skip("real-subprocess OOM is slow; skipped in -short mode")
	}
	t.Parallel()

	repoID := mustUUID(t)
	coord := NewModeCoordinator()
	if err := coord.HydrateMode(repoID, ModeEmbedded); err != nil {
		t.Fatalf("HydrateMode: %v", err)
	}

	pool, err := NewPool(SubprocessConfig{
		MemoryLimitBytes: 64 << 20, // 64 MiB
		Timeout:          30 * time.Second,
	}, coord)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	factory := func(language string, cfg SubprocessConfig) (Worker, error) {
		return NewExecWorker(language, cfg, ExecConfig{
			Program: exe,
			// Re-running TestMain triggers RunChild via
			// IsChildProcess(); add a deterministic
			// -test.run filter so we don't accidentally
			// run every test inside the child.
			Args:     []string{"-test.run=^$"},
			ExtraEnv: []string{"__ISOLATION_PARSER_CHILD_BEHAVIOUR=oom"},
		})
	}
	if err := pool.RegisterFactory("go", factory); err != nil {
		t.Fatalf("RegisterFactory: %v", err)
	}

	_, err = pool.Parse(context.Background(), repoID, ParseRequest{
		Language: "go", Path: "oom.go", Content: []byte("package oom"),
	})
	if err == nil {
		t.Fatalf("Pool.Parse: expected ErrParserOOM, got nil")
	}
	if !errors.Is(err, ErrParserOOM) {
		var pce *ParserCrashError
		extra := ""
		if errors.As(err, &pce) {
			extra = fmt.Sprintf(" exit=%d signal=%q stderr=%s", pce.ExitCode, pce.Signal, pce.StderrSnippet)
		}
		t.Fatalf("Pool.Parse error: errors.Is(err, ErrParserOOM)=false; err=%v%s", err, extra)
	}

	// Host-still-alive check: a follow-up Parse with a
	// non-OOM behaviour MUST succeed via the SAME pool.
	echoFactory := func(language string, cfg SubprocessConfig) (Worker, error) {
		return NewExecWorker(language, cfg, ExecConfig{
			Program:  exe,
			Args:     []string{"-test.run=^$"},
			ExtraEnv: []string{"__ISOLATION_PARSER_CHILD_BEHAVIOUR=echo"},
		})
	}
	// Register echo as a different language so the prior
	// "go" worker isn't reused.
	if err := pool.RegisterFactory("python", echoFactory); err != nil {
		t.Fatalf("RegisterFactory(python): %v", err)
	}
	res, err := pool.Parse(context.Background(), repoID, ParseRequest{
		Language: "python", Path: "echo.py", Content: []byte("hi"),
	})
	if err != nil {
		t.Fatalf("Pool.Parse (host-alive check): %v", err)
	}
	if want := "echo:hi"; string(res.AstFileBytes) != want {
		t.Fatalf("Pool.Parse (host-alive check) result: got %q want %q", res.AstFileBytes, want)
	}
}

// ---------------------------------------------------------------------
// ModeCoordinator drain-on-flip tests.
// ---------------------------------------------------------------------

// TestModeCoordinator_FlipDrainsInFlightScans pins the
// impl-plan scenario "mode-flip-drains-scans": two in-flight
// scans for repo r1 under mode=embedded; SetMode(r1, linked)
// MUST wait for both to complete (under embedded) before
// flipping; the next BeginScan returns mode=linked.
func TestModeCoordinator_FlipDrainsInFlightScans(t *testing.T) {
	t.Parallel()
	coord := NewModeCoordinator()
	repoID := mustUUID(t)
	if err := coord.HydrateMode(repoID, ModeEmbedded); err != nil {
		t.Fatalf("HydrateMode: %v", err)
	}

	// Admit two scans under embedded.
	tokA, err := coord.BeginScan(context.Background(), repoID)
	if err != nil {
		t.Fatalf("BeginScan A: %v", err)
	}
	tokB, err := coord.BeginScan(context.Background(), repoID)
	if err != nil {
		t.Fatalf("BeginScan B: %v", err)
	}
	if tokA.Mode() != ModeEmbedded {
		t.Fatalf("BeginScan A mode = %q, want %q", tokA.Mode(), ModeEmbedded)
	}
	if tokB.Mode() != ModeEmbedded {
		t.Fatalf("BeginScan B mode = %q, want %q", tokB.Mode(), ModeEmbedded)
	}
	if got := coord.InFlight(repoID); got != 2 {
		t.Fatalf("InFlight before flip = %d, want 2", got)
	}

	// Kick off SetMode in a goroutine and verify it BLOCKS
	// until both scans EndScan.
	applyCalled := atomic.Int32{}
	setModeDone := make(chan struct {
		prev    Mode
		changed bool
		err     error
	}, 1)
	go func() {
		prev, changed, err := coord.SetMode(context.Background(), repoID, ModeLinked, func(_ context.Context) error {
			applyCalled.Add(1)
			return nil
		})
		setModeDone <- struct {
			prev    Mode
			changed bool
			err     error
		}{prev, changed, err}
	}()

	// SetMode must NOT complete while scans are still
	// in-flight. Sleep briefly and verify.
	time.Sleep(50 * time.Millisecond)
	select {
	case r := <-setModeDone:
		t.Fatalf("SetMode returned before in-flight scans drained: prev=%q changed=%v err=%v", r.prev, r.changed, r.err)
	default:
	}
	if applyCalled.Load() != 0 {
		t.Fatalf("applyFn called before drain; called %d times", applyCalled.Load())
	}

	// Release one scan -- SetMode still waits for the
	// second.
	coord.EndScan(tokA)
	time.Sleep(25 * time.Millisecond)
	select {
	case r := <-setModeDone:
		t.Fatalf("SetMode returned with 1 scan still in-flight: prev=%q changed=%v err=%v", r.prev, r.changed, r.err)
	default:
	}

	// Release the second -- SetMode should now drain and
	// complete.
	coord.EndScan(tokB)
	select {
	case r := <-setModeDone:
		if r.err != nil {
			t.Fatalf("SetMode returned err=%v", r.err)
		}
		if r.prev != ModeEmbedded {
			t.Fatalf("SetMode previous = %q, want %q", r.prev, ModeEmbedded)
		}
		if !r.changed {
			t.Fatalf("SetMode changed = false, want true")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("SetMode did not complete within 2s after final EndScan")
	}
	if applyCalled.Load() != 1 {
		t.Fatalf("applyFn called %d times, want 1", applyCalled.Load())
	}

	// Next BeginScan should observe the NEW mode.
	tokC, err := coord.BeginScan(context.Background(), repoID)
	if err != nil {
		t.Fatalf("BeginScan C: %v", err)
	}
	if tokC.Mode() != ModeLinked {
		t.Fatalf("post-flip BeginScan mode = %q, want %q", tokC.Mode(), ModeLinked)
	}
	coord.EndScan(tokC)
}

// TestModeCoordinator_NewScansBlockedDuringFlip verifies the
// drain barrier: while SetMode is waiting for in-flight scans
// to drain, NEW BeginScan calls MUST block (they would
// otherwise see the old mode AND extend the drain window
// indefinitely).
func TestModeCoordinator_NewScansBlockedDuringFlip(t *testing.T) {
	t.Parallel()
	coord := NewModeCoordinator()
	repoID := mustUUID(t)
	if err := coord.HydrateMode(repoID, ModeEmbedded); err != nil {
		t.Fatalf("HydrateMode: %v", err)
	}
	// One scan in-flight under embedded.
	tokA, err := coord.BeginScan(context.Background(), repoID)
	if err != nil {
		t.Fatalf("BeginScan A: %v", err)
	}

	// Kick off SetMode -- will block waiting for tokA to
	// drain.
	setModeDone := make(chan error, 1)
	go func() {
		_, _, err := coord.SetMode(context.Background(), repoID, ModeLinked, func(_ context.Context) error {
			return nil
		})
		setModeDone <- err
	}()
	// Give SetMode time to mark flipping=true.
	time.Sleep(50 * time.Millisecond)

	// New BeginScan call MUST block until the flip completes.
	newScanResult := make(chan struct {
		tok ScanToken
		err error
	}, 1)
	go func() {
		tok, err := coord.BeginScan(context.Background(), repoID)
		newScanResult <- struct {
			tok ScanToken
			err error
		}{tok, err}
	}()

	time.Sleep(50 * time.Millisecond)
	select {
	case r := <-newScanResult:
		t.Fatalf("BeginScan returned during in-flight flip: tok.Mode=%q err=%v", r.tok.Mode(), r.err)
	default:
	}

	// Release the original scan; SetMode should complete
	// and then the new BeginScan should unblock with the
	// new mode.
	coord.EndScan(tokA)

	select {
	case err := <-setModeDone:
		if err != nil {
			t.Fatalf("SetMode: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("SetMode did not complete after EndScan")
	}

	select {
	case r := <-newScanResult:
		if r.err != nil {
			t.Fatalf("post-flip BeginScan: %v", r.err)
		}
		if r.tok.Mode() != ModeLinked {
			t.Fatalf("post-flip BeginScan mode = %q, want %q", r.tok.Mode(), ModeLinked)
		}
		coord.EndScan(r.tok)
	case <-time.After(2 * time.Second):
		t.Fatalf("post-flip BeginScan did not unblock within 2s")
	}
}

// TestModeCoordinator_ContextCancelInBeginScan verifies
// BeginScan honours ctx.Done() while waiting for a flip to
// clear, returning ctx.Err() rather than blocking forever.
func TestModeCoordinator_ContextCancelInBeginScan(t *testing.T) {
	t.Parallel()
	coord := NewModeCoordinator()
	repoID := mustUUID(t)
	if err := coord.HydrateMode(repoID, ModeEmbedded); err != nil {
		t.Fatalf("HydrateMode: %v", err)
	}
	// Pin a scan so SetMode never drains.
	tok, err := coord.BeginScan(context.Background(), repoID)
	if err != nil {
		t.Fatalf("BeginScan: %v", err)
	}
	t.Cleanup(func() { coord.EndScan(tok) })

	// Trigger a flip that will never drain.
	flipStarted := make(chan struct{})
	go func() {
		// Synchronise with the BeginScan below by waiting
		// briefly so flipping=true is set first.
		_, _, _ = coord.SetMode(context.Background(), repoID, ModeLinked, func(_ context.Context) error {
			close(flipStarted)
			return nil
		})
	}()
	time.Sleep(50 * time.Millisecond)

	// Now attempt BeginScan with a short-lived ctx; we
	// expect ctx.Err() rather than indefinite block.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = coord.BeginScan(ctx, repoID)
	if err == nil {
		t.Fatalf("BeginScan: expected ctx err, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("BeginScan err: errors.Is(err, context.DeadlineExceeded)=false; err=%v", err)
	}
}

// TestModeCoordinator_ContextCancelInSetMode verifies SetMode
// honours ctx.Done() while waiting for drain, AND clears the
// flipping flag on cancel so subsequent BeginScan calls can
// resume.
func TestModeCoordinator_ContextCancelInSetMode(t *testing.T) {
	t.Parallel()
	coord := NewModeCoordinator()
	repoID := mustUUID(t)
	if err := coord.HydrateMode(repoID, ModeEmbedded); err != nil {
		t.Fatalf("HydrateMode: %v", err)
	}
	// Pin a scan so the SetMode drain never completes.
	tok, err := coord.BeginScan(context.Background(), repoID)
	if err != nil {
		t.Fatalf("BeginScan: %v", err)
	}
	// Cancel SetMode mid-drain.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err = coord.SetMode(ctx, repoID, ModeLinked, func(_ context.Context) error {
		return nil
	})
	if err == nil {
		t.Fatalf("SetMode: expected ctx err, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SetMode err: errors.Is(err, context.DeadlineExceeded)=false; err=%v", err)
	}

	// After cancel the flipping flag MUST be cleared --
	// otherwise a subsequent BeginScan would block forever.
	coord.EndScan(tok)
	tokC, err := coord.BeginScan(context.Background(), repoID)
	if err != nil {
		t.Fatalf("post-cancel BeginScan: %v", err)
	}
	// Mode unchanged because applyFn never ran.
	if tokC.Mode() != ModeEmbedded {
		t.Fatalf("post-cancel BeginScan mode = %q, want %q (mode should be unchanged on cancel)", tokC.Mode(), ModeEmbedded)
	}
	coord.EndScan(tokC)
}

// TestModeCoordinator_ApplyFnErrorLeavesModeUnchanged covers
// the failure path: applyFn returns an error after drain
// completes. The coordinator MUST leave the in-memory mode
// unchanged AND clear the flipping flag so the operator can
// retry.
func TestModeCoordinator_ApplyFnErrorLeavesModeUnchanged(t *testing.T) {
	t.Parallel()
	coord := NewModeCoordinator()
	repoID := mustUUID(t)
	if err := coord.HydrateMode(repoID, ModeEmbedded); err != nil {
		t.Fatalf("HydrateMode: %v", err)
	}

	sentinel := errors.New("simulated store failure")
	_, changed, err := coord.SetMode(context.Background(), repoID, ModeLinked, func(_ context.Context) error {
		return sentinel
	})
	if err == nil {
		t.Fatalf("SetMode: expected error, got nil")
	}
	if !errors.Is(err, ErrModeFlipApplyFailed) {
		t.Fatalf("SetMode err: errors.Is(err, ErrModeFlipApplyFailed)=false; err=%v", err)
	}
	if changed {
		t.Fatalf("SetMode changed=true on applyFn failure; want false")
	}

	// Subsequent BeginScan returns old mode AND is not
	// blocked (flipping flag was cleared on error path).
	tok, err := coord.BeginScan(context.Background(), repoID)
	if err != nil {
		t.Fatalf("post-failure BeginScan: %v", err)
	}
	if tok.Mode() != ModeEmbedded {
		t.Fatalf("post-failure BeginScan mode = %q, want %q", tok.Mode(), ModeEmbedded)
	}
	coord.EndScan(tok)
}

// TestModeCoordinator_SameModeIsNoop verifies the canonical
// no-op: SetMode with the same mode does NOT take the flip
// lock and does NOT drain in-flight scans. ApplyFn is still
// invoked so the catalog can record a no-op event.
func TestModeCoordinator_SameModeIsNoop(t *testing.T) {
	t.Parallel()
	coord := NewModeCoordinator()
	repoID := mustUUID(t)
	if err := coord.HydrateMode(repoID, ModeEmbedded); err != nil {
		t.Fatalf("HydrateMode: %v", err)
	}
	// Hold an in-flight scan: a true flip would block on
	// this; a no-op MUST NOT.
	tok, err := coord.BeginScan(context.Background(), repoID)
	if err != nil {
		t.Fatalf("BeginScan: %v", err)
	}
	defer coord.EndScan(tok)

	applyCalls := atomic.Int32{}
	done := make(chan struct {
		prev    Mode
		changed bool
		err     error
	}, 1)
	go func() {
		prev, changed, err := coord.SetMode(context.Background(), repoID, ModeEmbedded, func(_ context.Context) error {
			applyCalls.Add(1)
			return nil
		})
		done <- struct {
			prev    Mode
			changed bool
			err     error
		}{prev, changed, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("SetMode (no-op): %v", r.err)
		}
		if r.changed {
			t.Fatalf("SetMode (no-op) changed=true; want false")
		}
		if r.prev != ModeEmbedded {
			t.Fatalf("SetMode (no-op) prev=%q; want %q", r.prev, ModeEmbedded)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("SetMode (no-op) did not complete within 500ms; the no-op path is incorrectly draining the in-flight scan")
	}
	if applyCalls.Load() != 1 {
		t.Fatalf("applyFn called %d times on no-op SetMode; want 1", applyCalls.Load())
	}
}

// TestModeCoordinator_DifferentReposDoNotBlock verifies a flip
// on repo r1 does NOT block scans / flips on repo r2.
func TestModeCoordinator_DifferentReposDoNotBlock(t *testing.T) {
	t.Parallel()
	coord := NewModeCoordinator()
	r1 := mustUUID(t)
	r2 := mustUUID(t)
	if err := coord.HydrateMode(r1, ModeEmbedded); err != nil {
		t.Fatalf("HydrateMode r1: %v", err)
	}
	if err := coord.HydrateMode(r2, ModeEmbedded); err != nil {
		t.Fatalf("HydrateMode r2: %v", err)
	}

	// Pin an in-flight scan on r1.
	tok1, err := coord.BeginScan(context.Background(), r1)
	if err != nil {
		t.Fatalf("BeginScan r1: %v", err)
	}
	defer coord.EndScan(tok1)

	// Kick off SetMode on r1 -- will block.
	r1Flip := make(chan error, 1)
	go func() {
		_, _, err := coord.SetMode(context.Background(), r1, ModeLinked, func(_ context.Context) error { return nil })
		r1Flip <- err
	}()
	time.Sleep(25 * time.Millisecond)

	// BeginScan + EndScan + SetMode on r2 MUST all proceed
	// while r1's flip is blocked.
	tok2, err := coord.BeginScan(context.Background(), r2)
	if err != nil {
		t.Fatalf("BeginScan r2: %v", err)
	}
	if tok2.Mode() != ModeEmbedded {
		t.Fatalf("BeginScan r2 mode = %q, want %q", tok2.Mode(), ModeEmbedded)
	}
	coord.EndScan(tok2)

	_, changed, err := coord.SetMode(context.Background(), r2, ModeLinked, func(_ context.Context) error { return nil })
	if err != nil {
		t.Fatalf("SetMode r2: %v", err)
	}
	if !changed {
		t.Fatalf("SetMode r2 changed=false; want true")
	}

	// r1's flip is still blocked.
	select {
	case <-r1Flip:
		t.Fatalf("SetMode r1 returned despite in-flight scan")
	default:
	}
}

// TestModeCoordinator_ConcurrentSetModesSerialize verifies two
// concurrent REAL flips on the SAME repo serialise via the flip
// lock (no interleaving, no deadlock). The setup forces both
// flips to be queued before either can run:
//
//  1. We hold an in-flight scan so SetMode A (embedded->linked)
//     blocks in waitForDrain.
//  2. We start SetMode B (linked->embedded) AFTER A has marked
//     flipping=true (verified by polling); B then blocks in
//     acquireFlipLock.
//  3. We end the in-flight scan; A drains, applies, swaps to
//     linked, releases the flip; B then acquires the flip,
//     applies, swaps to embedded.
//
// The test asserts: the two applyFns do NOT overlap; final
// mode is embedded; both flips reported changed=true.
func TestModeCoordinator_ConcurrentSetModesSerialize(t *testing.T) {
	t.Parallel()
	coord := NewModeCoordinator()
	repoID := mustUUID(t)
	if err := coord.HydrateMode(repoID, ModeEmbedded); err != nil {
		t.Fatalf("HydrateMode: %v", err)
	}

	// Step 1: pin an in-flight scan so any SetMode that
	// targets a different mode will block waiting for it.
	tok, err := coord.BeginScan(context.Background(), repoID)
	if err != nil {
		t.Fatalf("BeginScan: %v", err)
	}

	type window struct {
		start, end time.Time
		name       string
	}
	winCh := make(chan window, 2)
	noteApply := func(name string) func(context.Context) error {
		return func(_ context.Context) error {
			start := time.Now()
			time.Sleep(40 * time.Millisecond)
			end := time.Now()
			winCh <- window{start: start, end: end, name: name}
			return nil
		}
	}

	// Step 2: launch SetMode A (embedded -> linked); blocks
	// in waitForDrain.
	aDone := make(chan error, 1)
	go func() {
		_, _, err := coord.SetMode(context.Background(), repoID, ModeLinked, noteApply("A:embedded->linked"))
		aDone <- err
	}()
	// Wait for A to mark flipping=true. Polling is cheaper
	// than an extra coordination knob; the property tested
	// is robust.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if isFlipping(coord, repoID) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !isFlipping(coord, repoID) {
		coord.EndScan(tok)
		t.Fatalf("SetMode A did not mark flipping=true within 1s")
	}

	// Step 3: launch SetMode B (linked -> embedded); blocks
	// in acquireFlipLock (A holds the flip flag).
	bDone := make(chan error, 1)
	go func() {
		_, _, err := coord.SetMode(context.Background(), repoID, ModeEmbedded, noteApply("B:linked->embedded"))
		bDone <- err
	}()
	// Brief delay to let B reach acquireFlipLock.
	time.Sleep(20 * time.Millisecond)

	// Step 4: release the in-flight scan; A drains; A
	// applies; A swaps to linked; A releases; B acquires;
	// B applies; B swaps to embedded.
	coord.EndScan(tok)

	select {
	case err := <-aDone:
		if err != nil {
			t.Fatalf("SetMode A: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("SetMode A did not complete within 2s")
	}
	select {
	case err := <-bDone:
		if err != nil {
			t.Fatalf("SetMode B: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("SetMode B did not complete within 2s")
	}

	close(winCh)
	windows := []window{}
	for w := range winCh {
		windows = append(windows, w)
	}
	if len(windows) != 2 {
		t.Fatalf("windows len=%d, want 2; got %v", len(windows), windows)
	}
	// Sort so first is the earlier-starting window.
	first, second := windows[0], windows[1]
	if first.start.After(second.start) {
		first, second = second, first
	}
	if !first.end.Before(second.start) && !first.end.Equal(second.start) {
		t.Fatalf("apply windows overlap: first=%s..%s (%s) second=%s..%s (%s) -- flips were NOT serialised", first.start, first.end, first.name, second.start, second.end, second.name)
	}

	// Final mode is embedded (B's target, which ran second).
	mode, ok := coord.CurrentMode(repoID)
	if !ok {
		t.Fatalf("CurrentMode: !ok")
	}
	if mode != ModeEmbedded {
		t.Fatalf("final mode = %q, want %q", mode, ModeEmbedded)
	}
}

// isFlipping reads the per-repo flipping flag for a test
// assertion. Touches coordinator internals; this is a test
// file in the same package so the access is intentional.
func isFlipping(c *ModeCoordinator, repoID uuid.UUID) bool {
	s := c.getOrCreate(repoID)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flipping
}

// TestModeCoordinator_BeginScanUnhydratedRejects verifies the
// rubber-duck iter-1 fix: BeginScan on a never-hydrated repo
// MUST return ErrModeNotHydrated, never silently default to
// embedded (which would let the coordinator disagree with a
// persisted `linked` row at cold-start).
func TestModeCoordinator_BeginScanUnhydratedRejects(t *testing.T) {
	t.Parallel()
	coord := NewModeCoordinator()
	repoID := mustUUID(t)
	_, err := coord.BeginScan(context.Background(), repoID)
	if err == nil {
		t.Fatalf("BeginScan: expected ErrModeNotHydrated, got nil")
	}
	if !errors.Is(err, ErrModeNotHydrated) {
		t.Fatalf("BeginScan err: errors.Is(err, ErrModeNotHydrated)=false; err=%v", err)
	}
}

// TestModeCoordinator_HydrateModeRejectsInvalid verifies the
// AllowedModes guard.
func TestModeCoordinator_HydrateModeRejectsInvalid(t *testing.T) {
	t.Parallel()
	coord := NewModeCoordinator()
	repoID := mustUUID(t)
	err := coord.HydrateMode(repoID, Mode("bogus"))
	if !errors.Is(err, ErrInvalidMode) {
		t.Fatalf("HydrateMode err: errors.Is(err, ErrInvalidMode)=false; err=%v", err)
	}
	err = coord.HydrateMode(uuid.Nil, ModeEmbedded)
	if err == nil || !strings.Contains(err.Error(), "zero repoID") {
		t.Fatalf("HydrateMode(zero repoID): expected zero-uuid err, got %v", err)
	}
}

// TestModeCoordinator_EndScanDoublePanics verifies the
// programmer-bug guard fires.
func TestModeCoordinator_EndScanDoublePanics(t *testing.T) {
	t.Parallel()
	coord := NewModeCoordinator()
	repoID := mustUUID(t)
	if err := coord.HydrateMode(repoID, ModeEmbedded); err != nil {
		t.Fatalf("HydrateMode: %v", err)
	}
	tok, err := coord.BeginScan(context.Background(), repoID)
	if err != nil {
		t.Fatalf("BeginScan: %v", err)
	}
	coord.EndScan(tok)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("double EndScan did not panic")
		}
	}()
	coord.EndScan(tok) // expect panic
}

// ---------------------------------------------------------------------
// Pool guard tests.
// ---------------------------------------------------------------------

// TestNewPool_NilCoordinatorRejected guards the pool's hard
// requirement that a coordinator is wired.
func TestNewPool_NilCoordinatorRejected(t *testing.T) {
	t.Parallel()
	_, err := NewPool(SubprocessConfig{}, nil)
	if err == nil {
		t.Fatalf("NewPool(nil coord): expected error, got nil")
	}
}

// TestPool_UnknownLanguageRejected verifies the
// ErrUnknownLanguage path.
func TestPool_UnknownLanguageRejected(t *testing.T) {
	t.Parallel()
	coord := NewModeCoordinator()
	repoID := mustUUID(t)
	if err := coord.HydrateMode(repoID, ModeEmbedded); err != nil {
		t.Fatalf("HydrateMode: %v", err)
	}
	pool, err := NewPool(SubprocessConfig{}, coord)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	// No factory registered.
	_, err = pool.Parse(context.Background(), repoID, ParseRequest{
		Language: "ruby", Path: "x.rb", Content: []byte("puts 1"),
	})
	if !errors.Is(err, ErrUnknownLanguage) {
		t.Fatalf("Pool.Parse: errors.Is(err, ErrUnknownLanguage)=false; err=%v", err)
	}
}

// ---------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------

func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	u, err := uuid.NewV4()
	if err != nil {
		t.Fatalf("uuid.NewV4: %v", err)
	}
	return u
}
