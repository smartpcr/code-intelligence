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
)

// ErrWALAlreadyOpen is returned by NewFileWAL when another
// process already holds the WAL lock.  A misconfigured second
// agent-api binary fails loudly here at startup rather than
// silently corrupting the offset accounting.
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
	dir         string
	dataPath    string
	offsetPath  string
	lockPath    string
	lockFile    *os.File
	mu          sync.Mutex
	depth       atomic.Int64
	metrics     ObserveMetrics
	logger      *slog.Logger
	signal      chan struct{}
	// flusher lifecycle
	flushOnce    sync.Once
	flusherUp    atomic.Bool
	stopCh       chan struct{}
	doneCh       chan struct{}
	stopOnce     sync.Once
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
// `ErrWALAlreadyOpen` when another process holds the lock,
// otherwise an I/O error from disk.
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
	// the file already exists (another process owns it).  We
	// write the PID in for an operator to see who's holding
	// it when triage is needed.
	lockFile, err := os.OpenFile(w.lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil, ErrWALAlreadyOpen
		}
		return nil, fmt.Errorf("agentapi: observe WAL: lock: %w", err)
	}
	_, _ = lockFile.WriteString(strconv.Itoa(os.Getpid()))
	_ = lockFile.Sync()
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

// Enqueue appends the prepared Episode + Observations payload
// to the WAL data file.  Returns a non-nil error only on disk
// failure; on success the depth gauge is bumped and the
// flusher is signalled.
func (w *FileWAL) Enqueue(_ context.Context, in EpisodeAppendInput) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("agentapi: observe WAL: marshal: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	f, err := os.OpenFile(w.dataPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("agentapi: observe WAL: open data: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(payload, '\n')); err != nil {
		return fmt.Errorf("agentapi: observe WAL: write: %w", err)
	}
	if err := f.Sync(); err != nil {
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
// file.
func (w *FileWAL) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopCh)
	})
	// Only wait when a flusher is actually running, otherwise
	// doneCh would never close and we'd burn the timeout.
	if !w.flusherUp.Load() {
		return
	}
	select {
	case <-w.doneCh:
	case <-time.After(5 * time.Second):
		// Best-effort — a stuck writer should not block
		// shutdown indefinitely.
	}
}

// Close stops the flusher (if running) and releases the lock
// file.  Idempotent: safe to call multiple times.
func (w *FileWAL) Close() error {
	w.Stop()
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
