package embedding

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// PublishEventContentResolver is the production-side
// `ContentResolver` used by `Flusher` to reconstruct a
// `PublishRequest` from the persisted §9.6a write-log,
// WITHOUT requiring the caller (the long-running worker
// process) to keep source bytes in memory across a crash
// or restart.
//
// The resolver answers a `ContentLookup` by reading back
// the publish's ORIGINAL `queued` event's `details_json`
// blob, which the publisher wrote via `marshalQueuedDetails`
// at `Publish` / `Retry` time.  That blob is the
// self-contained snapshot of `Content`, `SignatureOnly`, and
// `EmbeddingModelVersion` the publisher used; combined with
// the `Kind` / `RepoID` / `CanonicalSignature` the flusher
// already populated from the `node` JOIN, the resolver
// reconstructs a `PublishRequest` identical (modulo the
// inherently mutable `Content` body) to the originating
// publish.
//
// Model-version drift guard
// -------------------------
// The resolver compares `lookup.ModelVersion` (which the
// flusher took from `embedding_publish.embedding_model_version`)
// against the `embedding_model_version` field inside the
// queued event's snapshot.  Disagreement is impossible by
// construction today — both fields are written from the
// same `modelVersion` variable inside `Publisher.Publish` /
// `Publisher.Retry` — but the resolver still treats a
// mismatch as `ErrSupersededByModel` rather than silently
// retrying with the wrong model.  This makes a future
// schema change (e.g. operator manual UPDATE of the
// publish row's model version after a re-train) safe by
// default.
//
// Latest-queued semantics
// -----------------------
// When `Publisher.Retry` runs, it appends a FRESH `queued`
// event with a higher `attempt_index`.  The resolver reads
// the LATEST queued event (highest `attempt_index`, latest
// `created_at`) so a retry-after-retry chain picks up the
// most recent body shape.  Without this, an operator that
// patched the request body between attempts would see the
// old body re-embedded.
type PublishEventContentResolver struct {
	db *sql.DB
}

// NewPublishEventContentResolver constructs a resolver over
// `db`.  The `db` should be the `agent_memory_app` role
// connection: the resolver only reads, but the flusher that
// owns it ALSO writes events via the embedded `*Publisher`,
// and reusing one connection pool keeps the operator's
// connection budget tight.
//
// Panics on nil `db` — a silent no-op resolver would leave
// the flusher's `Stats.ResolveErrors` counter climbing
// forever with no visible cause.
func NewPublishEventContentResolver(db *sql.DB) *PublishEventContentResolver {
	if db == nil {
		panic("embedding: NewPublishEventContentResolver: nil *sql.DB")
	}
	return &PublishEventContentResolver{db: db}
}

// Resolve is the `ContentResolver.Resolve` implementation.
// The contract is documented on the interface; in summary:
//
//   - Returns a `PublishRequest` whose `Content`,
//     `SignatureOnly`, and `ModelVersion` come from the
//     persisted snapshot, and whose `NodeID`, `RepoID`,
//     `Kind`, `CanonicalSignature` come from the
//     `ContentLookup` (which the flusher populated from
//     the `node` JOIN).
//   - Returns `ErrSupersededByModel` when the snapshot's
//     model version disagrees with the lookup's model
//     version.
//   - Returns a non-sentinel error when the queued event
//     is missing entirely or its `details_json` is NULL /
//     malformed.  Missing details is the resolver's signal
//     that the publish row predates the snapshot rollout
//     (no migration was done; legacy rows have NULL
//     details_json); the flusher logs and counts these as
//     `ResolveErrors`.
func (r *PublishEventContentResolver) Resolve(ctx context.Context, lookup ContentLookup) (PublishRequest, error) {
	if strings.TrimSpace(lookup.PublishID) == "" {
		return PublishRequest{}, errors.New(
			"embedding: PublishEventContentResolver.Resolve: empty PublishID")
	}
	if strings.TrimSpace(lookup.NodeID) == "" {
		return PublishRequest{}, errors.New(
			"embedding: PublishEventContentResolver.Resolve: empty NodeID")
	}

	// Read the LATEST `queued` event for this publish.  A
	// `Retry` writes a fresh `queued` row with a higher
	// `attempt_index`; if the retry path is in flight, the
	// fresh body shape is the one we want to re-embed.
	// `details_json IS NOT NULL` filters out any legacy
	// queued rows from before the snapshot rollout.
	const q = `
		SELECT details_json::text
		FROM embedding_publish_event
		WHERE publish_id = $1
		  AND event_kind = 'queued'
		  AND details_json IS NOT NULL
		ORDER BY attempt_index DESC, created_at DESC, event_id DESC
		LIMIT 1
	`
	var raw string
	if err := r.db.QueryRowContext(ctx, q, lookup.PublishID).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PublishRequest{}, fmt.Errorf(
				"embedding: PublishEventContentResolver: no queued event with details_json "+
					"for publish_id %s (legacy row or wiring bug)", lookup.PublishID)
		}
		return PublishRequest{}, fmt.Errorf(
			"embedding: PublishEventContentResolver: scan details_json: %w", err)
	}

	var snap queuedEventDetails
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return PublishRequest{}, fmt.Errorf(
			"embedding: PublishEventContentResolver: decode details_json for publish_id %s: %w",
			lookup.PublishID, err)
	}

	// Snapshot-driven rows carry no `content` field in their
	// queued snapshot — the §7.4 mgmt.snapshot handler writes
	// `{snapshot_id, source, embedding_model_version}` only
	// because the snapshot enqueuer has no source bytes
	// in-process.  The §9.6a re-embed contract still needs
	// real content for the Embedder call, so before refusing
	// an empty-content snapshot we attempt a fallback lookup
	// against the SAME `node_id`: find the most recent
	// `queued` event with non-empty content whose owning
	// publish reached `published`.  That guarantees the
	// fallback content is the version the recall path is
	// currently serving — exactly the body the snapshot
	// wants to re-embed under the active model.
	//
	// We keep the snapshot row's model metadata: the model-
	// drift checks below compare against `rowModel`, which
	// is the snapshot publish's recorded version (== current
	// embedder by construction).  Only `Content` and
	// `SignatureOnly` are copied from the fallback row.
	//
	// The `source` field is decoded from the same
	// `queuedEventDetails` struct (omitempty so the
	// publisher's normal-path queued events do not write
	// `source` at all and the field stays empty there).
	if snap.Content == "" {
		if snap.Source == snapshotSource {
			fallback, ferr := r.resolveSnapshotFallback(ctx, lookup)
			if ferr != nil {
				return PublishRequest{}, fmt.Errorf(
					"embedding: PublishEventContentResolver: empty content in queued snapshot "+
						"for publish_id %s; fallback lookup failed: %w",
					lookup.PublishID, ferr)
			}
			snap.Content = fallback.Content
			snap.SignatureOnly = fallback.SignatureOnly
		}
	}
	if snap.Content == "" {
		// An empty content snapshot means the publisher
		// recorded the queued event with no body — a
		// possible (but undesirable) shape if a future
		// operator hand-inserts a queued row.  Refuse to
		// retry with empty content; an empty embedding
		// would silently corrupt the recall index.
		return PublishRequest{}, fmt.Errorf(
			"embedding: PublishEventContentResolver: empty content in queued snapshot "+
				"for publish_id %s", lookup.PublishID)
	}

	// Operator-current model drift gate.  When the flusher
	// populates `lookup.CurrentModelVersion` from
	// `Publisher.ModelVersion()`, surface a supersede
	// signal here for callers that drive `Resolve` outside
	// the Flusher's pre-check (the Flusher itself
	// short-circuits BEFORE calling the resolver — see
	// `flusher.go` row loop — so this branch is
	// defence-in-depth for ad-hoc resolver callers).
	//
	// When `CurrentModelVersion` is the empty string the
	// caller has not opted into the gate (e.g. a test
	// driving the resolver in isolation), so the check is
	// skipped — emitting a supersede in that case would
	// surprise tests that don't model a current embedder.
	currentModel := strings.TrimSpace(lookup.CurrentModelVersion)
	rowModel := strings.TrimSpace(lookup.ModelVersion)
	if currentModel != "" && rowModel != "" && currentModel != rowModel {
		return PublishRequest{}, fmt.Errorf(
			"%w: publish_id %s row model %q != current embedder model %q",
			ErrSupersededByModel, lookup.PublishID,
			rowModel, currentModel)
	}

	// Snapshot-vs-publish-row model drift gate.  Today both
	// halves are written from the same `modelVersion` so
	// disagreement is impossible by construction, but this
	// check is the §9.6 defence against a future operator
	// UPDATE of the publish row's model column.
	if rowModel != "" &&
		strings.TrimSpace(snap.EmbeddingModelVersion) != "" &&
		rowModel != snap.EmbeddingModelVersion {
		return PublishRequest{}, fmt.Errorf(
			"%w: publish_id %s snapshot model %q != publish-row model %q",
			ErrSupersededByModel, lookup.PublishID,
			snap.EmbeddingModelVersion, rowModel)
	}

	return PublishRequest{
		NodeID:             lookup.NodeID,
		RepoID:             lookup.RepoID,
		Kind:               lookup.Kind,
		CanonicalSignature: lookup.CanonicalSignature,
		Content:            snap.Content,
		SignatureOnly:      snap.SignatureOnly,
	}, nil
}

// snapshotSource is the literal `details_json->>'source'`
// value the §7.4 mgmt.snapshot handler writes (see
// `internal/mgmtapi/handler_snapshot.go::buildSnapshotDetailsJSON`).
// Resolver-side classifier scans for this exact string to
// distinguish snapshot-enqueued queued events (which never
// carry content) from publisher-written queued events (which
// always do).
const snapshotSource = "mgmt.snapshot"

// resolveSnapshotFallback finds the most recent `queued`
// event with non-empty content whose owning `embedding_publish`
// row (a) targets the SAME `node_id` as the snapshot row, (b)
// is NOT the snapshot row itself, and (c) reached the §9.6a
// `published` terminal event (its latest event_kind is
// `published`).  The latest-published predicate ensures we
// only fall back to content that was actually served by the
// recall path, never to content from a failed or in-flight
// publish.
//
// The lateral subquery uses the canonical (created_at DESC,
// event_id DESC) tie-break shared by the rest of the §9.6a
// machinery (publisher.go's supersede CTE, flusher.go's
// stuck-row scan, promoter.go's selectStalled).
func (r *PublishEventContentResolver) resolveSnapshotFallback(
	ctx context.Context,
	lookup ContentLookup,
) (queuedEventDetails, error) {
	const q = `
		SELECT e.details_json::text
		  FROM embedding_publish_event e
		  JOIN embedding_publish      p ON p.publish_id = e.publish_id
		  CROSS JOIN LATERAL (
		      SELECT epe.event_kind
		        FROM embedding_publish_event epe
		       WHERE epe.publish_id = p.publish_id
		       ORDER BY epe.created_at DESC, epe.event_id DESC
		       LIMIT 1
		  ) latest
		 WHERE p.node_id              = $1::uuid
		   AND p.publish_id          <> $2::uuid
		   AND e.event_kind           = 'queued'
		   AND e.details_json         IS NOT NULL
		   AND coalesce(e.details_json->>'content', '') <> ''
		   AND latest.event_kind      = 'published'
		 ORDER BY e.created_at DESC, e.event_id DESC
		 LIMIT 1
	`
	var raw string
	err := r.db.QueryRowContext(ctx, q, lookup.NodeID, lookup.PublishID).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return queuedEventDetails{}, fmt.Errorf(
				"no prior published row for node_id %s carries content the snapshot can re-embed",
				lookup.NodeID)
		}
		return queuedEventDetails{}, fmt.Errorf("scan fallback details_json: %w", err)
	}
	var snap queuedEventDetails
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return queuedEventDetails{}, fmt.Errorf("decode fallback details_json: %w", err)
	}
	return snap, nil
}
