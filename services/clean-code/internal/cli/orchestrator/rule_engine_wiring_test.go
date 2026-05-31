// -----------------------------------------------------------------------
// <copyright file="rule_engine_wiring_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package orchestrator_test

import (
	"errors"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/devpolicy"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/orchestrator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/repocontext"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/scopebinding"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// fixedRepoContext returns a deterministic RepoContext the
// rule_engine wiring tests reuse so SampleID assertions can
// pin a known UUID-v5 pre-image.
func fixedRepoContext(t *testing.T) repocontext.RepoContext {
	t.Helper()
	return repocontext.RepoContext{
		RootPath: "/tmp/fixture",
		RepoID:   uuid.Must(uuid.FromString("11111111-1111-4111-8111-111111111111")),
		HeadSHA:  "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	}
}

// seedBinding inserts a [scopebinding.ScopeBinding] for the
// supplied scope ID and returns the same `(path, localID,
// signature, scopeKind)` shape so the caller can build a
// MetricSampleDraft that resolves through BuildSamples.
func seedBinding(t *testing.T, table *scopebinding.Table, scopeID uuid.UUID, signature, path string, kind scope.Kind) {
	t.Helper()
	if err := table.Insert(scopebinding.ScopeBinding{
		ScopeID:   scopeID,
		ScopeKind: string(kind),
		FilePath:  path,
		StartLine: 1,
		EndLine:   10,
		Signature: signature,
		Language:  "go",
	}); err != nil {
		t.Fatalf("seedBinding: Insert: %v", err)
	}
}

func TestBuildSamples_ConvertsDraftPerSec44(t *testing.T) {
	t.Parallel()
	rc := fixedRepoContext(t)
	table := scopebinding.NewTable()

	scopeID := uuid.Must(uuid.FromString("22222222-2222-4222-8222-222222222222"))
	seedBinding(t, table, scopeID, "fixture-sig://pkg/file.go:Foo", "pkg/file.go", scope.KindMethod)

	scopeIDs := map[orchestrator.ScopeBindingKey]uuid.UUID{
		{Path: "pkg/file.go", LocalID: "local:1"}: scopeID,
	}

	drafts := []recipes.MetricSampleDraft{
		{
			MetricKind:    "loc",
			MetricVersion: 1,
			Pack:          recipes.PackBase,
			Source:        recipes.SourceComputed,
			Value:         42,
			Scope: recipes.ScopeRef{
				LocalID:       "local:1",
				Kind:          scope.KindMethod,
				QualifiedName: "Foo",
				Path:          "pkg/file.go",
			},
		},
	}

	got := orchestrator.BuildSamples(rc, drafts, table, scopeIDs)
	if len(got) != 1 {
		t.Fatalf("BuildSamples len = %d, want 1", len(got))
	}
	s := got[0]
	if s.RepoID != rc.RepoID {
		t.Errorf("RepoID = %v, want %v", s.RepoID, rc.RepoID)
	}
	if s.SHA != rc.HeadSHA {
		t.Errorf("SHA = %q, want %q", s.SHA, rc.HeadSHA)
	}
	if s.ScopeID != scopeID {
		t.Errorf("ScopeID = %v, want %v", s.ScopeID, scopeID)
	}
	if s.ScopeKind != string(scope.KindMethod) {
		t.Errorf("ScopeKind = %q, want %q", s.ScopeKind, string(scope.KindMethod))
	}
	if s.MetricKind != "loc" {
		t.Errorf("MetricKind = %q, want loc", s.MetricKind)
	}
	if s.MetricVersion != 1 {
		t.Errorf("MetricVersion = %d, want 1", s.MetricVersion)
	}
	if s.Value != 42 {
		t.Errorf("Value = %v, want 42", s.Value)
	}
	if !s.HasValue {
		t.Errorf("HasValue = false, want true")
	}
	if s.Pack != string(recipes.PackBase) {
		t.Errorf("Pack = %q, want %q", s.Pack, string(recipes.PackBase))
	}
	if s.Source != string(recipes.SourceComputed) {
		t.Errorf("Source = %q, want %q", s.Source, string(recipes.SourceComputed))
	}
	if s.ScopeSignature != "fixture-sig://pkg/file.go:Foo" {
		t.Errorf("ScopeSignature = %q, want fixture-sig://...", s.ScopeSignature)
	}
	if s.SampleID == uuid.Nil {
		t.Errorf("SampleID = uuid.Nil, want deterministic non-zero UUID")
	}
}

func TestBuildSamples_DeterministicSampleID(t *testing.T) {
	t.Parallel()
	rc := fixedRepoContext(t)
	table := scopebinding.NewTable()
	scopeID := uuid.Must(uuid.FromString("33333333-3333-4333-8333-333333333333"))
	seedBinding(t, table, scopeID, "sig://stable", "f.go", scope.KindFile)

	scopeIDs := map[orchestrator.ScopeBindingKey]uuid.UUID{
		{Path: "f.go", LocalID: "local:1"}: scopeID,
	}
	drafts := []recipes.MetricSampleDraft{{
		MetricKind:    "loc",
		MetricVersion: 1,
		Pack:          recipes.PackBase,
		Source:        recipes.SourceComputed,
		Value:         10,
		Scope:         recipes.ScopeRef{LocalID: "local:1", Kind: scope.KindFile, Path: "f.go"},
	}}

	a := orchestrator.BuildSamples(rc, drafts, table, scopeIDs)
	b := orchestrator.BuildSamples(rc, drafts, table, scopeIDs)
	if a[0].SampleID != b[0].SampleID {
		t.Errorf("SampleID drift across calls: a=%v b=%v", a[0].SampleID, b[0].SampleID)
	}
	want := orchestrator.MintSampleID(rc.RepoID, rc.HeadSHA, scopeID, "loc", 1)
	if a[0].SampleID != want {
		t.Errorf("SampleID = %v, want %v (per MintSampleID)", a[0].SampleID, want)
	}
}

func TestBuildSamples_SkipsUnresolvedScopeOrMissingBinding(t *testing.T) {
	t.Parallel()
	rc := fixedRepoContext(t)
	table := scopebinding.NewTable()
	scopeID := uuid.Must(uuid.FromString("44444444-4444-4444-8444-444444444444"))
	// Deliberately do NOT seed table.Insert -- the (path,
	// local_id) key resolves but the binding lookup misses.

	scopeIDs := map[orchestrator.ScopeBindingKey]uuid.UUID{
		{Path: "has-id.go", LocalID: "local:1"}: scopeID,
	}
	drafts := []recipes.MetricSampleDraft{
		{
			// Resolved scope id but no binding row -- skipped.
			MetricKind:    "loc",
			MetricVersion: 1,
			Pack:          recipes.PackBase,
			Source:        recipes.SourceComputed,
			Value:         1,
			Scope:         recipes.ScopeRef{LocalID: "local:1", Kind: scope.KindFile, Path: "has-id.go"},
		},
		{
			// No scope id at all -- skipped.
			MetricKind:    "loc",
			MetricVersion: 1,
			Pack:          recipes.PackBase,
			Source:        recipes.SourceComputed,
			Value:         1,
			Scope:         recipes.ScopeRef{LocalID: "local:99", Kind: scope.KindFile, Path: "missing.go"},
		},
	}

	got := orchestrator.BuildSamples(rc, drafts, table, scopeIDs)
	if len(got) != 0 {
		t.Errorf("BuildSamples len = %d, want 0 (both drafts unresolved)", len(got))
	}
}

func TestLoadStore_SeedsPolicyRulesAndSamples(t *testing.T) {
	t.Parallel()
	rc := fixedRepoContext(t)

	pvID := uuid.Must(uuid.FromString("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	bundle := devpolicy.Bundle{
		PolicyVersion: steward.PolicyVersion{
			PolicyVersionID: pvID,
			Name:            "cleanc-dev-policy",
			CreatedAt:       time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC),
		},
		Rules: []steward.Rule{
			{RuleID: "loc.long_method", Version: 1, PackID: "base", PredicateDSL: "metric_kind == 'loc' AND value >= 5", SeverityDefault: steward.SeverityWarn},
			{RuleID: "loc.long_file", Version: 1, PackID: "base", PredicateDSL: "metric_kind == 'loc' AND value >= 1000", SeverityDefault: steward.SeverityBlock},
		},
	}

	samples := []rule_engine.Sample{}
	store, err := orchestrator.LoadStore(bundle, samples, rc)
	if err != nil {
		t.Fatalf("LoadStore: err=%v, want nil", err)
	}
	if store == nil {
		t.Fatalf("LoadStore: store is nil")
	}

	// Policy version round-trips.
	pv, err := store.GetPolicyVersion(t.Context(), pvID)
	if err != nil {
		t.Fatalf("GetPolicyVersion: %v", err)
	}
	if pv.PolicyVersionID != pvID {
		t.Errorf("PolicyVersion.PolicyVersionID = %v, want %v", pv.PolicyVersionID, pvID)
	}

	// Both rules round-trip.
	if _, err := store.GetRule(t.Context(), "loc.long_method", 1); err != nil {
		t.Errorf("GetRule(loc.long_method, 1): %v", err)
	}
	if _, err := store.GetRule(t.Context(), "loc.long_file", 1); err != nil {
		t.Errorf("GetRule(loc.long_file, 1): %v", err)
	}
}

func TestLoadStore_NilSamplesReturnsPreconditionError(t *testing.T) {
	t.Parallel()
	rc := fixedRepoContext(t)
	bundle := devpolicy.Bundle{
		PolicyVersion: steward.PolicyVersion{
			PolicyVersionID: uuid.Must(uuid.FromString("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")),
			Name:            "cleanc-dev-policy",
		},
	}
	store, err := orchestrator.LoadStore(bundle, nil, rc)
	if err == nil {
		t.Fatalf("LoadStore(nil samples): err=nil, want ErrLoadStoreNilSamples")
	}
	if !errors.Is(err, orchestrator.ErrLoadStoreNilSamples) {
		t.Errorf("LoadStore err: got %v, want errors.Is ErrLoadStoreNilSamples", err)
	}
	if store != nil {
		t.Errorf("LoadStore(nil samples): store != nil, want nil")
	}
}

func TestLoadStore_SeededSamplesListThrough(t *testing.T) {
	t.Parallel()
	rc := fixedRepoContext(t)
	scopeID := uuid.Must(uuid.FromString("55555555-5555-4555-8555-555555555555"))
	samples := []rule_engine.Sample{{
		ScopeSignature: "sig://x",
		// Embedded dsl.Sample fields filled enough to round-trip:
	}}
	samples[0].SampleID = uuid.Must(uuid.FromString("66666666-6666-4666-8666-666666666666"))
	samples[0].RepoID = rc.RepoID
	samples[0].SHA = rc.HeadSHA
	samples[0].ScopeID = scopeID
	samples[0].ScopeKind = string(scope.KindFile)
	samples[0].MetricKind = "loc"
	samples[0].MetricVersion = 1
	samples[0].Value = 100
	samples[0].HasValue = true
	samples[0].Pack = string(recipes.PackBase)
	samples[0].Source = string(recipes.SourceComputed)

	bundle := devpolicy.Bundle{
		PolicyVersion: steward.PolicyVersion{
			PolicyVersionID: uuid.Must(uuid.FromString("cccccccc-cccc-4ccc-8ccc-cccccccccccc")),
			Name:            "cleanc-dev-policy",
		},
	}
	store, err := orchestrator.LoadStore(bundle, samples, rc)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	rows, err := store.ListMetricSamples(t.Context(), rc.RepoID, rc.HeadSHA, nil)
	if err != nil {
		t.Fatalf("ListMetricSamples: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListMetricSamples len = %d, want 1", len(rows))
	}
	if rows[0].SampleID != samples[0].SampleID {
		t.Errorf("SampleID round-trip: got %v, want %v", rows[0].SampleID, samples[0].SampleID)
	}
}
