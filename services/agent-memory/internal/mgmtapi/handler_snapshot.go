package mgmtapi

// mgmt.snapshot -- implementation-plan.md Stage 7.4.
//
// `POST /v1/repos/{id}/snapshot` triggers a forced re-embed
// of every Method / Block node and every promoted Concept
// supported by the repo at the currently active embedding-
// model version (architecture.md §6.2.1, tech-spec §9.6 /
// §9.6a).
//
// Wire shape
// ----------
// The verb takes no request fields. The composition root
// supplies the active `embedding_model_version` via
// [Options.ActiveEmbeddingModelVersion]; the caller cannot
// override it on the wire because doing so would let an
// operator enqueue publishes for a model the rest of the
// system does not consider active (per rubber-duck review
// item #1).
//
// Successful 202 body:
//
//	{
//	  "repo_id":                 "...",
//	  "snapshot_id":             "...",
//	  "embedding_model_version": "v3",
//	  "node_publish_count":      100,
//	  "concept_publish_count":   5,
//	  "publish_count":           105,
//	  "degraded":                false
//	}
//
// Cross-store protocol (tech-spec §9.6a)
// --------------------------------------
// For each target the handler appends:
//
//  1. An `embedding_publish` row carrying the target id, the
//     active `embedding_model_version`, and a freshly
//     generated `qdrant_point_id` (the embedding-index writer
//     treats this id as authoritative; it never regenerates).
//  2. An `embedding_publish_event` row with
//     `event_kind='queued'` and `details_json` carrying the
//     snapshot's tracking uuid so an operator can later query
//     "show me every publish from this snapshot".
//
// The transition to `vector_written` / `published` / the
// retroactive `superseded` event on prior published rows are
// the EmbeddingIndex writer's responsibility (Repo Indexer
// for Method/Block, Concept Promoter for Concept). mgmt.api
// merely seeds the work.
//
// Tombstone hygiene
// -----------------
// `node` rows are append-only (G5); a retired node is
// indicated by the presence of a `node_retirement` tombstone.
// We anti-join `node_retirement` so a snapshot does not
// queue Qdrant work for dead code (rubber-duck review item
// #2). Concept publishes implicitly skip retired versions
// via the `concept_version.promoted = true` predicate
// because the Concept Promoter unsets `promoted` when it
// retires a version.
//
// Prior-publish hygiene
// ---------------------
// The snapshot enqueuer has no source bytes in-process, so
// the queued event it writes carries an EMPTY `content`
// field. The §9.6a flusher's `PublishEventContentResolver`
// fills that gap by looking up the most recent published
// row for the same target via `resolveSnapshotFallback`
// (see `internal/embedding/publish_event_resolver.go`). For
// targets that have NEVER been published, that fallback
// has nothing to inherit and the publish would stay
// permanently queued: the flusher's stuck-row scan keeps
// re-picking it tick after tick, the resolver keeps
// failing, and `embedding.flusher.resolve_failed` log
// noise accumulates forever (the flusher leaves the row
// untouched on `ResolveErrors`, so `latest_at` never moves
// forward past `queuedAgeThreshold`). Both CTEs therefore
// add an `EXISTS (SELECT 1 FROM embedding_publish ep ...)`
// predicate so the snapshot only targets ids that already
// have at least one publish row the resolver can fall back
// to. The first-time embed of a brand-new node / concept
// is the responsibility of the Repo Indexer / Concept
// Promoter ingest path -- snapshot is a re-embed verb, not
// an initial-embed verb.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// insertNodePublishesSQL is the snapshot handler's
// Method/Block-side CTE. Hoisted to package-level so the
// behavioural test
// [TestSnapshot_handlerSourceDoesNotEmitSuperseded] can scan
// the literal for forbidden tokens (e.g. `'superseded'`) and
// so a future caller composing additional logic on top of
// the snapshot enqueue path references the exact bytes the
// handler issues.
//
// Anti-join `node_retirement` keeps retired nodes out of the
// queued publish set (rubber-duck item #2 / G5).
//
// `EXISTS (SELECT 1 FROM embedding_publish ep WHERE
// ep.node_id = n.node_id)` keeps nodes that have NEVER been
// published out of the queued publish set, because the
// resolver's content-fallback lookup
// (`resolveSnapshotFallback`) needs at least one prior
// publish row to inherit content from; without this filter
// the snapshot would seed permanently-stuck queued rows
// that waste flusher cycles every retry tick. The
// `embedding_publish_node_id_idx` partial index makes the
// probe O(log N).
const insertNodePublishesSQL = `
		WITH src AS (
			SELECT n.node_id
			FROM node n
			LEFT JOIN node_retirement nr ON nr.node_id = n.node_id
			WHERE n.repo_id = $1::uuid
			  AND n.kind IN ('method'::node_kind, 'block'::node_kind)
			  AND nr.node_id IS NULL
			  AND EXISTS (
			      SELECT 1 FROM embedding_publish ep
			       WHERE ep.node_id = n.node_id
			  )
		),
		ins AS (
			INSERT INTO embedding_publish
				(node_id, embedding_model_version, qdrant_point_id)
			SELECT node_id, $2, gen_random_uuid()
			FROM src
			RETURNING publish_id
		),
		ev AS (
			INSERT INTO embedding_publish_event
				(publish_id, event_kind, attempt_index, details_json)
			SELECT publish_id, 'queued'::embedding_publish_event_kind, 0, $3::jsonb
			FROM ins
			RETURNING publish_id
		)
		SELECT count(*)::bigint FROM ev
	`

// insertConceptPublishesSQL is the snapshot handler's Concept-
// side CTE. DISTINCT over concept_version_id collapses the
// (concept_version × multi-support) cartesian product so two
// support rows in the same repo do not produce two
// publishes. `cv.promoted = true` keeps the snapshot from
// re-embedding versions the Concept Promoter has already
// retired off the recall path.
//
// `EXISTS (SELECT 1 FROM embedding_publish ep WHERE
// ep.concept_version_id = cv.concept_version_id)` mirrors
// the node-side prior-publish gate. In practice
// `cv.promoted = true` already implies a Promoter-written
// publish row by construction (the promoter writes both in
// the same transaction), but the explicit predicate is
// defence-in-depth against a future code path that flips
// `promoted` without seeding a publish, and keeps the two
// CTEs symmetric for the next reader. The
// `embedding_publish_concept_version_id_idx` partial index
// keeps the probe O(log N).
const insertConceptPublishesSQL = `
		WITH src AS (
			SELECT DISTINCT cv.concept_version_id
			FROM concept_version cv
			JOIN concept_support cs USING (concept_version_id)
			WHERE cs.repo_id = $1::uuid
			  AND cv.promoted = true
			  AND EXISTS (
			      SELECT 1 FROM embedding_publish ep
			       WHERE ep.concept_version_id = cv.concept_version_id
			  )
		),
		ins AS (
			INSERT INTO embedding_publish
				(concept_version_id, embedding_model_version, qdrant_point_id)
			SELECT concept_version_id, $2, gen_random_uuid()
			FROM src
			RETURNING publish_id
		),
		ev AS (
			INSERT INTO embedding_publish_event
				(publish_id, event_kind, attempt_index, details_json)
			SELECT publish_id, 'queued'::embedding_publish_event_kind, 0, $3::jsonb
			FROM ins
			RETURNING publish_id
		)
		SELECT count(*)::bigint FROM ev
	`

// SnapshotResponse is the wire shape of a successful POST
// /v1/repos/{id}/snapshot. The shape mirrors
// [IngestResponse] / [IngestDeltaResponse] in keeping the
// `degraded` envelope at the top level (architecture.md
// §6.3); Stage 7.4 always emits `degraded:false` because
// write verbs do not have a degraded mode.
type SnapshotResponse struct {
	RepoID                string `json:"repo_id"`
	SnapshotID            string `json:"snapshot_id"`
	EmbeddingModelVersion string `json:"embedding_model_version"`
	// NodePublishCount counts EmbeddingPublish rows
	// inserted for Method + Block nodes.
	NodePublishCount int `json:"node_publish_count"`
	// ConceptPublishCount counts EmbeddingPublish rows
	// inserted for promoted ConceptVersions backed by this
	// repo's concept_support rows.
	ConceptPublishCount int `json:"concept_publish_count"`
	// PublishCount is the sum of node + concept publishes;
	// equals the increment applied to
	// `snapshot_pending_total` by this call.
	PublishCount int `json:"publish_count"`
	// Degraded mirrors the §6.3 degraded-mode contract;
	// always false for write verbs in Stage 7.x.
	Degraded bool `json:"degraded"`
}

// snapshotCounts bundles the two count outputs of
// [executeSnapshot] so handleSnapshot can log and respond
// uniformly without re-marshalling.
type snapshotCounts struct {
	nodePublishes    int
	conceptPublishes int
}

func (h *Handler) handleSnapshot(w http.ResponseWriter, r *http.Request, rawRepoID string) {
	ctx := r.Context()

	if !reUUID.MatchString(rawRepoID) {
		writeJSONError(w, http.StatusBadRequest, "invalid_repo_id",
			"repo_id path segment is not a valid UUID")
		return
	}

	// Snapshot accepts an empty body. We still drain it
	// through the configured cap so a pathological client
	// cannot flood us; the (zero-field) request shape exists
	// so future Stage 8.x options can be added without a
	// breaking change.
	var req struct{}
	if !h.decodeJSONBody(w, r, &req) {
		return
	}

	// 503 (vs 500) when the active model version is missing:
	// it's a configuration problem the operator can fix by
	// setting AGENT_MEMORY_EMBEDDING_MODEL_VERSION, not a
	// runtime bug we should hide as an internal error.
	modelVersion := h.activeEmbeddingModelVersion
	if modelVersion == "" {
		h.logger.Error("mgmtapi.snapshot.no_active_model_version",
			slog.String("op", "snapshot"),
			slog.String("repo_id", rawRepoID),
		)
		writeJSONError(w, http.StatusServiceUnavailable,
			"embedding_model_version_unconfigured",
			"snapshot is unavailable: no active embedding_model_version is configured")
		return
	}

	// Verify the repo exists BEFORE the snapshot tx so an
	// unknown id returns a clean 404 driven by sql.ErrNoRows
	// rather than relying on FK-violation substring matches
	// at the driver layer. Mirrors handleIngest /
	// handleIngestDelta.
	if _, _, _, err := h.loadRepo(ctx, rawRepoID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSONError(w, http.StatusNotFound, "repo_not_found",
				"no repo with the supplied repo_id")
			return
		}
		h.logger.Error("mgmtapi.snapshot.load_repo_failed",
			slog.String("op", "snapshot"),
			slog.String("repo_id", rawRepoID),
			slog.String("error", err.Error()),
		)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	snapshotID, err := h.newUUID()
	if err != nil {
		h.logger.Error("mgmtapi.snapshot.uuid_gen_failed",
			slog.String("op", "snapshot"),
			slog.String("repo_id", rawRepoID),
			slog.String("error", err.Error()),
		)
		writeJSONError(w, http.StatusInternalServerError, "internal_error",
			"failed to generate snapshot id")
		return
	}

	counts, err := h.executeSnapshot(ctx, rawRepoID, modelVersion, snapshotID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || isForeignKeyViolation(err) {
			// Repo deleted between loadRepo and the
			// CTE INSERT (vanishingly unlikely but
			// possible).
			writeJSONError(w, http.StatusNotFound, "repo_not_found",
				"no repo with the supplied repo_id")
			return
		}
		h.logger.Error("mgmtapi.snapshot.db_failed",
			slog.String("op", "snapshot"),
			slog.String("repo_id", rawRepoID),
			slog.String("snapshot_id", snapshotID),
			slog.String("error", err.Error()),
		)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	total := counts.nodePublishes + counts.conceptPublishes
	// Always-call: empty repos still emit the counter event
	// so log / scrape shape parity is preserved across calls
	// (see the Metrics interface doc).
	h.metrics.IncSnapshotPending(total)

	resp := SnapshotResponse{
		RepoID:                rawRepoID,
		SnapshotID:            snapshotID,
		EmbeddingModelVersion: modelVersion,
		NodePublishCount:      counts.nodePublishes,
		ConceptPublishCount:   counts.conceptPublishes,
		PublishCount:          total,
		Degraded:              false,
	}

	subject, _ := SubjectFromContext(ctx)
	h.logger.Info("mgmtapi.snapshot.ok",
		slog.String("op", "snapshot"),
		slog.String("repo_id", rawRepoID),
		slog.String("snapshot_id", snapshotID),
		slog.String("embedding_model_version", modelVersion),
		slog.Int("node_publish_count", counts.nodePublishes),
		slog.Int("concept_publish_count", counts.conceptPublishes),
		slog.Int("publish_count", total),
		slog.String("subject", subject),
		slog.Time("at", h.clock()),
	)
	writeJSONResponse(w, http.StatusAccepted, resp)
}

// executeSnapshot runs the two CTE inserts inside a single
// transaction so a partial failure (e.g. concept side fails
// after node side already wrote) does not leave the repo in
// a half-snapshotted state. Returns the per-target counts on
// success.
//
// The CTEs append-only -- no row is updated. Both inserts
// also write a matching `embedding_publish_event` with
// `event_kind='queued'` and a `details_json` payload
// carrying the snapshot's tracking uuid; the EmbeddingIndex
// writer will subsequently append `'vector_written'` /
// `'published'` / `'superseded'` events per tech-spec §9.6a.
//
// `details_json` shape (queued event):
//
//	{
//	  "snapshot_id": "<uuid>",
//	  "source":      "mgmt.snapshot",
//	  "embedding_model_version": "<v>"
//	}
//
// The handler treats an empty repo (zero Method/Block/
// promoted-Concept targets) as a successful no-op: 202 with
// publish_count = 0. Logging upstream still records the call
// so operator forensics can prove the snapshot was attempted.
// "Empty" here ALSO subsumes the case where the repo has
// nodes / promoted concepts but none of them have ever been
// published (see the prior-publish hygiene note in the
// package doc): those targets are deliberately excluded by
// the EXISTS predicate inside [insertNodePublishesSQL] /
// [insertConceptPublishesSQL] because the §7.4 fallback
// resolver has no prior content to inherit from.
func (h *Handler) executeSnapshot(ctx context.Context, repoID, modelVersion, snapshotID string) (snapshotCounts, error) {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return snapshotCounts{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	details, err := buildSnapshotDetailsJSON(snapshotID, modelVersion)
	if err != nil {
		return snapshotCounts{}, fmt.Errorf("build details_json: %w", err)
	}

	// ----- Method / Block nodes -----
	//
	// LEFT JOIN node_retirement nr ... WHERE nr.node_id IS
	// NULL is the canonical "current Node only" anti-join
	// (architecture.md §5.2.4); the unique index on
	// node_retirement.node_id (migration 0004) keeps this
	// O(log N) per probe.
	//
	// The `EXISTS (SELECT 1 FROM embedding_publish ...)`
	// predicate excludes never-previously-published nodes
	// so the snapshot does not seed permanently-stuck
	// queued rows the §7.4 fallback resolver cannot
	// satisfy. See the prior-publish hygiene note in the
	// package doc.
	//
	// The outer SELECT count(*) returns the row count of the
	// LAST CTE (`ev`) -- equal to the number of queued
	// events inserted, which by construction equals the
	// number of EmbeddingPublish rows inserted. We could
	// instead RETURNING count(*) but PostgreSQL does not
	// allow aggregates in RETURNING, so we wrap the chain in
	// a top-level SELECT.
	//
	// The literal SQL lives in [insertNodePublishesSQL] at
	// package level so behavioural tests can scan it for
	// forbidden tokens (e.g. `'superseded'`) without
	// having to invoke the handler.
	var nodeCount int64
	if err := tx.QueryRowContext(ctx, insertNodePublishesSQL, repoID, modelVersion, details).
		Scan(&nodeCount); err != nil {
		return snapshotCounts{}, fmt.Errorf("insert node publishes: %w", err)
	}

	// ----- promoted Concept versions backed by this repo --
	//
	// `concept_support` is the per-repo linkage row that
	// ties a Concept (cross-repo by G6) to a particular
	// repository. We DISTINCT over concept_version_id so two
	// support rows in the same repo do not produce two
	// publishes.
	//
	// `cv.promoted = true` ensures the version is currently
	// promoted -- if the Concept Promoter has retired it,
	// it is already off the recall path and re-embedding it
	// would waste work.
	//
	// The `EXISTS (SELECT 1 FROM embedding_publish ...)`
	// predicate is the concept-side mirror of the node-side
	// prior-publish gate (defence-in-depth; today every
	// `promoted = true` row already has a Promoter-written
	// publish row in the same transaction).
	//
	// See [insertConceptPublishesSQL] for the package-level
	// literal.
	var conceptCount int64
	if err := tx.QueryRowContext(ctx, insertConceptPublishesSQL, repoID, modelVersion, details).
		Scan(&conceptCount); err != nil {
		return snapshotCounts{}, fmt.Errorf("insert concept publishes: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return snapshotCounts{}, fmt.Errorf("commit tx: %w", err)
	}
	return snapshotCounts{
		nodePublishes:    int(nodeCount),
		conceptPublishes: int(conceptCount),
	}, nil
}

// buildSnapshotDetailsJSON canonicalises the details payload
// every queued EmbeddingPublishEvent emitted by this snapshot
// will carry. Centralised so the field shape stays stable
// across the two CTEs and so tests can compare against the
// exact bytes.
func buildSnapshotDetailsJSON(snapshotID, modelVersion string) ([]byte, error) {
	// json.Marshal of a struct (not a map) so the field
	// order is deterministic, which makes operator-side
	// equality checks against details_json straightforward.
	payload := struct {
		SnapshotID            string `json:"snapshot_id"`
		Source                string `json:"source"`
		EmbeddingModelVersion string `json:"embedding_model_version"`
	}{
		SnapshotID:            snapshotID,
		Source:                "mgmt.snapshot",
		EmbeddingModelVersion: modelVersion,
	}
	return json.Marshal(payload)
}
