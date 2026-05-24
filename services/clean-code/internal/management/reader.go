package management

import (
	"context"
	"errors"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/keys"
)

// ErrManagerUnavailable is returned by [Reader] methods when
// the underlying [keys.Manager] dependency is nil. Catches a
// composition-root wiring bug at the first verb call rather
// than at runtime via a nil-pointer panic.
var ErrManagerUnavailable = errors.New("management: signing key manager not wired")

// Reader is the read-side surface of the clean-code service.
// Stage 5.1 wires it with a [keys.Manager]; later stages add
// further read backends (the PostgreSQL Measurement reader,
// the Audit reader, ...) as additional struct fields.
//
// All Reader methods are safe for concurrent use; concurrency
// guarantees come from the underlying read source (the
// `keys.Manager` cache is RWMutex-guarded).
type Reader struct {
	signingKeys *keys.Manager
}

// NewReader constructs a Reader. signingKeys MAY be nil for
// scaffold-mode bring-ups -- in that case `ListActiveSigningKeys`
// returns [ErrManagerUnavailable] and the HTTP handler
// translates it into a 503.
func NewReader(signingKeys *keys.Manager) *Reader {
	return &Reader{signingKeys: signingKeys}
}

// ListActiveSigningKeys returns the canonical
// `policy.keys.list_active` projection: every signing key that
// is currently inside its `[valid_from, valid_until)` window,
// sorted newest-first.
//
// Empty result is valid (returns nil, nil) -- callers
// interpret it as "no key has been activated yet" which is
// distinct from "the keys subsystem is mis-wired"
// ([ErrManagerUnavailable]).
func (r *Reader) ListActiveSigningKeys(ctx context.Context) ([]keys.ActiveKeyView, error) {
	if r.signingKeys == nil {
		return nil, ErrManagerUnavailable
	}
	return r.signingKeys.ListActive(ctx)
}
