package wal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/uuid"
)

// WriterConfig configures a [Writer]. Every required field is
// validated by [NewWriter]; defaults apply to the optional
// `Clock` / `UUIDGen` / `SyncFile` / `SyncDir` knobs (tests
// can pin them).
type WriterConfig struct {
	// Dir is the partition root. Per-day files are written
	// as `<Dir>/YYYY-MM-DD.wal` (UTC). Required.
	Dir string

	// Signer signs every staged frame's canonical payload.
	// Required. [NoopSigner] is for tests only -- the
	// composition root MUST wire the real policy/keys-backed
	// shim in production.
	Signer Signer

	// Clock returns the current wall-clock time. Defaults to
	// time.Now when nil. Tests inject a controllable clock to
	// pin the partition file name.
	Clock func() time.Time

	// UUIDGen returns a fresh UUID. Defaults to uuid.NewV4
	// when nil. Tests inject a deterministic generator so
	// `FrameID` assertions are stable.
	UUIDGen func() (uuid.UUID, error)

	// SyncFile is the per-writer fsync seam. Optional;
	// defaults to (*os.File).Sync. Each [Writer] captures
	// its own copy at [NewWriter] time so multiple writers
	// in parallel tests (each constructed with its own
	// failure-injecting closure) do NOT race on a shared
	// global. Prefer this over overriding the package-level
	// syncFile var.
	SyncFile func(*os.File) error

	// SyncDir is the per-writer parent-directory fsync
	// seam. Optional; defaults to opening + Sync()ing the
	// directory. Same per-writer ownership rationale as
	// [WriterConfig.SyncFile]. The returned error is
	// swallowed by [Writer.flush]; the field exists so
	// tests can assert the dir-sync was attempted on the
	// first write to a new partition.
	SyncDir func(string) error
}

// Writer appends signed [AuditFrame] records to per-partition
// files under [WriterConfig.Dir]. Safe for concurrent use; a
// single mutex serialises append+fsync across goroutines so
// frames never interleave inside a partition file.
//
// Per-tx use: callers do NOT call directly on the audit-write
// happy path. They allocate a [TxBatch] via [Writer.NewTxBatch],
// stage frames during the SQL transaction, and call
// [TxBatch.Commit] just before `sql.Tx.Commit`. This preserves
// the "WAL fsync before SQL commit" ordering the architecture
// pins (Sec 7.1).
type Writer struct {
	dir     string
	signer  Signer
	clock   func() time.Time
	newUUID func() (uuid.UUID, error)

	// syncFile / syncDir are per-instance fsync seams.
	// They default to the package-level syncFile / syncDir
	// vars (which in turn default to the real OS syncs) so
	// tests that override the package var continue to work
	// AND tests that want a race-free per-writer seam can
	// inject via [WriterConfig.SyncFile] /
	// [WriterConfig.SyncDir]. Captured once in [NewWriter];
	// never mutated thereafter, so concurrent flushes from
	// the same writer share a stable function value.
	syncFile func(*os.File) error
	syncDir  func(string) error

	mu sync.Mutex
}

// NewWriter constructs a [Writer]. Validates required
// dependencies and ensures the partition directory exists
// (creating it if absent). Returns an error if the directory
// cannot be created.
func NewWriter(cfg WriterConfig) (*Writer, error) {
	if cfg.Dir == "" {
		return nil, ErrDirUnwired
	}
	if cfg.Signer == nil {
		return nil, ErrSignerUnwired
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	newUUID := cfg.UUIDGen
	if newUUID == nil {
		newUUID = uuid.NewV4
	}
	// Per-writer fsync seams. When unset, the default
	// closure indirects through the package-level syncFile
	// / syncDir vars. This preserves backward compatibility
	// for existing tests that override those package vars
	// before constructing a Writer, while NEW tests that
	// want parallel isolation can supply their own
	// closures via WriterConfig and avoid the global
	// entirely.
	syncF := cfg.SyncFile
	if syncF == nil {
		syncF = func(f *os.File) error { return syncFile(f) }
	}
	syncD := cfg.SyncDir
	if syncD == nil {
		syncD = func(dir string) error { return syncDir(dir) }
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("wal: NewWriter: mkdir %s: %w", cfg.Dir, err)
	}
	return &Writer{
		dir:      cfg.Dir,
		signer:   cfg.Signer,
		clock:    clock,
		newUUID:  newUUID,
		syncFile: syncF,
		syncDir:  syncD,
	}, nil
}

// Dir returns the partition root. Read-only -- exposed for
// tests and the Stage 9.2 reconciler.
func (w *Writer) Dir() string { return w.dir }

// NewTxBatch returns a fresh batch bound to this writer. Each
// SQL transaction allocates one batch; the batch stages frames
// in memory and flushes them on [TxBatch.Commit].
//
// CRITICAL: the caller MUST either call [TxBatch.Commit] OR
// [TxBatch.Cancel] before discarding the batch. A `defer
// batch.Cancel()` immediately after allocation is the standard
// idiom -- Cancel is a no-op once Commit has succeeded.
func (w *Writer) NewTxBatch() *TxBatch {
	return &TxBatch{writer: w}
}

// NewFrame builds (but does NOT stage) a fresh [AuditFrame]
// for the supplied row. The frame's `FrameID` and `WrittenAt`
// come from the writer's [WriterConfig.UUIDGen] / Clock; the
// signature is computed via the writer's [Signer].
//
// `rowJSON` MUST be a well-formed JSON byte slice
// representing the full row body. The caller marshals the row
// before calling `NewFrame` so the byte slice can be embedded
// verbatim in the frame.
//
// The frame is NOT staged; the caller chains
// `frame := w.NewFrame(...); batch.Stage(frame)` so the
// staging contract is explicit at the audit-writer call site.
// The [TxBatch.StageNew] convenience does both in one call.
func (w *Writer) NewFrame(ctx context.Context, table Table, rowPK uuid.UUID, rowJSON []byte) (AuditFrame, error) {
	if err := ctx.Err(); err != nil {
		return AuditFrame{}, err
	}
	if !table.IsValid() {
		return AuditFrame{}, fmt.Errorf("wal: NewFrame: table=%q is not a canonical audit table", table)
	}
	if rowPK == uuid.Nil {
		return AuditFrame{}, errors.New("wal: NewFrame: rowPK is the zero uuid")
	}
	if len(rowJSON) == 0 {
		return AuditFrame{}, errors.New("wal: NewFrame: rowJSON is empty")
	}
	if !json.Valid(rowJSON) {
		return AuditFrame{}, errors.New("wal: NewFrame: rowJSON is not well-formed JSON")
	}
	frameID, err := w.newUUID()
	if err != nil {
		return AuditFrame{}, fmt.Errorf("wal: NewFrame: mint frame id: %w", err)
	}
	frame := AuditFrame{
		FrameID:   frameID,
		Table:     table,
		Op:        OpInsert,
		RowPK:     rowPK,
		RowJSON:   append([]byte(nil), rowJSON...),
		WrittenAt: w.clock().UTC(),
	}
	// CALLBACK SIGNING: the signer chooses the key id and
	// we hash it into the canonical payload BEFORE the
	// signature is produced. This guarantees signature
	// recomputation succeeds for any production signer that
	// returns a non-zero key id. Without the callback, a
	// signer returning a real key id would invalidate the
	// signed bytes (which would have been signed with
	// SigningKeyID=uuid.Nil).
	keyID, sig, err := w.signer.SignFrame(ctx, func(keyID uuid.UUID) ([]byte, error) {
		f := frame
		f.SigningKeyID = keyID
		return f.SigningPayload()
	})
	if err != nil {
		return AuditFrame{}, fmt.Errorf("wal: NewFrame: sign: %w", err)
	}
	if len(sig) == 0 {
		return AuditFrame{}, errors.New("wal: NewFrame: signer returned empty signature")
	}
	frame.SigningKeyID = keyID
	frame.Signature = append([]byte(nil), sig...)
	if err := frame.Validate(); err != nil {
		return AuditFrame{}, fmt.Errorf("%w: %v", ErrFrameValidate, err)
	}
	// WRITE-TIME SIZE CAP. The read path enforces
	// [MaxFrameSize] per line; without this matching
	// write-time check, a Rule Engine emitting a huge
	// `finding` row could land an oversized frame on disk
	// that the reconciler would later quarantine via
	// [ErrFrameSizeExceeded] -- breaking the WAL's
	// "write-then-replay" contract because the row in PG
	// would never get a paired replay. We serialise the
	// final frame here (compact, no trailing newline) and
	// reject if the on-disk line, including the trailing
	// '\n' [encodeFrames] will append, would exceed the
	// cap. The serialise-twice cost is negligible against
	// a sign + fsync and is the right place to surface
	// the error to the audit-write call site so the SQL
	// tx rolls back BEFORE we touch disk.
	serialized, err := json.Marshal(frame)
	if err != nil {
		return AuditFrame{}, fmt.Errorf("wal: NewFrame: serialise for size check: %w", err)
	}
	if len(serialized)+1 > MaxFrameSize {
		return AuditFrame{}, fmt.Errorf("%w: frame size %d > MaxFrameSize=%d (table=%s row_pk=%s)",
			ErrFrameSizeExceeded, len(serialized)+1, MaxFrameSize, frame.Table, frame.RowPK)
	}
	return frame, nil
}

// partitionPath returns the absolute path for the partition
// file owning frames with the supplied `writtenAt` UTC date.
func (w *Writer) partitionPath(writtenAt time.Time) string {
	name := writtenAt.UTC().Format("2006-01-02") + ".wal"
	return filepath.Join(w.dir, name)
}

// flush appends a buffered batch of frames to the appropriate
// per-day partition file and fsyncs. Frames are grouped by
// their `WrittenAt` UTC date; each group is written in one
// open + write + sync cycle so a fsync covers the entire
// group atomically. Returns the first error encountered.
//
// Holds the writer mutex for the full duration so concurrent
// flush calls never interleave.
func (w *Writer) flush(ctx context.Context, frames []AuditFrame) error {
	if len(frames) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Group consecutive frames by partition date so we open
	// each file at most once. TxBatch always passes frames
	// in insertion order, which is also their WrittenAt
	// order modulo a clock skew within one tx (negligible).
	groups := make(map[string][]AuditFrame, 1)
	keys := make([]string, 0, 1)
	for _, f := range frames {
		date := f.WrittenAt.UTC().Format("2006-01-02")
		if _, ok := groups[date]; !ok {
			keys = append(keys, date)
		}
		groups[date] = append(groups[date], f)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	for _, date := range keys {
		fname := filepath.Join(w.dir, date+".wal")
		buf, err := encodeFrames(groups[date])
		if err != nil {
			return fmt.Errorf("wal: flush: encode partition %s: %w", date, err)
		}
		// Detect first-write to this partition so we can
		// best-effort fsync the parent directory after
		// creating the entry. A pre-existing file is fine:
		// the parent-dir entry is already durable from a
		// prior process / partition rollover.
		isNew := false
		if _, statErr := os.Stat(fname); errors.Is(statErr, os.ErrNotExist) {
			isNew = true
		}
		if err := w.appendAndSync(fname, buf, isNew); err != nil {
			return fmt.Errorf("wal: flush: append+sync %s: %w", fname, err)
		}
	}
	return nil
}

// encodeFrames serialises the frames to one newline-delimited
// JSON document, with one frame per line. The byte slice is
// the exact buffer the writer appends to the partition file.
//
// Defence-in-depth size cap: even though [Writer.NewFrame]
// rejects oversized frames at staging time, encodeFrames
// re-checks each line's length so a hypothetical bypass path
// (a direct caller that constructed an [AuditFrame] by hand
// and skipped NewFrame) cannot land an oversized line on
// disk via the writer. The check uses the same [MaxFrameSize]
// constant as the reader, keeping writer / reader in lockstep.
func encodeFrames(frames []AuditFrame) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// We want compact, newline-delimited output. Encoder
	// already appends '\n' after each value.
	for _, f := range frames {
		preLen := buf.Len()
		if err := enc.Encode(f); err != nil {
			return nil, err
		}
		lineLen := buf.Len() - preLen
		if lineLen > MaxFrameSize {
			return nil, fmt.Errorf("%w: encoded frame line %d > MaxFrameSize=%d (table=%s row_pk=%s)",
				ErrFrameSizeExceeded, lineLen, MaxFrameSize, f.Table, f.RowPK)
		}
	}
	return buf.Bytes(), nil
}

// syncFile is a legacy package-level fsync seam. New code
// SHOULD inject via [WriterConfig.SyncFile] instead: each
// [Writer] then carries its own seam, so a test that wraps
// its own Writer in `t.Parallel()` cannot race a sibling
// test's override on a shared global. This var is retained
// only so existing tests that override it (and existing
// direct callers of the free [appendAndSync] function) keep
// compiling unchanged.
//
// Production callers always go through (*os.File).Sync.
var syncFile = func(f *os.File) error { return f.Sync() }

// syncDir best-effort fsyncs the parent directory after a new
// partition file is first created. POSIX requires the parent
// dir to be fsynced before the new directory entry is durable.
// On Windows this is a no-op (the Windows file system flushes
// directory metadata as part of the file's CreateFile path).
//
// The error is intentionally swallowed -- a failed parent dir
// fsync should NOT block a frame that already fsynced its own
// bytes; the writer's contract is best-effort durability for
// directory metadata and strict durability for frame bytes.
//
// Same legacy-seam status as [syncFile]: new code SHOULD
// inject via [WriterConfig.SyncDir].
var syncDir = func(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// appendAndSync opens `path` for append, writes `data`, and
// fsyncs the file. The OS-level fsync is essential for the
// "WAL fsync before SQL commit" ordering. Returns the first
// error encountered. The file handle is closed before return.
//
// ATOMICITY CONTRACT: once the kernel has accepted the write
// system call, the bytes MAY be visible to a concurrent reader
// even if the subsequent f.Sync() fails. The writer DOES NOT
// attempt to roll the file back: a truncate-after-failed-sync
// is racy when a second writer has already appended its own
// frames past the failure point. The reconciler is the
// authority for "frame on disk but SQL row absent"
// reconciliation; it looks up the row by (table, row_pk) and
// either inserts (idempotent) or skips. This means the
// four-state atomicity contract for the audit-write call site
// is:
//
//  1. Validation failure -> caller rolls back SQL; the WAL
//     frame was never on disk because appendAndSync was never
//     called.
//  2. appendAndSync returns an error (write or sync) ->
//     caller rolls back SQL. The bytes MAY be readable on
//     disk; if they are, the Stage 9.2 reconciler treats
//     them as a "speculative" frame and replays the row
//     idempotently. The row-replay is safe because the
//     PG insert was rolled back, so the row does not yet
//     exist.
//  3. appendAndSync returns nil + SQL commit succeeds ->
//     the WAL frame and the audit row both exist.
//  4. appendAndSync returns nil + SQL commit fails ->
//     the WAL frame is readable; the reconciler replays
//     the row idempotently.
//
// State 2 is the one this comment exists to be honest about:
// "WAL fsync failure" is NOT a guarantee of "no bytes on
// disk". It is a guarantee of "caller saw an error and
// rolled back SQL"; the reconciler closes the loop.
//
// `isNewPartition` controls whether we issue a best-effort
// parent-directory fsync after the file's first creation.
// Subsequent appends do not need to re-sync the parent dir.
//
// The fsync seams `sf` and `sd` are parameters so each
// [Writer] can supply its own per-instance hooks (via
// [WriterConfig.SyncFile] / [WriterConfig.SyncDir]). The
// public free function [appendAndSync] is retained as a
// thin wrapper using the package-level syncFile / syncDir
// vars for backward compatibility with existing direct
// callers.
func doAppendAndSync(path string, data []byte, isNewPartition bool, sf func(*os.File) error, sd func(string) error) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()
	n, err := f.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return fmt.Errorf("short write: wrote %d of %d bytes", n, len(data))
	}
	if err := sf(f); err != nil {
		return err
	}
	closed = true
	if err := f.Close(); err != nil {
		return err
	}
	if isNewPartition {
		// Best-effort parent-directory fsync so the new
		// directory entry is durable. A failure here does
		// NOT roll back the just-fsynced file -- the
		// frame bytes are durable; only the directory
		// entry may be lost on a crash.
		_ = sd(filepath.Dir(path))
	}
	return nil
}

// appendAndSync is the legacy free-function entry point.
// It delegates to [doAppendAndSync] using the package-level
// syncFile / syncDir vars. Kept so existing tests that
// override those vars (or that call appendAndSync directly,
// e.g. TestAppendAndSync_SyncFailure_LeavesBytesOnDisk)
// continue to compile and behave identically. New code that
// flushes through a [Writer] uses
// [Writer.appendAndSync] instead so the seam is per-instance.
func appendAndSync(path string, data []byte, isNewPartition bool) error {
	return doAppendAndSync(path, data, isNewPartition, syncFile, syncDir)
}

// appendAndSync (method) is the per-Writer entry point used
// by [Writer.flush]. It routes through the Writer's own
// syncFile / syncDir fields so each Writer has its own
// seam -- two parallel tests that each construct their own
// Writer with their own failure-injecting closures cannot
// race on a shared global.
func (w *Writer) appendAndSync(path string, data []byte, isNewPartition bool) error {
	return doAppendAndSync(path, data, isNewPartition, w.syncFile, w.syncDir)
}

// TxBatch stages frames for one SQL transaction. Frames are
// held in memory until [TxBatch.Commit]. On [TxBatch.Cancel]
// they are discarded with no disk write.
//
// A batch is single-use: after Commit or Cancel, every method
// returns [ErrBatchClosed]. The audit-writer call sites
// allocate a fresh batch per *sql.Tx.
//
// Not safe for concurrent use -- a batch belongs to exactly
// one transaction and exactly one goroutine.
type TxBatch struct {
	writer    *Writer
	frames    []AuditFrame
	finalised bool
}

// Stage validates and appends a frame to the batch's in-memory
// staging slice. Does NOT touch disk. Returns [ErrBatchClosed]
// once the batch has been committed or cancelled.
//
// Validation enforces the closed-table set, the closed-op set,
// and the non-zero PK / non-empty payload contract from
// [AuditFrame.Validate]; a misshapen frame fails staging and
// the caller MUST treat the failure as a SQL-rollback trigger.
func (b *TxBatch) Stage(frame AuditFrame) error {
	if b.finalised {
		return ErrBatchClosed
	}
	if err := frame.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrFrameValidate, err)
	}
	b.frames = append(b.frames, frame)
	return nil
}

// Len returns the number of frames currently staged. Used by
// the call site to assert "batch had N frames before commit"
// in tests.
func (b *TxBatch) Len() int { return len(b.frames) }

// Commit flushes every staged frame to its per-day partition
// file and fsyncs. Returns the first error encountered.
//
// Ordering contract:
//
//   - Caller MUST call Commit BEFORE sql.Tx.Commit.
//     The SQL transaction commits ONLY if WAL fsync succeeded.
//   - On Commit failure, the caller MUST rollback the SQL
//     transaction. The frame bytes MAY be readable on disk
//     (a successful write(2) followed by a failing fsync(2)
//     does NOT erase the buffered bytes). The Stage 9.2
//     reconciler observes the speculative frame, sees the
//     SQL row is absent, and replays the INSERT idempotently
//     keyed by (table, row_pk). See [appendAndSync] for the
//     rationale on why the writer does not truncate-back on
//     sync failure.
//   - After successful Commit, frames are durable. If
//     sql.Tx.Commit then fails, the Stage 9.2 reconciler
//     replays the missing rows on the next service start.
//
// After Commit (success or failure) the batch is finalised
// and every subsequent method returns [ErrBatchClosed].
func (b *TxBatch) Commit(ctx context.Context) error {
	if b.finalised {
		return ErrBatchClosed
	}
	b.finalised = true
	if len(b.frames) == 0 {
		return nil
	}
	return b.writer.flush(ctx, b.frames)
}

// Cancel finalises the batch WITHOUT flushing to disk. Safe
// to call after a successful Commit (it's a no-op then) -- the
// standard idiom is `defer batch.Cancel()` immediately after
// allocation so a panic mid-tx still discards the staged
// frames.
func (b *TxBatch) Cancel() {
	if b.finalised {
		return
	}
	b.finalised = true
	b.frames = nil
}

// StageNew is a convenience that mints a fresh frame via
// [Writer.NewFrame] and stages it in one call. Returns the
// minted frame for inspection. Used by the audit-writer call
// sites so the integration boilerplate stays narrow.
func (b *TxBatch) StageNew(ctx context.Context, table Table, rowPK uuid.UUID, rowJSON []byte) (AuditFrame, error) {
	if b.finalised {
		return AuditFrame{}, ErrBatchClosed
	}
	frame, err := b.writer.NewFrame(ctx, table, rowPK, rowJSON)
	if err != nil {
		return AuditFrame{}, err
	}
	if err := b.Stage(frame); err != nil {
		return AuditFrame{}, err
	}
	return frame, nil
}
