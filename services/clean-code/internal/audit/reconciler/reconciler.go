package reconciler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/audit/wal"
)

// Config configures a [Reconciler]. Every required field is
// validated by [NewReconciler]; the optional `Logger`
// defaults to a no-op so production wiring with a real
// `*slog.Logger` is one line at the composition root.
type Config struct {
	// Dir is the WAL partition root. Required. Production
	// wiring threads through `CLEAN_CODE_AUDIT_WAL_DIR`
	// (default `data/wal/audit`); the reconciler reads
	// only -- it never writes to this directory.
	Dir string

	// Verifier signs-checks every frame before replay.
	// Required. Test wiring uses
	// [NoopSignerVerifier] (a thin wrapper around
	// [wal.NoopVerify] that fits the [Verifier] interface);
	// production wiring uses a historical-keys adapter
	// that consults `clean_code.policy_signing_keys`
	// directly (the publish-time `policy/keys.Manager.Verify`
	// rejects retired keys, which would silently skip every
	// replayable frame after a rotation).
	Verifier Verifier

	// Replayer is the writer-of-record port. Required.
	// Production wiring constructs an [SQLReplayer]
	// authenticated as `clean_code_wal_reconciler`.
	Replayer Replayer

	// Logger is an optional structured-log hook. The
	// reconciler emits one line per warning surfaced by
	// `wal.ReadAll`, one line on completion with the
	// [Stats] summary, and one line per frame that the
	// dispatcher classified as bad-sig / bad-shape so an
	// operator can correlate counts with disk artifacts.
	// Defaults to a no-op when nil.
	Logger func(msg string, kv ...any)
}

// Reconciler is the Stage 9.2 replay-only worker. Construct
// with [NewReconciler]; invoke [Reconciler.Run] once per
// service restart. The struct is safe for concurrent use
// across goroutines (the underlying `wal.ReadAll` is
// stateless and the [Replayer] interface is documented as
// concurrent-safe), but the canonical use is a single
// startup invocation.
type Reconciler struct {
	dir      string
	verifier Verifier
	replayer Replayer
	logger   func(msg string, kv ...any)
}

// NewReconciler validates `cfg` and constructs a
// [Reconciler]. Returns the corresponding `ErrXxxUnwired`
// sentinel when a required field is missing.
func NewReconciler(cfg Config) (*Reconciler, error) {
	if cfg.Dir == "" {
		return nil, ErrDirUnwired
	}
	if cfg.Verifier == nil {
		return nil, ErrVerifierUnwired
	}
	if cfg.Replayer == nil {
		return nil, ErrReplayerUnwired
	}
	logger := cfg.Logger
	if logger == nil {
		logger = func(string, ...any) {}
	}
	return &Reconciler{
		dir:      cfg.Dir,
		verifier: cfg.Verifier,
		replayer: cfg.Replayer,
		logger:   logger,
	}, nil
}

// Run reads every WAL partition under [Config.Dir],
// verifies each frame, and replays MISSING rows into the
// three Audit tables in two phases:
//
//  1. Every `evaluation_run` frame, in WAL order.
//  2. Every `evaluation_verdict` and `finding` frame, in
//     WAL order.
//
// The two-phase layout guarantees `evaluation_run` PKs
// exist BEFORE any FK-bearing verdict or finding row is
// replayed, even if a corrupted partition has reordered
// frames out of writer-order.
//
// Returns the [Stats] tally and an error. The error is nil
// on a clean run AND on a run that surfaces a
// `wal.ErrTrailingPartialFrame` / `wal.ErrFrameSizeExceeded`
// warning (the warning is reflected in [Stats.Warnings]).
// A non-nil error means a frame caused the verifier or the
// replayer to fail in a way that is NOT a per-frame
// "skip and continue" outcome; the operator should
// investigate before the next restart.
//
// Loud-fail conditions that ABORT Run rather than skip:
//
//   - Verifier returns an unclassified (non-sentinel) error.
//     Implies KMS outage or schema-drifted manager.
//   - row_json decode AFTER a valid signature fails (e.g.
//     unknown JSON field, malformed JSON, trailing
//     content). Implies writer-side schema drift OR an
//     attacker with the signing key -- both warrant
//     operator triage before further replay.
//   - row_json.<pk> disagrees with `frame.RowPK` AFTER a
//     valid signature. Implies a durability-coordinate
//     corruption (the signed payload was produced with a
//     different identity than the WAL coordinate). Cannot
//     happen via a clean writer path; aborting is safer
//     than silently dropping.
//   - Frame Table is outside the closed Audit-table set
//     ({evaluation_run, evaluation_verdict, finding}).
//     Implies a misrouted writer or a malicious frame
//     injection.
//   - Replayer SQL returns any error other than RowsAffected
//     classification.
func (r *Reconciler) Run(ctx context.Context) (Stats, error) {
	if err := ctx.Err(); err != nil {
		return Stats{}, err
	}

	frames, readErr := wal.ReadAll(r.dir)
	stats := Stats{}
	if readErr != nil {
		// Both sentinels are non-fatal warnings; the
		// caller still receives every complete frame
		// preceding the warning so we replay them.
		if errors.Is(readErr, wal.ErrTrailingPartialFrame) {
			msg := fmt.Sprintf("reconciler: wal.ReadAll surfaced ErrTrailingPartialFrame in %s; operator should quarantine the tail bytes", r.dir)
			stats.Warnings = append(stats.Warnings, msg)
			r.logger(msg)
		} else if errors.Is(readErr, wal.ErrFrameSizeExceeded) {
			msg := fmt.Sprintf("reconciler: wal.ReadAll surfaced ErrFrameSizeExceeded in %s; operator must page an on-call to investigate the oversized frame", r.dir)
			stats.Warnings = append(stats.Warnings, msg)
			r.logger(msg)
		} else {
			return stats, fmt.Errorf("reconciler: Run: wal.ReadAll %s: %w", r.dir, readErr)
		}
	}

	// Phase 1: every evaluation_run frame, in WAL order.
	// FK ordering is honoured even on a corrupted
	// partition where a `finding` somehow precedes its
	// owning `evaluation_run` -- the run is INSERTed first
	// during this pass.
	for i := range frames {
		if frames[i].Table != wal.TableEvaluationRun {
			continue
		}
		if err := r.replayOne(ctx, frames[i], &stats); err != nil {
			return stats, fmt.Errorf("reconciler: Run: phase 1 evaluation_run frame %d: %w", i, err)
		}
	}

	// Phase 2: every evaluation_verdict + finding frame,
	// in WAL order. By the time we reach this pass, every
	// `evaluation_run` row the WAL knows about has been
	// reconciled into PG.
	for i := range frames {
		if frames[i].Table == wal.TableEvaluationRun {
			continue
		}
		if err := r.replayOne(ctx, frames[i], &stats); err != nil {
			return stats, fmt.Errorf("reconciler: Run: phase 2 %s frame %d: %w", frames[i].Table, i, err)
		}
	}

	r.logger("reconciler: Run completed",
		"frames", len(frames),
		"replayed_total", stats.Replayed.Total(),
		"skipped_existing_total", stats.SkippedExisting.Total(),
		"skipped_bad_sig_total", stats.SkippedBadSig.Total(),
		"skipped_bad_shape_total", stats.SkippedBadShape.Total(),
		"warnings", len(stats.Warnings),
	)
	return stats, nil
}

// replayOne validates, verifies, parses, and dispatches a
// single frame. Per-frame "skip and continue" outcomes
// update `stats` and return nil; only verifier-infra
// failures and Replayer SQL errors return a non-nil error.
//
// Step ordering matters:
//
//  1. Defence-in-depth: reject unknown tables BEFORE
//     anything else so the "never touches non-Audit
//     table" invariant fails loud at the dispatcher.
//  2. `wal.AuditFrame.Validate()` shape check.
//  3. Signature verification.
//  4. row_json decode (with DisallowUnknownFields to fail
//     loud on schema drift).
//  5. RowPK equality check.
//  6. Dispatch to the matching Replayer method.
func (r *Reconciler) replayOne(ctx context.Context, frame wal.AuditFrame, stats *Stats) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// (1) Defence-in-depth: closed-set audit table guard.
	// `wal.AuditFrame.Validate` (called below) also
	// catches this, but the explicit switch here makes
	// the "never modifies non-Audit table" invariant a
	// grep-friendly check at the reconciler's entry point.
	switch frame.Table {
	case wal.TableEvaluationRun, wal.TableEvaluationVerdict, wal.TableFinding:
		// allowed
	default:
		msg := fmt.Sprintf("reconciler: replayOne: rejecting frame %s: %v", frame.FrameID, ErrUnknownTable)
		r.logger(msg, "table", string(frame.Table))
		// We do NOT count this in SkippedBadShape: the
		// frame was structurally illegal at a layer
		// stronger than "row shape". A WAL with an
		// unknown-table frame is an alarm condition.
		// Returning the sentinel lets the caller decide
		// whether to abort Run (defaults to abort, per
		// our Run loop).
		return fmt.Errorf("%w: table=%q frame_id=%s", ErrUnknownTable, frame.Table, frame.FrameID)
	}

	// (2) Shape check.
	if err := frame.Validate(); err != nil {
		stats.SkippedBadShape.inc(frame.Table)
		r.logger("reconciler: replayOne: frame failed shape validation; skip and count",
			"frame_id", frame.FrameID, "table", string(frame.Table), "err", err)
		return nil
	}

	// (3) Signature verification.
	payload, err := frame.SigningPayload()
	if err != nil {
		stats.SkippedBadShape.inc(frame.Table)
		r.logger("reconciler: replayOne: SigningPayload failed; skip and count",
			"frame_id", frame.FrameID, "table", string(frame.Table), "err", err)
		return nil
	}
	if err := r.verifier.Verify(ctx, frame.SigningKeyID, payload, frame.Signature); err != nil {
		// Classify: durable-broken vs transient-infra.
		if errors.Is(err, ErrSignatureInvalid) || errors.Is(err, ErrSigningKeyUnknown) {
			stats.SkippedBadSig.inc(frame.Table)
			r.logger("reconciler: replayOne: frame signature invalid; skip and count",
				"frame_id", frame.FrameID, "table", string(frame.Table),
				"signing_key_id", frame.SigningKeyID, "err", err)
			return nil
		}
		// Transient-infra: abort Run so an operator can
		// address the verifier outage before retry. The
		// brief's "replay-only" guarantee depends on a
		// healthy verifier; silently skipping every frame
		// on a verifier outage erases the durability
		// guarantee Stage 9.1 set up.
		return fmt.Errorf("reconciler: replayOne: verifier returned non-classified error (transient infra?): %w", err)
	}

	// (4) row_json decode with DisallowUnknownFields so a
	// future audit-column addition that the reconciler
	// hasn't been updated for fails LOUD rather than
	// silently dropping the new column on replay.
	//
	// (5) RowPK equality check. (Done per table below.)
	//
	// IMPORTANT: Step (4) and step (5) BOTH abort Run on
	// failure rather than counting SkippedBadShape. Once
	// the signature has verified, any disagreement
	// between row_json and the WAL coordinates means
	// EITHER a writer-side schema drift OR a
	// durability-coordinate corruption -- both warrant
	// loud, immediate operator triage. Silently skipping
	// would betray the brief's "replay missing rows"
	// guarantee (the rows would silently never reach PG).
	//
	// (6) Dispatch.
	switch frame.Table {
	case wal.TableEvaluationRun:
		var row EvaluationRunRow
		if err := decodeStrict(frame.RowJSON, &row); err != nil {
			return fmt.Errorf("reconciler: replayOne: evaluation_run row_json decode failed AFTER valid signature (writer-side schema drift or signing-key compromise) frame_id=%s row_pk=%s: %w",
				frame.FrameID, frame.RowPK, err)
		}
		if row.EvaluationRunID != frame.RowPK {
			return fmt.Errorf("reconciler: replayOne: evaluation_run %w frame_id=%s frame.RowPK=%s row.EvaluationRunID=%s",
				ErrRowPKMismatch, frame.FrameID, frame.RowPK, row.EvaluationRunID)
		}
		outcome, err := r.replayer.ReplayRun(ctx, row)
		if err != nil {
			return fmt.Errorf("reconciler: replayOne: ReplayRun frame_id=%s row_pk=%s: %w", frame.FrameID, frame.RowPK, err)
		}
		tallyOutcome(stats, frame.Table, outcome)
		return nil

	case wal.TableEvaluationVerdict:
		var row EvaluationVerdictRow
		if err := decodeStrict(frame.RowJSON, &row); err != nil {
			return fmt.Errorf("reconciler: replayOne: evaluation_verdict row_json decode failed AFTER valid signature (writer-side schema drift or signing-key compromise) frame_id=%s row_pk=%s: %w",
				frame.FrameID, frame.RowPK, err)
		}
		if row.VerdictID != frame.RowPK {
			return fmt.Errorf("reconciler: replayOne: evaluation_verdict %w frame_id=%s frame.RowPK=%s row.VerdictID=%s",
				ErrRowPKMismatch, frame.FrameID, frame.RowPK, row.VerdictID)
		}
		outcome, err := r.replayer.ReplayVerdict(ctx, row)
		if err != nil {
			return fmt.Errorf("reconciler: replayOne: ReplayVerdict frame_id=%s row_pk=%s: %w", frame.FrameID, frame.RowPK, err)
		}
		tallyOutcome(stats, frame.Table, outcome)
		return nil

	case wal.TableFinding:
		var row FindingRow
		if err := decodeStrict(frame.RowJSON, &row); err != nil {
			return fmt.Errorf("reconciler: replayOne: finding row_json decode failed AFTER valid signature (writer-side schema drift or signing-key compromise) frame_id=%s row_pk=%s: %w",
				frame.FrameID, frame.RowPK, err)
		}
		if row.FindingID != frame.RowPK {
			return fmt.Errorf("reconciler: replayOne: finding %w frame_id=%s frame.RowPK=%s row.FindingID=%s",
				ErrRowPKMismatch, frame.FrameID, frame.RowPK, row.FindingID)
		}
		outcome, err := r.replayer.ReplayFinding(ctx, row)
		if err != nil {
			return fmt.Errorf("reconciler: replayOne: ReplayFinding frame_id=%s row_pk=%s: %w", frame.FrameID, frame.RowPK, err)
		}
		tallyOutcome(stats, frame.Table, outcome)
		return nil

	default:
		// Already caught by the step-1 guard above; this
		// branch is unreachable, but the compiler requires
		// a default and the explicit sentinel return keeps
		// the invariant grep-friendly.
		return fmt.Errorf("%w: table=%q frame_id=%s", ErrUnknownTable, frame.Table, frame.FrameID)
	}
}

// tallyOutcome bumps the matching counter for an
// [Outcome]+[wal.Table] pair.
func tallyOutcome(stats *Stats, t wal.Table, outcome Outcome) {
	switch outcome {
	case OutcomeInserted:
		stats.Replayed.inc(t)
	case OutcomeSkippedExisting:
		stats.SkippedExisting.inc(t)
	}
}

// decodeStrict json-decodes `data` into `dst` with
// `DisallowUnknownFields()` so a future schema change that
// adds a column to one of the Audit tables fails LOUD on
// the reconciler side rather than silently dropping the new
// column on replay. The WAL is the durability source of
// truth; partial replays would betray the brief's "replay
// missing rows" guarantee.
//
// The second [json.Decoder.Decode] call is a defence
// against concatenated-row injection. Three outcomes:
//
//   - `err == nil` -- another JSON value followed the
//     expected object; reject with a "trailing JSON" error.
//   - `errors.Is(err, io.EOF)` -- exactly one object was on
//     the wire; this is the success path.
//   - any other error -- malformed bytes followed the
//     expected object (e.g. an unbalanced brace or a
//     control character). The PRE-iter-2 implementation
//     swallowed these errors as success; the evaluator
//     caught the bug. We now propagate as a malformed-
//     trailing-content failure so the reconciler aborts on
//     a frame whose row_json carries garbage after the
//     well-formed prefix.
func decodeStrict(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var tail json.RawMessage
	err := dec.Decode(&tail)
	if err == nil {
		return fmt.Errorf("reconciler: decodeStrict: unexpected trailing JSON content")
	}
	if !errors.Is(err, io.EOF) {
		return fmt.Errorf("reconciler: decodeStrict: malformed trailing content after expected object: %w", err)
	}
	return nil
}

// NoopSignerVerifier is a [Verifier] backed by
// [wal.NoopVerify]. It exists so reconciler tests can pair
// a `wal.Writer` constructed with [wal.NoopSigner] (which
// produces SHA-256 stand-in signatures) with a verifier
// that knows the same shape.
//
// NEVER use NoopSignerVerifier in production -- the
// architecture's tamper-evidence guarantee requires a real
// Ed25519 signature.
type NoopSignerVerifier struct{}

// Verify implements [Verifier] by delegating to
// [wal.NoopVerify]. Maps the wal-package error message to
// [ErrSignatureInvalid] so the [Reconciler] classifies a
// mismatch as "frame durably broken" rather than
// "transient infrastructure failure".
func (NoopSignerVerifier) Verify(ctx context.Context, _ uuid.UUID, payload, signature []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := wal.NoopVerify(payload, signature); err != nil {
		return fmt.Errorf("%w: %v", ErrSignatureInvalid, err)
	}
	return nil
}

// Compile-time check.
var _ Verifier = NoopSignerVerifier{}
