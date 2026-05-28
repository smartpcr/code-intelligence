package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/audit/wal"
)

func TestNewReconciler_RejectsUnwiredDeps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  Config
		want error
	}{
		{"missing dir", Config{Verifier: NoopSignerVerifier{}, Replayer: newFakeReplayer()}, ErrDirUnwired},
		{"missing verifier", Config{Dir: t.TempDir(), Replayer: newFakeReplayer()}, ErrVerifierUnwired},
		{"missing replayer", Config{Dir: t.TempDir(), Verifier: NoopSignerVerifier{}}, ErrReplayerUnwired},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewReconciler(tc.cfg)
			if !errors.Is(err, tc.want) {
				t.Fatalf("NewReconciler err: %v want %v", err, tc.want)
			}
		})
	}
}

func TestNewReconciler_DefaultLoggerNoOp(t *testing.T) {
	t.Parallel()
	// A nil logger MUST NOT panic on the first warning /
	// completion line. Construct and Run an empty dir to
	// reach the completion log without any frames.
	r, err := NewReconciler(Config{
		Dir:      t.TempDir(),
		Verifier: NoopSignerVerifier{},
		Replayer: newFakeReplayer(),
	})
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	stats, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Replayed.Total() != 0 {
		t.Fatalf("empty dir Replayed: %+v want zero", stats.Replayed)
	}
}

// TestReconciler_PartialFrameTailIsNonFatal: a partition
// file with two complete frames followed by a truncated
// third frame must yield Replayed=2 + a Warnings entry.
func TestReconciler_PartialFrameTailIsNonFatal(t *testing.T) {
	t.Parallel()
	staged := stageFrames(t, "eval_gate")

	// Append a few bytes (no trailing newline) to the
	// single partition file -- the wal.ReadAll path
	// surfaces this as ErrTrailingPartialFrame and keeps
	// the complete frames.
	entries, err := os.ReadDir(staged.dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ReadDir: got %d entries want 1", len(entries))
	}
	path := filepath.Join(staged.dir, entries[0].Name())
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteString(`{"frame_id":"trunc"`); err != nil {
		f.Close()
		t.Fatalf("Write partial: %v", err)
	}
	f.Close()

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
	if stats.Replayed.EvaluationRun != 1 || stats.Replayed.EvaluationVerdict != 1 || stats.Replayed.Finding != 1 {
		t.Fatalf("Replayed: %+v want each=1 (complete frames preserved across the partial tail)", stats.Replayed)
	}
	if len(stats.Warnings) == 0 {
		t.Fatalf("Warnings: empty; want one entry about trailing partial frame")
	}
	if !strings.Contains(stats.Warnings[0], "ErrTrailingPartialFrame") {
		t.Fatalf("Warnings[0]: %q want it to mention ErrTrailingPartialFrame", stats.Warnings[0])
	}
}

// TestReconciler_PreservesScopeIDNullable: a run frame
// with `scope_id=null` and a finding frame with a non-zero
// scope must both round-trip through the reconciler so the
// reconciler doesn't accidentally coerce null to a wrong
// UUID (or vice versa).
func TestReconciler_PreservesScopeIDNullable(t *testing.T) {
	t.Parallel()
	staged := stageFrames(t, "eval_gate") // run frame's scope_id is nil
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
	if fake.recordedRuns[0].ScopeID != nil {
		t.Fatalf("run.ScopeID: got %v want nil (frame carried null)", fake.recordedRuns[0].ScopeID)
	}
	if len(fake.recordedFindings) != 1 {
		t.Fatalf("recordedFindings: got %d want 1", len(fake.recordedFindings))
	}
	if fake.recordedFindings[0].ScopeID == uuid.Nil {
		t.Fatalf("finding.ScopeID: got zero want a non-zero uuid (finding scope is NOT NULL)")
	}
}

// TestReconciler_PhasedReplay_RunFramesBeforeVerdictsAndFindings:
// the reconciler must replay ALL evaluation_run frames
// before ANY verdict / finding frame so a corrupted
// partition that has a finding ahead of its owning run
// still satisfies the FK constraint on PG.
func TestReconciler_PhasedReplay_RunFramesBeforeVerdictsAndFindings(t *testing.T) {
	t.Parallel()
	staged := stageFrames(t, "eval_gate")
	fake := newFakeReplayer()
	r, _ := NewReconciler(Config{
		Dir:      staged.dir,
		Verifier: NoopSignerVerifier{},
		Replayer: fake,
	})
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// First dispatch MUST be "run", regardless of WAL
	// order, because phase 1 picks runs first.
	if len(fake.dispatchOrder) != 3 {
		t.Fatalf("dispatchOrder: got %v want 3 entries", fake.dispatchOrder)
	}
	if fake.dispatchOrder[0] != "run" {
		t.Fatalf("dispatchOrder[0]: got %q want %q", fake.dispatchOrder[0], "run")
	}
}

// TestReconciler_DisallowUnknownFields_AbortsRun: a frame
// whose row_json carries an extra unknown column MUST
// abort Run (loud failure), not silently SkippedBadShape
// the frame. Once the signature has verified, an unknown
// column means writer-side schema drift OR a signing-key
// compromise -- both warrant operator triage before
// further replay. We bypass signature verification with
// alwaysValidVerifier so the strict-decode path is the
// FIRST guard the frame hits after the table dispatch.
func TestReconciler_DisallowUnknownFields_AbortsRun(t *testing.T) {
	t.Parallel()
	staged := stageFrames(t, "eval_gate")
	// Read every frame, mutate ONLY the run-frame's
	// row_json to add an unknown field, then re-write.
	frames, err := wal.ReadAll(staged.dir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	for i := range frames {
		if frames[i].Table != wal.TableEvaluationRun {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(frames[i].RowJSON, &obj); err != nil {
			t.Fatalf("unmarshal row_json: %v", err)
		}
		obj["surprise_new_column"] = "v"
		raw, err := json.Marshal(obj)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		frames[i].RowJSON = raw
	}
	// Group by partition and rewrite.
	type group struct {
		path   string
		frames []wal.AuditFrame
	}
	groups := map[string]*group{}
	for _, f := range frames {
		date := f.WrittenAt.UTC().Format("2006-01-02")
		path := filepath.Join(staged.dir, date+".wal")
		g, ok := groups[date]
		if !ok {
			g = &group{path: path}
			groups[date] = g
		}
		g.frames = append(g.frames, f)
	}
	for _, g := range groups {
		var buf []byte
		for _, fr := range g.frames {
			b, _ := json.Marshal(fr)
			buf = append(buf, b...)
			buf = append(buf, '\n')
		}
		if err := os.WriteFile(g.path, buf, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	fake := newFakeReplayer()
	r, _ := NewReconciler(Config{
		Dir:      staged.dir,
		Verifier: alwaysValidVerifier{}, // isolate the strict-decode path
		Replayer: fake,
	})
	_, err = r.Run(context.Background())
	if err == nil {
		t.Fatal("Run: err = nil; want loud abort on unknown-column row_json after valid signature")
	}
	if !strings.Contains(err.Error(), "decode failed") {
		t.Fatalf("Run err = %v; want it to mention decode failure (post-signature schema drift)", err)
	}
	if len(fake.recordedRuns) != 0 {
		t.Fatalf("recordedRuns: got %d want 0 (bad-shape frame must NOT reach the replayer)",
			len(fake.recordedRuns))
	}
}

// TestReplayOne_UnknownTableNeverReachesReplayer: directly
// invoke replayOne with a hand-crafted frame carrying an
// invalid table name. The dispatcher MUST return
// ErrUnknownTable and the fake replayer MUST remain
// uncalled.
func TestReplayOne_UnknownTableNeverReachesReplayer(t *testing.T) {
	t.Parallel()
	fake := newFakeReplayer()
	r, _ := NewReconciler(Config{
		Dir:      t.TempDir(),
		Verifier: NoopSignerVerifier{},
		Replayer: fake,
	})
	frame := wal.AuditFrame{
		FrameID:      mustUUID(t, "12345678-1234-1234-1234-123456789012"),
		Table:        wal.Table("solid_batch_finding"), // non-Audit table
		Op:           wal.OpInsert,
		RowPK:        mustUUID(t, "abcdef12-3456-7890-abcd-ef1234567890"),
		RowJSON:      []byte(`{"x":1}`),
		WrittenAt:    time.Now().UTC(),
		SigningKeyID: uuid.Nil,
		Signature:    bytesAllZero(32),
	}
	stats := Stats{}
	err := r.replayOne(context.Background(), frame, &stats)
	if !errors.Is(err, ErrUnknownTable) {
		t.Fatalf("replayOne err: %v want ErrUnknownTable", err)
	}
	if len(fake.recordedRuns)+len(fake.recordedVerdicts)+len(fake.recordedFindings) != 0 {
		t.Fatalf("replayer reached: runs=%d verdicts=%d findings=%d",
			len(fake.recordedRuns), len(fake.recordedVerdicts), len(fake.recordedFindings))
	}
}

// TestReconciler_RowPKMismatchAbortsRun: a tampered frame
// whose RowPK disagrees with row_json.evaluation_run_id
// MUST cause replayOne to return an error wrapping
// ErrRowPKMismatch (which abort Run upstream). Once the
// signature has verified, a RowPK disagreement is a
// durability-coordinate corruption; silently skipping
// would betray the brief's "replay missing rows"
// guarantee. We have to construct the frame WITHOUT going
// through wal.Writer (which would sign over the matching
// RowPK), so we hand-build the frame and bypass
// signature checking by using a verifier that returns nil.
func TestReconciler_RowPKMismatchAbortsRun(t *testing.T) {
	t.Parallel()
	fake := newFakeReplayer()
	r, _ := NewReconciler(Config{
		Dir:      t.TempDir(),
		Verifier: alwaysValidVerifier{},
		Replayer: fake,
	})

	frameRowPK := mustUUID(t, "11111111-1111-1111-1111-111111111111")
	jsonRowPK := mustUUID(t, "22222222-2222-2222-2222-222222222222")
	rowJSON := mustJSON(t, map[string]any{
		"evaluation_run_id": jsonRowPK.String(), // != frame.RowPK
		"repo_id":           mustUUID(t, "33333333-3333-3333-3333-333333333333").String(),
		"sha":               "abc",
		"policy_version_id": mustUUID(t, "44444444-4444-4444-4444-444444444444").String(),
		"caller":            "eval_gate",
		"scope_id":          nil,
		"created_at":        time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00"),
	})

	frame := wal.AuditFrame{
		FrameID:      mustUUID(t, "55555555-5555-5555-5555-555555555555"),
		Table:        wal.TableEvaluationRun,
		Op:           wal.OpInsert,
		RowPK:        frameRowPK,
		RowJSON:      rowJSON,
		WrittenAt:    time.Now().UTC(),
		SigningKeyID: uuid.Nil,
		Signature:    bytesAllZero(32),
	}
	stats := Stats{}
	err := r.replayOne(context.Background(), frame, &stats)
	if !errors.Is(err, ErrRowPKMismatch) {
		t.Fatalf("replayOne err: %v want wraps ErrRowPKMismatch", err)
	}
	if stats.SkippedBadShape.EvaluationRun != 0 {
		t.Fatalf("SkippedBadShape.EvaluationRun: got %d want 0 (post-signature mismatch must NOT count as bad-shape)",
			stats.SkippedBadShape.EvaluationRun)
	}
	if len(fake.recordedRuns) != 0 {
		t.Fatalf("recordedRuns: got %d want 0", len(fake.recordedRuns))
	}
}

// TestReconciler_TransientVerifierErrorAbortsRun: when the
// verifier returns a non-sentinel error (transient infra),
// Run propagates it so the operator can address the root
// cause before retry.
func TestReconciler_TransientVerifierErrorAbortsRun(t *testing.T) {
	t.Parallel()
	staged := stageFrames(t, "eval_gate")
	fake := newFakeReplayer()
	transient := errors.New("KMS unreachable")
	r, _ := NewReconciler(Config{
		Dir:      staged.dir,
		Verifier: errorVerifier{err: transient},
		Replayer: fake,
	})
	_, err := r.Run(context.Background())
	if !errors.Is(err, transient) {
		t.Fatalf("Run: got %v want chain to include %v", err, transient)
	}
	if len(fake.recordedRuns) != 0 {
		t.Fatalf("replayer was called despite verifier transient error")
	}
}

// TestReconciler_ReplayerErrorAbortsRun: when a Replayer
// method returns a non-nil error, Run propagates it.
func TestReconciler_ReplayerErrorAbortsRun(t *testing.T) {
	t.Parallel()
	staged := stageFrames(t, "eval_gate")
	fake := newFakeReplayer()
	fake.runErr = errors.New("pg connection terminated")
	r, _ := NewReconciler(Config{
		Dir:      staged.dir,
		Verifier: NoopSignerVerifier{},
		Replayer: fake,
	})
	_, err := r.Run(context.Background())
	if err == nil {
		t.Fatal("Run: want error from Replayer to propagate, got nil")
	}
	if !strings.Contains(err.Error(), "pg connection terminated") {
		t.Fatalf("Run err: %v want it to mention the pg error", err)
	}
}

// TestReconciler_ContextCanceledBeforeRun: when ctx is
// already canceled, Run returns the ctx.Err immediately.
func TestReconciler_ContextCanceledBeforeRun(t *testing.T) {
	t.Parallel()
	r, _ := NewReconciler(Config{
		Dir:      t.TempDir(),
		Verifier: NoopSignerVerifier{},
		Replayer: newFakeReplayer(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run on canceled ctx: got %v want context.Canceled", err)
	}
}

// alwaysValidVerifier is a test [Verifier] that ALWAYS
// returns nil so reconciler-internal tests can isolate the
// post-verify code path.
type alwaysValidVerifier struct{}

func (alwaysValidVerifier) Verify(ctx context.Context, _ uuid.UUID, _, _ []byte) error {
	return ctx.Err()
}

// errorVerifier returns a fixed non-sentinel error so we
// can prove the reconciler classifies it as transient and
// aborts.
type errorVerifier struct{ err error }

func (e errorVerifier) Verify(ctx context.Context, _ uuid.UUID, _, _ []byte) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return e.err
}
