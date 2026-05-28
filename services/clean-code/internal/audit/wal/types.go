package wal

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gofrs/uuid"
)

// Table is the closed enum of audit tables whose writes the
// WAL frames cover (architecture Sec 7.1 / tech-spec Sec 4.13).
// Any string outside this set is rejected by [AuditFrame.Validate]
// -- the WAL contract is scoped EXCLUSIVELY to the three audit
// tables; catalog / measurement / policy / refactor writes MUST
// NOT be staged as frames.
type Table string

// Canonical [Table] values. The bare table name appears in the
// frame (not the schema-qualified identifier) so the reconciler
// can route to the correct INSERT statement without parsing.
const (
	TableEvaluationRun     Table = "evaluation_run"
	TableEvaluationVerdict Table = "evaluation_verdict"
	TableFinding           Table = "finding"
)

// IsValid reports whether t is a member of the closed audit
// table set.
func (t Table) IsValid() bool {
	switch t {
	case TableEvaluationRun, TableEvaluationVerdict, TableFinding:
		return true
	default:
		return false
	}
}

// Op is the closed enum of DML operations a WAL frame can
// record. v1 only writes INSERTs -- the audit tables are
// append-only at the schema level (tech-spec Sec 7.10 +
// migration 0003's REVOKE UPDATE,DELETE grant) so [OpInsert]
// is the only valid value the writer ever emits.
type Op string

// Canonical [Op] values.
const (
	OpInsert Op = "insert"
)

// IsValid reports whether o is a member of the closed op set.
func (o Op) IsValid() bool { return o == OpInsert }

// AuditFrame is the on-disk shape of a WAL record. One frame
// per inserted row; multiple frames per audit transaction
// (one for `evaluation_run`, one for `evaluation_verdict`, one
// per `finding`).
//
// Field shape mirrors the Stage 9.1 brief verbatim
// (`AuditFrame{frame_id, table, op, row_pk, row_json,
// written_at, signing_key_id, signature}`).
//
//   - FrameID -- a fresh UUIDv4 minted by [Writer.NewFrame].
//     The WAL-side identity; the reconciler dedupes a SQL row
//     by `(table, row_pk)` so [FrameID] is informational for
//     replay tooling but never the dedupe key.
//   - Table -- the bare audit table name (see [Table]).
//   - Op -- always [OpInsert] in v1.
//   - RowPK -- the primary-key UUID of the inserted row.
//     For `evaluation_run` this is `evaluation_run_id`; for
//     `evaluation_verdict` it is `verdict_id`; for `finding`
//     it is `finding_id`.
//   - RowJSON -- the full row body the reconciler will replay.
//     JSON survives column additions without a WAL-format break.
//   - WrittenAt -- the wall-clock timestamp the frame was
//     minted. Used to pick the partition file
//     (`YYYY-MM-DD.wal`, UTC). Also signed.
//   - SigningKeyID -- the `policy_signing_keys.key_id` the
//     signer chose at sign time. The reconciler resolves the
//     row's public key via this id.
//   - Signature -- signature over [AuditFrame.SigningPayload].
type AuditFrame struct {
	FrameID      uuid.UUID `json:"frame_id"`
	Table        Table     `json:"table"`
	Op           Op        `json:"op"`
	RowPK        uuid.UUID `json:"row_pk"`
	RowJSON      []byte    `json:"row_json"`
	WrittenAt    time.Time `json:"written_at"`
	SigningKeyID uuid.UUID `json:"signing_key_id"`
	Signature    []byte    `json:"signature"`
}

// Validate sanity-checks a frame's shape. Called by
// [TxBatch.Stage] before staging and by the reconciler before
// replay.
//
// Validation enforces:
//
//   - Non-zero [AuditFrame.FrameID] -- the WAL-side identity.
//   - [AuditFrame.Table] is a member of the closed set.
//   - [AuditFrame.Op] is a member of the closed set.
//   - Non-zero [AuditFrame.RowPK] -- the audit tables all use
//     non-null UUID PKs.
//   - Non-empty, well-formed JSON in [AuditFrame.RowJSON].
//   - Non-zero [AuditFrame.WrittenAt] -- the partition key.
//   - Non-empty [AuditFrame.Signature] -- the writer always
//     signs before staging.
//
// SigningKeyID is allowed to be the zero UUID for the noop
// signer (test-only); production signers MUST populate it.
func (f AuditFrame) Validate() error {
	if f.FrameID == uuid.Nil {
		return errors.New("wal: frame.FrameID is the zero uuid")
	}
	if !f.Table.IsValid() {
		return fmt.Errorf("wal: frame.Table=%q is not a canonical audit table", f.Table)
	}
	if !f.Op.IsValid() {
		return fmt.Errorf("wal: frame.Op=%q is not a canonical op", f.Op)
	}
	if f.RowPK == uuid.Nil {
		return errors.New("wal: frame.RowPK is the zero uuid")
	}
	if len(f.RowJSON) == 0 {
		return errors.New("wal: frame.RowJSON is empty")
	}
	if !json.Valid(f.RowJSON) {
		return errors.New("wal: frame.RowJSON is not well-formed JSON")
	}
	if f.WrittenAt.IsZero() {
		return errors.New("wal: frame.WrittenAt is the zero time")
	}
	if len(f.Signature) == 0 {
		return errors.New("wal: frame.Signature is empty")
	}
	return nil
}

// SigningPayload returns the canonical byte string that the
// signer signs. Includes every field EXCEPT
// [AuditFrame.Signature], with a domain-separator prefix so a
// frame signature can never collide with a policy publication
// signature.
//
// Format:
//
//	"audit-wal-v1\n" + json.Marshal(unsignedFrame)
//
// where `unsignedFrame` is a struct literal with field order
// identical to [AuditFrame] minus the signature. Encoding/json
// emits fields in struct-declaration order, which gives a
// stable canonical byte string without bringing in a JSON
// canonicalization library.
func (f AuditFrame) SigningPayload() ([]byte, error) {
	type unsigned struct {
		FrameID      uuid.UUID `json:"frame_id"`
		Table        Table     `json:"table"`
		Op           Op        `json:"op"`
		RowPK        uuid.UUID `json:"row_pk"`
		RowJSON      []byte    `json:"row_json"`
		WrittenAt    time.Time `json:"written_at"`
		SigningKeyID uuid.UUID `json:"signing_key_id"`
	}
	body, err := json.Marshal(unsigned{
		FrameID:      f.FrameID,
		Table:        f.Table,
		Op:           f.Op,
		RowPK:        f.RowPK,
		RowJSON:      f.RowJSON,
		WrittenAt:    f.WrittenAt.UTC(),
		SigningKeyID: f.SigningKeyID,
	})
	if err != nil {
		return nil, fmt.Errorf("wal: marshal signing payload: %w", err)
	}
	out := make([]byte, 0, len(signingDomainPrefix)+len(body))
	out = append(out, signingDomainPrefix...)
	out = append(out, body...)
	return out, nil
}

// signingDomainPrefix is the domain separator prepended to the
// signing payload. Hard-coded -- a change here is a wire-format
// break and requires a v2 frame format.
const signingDomainPrefix = "audit-wal-v1\n"

// Sentinel errors. Each is wrapped by the writer so callers
// can branch on a single sentinel via [errors.Is].
var (
	// ErrSignerUnwired is returned by [NewWriter] when the
	// supplied config omits the [Signer]. Production wiring
	// REQUIRES a signer; the noop signer is for tests only.
	ErrSignerUnwired = errors.New("wal: writer requires a Signer")

	// ErrDirUnwired is returned by [NewWriter] when the
	// supplied config omits the partition directory.
	ErrDirUnwired = errors.New("wal: writer requires a partition directory")

	// ErrBatchClosed is returned by [TxBatch.Stage] /
	// [TxBatch.Commit] / [TxBatch.StageNew] after the batch
	// has already been committed OR cancelled. A batch is
	// single-use; the caller MUST allocate a fresh batch
	// per transaction.
	ErrBatchClosed = errors.New("wal: TxBatch already finalised")

	// ErrFrameValidate is the parent sentinel wrapping the
	// per-field complaints emitted by [AuditFrame.Validate].
	// Callers branch via `errors.Is(err, ErrFrameValidate)`.
	ErrFrameValidate = errors.New("wal: frame failed validation")
)
