package composition

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/audit/reconciler"
	"forge/services/clean-code/internal/policy/keys"
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

	// KeyStore is the historical-keys reader the verifier
	// snapshots at construction time. PRODUCTION wiring
	// builds a `keys.SQLStore` on the same DB pool used for
	// the SQLReplayer (the `clean_code_wal_reconciler` role
	// has SELECT on `clean_code.policy_signing_keys` per
	// migration 0005). Test wiring uses
	// [keys.NewInMemoryStore].
	//
	// Either `KeyStore` OR `Keys` MUST be set; if BOTH are
	// nil the factory returns `(nil, nil)` so the binary
	// can branch on "reconciler disabled" without
	// classifying the missing dependency as an error.
	// `KeyStore` takes precedence when both are supplied --
	// it is the explicit production path.
	KeyStore keys.Store

	// Keys is a convenience for callers that have already
	// built a `*keys.Manager` (e.g. the eval-gate binary
	// after `keys.Build`). When `KeyStore` is nil and
	// `Keys` is non-nil, the factory captures the manager's
	// loaded cache via [keys.Manager.HistoricalKeys] (a
	// rotation-stable snapshot copy) and uses that as the
	// historical-key source. The publish-time
	// `keys.Manager.Verify` active-window check is
	// INTENTIONALLY BYPASSED so a frame signed by a
	// now-retired key still verifies on replay -- the
	// Stage 9.2 contract per
	// `internal/audit/reconciler/types.go`'s [Verifier]
	// interface doc.
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
// historical-keys [reconciler.Verifier] adapter.
//
// The historical-keys adapter consults the FULL
// `clean_code.policy_signing_keys` table (including rows
// outside the live active window) and verifies signatures
// via direct [ed25519.Verify] against the persisted public
// key. This is the Stage 9.2 production contract per
// `internal/audit/reconciler/types.go`'s [reconciler.Verifier]
// interface doc: "a frame signed yesterday by a now-retired
// key must still verify on replay" -- the publish-time
// [keys.Manager.Verify] rejects retired keys on purpose, so
// the reconciler MUST bind a separate adapter that does NOT
// re-impose the active-window check.
//
// `ctx` is used only for the one-time snapshot fetch from
// `cfg.KeyStore` (when set); it is NOT retained past
// construction. Pass the binary's startup context so the
// snapshot fetch participates in shutdown semantics.
//
// Returns:
//
//   - `(reconciler, nil)` on success.
//   - `(nil, nil)` when BOTH `cfg.KeyStore` and `cfg.Keys`
//     are nil -- the binary branches on "reconciler
//     disabled" deliberately.
//   - `(nil, err)` when a required field is missing, the
//     snapshot fetch fails, or `NewSQLReplayer` rejects the
//     supplied DB.
//
// The production binaries (`cmd/clean-code-eval-gate` and
// `cmd/clean-code-gateway`) wire this factory ahead of
// `http.ListenAndServe` so the WAL replay runs BEFORE any
// traffic is accepted -- the brief's "on service restart"
// requirement is literally a blocking startup step. See
// `services/clean-code/docs/runbook.md` "Stage 9.2" for the
// operational expectations (env vars, role grants, stats
// interpretation).
func NewWALReconciler(ctx context.Context, cfg WALReconcilerConfig) (*reconciler.Reconciler, error) {
	if cfg.KeyStore == nil && cfg.Keys == nil {
		return nil, nil
	}
	if cfg.DB == nil {
		return nil, errors.New("composition: NewWALReconciler: DB is nil (production wiring MUST authenticate as clean_code_wal_reconciler per migration 0004)")
	}
	if cfg.Dir == "" {
		return nil, errors.New("composition: NewWALReconciler: Dir is required (set CLEAN_CODE_AUDIT_WAL_DIR)")
	}

	var verifier reconciler.Verifier
	switch {
	case cfg.KeyStore != nil:
		v, err := NewHistoricalKeysWALVerifier(ctx, cfg.KeyStore)
		if err != nil {
			return nil, fmt.Errorf("composition: NewWALReconciler: %w", err)
		}
		verifier = v
	case cfg.Keys != nil:
		verifier = NewKeysManagerWALVerifier(cfg.Keys)
	}
	if verifier == nil {
		return nil, errors.New("composition: NewWALReconciler: historical-keys verifier construction returned nil (KeyStore.List returned nothing or Keys cache is empty -- call Manager.Load before constructing the reconciler)")
	}

	replayer, err := reconciler.NewSQLReplayer(reconciler.SQLReplayerConfig{
		DB:     cfg.DB,
		Schema: cfg.Schema,
	})
	if err != nil {
		return nil, fmt.Errorf("composition: NewWALReconciler: %w", err)
	}
	return reconciler.NewReconciler(reconciler.Config{
		Dir:      cfg.Dir,
		Verifier: verifier,
		Replayer: replayer,
		Logger:   cfg.Logger,
	})
}

// NewHistoricalKeysWALVerifier builds a [reconciler.Verifier]
// that snapshots every row in `store` (active + retired) ONCE
// and answers every frame's signature check from the
// in-memory snapshot via direct [ed25519.Verify]. The
// active-window check that the publish-time
// [keys.Manager.Verify] applies is INTENTIONALLY BYPASSED --
// the Stage 9.2 reconciler contract requires that a frame
// signed yesterday by a now-retired key still verifies on
// replay. See `internal/audit/reconciler/types.go`'s
// [reconciler.Verifier] interface doc.
//
// The snapshot strategy is deliberate:
//
//   - One-time `store.List(ctx)` at construction. The
//     rubber-duck pass on Stage 9.2 iter 2 flagged a naive
//     "List per Verify" implementation as `frames × keys`
//     DB queries; the reconciler runs ONCE per restart over
//     a potentially large WAL backlog so the per-frame
//     cost dominates startup latency.
//
//   - `[]byte` copy of every `KeyRecord.PublicKey` into the
//     map value so a future Store mutation cannot race the
//     verifier. The reconciler is the sole reader so this
//     is defence-in-depth.
//
//   - Records whose `PublicKey` length is not
//     [ed25519.PublicKeySize] (32) are SILENTLY SKIPPED.
//     The migration's CHECK constraint
//     (`octet_length(public_key) = 32`) and
//     [validateRecord] enforce the invariant at the store
//     layer; a 32-byte mismatch reaching this code path
//     would already be a Store bug, and the verifier's
//     fall-through (key not in snapshot ->
//     [reconciler.ErrSigningKeyUnknown] -> per-frame skip)
//     is the safest outcome.
//
// Returns `(nil, nil)` when `store` is nil so the
// composition factory can branch on "scaffold-mode".
//
// Sentinel mapping (per [reconciler.Verifier] interface):
//
//   - key_id absent from snapshot ->
//     [reconciler.ErrSigningKeyUnknown] (per-frame skip).
//     This covers TWO disjoint cases: (a) the frame's
//     signing_key_id was never registered in
//     `clean_code.policy_signing_keys` (e.g. an attacker's
//     forged frame produced with their own keypair -- the
//     key is cryptographically valid but is not anchored in
//     the service's trusted key history); (b) the frame's
//     signing_key_id WAS registered but the snapshot is
//     stale (a key inserted after the snapshot was taken).
//     The reconciler runs once per restart, so case (b) is
//     only possible in a second concurrent run; in both
//     cases skip-and-count is the correct outcome.
//
//   - [ed25519.Verify] returns false (key found, signature
//     bytes do not match) -> [reconciler.ErrSignatureInvalid]
//     (per-frame skip).
//
//   - `store.List(ctx)` failure -> raw error wrapped with
//     a composition prefix. The reconciler treats this as
//     transient infra and aborts Run so the operator can
//     address the root cause before retrying.
func NewHistoricalKeysWALVerifier(ctx context.Context, store keys.Store) (reconciler.Verifier, error) {
	if store == nil {
		return nil, nil
	}
	recs, err := store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("composition: NewHistoricalKeysWALVerifier: store.List: %w", err)
	}
	return &historicalKeysWALVerifier{snap: buildHistoricalKeySnapshot(recs)}, nil
}

// NewKeysManagerWALVerifier is a convenience wrapper for
// callers that have already constructed a [keys.Manager]
// (the eval-gate / gateway binaries after `keys.Build`). It
// captures the manager's loaded cache via
// [keys.Manager.HistoricalKeys] -- a rotation-stable
// snapshot copy -- and returns a [reconciler.Verifier] that
// behaves IDENTICALLY to [NewHistoricalKeysWALVerifier]: the
// publish-time active-window check is BYPASSED; signatures
// are validated by direct [ed25519.Verify] against the
// persisted public keys.
//
// Returns `nil` when `m` is nil so the caller can branch
// deliberately on scaffold-mode (NewReconciler would refuse
// a nil Verifier).
//
// The caller MUST have invoked [keys.Manager.Load] BEFORE
// calling this function -- otherwise the snapshot is empty
// and every Verify call returns [reconciler.ErrSigningKeyUnknown].
// Both production binaries call `keys.Build` (which calls
// `Load`) before reaching the WAL reconciler wiring, so this
// precondition is satisfied in practice.
func NewKeysManagerWALVerifier(m *keys.Manager) reconciler.Verifier {
	if m == nil {
		return nil
	}
	return &historicalKeysWALVerifier{snap: buildHistoricalKeySnapshot(m.HistoricalKeys())}
}

// buildHistoricalKeySnapshot copies every [keys.KeyRecord]'s
// `KeyID` and `PublicKey` into a map keyed by `KeyID`. The
// returned map is suitable for read-only concurrent use; the
// reconciler is a single-goroutine consumer, so concurrent
// reads are not exercised in practice. Records with the wrong
// public-key length are skipped (see [NewHistoricalKeysWALVerifier]
// doc for the rationale).
func buildHistoricalKeySnapshot(recs []keys.KeyRecord) map[uuid.UUID]ed25519.PublicKey {
	snap := make(map[uuid.UUID]ed25519.PublicKey, len(recs))
	for _, rec := range recs {
		if len(rec.PublicKey) != ed25519.PublicKeySize {
			continue
		}
		pub := make([]byte, ed25519.PublicKeySize)
		copy(pub, rec.PublicKey)
		snap[rec.KeyID] = ed25519.PublicKey(pub)
	}
	return snap
}

// historicalKeysWALVerifier is the shared [reconciler.Verifier]
// implementation backing both [NewHistoricalKeysWALVerifier]
// and [NewKeysManagerWALVerifier]. Holds the rotation-stable
// snapshot map -- no DB / KMS access on the hot path.
type historicalKeysWALVerifier struct {
	snap map[uuid.UUID]ed25519.PublicKey
}

// Verify implements [reconciler.Verifier.Verify] by direct
// [ed25519.Verify] against the snapshotted public key for
// `keyID`. Honors `ctx` cancellation as a pre-check so a
// shutdown during a long replay short-circuits cleanly.
func (v *historicalKeysWALVerifier) Verify(ctx context.Context, keyID uuid.UUID, payload, signature []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	pub, ok := v.snap[keyID]
	if !ok {
		return fmt.Errorf("composition: historicalKeysWALVerifier: key_id=%s is not in the historical signing-key snapshot (no row in clean_code.policy_signing_keys with this id): %w", keyID, reconciler.ErrSigningKeyUnknown)
	}
	if len(signature) != ed25519.SignatureSize {
		return fmt.Errorf("composition: historicalKeysWALVerifier: signature length=%d for key_id=%s is not %d: %w", len(signature), keyID, ed25519.SignatureSize, reconciler.ErrSignatureInvalid)
	}
	if !ed25519.Verify(pub, payload, signature) {
		return fmt.Errorf("composition: historicalKeysWALVerifier: ed25519.Verify=false for key_id=%s: %w", keyID, reconciler.ErrSignatureInvalid)
	}
	return nil
}
