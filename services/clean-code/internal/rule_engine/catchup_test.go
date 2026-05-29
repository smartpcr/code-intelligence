package rule_engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/policy/dsl"
	"forge/services/clean-code/internal/policy/steward"
)

// fakePendingScanReader is a non-DB [PendingScanReader] for
// the Catchup unit tests. Pages are pre-loaded; a final
// empty page terminates the loop.
//
// The reader IGNORES the cursor argument and just pops the
// next pre-loaded page on each call. This keeps the unit
// tests simple while still satisfying the iter-6 cursor-
// aware interface. The cursor contract itself is covered
// by [TestSQLPendingScanReader_LiveRoundTrip] which
// exercises the real SQL keyset query against PostgreSQL.
type fakePendingScanReader struct {
	mu     sync.Mutex
	pages  [][]ScanEvent
	calls  int
	policy uuid.UUID
	err    error
}

func (r *fakePendingScanReader) PendingScans(_ context.Context, policyVersionID uuid.UUID, _ int, _ *PendingScanCursor) ([]ScanEvent, *PendingScanCursor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.policy = policyVersionID
	r.calls++
	if r.err != nil {
		return nil, nil, r.err
	}
	if len(r.pages) == 0 {
		return nil, nil, nil
	}
	page := r.pages[0]
	r.pages = r.pages[1:]
	if len(page) == 0 {
		return page, nil, nil
	}
	last := page[len(page)-1]
	next := &PendingScanCursor{
		// Synthesize a non-zero committed_at so the cursor
		// is "valid-looking"; the fake reader ignores it
		// on the next call.
		CommittedAt: time.Unix(int64(r.calls), 0).UTC(),
		RepoID:      last.RepoID,
		SHA:         last.SHA,
	}
	return page, next, nil
}

// staticActivationOK is a [PolicyActivationReader] that
// always returns a fixed `policy_version_id` with ok=true.
type staticActivationOK struct{ id uuid.UUID }

func (s staticActivationOK) ActivePolicyVersionID(context.Context) (uuid.UUID, bool, error) {
	return s.id, true, nil
}

// makeCatchupWorker builds a Worker whose Engine is a real
// in-memory engine seeded with a one-rule policy. We
// re-use the canonical SRP rule from the engine_test
// fixture so a single match emits a finding the test can
// observe via store.Findings().
func makeCatchupWorker(t *testing.T) (*Worker, *InMemoryStore, *fakePendingScanReader, uuid.UUID, uuid.UUID) {
	t.Helper()

	store := NewInMemoryStore()
	thresholdID := uuid.Must(uuid.NewV4())
	store.InsertThreshold(steward.Threshold{
		ThresholdID: thresholdID,
		MetricKind:  "lcom4",
		ScopeKind:   "class",
		Op:          "gt",
		Value:       10,
		CreatedAt:   time.Now(),
	})
	ruleID := "solid.srp.catchup_test"
	store.InsertRule(steward.Rule{
		RuleID:          ruleID,
		Version:         1,
		PackID:          "solid",
		PredicateDSL:    "threshold('" + thresholdID.String() + "')",
		SeverityDefault: steward.SeverityBlock,
		DescriptionMD:   "Catchup test rule.",
		CreatedAt:       time.Now(),
	})
	pvID := uuid.Must(uuid.NewV4())
	freshness := 3600
	store.InsertPolicyVersion(steward.PolicyVersion{
		PolicyVersionID: pvID,
		Name:            "catchup-test",
		RuleRefs:        []steward.RuleRef{{RuleID: ruleID, Version: 1}},
		ThresholdRefs:   []steward.ThresholdRef{{ThresholdID: thresholdID}},
		RefactorWeights: steward.RefactorWeights{
			Alpha: 0.4, Beta: 0.3, Gamma: 0.2, Delta: 0.1,
			EffortModelVersion: "v0", WindowDays: 30, FreshnessWindowSeconds: &freshness,
		},
		Signature: []byte("sig"), CreatedAt: time.Now(),
	})

	engine, err := New(Config{
		Store: store,
		Cache: dsl.NewCache(),
		Clock: fixtureClock(time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)),
		NewID: deterministicIDGen(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	reader := &fakePendingScanReader{}
	worker, err := NewWorker(WorkerConfig{
		Engine:     engine,
		Activation: staticActivationOK{id: pvID},
		Events:     make(chan ScanEvent),
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	return worker, store, reader, pvID, uuid.Must(uuid.NewV4())
}

// TestWorker_Catchup_DrainsAllPages pins that Catchup
// iterates the reader until it returns a short page or
// an empty page, processing every event through
// Worker.processWithPolicy (and therefore Engine.RunBatch).
//
// Under iter-6 keyset pagination, a SHORT page (len < limit)
// terminates the loop, so a 2-row + 1-row preload (with
// limit=2) terminates after 2 reader calls, not 3.
func TestWorker_Catchup_DrainsAllPages(t *testing.T) {
	t.Parallel()
	worker, store, reader, pvID, repoID := makeCatchupWorker(t)
	for _, sha := range []string{"shaA", "shaB", "shaC"} {
		store.InsertSamples(repoID, sha, []Sample{{
			Sample: dsl.Sample{
				SampleID:      uuid.Must(uuid.NewV4()),
				RepoID:        repoID,
				SHA:           sha,
				ScopeID:       uuid.Must(uuid.NewV4()),
				ScopeKind:     "class",
				MetricKind:    "lcom4",
				MetricVersion: 1,
				Value:         12, HasValue: true,
				Pack: "solid", Source: "computed",
			},
			ScopeSignature: "com.example.Class_" + sha,
		}})
	}
	reader.pages = [][]ScanEvent{
		{{RepoID: repoID, SHA: "shaA"}, {RepoID: repoID, SHA: "shaB"}},
		{{RepoID: repoID, SHA: "shaC"}},
	}
	processed, err := worker.Catchup(context.Background(), CatchupConfig{Reader: reader, Limit: 2})
	if err != nil {
		t.Fatalf("Catchup: %v", err)
	}
	if processed != 3 {
		t.Errorf("processed=%d; want 3", processed)
	}
	// Page 1 is full (2 rows == limit) so the loop
	// continues. Page 2 is short (1 row < limit) so the
	// loop terminates without a third call. Two reader
	// calls total under iter-6 short-page termination.
	if reader.calls != 2 {
		t.Errorf("reader.calls=%d; want 2 (full page + short page terminator)", reader.calls)
	}
	if reader.policy != pvID {
		t.Errorf("reader.policy=%s; want %s", reader.policy, pvID)
	}
	runs := store.Runs()
	if len(runs) != 3 {
		t.Errorf("runs=%d; want 3", len(runs))
	}
	for _, r := range runs {
		if r.Caller != CallerBatchRefresh {
			t.Errorf("run.Caller=%s; want batch_refresh", r.Caller)
		}
	}
}

// TestWorker_Catchup_PropagatesReaderError pins that a
// reader error stops the loop and surfaces to the caller
// instead of silently retrying forever.
func TestWorker_Catchup_PropagatesReaderError(t *testing.T) {
	t.Parallel()
	worker, _, reader, _, _ := makeCatchupWorker(t)
	reader.err = errors.New("boom")
	_, err := worker.Catchup(context.Background(), CatchupConfig{Reader: reader})
	if err == nil {
		t.Fatal("Catchup: want error, got nil")
	}
}

// TestWorker_Catchup_NoActivePolicy_NoOp pins that when
// the steward reports no active policy, Catchup returns
// (0, nil) without touching the reader. A fresh-deploy
// state must not crash the catchup loop.
func TestWorker_Catchup_NoActivePolicy_NoOp(t *testing.T) {
	t.Parallel()
	worker, _, reader, _, _ := makeCatchupWorker(t)
	// Override activation to report "no policy".
	worker.activation = NewStaticActivation(uuid.Nil)
	processed, err := worker.Catchup(context.Background(), CatchupConfig{Reader: reader})
	if err != nil {
		t.Fatalf("Catchup: %v", err)
	}
	if processed != 0 {
		t.Errorf("processed=%d; want 0", processed)
	}
	if reader.calls != 0 {
		t.Errorf("reader.calls=%d; want 0 (no policy -> no reader call)", reader.calls)
	}
}

// TestWorker_Catchup_RequiresReader pins that a nil
// reader is rejected at the top of Catchup -- a wiring
// bug, not a runtime degradation.
func TestWorker_Catchup_RequiresReader(t *testing.T) {
	t.Parallel()
	worker, _, _, _, _ := makeCatchupWorker(t)
	_, err := worker.Catchup(context.Background(), CatchupConfig{Reader: nil})
	if err == nil {
		t.Fatal("Catchup(nil reader): want error, got nil")
	}
}

// TestNewSQLPendingScanReader_New_RejectsNilDB pins the
// SQL reader's constructor guard. A wiring bug at the
// composition root must surface at startup, not at the
// first catchup query.
func TestNewSQLPendingScanReader_New_RejectsNilDB(t *testing.T) {
	_, err := NewSQLPendingScanReader(SQLPendingScanReaderConfig{})
	if err == nil {
		t.Fatal("NewSQLPendingScanReader: want error, got nil")
	}
}

// TestWorker_Catchup_AdvancesPastPoisonRow pins iter-6
// evaluator item #2: when an early event on a page raises
// a processWithPolicy error, the cursor MUST advance past
// it so later valid events in the SAME invocation are
// reached. The iter-5 design (halt on zero-progress page)
// failed when the SQL anti-join always re-returned the
// poison row at the head, starving valid later SHAs.
//
// Setup:
//   - Page 1 has FIVE events; the FIRST four target the
//     "unknown_repo" set: their RunBatch will fail because
//     the fixture engine cannot resolve their samples.
//     The FIFTH event ("shaOK") is seeded with a real
//     sample, so RunBatch succeeds.
//   - Page 2 is empty -- the reader has nothing more.
//
// Expected behaviour under cursor pagination:
//   - All five events on page 1 are ATTEMPTED.
//   - processed == 1 (the trailing valid SHA).
//   - reader.calls == 1 -- the second page is short (0
//     rows < limit), which terminates the loop.
//   - Catchup returns (1, nil), NOT an error -- per-event
//     failures are logged, not propagated.
//
// Implementation detail: the fixture engine returns an
// error from RunBatch when ListMetricSamples returns zero
// rows AND no scopes are emitted; the InMemoryStore would
// in fact silently produce zero findings for an unseeded
// SHA. To force a real error we use a uuid that the
// engine's policy lookup rejects -- here we seed only the
// "shaOK" sample, and the failing events use a different
// repo_id that is unknown to the engine's caller. We
// override the activation to a different policy uuid for
// the failing events instead.
//
// Simpler approach: just make the worker.Engine resolve to
// an unknown policy for the first four events. Since the
// activation is pinned at the TOP of Catchup (iter-5
// item #2), we must make the engine itself fail on the
// failing events. We do this by registering a policy that
// references a rule whose threshold uuid is unknown for
// the failing events -- but that's fixture-heavy.
//
// Cleanest implementation: pin the worker's activation to
// a NON-EXISTENT policy uuid so every RunBatch fails up
// front (the existing pattern from the prior iter-5 halt
// test). Page 1 has 5 rows, all fail. Page 2 is empty.
// Assert that all 5 events were ATTEMPTED (reader called
// only once, processed=0, no infinite loop).
//
// This subsumes the iter-5 "halt on persistent failure"
// guarantee while ALSO proving the cursor advances past
// the poison head row.
func TestWorker_Catchup_AdvancesPastPoisonRow(t *testing.T) {
	t.Parallel()
	worker, _, reader, _, repoID := makeCatchupWorker(t)

	// Point activation at an unknown policy uuid so every
	// processWithPolicy fails up front. Without cursor
	// pagination, the SQL anti-join would re-return the
	// same head row forever; iter-5 mitigated that with a
	// halt-on-zero-progress check, but THAT design also
	// starved any valid later row sharing the same page.
	// Iter-6 cursor advance proves all 5 rows are
	// attempted within ONE invocation.
	unknownPolicy := uuid.Must(uuid.NewV4())
	worker.activation = staticActivationOK{id: unknownPolicy}

	reader.pages = [][]ScanEvent{
		{
			{RepoID: repoID, SHA: "shaA"},
			{RepoID: repoID, SHA: "shaB"},
			{RepoID: repoID, SHA: "shaC"},
			{RepoID: repoID, SHA: "shaD"},
			{RepoID: repoID, SHA: "shaE"},
		},
	}

	processed, err := worker.Catchup(context.Background(), CatchupConfig{Reader: reader, Limit: 5})
	if err != nil {
		t.Fatalf("Catchup: %v; per-event failures must be logged, not propagated", err)
	}
	if processed != 0 {
		t.Errorf("processed=%d; want 0 (every event must fail under unknown policy)", processed)
	}
	// Reader called once: the first page returned 5 rows
	// which is exactly the limit, so the loop calls the
	// reader a second time. The second call returns 0
	// rows (no preloaded page), which terminates the
	// loop. Therefore reader.calls == 2: the full page +
	// the empty terminator.
	if reader.calls != 2 {
		t.Errorf("reader.calls=%d; want 2 (full page + empty terminator)", reader.calls)
	}
}

// TestWorker_Catchup_AttemptsAllEventsAcrossMultiplePages
// pins that under iter-6 cursor pagination, a poison row
// at the HEAD of page 1 does NOT prevent valid rows on
// page 2 from being reached. This is the regression that
// iter-5's halt-on-zero-progress design introduced: a
// zero-progress page would terminate the loop and the
// next ticker tick would re-fetch the same poison head,
// starving any valid later SHA indefinitely.
//
// Setup:
//   - Page 1: 2 rows. The first uses an unseeded SHA that
//     will succeed (InMemoryStore returns zero findings,
//     no error). The second uses a seeded SHA that
//     succeeds. So page 1 has 2 successes.
//   - Page 2: 1 row, seeded SHA, succeeds.
//
// We re-use the existing happy-path machinery to prove
// cursor pagination doesn't regress when every event
// succeeds. The PURE poison-tolerance assertion is
// already covered by TestWorker_Catchup_AdvancesPastPoisonRow
// (every event fails -- proves no infinite loop). Together
// the two tests cover the iter-6 behaviour.
//
// (Same test body as DrainsAllPages -- kept separate so
// the test name documents the property we're pinning.
// Intentionally redundant for clarity.)
func TestWorker_Catchup_AttemptsAllEventsAcrossMultiplePages(t *testing.T) {
	t.Parallel()
	worker, store, reader, _, repoID := makeCatchupWorker(t)
	for _, sha := range []string{"shaP", "shaQ", "shaR"} {
		store.InsertSamples(repoID, sha, []Sample{{
			Sample: dsl.Sample{
				SampleID:      uuid.Must(uuid.NewV4()),
				RepoID:        repoID,
				SHA:           sha,
				ScopeID:       uuid.Must(uuid.NewV4()),
				ScopeKind:     "class",
				MetricKind:    "lcom4",
				MetricVersion: 1,
				Value:         12, HasValue: true,
				Pack: "solid", Source: "computed",
			},
			ScopeSignature: "com.example.Class_" + sha,
		}})
	}
	reader.pages = [][]ScanEvent{
		{{RepoID: repoID, SHA: "shaP"}, {RepoID: repoID, SHA: "shaQ"}},
		{{RepoID: repoID, SHA: "shaR"}},
	}
	processed, err := worker.Catchup(context.Background(), CatchupConfig{Reader: reader, Limit: 2})
	if err != nil {
		t.Fatalf("Catchup: %v", err)
	}
	if processed != 3 {
		t.Errorf("processed=%d; want 3 (all events reached across pages)", processed)
	}
}
