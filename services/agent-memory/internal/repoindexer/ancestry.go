package repoindexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// AncestryWriter owns the load-bearing pre-EmitFile sequence
// described in REPO-SCANNER architecture S3.4: for a single
// (repoURL, sha) scan it drives
//
//  1. EnsureRepo (upsert the `repo` row by URL),
//  2. EnsureCommit (append the `repo_commit` row),
//  3. InsertNode(kind=repo) (root of the structural graph),
//  4. per-file InsertNode(kind=package) + repo→package
//     contains-edge (deduped by canonical-package-dir),
//  5. per-file InsertNode(kind=file) + package→file
//     contains-edge.
//
// The same sequence runs unchanged in
//   - the queue worker's `worker.runFull` (today inlined; a
//     follow-up workstream rewires it through this type so the
//     queue and the CLI cannot drift), and
//   - the new `codeintel scan` CLI (built on top of any
//     graphsink backend in Phase 3).
//
// The canonical-signature helpers (`CanonicalRepoSig`,
// `CanonicalPackageDir`, `CanonicalPackageSig`,
// `CanonicalFileSig` in canonical.go) are the single source of
// truth for node identity, so a SQLite scan and a Postgres
// scan of the same input produce byte-identical
// `(kind, canonical_signature)` tuples (architecture S2 / R5
// backend-parity invariant).
//
// AncestryWriter is single-writer / single-scan. Not safe for
// concurrent use because the per-scan package-dir cache is
// modified in-place by EnsureFile. Construct one per scan and
// discard.
type AncestryWriter struct {
	// w is the narrow writer the sequence drives. Today the
	// only concrete value is *graphwriter.Writer; Phase 3
	// widens this to graphsink.Sink (RepoCommitNodeEdgeWriter
	// is a strict subset, so existing callers keep working).
	w RepoCommitNodeEdgeWriter

	// repoURL is the URL the scan operates on. It is the
	// natural key for the `repo` row, the input to
	// CanonicalRepoSig / CanonicalPackageSig / CanonicalFileSig,
	// and the input to fingerprint.RepoIDFromURL (architecture
	// S3.4 backend-parity ID).
	repoURL string

	// sha is the SHA being scanned. Used as
	//   - CommitInput.SHA,
	//   - NodeInput.FromSHA / EdgeInput.FromSHA, and
	//   - the dedupe key the graphwriter folds into every
	//     fingerprint pre-image.
	sha string

	// now is the clock used for CommitInput.CommittedAt.
	// Tests pin it for reproducible commit timestamps;
	// production wiring leaves it at time.Now.
	now func() time.Time

	// packages is the per-scan dedupe cache mapping
	// CanonicalPackageDir(relPath) -> package Node ID. The
	// first time a directory is seen EnsureFile mints a
	// package Node and a Repo→Package contains-edge; every
	// subsequent file in the same directory hits the cache
	// and skips both inserts.
	packages map[string]string

	// ancestry is the cached output of EnsureRepoAndCommit.
	// EnsureFile reads ancestry.RepoNodeID (parent of
	// package Nodes) from here. Zero-valued until
	// EnsureRepoAndCommit succeeds; EnsureFile returns
	// ErrAncestryNotReady before then.
	ancestry RepoAncestry

	// ready is the gate that distinguishes "EnsureRepoAndCommit
	// has completed successfully" from the zero-value default.
	// A failed EnsureRepoAndCommit leaves ready=false so a
	// subsequent EnsureFile cannot accidentally mint Nodes
	// without a Repo parent.
	ready bool

	// assignedRepoID is the RepoID actually written into the
	// backing store. In the legacy Postgres path
	// `EnsureRepo` allocates `repo_id` via
	// `gen_random_uuid()` (migration 0002 line 32), so
	// assignedRepoID is the *random* UUID Postgres handed
	// back. The deterministic
	// `fingerprint.RepoIDFromURL(repoURL)` value is stored
	// separately on `ancestry.RepoID` (per architecture S3.4
	// — the deterministic ID is the cross-backend parity
	// anchor); when Phase 3 wires the deterministic ID
	// through `RepoInput.RepoID` the two values converge and
	// this field can be elided.
	//
	// EnsureFile MUST use assignedRepoID (not
	// `ancestry.RepoID`) when calling InsertNode / InsertEdge
	// so the `node.repo_id REFERENCES repo (repo_id)` foreign
	// key in migration 0003 still resolves.
	assignedRepoID fingerprint.RepoID

	// parentSHA is the value forwarded to
	// `CommitInput.ParentSHA` on the EnsureCommit step.
	// Defaults to "" (root commit) so the existing unit-test
	// shape of `EnsureRepoAndCommit(ctx, branch, hints)`
	// continues to behave identically. Worker callers that
	// derive the parent SHA from `Job.FromSHA` set this via
	// `SetParentSHA` before invoking EnsureRepoAndCommit; the
	// pre-refactor worker.runFull passed `job.FromSHA`
	// through directly, so preserving that channel keeps the
	// repo_commit rows byte-identical across the refactor.
	parentSHA string

	// currentHeadSHA overrides the value AncestryWriter
	// forwards to `RepoInput.CurrentHeadSHA` on EnsureRepo.
	// When `currentHeadSHAOverridden` is false the writer
	// falls back to `a.sha` (the scan SHA) so the existing
	// unit-test shape remains green. Worker callers preserve
	// the pre-existing `repo.current_head_sha` column by
	// reading the value out of the repo row up-front and
	// calling `SetCurrentHeadSHA` before EnsureRepoAndCommit
	// runs; without that step a scan with `job.ToSHA` older
	// than the live tip would silently rewind the operator-
	// facing head pointer.
	currentHeadSHA           string
	currentHeadSHAOverridden bool
}

// RepoAncestry is the cached state of a single (repoURL, sha)
// scan after EnsureRepoAndCommit succeeds.
//
// Field semantics follow architecture S3.4:
//
//   - RepoID is the deterministic fingerprint.RepoID derived
//     from the repo URL via fingerprint.RepoIDFromURL. It is
//     the cross-backend parity anchor: a Postgres, SQLite, and
//     in-memory scan of the same URL all stamp the same RepoID
//     into every NodeFingerprint / EdgeFingerprint pre-image.
//
//     In the Stage 2.3 legacy Postgres path the actual
//     `repo.repo_id` column is still allocated by
//     `gen_random_uuid()` (because RepoInput.RepoID is not
//     wired yet); the AncestryWriter tracks that random UUID
//     internally for FK resolution and exposes the
//     deterministic ID here so downstream consumers (e.g. the
//     CLI summary printer and the future diagram projector)
//     can rely on it.
//
//   - RepoUUID is the textual UUID form of the RepoID actually
//     stored in the backing store
//     (`== graphwriter.RepoRecord.RepoID`). In Phase 3 +
//     RepoInput.RepoID this string equals
//     `RepoID.String()`; in the Stage 2.3 legacy path it is
//     the random UUID Postgres allocated.
//
//   - RepoNodeID is the Node ID of the kind=repo Node minted
//     by InsertNode. It is the parent_node_id every
//     kind=package Node references.
//
//   - CommitID is the identifier of the `repo_commit` row.
//     The Postgres schema has no surrogate ID; the row's
//     primary key is the composite (repo_id, sha). We expose
//     the SHA here as the per-repo unique identifier.
//
//   - CommitInserted is true when EnsureCommit reported the
//     row was freshly inserted, false on idempotent re-ingest.
//     The worker's `repo.registered` event predicate consumes
//     this through the dispatcher summary.
type RepoAncestry struct {
	RepoID         fingerprint.RepoID
	RepoUUID       string
	RepoNodeID     string
	CommitID       string
	CommitInserted bool
}

// FileAncestry is the per-file output of EnsureFile: the
// package and file Node IDs the caller needs to construct an
// EmitFileEvent for the AST dispatcher and to track per-file
// counters in any summary it builds (worker's FullSummary or
// the CLI's scan summary).
type FileAncestry struct {
	// FileNodeID is the kind=file Node ID minted (or hit on
	// cache) by EnsureFile. Becomes EmitFileEvent.FileNodeID
	// for the AST dispatcher.
	FileNodeID string
	// PackageNodeID is the parent kind=package Node ID. Equal
	// to the value cached in AncestryWriter.packages for
	// PackageDir.
	PackageNodeID string
	// PackageDir is `CanonicalPackageDir(file.RelPath)` — the
	// forward-slash directory path or "" for files at the
	// repo root.
	PackageDir string
	// NewlyInserted is true when the kind=file Node was
	// freshly inserted (not an idempotent re-hit). The
	// worker's PackagesInserted / FilesInserted counters
	// consume this on the per-file path.
	NewlyInserted bool
	// PackageNewlyInserted is true when the kind=package
	// Node for this file's directory was freshly inserted on
	// THIS call (i.e. cache miss + InsertNode reported
	// Inserted=true). The worker's PackagesInserted counter
	// uses this; cache hits AND idempotent re-hits both
	// report false.
	PackageNewlyInserted bool
	// PackageEdgeInserted is true when the Repo→Package
	// `contains` edge for this file's directory was freshly
	// inserted on THIS call. Same semantics as
	// PackageNewlyInserted (only true on first encounter +
	// fresh insert).
	PackageEdgeInserted bool
	// FileEdgeInserted is true when the Package→File
	// `contains` edge was freshly inserted on THIS call.
	FileEdgeInserted bool
}

// ErrAncestryNotReady is returned by EnsureFile when called
// before EnsureRepoAndCommit has completed successfully. The
// ancestry root (RepoNodeID) MUST exist before any package
// Node references it as parent; this sentinel surfaces a
// caller-side ordering bug as a typed error rather than a
// silent FK violation.
var ErrAncestryNotReady = errors.New("repoindexer: AncestryWriter: EnsureRepoAndCommit must run before EnsureFile")

// NewAncestryWriter constructs a writer scoped to one
// (repoURL, sha) scan. The deterministic RepoID is intentionally
// NOT an argument: the writer derives it inside
// EnsureRepoAndCommit via fingerprint.RepoIDFromURL so callers
// cannot accidentally pass a mismatched ID (architecture S3.4).
//
// Panics on a nil writer — the constructor cannot operate
// without a backing store; failing eagerly catches the
// programming error at wiring time instead of the first
// EnsureRepoAndCommit call.
func NewAncestryWriter(w RepoCommitNodeEdgeWriter, repoURL, sha string) *AncestryWriter {
	if w == nil {
		panic("repoindexer: NewAncestryWriter: nil RepoCommitNodeEdgeWriter")
	}
	return &AncestryWriter{
		w:        w,
		repoURL:  repoURL,
		sha:      sha,
		now:      time.Now,
		packages: make(map[string]string),
	}
}

// EnsureRepoAndCommit runs the three-step pre-walk sequence
// `EnsureRepo` -> `EnsureCommit` -> `InsertNode(kind=repo)`
// in order and caches the resulting RepoAncestry on the writer
// for subsequent EnsureFile calls.
//
// The Stage 2.3 legacy Postgres path is in effect: EnsureRepo
// is called WITHOUT a precomputed RepoID (RepoInput.RepoID is
// not wired yet), so Postgres allocates the `repo.repo_id`
// column via `gen_random_uuid()` and returns that UUID in
// RepoRecord. AncestryWriter:
//
//   - records the RANDOM UUID internally on `assignedRepoID`
//     so subsequent EnsureCommit / InsertNode / EnsureFile
//     calls satisfy the `node.repo_id REFERENCES repo (repo_id)`
//     FK from migration 0003;
//   - records the DETERMINISTIC RepoID
//     (`fingerprint.RepoIDFromURL(repoURL)`) on
//     `ancestry.RepoID` per architecture S3.4 — this is the
//     cross-backend parity anchor downstream consumers (CLI
//     summary, diagram projector) treat as the canonical
//     repo identity.
//
// When Phase 3 wires `RepoInput.RepoID` through to
// `graphwriter.Writer.EnsureRepoWithID` the two IDs converge
// (`assignedRepoID == ancestry.RepoID`) and the internal
// duality goes away.
//
// Idempotent: re-running against the same (URL, SHA) reuses
// the existing rows (EnsureRepo is upsert-by-URL, EnsureCommit
// is INSERT ... ON CONFLICT DO NOTHING, InsertNode is
// fingerprint-dedupe). Sets `CommitInserted=false` on the
// replay path so the caller's `repo.registered` predicate
// behaves correctly.
//
// Errors at any step surface unchanged; on error the writer
// stays "not ready" (subsequent EnsureFile returns
// ErrAncestryNotReady) so a partially-applied ancestry root
// cannot leak orphan package Nodes.
func (a *AncestryWriter) EnsureRepoAndCommit(ctx context.Context, defaultBranch string, hints []string) (RepoAncestry, error) {
	if a.repoURL == "" {
		return RepoAncestry{}, errors.New("repoindexer: AncestryWriter: empty repo URL")
	}
	if a.sha == "" {
		return RepoAncestry{}, errors.New("repoindexer: AncestryWriter: empty sha")
	}

	// Deterministic cross-backend parity anchor (architecture
	// S3.4). Computed up front so a malformed URL fails BEFORE
	// any backing-store write — keeps the writer's "all or
	// nothing" failure semantics intact.
	derivedID, err := fingerprint.RepoIDFromURL(a.repoURL)
	if err != nil {
		return RepoAncestry{}, fmt.Errorf("repoindexer: AncestryWriter: derive repo id from url %q: %w", a.repoURL, err)
	}

	// 1. EnsureRepo. Legacy zero-value path: RepoInput.RepoID
	// not set, Postgres allocates the column default. The
	// returned RepoRecord.ID is the random UUID we use for
	// subsequent FK-bound inserts.
	//
	// `CurrentHeadSHA` defaults to the scan SHA so the
	// stand-alone scan path (CLI / unit-test surface) stays
	// idempotent; the worker rewires this via
	// `SetCurrentHeadSHA` to round-trip the value already in
	// the `repo` row, preserving the operator-facing head
	// pointer.
	currentHead := a.sha
	if a.currentHeadSHAOverridden {
		currentHead = a.currentHeadSHA
	}
	repoRec, err := a.w.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            a.repoURL,
		DefaultBranch:  defaultBranch,
		CurrentHeadSHA: currentHead,
		LanguageHints:  hints,
	})
	if err != nil {
		return RepoAncestry{}, fmt.Errorf("repoindexer: AncestryWriter: EnsureRepo: %w", err)
	}
	a.assignedRepoID = repoRec.ID

	// 2. EnsureCommit using the assignedRepoID so the
	// `repo_commit.repo_id` FK resolves against the row just
	// upserted above. CommittedAt is now() for full scans —
	// the SHA is the source of truth for ordering; the
	// timestamp is operator-facing observability only.
	// `ParentSHA` defaults to "" (root commit) for the
	// stand-alone scan surface and is overridden by the
	// worker via `SetParentSHA` so multi-commit pushes record
	// the real predecessor SHA.
	commitRec, err := a.w.EnsureCommit(ctx, graphwriter.CommitInput{
		RepoID:      a.assignedRepoID,
		SHA:         a.sha,
		ParentSHA:   a.parentSHA,
		CommittedAt: a.now().UTC(),
	})
	if err != nil {
		return RepoAncestry{}, fmt.Errorf("repoindexer: AncestryWriter: EnsureCommit: %w", err)
	}

	// 3. InsertNode(kind=repo). The canonical signature is
	// just the URL — the root Repo Node is the only Node of
	// its kind per repo so a richer signature would be
	// redundant. AttrsJSON uses the shared `fullModeAttrs`
	// wire shape so the byte-format matches the existing
	// worker (Producer="repoindexer.full"); a follow-on
	// rewire of worker.runFull through this type keeps the
	// node.attrs_json column bit-identical for replays.
	repoAttrs, err := json.Marshal(fullModeAttrs{Producer: "repoindexer.full"})
	if err != nil {
		return RepoAncestry{}, fmt.Errorf("repoindexer: AncestryWriter: marshal repo attrs: %w", err)
	}
	nodeRec, err := a.w.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             a.assignedRepoID,
		Kind:               "repo",
		CanonicalSignature: CanonicalRepoSig(a.repoURL),
		FromSHA:            a.sha,
		AttrsJSON:          repoAttrs,
	})
	if err != nil {
		return RepoAncestry{}, fmt.Errorf("repoindexer: AncestryWriter: InsertNode(repo): %w", err)
	}

	a.ancestry = RepoAncestry{
		RepoID:         derivedID,
		RepoUUID:       repoRec.RepoID,
		RepoNodeID:     nodeRec.NodeID,
		CommitID:       commitRec.SHA,
		CommitInserted: commitRec.Inserted,
	}
	a.ready = true
	return a.ancestry, nil
}

// EnsureFile runs the per-file portion of the ancestry pipeline:
//
//  1. resolve the file's canonical package directory via
//     CanonicalPackageDir;
//  2. on cache miss, InsertNode(kind=package) with
//     parent_node_id pointing at the cached RepoNodeID, then
//     InsertEdge(kind=contains) Repo→Package; cache the
//     package Node ID;
//  3. InsertNode(kind=file) with parent_node_id pointing at
//     the package Node, then InsertEdge(kind=contains)
//     Package→File.
//
// Returns a FileAncestry describing the package and file Node
// IDs the caller hands to the AST dispatcher (and the
// `*Inserted` counters drive worker / CLI summary tracking).
//
// Returns ErrAncestryNotReady if invoked before
// EnsureRepoAndCommit succeeds — the caller is responsible for
// running the pre-walk sequence first; this gate prevents a
// programming error from silently minting orphan Nodes whose
// parent_node_id is empty.
//
// All inserts use `a.assignedRepoID` (the actually-stored
// RepoID), NOT `a.ancestry.RepoID` (the deterministic anchor),
// so the FK constraints on node.repo_id / edge.repo_id resolve
// in the Stage 2.3 legacy Postgres path. The two IDs are
// equal in Phase 3 + memory / SQLite sinks.
func (a *AncestryWriter) EnsureFile(ctx context.Context, file WalkFile) (FileAncestry, error) {
	if !a.ready {
		return FileAncestry{}, ErrAncestryNotReady
	}

	dir := CanonicalPackageDir(file.RelPath)

	var (
		out                  FileAncestry
		pkgNodeID            string
		pkgNewlyInserted     bool
		pkgEdgeInserted      bool
	)
	pkgNodeID, cached := a.packages[dir]
	if !cached {
		pkgAttrs, mErr := json.Marshal(fullModeAttrs{
			RelPath: dir, Producer: "repoindexer.full",
		})
		if mErr != nil {
			return FileAncestry{}, fmt.Errorf("repoindexer: AncestryWriter: marshal package attrs (%q): %w", dir, mErr)
		}
		pkgRec, pErr := a.w.InsertNode(ctx, graphwriter.NodeInput{
			RepoID:             a.assignedRepoID,
			Kind:               "package",
			CanonicalSignature: CanonicalPackageSig(a.repoURL, dir),
			ParentNodeID:       a.ancestry.RepoNodeID,
			FromSHA:            a.sha,
			AttrsJSON:          pkgAttrs,
		})
		if pErr != nil {
			return FileAncestry{}, fmt.Errorf("repoindexer: AncestryWriter: InsertNode(package %q): %w", dir, pErr)
		}
		edgeRec, eErr := a.w.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID:    a.assignedRepoID,
			Kind:      "contains",
			SrcNodeID: a.ancestry.RepoNodeID,
			DstNodeID: pkgRec.NodeID,
			FromSHA:   a.sha,
		})
		if eErr != nil {
			return FileAncestry{}, fmt.Errorf("repoindexer: AncestryWriter: InsertEdge(repo->pkg %q): %w", dir, eErr)
		}
		pkgNodeID = pkgRec.NodeID
		pkgNewlyInserted = pkgRec.Inserted
		pkgEdgeInserted = edgeRec.Inserted
		a.packages[dir] = pkgNodeID
	}

	fileAttrs, mErr := json.Marshal(fullModeAttrs{
		RelPath: file.RelPath, Producer: "repoindexer.full",
	})
	if mErr != nil {
		return FileAncestry{}, fmt.Errorf("repoindexer: AncestryWriter: marshal file attrs (%q): %w", file.RelPath, mErr)
	}
	fileRec, fErr := a.w.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             a.assignedRepoID,
		Kind:               "file",
		CanonicalSignature: CanonicalFileSig(a.repoURL, file.RelPath),
		ParentNodeID:       pkgNodeID,
		FromSHA:            a.sha,
		AttrsJSON:          fileAttrs,
	})
	if fErr != nil {
		return FileAncestry{}, fmt.Errorf("repoindexer: AncestryWriter: InsertNode(file %q): %w", file.RelPath, fErr)
	}
	fileEdgeRec, eErr := a.w.InsertEdge(ctx, graphwriter.EdgeInput{
		RepoID:    a.assignedRepoID,
		Kind:      "contains",
		SrcNodeID: pkgNodeID,
		DstNodeID: fileRec.NodeID,
		FromSHA:   a.sha,
	})
	if eErr != nil {
		return FileAncestry{}, fmt.Errorf("repoindexer: AncestryWriter: InsertEdge(pkg->file %q): %w", file.RelPath, eErr)
	}

	out = FileAncestry{
		FileNodeID:           fileRec.NodeID,
		PackageNodeID:        pkgNodeID,
		PackageDir:           dir,
		NewlyInserted:        fileRec.Inserted,
		PackageNewlyInserted: pkgNewlyInserted,
		PackageEdgeInserted:  pkgEdgeInserted,
		FileEdgeInserted:     fileEdgeRec.Inserted,
	}
	return out, nil
}

// Ancestry returns the cached RepoAncestry. Zero-valued until
// EnsureRepoAndCommit succeeds. Exposed (read-only by
// construction) so callers that need to query ancestry state
// after the pre-walk sequence don't have to thread the
// EnsureRepoAndCommit return value around.
func (a *AncestryWriter) Ancestry() RepoAncestry { return a.ancestry }

// Ready reports whether EnsureRepoAndCommit has completed
// successfully on this writer. False until the first
// successful call; never reset (a fresh scan needs a fresh
// AncestryWriter).
func (a *AncestryWriter) Ready() bool { return a.ready }

// SetParentSHA overrides the `ParentSHA` value AncestryWriter
// forwards to `EnsureCommit` when `EnsureRepoAndCommit` runs.
// MUST be invoked BEFORE `EnsureRepoAndCommit` — calling it
// afterwards has no effect on the already-committed row.
//
// The pre-refactor worker.runFull passed `Job.FromSHA` directly
// to `EnsureCommit.ParentSHA`; preserving that channel through
// the AncestryWriter keeps `repo_commit` rows byte-identical
// after the worker rewires to this type. The default ("") is
// the right value for the stand-alone scan surface (CLI /
// unit-test path) where there is no parent SHA to thread.
func (a *AncestryWriter) SetParentSHA(s string) { a.parentSHA = s }

// SetCurrentHeadSHA overrides the `CurrentHeadSHA` value
// AncestryWriter forwards to `EnsureRepo` when
// `EnsureRepoAndCommit` runs. MUST be invoked BEFORE
// `EnsureRepoAndCommit` — calling it afterwards has no effect
// on the already-upserted row.
//
// Without the override, `EnsureRepo` falls back to the scan SHA
// (`a.sha`), which would silently rewind the operator-facing
// `repo.current_head_sha` column whenever the scan SHA is older
// than the live tip (e.g. a delayed full-mode re-ingest, or a
// manual operator re-scan of a historical commit). Worker
// callers preserve the pre-existing value by reading the
// `current_head_sha` column out of the `repo` row up front and
// calling this setter before `EnsureRepoAndCommit`. The
// stand-alone scan surface (CLI / unit-test path) leaves the
// override unset and accepts the scan-SHA default because it is
// the first writer to populate the row.
func (a *AncestryWriter) SetCurrentHeadSHA(s string) {
	a.currentHeadSHA = s
	a.currentHeadSHAOverridden = true
}
