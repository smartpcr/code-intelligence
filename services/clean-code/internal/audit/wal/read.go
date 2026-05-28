package wal

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ReadPartition opens the per-day partition file at `path` and
// returns every frame contained in it, in append order. A
// trailing partial JSON line (e.g. from a crash mid-write) is
// surfaced as [ErrTrailingPartialFrame] so the caller can
// quarantine it -- the Stage 9.2 reconciler treats a partial
// trailing frame as "skip"; tests use the error to assert that
// the writer never produces partial lines on its own.
//
// Returns `(nil, nil)` if the file does not exist -- the empty
// partition is a normal steady state (no audit writes that day
// yet).
func ReadPartition(path string) ([]AuditFrame, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("wal: ReadPartition: open %s: %w", path, err)
	}
	defer f.Close()
	return readFrames(f)
}

// ReadAll walks the directory `dir` for `YYYY-MM-DD.wal`
// files, opens each in date order, and returns every frame
// across every partition. Helper for tests and the
// reconciler's start-up sweep.
//
// If any partition contains a trailing partial frame OR an
// oversized frame, every complete frame preceding it (and
// every frame from earlier partitions) is still returned in
// `out`; the function surfaces [ErrTrailingPartialFrame] or
// [ErrFrameSizeExceeded] alongside the data so the caller
// can choose to quarantine or replay.
//
// When both sentinels are observed across the sweep, the
// LAST one encountered wins -- both are non-fatal signals,
// and the caller can re-scan the partitions if it needs
// per-file classification.
func ReadAll(dir string) ([]AuditFrame, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("wal: ReadAll: list %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !isPartitionFile(name) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	var (
		out        []AuditFrame
		warningErr error
	)
	for _, name := range names {
		frames, err := ReadPartition(filepath.Join(dir, name))
		// ErrTrailingPartialFrame and ErrFrameSizeExceeded
		// are non-fatal: the reconciler must see every
		// complete frame, then decide what to do with the
		// quarantined tail / oversized line. Accumulate
		// the sentinel signal so the caller can react
		// after the full sweep.
		out = append(out, frames...)
		if err != nil {
			if errors.Is(err, ErrTrailingPartialFrame) || errors.Is(err, ErrFrameSizeExceeded) {
				warningErr = err
				continue
			}
			return out, err
		}
	}
	return out, warningErr
}

// isPartitionFile reports whether `name` matches the
// `YYYY-MM-DD.wal` shape. Lets the reconciler ignore stray
// non-WAL files in the directory.
func isPartitionFile(name string) bool {
	if len(name) != len("YYYY-MM-DD.wal") {
		return false
	}
	if name[len(name)-4:] != ".wal" {
		return false
	}
	_, err := time.Parse("2006-01-02", name[:len("YYYY-MM-DD")])
	return err == nil
}

// ErrTrailingPartialFrame is returned by [ReadPartition] when
// the last record in a partition file is not a complete JSON
// line (the file ends mid-frame). All complete frames before
// the partial tail are returned alongside the error so the
// reconciler can replay them and quarantine the tail.
var ErrTrailingPartialFrame = errors.New("wal: partition has a trailing partial frame")

// ErrFrameSizeExceeded is returned by [ReadPartition] when a
// single frame line is larger than [MaxFrameSize] bytes. All
// complete frames decoded BEFORE the oversized frame are
// returned alongside the error so the reconciler can replay
// them; the oversized frame is quarantined and reported via
// this sentinel rather than silently parsed (an oversized
// frame is either a writer bug or an attacker forging a huge
// payload, and the reconciler must never blindly replay it
// into the canonical audit tables).
//
// Frames AFTER the oversized one are NOT returned -- a single
// reader pass cannot safely resume from the next newline
// without re-scanning the file for a recognisable record
// boundary, and the JSONL format provides no such marker.
// The reconciler treats this state as "stop and page an
// operator"; operations should not silently skip past an
// oversized frame.
var ErrFrameSizeExceeded = errors.New("wal: frame size exceeded")

// MaxFrameSize is the upper bound (in bytes) on a single
// serialised [AuditFrame] line. A worst-case `finding` row
// with many metric_sample_ids stays well under 1 MiB; the
// cap exists to (a) bound a single `json.Unmarshal` call's
// memory cost, (b) reject crash-tail bytes that exceed the
// writer's contract, and (c) surface obviously-forged frames
// instead of replaying them. See [ErrFrameSizeExceeded].
const MaxFrameSize = 1 << 20

// readFrames parses the newline-delimited JSON document on
// `r` into a slice of [AuditFrame]. Each frame is validated
// via [AuditFrame.Validate]; a malformed JSON line stops the
// read with a hard error (the reconciler would replay
// malformed audit records into the canonical tables, which
// is worse than failing loudly).
//
// Two non-fatal sentinels are surfaced alongside the frames
// decoded so far:
//
//   - [ErrTrailingPartialFrame] -- bytes after the last
//     newline do not form a complete JSON object.
//   - [ErrFrameSizeExceeded] -- a single line is longer
//     than [MaxFrameSize]. The check fires BEFORE the
//     trailing-partial check so a huge unterminated tail
//     is reported as oversized rather than as a benign
//     partial frame.
func readFrames(r io.Reader) ([]AuditFrame, error) {
	// Total-file cap: 16 GiB (MaxFrameSize * 2^14). Per-line
	// cap is enforced below by inspecting the
	// newline-to-newline distance.
	buf, err := io.ReadAll(io.LimitReader(r, int64(MaxFrameSize)*int64(1<<14)))
	if err != nil {
		return nil, fmt.Errorf("wal: readFrames: read: %w", err)
	}
	var out []AuditFrame
	i := 0
	for i < len(buf) {
		j := i
		for j < len(buf) && buf[j] != '\n' {
			j++
		}
		// Per-frame size cap. Enforced BEFORE the
		// trailing-partial check so a huge unterminated
		// tail surfaces as ErrFrameSizeExceeded, not as
		// a benign crash artifact.
		if j-i > MaxFrameSize {
			return out, fmt.Errorf("%w: frame %d is %d bytes (max %d)",
				ErrFrameSizeExceeded, len(out), j-i, MaxFrameSize)
		}
		if j == len(buf) {
			// No terminating newline -- this is either
			// an empty tail (i == j == len(buf)) or a
			// trailing partial frame the writer did not
			// finish.
			if j > i {
				return out, ErrTrailingPartialFrame
			}
			break
		}
		line := buf[i:j]
		i = j + 1
		if len(line) == 0 {
			continue
		}
		var f AuditFrame
		if err := json.Unmarshal(line, &f); err != nil {
			return out, fmt.Errorf("wal: readFrames: parse frame %d: %w", len(out), err)
		}
		if err := f.Validate(); err != nil {
			return out, fmt.Errorf("%w: frame %d: %v", ErrFrameValidate, len(out), err)
		}
		out = append(out, f)
	}
	return out, nil
}
