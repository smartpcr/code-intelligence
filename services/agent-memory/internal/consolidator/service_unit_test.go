package consolidator

// Pure / sqlmock unit tests. No live PostgreSQL required; every
// test in this file is hermetic so a developer can run
// `go test ./internal/consolidator/...` without the docker
// compose stack. Live-PG behaviour is exercised by the sibling
// service_integration_test.go file.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// silentLogger discards every Service-emitted record so the
// test output stays clean. Tests that need to assert on log
// content use captureLogger below instead.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// helper: fixed 32-byte fingerprint derived from a string seed
// (so tests are deterministic and readable).
func fpFromSeed(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// ────────────────────────────────────────────────────────────
// computeSignature
// ────────────────────────────────────────────────────────────

func TestComputeSignature_emptyKeysReturnsNotOK(t *testing.T) {
	sig, ok := computeSignature(nil)
	if ok {
		t.Fatalf("expected ok=false for nil keys, got ok=true sig=%x", sig)
	}
	zero := [32]byte{}
	if sig != zero {
		t.Fatalf("expected zero sig for nil keys, got %x", sig)
	}
}

func TestComputeSignature_allEmptyFingerprintsReturnsNotOK(t *testing.T) {
	keys := []observationKey{
		{role: "node_hit", fingerprint: nil},
		{role: "edge_hit", fingerprint: []byte{}},
	}
	if _, ok := computeSignature(keys); ok {
		t.Fatalf("expected ok=false when every key has empty fingerprint")
	}
}

func TestComputeSignature_isOrderIndependent(t *testing.T) {
	fp1 := fpFromSeed("alpha")
	fp2 := fpFromSeed("beta")

	a, okA := computeSignature([]observationKey{
		{role: "node_hit", fingerprint: fp1},
		{role: "edge_hit", fingerprint: fp2},
	})
	b, okB := computeSignature([]observationKey{
		{role: "edge_hit", fingerprint: fp2},
		{role: "node_hit", fingerprint: fp1},
	})
	if !okA || !okB {
		t.Fatalf("expected both sigs to be non-empty (okA=%v okB=%v)", okA, okB)
	}
	if a != b {
		t.Fatalf("signature must be order-independent\n  a=%x\n  b=%x", a, b)
	}
}

func TestComputeSignature_deduplicatesIdenticalKeys(t *testing.T) {
	fp1 := fpFromSeed("alpha")
	once, _ := computeSignature([]observationKey{
		{role: "node_hit", fingerprint: fp1},
	})
	twice, _ := computeSignature([]observationKey{
		{role: "node_hit", fingerprint: fp1},
		{role: "node_hit", fingerprint: fp1},
	})
	if once != twice {
		t.Fatalf("duplicate (role, fingerprint) entries must not change the signature\n  once=%x\n  twice=%x",
			once, twice)
	}
}

func TestComputeSignature_distinctRolesDistinctFingerprintsProduceDifferentSig(t *testing.T) {
	fp1 := fpFromSeed("alpha")
	a, _ := computeSignature([]observationKey{{role: "node_hit", fingerprint: fp1}})
	b, _ := computeSignature([]observationKey{{role: "edge_hit", fingerprint: fp1}})
	c, _ := computeSignature([]observationKey{{role: "node_hit", fingerprint: fpFromSeed("other")}})
	if a == b {
		t.Fatalf("role must affect signature; got identical sig %x for distinct roles", a)
	}
	if a == c {
		t.Fatalf("fingerprint must affect signature; got identical sig %x for distinct fingerprints", a)
	}
}

func TestComputeSignature_crossRepoSameFingerprintCollides(t *testing.T) {
	// Two "repos" with their own per-repo Node UUIDs but the
	// SAME canonical 32-byte fingerprint. The signature MUST
	// collide -- this is the G6 cross-repo invariant.
	shared := fpFromSeed("calls/pkg/Foo")
	repoA, _ := computeSignature([]observationKey{{role: "node_hit", fingerprint: shared}})
	repoB, _ := computeSignature([]observationKey{{role: "node_hit", fingerprint: shared}})
	if repoA != repoB {
		t.Fatalf("cross-repo same-fingerprint observations must collide\n  repoA=%x\n  repoB=%x",
			repoA, repoB)
	}
}

func TestComputeSignature_preimageUsesHexEncoding(t *testing.T) {
	// Regression guard for the rubber-duck concern that raw-byte
	// concatenation could let "role bytes spilling into fingerprint
	// bytes" collide. The hex-encoded pre-image keeps the role/
	// fingerprint boundary unambiguous.
	fp := fpFromSeed("alpha")
	got, _ := computeSignature([]observationKey{{role: "node_hit", fingerprint: fp}})

	// Reconstruct the expected pre-image: "role:hex(fingerprint)".
	want := sha256.Sum256([]byte("node_hit:" + hex.EncodeToString(fp)))
	if got != want {
		t.Fatalf("pre-image format drift\n  got=%x\n  want=%x", got, want)
	}
}

// ────────────────────────────────────────────────────────────
// bandOf
// ────────────────────────────────────────────────────────────

func TestBandOf_thresholds(t *testing.T) {
	for _, tc := range []struct {
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bandOf(tc.in); got != tc.want {
				t.Fatalf("bandOf(%v)=%q want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ────────────────────────────────────────────────────────────
// episodeState.polarity()
// ────────────────────────────────────────────────────────────

func TestEpisodeStatePolarity(t *testing.T) {
	cases := []struct {
		kind    string
		outcome string
		want    string
	}{
		{"agent", "success", "positive"},
		{"synthetic_positive", "success", "positive"},
		// synthetic_positive overrides any outcome polarity:
		{"synthetic_positive", "failure", "positive"},
		{"agent", "failure", "negative"},
		{"agent", "refused", "negative"},
		{"agent", "degraded", "negative"},
		{"agent", "human_corrected", "negative"},
		// Unknown outcomes do not map -> empty (Episode is
		// silently skipped by emitGroup; defensive contract).
		{"agent", "weird", ""},
	}
	for _, tc := range cases {
		ep := &episodeState{kind: tc.kind, outcome: tc.outcome}
		if got := ep.polarity(); got != tc.want {
			t.Fatalf("polarity(kind=%q outcome=%q)=%q want %q",
				tc.kind, tc.outcome, got, tc.want)
		}
	}
}

// ────────────────────────────────────────────────────────────
// episodeState.nodeIDs() -- dedup + sort
// ────────────────────────────────────────────────────────────

func TestEpisodeStateNodeIDs_dedupsAndSorts(t *testing.T) {
	ep := &episodeState{observations: []observationRow{
		{role: "node_hit", nodeID: "ccc"},
		{role: "node_hit", nodeID: "aaa"},
		{role: "node_hit", nodeID: "bbb"},
		{role: "node_hit", nodeID: "aaa"}, // duplicate
		{role: "edge_hit"},                // no nodeID -> skipped
	}}
	got := ep.nodeIDs()
	want := []string{"aaa", "bbb", "ccc"}
	if len(got) != len(want) {
		t.Fatalf("nodeIDs len=%d want %d (got=%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("nodeIDs[%d]=%q want %q (full got=%v)", i, got[i], want[i], got)
		}
	}
}

func TestEpisodeStateNodeIDs_noNodeHitReturnsEmpty(t *testing.T) {
	ep := &episodeState{observations: []observationRow{
		{role: "edge_hit"},
		{role: "concept_hit"},
	}}
	if got := ep.nodeIDs(); len(got) != 0 {
		t.Fatalf("nodeIDs for no-node-hit episode want empty, got %v", got)
	}
}

// ────────────────────────────────────────────────────────────
// nullIfEmpty
// ────────────────────────────────────────────────────────────

func TestNullIfEmpty(t *testing.T) {
	if v := nullIfEmpty(""); v != nil {
		t.Fatalf("nullIfEmpty(\"\") = %v, want nil", v)
	}
	if v := nullIfEmpty("x"); v != "x" {
		t.Fatalf("nullIfEmpty(\"x\") = %v, want \"x\"", v)
	}
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

	svc, err := New(db, Config{
		Threshold:          0,
		RunInterval:        0,
		TickTimeout:        -1,
		WakeAfterNEpisodes: -5,
		WakeCheckInterval:  0,
		AdvisoryLockKey:    0,
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cfg := svc.Config()
	if cfg.Threshold != DefaultThreshold {
		t.Fatalf("Threshold default not applied: got %d", cfg.Threshold)
	}
	if cfg.RunInterval != DefaultRunInterval {
		t.Fatalf("RunInterval default not applied: got %v", cfg.RunInterval)
	}
	if cfg.TickTimeout != DefaultTickTimeout {
		t.Fatalf("TickTimeout default not applied: got %v", cfg.TickTimeout)
	}
	if cfg.WakeAfterNEpisodes != 0 {
		t.Fatalf("WakeAfterNEpisodes negative clamped to 0; got %d", cfg.WakeAfterNEpisodes)
	}
	if cfg.WakeCheckInterval != DefaultWakeCheckInterval {
		t.Fatalf("WakeCheckInterval default not applied: got %v", cfg.WakeCheckInterval)
	}
	if cfg.AdvisoryLockKey != ConsolidatorAdvisoryLockKey {
		t.Fatalf("AdvisoryLockKey default not applied: got %x", cfg.AdvisoryLockKey)
	}
}

func TestNew_preservesNonZeroFields(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	svc, err := New(db, Config{
		Threshold:          7,
		RunInterval:        2 * time.Minute,
		TickTimeout:        9 * time.Minute,
		WakeAfterNEpisodes: 25,
		WakeCheckInterval:  200 * time.Millisecond,
		AdvisoryLockKey:    0x123,
	}, silentLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cfg := svc.Config()
	if cfg.Threshold != 7 || cfg.RunInterval != 2*time.Minute ||
		cfg.TickTimeout != 9*time.Minute || cfg.WakeAfterNEpisodes != 25 ||
		cfg.WakeCheckInterval != 200*time.Millisecond ||
		cfg.AdvisoryLockKey != 0x123 {
		t.Fatalf("non-zero fields clobbered: %+v", cfg)
	}
}

func TestNew_panicsOnNilDB(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("New(nil) must panic, did not")
		}
	}()
	_, _ = New(nil, Config{}, nil)
}

// ────────────────────────────────────────────────────────────
// Tick: lock-skip path on a fresh cluster (no prior run).
// Verifies the iter-2 deadlock fix: emission phase pins one
// conn and returns it BEFORE the finalize UPDATE is issued.
// The whole flow uses sqlmock with MaxOpenConns=1 so a leak
// would surface as a "no available connection" / unmet
// expectation failure.
// ────────────────────────────────────────────────────────────

const testLockKey int64 = 0x7EADBEEFCAFEBABE

func newTestSvc(t *testing.T) (*Service, sqlmock.Sqlmock, *sql.DB) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(false))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	svc, err := New(db, Config{
		Threshold:       3,
		RunInterval:     time.Second,
		TickTimeout:     5 * time.Second,
		AdvisoryLockKey: testLockKey,
	}, silentLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, mock, db
}

func TestTick_lockSkipped_finalisesRunAsLockSkippedWithInheritedNullMark(t *testing.T) {
	svc, mock, db := newTestSvc(t)
	defer db.Close()

	// Step 1: open run.
	mock.ExpectQuery(`INSERT INTO consolidator_run`).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow("00000000-0000-0000-0000-000000000001"))

	// Step 2: prior high-water (no prior 'done' run -> ErrNoRows).
	mock.ExpectQuery(`FROM consolidator_run`).
		WillReturnError(sql.ErrNoRows)

	// Step 3: pin conn + try_advisory_lock -> not acquired.
	mock.ExpectQuery(`pg_try_advisory_lock`).
		WithArgs(testLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(false))

	// Step 6: finalize as 'lock_skipped' with NULL mark (no prior,
	// no progress). status='lock_skipped' so priorHighWater's
	// `WHERE status='done'` filter excludes this row -- the iter-3
	// evaluator's #2 finding (a skipped run's stale mark must NOT
	// regress the effective cursor).
	mock.ExpectExec(`UPDATE consolidator_run`).
		WithArgs(nil, "lock_skipped", "00000000-0000-0000-0000-000000000001").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Step 7: max(episode.created_at) probe for lag gauge.
	mock.ExpectQuery(`SELECT max\(created_at\) FROM episode`).
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(nil))

	res, err := svc.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !res.LockSkipped {
		t.Fatalf("expected LockSkipped=true")
	}
	if res.EpisodesScanned != 0 || res.ConceptsCreated != 0 ||
		res.VersionsAppended != 0 || res.SupportsAppended != 0 {
		t.Fatalf("lock-skipped tick must report all-zero counters: %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestTick_lockSkipped_inheritsPriorMarkWhenAvailable(t *testing.T) {
	svc, mock, db := newTestSvc(t)
	defer db.Close()

	priorMarkID := "11111111-1111-1111-1111-111111111111"
	priorTS := time.Now().Add(-1 * time.Minute).UTC()

	mock.ExpectQuery(`INSERT INTO consolidator_run`).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow("00000000-0000-0000-0000-000000000002"))

	mock.ExpectQuery(`FROM consolidator_run`).
		WillReturnRows(sqlmock.NewRows([]string{"episode_high_water_mark", "created_at"}).
			AddRow(priorMarkID, priorTS))

	mock.ExpectQuery(`pg_try_advisory_lock`).
		WithArgs(testLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(false))

	// Finalize MUST inherit the prior mark (not pass NULL) AND
	// stamp status='lock_skipped' so this row is excluded from
	// priorHighWater's `WHERE status='done'` filter on the next
	// tick -- preserving the iter-2 winner's mark as the
	// effective cursor instead of letting a stale-mark skip row
	// regress it (iter-3 evaluator's #2 finding).
	mock.ExpectExec(`UPDATE consolidator_run`).
		WithArgs(&priorMarkID, "lock_skipped", "00000000-0000-0000-0000-000000000002").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery(`SELECT max\(created_at\) FROM episode`).
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(time.Now().UTC()))

	res, err := svc.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !res.LockSkipped {
		t.Fatalf("expected LockSkipped=true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestTick_openRunFailure surfaces the failure as the
// caller-visible error AND increments consolidator_errors_total.
// The deferred finalize-as-failed cannot fire because no run
// row was opened, so there are no other SQL expectations.
func TestTick_openRunFailure(t *testing.T) {
	svc, mock, db := newTestSvc(t)
	defer db.Close()

	mock.ExpectQuery(`INSERT INTO consolidator_run`).
		WillReturnError(errors.New("connection refused"))

	_, err := svc.Tick(context.Background())
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if svc.Metrics().ErrorsTotal() != 1 {
		t.Fatalf("expected ErrorsTotal=1, got %d", svc.Metrics().ErrorsTotal())
	}
	if svc.Metrics().RunsTotal() != 1 {
		t.Fatalf("expected RunsTotal=1 (incremented before the failure), got %d",
			svc.Metrics().RunsTotal())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// ────────────────────────────────────────────────────────────
// Metrics: rename validation -- the public CONST value is the
// literal "consolidator_episode_lag" (no _seconds suffix), per
// implementation-plan.md §6.1 line 903.
// ────────────────────────────────────────────────────────────

func TestMetricConsolidatorEpisodeLag_constLiteral(t *testing.T) {
	const want = "consolidator_episode_lag"
	if MetricConsolidatorEpisodeLag != want {
		t.Fatalf("metric name drift: got %q want %q (implementation-plan.md §6.1 line 903)",
			MetricConsolidatorEpisodeLag, want)
	}
}

// ────────────────────────────────────────────────────────────
// unconsumedEpisodeCount: when no prior 'done' run has a
// non-NULL mark (e.g. binary started on an empty cluster, fired
// its initial tick which finalised mark=NULL, then writers
// seeded a batch of episodes), the function MUST count ALL
// episodes -- they are all "unconsumed" by definition. This is
// the iter-3 evaluator's #1 finding: the prior `return 0`
// branch caused TestRun_wakeAfterNEpisodes to time out because
// the wake-check loop never saw N >= WakeAfterNEpisodes.
// ────────────────────────────────────────────────────────────

func TestUnconsumedEpisodeCount_noPriorMarkCountsAll(t *testing.T) {
	svc, mock, db := newTestSvc(t)
	defer db.Close()

	// priorHighWater: no prior 'done' run with non-NULL mark
	// (the LATEST 'done' run wrote mark=NULL on the empty
	// cluster). priorHighWater returns sql.ErrNoRows here.
	mock.ExpectQuery(`FROM consolidator_run`).
		WillReturnError(sql.ErrNoRows)

	// MUST count every episode row (no cursor predicate).
	mock.ExpectQuery(`SELECT count\(\*\) FROM episode$`).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(42))

	n, err := svc.unconsumedEpisodeCount(context.Background())
	if err != nil {
		t.Fatalf("unconsumedEpisodeCount: %v", err)
	}
	if n != 42 {
		t.Fatalf("expected count=42 (no prior mark -> count all), got %d", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestUnconsumedEpisodeCount_withPriorMarkUsesDelta(t *testing.T) {
	svc, mock, db := newTestSvc(t)
	defer db.Close()

	priorMarkID := "11111111-1111-1111-1111-111111111111"
	priorTS := time.Now().Add(-1 * time.Minute).UTC()

	mock.ExpectQuery(`FROM consolidator_run`).
		WillReturnRows(sqlmock.NewRows([]string{"episode_high_water_mark", "created_at"}).
			AddRow(priorMarkID, priorTS))

	// MUST use the DELTA predicate, NOT count all.
	mock.ExpectQuery(`FROM episode\s+WHERE \(created_at, episode_id\) >`).
		WithArgs(priorTS, priorMarkID).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(7))

	n, err := svc.unconsumedEpisodeCount(context.Background())
	if err != nil {
		t.Fatalf("unconsumedEpisodeCount: %v", err)
	}
	if n != 7 {
		t.Fatalf("expected count=7 (delta), got %d", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// ────────────────────────────────────────────────────────────
// promoteWithDedup -- iter-5 regression tests. The helper
// unifies fresh-concept and conflict-winner promotion under a
// single dedup-aware path. These tests pin two contracts that
// previously had only narrative / code-review coverage:
//
//  1. CONFLICT CASE: when the concept already has a v=0,
//     promoteWithDedup MUST append version_index = prev+1, NOT
//     blindly insert another v=0 (the iter-4 evaluator's #3
//     finding -- a v=0 re-insert would violate
//     concept_version_concept_version_uidx).
//
//  2. WITHIN-LOCKED DEDUP: when the locked candidate set
//     contains duplicate (episode, node) pairs (legitimate
//     under concept_candidate_support having no UNIQUE
//     constraint), the helper MUST insert exactly ONE
//     concept_support row per distinct pair, BUT MUST mark
//     EVERY locked candidate row promoted so no row stays
//     pending (rubber-duck iter-5 blocking issue #1).
// ────────────────────────────────────────────────────────────

// TestPromoteWithDedup_conflictAppendsNextVersionIndex is the
// direct regression test for the iter-4 evaluator's finding #3.
// Setup: concept already has v=0 sup=5 (episodes A..E). The
// locked candidate set contributes 5 NEW positive episodes
// (F..J) for the same node. Expected:
//   - version_index = 1 (NOT 0); the iter-4 bug bound 0 here
//     and crashed on concept_version_concept_version_uidx.
//   - support_count = 10 (prev 5 + delta 5).
//   - 5 INSERT concept_support rows (one per new episode).
//   - UPDATE candidate_support promoted for all 5 locked ids.
func TestPromoteWithDedup_conflictAppendsNextVersionIndex(t *testing.T) {
	svc, mock, db := newTestSvc(t)
	defer db.Close()

	conceptID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	runID := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	nodeID := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	repoID := "dddddddd-dddd-dddd-dddd-dddddddddddd"

	epAlready := []string{
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
		"33333333-3333-3333-3333-333333333333",
		"44444444-4444-4444-4444-444444444444",
		"55555555-5555-5555-5555-555555555555",
	}
	epNew := []string{
		"66666666-6666-6666-6666-666666666666",
		"77777777-7777-7777-7777-777777777777",
		"88888888-8888-8888-8888-888888888888",
		"99999999-9999-9999-9999-999999999999",
		"a0000000-0000-0000-0000-000000000001",
	}
	candIDs := []string{
		"10000000-0000-0000-0000-000000000001",
		"10000000-0000-0000-0000-000000000002",
		"10000000-0000-0000-0000-000000000003",
		"10000000-0000-0000-0000-000000000004",
		"10000000-0000-0000-0000-000000000005",
	}

	locked := make([]candidateRow, 0, len(epNew))
	for i, ep := range epNew {
		locked = append(locked, candidateRow{
			id:        candIDs[i],
			repoID:    repoID,
			nodeID:    sql.NullString{String: nodeID, Valid: true},
			episodeID: ep,
			polarity:  "positive",
		})
	}

	mock.ExpectBegin()

	// existing concept_support: 5 rows.
	rows1 := sqlmock.NewRows([]string{"episode_id", "node_id"})
	for _, ep := range epAlready {
		rows1.AddRow(ep, nodeID)
	}
	mock.ExpectQuery(`FROM concept_support`).
		WithArgs(conceptID).
		WillReturnRows(rows1)

	// latest concept_version: v=0, sup=5, neg=0.
	mock.ExpectQuery(`FROM concept_version`).
		WithArgs(conceptID).
		WillReturnRows(sqlmock.NewRows([]string{"version_index", "support_count", "negative_count"}).
			AddRow(int32(0), int32(5), int32(0)))

	// THE REGRESSION ASSERTION: version_index bind MUST be 1
	// (iter-4 bug bound 0; new helper does prev+1).
	newVersionID := "fa110000-0000-0000-0000-000000000001"
	mock.ExpectQuery(`INSERT INTO concept_version`).
		WithArgs(
			conceptID,
			1,                // version_index = prev(0) + 1
			sqlmock.AnyArg(), // confidence float
			sqlmock.AnyArg(), // band string
			10,               // cumulativePos = prev(5) + delta(5)
			0,                // cumulativeNeg
			runID,
		).
		WillReturnRows(sqlmock.NewRows([]string{"concept_version_id"}).AddRow(newVersionID))

	// 5 concept_support inserts (one per NEW episode).
	for _, ep := range epNew {
		mock.ExpectExec(`INSERT INTO concept_support`).
			WithArgs(conceptID, newVersionID, repoID, nodeID, ep, "positive").
			WillReturnResult(sqlmock.NewResult(0, 1))
	}

	// UPDATE candidate_support promoted for all 5 locked ids.
	mock.ExpectExec(`UPDATE concept_candidate_support`).
		WithArgs(conceptID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 5))

	mock.ExpectCommit()

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	vv, ss, perr := svc.promoteWithDedup(
		context.Background(), tx, runID, conceptID, locked, "deadbeefcafef00d")
	if perr != nil {
		t.Fatalf("promoteWithDedup: %v", perr)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if vv != 1 {
		t.Fatalf("versionsAppended: got %d want 1", vv)
	}
	if ss != 5 {
		t.Fatalf("supportsAppended: got %d want 5", ss)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestPromoteWithDedup_withinLockedDedupSingleInsertPerPair pins
// the rubber-duck iter-5 blocking #1 contract: even when the
// locked candidate set contains duplicate (episode, node) pairs,
// the helper MUST emit exactly ONE concept_support row per
// distinct pair BUT MUST mark EVERY locked candidate row
// promoted so no row stays pending.
//
// Setup: fresh concept (no prior version, no existing supports),
// locked has 3 candidate rows for the SAME (episode, node) pair.
// Expected:
//   - version_index = 0 with support_count = 1 (NOT 3 -- per-
//     EPISODE counting deduplicates the 3 rows down to 1).
//   - exactly 1 concept_support insert.
//   - UPDATE candidate_support promoted for ALL 3 locked ids.
func TestPromoteWithDedup_withinLockedDedupSingleInsertPerPair(t *testing.T) {
	svc, mock, db := newTestSvc(t)
	defer db.Close()

	conceptID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	runID := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	nodeID := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	repoID := "dddddddd-dddd-dddd-dddd-dddddddddddd"
	episodeID := "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"

	locked := []candidateRow{
		{
			id:        "10000000-0000-0000-0000-000000000001",
			repoID:    repoID,
			nodeID:    sql.NullString{String: nodeID, Valid: true},
			episodeID: episodeID,
			polarity:  "positive",
		},
		{
			id:        "10000000-0000-0000-0000-000000000002",
			repoID:    repoID,
			nodeID:    sql.NullString{String: nodeID, Valid: true},
			episodeID: episodeID,
			polarity:  "positive",
		},
		{
			id:        "10000000-0000-0000-0000-000000000003",
			repoID:    repoID,
			nodeID:    sql.NullString{String: nodeID, Valid: true},
			episodeID: episodeID,
			polarity:  "positive",
		},
	}

	mock.ExpectBegin()

	// Fresh concept -> empty concept_support.
	mock.ExpectQuery(`FROM concept_support`).
		WithArgs(conceptID).
		WillReturnRows(sqlmock.NewRows([]string{"episode_id", "node_id"}))

	// Fresh concept -> no prior concept_version.
	mock.ExpectQuery(`FROM concept_version`).
		WithArgs(conceptID).
		WillReturnError(sql.ErrNoRows)

	// Exactly ONE version insert with version_index=0 and
	// support_count=1 (the 3 duplicate locked rows resolve to 1
	// distinct positive episode).
	newVersionID := "fa110000-0000-0000-0000-000000000010"
	mock.ExpectQuery(`INSERT INTO concept_version`).
		WithArgs(
			conceptID,
			0, // fresh -> version_index = 0
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			1, // cumulativePos = 1 distinct episode (NOT 3)
			0,
			runID,
		).
		WillReturnRows(sqlmock.NewRows([]string{"concept_version_id"}).AddRow(newVersionID))

	// Exactly ONE concept_support INSERT for the deduped pair.
	mock.ExpectExec(`INSERT INTO concept_support`).
		WithArgs(conceptID, newVersionID, repoID, nodeID, episodeID, "positive").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// UPDATE candidate_support promoted for ALL 3 locked ids
	// (even though only 1 concept_support row was inserted) so
	// the duplicate rows are drained -- no pending row left
	// behind to be re-locked on the next tick.
	mock.ExpectExec(`UPDATE concept_candidate_support`).
		WithArgs(conceptID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 3))

	mock.ExpectCommit()

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	vv, ss, perr := svc.promoteWithDedup(
		context.Background(), tx, runID, conceptID, locked, "cafebabec0ffee00")
	if perr != nil {
		t.Fatalf("promoteWithDedup: %v", perr)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if vv != 1 {
		t.Fatalf("versionsAppended: got %d want 1", vv)
	}
	if ss != 1 {
		t.Fatalf("supportsAppended: got %d want 1 (deduped from 3 locked rows)", ss)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}


