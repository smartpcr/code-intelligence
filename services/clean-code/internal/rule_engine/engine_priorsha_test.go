package rule_engine

import (
	"context"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/steward"
)

// TestEngine_RunSync_RootCommit_NoPrior_DeltaNew pins the
// root-commit semantic surfaced by the rubber-duck Stage 5.7
// review (#1): when [Store.ParentSHA] returns `ok=false`,
// every firing rule lands as [DeltaNew] and no resolved row
// is emitted -- the engine MUST NOT fall back to "any
// earlier SHA" topology.
func TestEngine_RunSync_RootCommit_NoPrior_DeltaNew(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scopeID := uuid.Must(uuid.NewV4())

	// No RegisterCommit -> ParentSHA returns ok=false.
	// (The "implicit root" path the production indexer
	// would hit on the first commit of a fresh repo.)
	f.store.InsertSamples(f.repoID, "sha-root", []Sample{f.sample(scopeID, 12)})
	if _, err := f.engine.RunSync(context.Background(), f.repoID, "sha-root", nil, f.policyVersionID); err != nil {
		t.Fatalf("RunSync: %v", err)
	}

	findings := f.store.Findings()
	if len(findings) != 1 {
		t.Fatalf("findings=%d; want 1", len(findings))
	}
	if findings[0].Delta != DeltaNew {
		t.Errorf("delta=%s; want new (root commit -> no prior)", findings[0].Delta)
	}
}

// TestEngine_RunSync_ParentSHA_TopologicalNotChronological
// pins the rubber-duck #1 fix: prior-finding lookup uses
// `clean_code.commit.parent_sha`, NOT a wall-clock proxy
// like `created_at < currentSHA.created_at`. A back-merge or
// rebase that re-evaluates an older SHA after a newer one
// must NOT see the newer SHA's findings as "prior".
func TestEngine_RunSync_ParentSHA_TopologicalNotChronological(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scopeID := uuid.Must(uuid.NewV4())

	// Topology: shaA (root) -> shaB. We evaluate shaB
	// AFTER seeding a wall-clock-newer (but topologically
	// unrelated) shaC at the same scope/rule. A chronology-
	// based prior lookup would pick shaC; the engine must
	// pick shaA.
	priorTime := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	noiseTime := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)

	shaARunID := uuid.Must(uuid.NewV4())
	if err := f.store.AppendEvaluation(context.Background(),
		EvaluationRun{
			EvaluationRunID: shaARunID,
			RepoID:          f.repoID,
			SHA:             "shaA",
			PolicyVersionID: f.policyVersionID,
			Caller:          CallerBatchRefresh,
			CreatedAt:       priorTime,
		},
		EvaluationVerdict{
			VerdictID:       uuid.Must(uuid.NewV4()),
			EvaluationRunID: shaARunID,
			Verdict:         VerdictWarn,
			CreatedAt:       priorTime,
		},
		[]Finding{{
			FindingID:       uuid.Must(uuid.NewV4()),
			EvaluationRunID: shaARunID,
			RepoID:          f.repoID,
			SHA:             "shaA",
			ScopeID:         scopeID,
			RuleID:          f.ruleID,
			RuleVersion:     f.ruleVersion,
			PolicyVersionID: f.policyVersionID,
			Severity:        steward.SeverityWarn,
			Delta:           DeltaNew,
			CreatedAt:       priorTime,
		}},
	); err != nil {
		t.Fatalf("seed shaA: %v", err)
	}

	// Noise commit shaC with a NEWER created_at but no
	// topological relation to shaB. A chronology-based
	// lookup would mistakenly treat this as the prior.
	shaCRunID := uuid.Must(uuid.NewV4())
	if err := f.store.AppendEvaluation(context.Background(),
		EvaluationRun{
			EvaluationRunID: shaCRunID,
			RepoID:          f.repoID,
			SHA:             "shaC",
			PolicyVersionID: f.policyVersionID,
			Caller:          CallerBatchRefresh,
			CreatedAt:       noiseTime,
		},
		EvaluationVerdict{
			VerdictID:       uuid.Must(uuid.NewV4()),
			EvaluationRunID: shaCRunID,
			Verdict:         VerdictBlock,
			CreatedAt:       noiseTime,
		},
		[]Finding{{
			FindingID:       uuid.Must(uuid.NewV4()),
			EvaluationRunID: shaCRunID,
			RepoID:          f.repoID,
			SHA:             "shaC",
			ScopeID:         scopeID,
			RuleID:          f.ruleID,
			RuleVersion:     f.ruleVersion,
			PolicyVersionID: f.policyVersionID,
			Severity:        steward.SeverityBlock,
			Delta:           DeltaNew,
			CreatedAt:       noiseTime,
		}},
	); err != nil {
		t.Fatalf("seed shaC: %v", err)
	}

	// Evaluate shaB with parent=shaA. Engine MUST see the
	// shaA warn finding as the prior (delta=newly_failing
	// because current is block) and NOT the shaC block
	// finding (which would have given delta=unchanged).
	f.store.RegisterCommit(f.repoID, "shaB", "shaA")
	f.store.InsertSamples(f.repoID, "shaB", []Sample{f.sample(scopeID, 12)})
	if _, err := f.engine.RunSync(context.Background(), f.repoID, "shaB", nil, f.policyVersionID); err != nil {
		t.Fatalf("RunSync shaB: %v", err)
	}

	var shaBFinding *Finding
	for _, fnd := range f.store.Findings() {
		fnd := fnd
		if fnd.SHA == "shaB" {
			shaBFinding = &fnd
			break
		}
	}
	if shaBFinding == nil {
		t.Fatal("no finding for shaB")
	}
	if shaBFinding.Delta != DeltaNewlyFailing {
		t.Errorf("delta=%s; want newly_failing (shaA warn -> shaB block); chronological lookup would yield unchanged via shaC", shaBFinding.Delta)
	}
}

// TestEngine_RunSync_DeltaLookup_FiltersByPolicyVersionID
// pins the implementation-plan Stage 5.7 line 556 contract
// flagged by the evaluator (item 5): a finding written
// under policy version P1 is NOT a prior for the same
// `(scope, rule)` tuple evaluated under policy version P2.
func TestEngine_RunSync_DeltaLookup_FiltersByPolicyVersionID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scopeID := uuid.Must(uuid.NewV4())

	// Seed a prior block under a DIFFERENT policy version.
	priorTime := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	otherPV := uuid.Must(uuid.NewV4())
	priorRunID := uuid.Must(uuid.NewV4())
	if err := f.store.AppendEvaluation(context.Background(),
		EvaluationRun{
			EvaluationRunID: priorRunID,
			RepoID:          f.repoID,
			SHA:             "sha-prior",
			PolicyVersionID: otherPV,
			Caller:          CallerBatchRefresh,
			CreatedAt:       priorTime,
		},
		EvaluationVerdict{
			VerdictID:       uuid.Must(uuid.NewV4()),
			EvaluationRunID: priorRunID,
			Verdict:         VerdictBlock,
			CreatedAt:       priorTime,
		},
		[]Finding{{
			FindingID:       uuid.Must(uuid.NewV4()),
			EvaluationRunID: priorRunID,
			RepoID:          f.repoID,
			SHA:             "sha-prior",
			ScopeID:         scopeID,
			RuleID:          f.ruleID,
			RuleVersion:     f.ruleVersion,
			PolicyVersionID: otherPV,
			Severity:        steward.SeverityBlock,
			Delta:           DeltaNew,
			CreatedAt:       priorTime,
		}},
	); err != nil {
		t.Fatalf("seed prior: %v", err)
	}

	// Evaluate sha2 under the fixture's policy version
	// (NOT otherPV). The prior block under otherPV must
	// NOT count: the firing finding lands as delta=new.
	f.store.RegisterCommit(f.repoID, "sha2", "sha-prior")
	f.store.InsertSamples(f.repoID, "sha2", []Sample{f.sample(scopeID, 12)})
	if _, err := f.engine.RunSync(context.Background(), f.repoID, "sha2", nil, f.policyVersionID); err != nil {
		t.Fatalf("RunSync: %v", err)
	}

	var sha2Finding *Finding
	for _, fnd := range f.store.Findings() {
		fnd := fnd
		if fnd.SHA == "sha2" && fnd.PolicyVersionID == f.policyVersionID {
			sha2Finding = &fnd
			break
		}
	}
	if sha2Finding == nil {
		t.Fatal("no finding for sha2 under fixture policy version")
	}
	if sha2Finding.Delta != DeltaNew {
		t.Errorf("delta=%s; want new (prior is under a different policy_version_id and must be ignored)", sha2Finding.Delta)
	}
}

// TestEngine_RunSync_ResolvedLookup_FiltersByPolicyVersionID
// is the resolved-row counterpart to the delta test above:
// a prior block under a different policy version must NOT
// drive a resolved-row emission for the current policy
// version (otherwise a policy switch that retires a rule
// would look like the rule "got fixed" -- a misleading
// signal for the Insights surface).
func TestEngine_RunSync_ResolvedLookup_FiltersByPolicyVersionID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scopeID := uuid.Must(uuid.NewV4())

	priorTime := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	otherPV := uuid.Must(uuid.NewV4())
	priorRunID := uuid.Must(uuid.NewV4())
	if err := f.store.AppendEvaluation(context.Background(),
		EvaluationRun{
			EvaluationRunID: priorRunID,
			RepoID:          f.repoID,
			SHA:             "sha-prior",
			PolicyVersionID: otherPV,
			Caller:          CallerBatchRefresh,
			CreatedAt:       priorTime,
		},
		EvaluationVerdict{
			VerdictID:       uuid.Must(uuid.NewV4()),
			EvaluationRunID: priorRunID,
			Verdict:         VerdictBlock,
			CreatedAt:       priorTime,
		},
		[]Finding{{
			FindingID:       uuid.Must(uuid.NewV4()),
			EvaluationRunID: priorRunID,
			RepoID:          f.repoID,
			SHA:             "sha-prior",
			ScopeID:         scopeID,
			RuleID:          f.ruleID,
			RuleVersion:     f.ruleVersion,
			PolicyVersionID: otherPV,
			Severity:        steward.SeverityBlock,
			Delta:           DeltaNew,
			CreatedAt:       priorTime,
		}},
	); err != nil {
		t.Fatalf("seed prior: %v", err)
	}

	// sha2 under the fixture's policy version, no samples:
	// without the policy_version_id filter the engine
	// would emit a resolved row referencing the otherPV
	// block.
	f.store.RegisterCommit(f.repoID, "sha2", "sha-prior")
	if _, err := f.engine.RunSync(context.Background(), f.repoID, "sha2", nil, f.policyVersionID); err != nil {
		t.Fatalf("RunSync: %v", err)
	}

	for _, fnd := range f.store.Findings() {
		if fnd.SHA == "sha2" {
			t.Errorf("unexpected sha2 finding: %+v (prior under different policy_version_id must NOT drive a resolved row)", fnd)
		}
	}
}

// TestEngine_RunSync_WithEvaluationLock_RoundTrip pins the
// rubber-duck #5 fix: the engine routes all reads + writes
// through the [Store.WithEvaluationLock] envelope so the
// transaction-bound store (production) handles the entire
// read-modify-write window atomically. The in-memory fake
// exposes this as a serialised mutex.
func TestEngine_RunSync_WithEvaluationLock_RoundTrip(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scopeID := uuid.Must(uuid.NewV4())
	f.store.InsertSamples(f.repoID, "sha1", []Sample{f.sample(scopeID, 12)})

	// Sanity-check that a non-locking store cannot be wired
	// in. (The interface compiles -- this is a runtime
	// smoke check that RunSync still routes through
	// WithEvaluationLock by injecting a custom store that
	// counts calls.)
	counting := &lockCountingStore{Store: f.store}
	engine, err := New(Config{
		Store: counting,
		NewID: f.engine.newID,
		Clock: f.engine.clock,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := engine.RunSync(context.Background(), f.repoID, "sha1", nil, f.policyVersionID); err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if counting.lockCalls != 1 {
		t.Errorf("WithEvaluationLock calls=%d; want 1 per RunSync", counting.lockCalls)
	}
}

// lockCountingStore wraps an InMemoryStore and counts
// WithEvaluationLock calls. The engine MUST route every
// run through this envelope.
type lockCountingStore struct {
	Store
	lockCalls int
}

func (s *lockCountingStore) WithEvaluationLock(ctx context.Context, repoID uuid.UUID, sha string, fn func(Store) error) error {
	s.lockCalls++
	return s.Store.WithEvaluationLock(ctx, repoID, sha, fn)
}
