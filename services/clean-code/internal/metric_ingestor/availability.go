package metric_ingestor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gofrs/uuid"
)

// AstSourceAvailability is the optional pre-flight probe
// the state machine consults BEFORE issuing
// [ScanRunStore.ClaimNextPendingCommit] (iter-4 evaluator
// item 2). When wired via [WithStateMachineSourceProbe],
// the state machine peeks the next pending commit, asks
// the probe whether its underlying source can deliver
// AST files for that (RepoID, SHA) pair, and SKIPS the
// claim when the probe returns `false`.
//
// # Why a separate interface from [AstFileSource]
//
// [AstFileSource.Files] is invoked AFTER the claim, when
// the commit is already in the `scanning` state -- by
// then the only available terminal transitions are
// `scanning -> scanned` and `scanning -> failed`. A
// pre-flight that runs BEFORE the claim allows the
// state machine to leave the commit `pending` (no
// canonical transition occurs at all) so the next sweep
// tick retries naturally, preserving:
//
//  1. The four-state Commit diagram (the commit never
//     crosses an unsupported edge -- it never enters
//     `scanning` until its source is ready).
//  2. The sole-writer contract on `commit.scan_status`
//     (no operator hand-edit needed to recover from a
//     not-yet-materialised checkout).
//  3. [repo_indexer.ValidateTransition]: the rejected
//     edges `scanning->pending` and `failed->pending`
//     stay rejected at the validator AND at runtime.
//
// # TOCTOU race policy
//
// The probe is BEST-EFFORT: a `true` return is not a
// guarantee that the source will still be ready by the
// time [AstFileSource.Files] runs. When such a race
// fires (e.g. the checkout-resolver garbage-collected
// the SHA between probe and parse), the state machine
// transitions the commit to `failed` as usual --
// [ErrCommitRootNotMaterialised] surfaces inside the
// scan and the commit gets the canonical `failed`
// terminal state. The probe just keeps that outcome
// rare in steady-state operation.
type AstSourceAvailability interface {
	// HasFilesFor returns (true, nil) when the source
	// can deliver AST files for `commit`, (false, nil)
	// when the source is not yet ready (caller skips
	// the claim), and (false, err) on infrastructure
	// failure (caller propagates the error).
	HasFilesFor(ctx context.Context, commit PendingCommit) (bool, error)
}

// AlwaysAvailable is the no-op [AstSourceAvailability]
// implementation: it advertises every commit as
// available. Used by tests + by scaffold-mode wiring
// where no real source is configured; with this probe
// the state machine behaves exactly as if no probe were
// wired at all.
type AlwaysAvailable struct{}

// HasFilesFor returns (true, nil) for every input --
// the canonical no-op probe used when the state machine
// should not perform any pre-flight short-circuit.
func (AlwaysAvailable) HasFilesFor(context.Context, PendingCommit) (bool, error) {
	return true, nil
}

// HasFilesFor implements [AstSourceAvailability] on the
// [DirectoryAstFileSource] by checking the canonical
// `<Root>/<repo_id>/<sha>` path with [os.Stat]: when the
// directory is present + is a directory, the source can
// deliver (true, nil); when [os.IsNotExist] fires the
// source advertises "not yet ready" (false, nil); on any
// other stat failure the error propagates so the state
// machine can surface the infrastructure problem.
//
// iter-4 evaluator item 2 (recovery via structural
// pre-flight): wiring `directorySource` as both the
// [AstFileSource] AND the [AstSourceAvailability] keeps
// the source-of-truth single -- the same path-existence
// check that [DirectoryAstFileSource.Files] uses
// post-claim gates the pre-claim peek.
func (s *DirectoryAstFileSource) HasFilesFor(ctx context.Context, commit PendingCommit) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if s == nil {
		return false, fmt.Errorf("metric_ingestor: DirectoryAstFileSource.HasFilesFor called on nil receiver")
	}
	if strings.TrimSpace(s.Root) == "" {
		return false, ErrDirectoryAstSourceMissingRoot
	}
	if commit.RepoID == uuid.Nil {
		return false, fmt.Errorf("metric_ingestor: DirectoryAstFileSource.HasFilesFor: zero RepoID")
	}
	if strings.TrimSpace(commit.SHA) == "" {
		return false, fmt.Errorf("metric_ingestor: DirectoryAstFileSource.HasFilesFor: empty SHA")
	}
	commitRoot := filepath.Join(s.Root, commit.RepoID.String(), commit.SHA)
	info, err := os.Stat(commitRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("metric_ingestor: DirectoryAstFileSource.HasFilesFor.Stat(%q): %w", commitRoot, err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("metric_ingestor: DirectoryAstFileSource.HasFilesFor: %q is not a directory", commitRoot)
	}
	return true, nil
}
