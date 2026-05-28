package composition

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/audit/reconciler"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/keys"
)

// WALReconcilerConfig configures the composition-root factory
// [NewWALReconciler]. Every required field is validated by
// the factory; the optional `Logger` defaults to a no-op.
type WALReconcilerConfig struct {
	// DB is the `*sql.DB` handle the reconciler uses for
	// every Audit-table INSERT. PRODUCTION COMPOSITION
	// REQUIRES this handle to be authenticated as
	// `clean_code_wal_reconciler` (migration 0004 grants
	// INSERT+SELECT on the three Audit tables ONLY; UPDATE
	// / DELETE are REVOKED). The role posture is the
	// outer guard for the brief's invariants 2 and 3
	// ("never deletes a row; never modifies a non-Audit
	// table").
	DB *sql.DB

	// Dir is the Audit WAL partition root the reconciler
	// reads from. Production composition threads
	// `CLEAN_CODE_AUDIT_WAL_DIR` (default `data/wal/audit`)
	// from the binary's env config -- the same env knob the
	// `wal.Writer` uses, so a misconfigured operator cannot
	// point the reconciler at a different directory than
	// the writer that produced the frames.
	Dir string

	// Keys is the `policy/keys.Manager` instance the
	// adapter consults for signature verification. May be
	// nil in scaffold-mode wiring; in that case
	// [NewWALReconciler] returns `(nil, nil)` so the binary
	// can branch on "reconciler disabled" without
	// classifying the missing key store as an error.
	Keys *keys.Manager

	// Schema is the PostgreSQL schema name. Empty -> the
	// reconciler defaults to `clean_code`.
	Schema string

	// Logger is the structured-log hook the reconciler
	// emits to. Optional.
	Logger func(msg string, kv ...any)
}

// NewWALReconciler is the composition-root factory for the
// Stage 9.2 [reconciler.Reconciler]. Wires the production
// [reconciler.SQLReplayer] (authenticated as
// `clean_code_wal_reconciler` by the caller) and the
// [keys.Manager]-backed [reconciler.Verifier] adapter.
//
// Returns:
//
//   - `(reconciler, nil)` on success.
//   - `(nil, nil)` when `cfg.Keys` is nil -- the binary
//     branches on "reconciler disabled" deliberately.
//   - `(nil, err)` when a required field is missing or
//     `NewSQLReplayer` rejects the supplied DB.
//
// FOLLOW-UP REQUIRED before this factory is wired into
// `cmd/clean-code-eval-gate/main.go` or
// `cmd/clean-code-gateway/main.go`:
//
//   - The Verifier adapter uses `keys.Manager.Verify`,
//     which rejects KEYS OUTSIDE THEIR ACTIVE WINDOW
//     with `ErrUnknownKey`. A frame signed yesterday by
//     a now-retired key cannot verify against the live
//     manager. To preserve the brief's durability
//     guarantee, the Stage 9.2 adapter classifies
//     `keys.ErrUnknownKey` as a NON-sentinel error so the
//     reconciler ABORTS Run rather than silently
//     classifying the frame as `SkippedBadSig` and
//     dropping it. Operators MUST NOT enable this factory
//     in production until the historical-keys adapter
//     (Stage 9.3) lands -- they will get loud, immediate
//     aborts on the first retired-key frame instead of
//     silent data loss. The historical-keys variant
//     consults the full `clean_code.policy_signing_keys`
//     table (including retired rows) and resolves
//     historical keys safely; until then the live-only
//     adapter is intentionally fail-loud.
//   - Binary-level on-restart wiring is also a follow-up:
//     `cmd/clean-code-eval-gate/main.go` currently opens
//     two DB pools (default + solid-batch); the reconciler
//     needs a third pool authenticated as
//     `clean_code_wal_reconciler` AND a new env var
//     `CLEAN_CODE_WAL_RECONCILER_DSN`. Until that landing,
//     the binary continues to start without a reconciler;
//     operators run the WAL replay manually via a one-shot
//     wrapper documented in
//     `services/clean-code/docs/runbook.md` Stage 9.2.
func NewWALReconciler(cfg WALReconcilerConfig) (*reconciler.Reconciler, error) {
	if cfg.Keys == nil {
		return nil, nil
	}
	if cfg.DB == nil {
		return nil, errors.New("composition: NewWALReconciler: DB is nil (production wiring MUST authenticate as clean_code_wal_reconciler per migration 0004)")
	}
	if cfg.Dir == "" {
		return nil, errors.New("composition: NewWALReconciler: Dir is required (set CLEAN_CODE_AUDIT_WAL_DIR)")
	}
	replayer, err := reconciler.NewSQLReplayer(reconciler.SQLReplayerConfig{
		DB:     cfg.DB,
		Schema: cfg.Schema,
	})
	if err != nil {
		return nil, fmt.Errorf("composition: NewWALReconciler: %w", err)
	}
	verifier := NewKeysManagerWALVerifier(cfg.Keys)
	return reconciler.NewReconciler(reconciler.Config{
		Dir:      cfg.Dir,
		Verifier: verifier,
		Replayer: replayer,
		Logger:   cfg.Logger,
	})
}

// NewKeysManagerWALVerifier adapts a `policy/keys.Manager`
// to the [reconciler.Verifier] interface so the
// composition root can wire the Stage 9.2 reconciler's
// signature-verification path against the production KMS.
//
// Returns `nil` when `m` is nil so the caller can branch
// deliberately on a scaffold-mode `Manager` (NewReconciler
// would refuse a nil Verifier).
//
// KNOWN LIMITATION (Stage 9.3 follow-up): the adapter calls
// `keys.Manager.Verify`, which rejects RETIRED keys with
// `ErrUnknownKey`. A frame signed yesterday by a key that
// rotated out of the active window today CANNOT verify
// against the live Manager. To preserve durability the
// Stage 9.2 adapter ABORTS Run on this condition (it does
// NOT classify the frame as a per-frame `SkippedBadSig`)
// so the operator gets a loud failure instead of silent
// data loss. The historical-keys adapter -- which consults
// the full `clean_code.policy_signing_keys` table including
// the `retired_at IS NOT NULL` rows -- lands in Stage 9.3.
// Until then, production wiring SHOULD NOT enable the
// reconciler unless the operator has confirmed no
// retired-key frames are pending replay.
func NewKeysManagerWALVerifier(m *keys.Manager) reconciler.Verifier {
	if m == nil {
		return nil
	}
	return keysManagerWALVerifier{m: m}
}

// keysManagerWALVerifier is the [reconciler.Verifier]
// implementation backing [NewKeysManagerWALVerifier]. The
// struct holds the manager by pointer so a manager refresh
// (cache reload, rotation) is observed by every in-flight
// verification WITHOUT having to re-bind the verifier.
type keysManagerWALVerifier struct {
	m *keys.Manager
}

// Verify implements [reconciler.Verifier.Verify] by
// delegating to [keys.Manager.Verify] and mapping the
// manager's sentinels to the reconciler's classification
// sentinels:
//
//   - `keys.ErrUnknownKey` (unknown OR retired key) ->
//     wraps the raw error WITHOUT mapping to a reconciler
//     skip-sentinel -> the reconciler ABORTS Run. The live
//     manager cannot distinguish "truly unknown" from
//     "retired but legitimate", so a sentinel-skip
//     classification would silently drop legitimate
//     historical frames signed by a now-retired key. The
//     Stage 9.3 historical-keys adapter (consulting the
//     full `policy_signing_keys` table including retired
//     rows) is the right place to classify "truly unknown"
//     as `reconciler.ErrSigningKeyUnknown`.
//   - `keys.ErrSignatureMismatch` (bad signature) ->
//     wraps `reconciler.ErrSignatureInvalid` ->
//     skip-and-count classification (the manager DID
//     resolve the key, so we know the signature itself is
//     tampered; per-frame skip is the right outcome).
//   - Anything else (transient infra, KMS outage, ctx
//     cancellation) is propagated verbatim -> reconciler
//     aborts Run so an operator can address the root
//     cause before retrying.
func (v keysManagerWALVerifier) Verify(ctx context.Context, keyID uuid.UUID, payload, signature []byte) error {
	err := v.m.Verify(ctx, keyID, payload, signature)
	if err == nil {
		return nil
	}
	if errors.Is(err, keys.ErrUnknownKey) {
		// FAIL-LOUD: live keys.Manager cannot resolve
		// retired-window keys. Returning a NON-sentinel
		// error forces reconciler.Run to abort instead
		// of silently classifying the frame as
		// SkippedBadSig. See doc on NewKeysManagerWALVerifier
		// for the Stage 9.3 follow-up.
		return fmt.Errorf("composition: keysManagerWALVerifier: live keys.Manager cannot resolve key id=%s (unknown or retired); refusing to skip until Stage 9.3 historical-keys adapter lands: %w", keyID, err)
	}
	if errors.Is(err, keys.ErrSignatureMismatch) {
		return fmt.Errorf("%w: %v", reconciler.ErrSignatureInvalid, err)
	}
	// Transient or unclassified: propagate so the
	// reconciler treats it as "abort Run".
	return err
}
