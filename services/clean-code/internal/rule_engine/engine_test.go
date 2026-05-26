package rule_engine

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/dsl"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/steward"
)

// deterministicIDGen returns a uuid.NewV4-shaped generator
// that mints predictable uuids from a counter so tests can
// pin the order in which the engine asks for IDs.
//
// deterministicIDGen returns a [uuid] generator that mints
// strictly increasing values. SAFE for concurrent use: a
// mutex guards the counter so the
// `TestSync_AdvisoryLock_*` tests that exercise concurrent
// engine calls do not race on the increment.
//
// Format: `00000000-0000-0000-0000-XXXXXXXXXXXX` where the
// final group is the counter as a zero-padded hex string.
// The counter starts at 1 so an unminted slot ([uuid.Nil])
// stays distinguishable from a minted-but-zero slot.
func deterministicIDGen() func() (uuid.UUID, error) {
	var (
		mu      sync.Mutex
		counter uint64
	)
	return func() (uuid.UUID, error) {
		mu.Lock()
		counter++
		c := counter
		mu.Unlock()
		var id uuid.UUID
		// Pack counter into the last 8 bytes (big-endian).
		for i := 0; i < 8; i++ {
			id[8+i] = byte(c >> (8 * (7 - i)))
		}
		return id, nil
	}
}

// fixtureClock returns a clock that advances by 1ms on every
// call. SAFE for concurrent use: a mutex guards the cursor
// so the `TestSync_AdvisoryLock_*` tests that exercise
// concurrent engine calls do not race on the time advance.
//
// Used by tests that need to assert distinct `CreatedAt`
// timestamps across multiple engine runs without fighting
// against time.Now's monotonic resolution.
func fixtureClock(base time.Time) func() time.Time {
	var mu sync.Mutex
	cur := base
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t := cur
		cur = cur.Add(time.Millisecond)
		return t
	}
}

// fixture bundles the canonical test setup: a store, a
// policy_version_id pinned to a single SRP-LCOM rule, the
// SRP threshold, and a repo / sha for the run.
type fixture struct {
	store           *InMemoryStore
	engine          *Engine
	policyVersionID uuid.UUID
	thresholdID     uuid.UUID
	ruleID          string
	ruleVersion     int
	repoID          uuid.UUID
}

// newFixture wires the canonical fixture. The SRP rule's
// predicate is `metric_kind == 'lcom4' AND value > threshold('<id>')`
// which fires when a sample carries `metric_kind=lcom4` and
// value exceeds the threshold.
func newFixture(t *testing.T) *fixture {
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

	ruleID := "solid.srp.lcom4_high"
	ruleVersion := 1
	store.InsertRule(steward.Rule{
		RuleID:          ruleID,
		Version:         ruleVersion,
		PackID:          "solid",
		PredicateDSL:    "threshold('" + thresholdID.String() + "')",
		SeverityDefault: steward.SeverityBlock,
		DescriptionMD:   "Single-responsibility: LCOM4 exceeds the SRP threshold.",
		CreatedAt:       time.Now(),
	})

	policyVersionID := uuid.Must(uuid.NewV4())
	freshness := 3600
	store.InsertPolicyVersion(steward.PolicyVersion{
		PolicyVersionID: policyVersionID,
		Name:            "stage-5.7-test-policy",
		RuleRefs:        []steward.RuleRef{{RuleID: ruleID, Version: ruleVersion}},
		ThresholdRefs:   []steward.ThresholdRef{{ThresholdID: thresholdID}},
		RefactorWeights: steward.RefactorWeights{
			Alpha:                  0.4,
			Beta:                   0.3,
			Gamma:                  0.2,
			Delta:                  0.1,
			EffortModelVersion:     "v0",
			WindowDays:             30,
			FreshnessWindowSeconds: &freshness,
		},
		Signature: []byte("test-signature"),
		CreatedAt: time.Now(),
	})

	repoID := uuid.Must(uuid.NewV4())

	engine, err := New(Config{
		Store: store,
		Cache: dsl.NewCache(),
		Clock: fixtureClock(time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)),
		NewID: deterministicIDGen(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return &fixture{
		store:           store,
		engine:          engine,
		policyVersionID: policyVersionID,
		thresholdID:     thresholdID,
		ruleID:          ruleID,
		ruleVersion:     ruleVersion,
		repoID:          repoID,
	}
}

// sample builds an LCOM4 sample at the given scope and
// value. The scope_signature is derived from the scope_id so
// override-glob tests have a stable string to match.
func (f *fixture) sample(scopeID uuid.UUID, value float64) Sample {
	return Sample{
		Sample: dsl.Sample{
			SampleID:      uuid.Must(uuid.NewV4()),
			RepoID:        f.repoID,
			SHA:           "ignored-by-eval", // engine ignores this field; the run loop pins SHA
			ScopeID:       scopeID,
			ScopeKind:     "class",
			MetricKind:    "lcom4",
			MetricVersion: 1,
			Value:         value,
			HasValue:      true,
			Pack:          "solid",
			Source:        "computed",
			Degraded:      false,
		},
		ScopeSignature: "com.example.Class_" + scopeID.String()[:8],
	}
}

// quietLogger returns a slog.Logger that discards every
// record. Used by worker tests that don't want log noise on
// stdout.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestEngine_New_RefusesNilStore(t *testing.T) {
	t.Parallel()
	_, err := New(Config{Store: nil})
	if err != ErrStoreUnwired {
		t.Fatalf("err=%v; want ErrStoreUnwired", err)
	}
}

func TestEngine_New_AppliesDefaults(t *testing.T) {
	t.Parallel()
	store := NewInMemoryStore()
	e, err := New(Config{Store: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.cache == nil {
		t.Error("default Cache is nil")
	}
	if e.clock == nil {
		t.Error("default Clock is nil")
	}
	if e.newID == nil {
		t.Error("default NewID is nil")
	}
}

func TestEngine_RunSync_RefusesZeroInputs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tests := []struct {
		name            string
		repoID          uuid.UUID
		sha             string
		policyVersionID uuid.UUID
	}{
		{"zero repo", uuid.Nil, "abc", f.policyVersionID},
		{"empty sha", f.repoID, "", f.policyVersionID},
		{"zero policy", f.repoID, "abc", uuid.Nil},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := f.engine.RunSync(context.Background(), tc.repoID, tc.sha, nil, tc.policyVersionID)
			if err == nil {
				t.Fatal("expected error; got nil")
			}
		})
	}
}

func TestEngine_RunSync_UnknownPolicyVersionRejected(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	unknown := uuid.Must(uuid.NewV4())
	_, err := f.engine.RunSync(context.Background(), f.repoID, "sha1", nil, unknown)
	if err == nil {
		t.Fatal("expected error; got nil")
	}
	if !errorsIsUnknownPolicy(err) {
		t.Errorf("want ErrUnknownPolicyVersion; got %v", err)
	}
}

// errorsIsUnknownPolicy checks for our sentinel without
// importing the std errors package twice -- keeps the
// import block tidy in tests.
func errorsIsUnknownPolicy(err error) bool {
	for err != nil {
		if err == ErrUnknownPolicyVersion {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestEngine_RunSync_NoSamples_PassVerdict_NoFindings(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	result, err := f.engine.RunSync(context.Background(), f.repoID, "sha-empty", nil, f.policyVersionID)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if result.Verdict != VerdictPass {
		t.Errorf("Verdict=%s; want pass", result.Verdict)
	}
	if len(result.FindingIDs) != 0 {
		t.Errorf("len(FindingIDs)=%d; want 0", len(result.FindingIDs))
	}
	if len(f.store.Runs()) != 1 {
		t.Errorf("runs=%d; want 1", len(f.store.Runs()))
	}
	if len(f.store.Verdicts()) != 1 {
		t.Errorf("verdicts=%d; want 1", len(f.store.Verdicts()))
	}
	if len(f.store.Findings()) != 0 {
		t.Errorf("findings=%d; want 0", len(f.store.Findings()))
	}
}

func TestEngine_RunSync_FiresOnPredicateHit_FindingHasCanonicalSchema(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scopeID := uuid.Must(uuid.NewV4())
	sm := f.sample(scopeID, 12) // exceeds threshold (10)
	f.store.InsertSamples(f.repoID, "sha1", []Sample{sm})

	result, err := f.engine.RunSync(context.Background(), f.repoID, "sha1", nil, f.policyVersionID)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if result.Verdict != VerdictBlock {
		t.Errorf("Verdict=%s; want block (rule severity = block)", result.Verdict)
	}
	if len(result.FindingIDs) != 1 {
		t.Fatalf("len(FindingIDs)=%d; want 1", len(result.FindingIDs))
	}
	findings := f.store.Findings()
	if len(findings) != 1 {
		t.Fatalf("findings=%d; want 1", len(findings))
	}
	got := findings[0]
	// Canonical schema column assertions:
	if got.FindingID == uuid.Nil {
		t.Error("finding_id is zero uuid")
	}
	if got.EvaluationRunID != result.EvaluationRunID {
		t.Errorf("evaluation_run_id=%s; want %s", got.EvaluationRunID, result.EvaluationRunID)
	}
	if got.RepoID != f.repoID {
		t.Errorf("repo_id=%s; want %s", got.RepoID, f.repoID)
	}
	if got.SHA != "sha1" {
		t.Errorf("sha=%q; want sha1", got.SHA)
	}
	if got.ScopeID != scopeID {
		t.Errorf("scope_id=%s; want %s", got.ScopeID, scopeID)
	}
	if got.RuleID != f.ruleID {
		t.Errorf("rule_id=%s; want %s", got.RuleID, f.ruleID)
	}
	if got.RuleVersion != f.ruleVersion {
		t.Errorf("rule_version=%d; want %d", got.RuleVersion, f.ruleVersion)
	}
	if got.PolicyVersionID != f.policyVersionID {
		t.Errorf("policy_version_id=%s; want %s", got.PolicyVersionID, f.policyVersionID)
	}
	if len(got.MetricSampleIDs) != 1 || got.MetricSampleIDs[0] != sm.SampleID {
		t.Errorf("metric_sample_ids=%v; want [%s]", got.MetricSampleIDs, sm.SampleID)
	}
	if got.Severity != steward.SeverityBlock {
		t.Errorf("severity=%s; want block", got.Severity)
	}
	if got.Delta != DeltaNew {
		t.Errorf("delta=%s; want new (no prior finding)", got.Delta)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at is zero time")
	}
}

func TestEngine_RunSync_PredicateMiss_NoFinding(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scopeID := uuid.Must(uuid.NewV4())
	// value below the threshold (10) -> rule does not fire.
	f.store.InsertSamples(f.repoID, "sha1", []Sample{f.sample(scopeID, 5)})
	result, err := f.engine.RunSync(context.Background(), f.repoID, "sha1", nil, f.policyVersionID)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if result.Verdict != VerdictPass {
		t.Errorf("Verdict=%s; want pass", result.Verdict)
	}
	if len(result.FindingIDs) != 0 {
		t.Errorf("len(FindingIDs)=%d; want 0", len(result.FindingIDs))
	}
}

func TestEngine_RunSync_ScopeFilterRestrictsToOneScope(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scopeA := uuid.Must(uuid.NewV4())
	scopeB := uuid.Must(uuid.NewV4())
	f.store.InsertSamples(f.repoID, "sha1", []Sample{
		f.sample(scopeA, 12),
		f.sample(scopeB, 12),
	})

	result, err := f.engine.RunSync(context.Background(), f.repoID, "sha1", &scopeA, f.policyVersionID)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if len(result.FindingIDs) != 1 {
		t.Fatalf("len(FindingIDs)=%d; want 1 (scope filter)", len(result.FindingIDs))
	}
	findings := f.store.Findings()
	if findings[0].ScopeID != scopeA {
		t.Errorf("finding.scope_id=%s; want %s", findings[0].ScopeID, scopeA)
	}
}

func TestEngine_RunSync_MultipleMatchingSamples_SingleFindingWithSampleArray(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scopeID := uuid.Must(uuid.NewV4())
	a := f.sample(scopeID, 12)
	b := f.sample(scopeID, 15)
	f.store.InsertSamples(f.repoID, "sha1", []Sample{a, b})

	result, err := f.engine.RunSync(context.Background(), f.repoID, "sha1", nil, f.policyVersionID)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if len(result.FindingIDs) != 1 {
		t.Fatalf("len(FindingIDs)=%d; want 1 (same scope, same rule)", len(result.FindingIDs))
	}
	got := f.store.Findings()[0]
	if len(got.MetricSampleIDs) != 2 {
		t.Fatalf("metric_sample_ids len=%d; want 2", len(got.MetricSampleIDs))
	}
	// Both sample IDs MUST be present (order is deterministic
	// in the engine: it iterates the sample slice in input
	// order).
	gotIDs := map[uuid.UUID]bool{}
	for _, id := range got.MetricSampleIDs {
		gotIDs[id] = true
	}
	for _, want := range []uuid.UUID{a.SampleID, b.SampleID} {
		if !gotIDs[want] {
			t.Errorf("missing sample_id %s in metric_sample_ids", want)
		}
	}
}

func TestEngine_RunBatch_CallerStampIsBatchRefresh(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.store.InsertSamples(f.repoID, "sha1", []Sample{f.sample(uuid.Must(uuid.NewV4()), 12)})

	_, err := f.engine.RunBatch(context.Background(), f.repoID, "sha1", f.policyVersionID)
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	runs := f.store.Runs()
	if len(runs) != 1 {
		t.Fatalf("runs=%d; want 1", len(runs))
	}
	if runs[0].Caller != CallerBatchRefresh {
		t.Errorf("caller=%s; want batch_refresh", runs[0].Caller)
	}
}

func TestEngine_RunSync_CallerStampIsEvalGate(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, err := f.engine.RunSync(context.Background(), f.repoID, "sha1", nil, f.policyVersionID)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	runs := f.store.Runs()
	if len(runs) != 1 {
		t.Fatalf("runs=%d; want 1", len(runs))
	}
	if runs[0].Caller != CallerEvalGate {
		t.Errorf("caller=%s; want eval_gate", runs[0].Caller)
	}
}

func TestEngine_RunSync_MutedScopeProducesNoFinding(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scopeID := uuid.Must(uuid.NewV4())
	sm := f.sample(scopeID, 12)
	f.store.InsertSamples(f.repoID, "sha1", []Sample{sm})

	// Insert a mute override for this rule at the EXACT
	// scope signature.
	f.store.InsertOverride(steward.Override{
		OverrideID: uuid.Must(uuid.NewV4()),
		RuleID:     f.ruleID,
		ScopeFilter: steward.ScopeFilter{
			RepoID:             f.repoID.String(),
			ScopeKind:          steward.ScopeKindClass,
			ScopeSignatureGlob: sm.ScopeSignature,
		},
		Mute:      true,
		Reason:    "noisy in legacy module pending refactor",
		ActorID:   "operator@example.com",
		CreatedAt: time.Now(),
	})

	result, err := f.engine.RunSync(context.Background(), f.repoID, "sha1", nil, f.policyVersionID)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if len(result.FindingIDs) != 0 {
		t.Fatalf("muted scope produced %d findings; want 0", len(result.FindingIDs))
	}
	if result.Verdict != VerdictPass {
		t.Errorf("Verdict=%s; want pass (muted)", result.Verdict)
	}
}

func TestEngine_RunSync_MutedScopeRecoversWhenUnmuted(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scopeID := uuid.Must(uuid.NewV4())
	sm := f.sample(scopeID, 12)
	f.store.InsertSamples(f.repoID, "sha1", []Sample{sm})

	// Mute, then immediately unmute via a NEWER row -- the
	// latest-row-wins semantic ([steward.InMemoryStore]
	// LatestMatchingOverride) means the second row wins.
	muteCreated := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	unmuteCreated := muteCreated.Add(time.Minute)
	f.store.InsertOverride(steward.Override{
		OverrideID: uuid.Must(uuid.NewV4()),
		RuleID:     f.ruleID,
		ScopeFilter: steward.ScopeFilter{
			RepoID:             f.repoID.String(),
			ScopeKind:          steward.ScopeKindClass,
			ScopeSignatureGlob: sm.ScopeSignature,
		},
		Mute:      true,
		Reason:    "temp mute",
		ActorID:   "operator",
		CreatedAt: muteCreated,
	})
	f.store.InsertOverride(steward.Override{
		OverrideID: uuid.Must(uuid.NewV4()),
		RuleID:     f.ruleID,
		ScopeFilter: steward.ScopeFilter{
			RepoID:             f.repoID.String(),
			ScopeKind:          steward.ScopeKindClass,
			ScopeSignatureGlob: sm.ScopeSignature,
		},
		Mute:      false,
		Reason:    "",
		ActorID:   "operator",
		CreatedAt: unmuteCreated,
	})

	result, err := f.engine.RunSync(context.Background(), f.repoID, "sha1", nil, f.policyVersionID)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if len(result.FindingIDs) != 1 {
		t.Errorf("unmuted run produced %d findings; want 1", len(result.FindingIDs))
	}
}

func TestEngine_RunSync_DeltaNewlyFailingOnSeverityRise(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scopeID := uuid.Must(uuid.NewV4())

	// SHA1: seed a prior `warn` finding directly (simulates
	// a prior run where the rule fired at lower severity).
	priorFindingID := uuid.Must(uuid.NewV4())
	priorRunID := uuid.Must(uuid.NewV4())
	priorVerdictID := uuid.Must(uuid.NewV4())
	priorTime := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	if err := f.store.AppendEvaluation(context.Background(),
		EvaluationRun{
			EvaluationRunID: priorRunID,
			RepoID:          f.repoID,
			SHA:             "sha-prior",
			PolicyVersionID: f.policyVersionID,
			Caller:          CallerBatchRefresh,
			CreatedAt:       priorTime,
		},
		EvaluationVerdict{
			VerdictID:       priorVerdictID,
			EvaluationRunID: priorRunID,
			Verdict:         VerdictWarn,
			CreatedAt:       priorTime,
		},
		[]Finding{{
			FindingID:       priorFindingID,
			EvaluationRunID: priorRunID,
			RepoID:          f.repoID,
			SHA:             "sha-prior",
			ScopeID:         scopeID,
			RuleID:          f.ruleID,
			RuleVersion:     f.ruleVersion,
			PolicyVersionID: f.policyVersionID,
			MetricSampleIDs: []uuid.UUID{},
			Severity:        steward.SeverityWarn,
			Delta:           DeltaNew,
			CreatedAt:       priorTime,
		}},
	); err != nil {
		t.Fatalf("seed prior: %v", err)
	}

	// SHA2: rule fires at severity=block (the fixture
	// rule's default).
	f.store.RegisterCommit(f.repoID, "sha2", "sha-prior")
	f.store.InsertSamples(f.repoID, "sha2", []Sample{f.sample(scopeID, 12)})
	_, err := f.engine.RunSync(context.Background(), f.repoID, "sha2", nil, f.policyVersionID)
	if err != nil {
		t.Fatalf("RunSync sha2: %v", err)
	}

	// Find the freshly-written finding for sha2.
	var newFinding *Finding
	for _, fnd := range f.store.Findings() {
		fnd := fnd
		if fnd.SHA == "sha2" {
			newFinding = &fnd
			break
		}
	}
	if newFinding == nil {
		t.Fatal("no finding written for sha2")
	}
	if newFinding.Delta != DeltaNewlyFailing {
		t.Errorf("delta=%s; want newly_failing (prior=warn -> current=block)", newFinding.Delta)
	}
}

func TestEngine_RunSync_DeltaUnchangedOnBlockToBlock(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scopeID := uuid.Must(uuid.NewV4())

	// Seed a prior `block` finding.
	priorTime := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	priorRunID := uuid.Must(uuid.NewV4())
	if err := f.store.AppendEvaluation(context.Background(),
		EvaluationRun{
			EvaluationRunID: priorRunID,
			RepoID:          f.repoID,
			SHA:             "sha-prior",
			PolicyVersionID: f.policyVersionID,
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
			PolicyVersionID: f.policyVersionID,
			Severity:        steward.SeverityBlock,
			Delta:           DeltaNew,
			CreatedAt:       priorTime,
		}},
	); err != nil {
		t.Fatalf("seed prior: %v", err)
	}

	f.store.RegisterCommit(f.repoID, "sha2", "sha-prior")
	f.store.InsertSamples(f.repoID, "sha2", []Sample{f.sample(scopeID, 12)})
	_, err := f.engine.RunSync(context.Background(), f.repoID, "sha2", nil, f.policyVersionID)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}

	var newFinding *Finding
	for _, fnd := range f.store.Findings() {
		fnd := fnd
		if fnd.SHA == "sha2" {
			newFinding = &fnd
			break
		}
	}
	if newFinding == nil {
		t.Fatal("no finding written for sha2")
	}
	if newFinding.Delta != DeltaUnchanged {
		t.Errorf("delta=%s; want unchanged", newFinding.Delta)
	}
}

func TestEngine_RunSync_DeltaResolvedWhenPriorBlockNoLongerPresent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scopeID := uuid.Must(uuid.NewV4())

	// Seed a prior block finding.
	priorTime := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	priorRunID := uuid.Must(uuid.NewV4())
	if err := f.store.AppendEvaluation(context.Background(),
		EvaluationRun{
			EvaluationRunID: priorRunID,
			RepoID:          f.repoID,
			SHA:             "sha-prior",
			PolicyVersionID: f.policyVersionID,
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
			PolicyVersionID: f.policyVersionID,
			Severity:        steward.SeverityBlock,
			Delta:           DeltaNew,
			CreatedAt:       priorTime,
		}},
	); err != nil {
		t.Fatalf("seed prior: %v", err)
	}

	// sha2: no samples -> rule does not fire. Engine MUST
	// emit a resolved finding for the prior-block tuple.
	f.store.RegisterCommit(f.repoID, "sha2", "sha-prior")
	_, err := f.engine.RunSync(context.Background(), f.repoID, "sha2", nil, f.policyVersionID)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}

	var resolved *Finding
	for _, fnd := range f.store.Findings() {
		fnd := fnd
		if fnd.SHA == "sha2" {
			resolved = &fnd
			break
		}
	}
	if resolved == nil {
		t.Fatal("no resolved finding emitted for sha2")
	}
	if resolved.Delta != DeltaResolved {
		t.Errorf("delta=%s; want resolved", resolved.Delta)
	}
	if resolved.Severity != steward.SeverityInfo {
		t.Errorf("severity=%s; want info on resolved row", resolved.Severity)
	}
	if resolved.ScopeID != scopeID || resolved.RuleID != f.ruleID {
		t.Errorf("resolved finding mis-attributes scope/rule: scope=%s rule=%s", resolved.ScopeID, resolved.RuleID)
	}
}

func TestEngine_RunSync_NoDuplicateResolvedRowAfterResolution(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scopeID := uuid.Must(uuid.NewV4())

	// Prior: block at sha-prior.
	priorTime := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	if err := f.store.AppendEvaluation(context.Background(),
		EvaluationRun{
			EvaluationRunID: uuid.Must(uuid.NewV4()),
			RepoID:          f.repoID,
			SHA:             "sha-prior",
			PolicyVersionID: f.policyVersionID,
			Caller:          CallerBatchRefresh,
			CreatedAt:       priorTime,
		},
		EvaluationVerdict{
			VerdictID:       uuid.Must(uuid.NewV4()),
			EvaluationRunID: uuid.Nil, // fixed below
			Verdict:         VerdictBlock,
			CreatedAt:       priorTime,
		},
		[]Finding{},
	); err == nil {
		t.Fatal("AppendEvaluation should refuse zero EvaluationRunID on verdict")
	}
	priorRunID := uuid.Must(uuid.NewV4())
	if err := f.store.AppendEvaluation(context.Background(),
		EvaluationRun{
			EvaluationRunID: priorRunID,
			RepoID:          f.repoID,
			SHA:             "sha-prior",
			PolicyVersionID: f.policyVersionID,
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
			PolicyVersionID: f.policyVersionID,
			Severity:        steward.SeverityBlock,
			Delta:           DeltaNew,
			CreatedAt:       priorTime,
		}},
	); err != nil {
		t.Fatalf("seed prior: %v", err)
	}

	// sha2: resolve (parent = sha-prior).
	f.store.RegisterCommit(f.repoID, "sha2", "sha-prior")
	if _, err := f.engine.RunSync(context.Background(), f.repoID, "sha2", nil, f.policyVersionID); err != nil {
		t.Fatalf("RunSync sha2: %v", err)
	}
	// sha3: should NOT emit a second resolved row -- the
	// latest finding for the tuple is already resolved.
	f.store.RegisterCommit(f.repoID, "sha3", "sha2")
	if _, err := f.engine.RunSync(context.Background(), f.repoID, "sha3", nil, f.policyVersionID); err != nil {
		t.Fatalf("RunSync sha3: %v", err)
	}

	resolvedRows := 0
	for _, fnd := range f.store.Findings() {
		if fnd.Delta == DeltaResolved && fnd.ScopeID == scopeID && fnd.RuleID == f.ruleID {
			resolvedRows++
		}
	}
	if resolvedRows != 1 {
		t.Errorf("resolved rows=%d; want 1 (no duplicate resolved row after the tuple is already resolved)", resolvedRows)
	}
}

func TestEngine_RunSync_DeterministicOrdering(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scopeA := uuid.Must(uuid.NewV4())
	scopeB := uuid.Must(uuid.NewV4())
	f.store.InsertSamples(f.repoID, "sha1", []Sample{
		f.sample(scopeA, 12),
		f.sample(scopeB, 12),
	})
	result, err := f.engine.RunSync(context.Background(), f.repoID, "sha1", nil, f.policyVersionID)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if len(result.FindingIDs) != 2 {
		t.Fatalf("findings=%d; want 2", len(result.FindingIDs))
	}
	// FindingIDs must be sorted ASC (the engine pins this
	// in the run loop so the gate's HTTP response is
	// deterministic).
	want := make([]uuid.UUID, len(result.FindingIDs))
	copy(want, result.FindingIDs)
	sort.Slice(want, func(i, j int) bool { return uuidCompare(want[i], want[j]) < 0 })
	for i := range want {
		if result.FindingIDs[i] != want[i] {
			t.Errorf("FindingIDs[%d]=%s; want %s (sorted)", i, result.FindingIDs[i], want[i])
		}
	}
}

// TestEngine_RunSync_DeltaResolvedWhenPriorBlockDowngradedToLowerSeverity
// pins implementation-plan Stage 5.7 line 556 (iter-5
// evaluator item #4): when a tuple was severity='block' at
// the parent SHA AND is now firing at a strictly lower
// severity (warn / info), the engine MUST emit BOTH the
// current lower-severity finding (delta=new) AND a separate
// `severity=info, delta=resolved` row. Prior to this fix
// computeResolved skipped any still-firing tuple, hiding
// the block→warn downgrade under the warn finding and
// breaking the auditability of "is this block now fixed?".
func TestEngine_RunSync_DeltaResolvedWhenPriorBlockDowngradedToLowerSeverity(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scopeID := uuid.Must(uuid.NewV4())

	// Prior: a block finding at sha-prior for this scope/rule.
	priorTime := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	priorRunID := uuid.Must(uuid.NewV4())
	if err := f.store.AppendEvaluation(context.Background(),
		EvaluationRun{
			EvaluationRunID: priorRunID,
			RepoID:          f.repoID,
			SHA:             "sha-prior",
			PolicyVersionID: f.policyVersionID,
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
			PolicyVersionID: f.policyVersionID,
			Severity:        steward.SeverityBlock,
			Delta:           DeltaNew,
			CreatedAt:       priorTime,
		}},
	); err != nil {
		t.Fatalf("seed prior block: %v", err)
	}

	// Downgrade the rule's severity_default to warn so the
	// next run fires the same rule at a STRICTLY LOWER
	// severity than the seeded prior block. The
	// InMemoryStore's InsertRule overwrites by
	// `(rule_id, version)` so re-inserting with the same
	// version replaces the row.
	f.store.InsertRule(steward.Rule{
		RuleID:          f.ruleID,
		Version:         f.ruleVersion,
		PackID:          "solid",
		PredicateDSL:    "threshold('" + f.thresholdID.String() + "')",
		SeverityDefault: steward.SeverityWarn,
		DescriptionMD:   "Single-responsibility: LCOM4 exceeds the SRP threshold (downgraded to warn).",
		CreatedAt:       time.Now(),
	})

	// sha2: same scope, same lcom4 value still exceeding
	// the threshold. The rule still FIRES but at warn now.
	f.store.RegisterCommit(f.repoID, "sha2", "sha-prior")
	f.store.InsertSamples(f.repoID, "sha2", []Sample{f.sample(scopeID, 12)})
	_, err := f.engine.RunSync(context.Background(), f.repoID, "sha2", nil, f.policyVersionID)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}

	var firingWarn, resolved *Finding
	for i, fnd := range f.store.Findings() {
		fnd := fnd
		if fnd.SHA != "sha2" || fnd.ScopeID != scopeID || fnd.RuleID != f.ruleID {
			continue
		}
		switch fnd.Delta {
		case DeltaNew, DeltaNewlyFailing, DeltaUnchanged:
			firingWarn = &f.store.Findings()[i]
		case DeltaResolved:
			resolved = &f.store.Findings()[i]
		}
	}
	if firingWarn == nil {
		t.Fatal("no firing finding at sha2; expected the downgraded rule to still fire at warn")
	}
	if firingWarn.Severity != steward.SeverityWarn {
		t.Errorf("firing severity=%s; want warn (rule was downgraded)", firingWarn.Severity)
	}
	if resolved == nil {
		t.Fatal("no `delta=resolved` row for the prior block; the block->warn downgrade was not audited (impl-plan Stage 5.7 line 556)")
	}
	if resolved.Severity != steward.SeverityInfo {
		t.Errorf("resolved severity=%s; want info on resolved row", resolved.Severity)
	}
	if resolved.FindingID == firingWarn.FindingID {
		t.Error("resolved row and firing row share the same finding_id; schema permits multiple findings per (run, rule, scope) and the engine MUST mint distinct ids")
	}
}

// TestEngine_RunSync_DedupsConcurrentSameArgs pins iter-5
// evaluator item #3 / implementation-plan Stage 5.7 line
// 559: two PARALLEL [Engine.RunSync] calls for the same
// `(repo, sha, scope, policy_version)` produce a single
// canonical run+verdict; the second caller observes the
// first caller's just-written IDs via the in-process dedup
// cache rather than minting a duplicate audit row.
//
// Distinct from [TestSync_AdvisoryLock_SerialisesSameSHA]:
// that test exercises concurrent calls; this one runs them
// SEQUENTIALLY within the dedup window to lock down the
// cache-hit path independently of the lock-ordering
// behaviour.
func TestEngine_RunSync_DedupsConcurrentSameArgs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scopeID := uuid.Must(uuid.NewV4())
	f.store.InsertSamples(f.repoID, "sha-dedup", []Sample{f.sample(scopeID, 12)})

	ctx := context.Background()
	first, err := f.engine.RunSync(ctx, f.repoID, "sha-dedup", nil, f.policyVersionID)
	if err != nil {
		t.Fatalf("first RunSync: %v", err)
	}
	second, err := f.engine.RunSync(ctx, f.repoID, "sha-dedup", nil, f.policyVersionID)
	if err != nil {
		t.Fatalf("second RunSync: %v", err)
	}

	if first.EvaluationRunID != second.EvaluationRunID {
		t.Errorf("second RunSync got a DIFFERENT EvaluationRunID (%s vs %s); dedup cache miss",
			first.EvaluationRunID, second.EvaluationRunID)
	}
	if first.EvaluationVerdictID != second.EvaluationVerdictID {
		t.Errorf("second RunSync got a DIFFERENT EvaluationVerdictID (%s vs %s)",
			first.EvaluationVerdictID, second.EvaluationVerdictID)
	}
	if len(first.FindingIDs) != len(second.FindingIDs) {
		t.Errorf("FindingIDs len: first=%d second=%d", len(first.FindingIDs), len(second.FindingIDs))
	} else {
		for i := range first.FindingIDs {
			if first.FindingIDs[i] != second.FindingIDs[i] {
				t.Errorf("FindingIDs[%d]: first=%s second=%s", i, first.FindingIDs[i], second.FindingIDs[i])
			}
		}
	}
	if got := len(f.store.Runs()); got != 1 {
		t.Errorf("runs=%d; want 1 (dedup must not write a second audit row)", got)
	}
}

// TestEngine_RunSync_DedupsHonoursTTLBoundary pins the
// negative case: once the dedup TTL elapses, the engine
// mints a NEW canonical row rather than returning stale
// IDs. This satisfies the architecture's
// `every gate call is audit-stamped` contract for calls
// minutes apart while still honouring the
// "parallel calls produce a single row" rule.
func TestEngine_RunSync_DedupsHonoursTTLBoundary(t *testing.T) {
	t.Parallel()

	// Build a fixture with a custom short dedup TTL + a
	// clock we can advance to cross the boundary.
	store := NewInMemoryStore()
	thresholdID := uuid.Must(uuid.NewV4())
	store.InsertThreshold(steward.Threshold{
		ThresholdID: thresholdID, MetricKind: "lcom4",
		ScopeKind: "class", Op: "gt", Value: 10,
		CreatedAt: time.Now(),
	})
	ruleID := "solid.srp.lcom4_high"
	store.InsertRule(steward.Rule{
		RuleID: ruleID, Version: 1, PackID: "solid",
		PredicateDSL:    "threshold('" + thresholdID.String() + "')",
		SeverityDefault: steward.SeverityBlock,
		CreatedAt:       time.Now(),
	})
	pvID := uuid.Must(uuid.NewV4())
	store.InsertPolicyVersion(steward.PolicyVersion{
		PolicyVersionID: pvID, Name: "ttl-test",
		RuleRefs:      []steward.RuleRef{{RuleID: ruleID, Version: 1}},
		ThresholdRefs: []steward.ThresholdRef{{ThresholdID: thresholdID}},
		Signature:     []byte("sig"),
		CreatedAt:     time.Now(),
	})
	repoID := uuid.Must(uuid.NewV4())
	scopeID := uuid.Must(uuid.NewV4())
	store.InsertSamples(repoID, "sha-ttl", []Sample{{
		Sample: dsl.Sample{
			SampleID: uuid.Must(uuid.NewV4()), RepoID: repoID,
			SHA: "ignored", ScopeID: scopeID, ScopeKind: "class",
			MetricKind: "lcom4", MetricVersion: 1, Value: 12,
			HasValue: true, Pack: "solid", Source: "computed",
		},
		ScopeSignature: "com.example.Class",
	}})

	// Steerable clock: mu-guarded cursor we can advance
	// across the TTL boundary between calls.
	var clockMu sync.Mutex
	cur := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		t := cur
		cur = cur.Add(time.Microsecond)
		return t
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		defer clockMu.Unlock()
		cur = cur.Add(d)
	}
	engine, err := New(Config{
		Store:       store,
		Clock:       clock,
		NewID:       deterministicIDGen(),
		RunDedupTTL: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Share the engine's clock with the InMemoryStore so
	// the Store-level dedup lookup (newly enabled for
	// eval_gate by iter-7 evaluator item #2 via the
	// scope_id column in migration 0008) observes the
	// SAME notion of "now" as the engine. Without this
	// the InMemoryStore would ignore the TTL filter
	// (see InMemoryStore.LookupRecentCanonicalRun godoc)
	// and the second call would dedup to the first call's
	// row even after the TTL has elapsed.
	store.SetClock(clock)

	ctx := context.Background()
	first, err := engine.RunSync(ctx, repoID, "sha-ttl", nil, pvID)
	if err != nil {
		t.Fatalf("first RunSync: %v", err)
	}
	// Cross the TTL boundary BEFORE the next call. The
	// engine MUST mint fresh audit IDs now.
	advance(10 * time.Second)
	second, err := engine.RunSync(ctx, repoID, "sha-ttl", nil, pvID)
	if err != nil {
		t.Fatalf("second RunSync: %v", err)
	}
	if first.EvaluationRunID == second.EvaluationRunID {
		t.Error("calls separated by 2x the TTL returned the SAME EvaluationRunID; dedup cache must evict stale entries (audit-stamp contract)")
	}
	if got := len(store.Runs()); got != 2 {
		t.Errorf("runs=%d; want 2 after TTL expiry", got)
	}
}
