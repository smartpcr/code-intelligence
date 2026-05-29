package management

// Stage 9.3 -- `FlipCoordinator` interface seam consumed by
// [MgmtWriter.SetMode] to drain in-flight scans BEFORE the
// catalog mutation runs.
//
// # Brief (implementation-plan Stage 9.3, line 804)
//
//	"On `mgmt.set_mode(repo_id, mode)` transitions between
//	 `embedded` and `linked`, drain in-flight scans for the
//	 repo before flipping; new scans pick up the new mode."
//
// # Why an interface (not a concrete dependency)
//
// The drain primitive (per-repo admission + flip lock + drain
// barrier) lives in
// `services/clean-code/internal/ast/isolation/`. Importing
// that package from `management` would create an upward
// dependency: management would carry the (transitive)
// dependency on `parser.Registry` and its codegen, just so
// the `mgmt.set_mode` handler can call one method.
//
// Defining the interface here keeps management free of any
// isolation-package import. The
// `isolation.NewMgmtFlipCoordinator(coord)` adapter satisfies
// this interface STRUCTURALLY (Go's interface satisfaction
// rules don't require an explicit `implements` keyword).

import (
	"context"

	"github.com/gofrs/uuid"
)

// FlipCoordinator is the drain-before-flip coordinator the
// [MgmtWriter.SetMode] handler delegates to when wired via
// [WithMgmtWriterFlipCoordinator]. The interface intentionally
// uses primitive types (string, uuid.UUID) so concrete
// implementations from other packages (notably the
// `isolation.MgmtFlipCoordinator` adapter) satisfy it
// structurally without forcing management to import them.
//
// # Contract
//
// `SetMode` MUST:
//
//  1. Block until all in-flight scans for `repoID` complete
//     (drain barrier; new scans queued behind the flip flag).
//  2. Run `applyFn` (the catalog mutation:
//     `repoStore.SetRepoMode`) ATOMICALLY with the in-memory
//     mode swap, so the next admitted scan picks up the new
//     mode.
//  3. Return the previous mode (as a string for cross-package
//     interop) and `changed` (false on the same-mode no-op
//     path). On any error before applyFn runs, leave the
//     coordinator's cached mode UNCHANGED and clear any
//     drain barrier so retry is possible.
//
// `target` MUST be in [AllowedRepoModes]; implementations
// SHOULD validate up-front and surface an
// [ErrRepoStoreInvalidMode]-compatible error if not.
//
// The handler treats `applyFn`'s error as authoritative:
// when applyFn returns an error, the catalog mutation did
// NOT happen, the coordinator's cached mode is UNCHANGED,
// and the handler maps the error back to its existing
// error-class table ([writeRepoStoreError]).
//
// # Source-of-truth note
//
// The (previous, changed) tuple returned by SetMode reflects
// the COORDINATOR's view. The handler captures the
// authoritative `SetRepoModeResult` inside `applyFn` and uses
// THAT for the HTTP response body -- the coordinator's cache
// may lag the persisted row on a transient adapter error, but
// the response MUST reflect what the catalog actually wrote
// (rubber-duck iter-2 finding #5).
type FlipCoordinator interface {
	SetMode(
		ctx context.Context,
		repoID uuid.UUID,
		target string,
		applyFn func(ctx context.Context) error,
	) (previous string, changed bool, err error)
}
