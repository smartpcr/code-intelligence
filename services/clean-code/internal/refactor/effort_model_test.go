package refactor

// Stage 9.3 effort-model tests. Covers:
//
//   - [ResolveEffortSource] vocabulary (canonical pin, compose
//     shorthand, empty default, unknown).
//   - Each concrete [EffortModel] impl's contract
//     ([ZeroEffortModel], [HeuristicEffortModel],
//     [MLEffortModel]).
//   - [MLEffortModel] version pinning (matches / mismatches
//     `PolicySnapshot.Weights.EffortModelVersion`).
//   - [NewEffortModelFromConfig] dispatch including missing-URI
//     and missing-version error paths.
//   - [WithEffortModel] option wiring on [TaskPlanner] (nil
//     rejected; non-nil overrides default; Estimate failure
//     aborts the batch).

import (
	"context"
	"errors"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
)

// mkArtefactFile writes a deterministic non-empty fake model
// artefact to a temp dir and returns its `file://` URI. The
// loader-validated MLEffortModel constructor needs a real
// path; this helper keeps the unit tests hermetic without
// stamping a fixture into the repo.
func mkArtefactFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "x.onnx")
	if err := os.WriteFile(path, []byte("test-artefact-bytes-v1"), 0o644); err != nil {
		t.Fatalf("write artefact: %v", err)
	}
	// Build a portable file:// URI; on Windows the path
	// looks like C:\Users\... and needs forward-slash +
	// leading slash before the drive letter.
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	if len(path) >= 2 && path[1] == ':' {
		// Windows drive letter: file:///C:/...
		u.Path = "/" + filepath.ToSlash(path)
	}
	return u.String()
}

func TestResolveEffortSource_Vocabulary(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want EffortSource
	}{
		{"empty defaults to zero", "", EffortSourceZero},
		{"zero canonical", "zero", EffortSourceZero},
		{"zero uppercased", "ZERO", EffortSourceZero},
		{"zero with surrounding space", "  zero ", EffortSourceZero},
		{"none alias", "none", EffortSourceZero},
		{"placeholder alias", "placeholder", EffortSourceZero},
		{"heuristic canonical", "heuristic", EffortSourceHeuristic},
		{"heuristic plural alias", "heuristics", EffortSourceHeuristic},
		{"ml canonical", "ml", EffortSourceML},
		{"ml mixed case", "ML", EffortSourceML},
		{"ml dash alias", "ml-model", EffortSourceML},
		{"ml underscore alias", "ml_model", EffortSourceML},
		{"architecture canonical pin string",
			"ML model from historical commits", EffortSourceML},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolveEffortSource(c.raw)
			if err != nil {
				t.Fatalf("ResolveEffortSource(%q): unexpected error: %v", c.raw, err)
			}
			if got != c.want {
				t.Errorf("ResolveEffortSource(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

func TestResolveEffortSource_Unknown(t *testing.T) {
	cases := []string{"random", "0.5", "off-the-rails", "ml v2", "tflite"}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			_, err := ResolveEffortSource(raw)
			if !errors.Is(err, ErrUnknownEffortSource) {
				t.Fatalf("ResolveEffortSource(%q): want ErrUnknownEffortSource, got %v",
					raw, err)
			}
		})
	}
}

func TestZeroEffortModel_AlwaysZero(t *testing.T) {
	em := ZeroEffortModel{}
	task := mkTask(t, TaskKindSplitClass)
	hs := mkHotSpot(t, 9.5)
	snap := mkSnapshot(t, "v1.0")
	got, err := em.Estimate(task, hs, snap)
	if err != nil {
		t.Fatalf("ZeroEffortModel.Estimate: unexpected error: %v", err)
	}
	if got != 0.0 {
		t.Errorf("ZeroEffortModel.Estimate = %v, want 0.0", got)
	}
}

func TestHeuristicEffortModel_DeterministicAndBounded(t *testing.T) {
	em := HeuristicEffortModel{}
	cases := []struct {
		name     string
		kind     TaskKind
		score    float64
		wantMin  float64
		wantMaxB float64 // exclusive upper bound for the formula's natural range
	}{
		{"split_class low score", TaskKindSplitClass, 0.0, 8.0, 8.01},
		{"extract_method mid score", TaskKindExtractMethod, 5.0, 12.0, 12.01},
		{"break_cycle high score capped at Max", TaskKindBreakCycle, 10.0, MaxHeuristicHours, MaxHeuristicHours + 0.01},
		{"consolidate_duplication NaN score → uses 0", TaskKindConsolidateDuplication, math.NaN(), 3.0, 3.01},
		{"invert_dependency negative score → uses 0", TaskKindInvertDependency, -42.0, 4.0, 4.01},
		{"split_class +Inf score → sanitized to 0 (defensive)", TaskKindSplitClass, math.Inf(+1), 8.0, 8.01},
		{"split_class large finite score capped at Max", TaskKindSplitClass, 1e6, MaxHeuristicHours, MaxHeuristicHours + 0.01},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			task := mkTask(t, c.kind)
			hs := mkHotSpot(t, c.score)
			snap := mkSnapshot(t, "v1.0")
			got, err := em.Estimate(task, hs, snap)
			if err != nil {
				t.Fatalf("HeuristicEffortModel.Estimate: unexpected error: %v", err)
			}
			if got < c.wantMin || got >= c.wantMaxB {
				t.Errorf("HeuristicEffortModel.Estimate = %v, want in [%v, %v)",
					got, c.wantMin, c.wantMaxB)
			}
			// Deterministic: second call returns exactly the same value.
			got2, _ := em.Estimate(task, hs, snap)
			if got != got2 {
				t.Errorf("HeuristicEffortModel.Estimate not deterministic: %v != %v",
					got, got2)
			}
		})
	}
}

func TestHeuristicEffortModel_RejectsBadKind(t *testing.T) {
	em := HeuristicEffortModel{}
	// Use a rejected iter-3 alias to force ValidateTaskKind failure.
	task := RefactorTask{Kind: TaskKind("extract_function")}
	hs := mkHotSpot(t, 5.0)
	snap := mkSnapshot(t, "v1.0")
	_, err := em.Estimate(task, hs, snap)
	if err == nil {
		t.Fatalf("HeuristicEffortModel.Estimate: want error for rejected alias kind")
	}
	if !errors.Is(err, ErrRejectedTaskKindAlias) {
		t.Errorf("HeuristicEffortModel.Estimate: want wrapping ErrRejectedTaskKindAlias, got %v", err)
	}
}

func TestNewMLEffortModel_ValidatesInputs(t *testing.T) {
	uri := mkArtefactFile(t)
	t.Run("missing URI", func(t *testing.T) {
		_, err := NewMLEffortModel("", "v1.0")
		if !errors.Is(err, ErrMLModelURIMissing) {
			t.Fatalf("NewMLEffortModel: want ErrMLModelURIMissing, got %v", err)
		}
	})
	t.Run("missing version", func(t *testing.T) {
		_, err := NewMLEffortModel(uri, "")
		if !errors.Is(err, ErrMLModelVersionMissing) {
			t.Fatalf("NewMLEffortModel: want ErrMLModelVersionMissing, got %v", err)
		}
	})
	t.Run("whitespace URI rejected", func(t *testing.T) {
		_, err := NewMLEffortModel("   ", "v1.0")
		if !errors.Is(err, ErrMLModelURIMissing) {
			t.Fatalf("NewMLEffortModel(whitespace URI): want ErrMLModelURIMissing, got %v", err)
		}
	})
	t.Run("valid pair trims whitespace", func(t *testing.T) {
		em, err := NewMLEffortModel("  "+uri+"  ", "  v1.0  ")
		if err != nil {
			t.Fatalf("NewMLEffortModel: unexpected error: %v", err)
		}
		if em.ModelURI != uri {
			t.Errorf("ModelURI = %q, want %q (trimmed)", em.ModelURI, uri)
		}
		if em.ModelVersion != "v1.0" {
			t.Errorf("ModelVersion = %q, want %q (trimmed)", em.ModelVersion, "v1.0")
		}
		if em.ArtefactBytes <= 0 {
			t.Errorf("ArtefactBytes = %d, want > 0 (loader read the artefact)",
				em.ArtefactBytes)
		}
	})
	t.Run("missing file rejected", func(t *testing.T) {
		dir := t.TempDir()
		ghost := "file:///" + filepath.ToSlash(filepath.Join(dir, "does-not-exist.onnx"))
		_, err := NewMLEffortModel(ghost, "v1.0")
		if !errors.Is(err, ErrMLModelArtefactInvalid) {
			t.Fatalf("NewMLEffortModel(missing file): want ErrMLModelArtefactInvalid, got %v", err)
		}
	})
	t.Run("empty file rejected", func(t *testing.T) {
		dir := t.TempDir()
		empty := filepath.Join(dir, "empty.onnx")
		if err := os.WriteFile(empty, nil, 0o644); err != nil {
			t.Fatalf("write empty file: %v", err)
		}
		uri := "file:///" + filepath.ToSlash(empty)
		_, err := NewMLEffortModel(uri, "v1.0")
		if !errors.Is(err, ErrMLModelArtefactInvalid) {
			t.Fatalf("NewMLEffortModel(empty file): want ErrMLModelArtefactInvalid, got %v", err)
		}
	})
	t.Run("unsupported scheme rejected", func(t *testing.T) {
		_, err := NewMLEffortModel("http://example.com/x.onnx", "v1.0")
		if !errors.Is(err, ErrMLModelArtefactInvalid) {
			t.Fatalf("NewMLEffortModel(http): want ErrMLModelArtefactInvalid, got %v", err)
		}
	})
	t.Run("missing scheme rejected", func(t *testing.T) {
		_, err := NewMLEffortModel("/tmp/x.onnx", "v1.0")
		if !errors.Is(err, ErrMLModelArtefactInvalid) {
			t.Fatalf("NewMLEffortModel(no scheme): want ErrMLModelArtefactInvalid, got %v", err)
		}
	})
}

func TestMLEffortModel_Estimate_VersionPinning(t *testing.T) {
	uri := mkArtefactFile(t)
	em, err := NewMLEffortModel(uri, "v1.0")
	if err != nil {
		t.Fatalf("NewMLEffortModel: %v", err)
	}
	task := mkTask(t, TaskKindSplitClass)
	hs := mkHotSpot(t, 5.0)

	t.Run("matching policy version returns finite estimate", func(t *testing.T) {
		snap := mkSnapshot(t, "v1.0")
		got, err := em.Estimate(task, hs, snap)
		if err != nil {
			t.Fatalf("MLEffortModel.Estimate: unexpected error: %v", err)
		}
		if got < 0 || got > MaxMLHours {
			t.Errorf("estimate out of range: %v not in [0, %v]", got, MaxMLHours)
		}
	})

	t.Run("mismatched policy version is a hard error", func(t *testing.T) {
		snap := mkSnapshot(t, "v2.5")
		_, err := em.Estimate(task, hs, snap)
		if !errors.Is(err, ErrMLModelVersionMismatch) {
			t.Fatalf("MLEffortModel.Estimate: want ErrMLModelVersionMismatch, got %v", err)
		}
		if !strings.Contains(err.Error(), "v1.0") || !strings.Contains(err.Error(), "v2.5") {
			t.Errorf("error message missing version identifiers: %v", err)
		}
	})

	t.Run("empty policy version is permitted", func(t *testing.T) {
		snap := mkSnapshot(t, "")
		_, err := em.Estimate(task, hs, snap)
		if err != nil {
			t.Errorf("empty policy version: unexpected error: %v", err)
		}
	})

	t.Run("estimate is deterministic across calls", func(t *testing.T) {
		snap := mkSnapshot(t, "v1.0")
		a, _ := em.Estimate(task, hs, snap)
		b, _ := em.Estimate(task, hs, snap)
		if a != b {
			t.Errorf("MLEffortModel.Estimate not deterministic: %v != %v", a, b)
		}
	})

	t.Run("different inputs yield different estimates (no collapse)", func(t *testing.T) {
		snap := mkSnapshot(t, "v1.0")
		task2 := mkTask(t, TaskKindBreakCycle)
		task2.RuleID = "different.rule.id"
		a, _ := em.Estimate(task, hs, snap)
		b, _ := em.Estimate(task2, hs, snap)
		if a == b {
			t.Errorf("MLEffortModel.Estimate collapsed: distinct inputs returned identical %v", a)
		}
	})
}

func TestMLEffortModel_RejectsBadKind(t *testing.T) {
	uri := mkArtefactFile(t)
	em, _ := NewMLEffortModel(uri, "v1.0")
	task := RefactorTask{Kind: TaskKind("introduce_interface")}
	hs := mkHotSpot(t, 5.0)
	snap := mkSnapshot(t, "v1.0")
	_, err := em.Estimate(task, hs, snap)
	if !errors.Is(err, ErrRejectedTaskKindAlias) {
		t.Errorf("MLEffortModel.Estimate: want ErrRejectedTaskKindAlias, got %v", err)
	}
}

// TestMLEffortModel_ArtefactDigestAffectsEstimate confirms
// the loaded artefact bytes are folded into the estimator's
// hash: two MLEffortModel instances pinned to the same
// version but loaded from DIFFERENT artefact files MUST NOT
// produce identical estimates for the same (task, hs)
// triple. This pins the architecture Sec 8.3 reproducibility
// guarantee: swapping the artefact in place changes the
// estimate even when the version string is unchanged.
func TestMLEffortModel_ArtefactDigestAffectsEstimate(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.onnx")
	pathB := filepath.Join(dir, "b.onnx")
	if err := os.WriteFile(pathA, []byte("artefact-A"), 0o644); err != nil {
		t.Fatalf("write A: %v", err)
	}
	if err := os.WriteFile(pathB, []byte("artefact-B-different-bytes"), 0o644); err != nil {
		t.Fatalf("write B: %v", err)
	}
	uriA := "file:///" + filepath.ToSlash(pathA)
	uriB := "file:///" + filepath.ToSlash(pathB)
	emA, err := NewMLEffortModel(uriA, "v1.0")
	if err != nil {
		t.Fatalf("NewMLEffortModel(A): %v", err)
	}
	emB, err := NewMLEffortModel(uriB, "v1.0")
	if err != nil {
		t.Fatalf("NewMLEffortModel(B): %v", err)
	}
	task := mkTask(t, TaskKindSplitClass)
	hs := mkHotSpot(t, 5.0)
	snap := mkSnapshot(t, "v1.0")
	a, _ := emA.Estimate(task, hs, snap)
	b, _ := emB.Estimate(task, hs, snap)
	if a == b {
		t.Errorf("artefact digest not folded into estimate: A=%v B=%v", a, b)
	}
}

func TestNewEffortModelFromConfig_Dispatch(t *testing.T) {
	t.Run("empty source resolves to zero", func(t *testing.T) {
		em, err := NewEffortModelFromConfig(EffortModelConfig{})
		if err != nil {
			t.Fatalf("NewEffortModelFromConfig: unexpected error: %v", err)
		}
		if _, ok := em.(ZeroEffortModel); !ok {
			t.Errorf("want ZeroEffortModel, got %T", em)
		}
	})

	t.Run("heuristic source", func(t *testing.T) {
		em, err := NewEffortModelFromConfig(EffortModelConfig{Source: "heuristic"})
		if err != nil {
			t.Fatalf("NewEffortModelFromConfig: unexpected error: %v", err)
		}
		if _, ok := em.(HeuristicEffortModel); !ok {
			t.Errorf("want HeuristicEffortModel, got %T", em)
		}
	})

	t.Run("ml source requires URI", func(t *testing.T) {
		uri := mkArtefactFile(t)
		_ = uri // unused for missing-URI case
		_, err := NewEffortModelFromConfig(EffortModelConfig{
			Source:         "ml",
			MLModelVersion: "v1.0",
		})
		if !errors.Is(err, ErrMLModelURIMissing) {
			t.Fatalf("want ErrMLModelURIMissing, got %v", err)
		}
	})

	t.Run("ml source requires version", func(t *testing.T) {
		uri := mkArtefactFile(t)
		_, err := NewEffortModelFromConfig(EffortModelConfig{
			Source:     "ml",
			MLModelURI: uri,
		})
		if !errors.Is(err, ErrMLModelVersionMissing) {
			t.Fatalf("want ErrMLModelVersionMissing, got %v", err)
		}
	})

	t.Run("ml source with both pins succeeds", func(t *testing.T) {
		uri := mkArtefactFile(t)
		em, err := NewEffortModelFromConfig(EffortModelConfig{
			Source:         "ml",
			MLModelURI:     uri,
			MLModelVersion: "v1.0",
		})
		if err != nil {
			t.Fatalf("NewEffortModelFromConfig: unexpected error: %v", err)
		}
		m, ok := em.(*MLEffortModel)
		if !ok {
			t.Fatalf("want *MLEffortModel, got %T", em)
		}
		if m.ModelURI != uri || m.ModelVersion != "v1.0" {
			t.Errorf("MLEffortModel fields wrong: %+v", m)
		}
	})

	t.Run("architecture canonical pin → ml", func(t *testing.T) {
		uri := mkArtefactFile(t)
		em, err := NewEffortModelFromConfig(EffortModelConfig{
			Source:         "ML model from historical commits",
			MLModelURI:     uri,
			MLModelVersion: "v1.0",
		})
		if err != nil {
			t.Fatalf("NewEffortModelFromConfig: unexpected error: %v", err)
		}
		if _, ok := em.(*MLEffortModel); !ok {
			t.Errorf("want *MLEffortModel for architecture canonical pin, got %T", em)
		}
	})

	t.Run("ml source with bad artefact path is rejected", func(t *testing.T) {
		_, err := NewEffortModelFromConfig(EffortModelConfig{
			Source:         "ml",
			MLModelURI:     "file:///nonexistent/path/x.onnx",
			MLModelVersion: "v1.0",
		})
		if !errors.Is(err, ErrMLModelArtefactInvalid) {
			t.Fatalf("want ErrMLModelArtefactInvalid, got %v", err)
		}
	})

	t.Run("unknown source surfaces typed error", func(t *testing.T) {
		_, err := NewEffortModelFromConfig(EffortModelConfig{Source: "voodoo"})
		if !errors.Is(err, ErrUnknownEffortSource) {
			t.Fatalf("want ErrUnknownEffortSource, got %v", err)
		}
	})
}

func TestEffortModelFunc_NilEstimateRejected(t *testing.T) {
	var fn EffortModelFunc
	_, err := fn.Estimate(mkTask(t, TaskKindSplitClass), mkHotSpot(t, 1.0), mkSnapshot(t, "v1"))
	if !errors.Is(err, ErrNilEffortFunc) {
		t.Errorf("EffortModelFunc(nil).Estimate: want ErrNilEffortFunc, got %v", err)
	}
}

func TestWithEffortModel_NilRejected(t *testing.T) {
	stub := mustBuildTaskPlannerArgs(t)
	_, err := NewTaskPlanner(
		stub.policy, stub.hsReader, stub.detReader, stub.writer,
		WithEffortModel(nil),
	)
	if !errors.Is(err, ErrNilEffortModel) {
		t.Errorf("WithEffortModel(nil): want ErrNilEffortModel, got %v", err)
	}
}

// TestTaskPlanner_EffortModelEstimateAbortsBatch confirms that a
// Stage 9.3 [EffortModel] returning an error aborts the WHOLE
// atomic plan + tasks write -- no `refactor_plan` and no
// `refactor_task` row may land if even one task's estimate is
// rejected. This matches the existing kind-validation contract.
func TestTaskPlanner_EffortModelEstimateAbortsBatch(t *testing.T) {
	stub := mustBuildTaskPlannerArgs(t)
	// Seed one hotspot + one finding so the planner attempts
	// to emit exactly one task.
	scope := mustUUID(t)
	stub.hsReader.HotSpots = []HotSpot{{
		HotspotID:       mustUUID(t),
		PolicyVersionID: stub.snap.PolicyVersionID,
		ScopeID:         scope,
		Score:           5.0,
	}}
	stub.detReader.Details = []FindingDetail{{ScopeID: scope, RuleID: "solid.srp"}}

	failingModel := EffortModelFunc(func(_ RefactorTask, _ HotSpot, _ PolicySnapshot) (float64, error) {
		return 0, ErrInvalidEffortEstimate
	})
	tp, err := NewTaskPlanner(
		stub.policy, stub.hsReader, stub.detReader, stub.writer,
		WithEffortModel(failingModel),
	)
	if err != nil {
		t.Fatalf("NewTaskPlanner: %v", err)
	}
	repoID := mustUUID(t)
	_, err = tp.PlanFromSnapshot(t.Context(), repoID, "sha-deadbeef", stub.snap)
	if err == nil {
		t.Fatalf("PlanFromSnapshot: want error, got nil")
	}
	if !errors.Is(err, ErrInvalidEffortEstimate) {
		t.Errorf("PlanFromSnapshot: want wrapping ErrInvalidEffortEstimate, got %v", err)
	}
	// Atomic contract: writer MUST NOT have been called.
	if stub.writer.PlanCallCount() != 0 {
		t.Errorf("writer called %d times despite estimate failure -- atomic contract violated",
			stub.writer.PlanCallCount())
	}
}

// TestTaskPlanner_EffortModelStampsEstimate confirms the
// default-replacement path: when a non-zero estimator is wired,
// `RefactorTask.EffortHours` reflects its output (not the
// historical 0.0 placeholder).
func TestTaskPlanner_EffortModelStampsEstimate(t *testing.T) {
	stub := mustBuildTaskPlannerArgs(t)
	scope := mustUUID(t)
	stub.hsReader.HotSpots = []HotSpot{{
		HotspotID:       mustUUID(t),
		PolicyVersionID: stub.snap.PolicyVersionID,
		ScopeID:         scope,
		Score:           5.0,
	}}
	stub.detReader.Details = []FindingDetail{{ScopeID: scope, RuleID: "solid.srp"}}

	const want = 17.5
	model := EffortModelFunc(func(_ RefactorTask, _ HotSpot, _ PolicySnapshot) (float64, error) {
		return want, nil
	})
	tp, err := NewTaskPlanner(
		stub.policy, stub.hsReader, stub.detReader, stub.writer,
		WithEffortModel(model),
	)
	if err != nil {
		t.Fatalf("NewTaskPlanner: %v", err)
	}
	res, err := tp.PlanFromSnapshot(t.Context(), mustUUID(t), "sha-cafebabe", stub.snap)
	if err != nil {
		t.Fatalf("PlanFromSnapshot: %v", err)
	}
	if len(res.Tasks) != 1 {
		t.Fatalf("PlanFromSnapshot: want 1 task, got %d", len(res.Tasks))
	}
	if got := res.Tasks[0].EffortHours; got != want {
		t.Errorf("Task.EffortHours = %v, want %v", got, want)
	}
}

// TestTaskPlanner_DefaultEffortModelIsZero confirms the
// composition-root default preserves the prior "explicit 0.0
// placeholder" semantics so callers that do NOT wire an
// estimator see no behavioural change.
func TestTaskPlanner_DefaultEffortModelIsZero(t *testing.T) {
	stub := mustBuildTaskPlannerArgs(t)
	scope := mustUUID(t)
	stub.hsReader.HotSpots = []HotSpot{{
		HotspotID:       mustUUID(t),
		PolicyVersionID: stub.snap.PolicyVersionID,
		ScopeID:         scope,
		Score:           5.0,
	}}
	stub.detReader.Details = []FindingDetail{{ScopeID: scope, RuleID: "solid.srp"}}

	tp, err := NewTaskPlanner(
		stub.policy, stub.hsReader, stub.detReader, stub.writer,
	)
	if err != nil {
		t.Fatalf("NewTaskPlanner: %v", err)
	}
	res, err := tp.PlanFromSnapshot(t.Context(), mustUUID(t), "sha-feedbabe", stub.snap)
	if err != nil {
		t.Fatalf("PlanFromSnapshot: %v", err)
	}
	if len(res.Tasks) != 1 {
		t.Fatalf("want 1 task, got %d", len(res.Tasks))
	}
	if res.Tasks[0].EffortHours != 0.0 {
		t.Errorf("default EffortHours = %v, want 0.0 (legacy placeholder)",
			res.Tasks[0].EffortHours)
	}
}

// -----------------------------------------------------------------------------
// Test helpers
// -----------------------------------------------------------------------------

func mkTask(t *testing.T, k TaskKind) RefactorTask {
	t.Helper()
	return RefactorTask{
		TaskID:  mustUUID(t),
		PlanID:  mustUUID(t),
		ScopeID: mustUUID(t),
		Kind:    k,
		RuleID:  "solid.srp.lcom4_high",
	}
}

func mkHotSpot(t *testing.T, score float64) HotSpot {
	t.Helper()
	return HotSpot{
		HotspotID:       mustUUID(t),
		PolicyVersionID: mustUUID(t),
		ScopeID:         mustUUID(t),
		Score:           score,
	}
}

func mkSnapshot(t *testing.T, modelVersion string) PolicySnapshot {
	t.Helper()
	return PolicySnapshot{
		PolicyVersionID: mustUUID(t),
		Weights: steward.RefactorWeights{
			Alpha:              0.4,
			Beta:               0.3,
			Gamma:              0.2,
			Delta:              0.1,
			EffortModelVersion: modelVersion,
			WindowDays:         90,
		},
	}
}

type plannerStubArgs struct {
	policy    *fixedPolicyReader
	hsReader  *recHotSpotReader
	detReader *recDetailReader
	writer    *recPlanTaskWriter
	snap      PolicySnapshot
}

func mustBuildTaskPlannerArgs(t *testing.T) plannerStubArgs {
	t.Helper()
	snap := mkSnapshot(t, "v1.0")
	return plannerStubArgs{
		policy:    &fixedPolicyReader{snap: snap, ok: true},
		hsReader:  &recHotSpotReader{},
		detReader: &recDetailReader{},
		writer:    &recPlanTaskWriter{},
		snap:      snap,
	}
}

// fixedPolicyReader returns a pre-baked snapshot for the
// effort-model tests so the planner can run without a real
// steward.
type fixedPolicyReader struct {
	snap PolicySnapshot
	ok   bool
}

func (f *fixedPolicyReader) ActivePolicyVersion(_ context.Context) (PolicySnapshot, bool, error) {
	return f.snap, f.ok, nil
}

type recHotSpotReader struct {
	HotSpots []HotSpot
}

func (r *recHotSpotReader) LatestHotSpotsByScore(
	_ context.Context,
	_ uuid.UUID,
	_ string,
	_ uuid.UUID,
	_ int,
) ([]HotSpot, error) {
	return r.HotSpots, nil
}

type recDetailReader struct {
	Details []FindingDetail
}

func (r *recDetailReader) FindingDetails(
	_ context.Context,
	_ uuid.UUID,
	_ string,
	_ uuid.UUID,
	_ []uuid.UUID,
) ([]FindingDetail, error) {
	return r.Details, nil
}

type recPlanTaskWriter struct {
	calls int
}

func (r *recPlanTaskWriter) WriteRefactorPlanAndTasks(_ context.Context, _ RefactorPlan, _ []RefactorTask) error {
	r.calls++
	return nil
}

func (r *recPlanTaskWriter) PlanCallCount() int { return r.calls }
