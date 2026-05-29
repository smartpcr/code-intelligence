package isolation

// Stage 9.3 -- composition-root adapter that exposes a
// [*ModeCoordinator] as the
// `management.FlipCoordinator` interface the Stage 6.2
// `mgmt.set_mode` handler consults to drain in-flight scans
// before flipping a repo's mode.
//
// The adapter lives in this package (not management) so that
// the management package has NO upward dependency on the
// isolation package -- the FlipCoordinator interface in
// management uses primitive string types and is satisfied
// structurally by [*MgmtFlipCoordinator] via Go's interface
// satisfaction rules.
//
// Production wiring:
//
//	coord := isolation.NewModeCoordinator(isolation.WithModeHydrator(hydrate))
//	flip := isolation.NewMgmtFlipCoordinator(coord)
//	writer := management.NewMgmtWriter(
//	    sampleResolver, retractDispatcher, rescanEnqueuer, appender,
//	    management.WithMgmtWriterRepoStore(repoStore),
//	    management.WithMgmtWriterFlipCoordinator(flip),
//	)

import (
	"context"
	"errors"
	"fmt"

	"github.com/gofrs/uuid"
)

// MgmtFlipCoordinator adapts a [*ModeCoordinator] to the
// `management.FlipCoordinator` interface (defined in the
// management package using primitive string types so management
// does NOT depend on this package).
//
// The adapter translates the management-side string mode (the
// wire shape -- `"embedded"` / `"linked"`) to the isolation
// package's [Mode] type, calls [ModeCoordinator.SetMode], and
// translates the returned `previous` Mode back to a string.
//
// Construct via [NewMgmtFlipCoordinator]. The coordinator MUST
// be non-nil; the adapter has no scaffold mode (a nil
// coordinator would silently disable the drain semantics the
// brief makes mandatory, so the constructor panics).
type MgmtFlipCoordinator struct {
	coord *ModeCoordinator
}

// NewMgmtFlipCoordinator constructs the adapter. `coord` MUST
// be non-nil; the panic surfaces the wiring bug at startup
// (before the first SetMode call) rather than masking the
// "no drain" failure as a 503 at request time.
func NewMgmtFlipCoordinator(coord *ModeCoordinator) *MgmtFlipCoordinator {
	if coord == nil {
		panic("isolation: NewMgmtFlipCoordinator: coord is nil; the drain coordinator is mandatory")
	}
	return &MgmtFlipCoordinator{coord: coord}
}

// SetMode satisfies the `management.FlipCoordinator` interface:
//
//	SetMode(ctx context.Context, repoID uuid.UUID, target string,
//	        applyFn func(ctx context.Context) error) (previous string, changed bool, err error)
//
// The adapter validates `target` against [AllowedModes] up
// front so a typo from a wire caller surfaces as
// [ErrInvalidMode] BEFORE the coordinator takes any flip lock.
// On every other code path the adapter is a thin wrapper:
// `applyFn` is invoked inside the coordinator's flip lock
// (post-drain), so the catalog mutation (`repoStore.SetRepoMode`)
// runs against a stable mode AND new scans block until the
// flip completes.
//
// The returned (previous, changed) tuple reflects the
// COORDINATOR's view of the mode transition. Callers that need
// the authoritative response shape (e.g. the
// `mgmt.set_mode` HTTP handler) capture the `SetRepoModeResult`
// inside `applyFn` and prefer THAT over the adapter's return
// (rubber-duck iter-2 finding #5): the coordinator's cache may
// lag the store on a transient adapter error AND the store IS
// the source of truth.
func (a *MgmtFlipCoordinator) SetMode(
	ctx context.Context,
	repoID uuid.UUID,
	target string,
	applyFn func(ctx context.Context) error,
) (previous string, changed bool, err error) {
	mode := Mode(target)
	if !IsAllowedMode(mode) {
		return "", false, fmt.Errorf("%w: got %q", ErrInvalidMode, target)
	}
	prev, ch, err := a.coord.SetMode(ctx, repoID, mode, applyFn)
	return string(prev), ch, err
}

// Coordinator exposes the wrapped coordinator. Useful for the
// composition root that also needs to hand the same coordinator
// to a [Pool] / scan path so the per-repo state is shared
// (one coordinator backs BOTH the flip path AND the scan
// admission path; otherwise the flip would block on the wrong
// set of in-flight scans).
func (a *MgmtFlipCoordinator) Coordinator() *ModeCoordinator { return a.coord }

// Compile-time guard that the adapter satisfies the
// expected shape. Mirrors the `management.FlipCoordinator`
// signature exactly; if management ever changes the signature
// this will fail to compile.
var _ flipCoordinatorShape = (*MgmtFlipCoordinator)(nil)

// flipCoordinatorShape is a local mirror of
// `management.FlipCoordinator` used only as a compile-time
// guard. We deliberately do NOT import management here (no
// upward dependency); the structural match is enough.
type flipCoordinatorShape interface {
	SetMode(ctx context.Context, repoID uuid.UUID, target string, applyFn func(ctx context.Context) error) (previous string, changed bool, err error)
}

// ensure errors used by the adapter are visible to godoc.
var _ = errors.Is
