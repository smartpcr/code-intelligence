package promoter

// Pure / sqlmock unit tests. No live PostgreSQL required;
// every test in this file is hermetic so a developer can run
// `go test ./internal/promoter/...` without the docker
// compose stack. Live-PG behaviour is exercised by the
// sibling service_integration_test.go file.

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/embedding"
)

// silentLogger discards every Service-emitted record so the
// test output stays clean.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// fakeEmbedder is the unit-test embedder. It records every
// call and returns a deterministic vector.
type fakeEmbedder struct {
	mu      sync.Mutex
	model   string
	vec     []float32
	err     error
	calls   []string
}

func newFakeEmbedder(model string, dim int) *fakeEmbedder {
	v := make([]float32, dim)
	for i := range v {
		// Non-zero so a recall debug doesn't mistake stub
		// output for "missing vector".
		v[i] = 1.0 / float32(dim)
	}
	return &fakeEmbedder{model: model, vec: v}
}

func (f *fakeEmbedder) Embed(_ context.Context, content string) ([]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, content)
	if f.err != nil {
		return nil, f.err
	}
	out := make([]float32, len(f.vec))
	copy(out, f.vec)
	return out, nil
}

func (f *fakeEmbedder) ModelVersion() string { return f.model }

func (f *fakeEmbedder) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeQdrant is the in-memory unit-test Qdrant. Records every
// upsert; PointExists honours the upsert log.
type fakeQdrant struct {
	mu          sync.Mutex
	upsertedIDs map[string]bool
	upsertErr   error
	confirmErr  error
	confirmMiss bool // PointExists returns (false, nil) regardless of upserts
	upsertLog   []fakeUpsertCall
}

type fakeUpsertCall struct {
	Collection string
	PointID    string
	Payload    map[string]any
}

func newFakeQdrant() *fakeQdrant {
	return &fakeQdrant{upsertedIDs: make(map[string]bool)}
}

func (f *fakeQdrant) Upsert(_ context.Context, collection, pointID string, _ []float32, payload map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.upsertedIDs[pointID] = true
	f.upsertLog = append(f.upsertLog, fakeUpsertCall{
		Collection: collection,
		PointID:    pointID,
		Payload:    payload,
	})
	return nil
}

func (f *fakeQdrant) PointExists(_ context.Context, _, pointID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.confirmErr != nil {
		return false, f.confirmErr
	}
	if f.confirmMiss {
		return false, nil
	}
	return f.upsertedIDs[pointID], nil
}

func (f *fakeQdrant) upsertCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.upsertLog)
}

// ────────────────────────────────────────────────────────────
// New: Config default substitution
// ────────────────────────────────────────────────────────────

func TestNew_appliesDefaultsForZeroOrNegativeFields(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	emb := newFakeEmbedder("test@v1", 4)
	qd := newFakeQdrant()

	svc, err := New(db, emb, qd, Config{
		ConfidenceThreshold: 0,
		SupportThreshold:    -1,
		RunInterval:         0,
		TickTimeout:         -1,
		CandidateBatchSize:  0,
		RetryBatchSize:      0,
		AdvisoryLockKey:     0,
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cfg := svc.Config()
	if cfg.ConfidenceThreshold != DefaultConfidenceThreshold {
		t.Fatalf("ConfidenceThreshold default not applied: got %v", cfg.ConfidenceThreshold)
	}
	if cfg.SupportThreshold != DefaultSupportThreshold {
		t.Fatalf("SupportThreshold default not applied: got %d", cfg.SupportThreshold)
	}
	if cfg.RunInterval != DefaultRunInterval {
		t.Fatalf("RunInterval default not applied: got %v", cfg.RunInterval)
	}
	if cfg.TickTimeout != DefaultTickTimeout {
		t.Fatalf("TickTimeout default not applied: got %v", cfg.TickTimeout)
	}
	if cfg.CandidateBatchSize != DefaultCandidateBatchSize {
		t.Fatalf("CandidateBatchSize default not applied: got %d", cfg.CandidateBatchSize)
	}
	if cfg.RetryBatchSize != DefaultRetryBatchSize {
		t.Fatalf("RetryBatchSize default not applied: got %d", cfg.RetryBatchSize)
	}
	if cfg.AdvisoryLockKey != PromoterAdvisoryLockKey {
		t.Fatalf("AdvisoryLockKey default not applied: got %x", cfg.AdvisoryLockKey)
	}
}

func TestNew_preservesNonZeroFields(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	emb := newFakeEmbedder("test@v1", 4)
	qd := newFakeQdrant()
	svc, err := New(db, emb, qd, Config{
		ConfidenceThreshold: 0.9,
		SupportThreshold:    8,
		RunInterval:         2 * time.Minute,
		TickTimeout:         9 * time.Minute,
		CandidateBatchSize:  100,
		RetryBatchSize:      25,
		AdvisoryLockKey:     0x123,
	}, silentLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cfg := svc.Config()
	if cfg.ConfidenceThreshold != 0.9 || cfg.SupportThreshold != 8 ||
		cfg.RunInterval != 2*time.Minute || cfg.TickTimeout != 9*time.Minute ||
		cfg.CandidateBatchSize != 100 || cfg.RetryBatchSize != 25 ||
		cfg.AdvisoryLockKey != 0x123 {
		t.Fatalf("non-zero fields clobbered: %+v", cfg)
	}
}

func TestNew_panicsOnNilDB(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("New(nil, ...) must panic, did not")
		}
	}()
	emb := newFakeEmbedder("test", 4)
	qd := newFakeQdrant()
	_, _ = New(nil, emb, qd, Config{}, nil)
}

func TestNew_panicsOnNilEmbedder(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("New with nil Embedder must panic, did not")
		}
	}()
	qd := newFakeQdrant()
	_, _ = New(db, nil, qd, Config{}, nil)
}

func TestNew_panicsOnNilQdrant(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("New with nil Qdrant must panic, did not")
		}
	}()
	emb := newFakeEmbedder("test", 4)
	_, _ = New(db, emb, nil, Config{}, nil)
}

// ────────────────────────────────────────────────────────────
// bandOf
// ────────────────────────────────────────────────────────────

func TestBandOf_thresholds(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want string
	}{
		{"zero is low", 0.0, "low"},
		{"just below 0.3 is low", 0.29999, "low"},
		{"0.3 boundary is medium", 0.3, "medium"},
		{"just below 0.7 is medium", 0.69999, "medium"},
		{"0.7 boundary is high", 0.7, "high"},
		{"one is high", 1.0, "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bandOf(tc.in); got != tc.want {
				t.Fatalf("bandOf(%v)=%q want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ────────────────────────────────────────────────────────────
// buildConceptContent
// ────────────────────────────────────────────────────────────

func TestBuildConceptContent(t *testing.T) {
	cases := []struct {
		name, desc, want string
	}{
		{"foo", "bar", "foo\n\nbar"},
		{"", "bar", "bar"},
		{"foo", "", "foo"},
		{"", "", "(empty concept)"},
	}
	for _, tc := range cases {
		got := buildConceptContent(tc.name, tc.desc)
		if got != tc.want {
			t.Fatalf("buildConceptContent(%q,%q)=%q want %q", tc.name, tc.desc, got, tc.want)
		}
	}
}

// ────────────────────────────────────────────────────────────
// Lifecycle-ordering sqlmock tests
// ────────────────────────────────────────────────────────────

const testLockKey int64 = 0x4FEEDDEAFC0FFEE0

func newTestSvc(t *testing.T) (*Service, sqlmock.Sqlmock, *sql.DB, *fakeEmbedder, *fakeQdrant) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(false))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	emb := newFakeEmbedder("test@v1", 4)
	qd := newFakeQdrant()
	deterministicUUID := func() (string, error) {
		return "11111111-1111-1111-1111-111111111111", nil
	}
	svc, err := New(db, emb, qd, Config{
		ConfidenceThreshold: 0.7,
		SupportThreshold:    5,
		RunInterval:         time.Second,
		TickTimeout:         5 * time.Second,
		CandidateBatchSize:  10,
		RetryBatchSize:      10,
		AdvisoryLockKey:     testLockKey,
	}, silentLogger(), WithUUIDFactory(deterministicUUID))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, mock, db, emb, qd
}

// expectEmptyOrphanScan registers a sqlmock expectation that
// the orphan-recovery scan (Service.selectOrphans, run FIRST
// in runEmissionPhase per evaluator-2 finding #1) returns
// zero rows. Used by every existing tick test that does not
// exercise the orphan-recovery path — those tests need the
// orphan scan to be a no-op so the retry + forward phases
// behave identically to iter-1.
//
// The match pattern (`FROM concept_version cv\s+JOIN concept c`)
// is unique to selectOrphans; selectStalled has
// `FROM embedding_publish ep` and selectCandidates uses a
// `WITH latest AS (...) ... FROM latest` CTE shape, so the
// regex does not collide.
func expectEmptyOrphanScan(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`FROM concept_version cv\s+JOIN concept c`).
		WillReturnRows(sqlmock.NewRows([]string{
			"concept_version_id", "concept_id",
			"name", "description_md", "fingerprint",
		}))
}

// Tick on a fresh cluster where pg_try_advisory_lock returns
// false MUST:
//   - open promoter_run with status='running'
//   - acquire-attempt advisory lock (returns false)
//   - finalize the run row with status='lock_skipped' and 0
//     concepts_promoted
//   - emit no embedder calls, no Qdrant upserts.
func TestTick_lockSkippedFinalisesAsLockSkipped(t *testing.T) {
	svc, mock, db, emb, qd := newTestSvc(t)
	defer db.Close()

	// Step 1: open run.
	mock.ExpectQuery(`INSERT INTO promoter_run`).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).
			AddRow("00000000-0000-0000-0000-000000000001"))

	// Step 2: pin conn + try lock -> false.
	mock.ExpectQuery(`pg_try_advisory_lock`).
		WithArgs(testLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(false))

	// Step 6: finalize as lock_skipped (concepts_promoted=0).
	mock.ExpectExec(`UPDATE promoter_run`).
		WithArgs(0, "lock_skipped", "00000000-0000-0000-0000-000000000001").
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := svc.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !res.LockSkipped {
		t.Fatalf("expected LockSkipped=true; got %+v", res)
	}
	if res.ConceptsPromoted != 0 {
		t.Fatalf("lock-skipped tick must report 0 promoted; got %d", res.ConceptsPromoted)
	}
	if emb.callCount() != 0 {
		t.Fatalf("lock-skipped tick must not call embedder; calls=%d", emb.callCount())
	}
	if qd.upsertCount() != 0 {
		t.Fatalf("lock-skipped tick must not upsert; got %d", qd.upsertCount())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// When openRun fails, no advisory-lock acquisition is
// attempted and the deferred finalize is NOT issued (because
// no run_id exists to finalize against).
func TestTick_openRunFailureReturnsErrorBeforeAdvisoryLock(t *testing.T) {
	svc, mock, db, _, _ := newTestSvc(t)
	defer db.Close()

	mock.ExpectQuery(`INSERT INTO promoter_run`).
		WillReturnError(errors.New("simulated PG outage"))

	_, err := svc.Tick(context.Background())
	if err == nil {
		t.Fatalf("expected Tick to return error on openRun failure")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// When the candidate scan finds NOTHING and the retry scan
// finds NOTHING, Tick still runs the full lifecycle (open ->
// lock -> retry scan -> forward scan -> unlock -> finalize),
// reporting zero promotions.
func TestTick_noCandidatesFinalisesAsDoneWithZeroPromoted(t *testing.T) {
	svc, mock, db, emb, qd := newTestSvc(t)
	defer db.Close()

	mock.ExpectQuery(`INSERT INTO promoter_run`).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).
			AddRow("00000000-0000-0000-0000-000000000002"))

	mock.ExpectQuery(`pg_try_advisory_lock`).
		WithArgs(testLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))

	// Orphan-recovery scan: 0 rows (evaluator-2 finding #1).
	expectEmptyOrphanScan(mock)

	// Retry-phase scan: 0 rows.
	mock.ExpectQuery(`FROM embedding_publish ep`).
		WillReturnRows(sqlmock.NewRows([]string{
			"publish_id", "concept_version_id", "concept_id",
			"qdrant_point_id", "embedding_model_version",
			"name", "description_md", "fingerprint",
			"event_kind", "max_attempt",
		}))

	// Forward-phase scan: 0 rows.
	mock.ExpectQuery(`FROM latest`).
		WillReturnRows(sqlmock.NewRows([]string{
			"concept_id", "name", "description_md", "fingerprint",
			"version_index", "confidence", "support_count", "negative_count",
		}))

	// pg_advisory_unlock (during defer; uses Background ctx).
	mock.ExpectExec(`pg_advisory_unlock`).
		WithArgs(testLockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectExec(`UPDATE promoter_run`).
		WithArgs(0, "done", "00000000-0000-0000-0000-000000000002").
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := svc.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.LockSkipped {
		t.Fatalf("did not expect LockSkipped: %+v", res)
	}
	if res.CandidatesEvaluated != 0 || res.ConceptsPromoted != 0 {
		t.Fatalf("expected zero promotions; got %+v", res)
	}
	if emb.callCount() != 0 {
		t.Fatalf("no candidates means no embedder calls; got %d", emb.callCount())
	}
	if qd.upsertCount() != 0 {
		t.Fatalf("no candidates means no upserts; got %d", qd.upsertCount())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// When the forward-phase recheck inside the per-Concept tx
// finds a concurrent promoter already promoted this Concept,
// the candidate is soft-dropped (recheck returned
// already_promoted=true). The lifecycle still completes
// with status='done' and 0 concepts_promoted.
func TestTick_recheckDropsAlreadyPromotedCandidate(t *testing.T) {
	svc, mock, db, emb, qd := newTestSvc(t)
	defer db.Close()

	mock.ExpectQuery(`INSERT INTO promoter_run`).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).
			AddRow("00000000-0000-0000-0000-000000000003"))

	mock.ExpectQuery(`pg_try_advisory_lock`).
		WithArgs(testLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))

	// Orphan-recovery scan: 0 rows (evaluator-2 finding #1).
	expectEmptyOrphanScan(mock)

	// Retry-phase scan: 0 rows.
	mock.ExpectQuery(`FROM embedding_publish ep`).
		WillReturnRows(sqlmock.NewRows([]string{
			"publish_id", "concept_version_id", "concept_id",
			"qdrant_point_id", "embedding_model_version",
			"name", "description_md", "fingerprint",
			"event_kind", "max_attempt",
		}))

	// Forward-phase scan: 1 candidate.
	mock.ExpectQuery(`FROM latest`).
		WillReturnRows(sqlmock.NewRows([]string{
			"concept_id", "name", "description_md", "fingerprint",
			"version_index", "confidence", "support_count", "negative_count",
		}).AddRow(
			"22222222-2222-2222-2222-222222222222", "concept-name", "concept-desc",
			[]byte("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"),
			3, 0.85, 7, 0,
		))

	// Per-Concept tx1: BEGIN + SELECT FOR UPDATE + recheck
	// returns already_promoted=true + ROLLBACK.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM concept WHERE concept_id`).
		WithArgs("22222222-2222-2222-2222-222222222222").
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	mock.ExpectQuery(`SELECT cv.version_index,`).
		WithArgs("22222222-2222-2222-2222-222222222222").
		WillReturnRows(sqlmock.NewRows([]string{
			"version_index", "confidence", "support_count", "negative_count", "already_promoted",
		}).AddRow(3, 0.85, 7, 0, true))
	mock.ExpectRollback()

	mock.ExpectExec(`pg_advisory_unlock`).
		WithArgs(testLockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectExec(`UPDATE promoter_run`).
		WithArgs(0, "done", "00000000-0000-0000-0000-000000000003").
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := svc.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.CandidatesPending != 1 {
		t.Fatalf("CandidatesPending should be 1; got %d", res.CandidatesPending)
	}
	if res.CandidatesEvaluated != 0 {
		t.Fatalf("recheck-dropped candidate must NOT count as evaluated; got %d", res.CandidatesEvaluated)
	}
	if res.ConceptsPromoted != 0 {
		t.Fatalf("recheck-dropped candidate must NOT count as promoted; got %d", res.ConceptsPromoted)
	}
	if emb.callCount() != 0 {
		t.Fatalf("recheck-dropped candidate must not call embedder; got %d", emb.callCount())
	}
	if qd.upsertCount() != 0 {
		t.Fatalf("recheck-dropped candidate must not upsert; got %d", qd.upsertCount())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// When the forward-phase recheck finds the threshold no
// longer crossed (e.g. a fresh Consolidator ConceptVersion
// landed between the scan and the lock and brought
// support_count back under the floor), the candidate is
// soft-dropped. The lifecycle completes with zero promotions.
func TestTick_recheckDropsCandidateBelowThreshold(t *testing.T) {
	svc, mock, db, emb, qd := newTestSvc(t)
	defer db.Close()

	mock.ExpectQuery(`INSERT INTO promoter_run`).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).
			AddRow("00000000-0000-0000-0000-000000000004"))
	mock.ExpectQuery(`pg_try_advisory_lock`).
		WithArgs(testLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	// Orphan-recovery scan: 0 rows (evaluator-2 finding #1).
	expectEmptyOrphanScan(mock)
	mock.ExpectQuery(`FROM embedding_publish ep`).
		WillReturnRows(sqlmock.NewRows([]string{
			"publish_id", "concept_version_id", "concept_id",
			"qdrant_point_id", "embedding_model_version",
			"name", "description_md", "fingerprint",
			"event_kind", "max_attempt",
		}))
	mock.ExpectQuery(`FROM latest`).
		WillReturnRows(sqlmock.NewRows([]string{
			"concept_id", "name", "description_md", "fingerprint",
			"version_index", "confidence", "support_count", "negative_count",
		}).AddRow(
			"22222222-2222-2222-2222-222222222222", "concept-name", "concept-desc",
			[]byte("00"), 3, 0.85, 7, 0,
		))

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM concept WHERE concept_id`).
		WithArgs("22222222-2222-2222-2222-222222222222").
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	// Recheck returns support_count=2 (below floor of 5) and
	// already_promoted=false.
	mock.ExpectQuery(`SELECT cv.version_index,`).
		WithArgs("22222222-2222-2222-2222-222222222222").
		WillReturnRows(sqlmock.NewRows([]string{
			"version_index", "confidence", "support_count", "negative_count", "already_promoted",
		}).AddRow(4, 0.85, 2, 0, false))
	mock.ExpectRollback()

	mock.ExpectExec(`pg_advisory_unlock`).
		WithArgs(testLockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE promoter_run`).
		WithArgs(0, "done", "00000000-0000-0000-0000-000000000004").
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := svc.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.ConceptsPromoted != 0 {
		t.Fatalf("below-threshold recheck must not promote; got %d", res.ConceptsPromoted)
	}
	if emb.callCount() != 0 {
		t.Fatalf("below-threshold recheck must not call embedder; got %d", emb.callCount())
	}
	if qd.upsertCount() != 0 {
		t.Fatalf("below-threshold recheck must not upsert; got %d", qd.upsertCount())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// Happy path: 1 candidate flows through tx1 (INSERT CV) +
// tx2 (INSERT EP + queued event) + the runAttempt chain
// (vector_written + published events). Lifecycle finalises
// with concepts_promoted=1.
//
// This is the core §8.7.1 lines 818-833 write-protocol
// ordering test:
//   - PromoterRun row exists BEFORE ConceptVersion INSERT
//     (FK in spirit: producer_run_id = run_id).
//   - ConceptVersion INSERT happens BEFORE EmbeddingPublish
//     INSERT (FK direction: embedding_publish.concept_version_id
//     references concept_version.concept_version_id).
//   - Event chain advances queued → vector_written → published.
//   - Qdrant upsert + PointExists both happen, in that order.
func TestTick_happyPathPromotesOneCandidate(t *testing.T) {
	svc, mock, db, emb, qd := newTestSvc(t)
	defer db.Close()

	mock.ExpectQuery(`INSERT INTO promoter_run`).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).
			AddRow("00000000-0000-0000-0000-000000000005"))
	mock.ExpectQuery(`pg_try_advisory_lock`).
		WithArgs(testLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	// Orphan-recovery scan: 0 rows (evaluator-2 finding #1).
	expectEmptyOrphanScan(mock)
	mock.ExpectQuery(`FROM embedding_publish ep`).
		WillReturnRows(sqlmock.NewRows([]string{
			"publish_id", "concept_version_id", "concept_id",
			"qdrant_point_id", "embedding_model_version",
			"name", "description_md", "fingerprint",
			"event_kind", "max_attempt",
		}))
	mock.ExpectQuery(`FROM latest`).
		WillReturnRows(sqlmock.NewRows([]string{
			"concept_id", "name", "description_md", "fingerprint",
			"version_index", "confidence", "support_count", "negative_count",
		}).AddRow(
			"22222222-2222-2222-2222-222222222222", "concept-name", "concept-desc",
			[]byte{0x01, 0x02, 0x03}, 3, 0.85, 7, 0,
		))

	// tx1: BEGIN + lock + recheck + INSERT CV + COMMIT.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM concept WHERE concept_id`).
		WithArgs("22222222-2222-2222-2222-222222222222").
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	mock.ExpectQuery(`SELECT cv.version_index,`).
		WithArgs("22222222-2222-2222-2222-222222222222").
		WillReturnRows(sqlmock.NewRows([]string{
			"version_index", "confidence", "support_count", "negative_count", "already_promoted",
		}).AddRow(3, 0.85, 7, 0, false))
	mock.ExpectQuery(`INSERT INTO concept_version`).
		WithArgs("22222222-2222-2222-2222-222222222222", 4, 0.85, "high",
			7, 0, "00000000-0000-0000-0000-000000000005").
		WillReturnRows(sqlmock.NewRows([]string{"concept_version_id"}).
			AddRow("33333333-3333-3333-3333-333333333333"))
	mock.ExpectCommit()

	// tx2: BEGIN + INSERT EP + INSERT queued + COMMIT.
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO embedding_publish`).
		WithArgs("33333333-3333-3333-3333-333333333333", "test@v1",
			"11111111-1111-1111-1111-111111111111").
		WillReturnRows(sqlmock.NewRows([]string{"publish_id"}).
			AddRow("44444444-4444-4444-4444-444444444444"))
	mock.ExpectExec(`INSERT INTO embedding_publish_event`).
		WithArgs("44444444-4444-4444-4444-444444444444", "queued", 0, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// runAttempt: vector_written + published events on the
	// pool conn (not the pinned conn).
	mock.ExpectExec(`INSERT INTO embedding_publish_event`).
		WithArgs("44444444-4444-4444-4444-444444444444", "vector_written", 0, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Iter-2 fix #3: published-event commit is now wrapped
	// in a tx by `commitConceptPublishedWithSupersede` —
	// BEGIN, probe queued event for supersedes_publish_id
	// (none here → empty), INSERT published, COMMIT.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT coalesce\(details_json ->> 'supersedes_publish_id', ''\)`).
		WithArgs("44444444-4444-4444-4444-444444444444").
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(""))
	mock.ExpectExec(`INSERT INTO embedding_publish_event`).
		WithArgs("44444444-4444-4444-4444-444444444444", 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	mock.ExpectExec(`pg_advisory_unlock`).
		WithArgs(testLockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE promoter_run`).
		WithArgs(1, "done", "00000000-0000-0000-0000-000000000005").
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := svc.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.ConceptsPromoted != 1 {
		t.Fatalf("expected 1 promoted; got %d", res.ConceptsPromoted)
	}
	if emb.callCount() != 1 {
		t.Fatalf("expected exactly 1 embedder call; got %d", emb.callCount())
	}
	if qd.upsertCount() != 1 {
		t.Fatalf("expected exactly 1 Qdrant upsert; got %d", qd.upsertCount())
	}
	// Validate Qdrant payload provenance.
	call := qd.upsertLog[0]
	if call.Collection != embedding.CollectionConcept {
		t.Fatalf("expected collection %q; got %q", embedding.CollectionConcept, call.Collection)
	}
	if call.PointID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("expected deterministic point_id; got %q", call.PointID)
	}
	if call.Payload["concept_id"] != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("expected concept_id in payload; got %v", call.Payload["concept_id"])
	}
	if call.Payload["concept_version_id"] != "33333333-3333-3333-3333-333333333333" {
		t.Fatalf("expected concept_version_id in payload; got %v", call.Payload["concept_version_id"])
	}
	if call.Payload["publish_id"] != "44444444-4444-4444-4444-444444444444" {
		t.Fatalf("expected publish_id in payload; got %v", call.Payload["publish_id"])
	}
	if call.Payload["embedding_model_version"] != "test@v1" {
		t.Fatalf("expected embedding_model_version in payload; got %v", call.Payload["embedding_model_version"])
	}
	if call.Payload["kind"] != "concept" {
		t.Fatalf("expected kind='concept' in payload; got %v", call.Payload["kind"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// Embedder failure on the happy path MUST insert a 'failed'
// event, leave no vector_written, and finalize the run with
// concepts_promoted=0 (the candidate was evaluated but the
// publish chain did not reach 'published').
func TestTick_embedderFailureRecordsFailedEvent(t *testing.T) {
	svc, mock, db, emb, qd := newTestSvc(t)
	defer db.Close()
	emb.err = errors.New("embedder boom")

	mock.ExpectQuery(`INSERT INTO promoter_run`).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).
			AddRow("00000000-0000-0000-0000-000000000006"))
	mock.ExpectQuery(`pg_try_advisory_lock`).
		WithArgs(testLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	// Orphan-recovery scan: 0 rows (evaluator-2 finding #1).
	expectEmptyOrphanScan(mock)
	mock.ExpectQuery(`FROM embedding_publish ep`).
		WillReturnRows(sqlmock.NewRows([]string{
			"publish_id", "concept_version_id", "concept_id",
			"qdrant_point_id", "embedding_model_version",
			"name", "description_md", "fingerprint",
			"event_kind", "max_attempt",
		}))
	mock.ExpectQuery(`FROM latest`).
		WillReturnRows(sqlmock.NewRows([]string{
			"concept_id", "name", "description_md", "fingerprint",
			"version_index", "confidence", "support_count", "negative_count",
		}).AddRow(
			"22222222-2222-2222-2222-222222222222", "n", "d",
			[]byte{0xaa, 0xbb}, 1, 0.85, 7, 0,
		))

	// tx1.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM concept WHERE concept_id`).
		WithArgs("22222222-2222-2222-2222-222222222222").
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	mock.ExpectQuery(`SELECT cv.version_index,`).
		WithArgs("22222222-2222-2222-2222-222222222222").
		WillReturnRows(sqlmock.NewRows([]string{
			"version_index", "confidence", "support_count", "negative_count", "already_promoted",
		}).AddRow(1, 0.85, 7, 0, false))
	mock.ExpectQuery(`INSERT INTO concept_version`).
		WillReturnRows(sqlmock.NewRows([]string{"concept_version_id"}).
			AddRow("33333333-3333-3333-3333-333333333333"))
	mock.ExpectCommit()

	// tx2.
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO embedding_publish`).
		WillReturnRows(sqlmock.NewRows([]string{"publish_id"}).
			AddRow("44444444-4444-4444-4444-444444444444"))
	mock.ExpectExec(`INSERT INTO embedding_publish_event`).
		WithArgs("44444444-4444-4444-4444-444444444444", "queued", 0, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// runAttempt: embedder fails → failed event (NOT
	// vector_written, NOT published).
	mock.ExpectExec(`INSERT INTO embedding_publish_event`).
		WithArgs("44444444-4444-4444-4444-444444444444", "failed", 0, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectExec(`pg_advisory_unlock`).
		WithArgs(testLockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// concepts_promoted=0 (chain did not reach published).
	mock.ExpectExec(`UPDATE promoter_run`).
		WithArgs(0, "done", "00000000-0000-0000-0000-000000000006").
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := svc.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.ConceptsPromoted != 0 {
		t.Fatalf("embedder-failed candidate must NOT count as promoted; got %d", res.ConceptsPromoted)
	}
	if res.PublishFailures != 1 {
		t.Fatalf("expected 1 publish failure; got %d", res.PublishFailures)
	}
	if qd.upsertCount() != 0 {
		t.Fatalf("embedder failure means no upsert; got %d", qd.upsertCount())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// Retry-phase scan returns 1 stalled publish whose latest
// event is 'queued'. The retry path appends a fresh
// 'queued' at attempt_index=1, then re-runs the chain.
// On the happy path the run finalises with concepts_promoted=1.
func TestTick_retryPhaseResumesStalledPublish(t *testing.T) {
	svc, mock, db, emb, qd := newTestSvc(t)
	defer db.Close()

	mock.ExpectQuery(`INSERT INTO promoter_run`).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).
			AddRow("00000000-0000-0000-0000-000000000007"))
	mock.ExpectQuery(`pg_try_advisory_lock`).
		WithArgs(testLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))

	// Orphan-recovery scan: 0 rows (evaluator-2 finding #1).
	expectEmptyOrphanScan(mock)

	// Retry-phase scan returns 1 stalled publish at
	// attempt_index 0 with latest event 'queued'.
	mock.ExpectQuery(`FROM embedding_publish ep`).
		WillReturnRows(sqlmock.NewRows([]string{
			"publish_id", "concept_version_id", "concept_id",
			"qdrant_point_id", "embedding_model_version",
			"name", "description_md", "fingerprint",
			"event_kind", "max_attempt",
		}).AddRow(
			"55555555-5555-5555-5555-555555555555",
			"66666666-6666-6666-6666-666666666666",
			"22222222-2222-2222-2222-222222222222",
			"77777777-7777-7777-7777-777777777777",
			"test@v1",
			"stalled-name", "stalled-desc",
			[]byte{0xcc, 0xdd}, "queued", 0,
		))

	// Retry: insert fresh queued event at attempt 1.
	mock.ExpectExec(`INSERT INTO embedding_publish_event`).
		WithArgs("55555555-5555-5555-5555-555555555555", "queued", 1, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// runAttempt: vector_written + published at attempt 1.
	mock.ExpectExec(`INSERT INTO embedding_publish_event`).
		WithArgs("55555555-5555-5555-5555-555555555555", "vector_written", 1, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Iter-2 fix #3: tx-wrapped published-event commit.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT coalesce\(details_json ->> 'supersedes_publish_id', ''\)`).
		WithArgs("55555555-5555-5555-5555-555555555555").
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(""))
	mock.ExpectExec(`INSERT INTO embedding_publish_event`).
		WithArgs("55555555-5555-5555-5555-555555555555", 1).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// Forward-phase scan: 0 fresh candidates (already
	// handled in the retry).
	mock.ExpectQuery(`FROM latest`).
		WillReturnRows(sqlmock.NewRows([]string{
			"concept_id", "name", "description_md", "fingerprint",
			"version_index", "confidence", "support_count", "negative_count",
		}))

	mock.ExpectExec(`pg_advisory_unlock`).
		WithArgs(testLockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE promoter_run`).
		WithArgs(1, "done", "00000000-0000-0000-0000-000000000007").
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := svc.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.RetriesAttempted != 1 {
		t.Fatalf("expected 1 retry attempted; got %d", res.RetriesAttempted)
	}
	if res.ConceptsPromoted != 1 {
		t.Fatalf("retry-published should count as promoted; got %d", res.ConceptsPromoted)
	}
	if emb.callCount() != 1 {
		t.Fatalf("expected 1 embedder call on retry; got %d", emb.callCount())
	}
	if qd.upsertCount() != 1 {
		t.Fatalf("expected 1 Qdrant upsert on retry; got %d", qd.upsertCount())
	}
	if qd.upsertLog[0].PointID != "77777777-7777-7777-7777-777777777777" {
		t.Fatalf("retry must reuse original point_id; got %q", qd.upsertLog[0].PointID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// Retry-phase model mismatch: the stalled publish recorded
// model 'old@v1' but the current embedder reports 'new@v2'.
// The Promoter MUST NOT append a queued event under the new
// model (the supersede flow owns this transition).
func TestTick_retryPhaseSkipsOnModelMismatch(t *testing.T) {
	svc, mock, db, emb, qd := newTestSvc(t)
	defer db.Close()

	mock.ExpectQuery(`INSERT INTO promoter_run`).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).
			AddRow("00000000-0000-0000-0000-000000000008"))
	mock.ExpectQuery(`pg_try_advisory_lock`).
		WithArgs(testLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))

	// Orphan-recovery scan: 0 rows (evaluator-2 finding #1).
	expectEmptyOrphanScan(mock)

	// Stalled publish recorded model 'old@v1'; current
	// embedder reports 'test@v1' (mismatch).
	mock.ExpectQuery(`FROM embedding_publish ep`).
		WillReturnRows(sqlmock.NewRows([]string{
			"publish_id", "concept_version_id", "concept_id",
			"qdrant_point_id", "embedding_model_version",
			"name", "description_md", "fingerprint",
			"event_kind", "max_attempt",
		}).AddRow(
			"55555555-5555-5555-5555-555555555555",
			"66666666-6666-6666-6666-666666666666",
			"22222222-2222-2222-2222-222222222222",
			"77777777-7777-7777-7777-777777777777",
			"old@v1", // <-- mismatch
			"stalled-name", "stalled-desc",
			[]byte{0xcc}, "queued", 0,
		))

	// NO retry events expected.
	// Forward scan: empty.
	mock.ExpectQuery(`FROM latest`).
		WillReturnRows(sqlmock.NewRows([]string{
			"concept_id", "name", "description_md", "fingerprint",
			"version_index", "confidence", "support_count", "negative_count",
		}))

	mock.ExpectExec(`pg_advisory_unlock`).
		WithArgs(testLockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE promoter_run`).
		WithArgs(0, "done", "00000000-0000-0000-0000-000000000008").
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := svc.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.RetriesAttempted != 0 {
		t.Fatalf("model mismatch must NOT count as retry; got %d", res.RetriesAttempted)
	}
	if res.ConceptsPromoted != 0 {
		t.Fatalf("model-mismatch retry must not promote; got %d", res.ConceptsPromoted)
	}
	if emb.callCount() != 0 {
		t.Fatalf("model-mismatch retry must not call embedder; got %d", emb.callCount())
	}
	if qd.upsertCount() != 0 {
		t.Fatalf("model-mismatch retry must not upsert; got %d", qd.upsertCount())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// ────────────────────────────────────────────────────────────
// Evaluator-2 finding #1 — orphan-recovery phase
// ────────────────────────────────────────────────────────────

// TestTick_orphanRecoveryDrivesOrphanedCVToPublished asserts
// that an orphaned promoted ConceptVersion (tx1 committed in
// a prior tick but its sibling embedding_publish never
// landed) is re-driven through tx2 + the §9.6a publish chain
// on the very next tick and reaches the terminal `published`
// event. Prior to evaluator-2 finding #1's fix the row was
// invisible to BOTH selectStalled AND selectCandidates and
// stayed orphaned forever.
func TestTick_orphanRecoveryDrivesOrphanedCVToPublished(t *testing.T) {
	svc, mock, db, emb, qd := newTestSvc(t)
	defer db.Close()

	mock.ExpectQuery(`INSERT INTO promoter_run`).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).
			AddRow("00000000-0000-0000-0000-000000000009"))
	mock.ExpectQuery(`pg_try_advisory_lock`).
		WithArgs(testLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))

	// Orphan-recovery scan returns 1 orphan: a
	// promoted=true, producer='promoter' CV with no
	// embedding_publish sibling row.
	mock.ExpectQuery(`FROM concept_version cv\s+JOIN concept c`).
		WillReturnRows(sqlmock.NewRows([]string{
			"concept_version_id", "concept_id",
			"name", "description_md", "fingerprint",
		}).AddRow(
			"99999999-9999-9999-9999-999999999999",
			"22222222-2222-2222-2222-222222222222",
			"orphan-name", "orphan-desc",
			[]byte{0xab, 0xcd},
		))

	// processOrphans calls insertPublishAndQueued → tx2.
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO embedding_publish`).
		WithArgs("99999999-9999-9999-9999-999999999999", "test@v1", "11111111-1111-1111-1111-111111111111").
		WillReturnRows(sqlmock.NewRows([]string{"publish_id"}).
			AddRow("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"))
	mock.ExpectExec(`INSERT INTO embedding_publish_event`).
		WithArgs("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "queued", 0, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// runAttempt: vector_written + published events.
	mock.ExpectExec(`INSERT INTO embedding_publish_event`).
		WithArgs("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "vector_written", 0, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Iter-2 fix #3: tx-wrapped published-event commit.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT coalesce\(details_json ->> 'supersedes_publish_id', ''\)`).
		WithArgs("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa").
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(""))
	mock.ExpectExec(`INSERT INTO embedding_publish_event`).
		WithArgs("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// Retry-phase scan AFTER orphan recovery: 0 rows.
	mock.ExpectQuery(`FROM embedding_publish ep`).
		WillReturnRows(sqlmock.NewRows([]string{
			"publish_id", "concept_version_id", "concept_id",
			"qdrant_point_id", "embedding_model_version",
			"name", "description_md", "fingerprint",
			"event_kind", "max_attempt",
		}))
	// Forward-phase scan: 0 rows (the orphan's parent
	// concept is already promoted, so it would be filtered
	// out by the NOT EXISTS check anyway).
	mock.ExpectQuery(`FROM latest`).
		WillReturnRows(sqlmock.NewRows([]string{
			"concept_id", "name", "description_md", "fingerprint",
			"version_index", "confidence", "support_count", "negative_count",
		}))

	mock.ExpectExec(`pg_advisory_unlock`).
		WithArgs(testLockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE promoter_run`).
		WithArgs(1, "done", "00000000-0000-0000-0000-000000000009").
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := svc.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.OrphansPending != 1 {
		t.Fatalf("OrphansPending: got %d want 1", res.OrphansPending)
	}
	if res.OrphansRecovered != 1 {
		t.Fatalf("OrphansRecovered: got %d want 1 (the §8.7.1 chain must complete on orphan recovery)",
			res.OrphansRecovered)
	}
	if res.ConceptsPromoted != 1 {
		t.Fatalf("orphan-recovered chain must count toward ConceptsPromoted; got %d", res.ConceptsPromoted)
	}
	if emb.callCount() != 1 {
		t.Fatalf("expected 1 embedder call for the orphan; got %d", emb.callCount())
	}
	if qd.upsertCount() != 1 {
		t.Fatalf("expected 1 Qdrant upsert for the orphan; got %d", qd.upsertCount())
	}
	// Payload must identify the orphan's CV so a recall
	// reader can dereference back to PostgreSQL without a
	// second join.
	if got, want := qd.upsertLog[0].Payload["concept_version_id"], "99999999-9999-9999-9999-999999999999"; got != want {
		t.Fatalf("payload.concept_version_id: got %v want %s", got, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestTick_orphanRecoveryLeavesOrphanWhenTx2RetryFails asserts
// that when an orphan's tx2 retry ALSO fails, the orphan is
// left in place (no spurious failed-event row written —
// failed events require a publish_id, which we don't yet
// have) and the next tick will re-attempt it. Confirms the
// "orphan stays an orphan until tx2 succeeds" invariant
// from evaluator-2 finding #1.
func TestTick_orphanRecoveryLeavesOrphanWhenTx2RetryFails(t *testing.T) {
	svc, mock, db, _, qd := newTestSvc(t)
	defer db.Close()

	mock.ExpectQuery(`INSERT INTO promoter_run`).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).
			AddRow("00000000-0000-0000-0000-00000000000a"))
	mock.ExpectQuery(`pg_try_advisory_lock`).
		WithArgs(testLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))

	mock.ExpectQuery(`FROM concept_version cv\s+JOIN concept c`).
		WillReturnRows(sqlmock.NewRows([]string{
			"concept_version_id", "concept_id",
			"name", "description_md", "fingerprint",
		}).AddRow(
			"99999999-9999-9999-9999-999999999999",
			"22222222-2222-2222-2222-222222222222",
			"orphan-name", "orphan-desc",
			[]byte{0xab, 0xcd},
		))

	// processOrphans calls insertPublishAndQueued → tx2,
	// which fails on the INSERT (simulated DB transient).
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO embedding_publish`).
		WillReturnError(errors.New("simulated tx2 outage"))
	mock.ExpectRollback()

	// No vector_written / published events — runAttempt
	// is not invoked because insertPublishAndQueued errored.

	mock.ExpectQuery(`FROM embedding_publish ep`).
		WillReturnRows(sqlmock.NewRows([]string{
			"publish_id", "concept_version_id", "concept_id",
			"qdrant_point_id", "embedding_model_version",
			"name", "description_md", "fingerprint",
			"event_kind", "max_attempt",
		}))
	mock.ExpectQuery(`FROM latest`).
		WillReturnRows(sqlmock.NewRows([]string{
			"concept_id", "name", "description_md", "fingerprint",
			"version_index", "confidence", "support_count", "negative_count",
		}))

	mock.ExpectExec(`pg_advisory_unlock`).
		WithArgs(testLockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE promoter_run`).
		WithArgs(0, "done", "00000000-0000-0000-0000-00000000000a").
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := svc.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.OrphansPending != 1 {
		t.Fatalf("OrphansPending: got %d want 1", res.OrphansPending)
	}
	if res.OrphansRecovered != 0 {
		t.Fatalf("OrphansRecovered: got %d want 0 (tx2 failed)", res.OrphansRecovered)
	}
	if res.ConceptsPromoted != 0 {
		t.Fatalf("ConceptsPromoted: got %d want 0 (no chain finished)", res.ConceptsPromoted)
	}
	if qd.upsertCount() != 0 {
		t.Fatalf("expected 0 Qdrant upserts when tx2 fails; got %d", qd.upsertCount())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// ────────────────────────────────────────────────────────────
// Evaluator-2 finding #2 — latest-event ordering tiebreaker
// ────────────────────────────────────────────────────────────

// TestSelectStalled_latestEventTieBreakerInSQL pins the
// canonical `(created_at DESC, event_id DESC)` tie-break that
// selectStalled's lateral subquery is required to emit. The
// §9.6a contract + doc.go:95 + the mirror queries in
// internal/embedding/flusher.go:656 and
// internal/embedding/publish_event_resolver.go:117 all use the
// same ordering. Without the event_id tie-break, two events
// that share a microsecond timestamp (cheap to create when a
// single tick writes vector_written + published back-to-back)
// could be returned non-deterministically, letting a
// 'vector_written' row look "latest" when 'published' was
// actually appended at the same microsecond — re-queueing a
// finished chain.
//
// sqlmock's default regex matcher REQUIRES the literal clause
// in the executed query for this expectation to match; a
// regression to `ORDER BY created_at DESC` only would NOT
// satisfy the regex below and the test would fail with the
// "could not match actual sql" error.
func TestSelectStalled_latestEventTieBreakerInSQL(t *testing.T) {
	svc, mock, db, _, _ := newTestSvc(t)
	defer db.Close()

	mock.ExpectQuery(`INSERT INTO promoter_run`).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).
			AddRow("00000000-0000-0000-0000-00000000000b"))
	mock.ExpectQuery(`pg_try_advisory_lock`).
		WithArgs(testLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	expectEmptyOrphanScan(mock)

	mock.ExpectQuery(`ORDER BY epe\.created_at DESC, epe\.event_id DESC`).
		WillReturnRows(sqlmock.NewRows([]string{
			"publish_id", "concept_version_id", "concept_id",
			"qdrant_point_id", "embedding_model_version",
			"name", "description_md", "fingerprint",
			"event_kind", "max_attempt",
		}))
	mock.ExpectQuery(`FROM latest`).
		WillReturnRows(sqlmock.NewRows([]string{
			"concept_id", "name", "description_md", "fingerprint",
			"version_index", "confidence", "support_count", "negative_count",
		}))
	mock.ExpectExec(`pg_advisory_unlock`).
		WithArgs(testLockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE promoter_run`).
		WithArgs(0, "done", "00000000-0000-0000-0000-00000000000b").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if _, err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v (a failure here means selectStalled's lateral subquery is missing the (created_at DESC, event_id DESC) tie-break required by §9.6a and doc.go:95)",
			err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// ────────────────────────────────────────────────────────────
// evaluator-3 finding #1 + #2: unpinned-HTTP bootstrap tests
// ────────────────────────────────────────────────────────────

// lazyEmbedder simulates the cmd/concept-promoter httpEmbedder
// in UNPINNED mode: ModelVersion() returns "" until the FIRST
// successful Embed() call, after which it returns the
// upstream-reported version. Matches the cmd's httpEmbedder
// observable shape so the unit-test contract mirrors prod.
//
// The `embedErr` knob lets a test simulate the upstream
// embedder being down at bootstrap time. The
// `embedModelEmpty` knob lets a test simulate a misbehaving
// upstream that returns 2xx without populating
// `model_version`.
type lazyEmbedder struct {
	mu              sync.Mutex
	resolvedModel   string
	pendingModel    string
	vec             []float32
	embedErr        error
	embedModelEmpty bool
	calls           []string
}

func newLazyEmbedder(pendingModel string, dim int) *lazyEmbedder {
	v := make([]float32, dim)
	for i := range v {
		v[i] = 1.0 / float32(dim)
	}
	return &lazyEmbedder{pendingModel: pendingModel, vec: v}
}

func (l *lazyEmbedder) Embed(_ context.Context, content string) ([]float32, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, content)
	if l.embedErr != nil {
		return nil, l.embedErr
	}
	if !l.embedModelEmpty {
		l.resolvedModel = l.pendingModel
	}
	out := make([]float32, len(l.vec))
	copy(out, l.vec)
	return out, nil
}

func (l *lazyEmbedder) ModelVersion() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.resolvedModel
}

func (l *lazyEmbedder) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.calls)
}

// TestEnsureModelReady_pinnedNoOp pins ensureModelReady's
// contract that PINNED embedders return immediately without
// any Embed() call. The fakeEmbedder fixture is already
// pinned ("test@v1") so this is the baseline.
func TestEnsureModelReady_pinnedNoOp(t *testing.T) {
	svc, _, db, emb, _ := newTestSvc(t)
	defer db.Close()

	vec, mv, err := svc.ensureModelReady(context.Background(), "irrelevant")
	if err != nil {
		t.Fatalf("ensureModelReady: %v", err)
	}
	if mv != "test@v1" {
		t.Fatalf("expected mv='test@v1'; got %q", mv)
	}
	if vec != nil {
		t.Fatalf("expected nil vec in pinned mode; got %d-dim", len(vec))
	}
	if emb.callCount() != 0 {
		t.Fatalf("expected 0 Embed calls in pinned mode; got %d", emb.callCount())
	}
}

// TestEnsureModelReady_unpinnedBootstrapSuccess proves the
// helper performs a single bootstrap Embed call when the
// embedder is unpinned, and returns BOTH the resolved
// model_version AND the prefetched vector so the caller can
// thread it into runAttempt via publishState.prefetchedVec
// (avoiding a redundant second Embed for the same content).
func TestEnsureModelReady_unpinnedBootstrapSuccess(t *testing.T) {
	lz := newLazyEmbedder("upstream-model@v3", 4)
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	svc, err := New(db, lz, newFakeQdrant(), Config{
		ConfidenceThreshold: 0.7,
		SupportThreshold:    5,
		RunInterval:         time.Second,
		TickTimeout:         5 * time.Second,
		CandidateBatchSize:  10,
		RetryBatchSize:      10,
		AdvisoryLockKey:     testLockKey,
	}, silentLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	vec, mv, err := svc.ensureModelReady(context.Background(), "concept:name\ndesc")
	if err != nil {
		t.Fatalf("ensureModelReady bootstrap: %v", err)
	}
	if mv != "upstream-model@v3" {
		t.Fatalf("expected mv='upstream-model@v3'; got %q", mv)
	}
	if len(vec) != 4 {
		t.Fatalf("expected 4-dim prefetched vec; got %d-dim", len(vec))
	}
	if lz.callCount() != 1 {
		t.Fatalf("expected exactly 1 bootstrap Embed call; got %d", lz.callCount())
	}
	if _, _, err := svc.ensureModelReady(context.Background(), "anything"); err != nil {
		t.Fatalf("ensureModelReady second call: %v", err)
	}
	if lz.callCount() != 1 {
		t.Fatalf("expected ensureModelReady to be no-op once resolved; got %d Embed calls", lz.callCount())
	}
}

// TestEnsureModelReady_unpinnedEmbedderReturnsEmptyMV is the
// misbehaving-embedder case: the bootstrap Embed succeeded
// (2xx response) but the upstream omitted `model_version`,
// so ModelVersion() is STILL empty. The helper must surface
// a typed error rather than silently inserting an empty
// model_version into embedding_publish (which would violate
// the NOT NULL constraint and the §9.6a contract).
func TestEnsureModelReady_unpinnedEmbedderReturnsEmptyMV(t *testing.T) {
	lz := newLazyEmbedder("ignored", 4)
	lz.embedModelEmpty = true
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	svc, err := New(db, lz, newFakeQdrant(), Config{
		ConfidenceThreshold: 0.7,
		SupportThreshold:    5,
		RunInterval:         time.Second,
		TickTimeout:         5 * time.Second,
		CandidateBatchSize:  10,
		RetryBatchSize:      10,
		AdvisoryLockKey:     testLockKey,
	}, silentLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, _, err := svc.ensureModelReady(context.Background(), "c"); err == nil {
		t.Fatal("expected error when embedder returns 2xx but ModelVersion() stays empty; got nil")
	}
}

// TestEnsureModelReady_unpinnedEmbedFails pins the typed-
// error path when the bootstrap Embed call itself returns
// an error. The helper must NOT silently swallow this — the
// caller (promoteOne) needs to see the failure so the
// candidate stays unpromoted (no tx1, no orphan).
func TestEnsureModelReady_unpinnedEmbedFails(t *testing.T) {
	lz := newLazyEmbedder("anything", 4)
	lz.embedErr = errors.New("upstream HTTP 503")
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	svc, err := New(db, lz, newFakeQdrant(), Config{
		ConfidenceThreshold: 0.7,
		SupportThreshold:    5,
		RunInterval:         time.Second,
		TickTimeout:         5 * time.Second,
		CandidateBatchSize:  10,
		RetryBatchSize:      10,
		AdvisoryLockKey:     testLockKey,
	}, silentLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, _, err := svc.ensureModelReady(context.Background(), "c"); err == nil {
		t.Fatal("expected error when bootstrap Embed fails; got nil")
	}
}

// newUnpinnedTestSvc is the lazyEmbedder counterpart to
// newTestSvc. Used by the Tick-level regression tests below.
func newUnpinnedTestSvc(t *testing.T, lz *lazyEmbedder) (*Service, sqlmock.Sqlmock, *sql.DB, *fakeQdrant) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(false))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	qd := newFakeQdrant()
	deterministicUUID := func() (string, error) {
		return "11111111-1111-1111-1111-111111111111", nil
	}
	svc, err := New(db, lz, qd, Config{
		ConfidenceThreshold: 0.7,
		SupportThreshold:    5,
		RunInterval:         time.Second,
		TickTimeout:         5 * time.Second,
		CandidateBatchSize:  10,
		RetryBatchSize:      10,
		AdvisoryLockKey:     testLockKey,
	}, silentLogger(), WithUUIDFactory(deterministicUUID))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, mock, db, qd
}

// TestTick_unpinnedHTTPEndToEndPromotesWithCachedModelVersion is
// the headline evaluator-3 finding #2 regression: a full Tick
// using the UNPINNED HTTP embedder shape must (1) bootstrap
// the model_version via promoteOne's pre-tx1 warm-up Embed,
// (2) thread that version into tx2's embedding_publish row,
// and (3) NOT re-issue Embed inside runAttempt for the same
// content (the prefetched vec must be reused).
//
// Prior to evaluator-3 finding #1's fix this Tick would fail
// at the tx2 INSERT because insertPublishAndQueued read
// ModelVersion() BEFORE Embed had ever been called, so the
// pending-model cache was still empty.
func TestTick_unpinnedHTTPEndToEndPromotesWithCachedModelVersion(t *testing.T) {
	lz := newLazyEmbedder("upstream-model@v9", 4)
	svc, mock, db, qd := newUnpinnedTestSvc(t, lz)
	defer db.Close()

	mock.ExpectQuery(`INSERT INTO promoter_run`).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).
			AddRow("00000000-0000-0000-0000-00000000ffff"))
	mock.ExpectQuery(`pg_try_advisory_lock`).
		WithArgs(testLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	expectEmptyOrphanScan(mock)
	mock.ExpectQuery(`FROM embedding_publish ep`).
		WillReturnRows(sqlmock.NewRows([]string{
			"publish_id", "concept_version_id", "concept_id",
			"qdrant_point_id", "embedding_model_version",
			"name", "description_md", "fingerprint",
			"event_kind", "max_attempt",
		}))
	mock.ExpectQuery(`FROM latest`).
		WillReturnRows(sqlmock.NewRows([]string{
			"concept_id", "name", "description_md", "fingerprint",
			"version_index", "confidence", "support_count", "negative_count",
		}).AddRow(
			"22222222-2222-2222-2222-222222222222", "concept-name", "concept-desc",
			[]byte{0x01, 0x02, 0x03}, 3, 0.85, 7, 0,
		))

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM concept WHERE concept_id`).
		WithArgs("22222222-2222-2222-2222-222222222222").
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	mock.ExpectQuery(`SELECT cv.version_index,`).
		WithArgs("22222222-2222-2222-2222-222222222222").
		WillReturnRows(sqlmock.NewRows([]string{
			"version_index", "confidence", "support_count", "negative_count", "already_promoted",
		}).AddRow(3, 0.85, 7, 0, false))
	mock.ExpectQuery(`INSERT INTO concept_version`).
		WithArgs("22222222-2222-2222-2222-222222222222", 4, 0.85, "high",
			7, 0, "00000000-0000-0000-0000-00000000ffff").
		WillReturnRows(sqlmock.NewRows([]string{"concept_version_id"}).
			AddRow("33333333-3333-3333-3333-333333333333"))
	mock.ExpectCommit()

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO embedding_publish`).
		WithArgs("33333333-3333-3333-3333-333333333333", "upstream-model@v9",
			"11111111-1111-1111-1111-111111111111").
		WillReturnRows(sqlmock.NewRows([]string{"publish_id"}).
			AddRow("44444444-4444-4444-4444-444444444444"))
	mock.ExpectExec(`INSERT INTO embedding_publish_event`).
		WithArgs("44444444-4444-4444-4444-444444444444", "queued", 0, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	mock.ExpectExec(`INSERT INTO embedding_publish_event`).
		WithArgs("44444444-4444-4444-4444-444444444444", "vector_written", 0, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Iter-2 fix #3: tx-wrapped published-event commit.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT coalesce\(details_json ->> 'supersedes_publish_id', ''\)`).
		WithArgs("44444444-4444-4444-4444-444444444444").
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(""))
	mock.ExpectExec(`INSERT INTO embedding_publish_event`).
		WithArgs("44444444-4444-4444-4444-444444444444", 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	mock.ExpectExec(`pg_advisory_unlock`).
		WithArgs(testLockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE promoter_run`).
		WithArgs(1, "done", "00000000-0000-0000-0000-00000000ffff").
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := svc.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v (a failure here means evaluator-3 finding #1 has REGRESSED — promoteOne is no longer warming the unpinned embedder before tx2)", err)
	}
	if res.ConceptsPromoted != 1 {
		t.Fatalf("expected 1 promoted; got %d", res.ConceptsPromoted)
	}
	if lz.callCount() != 1 {
		t.Fatalf("expected exactly 1 Embed call (the ensureModelReady bootstrap, reused via publishState.prefetchedVec); got %d (a >1 count means runAttempt did NOT reuse the prefetched vec — evaluator-3 finding #1 efficiency regression)", lz.callCount())
	}
	if qd.upsertCount() != 1 {
		t.Fatalf("expected exactly 1 Qdrant upsert; got %d", qd.upsertCount())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestTick_unpinnedHTTPBootstrapEmbedFailureDoesNotCreateOrphan
// is the rubber-duck blocker #2 regression: an unpinned-mode
// transient embedder outage during promoteOne's pre-tx1
// warm-up must NOT commit a promoted ConceptVersion (which
// would create an orphan that processOrphans then has to
// recover). The candidate stays untouched and gets
// re-evaluated on the next tick.
//
// Key sqlmock expectation: NO `BEGIN`/`INSERT INTO concept_version`
// — tx1 must never start.
func TestTick_unpinnedHTTPBootstrapEmbedFailureDoesNotCreateOrphan(t *testing.T) {
	lz := newLazyEmbedder("upstream-model@v9", 4)
	lz.embedErr = errors.New("upstream HTTP 503")
	svc, mock, db, qd := newUnpinnedTestSvc(t, lz)
	defer db.Close()

	mock.ExpectQuery(`INSERT INTO promoter_run`).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).
			AddRow("00000000-0000-0000-0000-00000000fffe"))
	mock.ExpectQuery(`pg_try_advisory_lock`).
		WithArgs(testLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	expectEmptyOrphanScan(mock)
	mock.ExpectQuery(`FROM embedding_publish ep`).
		WillReturnRows(sqlmock.NewRows([]string{
			"publish_id", "concept_version_id", "concept_id",
			"qdrant_point_id", "embedding_model_version",
			"name", "description_md", "fingerprint",
			"event_kind", "max_attempt",
		}))
	mock.ExpectQuery(`FROM latest`).
		WillReturnRows(sqlmock.NewRows([]string{
			"concept_id", "name", "description_md", "fingerprint",
			"version_index", "confidence", "support_count", "negative_count",
		}).AddRow(
			"22222222-2222-2222-2222-222222222222", "concept-name", "concept-desc",
			[]byte{0x01, 0x02, 0x03}, 3, 0.85, 7, 0,
		))

	mock.ExpectExec(`pg_advisory_unlock`).
		WithArgs(testLockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE promoter_run`).
		WithArgs(0, sqlmock.AnyArg(), "00000000-0000-0000-0000-00000000fffe").
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, _ := svc.Tick(context.Background())
	if res.ConceptsPromoted != 0 {
		t.Fatalf("expected 0 promoted on warm-up failure; got %d", res.ConceptsPromoted)
	}
	if res.PublishFailures != 0 {
		t.Fatalf("expected 0 PublishFailures (no publish row ever opened); got %d", res.PublishFailures)
	}
	if qd.upsertCount() != 0 {
		t.Fatalf("expected 0 Qdrant upserts on warm-up failure; got %d", qd.upsertCount())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations (means tx1 ran when it should NOT have — rubber-duck blocker #2 regression: warm-up should run BEFORE tx1 so a failure doesn't create an orphan): %v", err)
	}
}

// TestTick_unpinnedHTTPRetryEarlyReturnsOnEmptyStalls is the
// rubber-duck blocker #1 regression: processRetries must
// early-return when selectStalled yields zero rows so the
// unpinned-mode ModelVersion()-check does NOT abort the
// tick before promoteOne can bootstrap.
func TestTick_unpinnedHTTPRetryEarlyReturnsOnEmptyStalls(t *testing.T) {
	lz := newLazyEmbedder("upstream-model@v9", 4)
	svc, mock, db, _ := newUnpinnedTestSvc(t, lz)
	defer db.Close()

	mock.ExpectQuery(`INSERT INTO promoter_run`).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).
			AddRow("00000000-0000-0000-0000-00000000fffd"))
	mock.ExpectQuery(`pg_try_advisory_lock`).
		WithArgs(testLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	expectEmptyOrphanScan(mock)
	mock.ExpectQuery(`FROM embedding_publish ep`).
		WillReturnRows(sqlmock.NewRows([]string{
			"publish_id", "concept_version_id", "concept_id",
			"qdrant_point_id", "embedding_model_version",
			"name", "description_md", "fingerprint",
			"event_kind", "max_attempt",
		}))
	mock.ExpectQuery(`FROM latest`).
		WillReturnRows(sqlmock.NewRows([]string{
			"concept_id", "name", "description_md", "fingerprint",
			"version_index", "confidence", "support_count", "negative_count",
		}))
	mock.ExpectExec(`pg_advisory_unlock`).
		WithArgs(testLockKey).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE promoter_run`).
		WithArgs(0, "done", "00000000-0000-0000-0000-00000000fffd").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if _, err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v (a failure here means processRetries is NOT early-returning on empty stalls — rubber-duck blocker #1 regression)", err)
	}
	if lz.callCount() != 0 {
		t.Fatalf("expected 0 Embed calls when both stalls AND candidates are empty; got %d", lz.callCount())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestCommitConceptPublishedWithSupersede_emitsSupersedeForPrior
// is the focused unit test for iter-2 fix #3 — the concept-
// publish supersede emission helper. When the queued event at
// attempt_index=0 carries `supersedes_publish_id`, the helper
// MUST emit a `superseded` event for that prior publish in
// the same tx as the `published` event for the current
// publish. Verifies (a) the probe SELECT runs, (b) the
// published INSERT runs, (c) the superseded INSERT runs for
// the prior, (d) the tx commits, (e) the returned
// supersededPublishID matches the probed value, and (f) the
// post-publish hook fires with the right payload.
func TestCommitConceptPublishedWithSupersede_emitsSupersedeForPrior(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	const (
		newPublishID   = "11111111-1111-1111-1111-111111111111"
		priorPublishID = "22222222-2222-2222-2222-222222222222"
	)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT coalesce\(details_json ->> 'supersedes_publish_id', ''\)`).
		WithArgs(newPublishID).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(priorPublishID))
	mock.ExpectExec(`INSERT INTO embedding_publish_event[\s\S]+'published'::embedding_publish_event_kind`).
		WithArgs(newPublishID, 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO embedding_publish_event[\s\S]+'superseded'::embedding_publish_event_kind`).
		WithArgs(priorPublishID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	svc := &Service{db: db, logger: silentLogger()}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	defer conn.Close()

	got, err := svc.commitConceptPublishedWithSupersede(context.Background(), conn, newPublishID, 0)
	if err != nil {
		t.Fatalf("commitConceptPublishedWithSupersede: %v", err)
	}
	if got != priorPublishID {
		t.Errorf("returned supersededPublishID = %q, want %q", got, priorPublishID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestCommitConceptPublishedWithSupersede_noSupersedeWhenAbsent
// verifies the non-snapshot path: when the queued event has
// no supersedes_publish_id, the helper inserts published only
// (no superseded), commits, and returns empty.
func TestCommitConceptPublishedWithSupersede_noSupersedeWhenAbsent(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	const newPublishID = "11111111-1111-1111-1111-111111111111"

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT coalesce\(details_json ->> 'supersedes_publish_id', ''\)`).
		WithArgs(newPublishID).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(""))
	mock.ExpectExec(`INSERT INTO embedding_publish_event[\s\S]+'published'::embedding_publish_event_kind`).
		WithArgs(newPublishID, 2).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// NO superseded insert expected.
	mock.ExpectCommit()

	svc := &Service{db: db, logger: silentLogger()}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	defer conn.Close()

	got, err := svc.commitConceptPublishedWithSupersede(context.Background(), conn, newPublishID, 2)
	if err != nil {
		t.Fatalf("commitConceptPublishedWithSupersede: %v", err)
	}
	if got != "" {
		t.Errorf("returned supersededPublishID = %q, want empty", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestRunAttempt_postPublishHookFires_withSupersedeID is the
// iter-3 fix #3 end-to-end test for the concept-side
// post-publish hook. The iter-2 unit tests called the
// `commitConceptPublishedWithSupersede` helper in isolation
// but never installed `WithPostPublishHook` on the Service
// or asserted the hook fired. This test drives the full
// `runAttempt` chain (embed → upsert → vector_written →
// confirm → committed-published+superseded) with a hook
// installed and verifies the captured `PublishedEvent`
// carries the SupersededPublishID returned by the helper.
//
// Behaviour pinned:
//
//   - Hook fires exactly once on the success path.
//   - PublishID matches the publish being processed.
//   - SupersededPublishID is the prior_publish_id the probe
//     read out of the queued event's JSONB (i.e. the
//     snapshot-mint discriminator round-trips end-to-end).
//   - Kind == "concept" so the metrics consumer can route by
//     publish kind without re-querying the row.
//   - NodeID is the concept_version_id (the promoter reuses
//     the field for routing-by-version downstream).
//   - ModelVersion is the publishState's modelVersion (which
//     matches what the promoter's run loop snapshots from
//     the queued JSONB).
//
// Without this assertion `snapshot_published_total` for
// concepts (which depends on SupersededPublishID being
// non-empty) is silently unwired.
func TestRunAttempt_postPublishHookFires_withSupersedeID(t *testing.T) {
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
		sqlmock.MonitorPingsOption(false),
	)
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	const (
		publishID      = "44444444-4444-4444-4444-444444444444"
		priorPublishID = "55555555-5555-5555-5555-555555555555"
		pointID        = "11111111-1111-1111-1111-111111111111"
		conceptID      = "22222222-2222-2222-2222-222222222222"
		versionID      = "33333333-3333-3333-3333-333333333333"
	)

	var (
		captured     embedding.PublishedEvent
		captureCount int
		captureMu    sync.Mutex
	)
	hook := func(ev embedding.PublishedEvent) {
		captureMu.Lock()
		defer captureMu.Unlock()
		captureCount++
		captured = ev
	}

	emb := newFakeEmbedder("test@v1", 4)
	qd := newFakeQdrant()
	deterministicUUID := func() (string, error) { return pointID, nil }
	svc, err := New(db, emb, qd, Config{
		ConfidenceThreshold: 0.7,
		SupportThreshold:    5,
		RunInterval:         time.Second,
		TickTimeout:         5 * time.Second,
		CandidateBatchSize:  10,
		RetryBatchSize:      10,
		AdvisoryLockKey:     testLockKey,
	}, silentLogger(),
		WithUUIDFactory(deterministicUUID),
		WithPostPublishHook(hook),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// runAttempt step 4c: vector_written event on the
	// pool-pinned conn (sqlmock conn).
	mock.ExpectExec(`INSERT INTO embedding_publish_event`).
		WithArgs(publishID, "vector_written", 0, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// runAttempt step 6: commitConceptPublishedWithSupersede.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT coalesce\(details_json ->> 'supersedes_publish_id', ''\)`).
		WithArgs(publishID).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(priorPublishID))
	mock.ExpectExec(`INSERT INTO embedding_publish_event[\s\S]+'published'::embedding_publish_event_kind`).
		WithArgs(publishID, 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO embedding_publish_event[\s\S]+'superseded'::embedding_publish_event_kind`).
		WithArgs(priorPublishID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	defer conn.Close()

	// Pre-seed Qdrant so the runAttempt confirm step (Step
	// 5) sees the point. We use a non-nil prefetchedVec so
	// step 4a is skipped without needing an embedder stub.
	state := publishState{
		publishID:     publishID,
		pointID:       pointID,
		modelVersion:  "test@v1",
		attemptIndex:  0,
		mode:          "candidate",
		conceptID:     conceptID,
		versionID:     versionID,
		fingerprint:   []byte{0xde, 0xad, 0xbe, 0xef},
		prefetchedVec: []float32{0.25, 0.25, 0.25, 0.25},
	}

	last, err := svc.runAttempt(context.Background(), conn, state, "content-not-embedded-prefetched")
	if err != nil {
		t.Fatalf("runAttempt: %v", err)
	}
	if last != embedding.EventKindPublished {
		t.Fatalf("runAttempt last event = %q, want %q", last, embedding.EventKindPublished)
	}

	captureMu.Lock()
	defer captureMu.Unlock()
	if captureCount != 1 {
		t.Fatalf("post-publish hook fire count = %d, want 1 (the hook MUST fire exactly once on the success path)", captureCount)
	}
	if captured.PublishID != publishID {
		t.Errorf("hook PublishID = %q, want %q", captured.PublishID, publishID)
	}
	if captured.SupersededPublishID != priorPublishID {
		t.Errorf("hook SupersededPublishID = %q, want %q (the probed supersedes_publish_id MUST round-trip into the hook payload — without this, snapshot_published_total for concepts stays at 0 even when a snapshot supersede actually happened)", captured.SupersededPublishID, priorPublishID)
	}
	if captured.Kind != "concept" {
		t.Errorf("hook Kind = %q, want %q", captured.Kind, "concept")
	}
	if captured.NodeID != versionID {
		t.Errorf("hook NodeID = %q, want %q (concept-side NodeID is the concept_version_id)", captured.NodeID, versionID)
	}
	if captured.ModelVersion != "test@v1" {
		t.Errorf("hook ModelVersion = %q, want %q", captured.ModelVersion, "test@v1")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
	if qd.upsertCount() != 1 {
		t.Errorf("expected exactly 1 Qdrant upsert; got %d", qd.upsertCount())
	}
	if emb.callCount() != 0 {
		t.Errorf("expected 0 Embed calls (prefetchedVec was supplied); got %d", emb.callCount())
	}
}

// TestRunAttempt_postPublishHookFires_emptySupersedeOnNonSnapshot
// pins the negative case: a publish that was NOT enqueued by
// the snapshot verb (queued event carries no
// supersedes_publish_id) MUST still fire the hook, but with
// SupersededPublishID="". The cross-binary metrics consumer
// uses the empty value as the signal to NOT bump
// snapshot_published_total — without this assertion, a
// regression that always returned a non-empty value (or
// never fired the hook at all) would silently break the
// counter accounting.
func TestRunAttempt_postPublishHookFires_emptySupersedeOnNonSnapshot(t *testing.T) {
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
		sqlmock.MonitorPingsOption(false),
	)
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	const (
		publishID = "66666666-6666-6666-6666-666666666666"
		pointID   = "11111111-1111-1111-1111-111111111111"
		conceptID = "22222222-2222-2222-2222-222222222222"
		versionID = "33333333-3333-3333-3333-333333333333"
	)

	var (
		captured     embedding.PublishedEvent
		captureCount int
		captureMu    sync.Mutex
	)
	hook := func(ev embedding.PublishedEvent) {
		captureMu.Lock()
		defer captureMu.Unlock()
		captureCount++
		captured = ev
	}

	emb := newFakeEmbedder("test@v1", 4)
	qd := newFakeQdrant()
	deterministicUUID := func() (string, error) { return pointID, nil }
	svc, err := New(db, emb, qd, Config{
		ConfidenceThreshold: 0.7,
		SupportThreshold:    5,
		RunInterval:         time.Second,
		TickTimeout:         5 * time.Second,
		CandidateBatchSize:  10,
		RetryBatchSize:      10,
		AdvisoryLockKey:     testLockKey,
	}, silentLogger(),
		WithUUIDFactory(deterministicUUID),
		WithPostPublishHook(hook),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mock.ExpectExec(`INSERT INTO embedding_publish_event`).
		WithArgs(publishID, "vector_written", 0, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT coalesce\(details_json ->> 'supersedes_publish_id', ''\)`).
		WithArgs(publishID).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(""))
	mock.ExpectExec(`INSERT INTO embedding_publish_event[\s\S]+'published'::embedding_publish_event_kind`).
		WithArgs(publishID, 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// NO superseded insert expected — non-snapshot path.
	mock.ExpectCommit()

	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	defer conn.Close()

	state := publishState{
		publishID:     publishID,
		pointID:       pointID,
		modelVersion:  "test@v1",
		attemptIndex:  0,
		mode:          "candidate",
		conceptID:     conceptID,
		versionID:     versionID,
		fingerprint:   []byte{0xca, 0xfe, 0xba, 0xbe},
		prefetchedVec: []float32{0.5, 0.5, 0.5, 0.5},
	}

	last, err := svc.runAttempt(context.Background(), conn, state, "concept-content")
	if err != nil {
		t.Fatalf("runAttempt: %v", err)
	}
	if last != embedding.EventKindPublished {
		t.Fatalf("runAttempt last event = %q, want %q", last, embedding.EventKindPublished)
	}

	captureMu.Lock()
	defer captureMu.Unlock()
	if captureCount != 1 {
		t.Fatalf("post-publish hook fire count = %d, want 1 (hook fires on EVERY published transition, not just supersede)", captureCount)
	}
	if captured.SupersededPublishID != "" {
		t.Errorf("hook SupersededPublishID = %q, want empty (non-snapshot publish MUST NOT carry a supersede id — the metrics consumer relies on '' to mean 'do not bump snapshot_published_total')", captured.SupersededPublishID)
	}
	if captured.PublishID != publishID {
		t.Errorf("hook PublishID = %q, want %q", captured.PublishID, publishID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
