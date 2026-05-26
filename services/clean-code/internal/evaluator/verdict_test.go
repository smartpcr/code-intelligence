package evaluator

import (
	"context"
	"errors"
	"testing"

	"github.com/gofrs/uuid"
)

// --- Stage 6.1 e2e-scenarios test: `verdict-enum-only-canonical`
//
// Pin the closed set of [Verdict] values so a future change
// that adds `fail` / `gated` / etc. (iter 1 evaluator item
// 6) trips this test BEFORE landing in CI. The implementation
// plan brief calls this out verbatim:
//
//	Scenario: verdict-enum-only-canonical -- Given the
//	`Verdict` Go enum at compile time, When iterating its
//	values, Then they are exactly `{pass, warn, block}` (no
//	`fail`, no `gated`) -- guards iter 1 evaluator item 6.

func TestVerdict_CanonicalSetIsExactlyPassWarnBlock(t *testing.T) {
	t.Parallel()

	// 1. Each canonical constant must validate.
	for _, v := range []Verdict{VerdictPass, VerdictWarn, VerdictBlock} {
		if !v.IsValid() {
			t.Errorf("VerdictIsValid(%q) = false; canonical value must validate", v)
		}
	}

	// 2. Constants must spell the architecture-canonical
	//    strings verbatim (matches migration 0003's
	//    `clean_code.evaluation_verdict_value` ENUM).
	cases := map[Verdict]string{
		VerdictPass:  "pass",
		VerdictWarn:  "warn",
		VerdictBlock: "block",
	}
	for v, want := range cases {
		if string(v) != want {
			t.Errorf("Verdict spelling mismatch: %s != %s", string(v), want)
		}
	}

	// 3. The non-canonical iter-1 strings MUST be rejected.
	for _, bad := range []Verdict{"fail", "gated", "pass|warn|block", "", " pass", "PASS"} {
		if bad.IsValid() {
			t.Errorf("Verdict(%q).IsValid() = true; non-canonical value MUST be rejected (iter-1 evaluator item 6)", bad)
		}
	}

	// 4. Sanity: String() projection is the canonical
	//    spelling (consumers shipping the value to a
	//    string-typed JSON field rely on this).
	if VerdictBlock.String() != "block" {
		t.Errorf("VerdictBlock.String() = %q; want %q", VerdictBlock.String(), "block")
	}
}

// --- Stage 6.1 e2e-scenarios test: `percentile-stale-not-on-gate`
//
// `percentile_stale` is admitted by the
// `evaluation_verdict_degraded_reason_canonical` DB CHECK
// (migration 0003 lines 620-628) but the gate MUST refuse
// to surface it -- it's an INSIGHTS-surface reason only
// (tech-spec C17, Stage 7.3). Two layers of defence:
//
//  1. [DegradedReason.IsValidForGate] returns false for
//     `percentile_stale`.
//  2. [Gate.writeDegraded] and
//     [SQLDegradedRunStore.AppendDegradedRun] both reject
//     the value with [ErrInvalidGateDegradedReason].

func TestDegradedReason_IsValidForGate_RejectsPercentileStale(t *testing.T) {
	t.Parallel()

	// Allowed reasons (architecture Sec 8.2 minus
	// percentile_stale per tech-spec C17).
	allowed := []DegradedReason{
		DegradedReasonSamplesPending,
		DegradedReasonPolicySignatureInvalid,
		DegradedReasonXRepoEdgesUnavailable,
	}
	for _, r := range allowed {
		if !r.IsValidForGate() {
			t.Errorf("DegradedReason(%q).IsValidForGate() = false; canonical eval.gate reason must validate", r)
		}
	}

	// percentile_stale: rejected. The DB CHECK admits it
	// (it's a valid DegradedReason for the metric_sample
	// + Insights surface) but the gate validator rejects
	// it explicitly.
	stale := degradedReasonPercentileStale
	if stale.IsValidForGate() {
		t.Errorf("DegradedReason(%q).IsValidForGate() = true; gate MUST reject percentile_stale (tech-spec C17)", stale)
	}

	// Unknown labels: rejected.
	for _, r := range []DegradedReason{"", "metric_regression", "policy_signature_invalid ", "PERCENTILE_STALE"} {
		if r.IsValidForGate() {
			t.Errorf("DegradedReason(%q).IsValidForGate() = true; unknown value MUST be rejected", r)
		}
	}
}

// TestGate_writeDegraded_RejectsPercentileStaleReason exercises
// the gate's internal validator at the write boundary --
// rubber-duck #1: validator-only test may undersatisfy the
// `percentile-stale-not-on-gate` scenario; this test
// invokes [Gate.writeDegraded] directly (same-package access)
// and confirms the reason is refused BEFORE the audit row is
// constructed.
func TestGate_writeDegraded_RejectsPercentileStaleReason(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())

	eng := &stubEngine{}
	ready := &stubReadiness{ready: true}
	pr := &stubPolicyReader{}
	ver := &stubVerifier{}
	deg := &stubDegradedStore{}
	g := newWiredGate(t, eng, ready, pr, ver, deg)

	got, err := g.writeDegraded(context.Background(), repoID, "sha1", nil, pvID, degradedReasonPercentileStale, errors.New("upstream"))
	if !errors.Is(err, ErrInvalidGateDegradedReason) {
		t.Fatalf("err=%v; want errors.Is(.., ErrInvalidGateDegradedReason)", err)
	}
	if got.EvaluationRunID != uuid.Nil || got.EvaluationVerdictID != uuid.Nil {
		t.Errorf("got=%+v; want zero-valued EvaluateResult on reject", got)
	}
	if len(deg.calls) != 0 {
		t.Errorf("degraded store written %d times; want 0 (validator must reject BEFORE INSERT)", len(deg.calls))
	}
}

// TestSQLDegradedRunStore_RejectsPercentileStaleReasonBeforeSQL
// double-covers the rejection at the storage boundary. The
// SQL store never reaches `tx.ExecContext` so this test
// works without a live DB.
func TestSQLDegradedRunStore_RejectsPercentileStaleReasonBeforeSQL(t *testing.T) {
	t.Parallel()
	// Construct a store without a DB handle -- the
	// validator runs BEFORE BeginTx so this is safe.
	// NewSQLDegradedRunStore requires DB != nil; we
	// construct the struct directly for the test.
	s := &SQLDegradedRunStore{db: nil, schema: "clean_code"}

	runID := uuid.Must(uuid.NewV4())
	verdictID := uuid.Must(uuid.NewV4())
	run := DegradedRun{
		EvaluationRunID: runID,
		RepoID:          uuid.Must(uuid.NewV4()),
		SHA:             "sha1",
		PolicyVersionID: uuid.Must(uuid.NewV4()),
		CreatedAt:       1,
	}
	verdict := DegradedVerdict{
		VerdictID:       verdictID,
		EvaluationRunID: runID,
		Verdict:         VerdictWarn,
		Degraded:        true,
		DegradedReason:  degradedReasonPercentileStale,
		CreatedAt:       1,
	}
	err := s.AppendDegradedRun(context.Background(), run, verdict)
	if !errors.Is(err, ErrInvalidGateDegradedReason) {
		t.Fatalf("err=%v; want errors.Is(.., ErrInvalidGateDegradedReason) -- gate writer MUST reject percentile_stale before SQL", err)
	}
}

// TestSQLDegradedRunStore_RejectsNonWarnDegradedVerdict pins
// the "degraded=true requires verdict='warn'" invariant from
// architecture Sec 3.7 + operator pin gate-degraded-policy=warn.
// A future writer that tries to surface `block` on a degraded
// path is refused at the storage boundary.
func TestSQLDegradedRunStore_RejectsNonWarnDegradedVerdict(t *testing.T) {
	t.Parallel()
	s := &SQLDegradedRunStore{db: nil, schema: "clean_code"}
	runID := uuid.Must(uuid.NewV4())
	run := DegradedRun{
		EvaluationRunID: runID,
		RepoID:          uuid.Must(uuid.NewV4()),
		SHA:             "sha1",
		PolicyVersionID: uuid.Must(uuid.NewV4()),
		CreatedAt:       1,
	}
	verdict := DegradedVerdict{
		VerdictID:       uuid.Must(uuid.NewV4()),
		EvaluationRunID: runID,
		Verdict:         VerdictBlock,
		Degraded:        true,
		DegradedReason:  DegradedReasonSamplesPending,
		CreatedAt:       1,
	}
	err := s.AppendDegradedRun(context.Background(), run, verdict)
	if err == nil {
		t.Fatal("AppendDegradedRun: err=nil; want rejection for degraded=true verdict!=warn")
	}
}
