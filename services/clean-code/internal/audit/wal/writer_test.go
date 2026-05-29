package wal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"
)

// fixedClock returns the same UTC time on every call. Lets the
// tests pin partition file names to a known date.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// counterUUIDGen returns deterministic UUIDs from a seed
// counter so frame IDs are stable across runs.
func counterUUIDGen() func() (uuid.UUID, error) {
	var counter uint64
	return func() (uuid.UUID, error) {
		counter++
		var u uuid.UUID
		// Encode the counter into the last 8 bytes.
		u[8] = byte(counter >> 56)
		u[9] = byte(counter >> 48)
		u[10] = byte(counter >> 40)
		u[11] = byte(counter >> 32)
		u[12] = byte(counter >> 24)
		u[13] = byte(counter >> 16)
		u[14] = byte(counter >> 8)
		u[15] = byte(counter)
		return u, nil
	}
}

func newTestWriter(t *testing.T) (*Writer, string) {
	t.Helper()
	dir := t.TempDir()
	w, err := NewWriter(WriterConfig{
		Dir:     dir,
		Signer:  NoopSigner{},
		Clock:   fixedClock(time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)),
		UUIDGen: counterUUIDGen(),
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	return w, dir
}

// TestNewWriter_RejectsMissingDeps validates the constructor's
// closed-set wiring: a missing Signer or Dir is a fatal
// composition-root mistake, not a degraded path.
func TestNewWriter_RejectsMissingDeps(t *testing.T) {
	t.Run("no_dir", func(t *testing.T) {
		_, err := NewWriter(WriterConfig{Signer: NoopSigner{}})
		if !errors.Is(err, ErrDirUnwired) {
			t.Fatalf("want ErrDirUnwired; got %v", err)
		}
	})
	t.Run("no_signer", func(t *testing.T) {
		_, err := NewWriter(WriterConfig{Dir: t.TempDir()})
		if !errors.Is(err, ErrSignerUnwired) {
			t.Fatalf("want ErrSignerUnwired; got %v", err)
		}
	})
}

// TestAuditFrame_Validate sweeps the closed-set / non-empty
// invariants enforced by [AuditFrame.Validate]. Each subtest
// mutates ONE field away from the happy shape so a regression
// in any single check trips its own test name.
func TestAuditFrame_Validate(t *testing.T) {
	pk := uuid.Must(uuid.NewV4())
	good := AuditFrame{
		FrameID:      uuid.Must(uuid.NewV4()),
		Table:        TableFinding,
		Op:           OpInsert,
		RowPK:        pk,
		RowJSON:      []byte(`{"a":1}`),
		WrittenAt:    time.Now().UTC(),
		SigningKeyID: uuid.Nil,
		Signature:    []byte("sig"),
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("happy frame failed: %v", err)
	}
	t.Run("zero_frame_id", func(t *testing.T) {
		f := good
		f.FrameID = uuid.Nil
		if err := f.Validate(); err == nil {
			t.Fatal("want error for zero FrameID")
		}
	})
	t.Run("non_canonical_table", func(t *testing.T) {
		f := good
		f.Table = "metric_sample"
		if err := f.Validate(); err == nil {
			t.Fatal("want error for non-canonical table")
		}
	})
	t.Run("non_canonical_op", func(t *testing.T) {
		f := good
		f.Op = "update"
		if err := f.Validate(); err == nil {
			t.Fatal("want error for non-canonical op")
		}
	})
	t.Run("zero_row_pk", func(t *testing.T) {
		f := good
		f.RowPK = uuid.Nil
		if err := f.Validate(); err == nil {
			t.Fatal("want error for zero RowPK")
		}
	})
	t.Run("empty_row_json", func(t *testing.T) {
		f := good
		f.RowJSON = nil
		if err := f.Validate(); err == nil {
			t.Fatal("want error for empty RowJSON")
		}
	})
	t.Run("malformed_row_json", func(t *testing.T) {
		f := good
		f.RowJSON = []byte("{not-json")
		if err := f.Validate(); err == nil {
			t.Fatal("want error for malformed RowJSON")
		}
	})
	t.Run("zero_written_at", func(t *testing.T) {
		f := good
		f.WrittenAt = time.Time{}
		if err := f.Validate(); err == nil {
			t.Fatal("want error for zero WrittenAt")
		}
	})
	t.Run("empty_signature", func(t *testing.T) {
		f := good
		f.Signature = nil
		if err := f.Validate(); err == nil {
			t.Fatal("want error for empty Signature")
		}
	})
}

// TestSignAndVerify_RoundTrip pins the canonical-bytes layout:
// signer signs [AuditFrame.SigningPayload] and a recomputation
// must validate.
func TestSignAndVerify_RoundTrip(t *testing.T) {
	w, _ := newTestWriter(t)
	rowPK := uuid.Must(uuid.NewV4())
	frame, err := w.NewFrame(context.Background(), TableEvaluationRun, rowPK, []byte(`{"x":1}`))
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	payload, err := frame.SigningPayload()
	if err != nil {
		t.Fatalf("SigningPayload: %v", err)
	}
	if err := NoopVerify(payload, frame.Signature); err != nil {
		t.Fatalf("NoopVerify: %v", err)
	}
	// Domain prefix appears verbatim at the head of the
	// signing payload.
	if !strings.HasPrefix(string(payload), signingDomainPrefix) {
		t.Fatalf("signing payload missing domain prefix")
	}
	// Tamper: flip a byte in the payload, verification fails.
	tampered := append([]byte(nil), payload...)
	tampered[len(tampered)-1] ^= 0xff
	if err := NoopVerify(tampered, frame.Signature); err == nil {
		t.Fatal("want verification failure on tampered payload")
	}
}

// TestTxBatch_StageThenCommit_WritesFrameToFile verifies the
// successful happy path: stage + commit produces a partition
// file whose newline-delimited frames round-trip through
// [ReadPartition] equal to the staged frames.
func TestTxBatch_StageThenCommit_WritesFrameToFile(t *testing.T) {
	w, dir := newTestWriter(t)
	ctx := context.Background()
	batch := w.NewTxBatch()
	defer batch.Cancel()

	row1 := []byte(`{"evaluation_run_id":"r1","caller":"eval_gate"}`)
	row2 := []byte(`{"verdict_id":"v1","verdict":"pass"}`)
	row3 := []byte(`{"finding_id":"f1","severity":"warn"}`)
	pk1 := uuid.Must(uuid.NewV4())
	pk2 := uuid.Must(uuid.NewV4())
	pk3 := uuid.Must(uuid.NewV4())

	if _, err := batch.StageNew(ctx, TableEvaluationRun, pk1, row1); err != nil {
		t.Fatalf("stage run: %v", err)
	}
	if _, err := batch.StageNew(ctx, TableEvaluationVerdict, pk2, row2); err != nil {
		t.Fatalf("stage verdict: %v", err)
	}
	if _, err := batch.StageNew(ctx, TableFinding, pk3, row3); err != nil {
		t.Fatalf("stage finding: %v", err)
	}
	if batch.Len() != 3 {
		t.Fatalf("want 3 staged frames; got %d", batch.Len())
	}
	if err := batch.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	partition := filepath.Join(dir, "2026-05-27.wal")
	frames, err := ReadPartition(partition)
	if err != nil {
		t.Fatalf("ReadPartition: %v", err)
	}
	if len(frames) != 3 {
		t.Fatalf("want 3 frames; got %d", len(frames))
	}
	wantTables := []Table{TableEvaluationRun, TableEvaluationVerdict, TableFinding}
	wantPKs := []uuid.UUID{pk1, pk2, pk3}
	wantRows := [][]byte{row1, row2, row3}
	for i, f := range frames {
		if f.Table != wantTables[i] {
			t.Errorf("frame[%d] table=%q want %q", i, f.Table, wantTables[i])
		}
		if f.Op != OpInsert {
			t.Errorf("frame[%d] op=%q want %q", i, f.Op, OpInsert)
		}
		if f.RowPK != wantPKs[i] {
			t.Errorf("frame[%d] row_pk=%s want %s", i, f.RowPK, wantPKs[i])
		}
		if string(f.RowJSON) != string(wantRows[i]) {
			t.Errorf("frame[%d] row_json mismatch", i)
		}
		// Validate signature round-trip via NoopVerify so
		// the test exercises the exact bytes-on-disk path
		// the reconciler will eventually consume.
		payload, err := f.SigningPayload()
		if err != nil {
			t.Fatalf("SigningPayload frame[%d]: %v", i, err)
		}
		if err := NoopVerify(payload, f.Signature); err != nil {
			t.Errorf("frame[%d] signature failed verify: %v", i, err)
		}
	}
}

// TestTxBatch_Cancel_NoDiskWrite pins the rollback contract:
// when the batch is cancelled (the SQL transaction rolled
// back), zero frames reach disk and the reconciler cannot
// observe them.
func TestTxBatch_Cancel_NoDiskWrite(t *testing.T) {
	w, dir := newTestWriter(t)
	ctx := context.Background()
	batch := w.NewTxBatch()
	pk := uuid.Must(uuid.NewV4())
	if _, err := batch.StageNew(ctx, TableFinding, pk, []byte(`{"x":1}`)); err != nil {
		t.Fatalf("stage: %v", err)
	}
	batch.Cancel()

	// The partition file MUST NOT exist (we never opened it).
	partition := filepath.Join(dir, "2026-05-27.wal")
	if _, err := os.Stat(partition); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partition file unexpectedly exists after cancel: %v", err)
	}
	// ReadAll returns the empty set.
	frames, err := ReadAll(dir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(frames) != 0 {
		t.Fatalf("want 0 frames after cancel; got %d", len(frames))
	}
}

// TestTxBatch_FinalisedTwice covers the single-use contract:
// every method on a finalised batch returns ErrBatchClosed.
func TestTxBatch_FinalisedTwice(t *testing.T) {
	w, _ := newTestWriter(t)
	ctx := context.Background()
	batch := w.NewTxBatch()
	if err := batch.Commit(ctx); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if _, err := batch.StageNew(ctx, TableFinding, uuid.Must(uuid.NewV4()), []byte(`{}`)); !errors.Is(err, ErrBatchClosed) {
		t.Fatalf("want ErrBatchClosed on post-commit stage; got %v", err)
	}
	if err := batch.Commit(ctx); !errors.Is(err, ErrBatchClosed) {
		t.Fatalf("want ErrBatchClosed on second commit; got %v", err)
	}
	// Cancel is allowed post-finalise (idempotent / no-op).
	batch.Cancel()

	// New batch, then cancel, then stage: also ErrBatchClosed.
	b2 := w.NewTxBatch()
	b2.Cancel()
	if _, err := b2.StageNew(ctx, TableFinding, uuid.Must(uuid.NewV4()), []byte(`{}`)); !errors.Is(err, ErrBatchClosed) {
		t.Fatalf("want ErrBatchClosed on post-cancel stage; got %v", err)
	}
}

// TestTxBatch_AtomicityWithSQLCommit covers the four-state
// invariant table the architecture pins for the
// WAL-fsync-before-SQL-commit ordering. The SQL transaction
// is modelled by a `commit func() error` so the test can
// inject failures at the interesting boundaries.
func TestTxBatch_AtomicityWithSQLCommit(t *testing.T) {
	type result struct {
		walReadable  bool
		sqlCommitted bool
		err          error
	}
	run := func(t *testing.T, sqlCommit func() error, failStaging bool, failBatchCommit bool) result {
		t.Helper()
		dir := t.TempDir()
		signer := NoopSigner{}
		clock := fixedClock(time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC))
		w, err := NewWriter(WriterConfig{Dir: dir, Signer: signer, Clock: clock, UUIDGen: counterUUIDGen()})
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		// Wire the syncFile seam for the fsync-failure
		// branch. The seam is global so we restore it at
		// the end of the sub-run regardless of which
		// branch we took.
		if failBatchCommit {
			prev := syncFile
			syncFile = func(*os.File) error { return errors.New("simulated fsync EIO") }
			defer func() { syncFile = prev }()
		}

		ctx := context.Background()
		batch := w.NewTxBatch()
		defer batch.Cancel()

		// Caller's INSERT-and-stage loop.
		var stageErr error
		row := []byte(`{"x":1}`)
		if failStaging {
			// Inject a frame with a malformed table to
			// trip Validate inside Stage.
			frame, _ := w.NewFrame(ctx, TableFinding, uuid.Must(uuid.NewV4()), row)
			frame.Table = "metric_sample"
			stageErr = batch.Stage(frame)
		} else {
			_, stageErr = batch.StageNew(ctx, TableFinding, uuid.Must(uuid.NewV4()), row)
		}
		if stageErr != nil {
			batch.Cancel()
			frames, _ := ReadAll(dir)
			return result{walReadable: len(frames) > 0, sqlCommitted: false, err: stageErr}
		}

		// WAL fsync BEFORE SQL commit (architecture Sec 7.1).
		if err := batch.Commit(ctx); err != nil {
			frames, _ := ReadAll(dir)
			return result{walReadable: len(frames) > 0, sqlCommitted: false, err: err}
		}

		commitErr := sqlCommit()
		frames, _ := ReadAll(dir)
		return result{
			walReadable:  len(frames) > 0,
			sqlCommitted: commitErr == nil,
			err:          commitErr,
		}
	}

	t.Run("validation_failure_no_wal", func(t *testing.T) {
		r := run(t, func() error { return nil }, true, false)
		if r.err == nil {
			t.Fatal("want staging error")
		}
		if r.walReadable {
			t.Fatal("WAL must NOT be readable on validation failure")
		}
		if r.sqlCommitted {
			t.Fatal("SQL must NOT have been committed")
		}
	})
	t.Run("wal_fsync_failure_returns_error", func(t *testing.T) {
		r := run(t, func() error { return nil }, false, true)
		if r.err == nil {
			t.Fatal("want batch.Commit error when syncFile fails")
		}
		// HONEST CONTRACT: the writer wrote the bytes
		// BEFORE syncing, so the bytes are on disk even
		// though sync failed. The caller saw the error
		// and rolled back SQL. The reconciler will treat
		// the readable frame as a speculative replay
		// candidate; the replay is idempotent because PG
		// never accepted the row.
		if !r.walReadable {
			t.Fatal("WAL bytes MUST be readable after a sync-failure (writer wrote before sync; honest four-state contract)")
		}
		if r.sqlCommitted {
			t.Fatal("SQL must NOT have been committed when WAL fsync failed")
		}
	})
	t.Run("sql_commit_success_wal_readable", func(t *testing.T) {
		r := run(t, func() error { return nil }, false, false)
		if r.err != nil {
			t.Fatalf("unexpected error: %v", r.err)
		}
		if !r.walReadable {
			t.Fatal("WAL frame MUST be readable on the happy path")
		}
		if !r.sqlCommitted {
			t.Fatal("SQL MUST have committed on the happy path")
		}
	})
	t.Run("sql_commit_failure_after_wal_fsync_wal_still_readable", func(t *testing.T) {
		r := run(t, func() error { return errors.New("simulated PG commit failure") }, false, false)
		if r.err == nil {
			t.Fatal("want SQL commit error")
		}
		if !r.walReadable {
			t.Fatal("WAL MUST still be readable when SQL commit fails after WAL fsync (reconciler replays)")
		}
		if r.sqlCommitted {
			t.Fatal("SQL must NOT have committed in this branch")
		}
	})
}

// TestWriter_ConcurrentStageThenCommit verifies the writer's
// per-partition mutex serialises concurrent appends: two
// goroutines each stage and commit a batch; both partition
// writes succeed and ReadPartition returns 2 well-formed
// frames whose contents match each goroutine's input.
func TestWriter_ConcurrentStageThenCommit(t *testing.T) {
	w, dir := newTestWriter(t)
	ctx := context.Background()
	row1 := []byte(`{"a":1}`)
	row2 := []byte(`{"b":2}`)
	pk1 := uuid.Must(uuid.NewV4())
	pk2 := uuid.Must(uuid.NewV4())

	errCh := make(chan error, 2)
	go func() {
		b := w.NewTxBatch()
		defer b.Cancel()
		if _, err := b.StageNew(ctx, TableFinding, pk1, row1); err != nil {
			errCh <- err
			return
		}
		errCh <- b.Commit(ctx)
	}()
	go func() {
		b := w.NewTxBatch()
		defer b.Cancel()
		if _, err := b.StageNew(ctx, TableFinding, pk2, row2); err != nil {
			errCh <- err
			return
		}
		errCh <- b.Commit(ctx)
	}()
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	frames, err := ReadPartition(filepath.Join(dir, "2026-05-27.wal"))
	if err != nil {
		t.Fatalf("ReadPartition: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("want 2 frames after concurrent commits; got %d", len(frames))
	}
	got := map[string]bool{}
	for _, f := range frames {
		got[f.RowPK.String()] = true
	}
	if !got[pk1.String()] || !got[pk2.String()] {
		t.Fatalf("missing one of the concurrent frames: got=%v", got)
	}
}

// TestWriter_PartitionFileName pins the YYYY-MM-DD.wal shape
// the reconciler relies on for ordered replay.
func TestWriter_PartitionFileName(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(WriterConfig{
		Dir:     dir,
		Signer:  NoopSigner{},
		Clock:   fixedClock(time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)),
		UUIDGen: counterUUIDGen(),
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	ctx := context.Background()
	batch := w.NewTxBatch()
	if _, err := batch.StageNew(ctx, TableEvaluationRun, uuid.Must(uuid.NewV4()), []byte(`{"x":1}`)); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if err := batch.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got := filepath.Join(dir, "2026-12-31.wal")
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("partition %s missing: %v", got, err)
	}
}

// TestReadPartition_RoundTripPreservesBytes confirms the
// JSON-Lines format is faithful: a frame written and read
// back matches byte-for-byte on every field the reconciler
// cares about.
func TestReadPartition_RoundTripPreservesBytes(t *testing.T) {
	w, dir := newTestWriter(t)
	ctx := context.Background()
	batch := w.NewTxBatch()
	defer batch.Cancel()
	pk := uuid.Must(uuid.NewV4())
	row := []byte(`{"finding_id":"abc","metric_sample_ids":["a","b"]}`)
	frame, err := batch.StageNew(ctx, TableFinding, pk, row)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if err := batch.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	frames, err := ReadPartition(filepath.Join(dir, "2026-05-27.wal"))
	if err != nil {
		t.Fatalf("ReadPartition: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("want 1 frame; got %d", len(frames))
	}
	got := frames[0]
	if got.FrameID != frame.FrameID {
		t.Errorf("FrameID mismatch")
	}
	if got.Table != frame.Table {
		t.Errorf("Table mismatch")
	}
	if got.Op != frame.Op {
		t.Errorf("Op mismatch")
	}
	if got.RowPK != frame.RowPK {
		t.Errorf("RowPK mismatch")
	}
	if string(got.RowJSON) != string(frame.RowJSON) {
		t.Errorf("RowJSON mismatch: got %q want %q", got.RowJSON, frame.RowJSON)
	}
	if !got.WrittenAt.Equal(frame.WrittenAt) {
		t.Errorf("WrittenAt mismatch: got %v want %v", got.WrittenAt, frame.WrittenAt)
	}
	if string(got.Signature) != string(frame.Signature) {
		t.Errorf("Signature mismatch")
	}
}

// TestEncodeFrames_NewlineDelimited verifies the on-the-wire
// shape: one frame per line, each line a valid JSON object.
func TestEncodeFrames_NewlineDelimited(t *testing.T) {
	w, _ := newTestWriter(t)
	ctx := context.Background()
	f1, err := w.NewFrame(ctx, TableEvaluationRun, uuid.Must(uuid.NewV4()), []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	f2, err := w.NewFrame(ctx, TableFinding, uuid.Must(uuid.NewV4()), []byte(`{"b":2}`))
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	buf, err := encodeFrames([]AuditFrame{f1, f2})
	if err != nil {
		t.Fatalf("encodeFrames: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines; got %d (raw=%q)", len(lines), string(buf))
	}
	for i, line := range lines {
		var probe AuditFrame
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			t.Errorf("line %d not valid JSON: %v (%q)", i, err, line)
		}
	}
}

// TestNoopSigner_RejectsEmptyPayload defends the signer's own
// invariants -- an empty payload would let a caller emit an
// "all-zeros" sha256 signature that pretends to be valid
// without any real bytes to attest.
func TestNoopSigner_RejectsEmptyPayload(t *testing.T) {
	if _, _, err := (NoopSigner{}).SignFrame(context.Background(), func(uuid.UUID) ([]byte, error) { return nil, nil }); err == nil {
		t.Fatal("want error for empty payload")
	}
	if err := NoopVerify(nil, []byte{1, 2}); err == nil {
		t.Fatal("want NoopVerify error for empty payload")
	}
}

// TestSigner_KeyIDInPayload_NonNil pins the invariant that a
// signer returning a non-zero key id MUST produce a signature
// whose recomputation succeeds. The callback API binds the
// key id into the payload before the signature is produced;
// this test exercises that path with a fixed non-zero key id.
func TestSigner_KeyIDInPayload_NonNil(t *testing.T) {
	wantKeyID := uuid.Must(uuid.NewV4())
	w, err := NewWriter(WriterConfig{
		Dir:     t.TempDir(),
		Signer:  fixedKeyIDSigner{keyID: wantKeyID},
		Clock:   fixedClock(time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)),
		UUIDGen: counterUUIDGen(),
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	rowPK := uuid.Must(uuid.NewV4())
	frame, err := w.NewFrame(context.Background(), TableEvaluationRun, rowPK, []byte(`{"x":1}`))
	if err != nil {
		t.Fatalf("NewFrame: %v", err)
	}
	if frame.SigningKeyID != wantKeyID {
		t.Fatalf("SigningKeyID: got %s want %s", frame.SigningKeyID, wantKeyID)
	}
	// Recompute payload from the frame as the reconciler
	// would: the persisted SigningKeyID must hash into the
	// bytes the signer signed.
	payload, err := frame.SigningPayload()
	if err != nil {
		t.Fatalf("SigningPayload: %v", err)
	}
	if err := NoopVerify(payload, frame.Signature); err != nil {
		t.Fatalf("signature must verify when SigningKeyID is hashed before signing: %v", err)
	}
}

// fixedKeyIDSigner is a test-only Signer that returns a fixed
// non-zero key id and signs via SHA-256 of the payload (same
// as NoopSigner). Used to prove the writer hashes the key id
// into the canonical bytes before producing the signature.
type fixedKeyIDSigner struct {
	keyID uuid.UUID
}

func (s fixedKeyIDSigner) SignFrame(ctx context.Context, build func(keyID uuid.UUID) ([]byte, error)) (uuid.UUID, []byte, error) {
	payload, err := build(s.keyID)
	if err != nil {
		return uuid.Nil, nil, err
	}
	if len(payload) == 0 {
		return uuid.Nil, nil, errors.New("empty payload")
	}
	_, sig, err := NoopSigner{}.SignFrame(ctx, func(uuid.UUID) ([]byte, error) { return payload, nil })
	if err != nil {
		return uuid.Nil, nil, err
	}
	return s.keyID, sig, nil
}

// TestAppendAndSync_SyncFailure_LeavesBytesOnDisk pins the
// honest contract for the fsync-failure path: writing bytes
// before fsync means that when fsync fails the writer returns
// an error AND the bytes MAY be readable on disk. The writer
// does NOT attempt a post-fsync truncate-back -- that pattern
// is racy when a sibling process or a writer restart has
// already appended past the failure point. The Stage 9.2
// reconciler closes the loop by replaying speculative frames
// idempotently.
func TestAppendAndSync_SyncFailure_LeavesBytesOnDisk(t *testing.T) {
	prev := syncFile
	syncFile = func(*os.File) error { return errors.New("simulated fsync ENOSPC") }
	t.Cleanup(func() { syncFile = prev })

	dir := t.TempDir()
	path := filepath.Join(dir, "2026-05-27.wal")
	payload := []byte("{\"frame_id\":\"abc\"}\n")
	err := appendAndSync(path, payload, true)
	if err == nil {
		t.Fatal("want error from simulated fsync failure")
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile: %v", readErr)
	}
	if string(got) != string(payload) {
		t.Fatalf("bytes on disk: got %q want %q (writer must NOT truncate after a failed fsync; reconciler quarantines)", string(got), string(payload))
	}
}

// TestTxBatch_Commit_SyncFailure_TxRollback drives the
// wal_fsync_failure branch of the four-state contract through
// the real TxBatch.Commit path with the syncFile seam
// injecting an ENOSPC-style failure.
func TestTxBatch_Commit_SyncFailure_TxRollback(t *testing.T) {
	prev := syncFile
	syncFile = func(*os.File) error { return errors.New("simulated fsync EIO") }
	t.Cleanup(func() { syncFile = prev })

	dir := t.TempDir()
	w, err := NewWriter(WriterConfig{
		Dir:     dir,
		Signer:  NoopSigner{},
		Clock:   fixedClock(time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)),
		UUIDGen: counterUUIDGen(),
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	batch := w.NewTxBatch()
	defer batch.Cancel()
	row := []byte(`{"finding_id":"abc"}`)
	if _, err := batch.StageNew(context.Background(), TableFinding, uuid.Must(uuid.NewV4()), row); err != nil {
		t.Fatalf("StageNew: %v", err)
	}
	if err := batch.Commit(context.Background()); err == nil {
		t.Fatal("want error from TxBatch.Commit when syncFile fails")
	}
	frames, err := ReadAll(dir)
	if err != nil {
		t.Fatalf("ReadAll after sync failure: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("want 1 frame readable after sync-failure (reconciler replays); got %d", len(frames))
	}
	if frames[0].Table != TableFinding {
		t.Fatalf("frame table mismatch: got %s", frames[0].Table)
	}
}

// TestReadPartition_TrailingPartialFrame pins the
// trailing-partial contract: a partition file that ends
// mid-frame (no terminating newline) must return every
// complete frame decoded so far AND the
// ErrTrailingPartialFrame sentinel so the reconciler can
// quarantine the tail without losing the body.
func TestReadPartition_TrailingPartialFrame(t *testing.T) {
	w, dir := newTestWriter(t)
	ctx := context.Background()
	batch := w.NewTxBatch()
	pk1 := uuid.Must(uuid.NewV4())
	if _, err := batch.StageNew(ctx, TableEvaluationRun, pk1, []byte(`{"x":1}`)); err != nil {
		t.Fatalf("StageNew: %v", err)
	}
	if err := batch.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Simulate a crash mid-write: append unterminated bytes.
	path := filepath.Join(dir, "2026-05-27.wal")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open partition: %v", err)
	}
	if _, err := f.Write([]byte(`{"frame_id":"partial"`)); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close partition: %v", err)
	}
	frames, err := ReadPartition(path)
	if !errors.Is(err, ErrTrailingPartialFrame) {
		t.Fatalf("want ErrTrailingPartialFrame; got %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("want 1 complete frame ahead of the partial tail; got %d", len(frames))
	}
	if frames[0].RowPK != pk1 {
		t.Fatalf("frame body mismatch: got RowPK=%s want %s", frames[0].RowPK, pk1)
	}
}

// TestReadAll_PreservesFramesAcrossPartialTail pins the
// invariant that a trailing partial frame in ONE partition
// must not discard completed frames from EARLIER partitions
// or from the preceding lines of the same partition.
func TestReadAll_PreservesFramesAcrossPartialTail(t *testing.T) {
	dir := t.TempDir()
	w1, err := NewWriter(WriterConfig{
		Dir:     dir,
		Signer:  NoopSigner{},
		Clock:   fixedClock(time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)),
		UUIDGen: counterUUIDGen(),
	})
	if err != nil {
		t.Fatalf("NewWriter day1: %v", err)
	}
	b1 := w1.NewTxBatch()
	pk1 := uuid.Must(uuid.NewV4())
	if _, err := b1.StageNew(context.Background(), TableEvaluationRun, pk1, []byte(`{"a":1}`)); err != nil {
		t.Fatalf("StageNew day1: %v", err)
	}
	if err := b1.Commit(context.Background()); err != nil {
		t.Fatalf("Commit day1: %v", err)
	}
	w2, err := NewWriter(WriterConfig{
		Dir:     dir,
		Signer:  NoopSigner{},
		Clock:   fixedClock(time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)),
		UUIDGen: counterUUIDGen(),
	})
	if err != nil {
		t.Fatalf("NewWriter day2: %v", err)
	}
	b2 := w2.NewTxBatch()
	pk2 := uuid.Must(uuid.NewV4())
	if _, err := b2.StageNew(context.Background(), TableEvaluationVerdict, pk2, []byte(`{"b":2}`)); err != nil {
		t.Fatalf("StageNew day2: %v", err)
	}
	if err := b2.Commit(context.Background()); err != nil {
		t.Fatalf("Commit day2: %v", err)
	}
	path2 := filepath.Join(dir, "2026-05-28.wal")
	f, err := os.OpenFile(path2, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open day2: %v", err)
	}
	if _, err := f.Write([]byte(`{"frame_id":"partial`)); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close day2: %v", err)
	}
	frames, err := ReadAll(dir)
	if !errors.Is(err, ErrTrailingPartialFrame) {
		t.Fatalf("ReadAll: want ErrTrailingPartialFrame; got %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("ReadAll must preserve both complete frames; got %d", len(frames))
	}
	seen := map[uuid.UUID]bool{frames[0].RowPK: true, frames[1].RowPK: true}
	if !seen[pk1] || !seen[pk2] {
		t.Fatalf("missing a complete frame across partial-tail boundary; got=%v", seen)
	}
}

// TestReadPartition_FrameSizeExceeded pins the per-line size
// cap on the read path: a single line larger than
// [MaxFrameSize] must surface [ErrFrameSizeExceeded] AND
// preserve every complete frame decoded BEFORE it. The check
// fires before the trailing-partial branch so a huge
// unterminated tail is classified as oversized rather than
// as a benign crash artifact.
func TestReadPartition_FrameSizeExceeded(t *testing.T) {
	w, dir := newTestWriter(t)
	ctx := context.Background()
	batch := w.NewTxBatch()
	pk1 := uuid.Must(uuid.NewV4())
	if _, err := batch.StageNew(ctx, TableEvaluationRun, pk1, []byte(`{"x":1}`)); err != nil {
		t.Fatalf("StageNew: %v", err)
	}
	if err := batch.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	path := filepath.Join(dir, "2026-05-27.wal")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open partition: %v", err)
	}
	huge := bytes.Repeat([]byte("a"), MaxFrameSize+1)
	if _, err := f.Write(huge); err != nil {
		t.Fatalf("write huge: %v", err)
	}
	if _, err := f.Write([]byte("\n")); err != nil {
		t.Fatalf("write terminator: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close partition: %v", err)
	}
	frames, err := ReadPartition(path)
	if !errors.Is(err, ErrFrameSizeExceeded) {
		t.Fatalf("want ErrFrameSizeExceeded; got %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("want 1 complete frame ahead of the oversized one; got %d", len(frames))
	}
	if frames[0].RowPK != pk1 {
		t.Fatalf("first frame mismatch: got RowPK=%s want %s", frames[0].RowPK, pk1)
	}
}

// TestReadPartition_OversizedUnterminatedTail pins the
// ordering invariant: a huge unterminated trailing segment
// must surface as ErrFrameSizeExceeded (the dangerous
// classification), NOT as the benign ErrTrailingPartialFrame.
// The check order in [readFrames] guarantees this.
func TestReadPartition_OversizedUnterminatedTail(t *testing.T) {
	w, dir := newTestWriter(t)
	ctx := context.Background()
	batch := w.NewTxBatch()
	pk1 := uuid.Must(uuid.NewV4())
	if _, err := batch.StageNew(ctx, TableEvaluationRun, pk1, []byte(`{"x":1}`)); err != nil {
		t.Fatalf("StageNew: %v", err)
	}
	if err := batch.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	path := filepath.Join(dir, "2026-05-27.wal")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open partition: %v", err)
	}
	huge := bytes.Repeat([]byte("a"), MaxFrameSize+1)
	if _, err := f.Write(huge); err != nil {
		t.Fatalf("write huge: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close partition: %v", err)
	}
	frames, err := ReadPartition(path)
	if !errors.Is(err, ErrFrameSizeExceeded) {
		t.Fatalf("want ErrFrameSizeExceeded (NOT ErrTrailingPartialFrame); got %v", err)
	}
	if errors.Is(err, ErrTrailingPartialFrame) {
		t.Fatal("oversized unterminated tail must NOT be classified as benign partial frame")
	}
	if len(frames) != 1 {
		t.Fatalf("want 1 complete frame; got %d", len(frames))
	}
}

// TestNewFrame_RejectsOversizedRowJSON pins the writer-side
// size cap: a row JSON whose framed line would exceed
// [MaxFrameSize] is refused at staging time, not silently
// landed on disk for the reader to quarantine. The
// reconciler's "write-then-replay" contract requires the SQL
// tx to roll back when the matching WAL line cannot be
// written, so the failure MUST surface at staging time.
func TestNewFrame_RejectsOversizedRowJSON(t *testing.T) {
	w, _ := newTestWriter(t)
	bigRow := append([]byte(`{"big":"`), bytes.Repeat([]byte("a"), MaxFrameSize)...)
	bigRow = append(bigRow, []byte(`"}`)...)
	_, err := w.NewFrame(context.Background(), TableFinding, uuid.Must(uuid.NewV4()), bigRow)
	if !errors.Is(err, ErrFrameSizeExceeded) {
		t.Fatalf("NewFrame with oversized row: want ErrFrameSizeExceeded, got %v", err)
	}
}

// TestNewFrame_AcceptsLargeButUnderCap asserts the cap is not
// so aggressive that legitimate near-cap frames are rejected.
// A row body half the cap is safely under MaxFrameSize after
// the frame wrapper overhead, even with base64 expansion.
func TestNewFrame_AcceptsLargeButUnderCap(t *testing.T) {
	w, _ := newTestWriter(t)
	body := append([]byte(`{"big":"`), bytes.Repeat([]byte("a"), MaxFrameSize/2)...)
	body = append(body, []byte(`"}`)...)
	frame, err := w.NewFrame(context.Background(), TableFinding, uuid.Must(uuid.NewV4()), body)
	if err != nil {
		t.Fatalf("NewFrame at half-cap: want nil err, got %v", err)
	}
	buf, encErr := encodeFrames([]AuditFrame{frame})
	if encErr != nil {
		t.Fatalf("encodeFrames at half-cap: %v", encErr)
	}
	if len(buf) > MaxFrameSize {
		t.Fatalf("half-cap frame encoded to %d bytes (> MaxFrameSize=%d) but NewFrame accepted it", len(buf), MaxFrameSize)
	}
}

// TestEncodeFrames_RejectsOversizedHandCraftedFrame pins the
// defence-in-depth check in encodeFrames: even if a caller
// bypasses Writer.NewFrame and constructs an [AuditFrame] by
// hand (impossible via the public API but nothing stops a
// malicious package-internal caller), the encoder refuses to
// land an oversized line on disk.
func TestEncodeFrames_RejectsOversizedHandCraftedFrame(t *testing.T) {
	bigRow := append([]byte(`{"big":"`), bytes.Repeat([]byte("a"), MaxFrameSize)...)
	bigRow = append(bigRow, []byte(`"}`)...)
	frame := AuditFrame{
		FrameID:      uuid.Must(uuid.NewV4()),
		Table:        TableFinding,
		Op:           OpInsert,
		RowPK:        uuid.Must(uuid.NewV4()),
		RowJSON:      bigRow,
		WrittenAt:    time.Now().UTC(),
		SigningKeyID: uuid.Must(uuid.NewV4()),
		Signature:    []byte{0x01, 0x02, 0x03},
	}
	_, err := encodeFrames([]AuditFrame{frame})
	if !errors.Is(err, ErrFrameSizeExceeded) {
		t.Fatalf("encodeFrames with oversized hand-crafted frame: want ErrFrameSizeExceeded, got %v", err)
	}
}

// TestWriter_NewFrame_RejectsNonCanonicalTable pins the table
// allow-list at frame-mint time so the WAL never carries a
// frame for a non-audit table.
func TestWriter_NewFrame_RejectsNonCanonicalTable(t *testing.T) {
	w, _ := newTestWriter(t)
	_, err := w.NewFrame(context.Background(), Table("metric_sample"), uuid.Must(uuid.NewV4()), []byte(`{"x":1}`))
	if err == nil {
		t.Fatal("want error for non-audit table; got nil")
	}
}

// TestWriter_NewFrame_RejectsZeroRowPK pins the row PK
// requirement.
func TestWriter_NewFrame_RejectsZeroRowPK(t *testing.T) {
	w, _ := newTestWriter(t)
	_, err := w.NewFrame(context.Background(), TableFinding, uuid.Nil, []byte(`{"x":1}`))
	if err == nil {
		t.Fatal("want error for zero rowPK; got nil")
	}
}
