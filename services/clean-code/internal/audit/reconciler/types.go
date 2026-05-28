package reconciler

import (
	"context"
	"errors"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/audit/wal"
)

// Verifier is the narrow port the [Reconciler] uses to
// validate every frame's signature before replaying its
// row. The interface is intentionally smaller than
// `policy/keys.Manager.Verify`:
//
//   - The reconciler resolves HISTORICAL keys -- a frame
//     signed yesterday by a now-retired key must still
//     verify on replay. The publish-time
//     `Manager.Verify` rejects retired keys on purpose; the
//     composition root binds a separate adapter (see
//     `internal/composition/wal_reconciler.go`) that
//     consults the full `clean_code.policy_signing_keys`
//     history.
//   - The interface returns sentinel-checkable errors so
//     [Reconciler.Run] can classify a failure as either
//     "frame is durably broken -> skip + count" or
//     "transient infra failure -> abort the run".
//
// `payload` is the canonical signed bytes returned by
// [wal.AuditFrame.SigningPayload]. `signature` is the
// signature bytes recorded on the frame.
type Verifier interface {
	// Verify reports whether `signature` was produced by
	// the key identified by `keyID` over `payload`.
	//
	// Returns:
	//   - nil on success.
	//   - An error wrapping [ErrSignatureInvalid] when the
	//     signature is well-formed but does not validate
	//     against the named key.
	//   - An error wrapping [ErrSigningKeyUnknown] when
	//     `keyID` does not resolve to a known key in any
	//     state (active or retired).
	//   - Any other error for transient infrastructure
	//     failures (KMS unreachable, DB outage). The
	//     [Reconciler] aborts on these so the operator can
	//     address the root cause before retrying.
	Verify(ctx context.Context, keyID uuid.UUID, payload, signature []byte) error
}

// Sentinel errors. Defined as exported sentinels so callers
// can branch via `errors.Is` rather than string-matching the
// wrapped message.
var (
	// ErrSignatureInvalid is the [Verifier] sentinel for a
	// frame whose signature does not validate against its
	// recorded `signing_key_id`. The [Reconciler] counts
	// the frame in [Stats.SkippedBadSig] and continues with
	// the next frame.
	ErrSignatureInvalid = errors.New("reconciler: frame signature did not validate against its signing_key_id")

	// ErrSigningKeyUnknown is the [Verifier] sentinel for a
	// frame whose `signing_key_id` cannot be resolved to a
	// known key (active or retired). The [Reconciler]
	// counts the frame in [Stats.SkippedBadSig] and
	// continues -- a frame produced by a key the verifier
	// no longer knows about is, from the reconciler's
	// perspective, unreplayable.
	ErrSigningKeyUnknown = errors.New("reconciler: frame signing_key_id is not in the historical key cache")

	// ErrUnknownTable is returned by the per-frame replay
	// dispatcher when `frame.Table` is not in the closed
	// audit set ([wal.TableEvaluationRun],
	// [wal.TableEvaluationVerdict], [wal.TableFinding]).
	// This is defence-in-depth: [wal.AuditFrame.Validate]
	// (called by [wal.ReadAll]) already rejects unknown
	// tables. A frame that reaches the reconciler's
	// dispatcher with an unknown table indicates the WAL's
	// closed-set guard was bypassed AND signals the
	// invariant "reconciler never modifies a non-Audit
	// table" needs to fail loud.
	ErrUnknownTable = errors.New("reconciler: frame.Table is not in the closed audit set {evaluation_run, evaluation_verdict, finding}")

	// ErrRowPKMismatch is returned when the parsed
	// row-JSON primary key does not equal the frame's
	// [wal.AuditFrame.RowPK]. The brief's "never inserts
	// a row whose `(table, row_pk)` already exists" guard
	// uses `RowPK` as the dedup coordinate; if `RowPK`
	// disagrees with `row_json.<pk>` AFTER a valid
	// signature, the durability coordinate is corrupted
	// and the reconciler ABORTS Run rather than silently
	// dropping the frame (silent skip would betray the
	// brief's "replay missing rows" guarantee). Operators
	// must triage the offending partition before retry.
	ErrRowPKMismatch = errors.New("reconciler: frame.RowPK does not match row_json primary key after valid signature; aborting Run for operator triage")

	// ErrVerifierUnwired is returned by [NewReconciler]
	// when the supplied [Config] omits the [Verifier].
	// Production wiring REQUIRES a real verifier; the
	// constructor refuses a nil seam so a misconfigured
	// composition cannot silently bypass signature
	// verification.
	ErrVerifierUnwired = errors.New("reconciler: NewReconciler: Verifier is required")

	// ErrReplayerUnwired is returned by [NewReconciler]
	// when the supplied [Config] omits the [Replayer].
	ErrReplayerUnwired = errors.New("reconciler: NewReconciler: Replayer is required")

	// ErrDirUnwired is returned by [NewReconciler] when
	// the supplied [Config] omits the WAL partition
	// directory.
	ErrDirUnwired = errors.New("reconciler: NewReconciler: Dir is required")

	// ErrFrameValidation is returned by the dispatcher
	// when [wal.AuditFrame.Validate] rejects the frame
	// shape. Counted in [Stats.SkippedBadShape].
	ErrFrameValidation = errors.New("reconciler: frame failed [wal.AuditFrame.Validate]")
)

// Stats is the per-Run summary returned by [Reconciler.Run].
// Per-table counters let the OTel telemetry stage (Stage
// 9.4) publish Prometheus counters with a `table` label;
// the slice-of-strings [Stats.Warnings] surfaces non-fatal
// signals from `wal.ReadAll` (trailing partial frame,
// oversized frame) so the runbook's operator checklist can
// react.
//
// Counters are scoped per Audit table so a per-table dashboard
// is straightforward; a single rollup is one summation away.
type Stats struct {
	// Replayed counts frames whose replay produced a fresh
	// INSERT (RowsAffected = 1).
	Replayed PerTable
	// SkippedExisting counts frames whose `(table, row_pk)`
	// already existed in PostgreSQL (RowsAffected = 0 under
	// the ON CONFLICT DO NOTHING path).
	SkippedExisting PerTable
	// SkippedBadSig counts frames whose signature did not
	// verify ([ErrSignatureInvalid] / [ErrSigningKeyUnknown]).
	SkippedBadSig PerTable
	// SkippedBadShape counts frames whose
	// PRE-SIGNATURE-CHECK structural validation failed:
	// `wal.AuditFrame.Validate` rejected the shape
	// ([ErrFrameValidation]) or `SigningPayload()` could
	// not canonicalize the frame. Post-signature
	// failures (decode rejection, RowPK mismatch) are
	// LOUD aborts -- they are NOT counted here.
	SkippedBadShape PerTable
	// Warnings carries non-fatal signals surfaced by
	// `wal.ReadAll`:
	//   * "trailing partial frame in <path>" --
	//     [wal.ErrTrailingPartialFrame].
	//   * "frame size exceeded in <path>" --
	//     [wal.ErrFrameSizeExceeded].
	// The reconciler replays every complete frame that
	// preceded the warning; the operator runbook calls
	// for a manual quarantine of the tail bytes.
	Warnings []string
}

// PerTable bundles a counter per audit table. Three named
// fields are clearer than a `map[wal.Table]int` and let
// callers compute sums via the [PerTable.Total] helper.
type PerTable struct {
	EvaluationRun     int
	EvaluationVerdict int
	Finding           int
}

// Total returns the sum across all three audit tables.
func (p PerTable) Total() int {
	return p.EvaluationRun + p.EvaluationVerdict + p.Finding
}

// inc bumps the counter for the supplied table.
func (p *PerTable) inc(t wal.Table) {
	switch t {
	case wal.TableEvaluationRun:
		p.EvaluationRun++
	case wal.TableEvaluationVerdict:
		p.EvaluationVerdict++
	case wal.TableFinding:
		p.Finding++
	}
}
