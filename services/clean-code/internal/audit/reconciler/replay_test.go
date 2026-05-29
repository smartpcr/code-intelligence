package reconciler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/audit/wal"
)

// fakeReplayer is a deterministic in-memory [Replayer] for
// reconciler tests. It tracks an existence map keyed by
// (table, row_pk) so test bodies can pre-populate "row
// already exists" cases AND assert the brief invariant
// "leave existing rows untouched" via the value-snapshot
// helper. The recordedRuns / recordedVerdicts /
// recordedFindings slices preserve call order so phased-
// replay tests can assert pass-1-before-pass-2.
type fakeReplayer struct {
	mu sync.Mutex

	// Pre-populated rows -- these will trigger
	// OutcomeSkippedExisting on replay AND must remain
	// byte-identical after the Run completes (the brief's
	// "leave existing rows untouched" invariant).
	existingRuns     map[uuid.UUID]EvaluationRunRow
	existingVerdicts map[uuid.UUID]EvaluationVerdictRow
	existingFindings map[uuid.UUID]FindingRow

	// Call log preserves order.
	recordedRuns     []EvaluationRunRow
	recordedVerdicts []EvaluationVerdictRow
	recordedFindings []FindingRow
	dispatchOrder    []string

	// Optional error injection per audit table.
	runErr     error
	verdictErr error
	findingErr error
}

func newFakeReplayer() *fakeReplayer {
	return &fakeReplayer{
		existingRuns:     map[uuid.UUID]EvaluationRunRow{},
		existingVerdicts: map[uuid.UUID]EvaluationVerdictRow{},
		existingFindings: map[uuid.UUID]FindingRow{},
	}
}

func (f *fakeReplayer) ReplayRun(_ context.Context, row EvaluationRunRow) (Outcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordedRuns = append(f.recordedRuns, row)
	f.dispatchOrder = append(f.dispatchOrder, "run")
	if f.runErr != nil {
		return 0, f.runErr
	}
	if _, ok := f.existingRuns[row.EvaluationRunID]; ok {
		return OutcomeSkippedExisting, nil
	}
	f.existingRuns[row.EvaluationRunID] = row
	return OutcomeInserted, nil
}

func (f *fakeReplayer) ReplayVerdict(_ context.Context, row EvaluationVerdictRow) (Outcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordedVerdicts = append(f.recordedVerdicts, row)
	f.dispatchOrder = append(f.dispatchOrder, "verdict")
	if f.verdictErr != nil {
		return 0, f.verdictErr
	}
	if _, ok := f.existingVerdicts[row.VerdictID]; ok {
		return OutcomeSkippedExisting, nil
	}
	f.existingVerdicts[row.VerdictID] = row
	return OutcomeInserted, nil
}

func (f *fakeReplayer) ReplayFinding(_ context.Context, row FindingRow) (Outcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordedFindings = append(f.recordedFindings, row)
	f.dispatchOrder = append(f.dispatchOrder, "finding")
	if f.findingErr != nil {
		return 0, f.findingErr
	}
	if _, ok := f.existingFindings[row.FindingID]; ok {
		return OutcomeSkippedExisting, nil
	}
	f.existingFindings[row.FindingID] = row
	return OutcomeInserted, nil
}

// stageFrames writes the three canonical frames (one
// evaluation_run, one evaluation_verdict, one finding) to a
// fresh WAL dir using a NoopSigner writer. Returns the
// directory path and the three frame ids so tests can
// correlate them with the call log.
type stagedFrames struct {
	dir       string
	runID     uuid.UUID
	verdictID uuid.UUID
	findingID uuid.UUID
	caller    string
}

func stageFrames(t *testing.T, caller string) stagedFrames {
	t.Helper()
	dir := t.TempDir()
	w, err := wal.NewWriter(wal.WriterConfig{
		Dir:    dir,
		Signer: wal.NoopSigner{},
	})
	if err != nil {
		t.Fatalf("wal.NewWriter: %v", err)
	}
	ctx := context.Background()

	runID := mustUUID(t, "11111111-1111-1111-1111-111111111111")
	repoID := mustUUID(t, "22222222-2222-2222-2222-222222222222")
	policyID := mustUUID(t, "33333333-3333-3333-3333-333333333333")
	verdictID := mustUUID(t, "55555555-5555-5555-5555-555555555555")
	findingID := mustUUID(t, "66666666-6666-6666-6666-666666666666")
	scopeID := mustUUID(t, "77777777-7777-7777-7777-777777777777")

	runRow := map[string]any{
		"evaluation_run_id": runID.String(),
		"repo_id":           repoID.String(),
		"sha":               "abcdef0123456789abcdef0123456789abcdef01",
		"policy_version_id": policyID.String(),
		"caller":            caller,
		"scope_id":          nil,
		"created_at":        time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC).Format("2006-01-02T15:04:05.000000000Z07:00"),
	}
	runJSON := mustJSON(t, runRow)
	verdictRow := map[string]any{
		"verdict_id":        verdictID.String(),
		"evaluation_run_id": runID.String(),
		"verdict":           "pass",
		"degraded":          false,
		"degraded_reason":   nil,
		"created_at":        time.Date(2025, 1, 2, 3, 4, 6, 0, time.UTC).Format("2006-01-02T15:04:05.000000000Z07:00"),
	}
	verdictJSON := mustJSON(t, verdictRow)
	findingRow := map[string]any{
		"finding_id":        findingID.String(),
		"evaluation_run_id": runID.String(),
		"repo_id":           repoID.String(),
		"sha":               "abcdef0123456789abcdef0123456789abcdef01",
		"scope_id":          scopeID.String(),
		"rule_id":           "complexity.cyclomatic",
		"rule_version":      3,
		"policy_version_id": policyID.String(),
		"metric_sample_ids": []string{},
		"severity":          "warn",
		"delta":             "regressed",
		"explanation_md":    "x",
		"created_at":        time.Date(2025, 1, 2, 3, 4, 7, 0, time.UTC).Format("2006-01-02T15:04:05.000000000Z07:00"),
	}
	findingJSON := mustJSON(t, findingRow)

	batch := w.NewTxBatch()
	defer batch.Cancel()
	if _, err := batch.StageNew(ctx, wal.TableEvaluationRun, runID, runJSON); err != nil {
		t.Fatalf("StageNew run: %v", err)
	}
	if _, err := batch.StageNew(ctx, wal.TableEvaluationVerdict, verdictID, verdictJSON); err != nil {
		t.Fatalf("StageNew verdict: %v", err)
	}
	if _, err := batch.StageNew(ctx, wal.TableFinding, findingID, findingJSON); err != nil {
		t.Fatalf("StageNew finding: %v", err)
	}
	if err := batch.Commit(ctx); err != nil {
		t.Fatalf("batch.Commit: %v", err)
	}
	return stagedFrames{
		dir:       dir,
		runID:     runID,
		verdictID: verdictID,
		findingID: findingID,
		caller:    caller,
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

// TestReconciler_ReplaysMissingRows -- brief scenario #1.
// Three frames on disk; the replayer reports the underlying
// table is EMPTY; Run produces three OutcomeInserted
// classifications, one per audit table.
func TestReconciler_ReplaysMissingRows(t *testing.T) {
	t.Parallel()
	staged := stageFrames(t, "eval_gate")
	fake := newFakeReplayer()
	r, err := NewReconciler(Config{
		Dir:      staged.dir,
		Verifier: NoopSignerVerifier{},
		Replayer: fake,
	})
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	stats, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Replayed.EvaluationRun != 1 || stats.Replayed.EvaluationVerdict != 1 || stats.Replayed.Finding != 1 {
		t.Fatalf("Replayed: %+v want each=1", stats.Replayed)
	}
	if stats.SkippedExisting.Total() != 0 {
		t.Fatalf("SkippedExisting: got %+v want all zero", stats.SkippedExisting)
	}
	if stats.SkippedBadSig.Total() != 0 || stats.SkippedBadShape.Total() != 0 {
		t.Fatalf("bad-sig/shape: %+v / %+v want all zero", stats.SkippedBadSig, stats.SkippedBadShape)
	}
	if len(fake.recordedRuns) != 1 || fake.recordedRuns[0].EvaluationRunID != staged.runID {
		t.Fatalf("recordedRuns: %+v want one call with id=%s", fake.recordedRuns, staged.runID)
	}
}

// TestReconciler_LeavesExistingRowsUntouched -- brief
// scenario #2. Pre-populate the replayer with the same
// (table, pk) coordinates; Run must classify every frame as
// SkippedExisting AND the pre-populated values must be
// byte-identical after Run.
func TestReconciler_LeavesExistingRowsUntouched(t *testing.T) {
	t.Parallel()
	staged := stageFrames(t, "batch_refresh")
	fake := newFakeReplayer()
	// Pre-populate with deliberately DIFFERENT field
	// values so we can prove the reconciler did not
	// overwrite them on the SkippedExisting path.
	preRun := EvaluationRunRow{
		EvaluationRunID: staged.runID,
		RepoID:          mustUUID(t, "deadbeef-dead-beef-dead-beefdeadbeef"),
		SHA:             "preexisting-sha-do-not-touch",
		PolicyVersionID: mustUUID(t, "deadbeef-dead-beef-dead-beefdeadbeef"),
		Caller:          "pre-existing-caller-must-not-change",
		CreatedAt:       time.Unix(0, 0).UTC(),
	}
	preVerdict := EvaluationVerdictRow{
		VerdictID: staged.verdictID,
		Verdict:   "DO_NOT_OVERWRITE",
	}
	preFinding := FindingRow{
		FindingID: staged.findingID,
		RuleID:    "DO_NOT_OVERWRITE",
	}
	fake.existingRuns[staged.runID] = preRun
	fake.existingVerdicts[staged.verdictID] = preVerdict
	fake.existingFindings[staged.findingID] = preFinding

	r, _ := NewReconciler(Config{
		Dir:      staged.dir,
		Verifier: NoopSignerVerifier{},
		Replayer: fake,
	})
	stats, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.SkippedExisting.EvaluationRun != 1 ||
		stats.SkippedExisting.EvaluationVerdict != 1 ||
		stats.SkippedExisting.Finding != 1 {
		t.Fatalf("SkippedExisting: %+v want each=1", stats.SkippedExisting)
	}
	if stats.Replayed.Total() != 0 {
		t.Fatalf("Replayed: %+v want all zero", stats.Replayed)
	}
	if got := fake.existingRuns[staged.runID]; got.SHA != preRun.SHA || got.Caller != preRun.Caller {
		t.Fatalf("existing run mutated: got %+v want %+v", got, preRun)
	}
	if got := fake.existingVerdicts[staged.verdictID]; got.Verdict != preVerdict.Verdict {
		t.Fatalf("existing verdict mutated: got %+v want %+v", got, preVerdict)
	}
	if got := fake.existingFindings[staged.findingID]; got.RuleID != preFinding.RuleID {
		t.Fatalf("existing finding mutated: got %+v want %+v", got, preFinding)
	}
}

// TestReconciler_PreservesCallerVerbatim -- brief scenario
// #3. The reconciler MUST pass `evaluation_run.caller`
// verbatim from the WAL frame, with NO substitution. We run
// the test once per canonical caller value.
func TestReconciler_PreservesCallerVerbatim(t *testing.T) {
	t.Parallel()
	for _, caller := range []string{"eval_gate", "batch_refresh"} {
		caller := caller
		t.Run(caller, func(t *testing.T) {
			t.Parallel()
			staged := stageFrames(t, caller)
			fake := newFakeReplayer()
			r, _ := NewReconciler(Config{
				Dir:      staged.dir,
				Verifier: NoopSignerVerifier{},
				Replayer: fake,
			})
			if _, err := r.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(fake.recordedRuns) != 1 {
				t.Fatalf("recordedRuns: got %d want 1", len(fake.recordedRuns))
			}
			if fake.recordedRuns[0].Caller != caller {
				t.Fatalf("caller: got %q want %q (verbatim from frame -- reconciler MUST NOT substitute)",
					fake.recordedRuns[0].Caller, caller)
			}
		})
	}
}

// TestReconciler_BadSignatureSkips -- brief scenario #4.
// A frame on disk whose signature does not validate must
// NOT reach the replayer. The reconciler must count it in
// Stats.SkippedBadSig and continue.
func TestReconciler_BadSignatureSkips(t *testing.T) {
	t.Parallel()
	staged := stageFrames(t, "eval_gate")

	// Tamper with the partition file: rewrite each frame's
	// `signature` to a fixed wrong value. We re-encode each
	// frame to keep the JSON well-formed; the WAL reader
	// will accept the well-formed frame, and the verifier
	// will reject the signature.
	tamperPartitions(t, staged.dir, func(f *wal.AuditFrame) {
		f.Signature = bytesAllZero(sha256.Size)
	})

	fake := newFakeReplayer()
	r, _ := NewReconciler(Config{
		Dir:      staged.dir,
		Verifier: NoopSignerVerifier{},
		Replayer: fake,
	})
	stats, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.SkippedBadSig.Total() != 3 {
		t.Fatalf("SkippedBadSig: %+v want total=3", stats.SkippedBadSig)
	}
	if stats.Replayed.Total() != 0 || stats.SkippedExisting.Total() != 0 {
		t.Fatalf("any insert / skip-existing path reached: %+v / %+v want zero",
			stats.Replayed, stats.SkippedExisting)
	}
	if len(fake.recordedRuns) != 0 || len(fake.recordedVerdicts) != 0 || len(fake.recordedFindings) != 0 {
		t.Fatalf("replayer was called for bad-sig frame: runs=%d verdicts=%d findings=%d",
			len(fake.recordedRuns), len(fake.recordedVerdicts), len(fake.recordedFindings))
	}
}

func bytesAllZero(n int) []byte { return make([]byte, n) }

// tamperPartitions rewrites every frame in every partition
// file under `dir` after `mut` mutates it. JSON shape is
// preserved -- the WAL reader accepts the frame, the
// verifier rejects it on the wrong signature.
func tamperPartitions(t *testing.T, dir string, mut func(*wal.AuditFrame)) {
	t.Helper()
	frames, err := wal.ReadAll(dir)
	if err != nil {
		t.Fatalf("wal.ReadAll for tamper: %v", err)
	}
	if len(frames) == 0 {
		t.Fatalf("tamperPartitions: no frames to tamper")
	}
	// Group by partition date so we rewrite each file
	// once.
	type group struct {
		path   string
		frames []wal.AuditFrame
	}
	groups := map[string]*group{}
	for _, f := range frames {
		date := f.WrittenAt.UTC().Format("2006-01-02")
		path := filepath.Join(dir, date+".wal")
		g, ok := groups[date]
		if !ok {
			g = &group{path: path}
			groups[date] = g
		}
		mut(&f)
		g.frames = append(g.frames, f)
	}
	for _, g := range groups {
		var buf []byte
		for _, f := range g.frames {
			b, err := json.Marshal(f)
			if err != nil {
				t.Fatalf("marshal tampered frame: %v", err)
			}
			buf = append(buf, b...)
			buf = append(buf, '\n')
		}
		if err := os.WriteFile(g.path, buf, 0o644); err != nil {
			t.Fatalf("rewrite tampered partition %s: %v", g.path, err)
		}
	}
}
