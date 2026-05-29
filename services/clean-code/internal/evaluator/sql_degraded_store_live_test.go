package evaluator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"
	_ "github.com/lib/pq"

	"forge/services/clean-code/internal/policy/steward"
)

// envEvaluatorLiveURL is the libpq DSN consumed by the
// evaluator package's live-PG tests. Matches the
// `CLEAN_CODE_PG_URL` env var the rule_engine /
// storage / steward live tests already consume so a
// single `export CLEAN_CODE_PG_URL=...` turns on every
// live test path at once.
const envEvaluatorLiveURL = "CLEAN_CODE_PG_URL"

// evaluatorLiveSchema is the ISOLATED schema this test
// owns. Kept distinct from `clean_code` (production)
// AND from the schemas owned by the rule_engine /
// storage / steward live tests so every live suite
// runs without racing on schema teardown.
const evaluatorLiveSchema = "clean_code_evaluator_live_test"

// evaluatorLiveSchemaPrep materialises ONLY the audit
// columns the [SQLDegradedRunStore] touches:
//
//   - `evaluation_run(evaluation_run_id, repo_id, sha,
//     policy_version_id, caller, scope_id, created_at)`
//   - `evaluation_verdict(verdict_id, evaluation_run_id,
//     verdict, degraded, degraded_reason, created_at)`
//
// We do NOT replay the real migration; the trimmed
// schema is enough to exercise the writer end-to-end
// and keeps the test independent of migration ordering.
// The shapes match `migrations/0008_*.up.sql` and
// architecture Sec 5.4.2 / 5.4.3 verbatim:
//
//   - `evaluation_run.policy_version_id` is NOT NULL
//   - `evaluation_verdict.evaluation_run_id` is NOT NULL
//     and FK-references `evaluation_run`
//   - timestamp column is `created_at` (NEVER `settled_at`)
//   - `evaluation_verdict` has NO `scope` column
//   - `caller` enum admits 'eval_gate'
//   - `verdict` enum admits 'pass' | 'warn' | 'block'
const evaluatorLiveSchemaPrep = `
DROP SCHEMA IF EXISTS %[1]s CASCADE;
CREATE SCHEMA %[1]s;

CREATE TYPE %[1]s.eval_caller  AS ENUM ('eval_gate', 'batch_refresh', 'reconciler_replay');
CREATE TYPE %[1]s.eval_verdict AS ENUM ('pass', 'warn', 'block');

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
`

// openEvaluatorLiveDB connects to the libpq URL in
// CLEAN_CODE_PG_URL, rebuilds the audit schema, and
// returns a handle. Skips the test when the env var is
// unset or the DB is unreachable -- developer laptops
// without PG do not see a spurious failure.
func openEvaluatorLiveDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(envEvaluatorLiveURL))
	if dsn == "" {
		t.Skipf("%s unset; skipping evaluator live audit test", envEvaluatorLiveURL)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("postgres not reachable at %s: %v", envEvaluatorLiveURL, err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(evaluatorLiveSchemaPrep, evaluatorLiveSchema)); err != nil {
		t.Fatalf("preparing schema %s: %v", evaluatorLiveSchema, err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, evaluatorLiveSchema)); err != nil {
			t.Logf("cleanup: drop schema: %v", err)
		}
		_ = db.Close()
	})
	return db
}

// liveStubActivation is a stateless [PolicyActivationReader]
// used by the live audit test. Returns the configured
// pvid (always ok=true) so the gate enters the verb path.
type liveStubActivation struct{ pvID uuid.UUID }

func (s liveStubActivation) ActivePolicyVersionID(ctx context.Context) (uuid.UUID, bool, error) {
	return s.pvID, true, nil
}

// liveStubPolicyReader returns a minimal
// [steward.PolicyVersion] for ANY id so the gate's
// PolicyResolver step succeeds.
type liveStubPolicyReader struct{}

func (s liveStubPolicyReader) GetPolicyVersion(ctx context.Context, id uuid.UUID) (steward.PolicyVersion, error) {
	return steward.PolicyVersion{PolicyVersionID: id}, nil
}

// liveStubVerifier is a [PolicySignatureVerifier] stub
// that returns the configured error. The signature-invalid
// degraded short-circuit fires when this returns a
// non-nil error.
type liveStubVerifier struct{ err error }

func (s liveStubVerifier) VerifyPolicyVersionSignature(ctx context.Context, pv steward.PolicyVersion) error {
	return s.err
}

// liveStubReadiness returns the configured readiness
// value. The samples-pending degraded short-circuit
// fires when this returns false.
type liveStubReadiness struct{ ready bool }

func (s liveStubReadiness) SamplesReady(ctx context.Context, repoID uuid.UUID, sha string) (bool, error) {
	return s.ready, nil
}

// liveStubEngine is a [RuleEngine] stub that records
// whether `RunSync` was invoked. The two degraded
// short-circuits MUST NOT invoke the engine; the test
// asserts that by checking `called == false`.
type liveStubEngine struct{ called bool }

func (s *liveStubEngine) RunSync(ctx context.Context, repoID uuid.UUID, sha string, scope *uuid.UUID, pvID uuid.UUID) (EngineRunResult, error) {
	s.called = true
	return EngineRunResult{}, errors.New("liveStubEngine: RunSync should NOT be called on degraded short-circuit")
}

// TestSQLDegradedRunStore_LiveRoundTrip_SignatureInvalid
// pins the iter-2 evaluator feedback #3 fix: end-to-end,
// the gate's signature-invalid degraded short-circuit
// MUST insert EXACTLY ONE `evaluation_run` row + EXACTLY
// ONE `evaluation_verdict` row through the PRODUCTION
// [SQLDegradedRunStore]. Prior iters had only stub-only
// audit-write coverage; this test exercises the real SQL
// round-trip and asserts the canonical Audit schema is
// preserved:
//
//   - `evaluation_run` row: caller='eval_gate',
//     non-null `policy_version_id`, non-null `created_at`
//   - `evaluation_verdict` row: FK matches the run's
//     `evaluation_run_id` (NEVER NULL), verdict='warn',
//     degraded=true, degraded_reason='policy_signature_invalid',
//     non-null `created_at`
//   - the Rule Engine is NOT invoked (the
//     [liveStubEngine] `called` flag stays false)
//
// Skipped on developer laptops where CLEAN_CODE_PG_URL
// is unset.
func TestSQLDegradedRunStore_LiveRoundTrip_SignatureInvalid(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode; skipping live PG test")
	}
	db := openEvaluatorLiveDB(t)
	schema := evaluatorLiveSchema
	ctx := context.Background()

	store, err := NewSQLDegradedRunStore(SQLDegradedRunStoreConfig{DB: db, Schema: schema, WalWriter: newTestWALWriter(t)})
	if err != nil {
		t.Fatalf("NewSQLDegradedRunStore: %v", err)
	}
	engine := &liveStubEngine{}
	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())

	gate := NewGateWithEngine(NewGate(nil), EvaluateConfig{
		Engine:          engine,
		Readiness:       liveStubReadiness{ready: true},
		PolicyReader:    liveStubPolicyReader{},
		SignatureVerify: liveStubVerifier{err: errors.New("ed25519: invalid signature")},
		DegradedStore:   store,
		Activation:      liveStubActivation{pvID: pvID},
		NewID:           uuid.NewV4,
	})

	sha := "abc1234567890abcdef1234567890abcdef12345"
	result, err := gate.Gate(ctx, repoID, sha, nil)
	if !errors.Is(err, ErrPolicySignatureInvalid) {
		t.Fatalf("Gate.Gate: err=%v; want ErrPolicySignatureInvalid", err)
	}
	if !result.Degraded {
		t.Errorf("result.Degraded=false; want true")
	}
	if result.Verdict != VerdictWarn {
		t.Errorf("result.Verdict=%q; want warn", result.Verdict)
	}
	if result.DegradedReason != DegradedReasonPolicySignatureInvalid {
		t.Errorf("result.DegradedReason=%q; want %q", result.DegradedReason, DegradedReasonPolicySignatureInvalid)
	}
	if engine.called {
		t.Error("liveStubEngine.called=true; the Rule Engine MUST NOT be invoked on a degraded short-circuit")
	}

	// --- assert exactly one evaluation_run row landed ---

	var (
		runRows      int
		runID        string
		runRepoID    string
		runSHA       string
		runPolicyID  string
		runCaller    string
		runScopeID   sql.NullString
		runCreatedAt time.Time
	)
	if err := db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT count(*) FROM %s.evaluation_run`, schema),
	).Scan(&runRows); err != nil {
		t.Fatalf("count evaluation_run: %v", err)
	}
	if runRows != 1 {
		t.Fatalf("evaluation_run row count: got=%d; want=1", runRows)
	}
	if err := db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT evaluation_run_id::text, repo_id::text, sha, policy_version_id::text, caller::text, scope_id::text, created_at FROM %s.evaluation_run`, schema),
	).Scan(&runID, &runRepoID, &runSHA, &runPolicyID, &runCaller, &runScopeID, &runCreatedAt); err != nil {
		t.Fatalf("read evaluation_run: %v", err)
	}
	if runCaller != "eval_gate" {
		t.Errorf("evaluation_run.caller=%q; want eval_gate", runCaller)
	}
	if runRepoID != repoID.String() {
		t.Errorf("evaluation_run.repo_id=%q; want %q", runRepoID, repoID.String())
	}
	if runSHA != sha {
		t.Errorf("evaluation_run.sha=%q; want %q", runSHA, sha)
	}
	if runPolicyID != pvID.String() {
		t.Errorf("evaluation_run.policy_version_id=%q; want %q", runPolicyID, pvID.String())
	}
	if runPolicyID == uuid.Nil.String() {
		t.Errorf("evaluation_run.policy_version_id is the zero uuid; the audit row is unrecoverable")
	}
	if runCreatedAt.IsZero() {
		t.Errorf("evaluation_run.created_at is zero; the canonical timestamp column MUST be populated")
	}
	if runScopeID.Valid {
		t.Errorf("evaluation_run.scope_id=%q; want NULL (whole-SHA gate.Gate call passed scope=nil)", runScopeID.String)
	}

	// --- assert exactly one evaluation_verdict row landed ---

	var (
		verdictRows      int
		verdictID        string
		verdictRunID     string
		verdictVerdict   string
		verdictDegraded  bool
		verdictReason    sql.NullString
		verdictCreatedAt time.Time
	)
	if err := db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT count(*) FROM %s.evaluation_verdict`, schema),
	).Scan(&verdictRows); err != nil {
		t.Fatalf("count evaluation_verdict: %v", err)
	}
	if verdictRows != 1 {
		t.Fatalf("evaluation_verdict row count: got=%d; want=1", verdictRows)
	}
	if err := db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT verdict_id::text, evaluation_run_id::text, verdict::text, degraded, degraded_reason, created_at FROM %s.evaluation_verdict`, schema),
	).Scan(&verdictID, &verdictRunID, &verdictVerdict, &verdictDegraded, &verdictReason, &verdictCreatedAt); err != nil {
		t.Fatalf("read evaluation_verdict: %v", err)
	}
	// FK invariant: the verdict's evaluation_run_id MUST
	// match the run's id and MUST NOT be the zero uuid.
	if verdictRunID != runID {
		t.Errorf("evaluation_verdict.evaluation_run_id=%q; want %q (the run we just inserted)", verdictRunID, runID)
	}
	if verdictRunID == uuid.Nil.String() {
		t.Errorf("evaluation_verdict.evaluation_run_id is the zero uuid; the FK contract is broken")
	}
	if verdictVerdict != "warn" {
		t.Errorf("evaluation_verdict.verdict=%q; want warn (degraded paths surface warn per architecture Sec 1.6 gate-degraded-policy=warn)", verdictVerdict)
	}
	if !verdictDegraded {
		t.Error("evaluation_verdict.degraded=false; want true on the degraded short-circuit")
	}
	if !verdictReason.Valid || verdictReason.String != string(DegradedReasonPolicySignatureInvalid) {
		t.Errorf("evaluation_verdict.degraded_reason=%v; want %q", verdictReason, DegradedReasonPolicySignatureInvalid)
	}
	if verdictCreatedAt.IsZero() {
		t.Errorf("evaluation_verdict.created_at is zero; the canonical timestamp column MUST be populated (NEVER 'settled_at')")
	}
}

// TestSQLDegradedRunStore_LiveRoundTrip_SamplesPending
// mirrors the signature-invalid live test for the OTHER
// degraded short-circuit (samples-pending). Asserts
// the canonical Audit schema is preserved:
//
//   - ONE `evaluation_run(caller='eval_gate', ...)` row
//   - ONE `evaluation_verdict(verdict='warn',
//     degraded=true, degraded_reason='samples_pending',
//     non-null FK + non-null created_at)` row
//   - the Rule Engine is NOT invoked
func TestSQLDegradedRunStore_LiveRoundTrip_SamplesPending(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode; skipping live PG test")
	}
	db := openEvaluatorLiveDB(t)
	schema := evaluatorLiveSchema
	ctx := context.Background()

	store, err := NewSQLDegradedRunStore(SQLDegradedRunStoreConfig{DB: db, Schema: schema, WalWriter: newTestWALWriter(t)})
	if err != nil {
		t.Fatalf("NewSQLDegradedRunStore: %v", err)
	}
	engine := &liveStubEngine{}
	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())

	gate := NewGateWithEngine(NewGate(nil), EvaluateConfig{
		Engine:          engine,
		Readiness:       liveStubReadiness{ready: false}, // samples-pending
		PolicyReader:    liveStubPolicyReader{},
		SignatureVerify: liveStubVerifier{}, // valid signature
		DegradedStore:   store,
		Activation:      liveStubActivation{pvID: pvID},
		NewID:           uuid.NewV4,
	})

	sha := "deadbeef1234567890abcdef1234567890abcdef"
	result, err := gate.Gate(ctx, repoID, sha, nil)
	if !errors.Is(err, ErrSamplesPending) {
		t.Fatalf("Gate.Gate: err=%v; want ErrSamplesPending", err)
	}
	if !result.Degraded {
		t.Errorf("result.Degraded=false; want true")
	}
	if result.Verdict != VerdictWarn {
		t.Errorf("result.Verdict=%q; want warn", result.Verdict)
	}
	if result.DegradedReason != DegradedReasonSamplesPending {
		t.Errorf("result.DegradedReason=%q; want %q", result.DegradedReason, DegradedReasonSamplesPending)
	}
	if engine.called {
		t.Error("liveStubEngine.called=true; the Rule Engine MUST NOT be invoked on a samples-pending degraded short-circuit")
	}

	// One run + one verdict for this run. (The
	// signature-invalid test in this same package
	// uses a different schema-reset cycle via
	// t.Cleanup -- each *Test* gets a fresh schema
	// because t.Cleanup runs at the end of the test
	// and openEvaluatorLiveDB drops + recreates.)
	var runRows int
	if err := db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT count(*) FROM %s.evaluation_run WHERE repo_id = $1`, schema),
		repoID.String(),
	).Scan(&runRows); err != nil {
		t.Fatalf("count evaluation_run: %v", err)
	}
	if runRows != 1 {
		t.Fatalf("evaluation_run rows for repo=%s: got=%d; want=1", repoID, runRows)
	}

	var (
		runID        string
		runPolicyID  string
		runCaller    string
		runCreatedAt time.Time
	)
	if err := db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT evaluation_run_id::text, policy_version_id::text, caller::text, created_at
		             FROM %s.evaluation_run WHERE repo_id = $1`, schema),
		repoID.String(),
	).Scan(&runID, &runPolicyID, &runCaller, &runCreatedAt); err != nil {
		t.Fatalf("read evaluation_run: %v", err)
	}
	if runCaller != "eval_gate" {
		t.Errorf("evaluation_run.caller=%q; want eval_gate", runCaller)
	}
	if runPolicyID != pvID.String() {
		t.Errorf("evaluation_run.policy_version_id=%q; want %q", runPolicyID, pvID.String())
	}
	if runCreatedAt.IsZero() {
		t.Errorf("evaluation_run.created_at is zero")
	}

	var (
		verdictRows      int
		verdictRunID     string
		verdictVerdict   string
		verdictDegraded  bool
		verdictReason    sql.NullString
		verdictCreatedAt time.Time
	)
	if err := db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT count(*) FROM %s.evaluation_verdict WHERE evaluation_run_id = $1`, schema),
		runID,
	).Scan(&verdictRows); err != nil {
		t.Fatalf("count evaluation_verdict: %v", err)
	}
	if verdictRows != 1 {
		t.Fatalf("evaluation_verdict rows for run=%s: got=%d; want=1", runID, verdictRows)
	}
	if err := db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT evaluation_run_id::text, verdict::text, degraded, degraded_reason, created_at
		             FROM %s.evaluation_verdict WHERE evaluation_run_id = $1`, schema),
		runID,
	).Scan(&verdictRunID, &verdictVerdict, &verdictDegraded, &verdictReason, &verdictCreatedAt); err != nil {
		t.Fatalf("read evaluation_verdict: %v", err)
	}
	if verdictRunID != runID {
		t.Errorf("evaluation_verdict.evaluation_run_id=%q; want %q", verdictRunID, runID)
	}
	if verdictVerdict != "warn" {
		t.Errorf("evaluation_verdict.verdict=%q; want warn", verdictVerdict)
	}
	if !verdictDegraded {
		t.Error("evaluation_verdict.degraded=false; want true")
	}
	if !verdictReason.Valid || verdictReason.String != string(DegradedReasonSamplesPending) {
		t.Errorf("evaluation_verdict.degraded_reason=%v; want %q", verdictReason, DegradedReasonSamplesPending)
	}
	if verdictCreatedAt.IsZero() {
		t.Errorf("evaluation_verdict.created_at is zero")
	}
}
