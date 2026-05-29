// Aggregator-level integration tests for the Stage 10.1
// linked-mode adapter hook: `WithLinkedEdgeReader`. These
// tests live in the `aggregator_test` package and exercise
// the `tickSystemTier` integration end-to-end -- they DO NOT
// stand up the production `internal/linked` adapter (those
// tests live in `internal/linked/client_test.go`).
package aggregator_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/aggregator"
)

// stubLinkedReader is an in-test [aggregator.LinkedEdgeReader]
// double letting each subtest pre-program a per-key reply.
type stubLinkedReader struct {
	mu    sync.Mutex
	calls int
	reply func(repoID uuid.UUID, sha string) (aggregator.LinkedEdges, error)
}

func (s *stubLinkedReader) ResolveLinkedEdges(ctx context.Context, repoID uuid.UUID, sha string) (aggregator.LinkedEdges, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.reply == nil {
		return aggregator.LinkedEdges{}, nil
	}
	return s.reply(repoID, sha)
}

func (s *stubLinkedReader) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// TestAggregator_LinkedReader_NotWired_NoEnrichment is the
// regression guard: an aggregator built WITHOUT
// `WithLinkedEdgeReader` MUST behave byte-identically to the
// pre-Stage-10.1 baseline. The system-tier inputs stay in
// embedded shape, no LinkedEdge* counters fire.
func TestAggregator_LinkedReader_NotWired_NoEnrichment(t *testing.T) {
	t.Parallel()
	tickAt := time.Date(2025, 7, 1, 12, 0, 0, 0, time.UTC)
	foundationSource := aggregator.NewInMemorySampleSource(nil)
	snapshotWriter := aggregator.NewInMemorySnapshotWriter()
	composer, err := aggregator.NewSystemTierComposer()
	if err != nil {
		t.Fatalf("NewSystemTierComposer: %v", err)
	}
	in := systemTierTickInput(t, 0)
	sysSource := aggregator.NewInMemorySystemTierInputSource([]aggregator.SystemTierInput{in})
	sysWriter := aggregator.NewInMemorySystemTierWriter()

	agg, err := aggregator.NewAggregator(
		foundationSource,
		snapshotWriter,
		aggregator.WithClock(fixedClock(tickAt)),
		aggregator.WithSystemTier(composer, sysSource, sysWriter),
	)
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	report, err := agg.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if report.LinkedEdgeReaderInvocations != 0 {
		t.Errorf("LinkedEdgeReaderInvocations = %d, want 0 (reader not wired)", report.LinkedEdgeReaderInvocations)
	}
	if report.LinkedEdgeReaderApplied != 0 {
		t.Errorf("LinkedEdgeReaderApplied = %d, want 0", report.LinkedEdgeReaderApplied)
	}
	if report.LinkedEdgeFetchFailures != 0 {
		t.Errorf("LinkedEdgeFetchFailures = %d, want 0", report.LinkedEdgeFetchFailures)
	}
}

// TestAggregator_LinkedReader_NotApplicable_LeavesEmbedded
// pins the gating verdict: when the reader returns
// `Applicable=false` (e.g. repo still embedded OR global
// flag closed) the input keeps its embedded shape so the
// composer continues to degrade the row. The invocation
// counter MUST still fire so operators can distinguish
// "wired but suppressed" from "not wired".
func TestAggregator_LinkedReader_NotApplicable_LeavesEmbedded(t *testing.T) {
	t.Parallel()
	tickAt := time.Date(2025, 7, 1, 12, 0, 0, 0, time.UTC)
	composer, _ := aggregator.NewSystemTierComposer()
	in := systemTierTickInput(t, 0)
	sysSource := aggregator.NewInMemorySystemTierInputSource([]aggregator.SystemTierInput{in})
	sysWriter := aggregator.NewInMemorySystemTierWriter()
	reader := &stubLinkedReader{
		reply: func(uuid.UUID, string) (aggregator.LinkedEdges, error) {
			return aggregator.LinkedEdges{Applicable: false}, nil
		},
	}

	agg, err := aggregator.NewAggregator(
		aggregator.NewInMemorySampleSource(nil),
		aggregator.NewInMemorySnapshotWriter(),
		aggregator.WithClock(fixedClock(tickAt)),
		aggregator.WithSystemTier(composer, sysSource, sysWriter),
		aggregator.WithLinkedEdgeReader(reader, nil),
	)
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	report, err := agg.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if reader.Calls() != 1 {
		t.Errorf("reader.Calls() = %d, want 1", reader.Calls())
	}
	if report.LinkedEdgeReaderInvocations != 1 {
		t.Errorf("LinkedEdgeReaderInvocations = %d, want 1", report.LinkedEdgeReaderInvocations)
	}
	if report.LinkedEdgeReaderApplied != 0 {
		t.Errorf("LinkedEdgeReaderApplied = %d, want 0", report.LinkedEdgeReaderApplied)
	}
	if report.LinkedEdgeFetchFailures != 0 {
		t.Errorf("LinkedEdgeFetchFailures = %d, want 0", report.LinkedEdgeFetchFailures)
	}
	// Embedded-shape contract: the composer must still emit
	// at least some degraded rows for the cross-repo-edge-
	// dependent kinds.
	if report.SystemTierDegradedSamples == 0 {
		t.Errorf("SystemTierDegradedSamples = 0; embedded-mode tick should degrade xrepo/blast rows")
	}
}

// TestAggregator_LinkedReader_Applicable_OverlaysEdges pins
// the happy path: an Applicable reply flips the input to
// linked mode and copies edges + per-family availability
// flags. The composer then emits non-degraded rows for the
// xrepo-edge-dependent kinds (xrepo_dep_depth +
// blast_radius).
func TestAggregator_LinkedReader_Applicable_OverlaysEdges(t *testing.T) {
	t.Parallel()
	tickAt := time.Date(2025, 7, 1, 12, 0, 0, 0, time.UTC)
	composer, _ := aggregator.NewSystemTierComposer()
	in := systemTierTickInput(t, 0)
	sysSource := aggregator.NewInMemorySystemTierInputSource([]aggregator.SystemTierInput{in})
	sysWriter := aggregator.NewInMemorySystemTierWriter()

	// One outbound edge in each family so the linked-mode
	// composer pass has something to count.
	fromRepo := in.RepoID
	toRepo := uuid.Must(uuid.NewV4())
	fromScope := in.Scopes[1].ScopeID // method scope
	toScope := uuid.Must(uuid.NewV4())

	reader := &stubLinkedReader{
		reply: func(uuid.UUID, string) (aggregator.LinkedEdges, error) {
			return aggregator.LinkedEdges{
				Applicable:          true,
				XRepoEdges:          []aggregator.XRepoEdge{{FromRepo: fromRepo, ToRepo: toRepo}},
				XRepoEdgesAvailable: true,
				CallEdges:           []aggregator.CallEdge{{FromScope: fromScope, ToScope: toScope}},
				CallEdgesAvailable:  true,
			}, nil
		},
	}

	agg, err := aggregator.NewAggregator(
		aggregator.NewInMemorySampleSource(nil),
		aggregator.NewInMemorySnapshotWriter(),
		aggregator.WithClock(fixedClock(tickAt)),
		aggregator.WithSystemTier(composer, sysSource, sysWriter),
		aggregator.WithLinkedEdgeReader(reader, slog.Default()),
	)
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	report, err := agg.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if report.LinkedEdgeReaderInvocations != 1 {
		t.Errorf("LinkedEdgeReaderInvocations = %d, want 1", report.LinkedEdgeReaderInvocations)
	}
	if report.LinkedEdgeReaderApplied != 1 {
		t.Errorf("LinkedEdgeReaderApplied = %d, want 1", report.LinkedEdgeReaderApplied)
	}
	if report.LinkedEdgeFetchFailures != 0 {
		t.Errorf("LinkedEdgeFetchFailures = %d, want 0", report.LinkedEdgeFetchFailures)
	}
	// The composer should NOT degrade rows when both
	// availability flags are true (the overlay flipped the
	// input from embedded to linked + available). Note the
	// fixture has no foundation samples for xrepo_dep_depth/
	// blast_radius, but the linked-mode flow should at least
	// produce them WITHOUT the `xrepo_edges_unavailable`
	// reason. We don't assert == 0 because other kinds (e.g.
	// authors_per_window in a 1-week window with thin author
	// data) may still degrade independently; we assert the
	// counter is STRICTLY LESS than the embedded baseline.
	embeddedBaseline := func() int {
		agg2, _ := aggregator.NewAggregator(
			aggregator.NewInMemorySampleSource(nil),
			aggregator.NewInMemorySnapshotWriter(),
			aggregator.WithClock(fixedClock(tickAt)),
			aggregator.WithSystemTier(composer,
				aggregator.NewInMemorySystemTierInputSource([]aggregator.SystemTierInput{systemTierTickInput(t, 0)}),
				aggregator.NewInMemorySystemTierWriter(),
			),
		)
		r, _ := agg2.Tick(context.Background())
		return r.SystemTierDegradedSamples
	}()
	if report.SystemTierDegradedSamples >= embeddedBaseline {
		t.Errorf("SystemTierDegradedSamples = %d; want strictly less than embedded baseline %d (linked overlay should drop xrepo/blast degradations)",
			report.SystemTierDegradedSamples, embeddedBaseline)
	}
}

// TestAggregator_LinkedReader_RemoteError_DegradesInPlace
// pins the fail-safe contract: a remote (non-ctx) reader
// error MUST NOT abort the tick. The affected input stays
// embedded, the composer degrades the row, the
// LinkedEdgeFetchFailures counter bumps so operators can
// alert on it via Prometheus.
func TestAggregator_LinkedReader_RemoteError_DegradesInPlace(t *testing.T) {
	t.Parallel()
	tickAt := time.Date(2025, 7, 1, 12, 0, 0, 0, time.UTC)
	composer, _ := aggregator.NewSystemTierComposer()
	in := systemTierTickInput(t, 0)
	sysSource := aggregator.NewInMemorySystemTierInputSource([]aggregator.SystemTierInput{in})
	sysWriter := aggregator.NewInMemorySystemTierWriter()

	remoteErr := errors.New("linked.HTTPClient: 502 from agent-memory")
	reader := &stubLinkedReader{
		reply: func(uuid.UUID, string) (aggregator.LinkedEdges, error) {
			return aggregator.LinkedEdges{}, remoteErr
		},
	}

	agg, err := aggregator.NewAggregator(
		aggregator.NewInMemorySampleSource(nil),
		aggregator.NewInMemorySnapshotWriter(),
		aggregator.WithClock(fixedClock(tickAt)),
		aggregator.WithSystemTier(composer, sysSource, sysWriter),
		aggregator.WithLinkedEdgeReader(reader, nil),
	)
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	report, err := agg.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v (want nil; remote error must NOT abort tick)", err)
	}
	if report.LinkedEdgeReaderInvocations != 1 {
		t.Errorf("LinkedEdgeReaderInvocations = %d, want 1", report.LinkedEdgeReaderInvocations)
	}
	if report.LinkedEdgeReaderApplied != 0 {
		t.Errorf("LinkedEdgeReaderApplied = %d, want 0 on remote failure", report.LinkedEdgeReaderApplied)
	}
	if report.LinkedEdgeFetchFailures != 1 {
		t.Errorf("LinkedEdgeFetchFailures = %d, want 1", report.LinkedEdgeFetchFailures)
	}
	// Composer should have run AND emitted degraded rows.
	if report.SystemTierReposComposed != 1 {
		t.Errorf("SystemTierReposComposed = %d, want 1", report.SystemTierReposComposed)
	}
	if report.SystemTierDegradedSamples == 0 {
		t.Errorf("SystemTierDegradedSamples = 0; embedded fallback should degrade xrepo/blast rows")
	}
}

// TestAggregator_LinkedReader_CtxError_AbortsTick pins the
// other half of the error split: context.Canceled /
// context.DeadlineExceeded MUST propagate to abort the
// tick. The aggregator MUST NOT swallow a cancel as a
// "degrade in place" -- the operator's cancel signal is
// authoritative.
func TestAggregator_LinkedReader_CtxError_AbortsTick(t *testing.T) {
	t.Parallel()
	tickAt := time.Date(2025, 7, 1, 12, 0, 0, 0, time.UTC)
	composer, _ := aggregator.NewSystemTierComposer()
	in := systemTierTickInput(t, 0)
	sysSource := aggregator.NewInMemorySystemTierInputSource([]aggregator.SystemTierInput{in})
	sysWriter := aggregator.NewInMemorySystemTierWriter()

	reader := &stubLinkedReader{
		reply: func(uuid.UUID, string) (aggregator.LinkedEdges, error) {
			return aggregator.LinkedEdges{}, context.Canceled
		},
	}

	agg, err := aggregator.NewAggregator(
		aggregator.NewInMemorySampleSource(nil),
		aggregator.NewInMemorySnapshotWriter(),
		aggregator.WithClock(fixedClock(tickAt)),
		aggregator.WithSystemTier(composer, sysSource, sysWriter),
		aggregator.WithLinkedEdgeReader(reader, nil),
	)
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	_, err = agg.Tick(context.Background())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Tick err = %v, want context.Canceled propagated", err)
	}
}

// TestAggregator_LinkedReader_CtxDeadlineErrorAbortsTick is
// the symmetric pin for DeadlineExceeded -- same logic, the
// adapter must not silently degrade a deadline.
func TestAggregator_LinkedReader_CtxDeadlineErrorAbortsTick(t *testing.T) {
	t.Parallel()
	tickAt := time.Date(2025, 7, 1, 12, 0, 0, 0, time.UTC)
	composer, _ := aggregator.NewSystemTierComposer()
	in := systemTierTickInput(t, 0)
	sysSource := aggregator.NewInMemorySystemTierInputSource([]aggregator.SystemTierInput{in})
	sysWriter := aggregator.NewInMemorySystemTierWriter()

	reader := &stubLinkedReader{
		reply: func(uuid.UUID, string) (aggregator.LinkedEdges, error) {
			return aggregator.LinkedEdges{}, context.DeadlineExceeded
		},
	}

	agg, err := aggregator.NewAggregator(
		aggregator.NewInMemorySampleSource(nil),
		aggregator.NewInMemorySnapshotWriter(),
		aggregator.WithClock(fixedClock(tickAt)),
		aggregator.WithSystemTier(composer, sysSource, sysWriter),
		aggregator.WithLinkedEdgeReader(reader, nil),
	)
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	_, err = agg.Tick(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Tick err = %v, want context.DeadlineExceeded propagated", err)
	}
}

// TestAggregator_LinkedReader_ModeStoreError_AbortsTick pins
// the FATAL classification for mode-store errors. When the
// reader returns an error wrapping [aggregator.ErrLinkedModeStore]
// (as production's `linked.AggregatorAdapter.ResolveLinkedEdges`
// does when `ReadRepoMode` fails), the tick MUST:
//
//  1. abort with a non-nil error,
//  2. carry BOTH the sentinel AND the raw cause on the error
//     chain (operators inspect raw cause; the aggregator
//     classifies via the sentinel), AND
//  3. NOT increment `Report.LinkedEdgeFetchFailures` (that
//     counter is reserved for class-3 agent-memory remote
//     faults; a spike must unambiguously point at agent-memory
//     uptime, not at a Postgres outage in the management plane).
//
// This is the aggregator-side half of the fatal-vs-degrade fix
// for the iter-2 evaluator finding; the linked-side wrap is
// pinned by `TestAggregatorAdapter_ModeReadErrorPropagates` in
// `internal/linked/client_test.go`.
func TestAggregator_LinkedReader_ModeStoreError_AbortsTick(t *testing.T) {
	t.Parallel()
	tickAt := time.Date(2025, 7, 1, 12, 0, 0, 0, time.UTC)
	composer, _ := aggregator.NewSystemTierComposer()
	in := systemTierTickInput(t, 0)
	sysSource := aggregator.NewInMemorySystemTierInputSource([]aggregator.SystemTierInput{in})
	sysWriter := aggregator.NewInMemorySystemTierWriter()

	rawCause := errors.New("pg: connection refused")
	// Wrap exactly the way `linked.AggregatorAdapter.ResolveLinkedEdges`
	// wraps in production (Go 1.20+ multi-`%w`): the sentinel
	// is first so classification is fast, the raw cause is
	// second so log detail is preserved.
	wrappedErr := fmt.Errorf("%w: linked.AggregatorAdapter: read repo mode: %w", aggregator.ErrLinkedModeStore, rawCause)
	reader := &stubLinkedReader{
		reply: func(uuid.UUID, string) (aggregator.LinkedEdges, error) {
			return aggregator.LinkedEdges{}, wrappedErr
		},
	}

	agg, err := aggregator.NewAggregator(
		aggregator.NewInMemorySampleSource(nil),
		aggregator.NewInMemorySnapshotWriter(),
		aggregator.WithClock(fixedClock(tickAt)),
		aggregator.WithSystemTier(composer, sysSource, sysWriter),
		aggregator.WithLinkedEdgeReader(reader, nil),
	)
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	report, err := agg.Tick(context.Background())
	if err == nil {
		t.Fatalf("Tick: nil err; want non-nil (mode-store error MUST abort tick)")
	}
	if !errors.Is(err, aggregator.ErrLinkedModeStore) {
		t.Errorf("Tick err = %v, want errors.Is(aggregator.ErrLinkedModeStore) (sentinel must remain on chain so callers can classify FATAL vs remote)", err)
	}
	if !errors.Is(err, rawCause) {
		t.Errorf("Tick err = %v, want errors.Is(rawCause) (raw cause must remain on chain for operator log detail)", err)
	}
	if report.LinkedEdgeFetchFailures != 0 {
		t.Errorf("LinkedEdgeFetchFailures = %d, want 0 (FATAL mode-store path must NOT bump the remote-fault counter; that counter is reserved for class-3 agent-memory faults so operator signal stays clean)", report.LinkedEdgeFetchFailures)
	}
}

// TestWithLinkedEdgeReader_NilPanics pins the wiring-bug
// fail-fast: the composition root MUST NOT silently no-op
// the linked-mode pass.
func TestWithLinkedEdgeReader_NilPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("WithLinkedEdgeReader(nil) did not panic")
		}
	}()
	_ = aggregator.WithLinkedEdgeReader(nil, nil)
}
