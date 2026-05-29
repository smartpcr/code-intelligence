package rule_engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"
	_ "github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
)

// envSQLStoreURL is the libpq DSN the rule_engine SQLStore
// live tests connect to. Matches the same `CLEAN_CODE_PG_URL`
// the storage package, the policy/keys package, AND the
// steward SQLStore tests all consume, so a single
// `export CLEAN_CODE_PG_URL=...` turns on every live test
// path at once.
const envSQLStoreURL = "CLEAN_CODE_PG_URL"

// ruleEngineTestSchemaName is the ISOLATED PostgreSQL schema
// the rule_engine SQLStore live tests own. Kept distinct
// from `clean_code` (production) AND from the schemas owned
// by the storage / policy/keys / steward SQLStore tests so
// every live test suite runs without racing on schema
// teardown.
const ruleEngineTestSchemaName = "clean_code_rule_engine_test"

// ruleEngineSchemaPrep materialises ONLY the columns the
// rule_engine SQLStore touches:
//
//   - `commit(repo_id, sha, parent_sha)` for ParentSHA
//   - `scope_binding(scope_id, scope_kind, canonical_signature)`
//   - `metric_sample(sample_id, repo_id, sha, scope_id,
//     metric_kind, metric_version, value, degraded)`
//   - `rule(rule_id, version, pack_id, predicate_dsl,
//     severity_default, description_md)`
//   - `threshold(threshold_id, metric_kind, scope_kind, op, value)`
//   - `policy_version(...)`, `evaluation_run(...)`,
//     `evaluation_verdict(...)`, `finding(...)`
//
// We do NOT replay the real migration; the simplified
// schema is enough to exercise the Store interface end-to-end
// and keeps the test independent of migration ordering.
const ruleEngineSchemaPrep = `
DROP SCHEMA IF EXISTS %[1]s CASCADE;
CREATE SCHEMA %[1]s;

CREATE TYPE %[1]s.rule_severity AS ENUM ('info', 'warn', 'block');
CREATE TYPE %[1]s.threshold_op  AS ENUM ('lt', 'le', 'gt', 'ge', 'eq', 'ne');
CREATE TYPE %[1]s.eval_caller   AS ENUM ('eval_gate', 'batch_refresh', 'reconciler_replay');
CREATE TYPE %[1]s.eval_verdict  AS ENUM ('pass', 'warn', 'block');
CREATE TYPE %[1]s.finding_delta AS ENUM ('new', 'newly_failing', 'unchanged', 'resolved');

CREATE TABLE %[1]s.commit (
    repo_id      uuid        NOT NULL,
    sha          text        NOT NULL,
    parent_sha   text,
    scan_status  text        NOT NULL DEFAULT 'scanned',
    committed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repo_id, sha)
);

CREATE TABLE %[1]s.scope_binding (
    scope_id            uuid PRIMARY KEY,
    scope_kind          text NOT NULL,
    canonical_signature text NOT NULL
);

CREATE TABLE %[1]s.metric_sample (
    sample_id       uuid PRIMARY KEY,
    repo_id         uuid NOT NULL,
    sha             text NOT NULL,
    scope_id        uuid NOT NULL REFERENCES %[1]s.scope_binding(scope_id),
    metric_kind     text NOT NULL,
    metric_version  int  NOT NULL,
    value           double precision,
    degraded        boolean NOT NULL DEFAULT false,
    pack            text NOT NULL DEFAULT 'solid',
    source          text NOT NULL DEFAULT 'computed',
    degraded_reason text
);

-- Active-row pointer table per migrations/0002_measurement.up.sql
-- lines 506-552. The rule_engine SQLStore JOINs through this
-- table so retracted/inactive samples (which point at
-- newer sample_id values) cannot trigger findings.
CREATE TABLE %[1]s.metric_sample_active (
    repo_id        uuid    NOT NULL,
    sha            text    NOT NULL,
    scope_id       uuid    NOT NULL,
    metric_kind    text    NOT NULL,
    metric_version int     NOT NULL,
    sample_id      uuid    NOT NULL REFERENCES %[1]s.metric_sample(sample_id),
    PRIMARY KEY (repo_id, sha, scope_id, metric_kind, metric_version)
);

-- Retraction tombstone table per migrations/0002_measurement.up.sql
-- lines 448-475. The Stage 3.4 SHA-pinned readers
-- (mgmt.read.metric_sample, mgmt.read.metric_samples, eval.gate)
-- LEFT JOIN through this table and filter WHERE mr.sample_id IS
-- NULL so a retracted sample never feeds the rule engine even
-- when metric_sample_active still points at it (DELETE on
-- metric_sample_active is REVOKEd per tech-spec Sec 7.2 line
-- 1248). The UNIQUE on sample_id mirrors the architecture's
-- "double-retract is a no-op" invariant.
CREATE TABLE %[1]s.metric_retraction (
    retraction_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    sample_id     uuid NOT NULL UNIQUE REFERENCES %[1]s.metric_sample(sample_id),
    reason        text NOT NULL,
    appended_by   text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE %[1]s.rule (
    rule_id          text                       NOT NULL,
    version          integer                    NOT NULL,
    pack_id          text                       NOT NULL,
    predicate_dsl    text                       NOT NULL,
    severity_default %[1]s.rule_severity        NOT NULL,
    description_md   text                       NOT NULL,
    created_at       timestamptz                NOT NULL DEFAULT now(),
    PRIMARY KEY (rule_id, version)
);

CREATE TABLE %[1]s.threshold (
    threshold_id uuid PRIMARY KEY,
    metric_kind  text NOT NULL,
    scope_kind   text NOT NULL,
    op           %[1]s.threshold_op NOT NULL,
    value        double precision NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE %[1]s.policy_version (
    policy_version_id uuid PRIMARY KEY,
    name              text NOT NULL,
    rule_refs         jsonb NOT NULL,
    threshold_refs    jsonb NOT NULL,
    refactor_weights  jsonb NOT NULL,
    signature         bytea NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE %[1]s.evaluation_run (
    evaluation_run_id uuid PRIMARY KEY,
    repo_id           uuid NOT NULL,
    sha               text NOT NULL,
    policy_version_id uuid NOT NULL,
    caller            %[1]s.eval_caller NOT NULL,
    scope_id          uuid NULL,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE %[1]s.evaluation_verdict (
    verdict_id        uuid PRIMARY KEY,
    evaluation_run_id uuid NOT NULL REFERENCES %[1]s.evaluation_run(evaluation_run_id),
    verdict           %[1]s.eval_verdict NOT NULL,
    degraded          boolean NOT NULL DEFAULT false,
    degraded_reason   text,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE %[1]s.finding (
    finding_id        uuid PRIMARY KEY,
    evaluation_run_id uuid NOT NULL REFERENCES %[1]s.evaluation_run(evaluation_run_id),
    repo_id           uuid NOT NULL,
    sha               text NOT NULL,
    scope_id          uuid NOT NULL,
    rule_id           text NOT NULL,
    rule_version      integer NOT NULL,
    policy_version_id uuid NOT NULL,
    metric_sample_ids jsonb NOT NULL,
    severity          %[1]s.rule_severity NOT NULL,
    delta             %[1]s.finding_delta NOT NULL,
    explanation_md    text NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now()
);
`

// openLiveDB connects to the libpq URL in CLEAN_CODE_PG_URL,
// rebuilds the test schema, and returns a handle. Skips the
// test when the env var is unset.
func openLiveDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(envSQLStoreURL))
	if dsn == "" {
		t.Skipf("%s unset; skipping rule_engine SQLStore live test", envSQLStoreURL)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("postgres not reachable at %s: %v", envSQLStoreURL, err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(ruleEngineSchemaPrep, ruleEngineTestSchemaName)); err != nil {
		t.Fatalf("preparing schema %s: %v", ruleEngineTestSchemaName, err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, ruleEngineTestSchemaName)); err != nil {
			t.Logf("cleanup: drop schema: %v", err)
		}
		_ = db.Close()
	})
	return db
}

// TestSQLStore_LiveRoundTrip exercises the full
// happy-path engine -> SQLStore -> PostgreSQL flow:
//
//  1. Seed commit + scope + metric_sample rows.
//  2. Run [Engine.RunBatch] against the SQLStore.
//  3. Assert ONE evaluation_run + ONE evaluation_verdict +
//     N finding rows landed in the schema.
//
// Verifies the [Store.AppendEvaluation] single-transaction
// contract end-to-end (run + verdict + findings either all
// commit or none do) and the
// [Store.WithEvaluationLock] envelope under
// `pg_advisory_xact_lock`.
func TestSQLStore_LiveRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode; skipping live PG test")
	}
	db := openLiveDB(t)
	schema := ruleEngineTestSchemaName

	// --- seed test data ---

	repoID := uuid.Must(uuid.NewV4())
	sha := "0000000000000000000000000000000000000001"
	parentSHA := "0000000000000000000000000000000000000000"
	scopeID := uuid.Must(uuid.NewV4())
	scopeSignature := "src/Service.cs::Service"
	thresholdID := uuid.Must(uuid.NewV4())
	policyID := uuid.Must(uuid.NewV4())
	sampleID := uuid.Must(uuid.NewV4())
	ctx := context.Background()

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, fmt.Sprintf(q, schema), args...); err != nil {
			t.Fatalf("seed (%q): %v", q, err)
		}
	}

	// Commit (root + child so ParentSHA is exercised).
	exec(`INSERT INTO %s.commit (repo_id, sha) VALUES ($1, $2)`, repoID.String(), parentSHA)
	exec(`INSERT INTO %s.commit (repo_id, sha, parent_sha) VALUES ($1, $2, $3)`, repoID.String(), sha, parentSHA)
	// Scope binding.
	exec(`INSERT INTO %s.scope_binding (scope_id, scope_kind, canonical_signature)
	      VALUES ($1, $2, $3)`, scopeID.String(), "class", scopeSignature)
	// Metric sample that will trigger lcom4 > 5.
	exec(`INSERT INTO %s.metric_sample (sample_id, repo_id, sha, scope_id, metric_kind, metric_version, value, degraded, pack, source, degraded_reason)
	      VALUES ($1, $2, $3, $4, 'lcom4', 1, 7.0, false, 'solid', 'computed', NULL)`,
		sampleID.String(), repoID.String(), sha, scopeID.String())
	// Active-row pointer (G3/C2 contract from migrations/0002):
	// SQLStore.ListMetricSamples JOINs through metric_sample_active,
	// so this row MUST exist for the sample to be visible to
	// the rule engine. A retracted sample would point sample_id
	// at a different uuid here.
	exec(`INSERT INTO %s.metric_sample_active (repo_id, sha, scope_id, metric_kind, metric_version, sample_id)
	      VALUES ($1, $2, $3, 'lcom4', 1, $4)`,
		repoID.String(), sha, scopeID.String(), sampleID.String())
	// Threshold (metric_kind=lcom4, scope_kind=class, op=gt, value=5).
	exec(`INSERT INTO %s.threshold (threshold_id, metric_kind, scope_kind, op, value)
	      VALUES ($1, 'lcom4', 'class', 'gt', 5)`, thresholdID.String())
	// Rule.
	predicate := fmt.Sprintf("threshold('%s')", thresholdID.String())
	exec(`INSERT INTO %s.rule (rule_id, version, pack_id, predicate_dsl, severity_default, description_md)
	      VALUES ('solid.srp.lcom4_high', 1, 'solid', $1, 'block', 'LCOM4 too high')`, predicate)
	// PolicyVersion -- minimal JSONB payloads matching steward's PublishRequest shape.
	exec(`INSERT INTO %s.policy_version (policy_version_id, name, rule_refs, threshold_refs, refactor_weights, signature)
	      VALUES ($1, 'live-test', $2::jsonb, $3::jsonb, '{}'::jsonb, ''::bytea)`,
		policyID.String(),
		`[{"rule_id":"solid.srp.lcom4_high","version":1}]`,
		fmt.Sprintf(`[{"threshold_id":"%s"}]`, thresholdID.String()),
	)

	// --- compose engine + run ---

	stewardStore, err := steward.NewSQLStoreWithSchema(db, schema)
	if err != nil {
		t.Fatalf("steward.NewSQLStoreWithSchema: %v", err)
	}
	store, err := NewSQLStore(SQLStoreConfig{DB: db, Schema: schema, Steward: stewardStore, WalWriter: newTestWALWriter(t)})
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}
	engine, err := New(Config{Store: store})
	if err != nil {
		t.Fatalf("rule_engine.New: %v", err)
	}

	// Sanity-check raw store reads.
	if pid, ok, err := store.ParentSHA(ctx, repoID, sha); err != nil || !ok || pid != parentSHA {
		t.Fatalf("ParentSHA: got=(%q,%v,%v); want=(%s,true,nil)", pid, ok, err, parentSHA)
	}
	samples, err := store.ListMetricSamples(ctx, repoID, sha, nil)
	if err != nil {
		t.Fatalf("ListMetricSamples: %v", err)
	}
	if len(samples) != 1 || samples[0].MetricKind != "lcom4" {
		t.Fatalf("ListMetricSamples: %+v", samples)
	}

	// Run the engine -- exercises advisory lock + tx
	// + AppendEvaluation under load.
	result, err := engine.RunBatch(ctx, repoID, sha, policyID)
	if err != nil {
		t.Fatalf("Engine.RunBatch: %v", err)
	}
	if result.Verdict != VerdictBlock {
		t.Fatalf("Verdict: got=%s; want=%s", result.Verdict, VerdictBlock)
	}
	if len(result.FindingIDs) != 1 {
		t.Fatalf("FindingIDs: got=%d; want=1", len(result.FindingIDs))
	}

	// --- assert rows landed ---

	var runRows, verdictRows, findingRows int
	if err := db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT count(*) FROM %s.evaluation_run WHERE evaluation_run_id = $1`, schema),
		result.EvaluationRunID.String()).Scan(&runRows); err != nil {
		t.Fatalf("count evaluation_run: %v", err)
	}
	if runRows != 1 {
		t.Fatalf("evaluation_run count: got=%d; want=1", runRows)
	}
	if err := db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT count(*) FROM %s.evaluation_verdict WHERE verdict_id = $1`, schema),
		result.EvaluationVerdictID.String()).Scan(&verdictRows); err != nil {
		t.Fatalf("count evaluation_verdict: %v", err)
	}
	if verdictRows != 1 {
		t.Fatalf("evaluation_verdict count: got=%d; want=1", verdictRows)
	}
	if err := db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT count(*) FROM %s.finding WHERE evaluation_run_id = $1`, schema),
		result.EvaluationRunID.String()).Scan(&findingRows); err != nil {
		t.Fatalf("count finding: %v", err)
	}
	if findingRows != 1 {
		t.Fatalf("finding count: got=%d; want=1", findingRows)
	}

	// --- now drive the Worker over a buffered channel ---
	// Verifies the post-scan dispatcher hand-off composes
	// cleanly against the live SQLStore (uses
	// StewardActivationReader against a noop steward that
	// returns ok=false -> worker should INFO-log and skip).
	events := make(chan ScanEvent, 1)
	logger := slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
	worker, err := NewWorker(WorkerConfig{
		Engine:     engine,
		Activation: NewStaticActivation(uuid.Nil), // no active policy
		Events:     events,
		Logger:     logger,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	wctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.Run(wctx) }()
	events <- ScanEvent{RepoID: repoID, SHA: sha}
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("worker.Run: %v", err)
	}
}

// TestSQLStore_ListMetricSamples_FiltersRetracted pins
// iter 2 fix #2 (workstream brief Stage 3.4 verbatim):
// SHA-pinned readers (mgmt.read.metric_sample,
// mgmt.read.metric_samples, eval.gate) MUST filter the
// retracted sample out via a `metric_retraction` join.
//
// Before iter 2 the SQLStore.ListMetricSamples query
// joined only `metric_sample_active x metric_sample x
// scope_binding`. The DELETE on `metric_sample_active`
// is REVOKEd per tech-spec Sec 7.2 line 1248, so a retract
// without a follow-up rescan leaves the active pointer
// in place -- the rule_engine would evaluate the retracted
// sample. The fix adds a LEFT JOIN to `metric_retraction`
// with `WHERE mr.sample_id IS NULL` so retracted samples
// are filtered.
//
// The test:
//
//  1. Seeds a `metric_sample` row and an active pointer.
//  2. Asserts ListMetricSamples returns the sample.
//  3. INSERTs a `metric_retraction` row referencing the
//     sample.
//  4. Asserts ListMetricSamples NOW returns ZERO rows --
//     the active pointer is STILL in place, but the
//     retraction filter excludes the row.
func TestSQLStore_ListMetricSamples_FiltersRetracted(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode; skipping live PG test")
	}
	db := openLiveDB(t)
	schema := ruleEngineTestSchemaName
	ctx := context.Background()

	repoID := uuid.Must(uuid.NewV4())
	sha := "0000000000000000000000000000000000000042"
	scopeID := uuid.Must(uuid.NewV4())
	sampleID := uuid.Must(uuid.NewV4())

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, fmt.Sprintf(q, schema), args...); err != nil {
			t.Fatalf("seed (%q): %v", q, err)
		}
	}

	exec(`INSERT INTO %s.commit (repo_id, sha) VALUES ($1, $2)`, repoID.String(), sha)
	exec(`INSERT INTO %s.scope_binding (scope_id, scope_kind, canonical_signature)
	      VALUES ($1, $2, $3)`, scopeID.String(), "class", "src/Foo.cs::Foo")
	exec(`INSERT INTO %s.metric_sample (sample_id, repo_id, sha, scope_id, metric_kind, metric_version, value)
	      VALUES ($1, $2, $3, $4, 'lcom4', 1, 9.0)`,
		sampleID.String(), repoID.String(), sha, scopeID.String())
	// Active pointer -- per the schema's PK over the
	// quintuple, only one active row per (repo, sha,
	// scope_id, metric_kind, metric_version).
	exec(`INSERT INTO %s.metric_sample_active (repo_id, sha, scope_id, metric_kind, metric_version, sample_id)
	      VALUES ($1, $2, $3, 'lcom4', 1, $4)`,
		repoID.String(), sha, scopeID.String(), sampleID.String())

	stewardStore, err := steward.NewSQLStoreWithSchema(db, schema)
	if err != nil {
		t.Fatalf("steward.NewSQLStoreWithSchema: %v", err)
	}
	store, err := NewSQLStore(SQLStoreConfig{DB: db, Schema: schema, Steward: stewardStore, WalWriter: newTestWALWriter(t)})
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}

	// --- pre-retraction: the sample is visible -------------
	samples, err := store.ListMetricSamples(ctx, repoID, sha, nil)
	if err != nil {
		t.Fatalf("pre-retract ListMetricSamples: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("pre-retract: got %d samples, want 1", len(samples))
	}
	if samples[0].SampleID != sampleID {
		t.Fatalf("pre-retract sample_id: got %s, want %s", samples[0].SampleID, sampleID)
	}

	// --- append a retraction row --------------------------
	// The active pointer stays in place -- DELETE on
	// metric_sample_active is REVOKEd per tech-spec Sec 7.2
	// line 1248.
	exec(`INSERT INTO %s.metric_retraction (sample_id, reason, appended_by)
	      VALUES ($1, $2, $3)`, sampleID.String(), "vendored file", "operator:alice")

	// --- post-retraction: ListMetricSamples filters it ----
	samples, err = store.ListMetricSamples(ctx, repoID, sha, nil)
	if err != nil {
		t.Fatalf("post-retract ListMetricSamples: %v", err)
	}
	if len(samples) != 0 {
		t.Fatalf("post-retract: got %d samples, want 0 (retracted sample MUST be filtered via metric_retraction anti-join)", len(samples))
	}

	// Verify the active pointer is STILL present -- the
	// retraction does not delete it.
	var activeCount int
	if err := db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT count(*) FROM %s.metric_sample_active WHERE sample_id = $1`, schema),
		sampleID.String()).Scan(&activeCount); err != nil {
		t.Fatalf("count metric_sample_active: %v", err)
	}
	if activeCount != 1 {
		t.Errorf("metric_sample_active count: got %d, want 1 (DELETE is REVOKEd; the retraction filter is the ONLY thing hiding the sample)", activeCount)
	}
}

// discardWriter is an [io.Writer] that drops everything --
// used by the SQLStore live test so the slog default text
// formatter does not flood test output. Defined locally
// to avoid pulling in `io/ioutil` (deprecated) and to keep
// the test self-contained.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestSQLPendingScanReader_LiveRoundTrip pins iter-6
// evaluator item #4: the production catchup query is
// exercised against PostgreSQL, including:
//
//   - The `committed_at` ordering (iter-6 item #1: the
//     iter-5 code used `created_at` which does NOT exist).
//   - The keyset cursor pagination over
//     `(committed_at, repo_id, sha)` (iter-6 item #2: a
//     persistent poison row at the head must NOT starve
//     valid later SHAs; the cursor advances past it).
//   - The anti-join correctly EXCLUDES non-degraded
//     `caller='batch_refresh'` rows.
//   - The anti-join correctly INCLUDES rows whose existing
//     `evaluation_run` is for `caller='eval_gate'` (a
//     synchronous gate run must NOT suppress the canonical
//     batch refresh).
//   - The anti-join correctly INCLUDES rows whose existing
//     batch_refresh row is DEGRADED (a degraded run is not
//     the canonical witness).
//   - Same-committed_at rows tie-break by `(repo_id, sha)`
//     so the cursor advances deterministically.
func TestSQLPendingScanReader_LiveRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode; skipping live PG test")
	}
	db := openLiveDB(t)
	schema := ruleEngineTestSchemaName
	ctx := context.Background()

	policyID := uuid.Must(uuid.NewV4())

	// Seed FIVE commits across THREE repos with deliberately
	// crafted committed_at timestamps:
	//
	//   t0 = 10:00:00
	//   t1 = 10:00:01
	//   t2 = 10:00:02 (TWO commits at same instant -- cursor
	//                  tie-break by (repo_id, sha))
	//   t3 = 10:00:03
	//
	// Commits A, B at t0, t1 are clean (no evaluation_run).
	// Commit C at t2 already has an eval_gate run -- must NOT
	// suppress the canonical batch refresh.
	// Commit D at t2 (same instant as C, different sha) is
	// clean.
	// Commit E at t3 has a DEGRADED batch_refresh run -- the
	// anti-join must NOT count it as the canonical witness.
	repoA := uuid.Must(uuid.NewV4())
	repoB := uuid.Must(uuid.NewV4())
	repoC := uuid.Must(uuid.NewV4())

	base := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	t0 := base
	t1 := base.Add(1 * time.Second)
	t2 := base.Add(2 * time.Second)
	t3 := base.Add(3 * time.Second)

	type row struct {
		repo uuid.UUID
		sha  string
		ts   time.Time
	}
	rows := []row{
		{repoA, "shaA-clean", t0},
		{repoA, "shaB-clean", t1},
		{repoB, "shaC-eval-gate-noise", t2},
		{repoC, "shaD-clean-same-instant", t2}, // ties with C on committed_at
		{repoA, "shaE-degraded-batch", t3},
	}
	for _, r := range rows {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			`INSERT INTO %s.commit (repo_id, sha, scan_status, committed_at) VALUES ($1, $2, 'scanned', $3)`,
			schema,
		), r.repo.String(), r.sha, r.ts.UTC()); err != nil {
			t.Fatalf("seed commit %s/%s: %v", r.repo, r.sha, err)
		}
	}

	// Seed an eval_gate run for shaC -- must NOT suppress.
	cEvalRun := uuid.Must(uuid.NewV4())
	cEvalVerdict := uuid.Must(uuid.NewV4())
	if _, err := db.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO %s.evaluation_run (evaluation_run_id, repo_id, sha, policy_version_id, caller, created_at)
		 VALUES ($1, $2, $3, $4, 'eval_gate', now())`, schema,
	), cEvalRun.String(), repoB.String(), "shaC-eval-gate-noise", policyID.String()); err != nil {
		t.Fatalf("seed eval_gate run: %v", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO %s.evaluation_verdict (verdict_id, evaluation_run_id, verdict, degraded, created_at)
		 VALUES ($1, $2, 'pass', false, now())`, schema,
	), cEvalVerdict.String(), cEvalRun.String()); err != nil {
		t.Fatalf("seed eval_gate verdict: %v", err)
	}

	// Seed a DEGRADED batch_refresh run for shaE -- must NOT
	// suppress (degraded verdicts are not the canonical
	// witness).
	eDegRun := uuid.Must(uuid.NewV4())
	eDegVerdict := uuid.Must(uuid.NewV4())
	if _, err := db.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO %s.evaluation_run (evaluation_run_id, repo_id, sha, policy_version_id, caller, created_at)
		 VALUES ($1, $2, $3, $4, 'batch_refresh', now())`, schema,
	), eDegRun.String(), repoA.String(), "shaE-degraded-batch", policyID.String()); err != nil {
		t.Fatalf("seed degraded batch_refresh run: %v", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO %s.evaluation_verdict (verdict_id, evaluation_run_id, verdict, degraded, degraded_reason, created_at)
		 VALUES ($1, $2, 'warn', true, 'samples_pending', now())`, schema,
	), eDegVerdict.String(), eDegRun.String()); err != nil {
		t.Fatalf("seed degraded verdict: %v", err)
	}

	// Build the reader and exercise the paging contract.
	reader, err := NewSQLPendingScanReader(SQLPendingScanReaderConfig{DB: db, Schema: schema})
	if err != nil {
		t.Fatalf("NewSQLPendingScanReader: %v", err)
	}

	// First page: limit=2, expect [shaA-clean, shaB-clean]
	// (ordered by committed_at ASC). Cursor returned.
	page1, cur1, err := reader.PendingScans(ctx, policyID, 2, nil)
	if err != nil {
		t.Fatalf("PendingScans page 1: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page 1 len=%d; want 2 (got=%v)", len(page1), page1)
	}
	if page1[0].SHA != "shaA-clean" {
		t.Errorf("page 1 [0].SHA=%s; want shaA-clean", page1[0].SHA)
	}
	if page1[1].SHA != "shaB-clean" {
		t.Errorf("page 1 [1].SHA=%s; want shaB-clean", page1[1].SHA)
	}
	if cur1 == nil {
		t.Fatal("page 1 cursor is nil; expected non-nil")
	}

	// Second page: limit=2, expect the two same-committed_at
	// SHAs in (repo_id, sha) order. shaC was suppressed by
	// the eval_gate row? -- NO, eval_gate must NOT suppress.
	// shaD is clean. Both rows have t2 -- the cursor advances
	// by (repo_id, sha) tiebreak.
	page2, cur2, err := reader.PendingScans(ctx, policyID, 2, cur1)
	if err != nil {
		t.Fatalf("PendingScans page 2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page 2 len=%d; want 2", len(page2))
	}
	// Same-committed_at: order by repo_id ASC. We don't
	// know repoB vs repoC ordering up front, but BOTH must
	// appear.
	got := map[string]bool{page2[0].SHA: true, page2[1].SHA: true}
	if !got["shaC-eval-gate-noise"] {
		t.Errorf("page 2 missing shaC-eval-gate-noise (eval_gate run wrongly suppressed canonical refresh)")
	}
	if !got["shaD-clean-same-instant"] {
		t.Errorf("page 2 missing shaD-clean-same-instant")
	}
	if cur2 == nil {
		t.Fatal("page 2 cursor is nil; expected non-nil")
	}

	// Third page: limit=2, expect [shaE-degraded-batch]
	// because the degraded run does NOT suppress, and that's
	// the last row. Short page (1 < 2) signals end-of-backlog
	// to Worker.Catchup.
	page3, cur3, err := reader.PendingScans(ctx, policyID, 2, cur2)
	if err != nil {
		t.Fatalf("PendingScans page 3: %v", err)
	}
	if len(page3) != 1 {
		t.Fatalf("page 3 len=%d; want 1 (degraded batch_refresh must NOT suppress)", len(page3))
	}
	if page3[0].SHA != "shaE-degraded-batch" {
		t.Errorf("page 3 [0].SHA=%s; want shaE-degraded-batch", page3[0].SHA)
	}
	if cur3 == nil {
		t.Error("page 3 cursor is nil; expected non-nil for a non-empty page")
	}

	// Fourth page: empty (cursor past every row).
	page4, cur4, err := reader.PendingScans(ctx, policyID, 2, cur3)
	if err != nil {
		t.Fatalf("PendingScans page 4: %v", err)
	}
	if len(page4) != 0 {
		t.Errorf("page 4 len=%d; want 0 (backlog drained)", len(page4))
	}
	if cur4 != nil {
		t.Error("page 4 cursor must be nil when the page is empty")
	}

	// Seed a NON-DEGRADED batch_refresh row for shaA-clean and
	// re-run PendingScans -- shaA must now be filtered out
	// (the canonical witness is present).
	aRun := uuid.Must(uuid.NewV4())
	aVerdict := uuid.Must(uuid.NewV4())
	if _, err := db.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO %s.evaluation_run (evaluation_run_id, repo_id, sha, policy_version_id, caller, created_at)
		 VALUES ($1, $2, $3, $4, 'batch_refresh', now())`, schema,
	), aRun.String(), repoA.String(), "shaA-clean", policyID.String()); err != nil {
		t.Fatalf("seed canonical batch_refresh: %v", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO %s.evaluation_verdict (verdict_id, evaluation_run_id, verdict, degraded, created_at)
		 VALUES ($1, $2, 'pass', false, now())`, schema,
	), aVerdict.String(), aRun.String()); err != nil {
		t.Fatalf("seed canonical verdict: %v", err)
	}
	pageAfter, _, err := reader.PendingScans(ctx, policyID, 10, nil)
	if err != nil {
		t.Fatalf("PendingScans after seeding canonical: %v", err)
	}
	for _, ev := range pageAfter {
		if ev.RepoID == repoA && ev.SHA == "shaA-clean" {
			t.Errorf("PendingScans returned shaA-clean after a non-degraded batch_refresh row was inserted; anti-join broken")
		}
	}
}
