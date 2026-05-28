package composition

import (
	"context"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/audit/wal"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/keys"
)

// NewKeysManagerWALSigner adapts a `policy/keys.Manager` to
// the [wal.Signer] interface so the composition root can wire
// the production Audit-WAL writer with a real Ed25519 signing
// path rooted in the KMS (architecture Sec 7.1 / Sec 7.10).
//
// Why this lives in `composition` (and NOT in `audit/wal`):
//
//   - `audit/wal` is scope-gated by `test/conformance/wal_scope_test.go`
//     to a small allow-list of importers; the wal package itself
//     deliberately does NOT import `policy/keys` so the
//     audit-WAL frame format stays decoupled from the KMS
//     adapter (a future operator could swap in an HSM-rooted
//     Signer by adding a new adapter in the same composition
//     surface).
//   - `composition` is the canonical wiring point where both
//     `keys.Manager` and `wal.Writer` already exist, so the
//     adapter belongs alongside `BuildEvalGate`.
//
// Returns a signer that delegates to
// [keys.Manager.SignActive], which preserves the
// "keyID-into-payload" callback invariant. Returns `nil` when
// `m` is nil so the caller can branch deliberately on a
// scaffold-mode `keys.Manager` -- callers MUST NOT pass the
// result to [wal.WriterConfig] without first verifying
// non-nil; `NewWriter` rejects a nil Signer.
func NewKeysManagerWALSigner(m *keys.Manager) wal.Signer {
	if m == nil {
		return nil
	}
	return keysManagerWALSigner{m: m}
}

// keysManagerWALSigner is the [wal.Signer] implementation
// the composition root binds to a `keys.Manager`. The struct
// holds the manager by pointer so a manager refresh (cache
// reload, rotation) is observed by every in-flight Audit-WAL
// frame WITHOUT having to re-bind the signer.
type keysManagerWALSigner struct {
	m *keys.Manager
}

// SignFrame implements [wal.Signer.SignFrame] by delegating
// to [keys.Manager.SignActive]. The build callback is passed
// through verbatim; SignActive guarantees the keyID handed to
// `build` is the same keyID returned to the WAL writer.
func (s keysManagerWALSigner) SignFrame(ctx context.Context, build func(keyID uuid.UUID) ([]byte, error)) (uuid.UUID, []byte, error) {
	return s.m.SignActive(ctx, build)
}
