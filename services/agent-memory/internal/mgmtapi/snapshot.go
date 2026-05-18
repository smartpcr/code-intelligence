package mgmtapi

// Stage 7.4: `mgmt.snapshot` -- POST /v1/repos/{repo_id}/snapshot.
//
// architecture.md §6.2.1 line 740 / implementation-plan Stage
// 7.4 pin the verb as the operator-driven hook that forces a
// re-embed of every Method, Block, and promoted Concept
// attributable to `repo_id` using the CURRENTLY active
// embedding-model version (risk §9.6).  The HTTP handler is the
// thin enqueue half: it validates, authorises, defers to the
// injected `Snapshotter`, and returns 202 + counts.  The
// fan-out (queueing one fresh `embedding_publish` per target,
// stamping the `supersedes_publish_id` discriminator) lives in
// `internal/snapshot/service.go`; the publisher's transition-
// to-published hook (see `internal/embedding/publisher.go`)
// emits the matching `superseded` event for the prior publish
// in the SAME transaction as the new `published` event so the
// recall index never sees two `published` rows for the same
// Node simultaneously.
//
// Why a separate `Snapshotter` interface?
// ---------------------------------------
// The mgmt-api binary owns `*sql.DB` only; it has no
// `embedding.Publisher` (the embed + Qdrant chain belongs to
// repoindexer / promoter binaries).  The verb's responsibility
// is purely enqueue — write fresh `embedding_publish` +
// `queued` event rows — which needs only the DB.  By exposing
// a `Snapshotter` interface here we:
//
//   - keep the handler's auth/validate/respond pipeline free
//     of the snapshot package's per-target SQL fan-out; the
//     handler unit tests can stub the interface and exercise
//     the full HTTP surface without sqlmock-ing the enqueue
//     queries.
//   - leave the door open for a hypothetical alternative
//     Snapshotter (e.g. a Kafka-publishing variant) without
//     touching the handler.
//
// Production wires the concrete `*snapshot.Service` via
// `Options.Snapshotter` in `cmd/mgmt-api/main.go`.
//
// 202 vs 201 / 200
// ----------------
// The verb is asynchronous: the publisher drains the queue on
// its own cadence; the operator polls `mgmt.read.embedding_*`
// (Stage 7.5) for completion.  Per RFC 7231 §6.3 we return 202
// Accepted to make the asynchrony explicit on the wire.
//
// Nil-tolerance: when `Options.Snapshotter` is unset (e.g. a
// binary that omits the snapshot wiring) the handler returns
// 503 + `snapshot_unavailable` rather than panicking.  This
// matches the §6.3 "fail loud, fail safe" pattern on
// optional surfaces.

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"
)

// snapshotSuffix is the trailing segment of the snapshot
// verb's path.  Kept lower-case to match `ingestSuffix` /
// `ingestDeltaSuffix` and the rest of the surface.
const snapshotSuffix = "/snapshot"

// Snapshotter is the wiring seam the snapshot verb handler
// uses to invoke the per-target enqueue protocol.  Production
// wires `*snapshot.Service` (in `internal/snapshot`) via
// `Options.Snapshotter`; unit tests pass a stub so the HTTP
// pipeline can be exercised without a live DB.
//
// Implementations MUST:
//
//   - Return [ErrSnapshotRepoNotFound] when `repoID` is a
//     well-formed UUID but no `repo` row matches.  The handler
//     maps this to 404 + `repo_not_found`.
//   - Return any other error for transient / DB-class failures;
//     the handler maps those to 500 + `internal_error`.
//   - Be safe for concurrent use across goroutines (the HTTP
//     server multiplexes requests onto a shared handler).
type Snapshotter interface {
	Snapshot(ctx context.Context, repoID string) (SnapshotResult, error)
}

// ErrSnapshotRepoNotFound is the sentinel a `Snapshotter`
// returns when the supplied `repo_id` is well-formed but the
// repo row does not exist.  The handler maps it to a 404
// envelope so the operator can distinguish "wrong id" from
// "transient outage".
var ErrSnapshotRepoNotFound = errors.New("mgmtapi: snapshot: repo not found")

// SnapshotResult is the wire-safe handoff between the
// Snapshotter and the HTTP handler.  Field names are the
// downstream JSON payload's exact shape, so a handler that
// returns `result` verbatim does not need a separate "render"
// step.  See [SnapshotResponse] for the operator-facing JSON
// envelope; the result type is what `*snapshot.Service`
// already returns natively to avoid an avoidable copy at the
// handler/service boundary.
type SnapshotResult struct {
	// SnapshotID is the opaque token the Snapshotter mints
	// per call and stamps onto every queued event.  The
	// operator surfaces it in subsequent audit queries.
	SnapshotID string
	// ModelVersion is the `embedding_model_version` value
	// the new publish rows were stamped with.
	ModelVersion string
	// MethodsEnqueued is the count of fresh
	// `embedding_publish` rows written for `kind='method'`
	// Nodes in the repo.
	MethodsEnqueued int
	// BlocksEnqueued is the count of fresh
	// `embedding_publish` rows written for `kind='block'`
	// Nodes in the repo.
	BlocksEnqueued int
	// ConceptsEnqueued is the count of fresh
	// `embedding_publish` rows written for promoted
	// `concept_version` rows attributable to the repo.
	ConceptsEnqueued int
	// MethodBlocksSkipped is the count of Method/Block
	// targets the Snapshotter skipped because their prior
	// publish already had a non-terminal snapshot
	// replacement queued (the dedupe gate the §6.2.1 verb
	// honours when the operator double-clicks the snapshot
	// button).
	MethodBlocksSkipped int
	// ConceptsSkipped mirrors `MethodBlocksSkipped` for
	// concept targets.
	ConceptsSkipped int
}

// SnapshotResponse is the wire shape of a successful POST
// /v1/repos/{repo_id}/snapshot.  Cumulative counts make the
// 202 envelope self-describing — the operator does not need
// to follow up with a read verb just to know how many targets
// were enqueued.
type SnapshotResponse struct {
	// SnapshotID is the opaque token to correlate this call
	// with the downstream `embedding_publish_event`
	// `details_json` rows.
	SnapshotID string `json:"snapshot_id"`
	// ModelVersion is the embedding model version the
	// snapshot service stamped onto every new publish row.
	ModelVersion string `json:"model_version"`
	// MethodsEnqueued is the count of fresh
	// `embedding_publish` rows written for Method Nodes.
	MethodsEnqueued int `json:"methods_enqueued"`
	// BlocksEnqueued is the count of fresh
	// `embedding_publish` rows written for Block Nodes.
	BlocksEnqueued int `json:"blocks_enqueued"`
	// ConceptsEnqueued is the count of fresh
	// `embedding_publish` rows written for promoted
	// `concept_version` rows attributable to the repo.
	ConceptsEnqueued int `json:"concepts_enqueued"`
	// TargetsSkipped is the cumulative skipped-count
	// (Method/Block + Concept).  Surfaced so the operator
	// can distinguish "no targets to re-embed" from
	// "snapshot already in flight".
	TargetsSkipped int `json:"targets_skipped"`
}

// extractSnapshotPath parses `/v1/repos/{repo_id}/snapshot`.
// Returns the raw repo_id and the suffix (`/snapshot`).  The
// repo_id is NOT validated as a UUID here — that's the verb
// handler's job so 400 / 404 attribution stays clean.
//
// Returns ok=false for any path that does not match the shape
// exactly (extra path segments, empty repo, missing suffix).
func extractSnapshotPath(path string) (repoID, suffix string, ok bool) {
	rest := strings.TrimPrefix(path, RouteRepos+"/")
	if !strings.HasSuffix(rest, snapshotSuffix) {
		return "", "", false
	}
	id := strings.TrimSuffix(rest, snapshotSuffix)
	if id == "" || strings.Contains(id, "/") {
		return "", "", false
	}
	return id, snapshotSuffix, true
}

// handleSnapshot is the dispatch target for POST
// /v1/repos/{repo_id}/snapshot.  Always emits a JSON
// envelope; never panics on a nil Snapshotter (a
// configuration error degrades to 503 service_unavailable
// rather than crashing the process).
func (h *Handler) handleSnapshot(w http.ResponseWriter, r *http.Request, rawRepoID string) {
	ctx := r.Context()

	if !reUUID.MatchString(rawRepoID) {
		writeJSONError(w, http.StatusBadRequest, "invalid_repo_id",
			"repo_id path segment is not a valid UUID")
		return
	}

	// Defence-in-depth: confirm the repo exists BEFORE we
	// delegate to the snapshotter so a 404 is attributed to
	// the repo (not "the snapshot service rejected it").
	// The Snapshotter implementation MUST also check (so a
	// concurrent delete between this loadRepo and the
	// snapshot enqueue still surfaces correctly).
	_, _, _, err := h.loadRepo(ctx, rawRepoID)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "repo_not_found",
			"no repo with the supplied repo_id")
		return
	}
	if err != nil {
		h.logger.Error("mgmtapi.snapshot.load_repo_failed",
			slog.String("op", "snapshot"),
			slog.String("repo_id", rawRepoID),
			slog.String("error", err.Error()),
		)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	if h.snapshotter == nil {
		// Optional surface not wired — fail loud per §6.3.
		h.logger.Warn("mgmtapi.snapshot.unconfigured",
			slog.String("op", "snapshot"),
			slog.String("repo_id", rawRepoID),
		)
		writeJSONError(w, http.StatusServiceUnavailable, "snapshot_unavailable",
			"snapshot endpoint is not enabled on this binary")
		return
	}

	result, err := h.snapshotter.Snapshot(ctx, rawRepoID)
	if err != nil {
		if errors.Is(err, ErrSnapshotRepoNotFound) {
			writeJSONError(w, http.StatusNotFound, "repo_not_found",
				"no repo with the supplied repo_id")
			return
		}
		h.logger.Error("mgmtapi.snapshot.enqueue_failed",
			slog.String("op", "snapshot"),
			slog.String("repo_id", rawRepoID),
			slog.String("error", err.Error()),
		)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	resp := SnapshotResponse{
		SnapshotID:       result.SnapshotID,
		ModelVersion:     result.ModelVersion,
		MethodsEnqueued:  result.MethodsEnqueued,
		BlocksEnqueued:   result.BlocksEnqueued,
		ConceptsEnqueued: result.ConceptsEnqueued,
		TargetsSkipped:   result.MethodBlocksSkipped + result.ConceptsSkipped,
	}

	subject, _ := SubjectFromContext(ctx)
	h.logger.Info("mgmtapi.snapshot.ok",
		slog.String("op", "snapshot"),
		slog.String("repo_id", rawRepoID),
		slog.String("snapshot_id", resp.SnapshotID),
		slog.String("model_version", resp.ModelVersion),
		slog.Int("methods_enqueued", resp.MethodsEnqueued),
		slog.Int("blocks_enqueued", resp.BlocksEnqueued),
		slog.Int("concepts_enqueued", resp.ConceptsEnqueued),
		slog.Int("targets_skipped", resp.TargetsSkipped),
		slog.String("subject", subject),
		slog.Time("at", h.clock()),
	)
	writeJSONResponse(w, http.StatusAccepted, resp)
}
