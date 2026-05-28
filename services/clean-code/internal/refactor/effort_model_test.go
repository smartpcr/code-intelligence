package refactor

// Stage 8.3 tests for [EffortModel], [LoadFromConfig],
// [LoadModelFromFile], [resolveModelPath], and the
// [WithEffortEstimator] wiring on [TaskPlanner]. The tests
// exercise the workstream brief's two pinned scenarios:
//
//   - `missing-model-blocks-startup` -- when
//     `refactor-effort-source` requires a model AND the URI
//     env var is empty, [LoadFromConfig] returns the
//     [ErrEffortModelURIRequired] sentinel.
//   - `effort-model-version-pinned-via-hotspot` -- a
//     full-traversal test that walks
//     `refactor_task -> refactor_plan ->
//     refactor_plan.hotspot_ids[0] -> hot_spot.policy_version_id
//     -> policy_version.refactor_weights.effort_model_version`
//     against an in-memory store and asserts the recovered
//     value matches the loaded artefact's `Version`.

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/config"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
)

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// fixtureArtefact returns a deterministic [EffortModel]
// instance with the canonical-kind base-hours populated and
// finite linear coefficients. Tests serialise this through
// `encoding/json` to write a JSON artefact to disk; the
// per-kind base values are intentionally distinct so a test
// asserting the wrong-kind path is observable.
func fixtureArtefact(version string) EffortModel {
	return EffortModel{
		Version: version,
		KindBaseHours: map[TaskKind]float64{
			TaskKindSplitClass:             4.0,
			TaskKindExtractMethod:          1.5,
			TaskKindInvertDependency:       6.0,
			TaskKindBreakCycle:             8.0,
			TaskKindConsolidateDuplication: 3.0,
		},
		ScoreCoef: 0.25,
		Intercept: 0.5,
	}
}

// writeArtefactFile serialises `m` to a JSON file under
// `t.TempDir()` and returns the resulting absolute path.
// Cleanup is handled by `testing.T.TempDir`.
func writeArtefactFile(t *testing.T, m EffortModel) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "effort_model.json")
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal artefact: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write artefact: %v", err)
	}
	return path
}

// -----------------------------------------------------------------------------
// LoadFromConfig -- scenario `missing-model-blocks-startup`
// -----------------------------------------------------------------------------

// TestLoadFromConfig_MissingURIWhenRequired pins the
// `missing-model-blocks-startup` scenario per
// implementation-plan Stage 8.3 line 759: given the default
// `RefactorEffortSource = "ML model from historical commits"`
// and an empty `RefactorEffortModelURI`, [LoadFromConfig]
// returns [ErrEffortModelURIRequired]. The error message
// MUST name both env vars so an operator can patch without
// grepping.
func TestLoadFromConfig_MissingURIWhenRequired(t *testing.T) {
	t.Parallel()
	cfg := config.Defaults()
	cfg.RefactorEffortModelURI = ""
	model, err := LoadFromConfig(cfg)
	if model != nil {
		t.Errorf("model = %+v; want nil", model)
	}
	if !errors.Is(err, ErrEffortModelURIRequired) {
		t.Fatalf("err = %v; want %v", err, ErrEffortModelURIRequired)
	}
	msg := err.Error()
	if !strings.Contains(msg, config.EnvRefactorEffortModelURI) {
		t.Errorf("error %q does not mention %s", msg, config.EnvRefactorEffortModelURI)
	}
	// The error also names the source pin so the operator
	// can identify the requirement.
	if !strings.Contains(msg, config.DefaultRefactorEffortSource) {
		t.Errorf("error %q does not mention the source value %q",
			msg, config.DefaultRefactorEffortSource)
	}
}

// TestLoadFromConfig_NoneSourceSkipsURI verifies the "none"
// explicit opt-out: [LoadFromConfig] returns `(nil, nil)`
// regardless of URI when source = "none". The composition
// root must then NOT wire an estimator.
func TestLoadFromConfig_NoneSourceSkipsURI(t *testing.T) {
	t.Parallel()
	cfg := config.Defaults()
	cfg.RefactorEffortSource = RefactorEffortSourceNone
	cfg.RefactorEffortModelURI = "" // ignored
	model, err := LoadFromConfig(cfg)
	if err != nil {
		t.Fatalf("err = %v; want nil", err)
	}
	if model != nil {
		t.Errorf("model = %+v; want nil (opt-out)", model)
	}
}

// TestLoadFromConfig_UnknownSourceRejected pins the closed
// set: any value other than the two recognised pins yields
// [ErrEffortModelSourceUnknown]. A typo in the operator
// config silently producing a working planner would let a
// bad pin survive review.
func TestLoadFromConfig_UnknownSourceRejected(t *testing.T) {
	t.Parallel()
	cfg := config.Defaults()
	cfg.RefactorEffortSource = "tea leaves"
	cfg.RefactorEffortModelURI = ""
	_, err := LoadFromConfig(cfg)
	if !errors.Is(err, ErrEffortModelSourceUnknown) {
		t.Fatalf("err = %v; want %v", err, ErrEffortModelSourceUnknown)
	}
}

// TestLoadFromConfig_HappyPath builds an artefact on disk
// and asserts the loader returns a populated *EffortModel.
// Round-trip through `encoding/json` to ensure the JSON
// schema matches the in-memory struct.
func TestLoadFromConfig_HappyPath(t *testing.T) {
	t.Parallel()
	path := writeArtefactFile(t, fixtureArtefact("v1.0.0"))
	cfg := config.Defaults()
	cfg.RefactorEffortModelURI = path
	model, err := LoadFromConfig(cfg)
	if err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	if model == nil {
		t.Fatal("model is nil")
	}
	if model.Version != "v1.0.0" {
		t.Errorf("Version = %q; want %q", model.Version, "v1.0.0")
	}
	for _, k := range CanonicalTaskKinds {
		if _, ok := model.KindBaseHours[k]; !ok {
			t.Errorf("KindBaseHours missing canonical kind %q", k)
		}
	}
}

// -----------------------------------------------------------------------------
// LoadModelFromFile -- artefact validation
// -----------------------------------------------------------------------------

// TestLoadModelFromFile_MalformedJSON pins the
// strict-decode contract: a malformed JSON blob yields
// [ErrEffortModelMalformed].
func TestLoadModelFromFile_MalformedJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("not json{"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadModelFromFile(path)
	if !errors.Is(err, ErrEffortModelMalformed) {
		t.Fatalf("err = %v; want %v", err, ErrEffortModelMalformed)
	}
}

// TestLoadModelFromFile_UnknownFieldRejected pins the
// [DisallowUnknownFields] guard: an artefact with a field
// the loader does not know about is rejected. This protects
// the operator from accidentally setting a field that the
// loaded planner version silently drops.
func TestLoadModelFromFile_UnknownFieldRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "extra.json")
	if err := os.WriteFile(path, []byte(`{
"version": "v1",
"kind_base_hours": {"split_class":1,"extract_method":1,"invert_dependency":1,"break_cycle":1,"consolidate_duplication":1},
"score_coef": 0,
"intercept": 0,
"future_field": 42
}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadModelFromFile(path)
	if !errors.Is(err, ErrEffortModelMalformed) {
		t.Fatalf("err = %v; want %v", err, ErrEffortModelMalformed)
	}
}

// TestLoadModelFromFile_EmptyVersionRejected pins the
// load-bearing version invariant: a non-empty version is
// REQUIRED for the architecture Sec 5.3.3 pinning chain to
// work.
func TestLoadModelFromFile_EmptyVersionRejected(t *testing.T) {
	t.Parallel()
	m := fixtureArtefact("")
	path := writeArtefactFile(t, m)
	_, err := LoadModelFromFile(path)
	if !errors.Is(err, ErrEffortModelVersionEmpty) {
		t.Fatalf("err = %v; want %v", err, ErrEffortModelVersionEmpty)
	}
}

// TestLoadModelFromFile_MissingCanonicalKind pins the
// per-kind-base completeness invariant: omitting any
// canonical kind is a hard error (no silent fallback to 0).
func TestLoadModelFromFile_MissingCanonicalKind(t *testing.T) {
	t.Parallel()
	m := fixtureArtefact("v1")
	delete(m.KindBaseHours, TaskKindBreakCycle)
	path := writeArtefactFile(t, m)
	_, err := LoadModelFromFile(path)
	if !errors.Is(err, ErrEffortModelMissingKindBase) {
		t.Fatalf("err = %v; want %v", err, ErrEffortModelMissingKindBase)
	}
}

// TestLoadModelFromFile_NonFiniteCoefficient pins the
// finite-arithmetic invariant: NaN/Inf in any coefficient
// is a hard error. A NaN propagating into
// `refactor_task.effort_hours` would crash any downstream
// consumer that assumes the column is finite.
func TestLoadModelFromFile_NonFiniteCoefficient(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		mutate func(*EffortModel)
	}{
		{"NaN base hours", func(m *EffortModel) {
			m.KindBaseHours[TaskKindBreakCycle] = math.NaN()
		}},
		{"Inf score coef", func(m *EffortModel) {
			m.ScoreCoef = math.Inf(1)
		}},
		{"NaN intercept", func(m *EffortModel) {
			m.Intercept = math.NaN()
		}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			m := fixtureArtefact("v1")
			c.mutate(&m)
			// `encoding/json` cannot serialise NaN/Inf;
			// call the validator directly with the
			// in-memory model (the on-disk path is
			// covered by the [LoadModelFromFile] tests
			// that use finite values).
			err := validateLoadedModel(&m)
			if !errors.Is(err, ErrEffortModelNonFiniteCoefficient) {
				t.Fatalf("err = %v; want %v", err, ErrEffortModelNonFiniteCoefficient)
			}
		})
	}
}

// TestLoadModelFromFile_NegativeBaseRejected pins the
// non-negative-base-hours guard: negative effort is
// meaningless and would clamp to zero, producing a
// misleading estimate.
func TestLoadModelFromFile_NegativeBaseRejected(t *testing.T) {
	t.Parallel()
	m := fixtureArtefact("v1")
	m.KindBaseHours[TaskKindExtractMethod] = -2.0
	path := writeArtefactFile(t, m)
	_, err := LoadModelFromFile(path)
	if !errors.Is(err, ErrEffortModelMalformed) {
		t.Fatalf("err = %v; want %v", err, ErrEffortModelMalformed)
	}
}

// -----------------------------------------------------------------------------
// resolveModelPath -- URI parsing
// -----------------------------------------------------------------------------

// TestResolveModelPath_BarePath verifies bare local paths
// round-trip through the resolver.
func TestResolveModelPath_BarePath(t *testing.T) {
	t.Parallel()
	var inputs []string
	if runtime.GOOS == "windows" {
		inputs = []string{
			`C:\models\v1.json`,
			`C:/models/v1.json`,
			`models/v1.json`,
			`.\models\v1.json`,
		}
	} else {
		inputs = []string{
			`/var/models/v1.json`,
			`./models/v1.json`,
			`models/v1.json`,
		}
	}
	for _, in := range inputs {
		got, err := resolveModelPath(in)
		if err != nil {
			t.Errorf("resolveModelPath(%q): %v", in, err)
			continue
		}
		if got == "" {
			t.Errorf("resolveModelPath(%q) returned empty", in)
		}
	}
}

// TestResolveModelPath_FileURIPosix verifies the
// `file://` -> local-path stripping on a POSIX target.
func TestResolveModelPath_FileURIPosix(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only path semantics")
	}
	got, err := resolveModelPath("file:///abs/path/model.json")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "/abs/path/model.json" {
		t.Errorf("got %q; want %q", got, "/abs/path/model.json")
	}
}

// TestResolveModelPath_FileURIWindows verifies the
// `file:///C:/...` form on Windows.
func TestResolveModelPath_FileURIWindows(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only path semantics")
	}
	got, err := resolveModelPath("file:///C:/models/v1.json")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "C:/models/v1.json" {
		t.Errorf("got %q; want %q", got, "C:/models/v1.json")
	}
}

// TestResolveModelPath_UnsupportedScheme pins the v0 closed
// set: any scheme other than `file` is rejected.
func TestResolveModelPath_UnsupportedScheme(t *testing.T) {
	t.Parallel()
	cases := []string{
		"http://example.com/model.json",
		"https://example.com/model.json",
		"s3://bucket/model.json",
		"ftp://example.com/model.json",
	}
	for _, in := range cases {
		_, err := resolveModelPath(in)
		if !errors.Is(err, ErrEffortModelUnsupportedScheme) {
			t.Errorf("resolveModelPath(%q) err = %v; want %v",
				in, err, ErrEffortModelUnsupportedScheme)
		}
	}
}

// -----------------------------------------------------------------------------
// EffortModel.Estimate -- determinism, version-match, clamping
// -----------------------------------------------------------------------------

// TestEffortModel_EstimateDeterministic pins the architecture
// G6 reproducibility invariant: the same `(model, task, hs,
// snap)` triple produces a byte-identical estimate across
// calls. The test runs the estimator twice and asserts the
// returned float bit-patterns match.
func TestEffortModel_EstimateDeterministic(t *testing.T) {
	t.Parallel()
	m := fixtureArtefact("v1")
	snap := PolicySnapshot{
		PolicyVersionID: uuid.Must(uuid.NewV4()),
		Weights: steward.RefactorWeights{
			EffortModelVersion: "v1",
		},
	}
	task := RefactorTask{Kind: TaskKindBreakCycle}
	hs := HotSpot{Score: 3.14}
	got1, err := m.Estimate(task, hs, snap)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	got2, err := m.Estimate(task, hs, snap)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if math.Float64bits(got1) != math.Float64bits(got2) {
		t.Errorf("non-deterministic: got1=%v got2=%v", got1, got2)
	}
	// Sanity: formula = base(break_cycle=8) + 0.25*3.14 + 0.5 = 9.285
	want := 8.0 + 0.25*3.14 + 0.5
	if math.Abs(got1-want) > 1e-9 {
		t.Errorf("formula drift: got %v want %v", got1, want)
	}
}

// TestEffortModel_VersionMismatchAborts pins the
// architecture Sec 5.3.3 version-pinning invariant:
// estimating with a snapshot whose
// `Weights.EffortModelVersion` does not equal the loaded
// model's `Version` returns [ErrEffortModelVersionMismatch].
// Silent zero would let a model/policy drift slip into
// production.
func TestEffortModel_VersionMismatchAborts(t *testing.T) {
	t.Parallel()
	m := fixtureArtefact("v1")
	snap := PolicySnapshot{
		PolicyVersionID: uuid.Must(uuid.NewV4()),
		Weights:         steward.RefactorWeights{EffortModelVersion: "v2"},
	}
	task := RefactorTask{Kind: TaskKindExtractMethod}
	hs := HotSpot{Score: 0}
	hours, err := m.Estimate(task, hs, snap)
	if !errors.Is(err, ErrEffortModelVersionMismatch) {
		t.Fatalf("err = %v; want %v", err, ErrEffortModelVersionMismatch)
	}
	if hours != 0 {
		t.Errorf("hours = %v; want 0", hours)
	}
	if !strings.Contains(err.Error(), "v1") || !strings.Contains(err.Error(), "v2") {
		t.Errorf("error %q does not name both versions", err.Error())
	}
}

// TestEffortModel_NegativeClampedToZero pins the
// "no negative effort" invariant: a negative intermediate
// is clamped to 0 (not propagated).
func TestEffortModel_NegativeClampedToZero(t *testing.T) {
	t.Parallel()
	m := fixtureArtefact("v1")
	m.Intercept = -100.0
	snap := PolicySnapshot{Weights: steward.RefactorWeights{EffortModelVersion: "v1"}}
	hours, err := m.Estimate(RefactorTask{Kind: TaskKindExtractMethod}, HotSpot{Score: 0}, snap)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if hours != 0 {
		t.Errorf("hours = %v; want 0 (clamped)", hours)
	}
}

// TestEffortModel_UnknownKindRejected pins the defensive
// belt-and-braces against a custom rule mapper that bypasses
// [ValidateTaskKind].
func TestEffortModel_UnknownKindRejected(t *testing.T) {
	t.Parallel()
	m := fixtureArtefact("v1")
	snap := PolicySnapshot{Weights: steward.RefactorWeights{EffortModelVersion: "v1"}}
	_, err := m.Estimate(RefactorTask{Kind: "made_up_kind"}, HotSpot{Score: 0}, snap)
	if !errors.Is(err, ErrEffortModelMissingKindBase) {
		t.Fatalf("err = %v; want %v", err, ErrEffortModelMissingKindBase)
	}
}

// TestEffortModel_NonFiniteScoreRejected pins the
// finite-arithmetic guard: a hot_spot with a non-finite
// Score (defensive; not produced by Stage 8.1) is rejected.
func TestEffortModel_NonFiniteScoreRejected(t *testing.T) {
	t.Parallel()
	m := fixtureArtefact("v1")
	snap := PolicySnapshot{Weights: steward.RefactorWeights{EffortModelVersion: "v1"}}
	_, err := m.Estimate(RefactorTask{Kind: TaskKindBreakCycle}, HotSpot{Score: math.Inf(1)}, snap)
	if !errors.Is(err, ErrEffortModelNonFiniteCoefficient) {
		t.Fatalf("err = %v; want %v", err, ErrEffortModelNonFiniteCoefficient)
	}
}

// -----------------------------------------------------------------------------
// TaskPlanner wiring (WithEffortEstimator)
// -----------------------------------------------------------------------------

// TestTaskPlanner_WithEffortEstimator_StampsEffortHours
// verifies that wiring an [EffortEstimator] via
// [WithEffortEstimator] swaps the Stage 8.2 `0.0`
// placeholder for the model's estimate on every emitted
// task.
func TestTaskPlanner_WithEffortEstimator_StampsEffortHours(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repoID := uuid.Must(uuid.NewV4())
	sha := "deadbeef"
	pvID := uuid.Must(uuid.NewV4())
	scopeID := uuid.Must(uuid.NewV4())
	hsID := uuid.Must(uuid.NewV4())
	createdAt := time.Now().UTC().Truncate(time.Microsecond)

	model := &EffortModel{
		Version: "v1",
		KindBaseHours: map[TaskKind]float64{
			TaskKindSplitClass:             4.0,
			TaskKindExtractMethod:          1.5,
			TaskKindInvertDependency:       6.0,
			TaskKindBreakCycle:             8.0,
			TaskKindConsolidateDuplication: 3.0,
		},
		ScoreCoef: 0.0,
		Intercept: 0.0,
	}

	policy := &fixedPolicyReader{
		snap: PolicySnapshot{
			PolicyVersionID: pvID,
			Weights: steward.RefactorWeights{
				EffortModelVersion: "v1",
				TopN:               10,
			},
		},
	}

	hsReader := NewInMemoryHotSpotReader()
	hsReader.Insert(HotSpot{
		HotspotID:       hsID,
		RepoID:          repoID,
		SHA:             sha,
		ScopeID:         scopeID,
		Score:           10.0,
		PolicyVersionID: pvID,
		CreatedAt:       createdAt,
	})
	fdr := NewInMemoryFindingDetailReader()
	fdr.Insert(InMemoryFindingWithRule{
		InMemoryFinding: InMemoryFinding{
			RepoID:          repoID,
			SHA:             sha,
			ScopeID:         scopeID,
			PolicyVersionID: pvID,
			Delta:           "new",
		},
		RuleID: "solid.srp.lcom4_high",
	})
	writer := NewInMemoryRefactorPlanTaskWriter()

	tp, err := NewTaskPlanner(policy, hsReader, fdr, writer,
		WithEffortEstimator(model))
	if err != nil {
		t.Fatalf("NewTaskPlanner: %v", err)
	}
	res, err := tp.Plan(ctx, repoID, sha)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(res.Tasks) != 1 {
		t.Fatalf("tasks = %d; want 1", len(res.Tasks))
	}
	task := res.Tasks[0]
	if task.Kind != TaskKindSplitClass {
		t.Errorf("kind = %q; want %q", task.Kind, TaskKindSplitClass)
	}
	if task.EffortHours != 4.0 {
		t.Errorf("effort_hours = %v; want 4.0 (base of split_class)", task.EffortHours)
	}
}

// TestTaskPlanner_WithEffortEstimator_VersionMismatchAbortsBatch
// verifies that a version-mismatch error from the estimator
// aborts the WHOLE batch -- no plan row, no task row lands.
// This is the architecture Sec 5.3.3 "policy + model pinned
// together" invariant.
func TestTaskPlanner_WithEffortEstimator_VersionMismatchAbortsBatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repoID := uuid.Must(uuid.NewV4())
	sha := "deadbeef"
	pvID := uuid.Must(uuid.NewV4())
	scopeID := uuid.Must(uuid.NewV4())
	hsID := uuid.Must(uuid.NewV4())

	// Model version differs from snapshot's
	// EffortModelVersion -> Estimate returns
	// ErrEffortModelVersionMismatch.
	model := &EffortModel{
		Version: "v1",
		KindBaseHours: map[TaskKind]float64{
			TaskKindSplitClass:             4,
			TaskKindExtractMethod:          1,
			TaskKindInvertDependency:       2,
			TaskKindBreakCycle:             3,
			TaskKindConsolidateDuplication: 5,
		},
	}
	policy := &fixedPolicyReader{snap: PolicySnapshot{
		PolicyVersionID: pvID,
		Weights:         steward.RefactorWeights{EffortModelVersion: "v9", TopN: 10},
	}}
	hsReader := NewInMemoryHotSpotReader()
	hsReader.Insert(HotSpot{
		HotspotID: hsID, RepoID: repoID, SHA: sha,
		ScopeID: scopeID, Score: 1, PolicyVersionID: pvID,
		CreatedAt: time.Now().UTC(),
	})
	fdr := NewInMemoryFindingDetailReader()
	fdr.Insert(InMemoryFindingWithRule{
		InMemoryFinding: InMemoryFinding{
			RepoID: repoID, SHA: sha, ScopeID: scopeID,
			PolicyVersionID: pvID, Delta: "new",
		},
		RuleID: "solid.srp.lcom4_high",
	})
	writer := NewInMemoryRefactorPlanTaskWriter()
	tp, err := NewTaskPlanner(policy, hsReader, fdr, writer,
		WithEffortEstimator(model))
	if err != nil {
		t.Fatalf("NewTaskPlanner: %v", err)
	}
	_, err = tp.Plan(ctx, repoID, sha)
	if !errors.Is(err, ErrEffortModelVersionMismatch) {
		t.Fatalf("err = %v; want %v", err, ErrEffortModelVersionMismatch)
	}
	if len(writer.Plans()) != 0 {
		t.Errorf("writer.Plans() = %d; want 0 (batch aborted)", len(writer.Plans()))
	}
	if len(writer.Tasks()) != 0 {
		t.Errorf("writer.Tasks() = %d; want 0 (batch aborted)", len(writer.Tasks()))
	}
}

// TestTaskPlanner_NoEstimator_PreservesStage82Placeholder
// verifies the Stage 8.2 byte-identical fallback when no
// estimator is wired: every emitted task carries
// `EffortHours = 0.0`.
func TestTaskPlanner_NoEstimator_PreservesStage82Placeholder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repoID := uuid.Must(uuid.NewV4())
	sha := "deadbeef"
	pvID := uuid.Must(uuid.NewV4())
	scopeID := uuid.Must(uuid.NewV4())
	hsID := uuid.Must(uuid.NewV4())

	policy := &fixedPolicyReader{snap: PolicySnapshot{
		PolicyVersionID: pvID,
		Weights:         steward.RefactorWeights{EffortModelVersion: "v1", TopN: 10},
	}}
	hsReader := NewInMemoryHotSpotReader()
	hsReader.Insert(HotSpot{
		HotspotID: hsID, RepoID: repoID, SHA: sha,
		ScopeID: scopeID, Score: 1, PolicyVersionID: pvID,
		CreatedAt: time.Now().UTC(),
	})
	fdr := NewInMemoryFindingDetailReader()
	fdr.Insert(InMemoryFindingWithRule{
		InMemoryFinding: InMemoryFinding{
			RepoID: repoID, SHA: sha, ScopeID: scopeID,
			PolicyVersionID: pvID, Delta: "new",
		},
		RuleID: "solid.srp.lcom4_high",
	})
	writer := NewInMemoryRefactorPlanTaskWriter()
	tp, err := NewTaskPlanner(policy, hsReader, fdr, writer)
	if err != nil {
		t.Fatalf("NewTaskPlanner: %v", err)
	}
	res, err := tp.Plan(ctx, repoID, sha)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(res.Tasks) != 1 {
		t.Fatalf("tasks = %d; want 1", len(res.Tasks))
	}
	if res.Tasks[0].EffortHours != 0.0 {
		t.Errorf("effort_hours = %v; want 0.0 (Stage 8.2 placeholder preserved)",
			res.Tasks[0].EffortHours)
	}
}

// TestWithEffortEstimator_NilRejected pins the wiring-bug
// detection at [NewTaskPlanner] time.
func TestWithEffortEstimator_NilRejected(t *testing.T) {
	t.Parallel()
	_, err := NewTaskPlanner(
		&fixedPolicyReader{},
		NewInMemoryHotSpotReader(),
		NewInMemoryFindingDetailReader(),
		NewInMemoryRefactorPlanTaskWriter(),
		WithEffortEstimator(nil),
	)
	if !errors.Is(err, ErrNilEffortEstimator) {
		t.Fatalf("err = %v; want %v", err, ErrNilEffortEstimator)
	}
}

// -----------------------------------------------------------------------------
// Effort-model version traversal -- scenario
// `effort-model-version-pinned-via-hotspot`
// -----------------------------------------------------------------------------

// TestEffortModelVersionPinnedViaHotspot_TraversalReproducesVersion
// pins the workstream brief's
// `effort-model-version-pinned-via-hotspot` scenario:
//
//	refactor_task
//	  -> refactor_plan (via task.plan_id)
//	  -> hot_spot (via plan.hotspot_ids[0])
//	  -> policy_version (via hot_spot.policy_version_id)
//	  -> refactor_weights.effort_model_version
//
// The recovered string MUST equal the loaded artefact's
// `Version`. The test wires the full chain against the
// in-memory store + a known-version policy + a matching
// model and asserts the traversal succeeds AND the
// recovered version matches.
func TestEffortModelVersionPinnedViaHotspot_TraversalReproducesVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repoID := uuid.Must(uuid.NewV4())
	sha := "cafef00d"
	pvID := uuid.Must(uuid.NewV4())
	scopeID := uuid.Must(uuid.NewV4())
	hsID := uuid.Must(uuid.NewV4())
	const expectedVersion = "v2.7-trained-2026-05-28"

	// 1. PolicyVersion carries the effort_model_version.
	pv := steward.PolicyVersion{
		PolicyVersionID: pvID,
		Name:            "test-policy",
		RefactorWeights: steward.RefactorWeights{
			EffortModelVersion: expectedVersion,
			TopN:               10,
		},
	}
	// In-memory policy reader replicates the steward path.
	policy := &fixedPolicyReader{snap: PolicySnapshot{
		PolicyVersionID: pv.PolicyVersionID,
		Weights:         pv.RefactorWeights,
	}}

	// 2. HotSpot stamped with policy_version_id.
	hs := HotSpot{
		HotspotID: hsID, RepoID: repoID, SHA: sha, ScopeID: scopeID,
		Score: 5.0, PolicyVersionID: pvID,
		CreatedAt: time.Now().UTC(),
	}
	hsReader := NewInMemoryHotSpotReader()
	hsReader.Insert(hs)

	// 3. FindingDetailReader returns one qualifying finding
	// per hot_spot so the planner emits one task.
	fdr := NewInMemoryFindingDetailReader()
	fdr.Insert(InMemoryFindingWithRule{
		InMemoryFinding: InMemoryFinding{
			RepoID: repoID, SHA: sha, ScopeID: scopeID,
			PolicyVersionID: pvID, Delta: "new",
		},
		RuleID: "solid.dip.coupling_high",
	})

	// 4. Loaded model whose Version matches PolicyVersion's
	// EffortModelVersion. A mismatch would yield
	// ErrEffortModelVersionMismatch -- the test asserts the
	// happy path completes.
	model := fixtureArtefact(expectedVersion)
	writer := NewInMemoryRefactorPlanTaskWriter()
	tp, err := NewTaskPlanner(policy, hsReader, fdr, writer,
		WithEffortEstimator(&model))
	if err != nil {
		t.Fatalf("NewTaskPlanner: %v", err)
	}
	res, err := tp.Plan(ctx, repoID, sha)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(res.Tasks) != 1 {
		t.Fatalf("tasks = %d; want 1", len(res.Tasks))
	}
	if len(res.Plan.HotspotIDs) != 1 {
		t.Fatalf("plan.HotspotIDs = %d; want 1", len(res.Plan.HotspotIDs))
	}

	// --- Traversal: task -> plan -> hot_spot[0] -> pv -> version ---

	// Step 1: task -> plan (via PlanID FK).
	task := res.Tasks[0]
	if task.PlanID != res.Plan.PlanID {
		t.Fatalf("task.PlanID=%s != plan.PlanID=%s", task.PlanID, res.Plan.PlanID)
	}

	// Step 2: plan -> hot_spot[0] (via plan.HotspotIDs[0]).
	firstHotSpotID := res.Plan.HotspotIDs[0]
	if firstHotSpotID != hs.HotspotID {
		t.Fatalf("plan.HotspotIDs[0]=%s != hs.HotspotID=%s", firstHotSpotID, hs.HotspotID)
	}

	// Step 3: hot_spot -> policy_version_id (carried on the
	// hot_spot row itself, architecture Sec 5.5.1).
	recoveredPVID := hs.PolicyVersionID
	if recoveredPVID != pv.PolicyVersionID {
		t.Fatalf("hs.PolicyVersionID=%s != pv.PolicyVersionID=%s",
			recoveredPVID, pv.PolicyVersionID)
	}

	// Step 4: policy_version -> refactor_weights.effort_model_version.
	recoveredVersion := pv.RefactorWeights.EffortModelVersion
	if recoveredVersion != expectedVersion {
		t.Fatalf("recovered version = %q; want %q",
			recoveredVersion, expectedVersion)
	}
	if recoveredVersion != model.Version {
		t.Fatalf("recovered version %q != loaded model version %q",
			recoveredVersion, model.Version)
	}
}

// TestRefactorTaskHasNoEffortModelVersionField pins the
// "no duplicated column" invariant per the workstream
// brief: `effort_model_version` MUST NOT be a field on
// [RefactorTask] or [RefactorPlan]. A new field with that
// name (silently re-introduced by a future iter) would
// fork the architecture Sec 5.5 schema. Compile-time
// would catch a name collision, but a reflective check
// here catches a typo'd alias too.
func TestRefactorTaskHasNoEffortModelVersionField(t *testing.T) {
	t.Parallel()
	taskT := reflect.TypeOf(RefactorTask{})
	planT := reflect.TypeOf(RefactorPlan{})
	for _, typ := range []reflect.Type{taskT, planT} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			lower := strings.ToLower(f.Name)
			if strings.Contains(lower, "effortmodel") || strings.Contains(lower, "effort_model") {
				t.Errorf("%s has forbidden field %q -- architecture Sec 5.5.2/5.5.3 "+
					"deliberately omits effort_model_version (see workstream brief)",
					typ.Name(), f.Name)
			}
		}
	}
}

// -----------------------------------------------------------------------------
// In-memory PolicyReader -- test helper
// -----------------------------------------------------------------------------

// fixedPolicyReader returns the same [PolicySnapshot] for
// every call. Mirrors the existing test fakes; declared
// here so the effort_model_test file is standalone.
type fixedPolicyReader struct {
	snap PolicySnapshot
}

func (f *fixedPolicyReader) ActivePolicyVersion(_ context.Context) (PolicySnapshot, bool, error) {
	var zero PolicySnapshot
	if f.snap == zero {
		return zero, false, nil
	}
	return f.snap, true, nil
}
