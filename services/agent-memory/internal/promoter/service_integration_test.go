package promoter

// Integration tests for the Stage 6.2 Concept Promoter worker
// against a live PostgreSQL 16 + pg_partman v5 instance. Skips
// cleanly when AGENT_MEMORY_PG_URL is unset, mirroring the
// convention in migrations/test_migrate_test.go and the
// sibling consolidator integration suite.
//
// Implementation-plan.md Stage 6.2 acceptance scenarios
// (lines 1010-1054):
//
//   * "threshold flips promoted=true with §8.7.1 ordering"
//     -- TestTick_thresholdFlipsPromotedTrueWithOrdering
//   * "PromoterRun precedes ConceptVersion FK reference"
//     -- TestTick_promoterRunPrecedesConceptVersionInsert
//   * "ConceptVersion precedes EmbeddingPublish"
//     -- TestTick_conceptVersionPrecedesEmbeddingPublish
//   * "below threshold stays unpromoted"
//     -- TestTick_belowThresholdStaysUnpromoted
//   * "Consolidator never writes EmbeddingIndex"
//     -- TestTick_consolidatorDoesNotWriteEmbeddingPublish
//
// The fakes from service_unit_test.go (fakeEmbedder, fakeQdrant)
// are reused here -- they live in the same package so the
// integration tests get a deterministic vector + in-memory
// upsert log without spinning up a real Qdrant.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/consolidator"
	"github.com/smartpcr/code-intelligence/services/agent-memory/migrations"
)

const (
	intEnvPGURL      = "AGENT_MEMORY_PG_URL"
	intTestDBTimeout = 60 * time.Second
)

// promFixture is the per-test PostgreSQL substrate.
type promFixture struct {
	db      *sql.DB
	schema  string
	cleanup func()
}

func openPromFixture(t *testing.T) *promFixture {
	t.Helper()
	base := os.Getenv(intEnvPGURL)
	if base == "" {
		t.Skipf("skipping: %s is unset; integration tests require a live PostgreSQL", intEnvPGURL)
	}

	owner, err := sql.Open("postgres", base)
	if err != nil {
		t.Fatalf("sql.Open owner: %v", err)
	}
	// Keep at least 3 conns: the promoter pins ONE conn per
	// tick (advisory lock + emission writes), the
	// open/finalize lifecycle borrows a SECOND, and the test
	// harness uses a THIRD for assertions while the tick runs.
	owner.SetMaxOpenConns(4)
	owner.SetMaxIdleConns(2)

	ctx, cancel := context.WithTimeout(context.Background(), intTestDBTimeout)
	defer cancel()
	if err := owner.PingContext(ctx); err != nil {
		_ = owner.Close()
		t.Skipf("skipping: cannot reach PostgreSQL at %s: %v", intEnvPGURL, err)
	}
	schema := newPromSchemaName(t)
	if _, err := owner.ExecContext(ctx, `CREATE SCHEMA `+quotePromIdent(schema)); err != nil {
		_ = owner.Close()
		t.Fatalf("create schema %q: %v", schema, err)
	}
	// search_path must include partman so pg_partman helpers
	// resolve unqualified. Pin on every potential pool conn so
	// the harness asserts the entire suite uses the same path.
	for i := 0; i < 4; i++ {
		conn, cerr := owner.Conn(ctx)
		if cerr != nil {
			_ = owner.Close()
			t.Fatalf("pin conn for SET search_path: %v", cerr)
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf(
			`SET search_path TO %s, public, partman`, quotePromIdent(schema),
		)); err != nil {
			_ = conn.Close()
			_ = owner.Close()
			t.Fatalf("set search_path: %v", err)
		}
		_ = conn.Close()
	}
	if err := migrations.New(owner).Up(ctx); err != nil {
		_ = owner.Close()
		t.Fatalf("migrations.Up: %v", err)
	}

	cleanup := func() {
		ctx2, c2 := context.WithTimeout(context.Background(), intTestDBTimeout)
		defer c2()
		schemaPrefix := strings.ReplaceAll(schema, "_", "#_") + ".%"
		_, _ = owner.ExecContext(ctx2, `
			DELETE FROM partman.part_config
			WHERE parent_table LIKE $1 ESCAPE '#'
		`, schemaPrefix)
		_, _ = owner.ExecContext(ctx2, `DROP SCHEMA `+quotePromIdent(schema)+` CASCADE`)
		_ = owner.Close()
	}
	return &promFixture{db: owner, schema: schema, cleanup: cleanup}
}

func newPromSchemaName(t *testing.T) string {
	t.Helper()
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "ampromoter_" + hex.EncodeToString(buf[:])
}

func quotePromIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// uniquePromLockKey: top bit set so the per-test key cannot
// collide with PromoterAdvisoryLockKey (0x50524F4D4F544521).
var promLockCounter atomic.Int64

func uniquePromLockKey() int64 {
	return 0x4000000000000000 | promLockCounter.Add(1)
}

// ────────────────────────────────────────────────────────────
// Seed helpers
// ────────────────────────────────────────────────────────────

func promRandomHex(t *testing.T, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(buf)
}

// deterministicConceptFingerprint hashes a string seed so two
// callers passing the same seed get bit-identical bytes -- this
// is how we engineer reproducible Concept rows across tests.
func deterministicConceptFingerprint(seed string) []byte {
	sum := sha256.Sum256([]byte(seed))
	return sum[:]
}

// seedConcept inserts a concept row and returns its concept_id.
func seedConcept(ctx context.Context, t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	fp := deterministicConceptFingerprint(name + "-" + promRandomHex(t, 4))
	var id string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO concept (fingerprint, name, description_md)
		VALUES ($1::bytea, $2, $3)
		RETURNING concept_id::text
	`, fp, name, "# "+name+"\n\nseed").Scan(&id); err != nil {
		t.Fatalf("seed concept: %v", err)
	}
	return id
}

// seedConsolidatorRun mints a finished consolidator_run row
// returning its run_id so we can stamp ConceptVersion.producer_run_id
// (which is application-level polymorphic, see arch §5.5.2 / 0011).
func seedConsolidatorRun(ctx context.Context, t *testing.T, db *sql.DB) string {
	t.Helper()
	var id string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO consolidator_run (status, finished_at)
		VALUES ('done', now())
		RETURNING run_id::text
	`).Scan(&id); err != nil {
		t.Fatalf("seed consolidator_run: %v", err)
	}
	return id
}

// seedConceptVersion appends a consolidator-producer
// ConceptVersion with the supplied (version_index, confidence,
// support_count). Used to engineer a "candidate above threshold
// not yet promoted" or "candidate below threshold" state.
func seedConceptVersion(
	ctx context.Context, t *testing.T, db *sql.DB,
	conceptID, consolRunID string,
	versionIndex int, confidence float64, supportCount int, promoted bool,
) string {
	t.Helper()
	var band string
	switch {
	case confidence >= 0.7:
		band = "high"
	case confidence >= 0.3:
		band = "medium"
	default:
		band = "low"
	}
	var id string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO concept_version
		    (concept_id, version_index, confidence, confidence_band,
		     support_count, negative_count, producer, producer_run_id, promoted)
		VALUES ($1::uuid, $2, $3, $4::concept_band, $5, 0,
		        'consolidator'::producer, $6::uuid, $7)
		RETURNING concept_version_id::text
	`, conceptID, versionIndex, confidence, band, supportCount, consolRunID, promoted).Scan(&id); err != nil {
		t.Fatalf("seed concept_version: %v", err)
	}
	return id
}

// ────────────────────────────────────────────────────────────
// Assertion helpers
// ────────────────────────────────────────────────────────────

func mustCountPromoterRuns(ctx context.Context, t *testing.T, db *sql.DB, status string) int {
	t.Helper()
	var n int
	q := `SELECT count(*) FROM promoter_run`
	args := []any{}
	if status != "" {
		q += ` WHERE status = $1`
		args = append(args, status)
	}
	if err := db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count promoter_runs: %v", err)
	}
	return n
}

func mustCountPromotedConceptVersions(ctx context.Context, t *testing.T, db *sql.DB, conceptID string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM concept_version
		 WHERE concept_id = $1::uuid AND promoted = true
	`, conceptID).Scan(&n); err != nil {
		t.Fatalf("count promoted versions: %v", err)
	}
	return n
}

func mustReadPromotedConceptVersion(ctx context.Context, t *testing.T, db *sql.DB, conceptID string) (cvID string, versionIndex int, producer string, producerRunID string, createdAt time.Time) {
	t.Helper()
	if err := db.QueryRowContext(ctx, `
		SELECT concept_version_id::text, version_index, producer::text,
		       producer_run_id::text, created_at
		  FROM concept_version
		 WHERE concept_id = $1::uuid AND promoted = true
		 ORDER BY version_index DESC
		 LIMIT 1
	`, conceptID).Scan(&cvID, &versionIndex, &producer, &producerRunID, &createdAt); err != nil {
		t.Fatalf("read promoted concept_version: %v", err)
	}
	return
}

func mustReadEmbeddingPublishForCV(ctx context.Context, t *testing.T, db *sql.DB, cvID string) (publishID string, pointID string, modelVersion string, createdAt time.Time) {
	t.Helper()
	if err := db.QueryRowContext(ctx, `
		SELECT publish_id::text, qdrant_point_id::text,
		       embedding_model_version, created_at
		  FROM embedding_publish
		 WHERE concept_version_id = $1::uuid
		 ORDER BY created_at DESC
		 LIMIT 1
	`, cvID).Scan(&publishID, &pointID, &modelVersion, &createdAt); err != nil {
		t.Fatalf("read embedding_publish for cv %s: %v", cvID, err)
	}
	return
}

func mustReadEventChain(ctx context.Context, t *testing.T, db *sql.DB, publishID string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT event_kind::text
		  FROM embedding_publish_event
		 WHERE publish_id = $1::uuid
		 ORDER BY created_at ASC, attempt_index ASC
	`, publishID)
	if err != nil {
		t.Fatalf("read event chain: %v", err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan event kind: %v", err)
		}
		kinds = append(kinds, k)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("event chain rows.Err: %v", err)
	}
	return kinds
}

func mustReadPromoterRunRow(ctx context.Context, t *testing.T, db *sql.DB, runID string) (status string, conceptsPromoted int, startedAt, finishedAt time.Time) {
	t.Helper()
	var ft sql.NullTime
	if err := db.QueryRowContext(ctx, `
		SELECT status, concepts_promoted, started_at, finished_at
		  FROM promoter_run WHERE run_id = $1::uuid
	`, runID).Scan(&status, &conceptsPromoted, &startedAt, &ft); err != nil {
		t.Fatalf("read promoter_run: %v", err)
	}
	if ft.Valid {
		finishedAt = ft.Time
	}
	return
}

func mustCountEmbeddingPublishRows(ctx context.Context, t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM embedding_publish`).Scan(&n); err != nil {
		t.Fatalf("count embedding_publish: %v", err)
	}
	return n
}

// newPromService constructs a Service against a real DB using
// the unit-test fakes (fakeEmbedder + fakeQdrant) so the
// integration test does NOT depend on a live Qdrant.
func newPromService(t *testing.T, db *sql.DB) (*Service, *fakeEmbedder, *fakeQdrant) {
	t.Helper()
	emb := newFakeEmbedder("integration-stub@v1", 768)
	qd := newFakeQdrant()
	svc, err := New(db, emb, qd, Config{
		ConfidenceThreshold: 0.7,
		SupportThreshold:    5,
		RunInterval:         time.Second,
		TickTimeout:         intTestDBTimeout,
		CandidateBatchSize:  16,
		RetryBatchSize:      8,
		AdvisoryLockKey:     uniquePromLockKey(),
	}, silentPromLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, emb, qd
}

// silentPromLogger discards all log output for clean test runs.
func silentPromLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ────────────────────────────────────────────────────────────
// Scenario 1 — threshold flips promoted=true with §8.7.1 ordering
// ────────────────────────────────────────────────────────────
//
// Given a Concept whose latest ConceptVersion has confidence
// >= 0.7 AND support_count >= 5, and no promoted version yet:
// the Promoter MUST append a fresh ConceptVersion with
// producer='promoter' AND promoted=true, AND drive the
// embedding_publish chain through queued → vector_written →
// published. The point MUST land in the agent_memory_concept
// Qdrant collection (asserted via fakeQdrant.upsertLog).

func TestTick_thresholdFlipsPromotedTrueWithOrdering(t *testing.T) {
	fx := openPromFixture(t)
	defer fx.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), intTestDBTimeout)
	defer cancel()

	consolRun := seedConsolidatorRun(ctx, t, fx.db)
	conceptID := seedConcept(ctx, t, fx.db, "AuthRetryPattern")
	seedConceptVersion(ctx, t, fx.db, conceptID, consolRun,
		0 /*version*/, 0.82 /*confidence*/, 7 /*support*/, false /*promoted*/)

	svc, emb, qd := newPromService(t, fx.db)
	result, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if result.LockSkipped {
		t.Fatalf("LockSkipped should be false (no rival promoter)")
	}
	if result.ConceptsPromoted != 1 {
		t.Fatalf("ConceptsPromoted: got %d want 1", result.ConceptsPromoted)
	}

	// promoter_run lifecycle.
	if n := mustCountPromoterRuns(ctx, t, fx.db, "done"); n != 1 {
		t.Fatalf("promoter_run(done) count: got %d want 1", n)
	}
	status, cp, started, finished := mustReadPromoterRunRow(ctx, t, fx.db, result.RunID)
	if status != "done" {
		t.Fatalf("promoter_run.status: got %q want done", status)
	}
	if cp != 1 {
		t.Fatalf("promoter_run.concepts_promoted: got %d want 1", cp)
	}
	if finished.Before(started) {
		t.Fatalf("promoter_run.finished_at %v < started_at %v", finished, started)
	}

	// Promoted ConceptVersion (producer='promoter', promoted=true, version_index=1).
	if n := mustCountPromotedConceptVersions(ctx, t, fx.db, conceptID); n != 1 {
		t.Fatalf("promoted ConceptVersion count: got %d want 1", n)
	}
	cvID, vIdx, producer, producerRunID, cvCreated := mustReadPromotedConceptVersion(ctx, t, fx.db, conceptID)
	if producer != "promoter" {
		t.Fatalf("promoted CV.producer: got %q want promoter", producer)
	}
	if vIdx != 1 {
		t.Fatalf("promoted CV.version_index: got %d want 1 (seed was 0)", vIdx)
	}
	if producerRunID != result.RunID {
		t.Fatalf("promoted CV.producer_run_id %q does not match PromoterRun %q",
			producerRunID, result.RunID)
	}

	// EmbeddingPublish row + event chain.
	publishID, pointID, mv, epCreated := mustReadEmbeddingPublishForCV(ctx, t, fx.db, cvID)
	if mv != "integration-stub@v1" {
		t.Fatalf("EmbeddingPublish.embedding_model_version: got %q want integration-stub@v1", mv)
	}

	// §8.7.1 ordering: ConceptVersion.created_at < EmbeddingPublish.created_at.
	if !cvCreated.Before(epCreated) {
		t.Fatalf("ordering violated: ConceptVersion.created_at %v NOT strictly before EmbeddingPublish.created_at %v",
			cvCreated, epCreated)
	}

	kinds := mustReadEventChain(ctx, t, fx.db, publishID)
	wantChain := []string{"queued", "vector_written", "published"}
	if len(kinds) != len(wantChain) {
		t.Fatalf("event chain length: got %v want %v", kinds, wantChain)
	}
	for i, k := range wantChain {
		if kinds[i] != k {
			t.Fatalf("event chain[%d]: got %q want %q (full: %v)", i, kinds[i], k, kinds)
		}
	}

	// Qdrant got exactly one upsert in the concept collection
	// targeting our pointID.
	if qd.upsertCount() != 1 {
		t.Fatalf("Qdrant upsert count: got %d want 1", qd.upsertCount())
	}
	call := qd.upsertLog[0]
	if call.Collection != "agent_memory_concept" {
		t.Fatalf("Qdrant collection: got %q want agent_memory_concept", call.Collection)
	}
	if call.PointID != pointID {
		t.Fatalf("Qdrant point_id: got %q want %q (matches EmbeddingPublish)", call.PointID, pointID)
	}

	// Embedder was invoked exactly once.
	if emb.callCount() != 1 {
		t.Fatalf("embedder call count: got %d want 1", emb.callCount())
	}
}

// ────────────────────────────────────────────────────────────
// Scenario 2 — PromoterRun precedes ConceptVersion FK
// ────────────────────────────────────────────────────────────
//
// Given a fresh candidate Concept above threshold: the
// promoter_run row MUST exist (status=running) BEFORE the
// ConceptVersion(producer='promoter') row is inserted with
// producer_run_id pointing at it. We verify this by reading the
// per-row created_at: promoter_run.started_at < promoted
// concept_version.created_at, AND
// promoted_cv.producer_run_id = promoter_run.run_id.

func TestTick_promoterRunPrecedesConceptVersionInsert(t *testing.T) {
	fx := openPromFixture(t)
	defer fx.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), intTestDBTimeout)
	defer cancel()

	consolRun := seedConsolidatorRun(ctx, t, fx.db)
	conceptID := seedConcept(ctx, t, fx.db, "OrderingTest")
	seedConceptVersion(ctx, t, fx.db, conceptID, consolRun, 0, 0.9, 6, false)

	svc, _, _ := newPromService(t, fx.db)
	result, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if result.RunID == "" {
		t.Fatalf("Tick returned empty RunID")
	}

	_, _, _, producerRunID, cvCreated := mustReadPromotedConceptVersion(ctx, t, fx.db, conceptID)
	if producerRunID != result.RunID {
		t.Fatalf("CV.producer_run_id %q != PromoterRun.run_id %q", producerRunID, result.RunID)
	}

	var prStarted time.Time
	if err := fx.db.QueryRowContext(ctx, `
		SELECT started_at FROM promoter_run WHERE run_id = $1::uuid
	`, result.RunID).Scan(&prStarted); err != nil {
		t.Fatalf("read promoter_run.started_at: %v", err)
	}
	if !prStarted.Before(cvCreated) {
		t.Fatalf("ordering violated: PromoterRun.started_at %v NOT strictly before "+
			"promoted ConceptVersion.created_at %v", prStarted, cvCreated)
	}
}

// ────────────────────────────────────────────────────────────
// Scenario 3 — ConceptVersion precedes EmbeddingPublish
// ────────────────────────────────────────────────────────────
//
// Strict created_at ordering required by §8.7.1: the
// ConceptVersion(promoted=true) row's created_at MUST be
// strictly less than the EmbeddingPublish row's created_at.
// Engineered by the implementation as two separate transactions
// (rubber-duck #2 fix). This test is the canary that catches a
// regression collapsing them back into a single tx.

func TestTick_conceptVersionPrecedesEmbeddingPublish(t *testing.T) {
	fx := openPromFixture(t)
	defer fx.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), intTestDBTimeout)
	defer cancel()

	consolRun := seedConsolidatorRun(ctx, t, fx.db)
	conceptID := seedConcept(ctx, t, fx.db, "TwoTxOrderingCanary")
	seedConceptVersion(ctx, t, fx.db, conceptID, consolRun, 3, 0.78, 9, false)

	svc, _, _ := newPromService(t, fx.db)
	if _, err := svc.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	cvID, _, _, _, cvCreated := mustReadPromotedConceptVersion(ctx, t, fx.db, conceptID)
	_, _, _, epCreated := mustReadEmbeddingPublishForCV(ctx, t, fx.db, cvID)
	if !cvCreated.Before(epCreated) {
		t.Fatalf("§8.7.1 strict ordering violated: ConceptVersion.created_at %v "+
			"NOT strictly before EmbeddingPublish.created_at %v "+
			"(two-tx split regression?)", cvCreated, epCreated)
	}
}

// ────────────────────────────────────────────────────────────
// Scenario 4 — below threshold stays unpromoted
// ────────────────────────────────────────────────────────────
//
// Given a Concept whose latest ConceptVersion has confidence
// below 0.7 OR support_count below 5: the promoter MUST NOT
// promote it. We test both axes (confidence-only-below,
// support-only-below) plus the joint above-threshold control
// that DOES get promoted, all in one Tick.

func TestTick_belowThresholdStaysUnpromoted(t *testing.T) {
	fx := openPromFixture(t)
	defer fx.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), intTestDBTimeout)
	defer cancel()

	consolRun := seedConsolidatorRun(ctx, t, fx.db)

	// Three candidates: two below-threshold, one above.
	lowConfidence := seedConcept(ctx, t, fx.db, "LowConfidence")
	seedConceptVersion(ctx, t, fx.db, lowConfidence, consolRun, 0, 0.69, 100, false)

	lowSupport := seedConcept(ctx, t, fx.db, "LowSupport")
	seedConceptVersion(ctx, t, fx.db, lowSupport, consolRun, 0, 0.99, 4, false)

	above := seedConcept(ctx, t, fx.db, "AboveThreshold")
	seedConceptVersion(ctx, t, fx.db, above, consolRun, 0, 0.71, 5, false)

	svc, _, _ := newPromService(t, fx.db)
	result, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if result.ConceptsPromoted != 1 {
		t.Fatalf("ConceptsPromoted: got %d want 1 (only AboveThreshold)",
			result.ConceptsPromoted)
	}

	if n := mustCountPromotedConceptVersions(ctx, t, fx.db, lowConfidence); n != 0 {
		t.Fatalf("LowConfidence got promoted (%d rows); §7.8 violation", n)
	}
	if n := mustCountPromotedConceptVersions(ctx, t, fx.db, lowSupport); n != 0 {
		t.Fatalf("LowSupport got promoted (%d rows); §7.8 violation", n)
	}
	if n := mustCountPromotedConceptVersions(ctx, t, fx.db, above); n != 1 {
		t.Fatalf("AboveThreshold did NOT get promoted; got %d rows want 1", n)
	}

	// Only one embedding_publish row in the schema (the
	// above-threshold concept's). LowConfidence and LowSupport
	// must NOT produce one.
	if n := mustCountEmbeddingPublishRows(ctx, t, fx.db); n != 1 {
		t.Fatalf("embedding_publish rows: got %d want 1 (one promoted concept)", n)
	}
}

// ────────────────────────────────────────────────────────────
// Scenario 5 — Consolidator does NOT write EmbeddingPublish
// ────────────────────────────────────────────────────────────
//
// Sole-writer rule (architecture §7.7): only the Concept
// Promoter writes embedding_publish rows for ConceptVersion
// targets. We verify this by seeding a Consolidator-producer
// ConceptVersion above threshold WITHOUT running the promoter,
// then asserting embedding_publish has zero rows. Then we run
// the promoter and assert it (and only it) produces the row.

func TestTick_consolidatorDoesNotWriteEmbeddingPublish(t *testing.T) {
	fx := openPromFixture(t)
	defer fx.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), intTestDBTimeout)
	defer cancel()

	consolRun := seedConsolidatorRun(ctx, t, fx.db)
	conceptID := seedConcept(ctx, t, fx.db, "SoleWriterTest")
	seedConceptVersion(ctx, t, fx.db, conceptID, consolRun, 0, 0.95, 50, false)

	// PRE-condition: even though a Consolidator-producer
	// ConceptVersion is above threshold, no EmbeddingPublish
	// row exists -- the consolidator never writes them.
	if n := mustCountEmbeddingPublishRows(ctx, t, fx.db); n != 0 {
		t.Fatalf("PRE: embedding_publish rows: got %d want 0 (consolidator must NOT write)", n)
	}

	// Now run the Promoter. It is the sole writer of
	// embedding_publish for concept_version targets.
	svc, _, _ := newPromService(t, fx.db)
	result, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if result.ConceptsPromoted != 1 {
		t.Fatalf("Promoter ConceptsPromoted: got %d want 1", result.ConceptsPromoted)
	}

	// POST: the row exists, and exactly one (no double-write).
	if n := mustCountEmbeddingPublishRows(ctx, t, fx.db); n != 1 {
		t.Fatalf("POST: embedding_publish rows: got %d want 1 (Promoter must write exactly one)", n)
	}

	// And the row's target is a concept_version, not a node.
	var nodeID, cvID sql.NullString
	if err := fx.db.QueryRowContext(ctx, `
		SELECT node_id::text, concept_version_id::text FROM embedding_publish
	`).Scan(&nodeID, &cvID); err != nil {
		t.Fatalf("read embedding_publish target: %v", err)
	}
	if nodeID.Valid {
		t.Fatalf("Promoter wrote node-target embedding_publish (node_id=%v); should be concept-target only",
			nodeID.String)
	}
	if !cvID.Valid {
		t.Fatalf("Promoter wrote embedding_publish with NULL concept_version_id; §8.7.1 violation")
	}
}

// ────────────────────────────────────────────────────────────
// Scenario 5 (strong form, evaluator-2 finding #4) —
// drive the REAL consolidator, not a manually-seeded CV
// ────────────────────────────────────────────────────────────
//
// Iteration-1 implemented the "Consolidator never writes
// EmbeddingPublish" scenario by hand-seeding a CV with
// producer='consolidator' and then asserting embedding_publish
// stays empty. That covers the row-level invariant but does
// not catch a future regression where the Consolidator itself
// starts writing publish rows.
//
// This stronger variant constructs a REAL
// `internal/consolidator.Service`, feeds it Episodes via the
// same seed pattern the consolidator suite uses, runs
// `consolidator.Tick(ctx)` end-to-end and then asserts that
// (a) the Consolidator successfully crystallised a Concept +
// ConceptVersion, AND (b) the embedding_publish table is
// STILL empty. Only the subsequent Promoter Tick produces the
// row. This pins the sole-writer architecture invariant
// (arch §7.7) against the consolidator's actual write path —
// not a synthetic row that bypasses it.

func TestTick_realConsolidatorDoesNotWriteEmbeddingPublish(t *testing.T) {
	fx := openPromFixture(t)
	defer fx.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), intTestDBTimeout)
	defer cancel()

	// Seed an Episode mass that will trip the
	// consolidator's threshold. Reuses the seed-helper
	// shape from internal/consolidator/service_integration_test.go.
	repoID := promSeedRepo(ctx, t, fx.db, "real-consol-scenario-5")
	contextID := promSeedRecallContext(ctx, t, fx.db, repoID)
	nodeID := promSeedNode(ctx, t, fx.db, repoID,
		deterministicConceptFingerprint("real-consol-node-"+promRandomHex(t, 4)),
		"node-promoter-scenario5")
	const supportN = 10
	for i := 0; i < supportN; i++ {
		_ = promSeedEpisode(ctx, t, fx.db, repoID, contextID, nodeID)
	}

	// Run the REAL consolidator with a per-test lock key so
	// we never collide with the promoter's lock key (which is
	// in a different bit-prefix anyway, but explicit is
	// better than tribal knowledge).
	consolSvc, err := consolidator.New(fx.db, consolidator.Config{
		Threshold:       supportN,
		RunInterval:     time.Second,
		TickTimeout:     intTestDBTimeout,
		AdvisoryLockKey: uniqueConsolLockKey(),
	}, silentPromLogger())
	if err != nil {
		t.Fatalf("consolidator.New: %v", err)
	}
	consolRes, err := consolSvc.Tick(ctx)
	if err != nil {
		t.Fatalf("consolidator.Tick: %v", err)
	}
	if consolRes.LockSkipped {
		t.Fatalf("consolidator unexpectedly lock-skipped: %+v", consolRes)
	}
	if consolRes.VersionsAppended < 1 {
		t.Fatalf("consolidator should have appended at least 1 ConceptVersion; got %+v",
			consolRes)
	}

	// PRE: even though the consolidator just crystallised
	// a Concept + ConceptVersion above threshold, NO
	// embedding_publish row exists. The Consolidator is NOT
	// allowed to write to that table — only the Promoter is.
	// This is the assertion that catches a future regression
	// where someone wires a publish INSERT into the
	// consolidator's emission phase.
	if n := mustCountEmbeddingPublishRows(ctx, t, fx.db); n != 0 {
		t.Fatalf("PRE: consolidator wrote %d embedding_publish row(s); arch §7.7 sole-writer rule violated",
			n)
	}

	// Now the Promoter picks up the consolidator-produced
	// CV (which has producer='consolidator', promoted=false,
	// support_count=supportN, confidence>=0.7 — i.e. a
	// genuine §7.8 candidate) and turns it into a
	// promoter-producer promoted=true CV with the full
	// §8.7.1 publish chain.
	promSvc, _, qd := newPromService(t, fx.db)
	promRes, err := promSvc.Tick(ctx)
	if err != nil {
		t.Fatalf("promoter.Tick: %v", err)
	}
	if promRes.ConceptsPromoted != 1 {
		t.Fatalf("promoter ConceptsPromoted: got %d want 1", promRes.ConceptsPromoted)
	}

	// POST: exactly one embedding_publish row, and it
	// targets a concept_version (not a node).
	if n := mustCountEmbeddingPublishRows(ctx, t, fx.db); n != 1 {
		t.Fatalf("POST: embedding_publish rows: got %d want 1", n)
	}
	var nodeIDCol, cvIDCol sql.NullString
	if err := fx.db.QueryRowContext(ctx, `
		SELECT node_id::text, concept_version_id::text FROM embedding_publish
	`).Scan(&nodeIDCol, &cvIDCol); err != nil {
		t.Fatalf("read embedding_publish target: %v", err)
	}
	if nodeIDCol.Valid {
		t.Fatalf("Promoter wrote node-target embedding_publish (node_id=%v); should be concept-target",
			nodeIDCol.String)
	}
	if !cvIDCol.Valid {
		t.Fatalf("Promoter wrote embedding_publish with NULL concept_version_id; §8.7.1 violation")
	}
	if qd.upsertCount() != 1 {
		t.Fatalf("expected 1 Qdrant upsert; got %d", qd.upsertCount())
	}
}

// ────────────────────────────────────────────────────────────
// Scenario 6 (evaluator-2 finding #1) —
// orphan-recovery phase end-to-end against a live PostgreSQL
// ────────────────────────────────────────────────────────────
//
// Inserts a promoted=true, producer='promoter' ConceptVersion
// WITHOUT a sibling embedding_publish row — exactly the
// crash-recovery state a prior tick would have left if it had
// committed tx1 and died before tx2. Asserts that the next
// Tick's orphan-recovery phase finds the row, runs tx2 + the
// §9.6a publish chain, and ends with OrphansRecovered=1 +
// ConceptsPromoted=1 + a terminal `published` event.

func TestTick_orphanedPromotedConceptVersionIsRecoveredOnNextTick(t *testing.T) {
	fx := openPromFixture(t)
	defer fx.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), intTestDBTimeout)
	defer cancel()

	// First, seed a promoter_run row so the orphan's
	// producer_run_id has a real UUID to point at (the FK
	// is application-polymorphic, see §0011 line 18 — so
	// any UUID works at the schema level, but using a real
	// promoter_run row makes the test reflect the actual
	// crash scenario).
	var promRunID string
	if err := fx.db.QueryRowContext(ctx, `
		INSERT INTO promoter_run (status, finished_at)
		VALUES ('failed', now())
		RETURNING run_id::text
	`).Scan(&promRunID); err != nil {
		t.Fatalf("seed orphan-producing promoter_run: %v", err)
	}
	conceptID := seedConcept(ctx, t, fx.db, "OrphanRecoveryConcept")
	// Manually insert a promoter-producer, promoted=true
	// ConceptVersion with NO sibling embedding_publish row —
	// the orphan state.
	var orphanCVID string
	if err := fx.db.QueryRowContext(ctx, `
		INSERT INTO concept_version
		    (concept_id, version_index, confidence, confidence_band,
		     support_count, negative_count, producer, producer_run_id, promoted)
		VALUES ($1::uuid, 0, 0.9, 'high'::concept_band,
		        7, 0, 'promoter'::producer, $2::uuid, true)
		RETURNING concept_version_id::text
	`, conceptID, promRunID).Scan(&orphanCVID); err != nil {
		t.Fatalf("seed orphan ConceptVersion: %v", err)
	}

	// PRE: the orphan exists, no publish row, no events.
	if n := mustCountEmbeddingPublishRows(ctx, t, fx.db); n != 0 {
		t.Fatalf("PRE: embedding_publish rows: got %d want 0", n)
	}

	svc, emb, qd := newPromService(t, fx.db)
	res, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.OrphansPending != 1 {
		t.Fatalf("OrphansPending: got %d want 1", res.OrphansPending)
	}
	if res.OrphansRecovered != 1 {
		t.Fatalf("OrphansRecovered: got %d want 1", res.OrphansRecovered)
	}
	if res.ConceptsPromoted != 1 {
		t.Fatalf("ConceptsPromoted: got %d want 1 (orphan-recovered chain finished)",
			res.ConceptsPromoted)
	}
	if emb.callCount() != 1 {
		t.Fatalf("expected 1 embedder call; got %d", emb.callCount())
	}
	if qd.upsertCount() != 1 {
		t.Fatalf("expected 1 Qdrant upsert; got %d", qd.upsertCount())
	}

	// POST: a single embedding_publish row that targets the
	// recovered orphan CV, with a terminal `published` event.
	if n := mustCountEmbeddingPublishRows(ctx, t, fx.db); n != 1 {
		t.Fatalf("POST: embedding_publish rows: got %d want 1", n)
	}
	publishID, _, _, _ := mustReadEmbeddingPublishForCV(ctx, t, fx.db, orphanCVID)
	chain := mustReadEventChain(ctx, t, fx.db, publishID)
	if len(chain) == 0 || chain[len(chain)-1] != "published" {
		t.Fatalf("event chain terminal: got %v want last='published'", chain)
	}

	// SECOND TICK: with the orphan recovered, the scan
	// should return zero rows — invariant on the
	// `NOT EXISTS embedding_publish` filter.
	res2, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if res2.OrphansPending != 0 {
		t.Fatalf("second tick OrphansPending: got %d want 0 (orphan was recovered)",
			res2.OrphansPending)
	}
	if res2.OrphansRecovered != 0 {
		t.Fatalf("second tick OrphansRecovered: got %d want 0", res2.OrphansRecovered)
	}
}

// ────────────────────────────────────────────────────────────
// Seed helpers reused from internal/consolidator/service_integration_test.go.
// These are duplicated here (rather than exported from the
// consolidator package) because the consolidator's test
// helpers are unexported and adding a public seed surface to
// production code purely for cross-package tests would be a
// worse trade. The SQL is identical to consolidator's
// seedRepo / seedNode / seedRecallContext / seedEpisode at
// lines 166-244 of that file; keep them in lock-step.
// ────────────────────────────────────────────────────────────

func promSeedRepo(ctx context.Context, t *testing.T, db *sql.DB, slug string) string {
	t.Helper()
	var repoID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO repo (url, default_branch, current_head_sha, language_hints)
		VALUES ($1, 'main', 'deadbeef', ARRAY['go']::text[])
		RETURNING repo_id::text
	`, "https://example.test/"+slug+"-"+promRandomHex(t, 4)).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	return repoID
}

func promSeedNode(ctx context.Context, t *testing.T, db *sql.DB, repoID string, fp []byte, label string) string {
	t.Helper()
	var nodeID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO node (repo_id, kind, canonical_signature, fingerprint, from_sha)
		VALUES ($1::uuid, 'method', $2, $3::bytea, 'deadbeef')
		RETURNING node_id::text
	`, repoID, label+"-"+promRandomHex(t, 4), fp).Scan(&nodeID); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	return nodeID
}

func promSeedRecallContext(ctx context.Context, t *testing.T, db *sql.DB, repoID string) string {
	t.Helper()
	var ctxID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO recall_context_log
		    (repo_id, verb, query_json, reranker_model_version)
		VALUES ($1::uuid, 'recall'::verb, '{}'::jsonb, 'v-test')
		RETURNING context_id::text
	`, repoID).Scan(&ctxID); err != nil {
		t.Fatalf("seed recall_context_log: %v", err)
	}
	return ctxID
}

func promSeedEpisode(ctx context.Context, t *testing.T, db *sql.DB, repoID, contextID, nodeID string) string {
	t.Helper()
	var epID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO episode
		    (episode_group_id, repo_id, session_id, trace_id, kind,
		     context_id, action, outcome)
		VALUES (gen_random_uuid(), $1::uuid, $2, $3, 'agent'::episode_kind,
		        $4::uuid, '{"op":"test"}'::jsonb, 'success'::outcome)
		RETURNING episode_id::text
	`, repoID, "sess-"+promRandomHex(t, 4), "trace-"+promRandomHex(t, 4), contextID).Scan(&epID); err != nil {
		t.Fatalf("seed episode: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO observation
		    (episode_id, role, node_id)
		VALUES ($1::uuid, 'node_hit'::observation_role, $2::uuid)
	`, epID, nodeID); err != nil {
		t.Fatalf("seed observation: %v", err)
	}
	return epID
}

// uniqueConsolLockKey: same top-bit-set pattern as
// uniquePromLockKey but a different bit so the per-test
// consolidator lock CANNOT collide with the per-test promoter
// lock under a single test binary.
var consolLockCounter atomic.Int64

func uniqueConsolLockKey() int64 {
	return 0x2000000000000000 | consolLockCounter.Add(1)
}

// ────────────────────────────────────────────────────────────
// Scenario — unpinned-HTTP end-to-end (evaluator-3 finding #2)
// ────────────────────────────────────────────────────────────
//
// Given a Concept above threshold AND an embedder that returns
// model_version="" until the FIRST Embed() succeeds (mirroring
// the cmd/concept-promoter httpEmbedder when
// AGENT_MEMORY_EMBEDDER_MODEL_VERSION is unset): the live-PG
// Tick MUST still drive the candidate to published, with the
// embedding_publish row's embedding_model_version equal to the
// model the bootstrap Embed resolved.
//
// Prior to evaluator-3 finding #1's fix this would fail at the
// real PG NOT NULL check on embedding_publish.embedding_model_version
// because insertPublishAndQueued sampled ModelVersion() BEFORE
// the bootstrap Embed had ever been called.
func TestTick_unpinnedHTTPEmbedderEndToEndPromotesAgainstLivePG(t *testing.T) {
	fx := openPromFixture(t)
	defer fx.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), intTestDBTimeout)
	defer cancel()

	consolRun := seedConsolidatorRun(ctx, t, fx.db)
	conceptID := seedConcept(ctx, t, fx.db, "UnpinnedHTTPBootstrap")
	seedConceptVersion(ctx, t, fx.db, conceptID, consolRun,
		0 /*version*/, 0.91 /*confidence*/, 9 /*support*/, false /*promoted*/)

	// lazyEmbedder mirrors cmd/concept-promoter/main.go's
	// httpEmbedder in UNPINNED mode: ModelVersion() returns
	// "" until the first Embed() call caches the upstream
	// model_version.
	lz := newLazyEmbedder("upstream-bge-large@2024-11", 768)
	qd := newFakeQdrant()
	svc, err := New(fx.db, lz, qd, Config{
		ConfidenceThreshold: 0.7,
		SupportThreshold:    5,
		RunInterval:         time.Second,
		TickTimeout:         intTestDBTimeout,
		CandidateBatchSize:  16,
		RetryBatchSize:      8,
		AdvisoryLockKey:     uniquePromLockKey(),
	}, silentPromLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v (a failure here means evaluator-3 finding #1 has REGRESSED end-to-end against live PG — the unpinned HTTP embedder configuration cannot promote)", err)
	}
	if result.LockSkipped {
		t.Fatalf("LockSkipped should be false")
	}
	if result.ConceptsPromoted != 1 {
		t.Fatalf("ConceptsPromoted: got %d want 1 (unpinned HTTP bootstrap should reach published on first tick)", result.ConceptsPromoted)
	}

	cvID, _, producer, _, _ := mustReadPromotedConceptVersion(ctx, t, fx.db, conceptID)
	if producer != "promoter" {
		t.Fatalf("promoted CV.producer: got %q want promoter", producer)
	}

	// THE assertion: embedding_publish.embedding_model_version
	// was written with the lazy-resolved model — not "" — and
	// the value matches what the embedder's first Embed call
	// cached.
	publishID, _, mv, _ := mustReadEmbeddingPublishForCV(ctx, t, fx.db, cvID)
	if mv != "upstream-bge-large@2024-11" {
		t.Fatalf("embedding_publish.embedding_model_version: got %q want %q (the bootstrap Embed should have resolved the model_version BEFORE insertPublishAndQueued sampled it)", mv, "upstream-bge-large@2024-11")
	}

	kinds := mustReadEventChain(ctx, t, fx.db, publishID)
	wantChain := []string{"queued", "vector_written", "published"}
	if len(kinds) != len(wantChain) {
		t.Fatalf("event chain length: got %v want %v", kinds, wantChain)
	}
	for i, k := range wantChain {
		if kinds[i] != k {
			t.Fatalf("event chain[%d]: got %q want %q (full: %v)", i, kinds[i], k, kinds)
		}
	}

	if qd.upsertCount() != 1 {
		t.Fatalf("Qdrant upsert count: got %d want 1", qd.upsertCount())
	}

	// Single Embed call: ensureModelReady embeds for
	// bootstrap, runAttempt reuses publishState.prefetchedVec.
	if lz.callCount() != 1 {
		t.Fatalf("Embed call count: got %d want 1 (>1 means runAttempt did NOT reuse the prefetched vec — the bootstrap embed and runAttempt embed are duplicating work)", lz.callCount())
	}
}
