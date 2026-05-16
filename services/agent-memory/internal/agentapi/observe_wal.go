// Package agentapi: file-based WAL for the agent.observe
// fallback path.
//
// Stage 5.2 §7.5 contract:
//
//	"if the Episode/Observation write fails because the
//	 partition is unavailable, buffer to a local file-based
//	 queue and return degraded=true,
//	 degraded_reason='episodic_log_unavailable' with the
//	 eventually-assigned episode_id."
//
// The WAL is a single append-only JSON-lines file with an
// offset sidecar that tracks how many bytes have been
// successfully drained. The depth metric
// `observe_wal_buffer_depth` reflects pending entries
// (bytes-after-offset translated into line count) and is kept
// in sync via the ObserveMetrics callback.
//
// Crash safety
// ------------
//   - Enqueue: append one JSON line + LF, fsync the data file,
//     bump the in-memory depth, notify the flusher.
//   - Drain: read the next line from `offset`, call the
//     EpisodeAppender; on success advance `offset` and rewrite
//     the sidecar atomically (write-temp + rename + fsync dir
//     when supported). On failure, leave `offset` at the
//     failing entry so the next drain retries the same payload.
//   - Startup: open the data file, read the offset sidecar (if
//     present), count remaining lines past the offset to seed
//     the depth gauge.
//   - Partial trailing line tolerance: a line without a
//     terminating LF (crash mid-Enqueue) is treated as if it
//     never landed; the offset sidecar never points into the
//     middle of a record.
//
// The WAL is intended for SINGLE-PROCESS use.  The agent-api
// binary runs one instance per pod; running two side-by-side
// against the same WAL dir would interleave writes and
// corrupt the offset accounting.  A process-exclusivity lock
// file lives next to the data file to make a misconfiguration
// fail loudly rather than silently corrupt history.
//
// Stale-lock handling
// -------------------
// The lock file is created with `O_CREATE|O_EXCL` and records
// the holder's PID.  If a previous process was killed by
// SIGKILL / OOM / panic-without-defer the file persists on
// disk after the holder is gone.  In Kubernetes this happens
// routinely (OOM kills, node evictions, SIGKILL on hung
// rollouts).  On EEXIST we therefore read the recorded PID
// and probe whether it is still alive; if the holder is gone
// the stale lock is removed and the open is retried once.  A
// live holder still produces `ErrWALAlreadyOpen` exactly as
// before so a real misconfiguration continues to fail loudly.
//
// Data-file handle lifecycle
// --------------------------
// The append-only data file handle is kept open for the
// lifetime of the FileWAL so each Enqueue pays only write +
// fsync — not the open / write+fsync / close round-trip the
// original implementation did on every call.  Under sustained
// partition outages (the very scenario this WAL exists for)
// that previous per-call open/close was 3+ syscalls of pure
// overhead and excessive file-descriptor churn.  The handle
// is opened lazily on the first Enqueue (so a WAL that never
// has anything written to it does not create an empty file
// on disk) and closed by Close — but only when the flusher
// has actually stopped, so a stuck Drain holding `mu` cannot
// turn the bounded 5-second Stop deadline into an indefinite
// hang.  In the (rare) timeout case the OS reclaims the FD
// at process exit.
package agentapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// walFileName is the name of the append-only WAL data file
	// inside the configured directory.
	walFileName = "observe.wal"
	// walOffsetFileName is the offset sidecar.  Holds the
	// decimal byte offset of the next unread line.
	walOffsetFileName = "observe.wal.offset"
	// walLockFileName is the process-exclusivity lock so a
	// misconfigured second binary instance against the same
	// dir fails loudly.
	walLockFileName = "observe.wal.lock"
	// defaultFlushInterval is the slow-path tick the flusher
	// uses when the writer recovers between enqueues.  Each
	// enqueue ALSO signals the flusher immediately so the
	// drain typically runs well under this interval.
	defaultFlushInterval = 5 * time.Second
	// defaultDrainBatch caps the number of entries a single
	// drain cycle consumes so the flusher does not starve
	// other work when the WAL holds a large backlog.
	defaultDrainBatch = 256
	// lockAcquireMaxAttempts bounds the lock-acquire retry
	// loop so a pathological flap (e.g. another process
	// constantly racing into the just-cleared lock file)
	// cannot spin NewFileWAL forever.  We only need one
	// extra attempt after reclaiming a stale lock.
	lockAcquireMaxAttempts = 2
	// stopWaitDeadline bounds how long Close / Stop will wait
	// for the flusher goroutine to exit.  A stuck Drain (the
	// downstream EpisodeAppender hanging on an external call)
	// would otherwise wedge shutdown indefinitely.
	stopWaitDeadline = 5 * time.Second
)

// ErrWALAlreadyOpen is returned by NewFileWAL when another
// LIVE process already holds the WAL lock.  A misconfigured
// second agent-api binary fails loudly here at startup rather
// than silently corrupting the offset accounting.  Stale lock
// files left by a crashed predecessor are reclaimed
// automatically and do NOT produce this error.
var ErrWALAlreadyOpen = errors.New(
	"agentapi: observe WAL: another process already owns this directory")

// FileWAL is the production WAL implementation.  Construct
// via `NewFileWAL`; call `Enqueue` for the §7.5 fallback path,
// `Drain` from the background flusher, and `Close` at
// shutdown.
//
// Concurrent-use safety: Enqueue / Drain / Depth / Close are
// safe to call from multiple goroutines.  All file mutations
// are serialised behind `mu`.
type FileWAL struct {
	dir        string
	dataPath   string
	offsetPath string
	lockPath   string
	lockFile   *os.File
	// dataFile is the long-lived append-only write handle.
	// It is lazily opened by Enqueue under `mu` on first use
	// and released by Close, eliminating the per-Enqueue
	// open/close syscall churn the original implementation
	// paid on every fallback-path call.  Every access to
	// this field is serialised by `mu`.
	dataFile *os.File
	mu       sync.Mutex
	depth    atomic.Int64
	metrics  ObserveMetrics
	logger   *slog.Logger
	signal   chan struct{}
	// flusher lifecycle
	flushOnce sync.Once
	flusherUp atomic.Bool
	stopCh    chan struct{}
	doneCh    chan struct{}
	stopOnce  sync.Once
}

// FileWALOptions configures NewFileWAL.
type FileWALOptions struct {
	// Metrics receives every depth change.  Optional.
	Metrics ObserveMetrics
	// Logger is the slog handler the WAL emits diagnostics
	// against.  Defaults to slog.Default().
	Logger *slog.Logger
}

// NewFileWAL opens (or creates) the WAL in `dir`.  Returns
// `ErrWALAlreadyOpen` when another LIVE process holds the
// lock, otherwise an I/O error from disk.  Stale lock files
// from a crashed predecessor are reclaimed transparently and
// logged at warn level.
func NewFileWAL(dir string, opts FileWALOptions) (*FileWAL, error) {
	if dir == "" {
		return nil, errors.New("agentapi: observe WAL: empty directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("agentapi: observe WAL: mkdir %s: %w", dir, err)
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	w := &FileWAL{
		dir:        dir,
		dataPath:   filepath.Join(dir, walFileName),
		offsetPath: filepath.Join(dir, walOffsetFileName),
		lockPath:   filepath.Join(dir, walLockFileName),
		metrics:    opts.Metrics,
		logger:     logger,
		signal:     make(chan struct{}, 1),
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
	// Acquire the lock file.  `O_CREATE | O_EXCL` fails if
	// the file already exists; acquireLockFile handles the
	// stale-vs-live disambiguation and writes our PID in for
	// an operator to see who's holding it when triage is
	// needed.
	lockFile, err := w.acquireLockFile()
	if err != nil {
		return nil, err
	}
	w.lockFile = lockFile

	// Seed the depth gauge from on-disk state so a process
	// restart picks up where the previous one left off.
	depth, err := w.recoverDepth()
	if err != nil {
		_ = w.releaseLock()
		return nil, fmt.Errorf("agentapi: observe WAL: recover: %w", err)
	}
	w.depth.Store(depth)
	w.publishDepth(depth)
	logger.Info("agentapi.observe.wal_open",
		slog.String("dir", dir),
		slog.Int64("depth", depth))
	return w, nil
}

// acquireLockFile creates the WAL lock file with O_CREATE|
// O_EXCL.  On EEXIST it inspects the recorded PID: if the
// holder is still alive the call fails with
// ErrWALAlreadyOpen; if the holder is gone (crashed, OOM
// killed, SIGKILLed before Close ran) the stale lock is
// removed and the open is retried exactly once.  Returns the
// open lock file with our PID written and fsynced.
func (w *FileWAL) acquireLockFile() (*os.File, error) {
	for attempt := 1; attempt <= lockAcquireMaxAttempts; attempt++ {
		f, err := os.OpenFile(w.lockPath,
			os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = f.WriteString(strconv.Itoa(os.Getpid()))
			_ = f.Sync()
			return f, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("agentapi: observe WAL: lock: %w", err)
		}
		// Last attempt: do not re-probe; if we got here
		// after already reclaiming once, treat the new lock
		// as a legitimate live owner (someone raced us) and
		// fail loudly rather than spinning.
		if attempt == lockAcquireMaxAttempts {
			return nil, ErrWALAlreadyOpen
		}
		holder, perr := w.readLockHolderPID()
		// Live owner = our own PID (a same-process double-open
		// is, by definition, an in-process conflict — we are
		// running, so any PID equal to ours is alive) OR a
		// foreign PID that responds to a signal-0 probe.  In
		// both cases the existing handle is real and we MUST
		// NOT touch it.
		if perr == nil && holder > 0 &&
			(holder == os.Getpid() || isPIDAlive(holder)) {
			return nil, ErrWALAlreadyOpen
		}
		// Stale (or unreadable / empty / non-numeric) lock —
		// reclaim it.  Log loudly so operators can correlate
		// with the prior crash/OOM in the same pod.
		w.logger.Warn("agentapi.observe.wal_stale_lock_reclaimed",
			slog.String("path", w.lockPath),
			slog.Int("stale_pid", holder),
			slog.String("read_error", errString(perr)))
		if rerr := os.Remove(w.lockPath); rerr != nil && !os.IsNotExist(rerr) {
			return nil, fmt.Errorf(
				"agentapi: observe WAL: remove stale lock: %w", rerr)
		}
	}
	// Unreachable — loop body always returns.
	return nil, ErrWALAlreadyOpen
}

// readLockHolderPID returns the PID stored inside an existing
// lock file.  Returns (0, nil) if the file is empty or holds
// non-numeric content (we still treat it as stale-and-
// reclaimable; an unreadable lock from a crashed predecessor
// is worse than useless).
func (w *FileWAL) readLockHolderPID() (int, error) {
	b, err := os.ReadFile(w.lockPath)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0, nil
	}
	pid, err := strconv.Atoi(s)
	if err != nil {
		return 0, nil
	}
	return pid, nil
}

// isPIDAlive reports whether a process with the given PID is
// currently running.  Used to distinguish a real second-binary
// misconfiguration (return ErrWALAlreadyOpen) from a stale
// lock left by a crashed/OOM-killed predecessor (reclaim).
//
// On Unix this is the standard signal-0 probe:
//   - Signal returns nil          → process exists and we can signal it → alive.
//   - Signal returns EPERM        → process exists, no permission       → alive.
//   - Signal returns ESRCH /
//     errors.Is(ErrProcessDone)   → process is gone                     → dead.
//   - Any other error             → unknown; treat as alive so we never
//     incorrectly reclaim a live owner's lock.
//
// On Windows os.FindProcess opens a real handle; failure to
// open already means the process is gone (returns dead).  If
// the handle does open, Signal(0) is best-effort and we again
// fall back to the conservative "alive" default for unknown
// errors.
func isPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
		return false
	}
	// Unknown error — be conservative: report alive so we
	// never reclaim a lock from a live owner just because
	// the platform returned something we don't recognise.
	return true
}

// errString returns err.Error() or "" so we can pass a nil-
// safe value into slog.String without an extra branch at every
// call site.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Enqueue appends the prepared Episode + Observations payload
// to the WAL data file.  Returns a non-nil error only on disk
// failure; on success the depth gauge is bumped and the
// flusher is signalled.
//
// The data file handle is kept open across calls (lazily
// opened here on first use under `mu`, closed by Close) so
// each enqueue pays only `write` + `fsync` — no open/close
// churn on the hot fallback path during a sustained partition
// outage.  `mu` serialises every Enqueue and every internal
// reader/writer of `w.dataFile`, so the shared handle has
// exactly one user at a time.
//
// On a write or fsync failure the handle is closed and
// cleared so the next Enqueue gets a fresh open — that
// preserves the original implementation's "next call gets a
// fresh fd" recovery property in case the failure was
// handle-specific (e.g. EBADF after an external truncate).
func (w *FileWAL) Enqueue(_ context.Context, in EpisodeAppendInput) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("agentapi: observe WAL: marshal: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dataFile == nil {
		f, err := os.OpenFile(w.dataPath,
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("agentapi: observe WAL: open data: %w", err)
		}
		w.dataFile = f
	}
	if _, err := w.dataFile.Write(append(payload, '\n')); err != nil {
		w.discardDataFileLocked()
		return fmt.Errorf("agentapi: observe WAL: write: %w", err)
	}
	if err := w.dataFile.Sync(); err != nil {
		w.discardDataFileLocked()
		return fmt.Errorf("agentapi: observe WAL: fsync: %w", err)
	}
	d := w.depth.Add(1)
	w.publishDepth(d)
	// Best-effort signal — non-blocking so a busy flusher
	// does not stall the enqueue.
	select {
	case w.signal <- struct{}{}:
	default:
	}
	return nil
}

// discardDataFileLocked closes and nils the kept-open data
// file handle so the next Enqueue gets a fresh open.  Caller
// MUST hold `mu`.  Errors from Close are ignored — the caller
// is already returning a write/fsync error and forcing a
// reopen is the recovery action.
func (w *FileWAL) discardDataFileLocked() {
	if w.dataFile == nil {
		return
	}
	_ = w.dataFile.Close()
	w.dataFile = nil
}

// Depth returns the current number of pending entries.  Safe
// to call from any goroutine.
func (w *FileWAL) Depth() int64 { return w.depth.Load() }

// Drain reads pending WAL entries in ARRIVAL ORDER and feeds
// each one to `writer.Append`.  Stops at the first failing
// entry (the bad entry stays at the head so the next call
// retries it) AND at `batch` entries (so a large backlog does
// not starve other work).  `batch <= 0` falls back to the
// default.
//
// Returns the number of entries successfully drained, plus
// the underlying writer / IO error (or nil on a clean stop at
// either the batch cap or end-of-file).
func (w *FileWAL) Drain(ctx context.Context, writer EpisodeAppender, batch int) (int, error) {
	if writer == nil {
		return 0, errors.New("agentapi: observe WAL: nil EpisodeAppender")
	}
	if batch <= 0 {
		batch = defaultDrainBatch
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	offset, err := w.readOffsetLocked()
	if err != nil {
		return 0, err
	}
	f, err := os.OpenFile(w.dataPath, os.O_RDONLY, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("agentapi: observe WAL: open data for drain: %w", err)
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("agentapi: observe WAL: seek: %w", err)
	}
	reader := bufio.NewReader(f)
	drained := 0
	for drained < batch {
		if ctx.Err() != nil {
			return drained, ctx.Err()
		}
		line, rerr := reader.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] != '\n' {
			// Partial trailing line — crash mid-Enqueue.
			// Treat as if it never landed; the offset
			// stays where it was so a future complete
			// append will pick up at the same position.
			break
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return drained, fmt.Errorf("agentapi: observe WAL: read: %w", rerr)
		}
		var in EpisodeAppendInput
		// Strip the trailing newline before unmarshalling so
		// the JSON parser does not see whitespace.
		if err := json.Unmarshal(line[:len(line)-1], &in); err != nil {
			// Malformed line — skip it but advance the
			// offset (so a corrupt WAL does not stall the
			// drain forever).  Logged at error so the
			// operator sees the corruption.
			w.logger.Error("agentapi.observe.wal_corrupt_entry",
				slog.String("error", err.Error()),
				slog.Int("len", len(line)))
			offset += int64(len(line))
			if err := w.writeOffsetLocked(offset); err != nil {
				return drained, err
			}
			d := w.depth.Add(-1)
			if d < 0 {
				w.depth.Store(0)
				d = 0
			}
			w.publishDepth(d)
			continue
		}
		// Defence-in-depth: every entry that lands in this
		// WAL is, by construction, an
		// `episodic_log_unavailable` fallback (it's the only
		// path that enqueues here). Stamp the degraded
		// fields on REPLAY too so pre-Item-1 backlog entries
		// (which were enqueued without these fields) still
		// land with `degraded=true` on the eventually-flushed
		// `episode` row per architecture.md §7.5. The
		// synchronous Observe path already stamps before
		// enqueue, so this is a no-op for new entries.
		if !in.Degraded {
			in.Degraded = true
			in.DegradedReason = degradedReasonEpisodicLogUnavailable
		}
		if err := writer.Append(ctx, in); err != nil {
			// Writer failure leaves the entry at the
			// head.  The caller decides whether to retry
			// immediately or back off.
			return drained, fmt.Errorf("agentapi: observe WAL: writer: %w", err)
		}
		offset += int64(len(line))
		if err := w.writeOffsetLocked(offset); err != nil {
			return drained, err
		}
		d := w.depth.Add(-1)
		if d < 0 {
			w.depth.Store(0)
			d = 0
		}
		w.publishDepth(d)
		drained++
	}
	// Compaction — when the offset has caught up to file
	// size, truncate so the file does not grow unbounded.
	st, err := os.Stat(w.dataPath)
	if err == nil && offset >= st.Size() {
		if err := w.compactLocked(); err != nil {
			w.logger.Warn("agentapi.observe.wal_compact_failed",
				slog.String("error", err.Error()))
		}
	}
	return drained, nil
}

// StartFlusher launches the background drain loop.  Safe to
// call once per FileWAL instance; subsequent calls are no-ops.
// The flusher uses `tick` as the slow-path interval (defaults
// to defaultFlushInterval); each Enqueue also signals the
// flusher immediately so the typical drain happens promptly.
func (w *FileWAL) StartFlusher(writer EpisodeAppender, tick time.Duration) {
	if tick <= 0 {
		tick = defaultFlushInterval
	}
	w.flushOnce.Do(func() {
		w.flusherUp.Store(true)
		go w.flusherLoop(writer, tick)
	})
}

// Stop signals the flusher goroutine to exit and waits for it.
// Safe to call before StartFlusher (in which case Stop is a
// no-op). Idempotent.  Use Close instead during normal
// shutdown — Close stops the flusher AND releases the lock
// file (and the kept-open data file handle).
func (w *FileWAL) Stop() {
	_ = w.stopAndWait()
}

// stopAndWait runs the Stop logic and reports whether the
// flusher actually exited within the deadline (true) or the
// 5-second wait timed out (false).  Close uses the bool to
// decide whether it is safe to close the kept-open data file
// handle under `mu`: a Drain stuck on a stuck downstream
// writer still holds `mu`, and waiting on it during shutdown
// would convert the bounded deadline into an indefinite hang.
func (w *FileWAL) stopAndWait() bool {
	w.stopOnce.Do(func() {
		close(w.stopCh)
	})
	// Only wait when a flusher is actually running, otherwise
	// doneCh would never close and we'd burn the timeout.
	if !w.flusherUp.Load() {
		return true
	}
	select {
	case <-w.doneCh:
		return true
	case <-time.After(stopWaitDeadline):
		// Best-effort — a stuck writer should not block
		// shutdown indefinitely.
		return false
	}
}

// Close stops the flusher (if running), releases the kept-
// open data file handle, and releases the lock file.
// Idempotent: safe to call multiple times.
//
// If the flusher's bounded shutdown deadline is exceeded
// (a Drain stuck on a downstream writer keeps `mu` held),
// the data file handle is NOT closed here — taking `mu`
// would turn the bounded shutdown into an indefinite hang.
// The OS reclaims the FD at process exit; lock-file cleanup
// always runs so a restart can re-acquire the directory.
func (w *FileWAL) Close() error {
	flusherStopped := w.stopAndWait()
	if flusherStopped {
		w.mu.Lock()
		w.discardDataFileLocked()
		w.mu.Unlock()
	}
	return w.releaseLock()
}

func (w *FileWAL) flusherLoop(writer EpisodeAppender, tick time.Duration) {
	defer close(w.doneCh)
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-w.stopCh:
			// One last drain attempt on the way out so a
			// recently-recovered partition does not lose
			// the tail of the WAL on shutdown.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = w.Drain(ctx, writer, defaultDrainBatch)
			cancel()
			return
		case <-w.signal:
		case <-t.C:
		}
		// Drain until either the WAL empties or the writer
		// errors.  Use a generous batch so a recovered
		// partition can catch up quickly.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		for {
			drained, err := w.Drain(ctx, writer, defaultDrainBatch)
			if err != nil {
				w.logger.Warn("agentapi.observe.wal_drain_failed",
					slog.String("error", err.Error()),
					slog.Int64("depth", w.depth.Load()))
				break
			}
			if drained == 0 || w.depth.Load() == 0 {
				break
			}
		}
		cancel()
	}
}

// recoverDepth seeds the depth gauge from the on-disk state.
// Counts the number of newline-terminated lines past the
// current offset; tolerates a missing data file (returns 0)
// and a missing offset sidecar (treats as offset=0).
func (w *FileWAL) recoverDepth() (int64, error) {
	offset, err := w.readOffsetLocked()
	if err != nil {
		return 0, err
	}
	f, err := os.OpenFile(w.dataPath, os.O_RDONLY, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("agentapi: observe WAL: open data: %w", err)
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("agentapi: observe WAL: seek: %w", err)
	}
	reader := bufio.NewReader(f)
	var count int64
	for {
		line, rerr := reader.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			count++
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return count, fmt.Errorf("agentapi: observe WAL: count: %w", rerr)
		}
	}
	return count, nil
}

func (w *FileWAL) readOffsetLocked() (int64, error) {
	b, err := os.ReadFile(w.offsetPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("agentapi: observe WAL: read offset: %w", err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("agentapi: observe WAL: parse offset %q: %w", s, err)
	}
	if v < 0 {
		v = 0
	}
	return v, nil
}

// writeOffsetLocked persists the offset atomically via
// write-temp + rename.  The sidecar size is tiny (<=20 bytes)
// so the rename is effectively atomic on every modern
// filesystem we target.
func (w *FileWAL) writeOffsetLocked(offset int64) error {
	tmp := w.offsetPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(offset, 10)), 0o644); err != nil {
		return fmt.Errorf("agentapi: observe WAL: write offset tmp: %w", err)
	}
	if err := os.Rename(tmp, w.offsetPath); err != nil {
		return fmt.Errorf("agentapi: observe WAL: rename offset: %w", err)
	}
	return nil
}

// compactLocked truncates the data file + resets the offset
// to 0.  Called when offset >= file size (everything
// drained).  Caller MUST hold w.mu.
//
// The kept-open append-only `dataFile` handle remains valid
// across this call: O_APPEND on Linux positions writes at
// the current EOF atomically per-write, so a subsequent
// Enqueue after a truncate-to-0 lands at offset 0 as
// expected.  No reopen is required.
func (w *FileWAL) compactLocked() error {
	if err := os.Truncate(w.dataPath, 0); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("agentapi: observe WAL: truncate: %w", err)
		}
	}
	return w.writeOffsetLocked(0)
}

func (w *FileWAL) releaseLock() error {
	if w.lockFile == nil {
		return nil
	}
	_ = w.lockFile.Close()
	w.lockFile = nil
	if err := os.Remove(w.lockPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("agentapi: observe WAL: remove lock: %w", err)
	}
	return nil
}

func (w *FileWAL) publishDepth(depth int64) {
	if w.metrics == nil {
		return
	}
	w.metrics.RecordWALDepth(depth)
}

// -- Metrics ----------------------------------------------------------

// Metrics is the package's in-process gauge surface.  Tests
// inspect the Depth() value; production wires a Prometheus
// adapter in `cmd/agent-api/main.go` to expose
// `observe_wal_buffer_depth` over the metrics endpoint.
//
// Concurrent-safe: every accessor uses atomic loads.
type Metrics struct {
	walDepth atomic.Int64
}

// RecordWALDepth implements ObserveMetrics.
func (m *Metrics) RecordWALDepth(depth int64) {
	m.walDepth.Store(depth)
}

// WALDepth returns the most recently recorded WAL depth.  The
// gauge name on the operator dashboard is
// `observe_wal_buffer_depth`.
func (m *Metrics) WALDepth() int64 {
	return m.walDepth.Load()
}
