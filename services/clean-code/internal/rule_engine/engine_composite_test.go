package rule_engine

import (
	"context"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/dsl"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/steward"
)

// Stage 5.7 evaluator feedback item #7 -- the engine MUST
// fire a composite SOLID rule like SRP
// (`threshold(lcom4) AND threshold(interface_width)`) at a
// class scope when the class has BOTH a high-LCOM4 sample
// AND a wide-interface sample, even though no single
// MetricSample row carries both metric_kinds.
//
// Architecture Sec 3.5.1.a (lines 503-514): "A class with
// `lcom4 >= threshold AND interface_width >= threshold` is
// flagged." This test pins that semantic.
//
// The fix lives in [dsl.Predicate.EvalAtScope] -- a two-
// phase evaluator that first tries per-sample shared-witness
// AND, then falls back to cross-sample threshold-only AND
// when every AND child is a [dsl.ThresholdNode]. Pure
// threshold conjunctions take the cross-sample path; mixed
// `metric_kind == 'lcom4' AND value > 5` stays per-sample
// (rubber-duck correctness rail).
func TestEngine_RunSync_CompositeSRPThresholdAndThreshold(t *testing.T) {
	t.Parallel()
	store := NewInMemoryStore()

	// Two thresholds: one for LCOM4 and one for
	// interface_width. Both at class scope_kind.
	lcomID := uuid.Must(uuid.NewV4())
	store.InsertThreshold(steward.Threshold{
		ThresholdID: lcomID,
		MetricKind:  "lcom4",
		ScopeKind:   "class",
		Op:          "gt",
		Value:       10,
		CreatedAt:   time.Now(),
	})
	widthID := uuid.Must(uuid.NewV4())
	store.InsertThreshold(steward.Threshold{
		ThresholdID: widthID,
		MetricKind:  "interface_width",
		ScopeKind:   "class",
		Op:          "gt",
		Value:       7,
		CreatedAt:   time.Now(),
	})

	// SRP composite rule: AND across two different
	// metric_kinds. Per-sample evaluation cannot satisfy
	// this -- no single sample carries both metric_kinds.
	// EvalAtScope's Phase 2 (cross-sample threshold AND)
	// is the only path that fires this.
	ruleID := "solid.srp.composite_lcom4_and_interface_width"
	predicate := "threshold('" + lcomID.String() + "') AND threshold('" + widthID.String() + "')"
	store.InsertRule(steward.Rule{
		RuleID:          ruleID,
		Version:         1,
		PackID:          "solid",
		PredicateDSL:    predicate,
		SeverityDefault: steward.SeverityBlock,
		DescriptionMD:   "SRP composite: a class flagged for HIGH LCOM4 AND wide interface.",
		CreatedAt:       time.Now(),
	})

	policyVersionID := uuid.Must(uuid.NewV4())
	freshness := 3600
	store.InsertPolicyVersion(steward.PolicyVersion{
		PolicyVersionID: policyVersionID,
		Name:            "stage-5.7-composite-srp",
		RuleRefs:        []steward.RuleRef{{RuleID: ruleID, Version: 1}},
		ThresholdRefs: []steward.ThresholdRef{
			{ThresholdID: lcomID},
			{ThresholdID: widthID},
		},
		RefactorWeights: steward.RefactorWeights{
			Alpha: 0.4, Beta: 0.3, Gamma: 0.2, Delta: 0.1,
			EffortModelVersion:     "v0",
			WindowDays:             30,
			FreshnessWindowSeconds: &freshness,
		},
		Signature: []byte("test-signature"),
		CreatedAt: time.Now(),
	})

	repoID := uuid.Must(uuid.NewV4())
	scopeID := uuid.Must(uuid.NewV4())
	scopeSig := "com.example.WideAndIncoherentClass"

	// One class with TWO samples -- lcom4 sample with
	// value=12 (above threshold 10), interface_width
	// sample with value=10 (above threshold 7).
	lcomSampleID := uuid.Must(uuid.NewV4())
	widthSampleID := uuid.Must(uuid.NewV4())
	store.InsertSamples(repoID, "sha1", []Sample{
		{
			Sample: dsl.Sample{
				SampleID:      lcomSampleID,
				RepoID:        repoID,
				SHA:           "sha1",
				ScopeID:       scopeID,
				ScopeKind:     "class",
				MetricKind:    "lcom4",
				MetricVersion: 1,
				Value:         12,
				HasValue:      true,
				Pack:          "solid",
				Source:        "computed",
			},
			ScopeSignature: scopeSig,
		},
		{
			Sample: dsl.Sample{
				SampleID:      widthSampleID,
				RepoID:        repoID,
				SHA:           "sha1",
				ScopeID:       scopeID,
				ScopeKind:     "class",
				MetricKind:    "interface_width",
				MetricVersion: 1,
				Value:         10,
				HasValue:      true,
				Pack:          "solid",
				Source:        "computed",
			},
			ScopeSignature: scopeSig,
		},
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

	result, err := engine.RunSync(context.Background(), repoID, "sha1", nil, policyVersionID)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if len(result.FindingIDs) != 1 {
		t.Fatalf("FindingIDs=%d; want 1 (the SRP composite rule must fire at the class scope)", len(result.FindingIDs))
	}
	// Both sample IDs must appear in metric_sample_ids --
	// the witness set per [dsl.Predicate.EvalAtScope]
	// Phase 2: union of both threshold atoms' contributing
	// samples.
	findings := store.Findings()
	if len(findings) != 1 {
		t.Fatalf("findings=%d; want 1", len(findings))
	}
	got := findings[0]
	if got.RuleID != ruleID {
		t.Errorf("rule_id=%s; want %s", got.RuleID, ruleID)
	}
	if got.Severity != steward.SeverityBlock {
		t.Errorf("severity=%s; want block (rule's default)", got.Severity)
	}
	if len(got.MetricSampleIDs) != 2 {
		t.Fatalf("metric_sample_ids count=%d; want 2 (one lcom4 + one interface_width witness)", len(got.MetricSampleIDs))
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range got.MetricSampleIDs {
		seen[id] = true
	}
	if !seen[lcomSampleID] {
		t.Error("metric_sample_ids missing the lcom4 witness")
	}
	if !seen[widthSampleID] {
		t.Error("metric_sample_ids missing the interface_width witness")
	}
}

// Negative companion: when ONLY ONE of the threshold atoms
// has a satisfying sample at the scope, the composite SRP
// rule MUST NOT fire. Pinning this guards against the
// permissive "any subset of ANDs matches" misfire.
func TestEngine_RunSync_CompositeSRP_OneThresholdMissing_NoFinding(t *testing.T) {
	t.Parallel()
	store := NewInMemoryStore()

	lcomID := uuid.Must(uuid.NewV4())
	store.InsertThreshold(steward.Threshold{
		ThresholdID: lcomID, MetricKind: "lcom4", ScopeKind: "class",
		Op: "gt", Value: 10, CreatedAt: time.Now(),
	})
	widthID := uuid.Must(uuid.NewV4())
	store.InsertThreshold(steward.Threshold{
		ThresholdID: widthID, MetricKind: "interface_width", ScopeKind: "class",
		Op: "gt", Value: 7, CreatedAt: time.Now(),
	})
	ruleID := "solid.srp.composite"
	predicate := "threshold('" + lcomID.String() + "') AND threshold('" + widthID.String() + "')"
	store.InsertRule(steward.Rule{
		RuleID: ruleID, Version: 1, PackID: "solid",
		PredicateDSL: predicate, SeverityDefault: steward.SeverityBlock,
		DescriptionMD: "SRP composite.", CreatedAt: time.Now(),
	})
	pvID := uuid.Must(uuid.NewV4())
	freshness := 3600
	store.InsertPolicyVersion(steward.PolicyVersion{
		PolicyVersionID: pvID, Name: "test",
		RuleRefs:      []steward.RuleRef{{RuleID: ruleID, Version: 1}},
		ThresholdRefs: []steward.ThresholdRef{{ThresholdID: lcomID}, {ThresholdID: widthID}},
		RefactorWeights: steward.RefactorWeights{
			Alpha: 0.4, Beta: 0.3, Gamma: 0.2, Delta: 0.1,
			EffortModelVersion: "v0", WindowDays: 30, FreshnessWindowSeconds: &freshness,
		},
		Signature: []byte("sig"), CreatedAt: time.Now(),
	})
	repoID := uuid.Must(uuid.NewV4())
	scopeID := uuid.Must(uuid.NewV4())
	// Only lcom4 sample present; no interface_width sample.
	store.InsertSamples(repoID, "sha1", []Sample{
		{
			Sample: dsl.Sample{
				SampleID:   uuid.Must(uuid.NewV4()),
				RepoID:     repoID, SHA: "sha1", ScopeID: scopeID, ScopeKind: "class",
				MetricKind: "lcom4", MetricVersion: 1, Value: 12, HasValue: true,
				Pack: "solid", Source: "computed",
			},
			ScopeSignature: "com.example.LonelyClass",
		},
	})
	engine, err := New(Config{Store: store, Cache: dsl.NewCache(),
		Clock: fixtureClock(time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)),
		NewID: deterministicIDGen()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := engine.RunSync(context.Background(), repoID, "sha1", nil, pvID)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if len(result.FindingIDs) != 0 {
		t.Fatalf("FindingIDs=%d; want 0 (composite SRP must NOT fire when only one threshold's metric_kind is present)", len(result.FindingIDs))
	}
}

// rubber-duck correctness rail #1: a mixed AND combining
// per-sample comparison + threshold MUST NOT misfire by
// drawing operands from two different samples. The
// canonical hazard: `metric_kind == 'lcom4' AND value > 5`
// against a scope where one sample has metric_kind='lcom4'
// value=1 AND a different sample has metric_kind='other'
// value=10 -- the AND must be FALSE because the
// per-sample-correlated atoms cannot share a witness.
//
// Phase 1 of [dsl.Predicate.EvalAtScope] enforces this:
// when ANY AND child is a per-sample atom (here, the
// `metric_kind == 'lcom4'` CompareNode), the cross-sample
// Phase 2 is REJECTED and the AND only matches if a single
// sample satisfies every child.
func TestEngine_RunSync_MixedAND_NoCrossSampleMisfire(t *testing.T) {
	t.Parallel()
	store := NewInMemoryStore()

	ruleID := "test.mixed_and_no_crosssample"
	// Predicate: per-sample metric_kind filter AND
	// per-sample value comparison. Both atoms are
	// per-sample-correlated.
	predicate := "metric_kind == 'lcom4' AND value > 5"
	store.InsertRule(steward.Rule{
		RuleID: ruleID, Version: 1, PackID: "solid",
		PredicateDSL: predicate, SeverityDefault: steward.SeverityBlock,
		DescriptionMD: "Mixed AND test.", CreatedAt: time.Now(),
	})
	pvID := uuid.Must(uuid.NewV4())
	freshness := 3600
	store.InsertPolicyVersion(steward.PolicyVersion{
		PolicyVersionID: pvID, Name: "test",
		RuleRefs:      []steward.RuleRef{{RuleID: ruleID, Version: 1}},
		ThresholdRefs: nil,
		RefactorWeights: steward.RefactorWeights{
			Alpha: 0.4, Beta: 0.3, Gamma: 0.2, Delta: 0.1,
			EffortModelVersion: "v0", WindowDays: 30, FreshnessWindowSeconds: &freshness,
		},
		Signature: []byte("sig"), CreatedAt: time.Now(),
	})
	repoID := uuid.Must(uuid.NewV4())
	scopeID := uuid.Must(uuid.NewV4())
	// Sample A: metric_kind=lcom4, value=1. Atom1 true,
	// atom2 false (value not > 5).
	// Sample B: metric_kind=fan_in, value=10. Atom1 false,
	// atom2 true.
	// No single sample satisfies BOTH -> AND must be false.
	store.InsertSamples(repoID, "sha1", []Sample{
		{
			Sample: dsl.Sample{
				SampleID: uuid.Must(uuid.NewV4()), RepoID: repoID,
				SHA: "sha1", ScopeID: scopeID, ScopeKind: "class",
				MetricKind: "lcom4", MetricVersion: 1, Value: 1, HasValue: true,
				Pack: "solid", Source: "computed",
			},
			ScopeSignature: "com.example.X",
		},
		{
			Sample: dsl.Sample{
				SampleID: uuid.Must(uuid.NewV4()), RepoID: repoID,
				SHA: "sha1", ScopeID: scopeID, ScopeKind: "class",
				MetricKind: "fan_in", MetricVersion: 1, Value: 10, HasValue: true,
				Pack: "solid", Source: "computed",
			},
			ScopeSignature: "com.example.X",
		},
	})
	engine, err := New(Config{Store: store, Cache: dsl.NewCache(),
		Clock: fixtureClock(time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)),
		NewID: deterministicIDGen()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := engine.RunSync(context.Background(), repoID, "sha1", nil, pvID)
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if len(result.FindingIDs) != 0 {
		t.Fatalf("FindingIDs=%d; want 0 -- per-sample-correlated mixed AND must not cross witnesses", len(result.FindingIDs))
	}
}
