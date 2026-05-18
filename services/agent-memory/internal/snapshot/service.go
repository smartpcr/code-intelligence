package snapshot

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/embedding"
)

// -----------------------------------------------------------
// Public surface
// -----------------------------------------------------------

// Result is the per-call summary the `mgmt.snapshot` handler
// returns to the operator. The counts are split by target
// kind so an operator who triggered snapshot after a model
// bump can confirm the expected fan-out and notice an
// unusually small "ConceptsEnqueued" relative to the repo's
// known concept count.
type Result struct {
	// SnapshotID is the opaque token Service mints on every
	// successful Enqueue and stamps into the queued event's
	// `details_json` (see [QueuedDetailsKeySnapshotID]). The
	// same id flows back to the operator so a downstream
	// observability stack can correlate the verb call with
	// the eventual `published` event rows in the audit log.
	SnapshotID string
	// ModelVersion is the `embedding_model_version` value the
	// new publish rows were stamped with — copied verbatim
	// from [Service.modelVersion]. The same string lands on
	// every new `embedding_publish` row this call wrote.
	ModelVersion string
	// MethodsEnqueued is the count of fresh
	// `embedding_publish` rows the Service wrote for
	// `kind='method'` Nodes in the repo. Zero is legal (e.g.
	// a freshly-registered repo whose Methods have not yet
	// reached a `published` publish row).
	MethodsEnqueued int
	// BlocksEnqueued mirrors MethodsEnqueued for
	// `kind='block'` Nodes.
	BlocksEnqueued int
	// ConceptsEnqueued is the count of fresh
	// `embedding_publish` rows the Service wrote for promoted
	// `concept_version` rows whose support is attributable to
	// the repo (via `concept_support.repo_id`).
	ConceptsEnqueued int
	// MethodBlocksSkipped is the count of Method/Block targets
	// the Service deliberately skipped because their prior
	// publish already had a non-terminal snapshot replacement
	// queued (the dedupe gate the §6.2.1 verb honours when
	// the operator double-clicks the snapshot button).
	// Surfaced so the operator can distinguish "no targets"
	// from "snapshot already in flight".
	MethodBlocksSkipped int
	// ConceptsSkipped mirrors MethodBlocksSkipped for concept
	// targets.
	ConceptsSkipped int
}

// TotalEnqueued returns the sum of method + block + concept
// enqueue counts. Useful for metrics and the operator-facing
// "202 Accepted" envelope.
func (r Result) TotalEnqueued() int {
	return r.MethodsEnqueued + r.BlocksEnqueued + r.ConceptsEnqueued
}

// TotalSkipped is the symmetric sum of skipped targets.
func (r Result) TotalSkipped() int {
	return r.MethodBlocksSkipped + r.ConceptsSkipped
}

// Service is the production implementation of the snapshot
// enqueue protocol. Construct via [New]; the returned value
// is safe for concurrent use across goroutines (the
// underlying *sql.DB is, and Service holds no per-call
// mutable state).
type Service struct {
	db           *sql.DB
	modelVersion string
	logger       *slog.Logger
	metrics      *Metrics

	// now is the wall-clock the Service uses for structured
	// log timestamps. PostgreSQL columns default to `now()`
	// server-side so a frozen Go-side clock does NOT shift
	// DB timestamps; tests inject one to assert log shape.
	now func() time.Time

	// newSnapshotID is overridable so unit tests can pin the
	// minted `snapshot_id` against a deterministic value
	// without parsing the response. Production passes nil →
	// defaults to a 16-byte hex token.
	newSnapshotID func() (string, error)
}

// Option configures a Service. Functional-options shape
// mirrors `embedding.Option` / `mgmtapi.Options`.
type Option func(*Service)

// WithLogger overrides the structured logger. Defaults to
// [slog.Default].
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithMetrics overrides the Metrics sink. Defaults to a
// freshly-constructed [Metrics] so production binaries that
// forget to wire one still get observable counters.
func WithMetrics(m *Metrics) Option {
	return func(s *Service) {
		if m != nil {
			s.metrics = m
		}
	}
}

// WithClock overrides the wall-clock the Service uses for
// log timestamps. Defaults to [time.Now].
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// WithSnapshotIDFactory overrides the snapshot id minter.
// Useful for tests that need a deterministic id.
func WithSnapshotIDFactory(fn func() (string, error)) Option {
	return func(s *Service) {
		if fn != nil {
			s.newSnapshotID = fn
		}
	}
}

// New constructs a Service. Panics on a nil *sql.DB or an
// empty `modelVersion` — both are configuration errors the
// operator should see at boot, not at the first snapshot
// call. (The snapshot verb's whole point is to re-embed at
// the CURRENTLY active model version; an empty version
// would silently widen the §9.6 model-drift risk surface.)
func New(db *sql.DB, modelVersion string, opts ...Option) *Service {
	if db == nil {
		panic("snapshot: New: nil *sql.DB")
	}
	if strings.TrimSpace(modelVersion) == "" {
		panic("snapshot: New: modelVersion is required (risk §9.6 forbids unversioned snapshots)")
	}
	s := &Service{
		db:            db,
		modelVersion:  strings.TrimSpace(modelVersion),
		logger:        slog.Default(),
		metrics:       NewMetrics(),
		now:           time.Now,
		newSnapshotID: defaultSnapshotID,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Metrics returns the Service's Metrics sink. Exposed so the
// `cmd/mgmt-api` binary's /metrics scraper can read counter
// values without poking package-private state.
func (s *Service) Metrics() *Metrics { return s.metrics }

// ModelVersion returns the embedding-model version the Service
// stamps onto every new `embedding_publish` row it writes.
// Exposed so the handler can include it in the 202 envelope
// and the operator-facing audit log can attribute the call.
func (s *Service) ModelVersion() string { return s.modelVersion }

// ErrRepoNotFound is the sentinel Snapshot returns when the
// supplied `repoID` is a well-formed UUID but no `repo` row
// matches. The HTTP handler maps it to 404 +
// `repo_not_found`.
var ErrRepoNotFound = errors.New("snapshot: repo not found")

// -----------------------------------------------------------
// QueuedDetailsKey discriminators
// -----------------------------------------------------------

// QueuedDetailsKeySnapshotID is the JSONB key the snapshot
// service stamps onto every queued event it writes. Exported
// so the publisher's transition-to-published hook and any
// future operator-facing tooling can dispatch on the same
// stable name without typo risk. The value is the
// [Result.SnapshotID] minted on the enclosing Snapshot()
// call.
const QueuedDetailsKeySnapshotID = "snapshot_id"

// QueuedDetailsKeySupersedesPublishID is the JSONB key the
// snapshot service stamps onto every queued event it writes,
// pointing at the prior `published` publish row this new
// publish is intended to replace. The publisher's
// transition-to-published hook reads this key inside the
// SAME transaction it inserts the `published` event into
// and emits a matching `superseded` event for the prior
// publish — so recall never sees two `published` rows for
// the same Node / ConceptVersion.
const QueuedDetailsKeySupersedesPublishID = "supersedes_publish_id"

// -----------------------------------------------------------
// Snapshot — public entry point
// -----------------------------------------------------------

// Snapshot enqueues fresh `embedding_publish` rows + queued
// events for every Method/Block Node and every promoted
// Concept currently attributable to `repoID`. See package
// doc for the full §6.2.1 contract.
//
// Returns [ErrRepoNotFound] when `repoID` is well-formed but
// unknown. Any other error is a transient DB / wiring
// failure — callers MUST treat it as 500-class and retry.
//
// The method is NOT a transaction: each per-target enqueue
// runs in its own short transaction. The trade-off is
// granularity (a partial failure on row N+1 does not erase
// the N already-committed enqueues) versus atomicity (an
// operator-visible "82 of 100 enqueued" outcome is possible
// on a mid-call PG outage). Granularity is the right choice
// because the dedupe gate makes a retry safe: a second
// `mgmt.snapshot` call resumes where the first stopped.
func (s *Service) Snapshot(ctx context.Context, repoID string) (Result, error) {
	repoID = strings.TrimSpace(repoID)
	if repoID == "" {
		return Result{}, errors.New("snapshot: repoID is required")
	}

	if err := s.assertRepoExists(ctx, repoID); err != nil {
		return Result{}, err
	}

	snapshotID, err := s.newSnapshotID()
	if err != nil {
		return Result{}, fmt.Errorf("snapshot: mint snapshot_id: %w", err)
	}

	result := Result{
		SnapshotID:   snapshotID,
		ModelVersion: s.modelVersion,
	}

	// Method/Block targets first — typically the dominant
	// fan-out on a real repo. Each target runs in its own
	// short transaction; the per-target dedupe gate inside
	// enqueueNodeTarget makes a retry safe.
	methodCount, blockCount, mbSkipped, err := s.enqueueNodeTargets(ctx, repoID, snapshotID)
	if err != nil {
		return result, fmt.Errorf("snapshot: enqueue node targets: %w", err)
	}
	result.MethodsEnqueued = methodCount
	result.BlocksEnqueued = blockCount
	result.MethodBlocksSkipped = mbSkipped

	conceptCount, conceptSkipped, err := s.enqueueConceptTargets(ctx, repoID, snapshotID)
	if err != nil {
		// We already wrote the node-target rows; surface
		// the partial count alongside the error so the
		// operator can see how far the call got.
		return result, fmt.Errorf("snapshot: enqueue concept targets: %w", err)
	}
	result.ConceptsEnqueued = conceptCount
	result.ConceptsSkipped = conceptSkipped

	s.logger.Info("snapshot.enqueue.ok",
		slog.String("op", "snapshot"),
		slog.String("snapshot_id", snapshotID),
		slog.String("repo_id", repoID),
		slog.String("model_version", s.modelVersion),
		slog.Int("methods_enqueued", result.MethodsEnqueued),
		slog.Int("blocks_enqueued", result.BlocksEnqueued),
		slog.Int("concepts_enqueued", result.ConceptsEnqueued),
		slog.Int("targets_skipped", result.TotalSkipped()),
		slog.Time("at", s.now()),
	)
	return result, nil
}

// -----------------------------------------------------------
// Repo existence check
// -----------------------------------------------------------

// assertRepoExists returns [ErrRepoNotFound] when no row in
// `repo` matches `repoID`. Also catches the PostgreSQL
// `invalid input syntax for type uuid` error and maps it to
// the same sentinel so the handler emits a clean 404 instead
// of a 500 on a malformed id (defence-in-depth — the handler
// also validates the UUID shape up front).
func (s *Service) assertRepoExists(ctx context.Context, repoID string) error {
	const q = `SELECT 1 FROM repo WHERE repo_id = $1::uuid`
	var dummy int
	err := s.db.QueryRowContext(ctx, q, repoID).Scan(&dummy)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrRepoNotFound
	case err != nil:
		if isInvalidUUIDError(err) {
			return ErrRepoNotFound
		}
		return fmt.Errorf("snapshot: assert repo exists: %w", err)
	}
	return nil
}

// -----------------------------------------------------------
// Node (Method/Block) enqueue path
// -----------------------------------------------------------

// nodeTarget is the per-Node row the enumerator yields. It
// carries everything enqueueNodeTarget needs to write the
// new publish row + queued event without a follow-up SELECT.
type nodeTarget struct {
	NodeID              string
	Kind                string
	CanonicalSignature  string
	PriorPublishID      string
	PriorQueuedDetailsJ string
	// InFlight is true when the scan detected that a NEWER
	// `embedding_publish` row already exists for this Node
	// whose queued event carries a `supersedes_publish_id`
	// matching this target's PriorPublishID, AND whose
	// latest event has not yet reached a terminal state
	// (`superseded` or `failed`). Flagged targets are
	// skip-counted by the enqueue loop without even
	// entering the per-target transaction so an operator
	// who double-clicks the snapshot button sees the
	// "already in flight" count instead of being silently
	// omitted. (Iter-2 fix #2.)
	InFlight bool
}

// enqueueNodeTargets enumerates every Method/Block Node in the
// repo that has at least one `published` `embedding_publish`
// row, then writes a fresh publish row + queued event for
// each whose prior publish does NOT already have an in-flight
// snapshot replacement. Returns (method_count, block_count,
// skipped_count, error). The skipped count includes both
// scan-time in-flight detection AND per-target dedupe-gate
// hits inside the tx — they are operationally equivalent for
// the operator's "is the snapshot complete?" question.
//
// The enumerate-then-enqueue split keeps the long-running
// scan outside the per-target transactions; the dedupe gate
// inside enqueueNodeTarget closes the small race window
// where two concurrent `mgmt.snapshot` calls picked the same
// target between the SELECT and the INSERT.
func (s *Service) enqueueNodeTargets(ctx context.Context, repoID, snapshotID string) (int, int, int, error) {
	targets, err := s.scanNodeTargets(ctx, repoID)
	if err != nil {
		return 0, 0, 0, err
	}

	methodCount := 0
	blockCount := 0
	skipped := 0
	for _, t := range targets {
		// Iter-2 fix #2: count scan-detected in-flight
		// replacements as skipped rather than silently
		// omitting them from the result envelope. The
		// operator who double-clicks the snapshot button
		// sees the full target population accounted for.
		if t.InFlight {
			skipped++
			s.logger.Info("snapshot.node.skip.in_flight",
				slog.String("op", "snapshot"),
				slog.String("snapshot_id", snapshotID),
				slog.String("repo_id", repoID),
				slog.String("node_id", t.NodeID),
				slog.String("prior_publish_id", t.PriorPublishID),
			)
			continue
		}
		ok, err := s.enqueueNodeTarget(ctx, repoID, snapshotID, t)
		if err != nil {
			return methodCount, blockCount, skipped, err
		}
		if !ok {
			skipped++
			continue
		}
		switch t.Kind {
		case embedding.NodeKindMethod:
			methodCount++
		case embedding.NodeKindBlock:
			blockCount++
		}
		s.metrics.IncPending(1)
	}
	return methodCount, blockCount, skipped, nil
}

// scanNodeTargets returns the eligible Method/Block targets
// for the repo. The query joins each Node to its LATEST-
// `published` `embedding_publish` row (the row whose latest
// event reached `published`, even if a NEWER snapshot
// replacement is already in flight) and to the queued event
// that publish wrote (which carries the body content the
// snapshot needs to re-enqueue). Each row also indicates
// whether an in-flight replacement already exists so the
// enqueue loop can count it as skipped instead of silently
// omitting it (iter-2 evaluator fix #2).
//
// Iter-2 fix #2: the earlier shape filtered nodes whose
// LATEST publish event was `published` — so a node with an
// already-queued replacement was OMITTED from the scan
// rather than counted as skipped. The new shape finds the
// most recent publish whose latest event reached `published`
// regardless of whether a NEWER publish exists, and joins a
// per-prior in-flight detector that flags the row when a
// non-terminal supersede is in progress. The enqueue loop
// then skip-counts flagged rows.
//
// NOTE on partition scan cost: `embedding_publish` and
// `embedding_publish_event` are monthly-partitioned
// (migration 0015). The LATERAL latest-event lookup keys
// off the (publish_id, created_at DESC, event_id DESC)
// index defined in 0015 and propagated to every partition;
// pruning does NOT engage because the predicate is
// publish_id rather than created_at. This is acceptable for
// an operator-triggered verb whose call rate is human-scale.
func (s *Service) scanNodeTargets(ctx context.Context, repoID string) ([]nodeTarget, error) {
	const q = `
		SELECT
		    n.node_id::text,
		    n.kind::text,
		    n.canonical_signature,
		    last_pub.publish_id::text,
		    coalesce(qe.details_json::text, ''),
		    in_flight.exists_flag IS NOT NULL AS in_flight
		FROM node n
		JOIN LATERAL (
		    SELECT ep.publish_id, ep.created_at
		    FROM embedding_publish ep
		    JOIN LATERAL (
		        SELECT event_kind
		        FROM embedding_publish_event
		        WHERE publish_id = ep.publish_id
		        ORDER BY created_at DESC, event_id DESC
		        LIMIT 1
		    ) latest_inner ON true
		    WHERE ep.node_id = n.node_id
		      AND latest_inner.event_kind = 'published'
		    ORDER BY ep.created_at DESC
		    LIMIT 1
		) last_pub ON true
		LEFT JOIN LATERAL (
		    SELECT details_json
		    FROM embedding_publish_event
		    WHERE publish_id = last_pub.publish_id
		      AND event_kind = 'queued'
		      AND details_json IS NOT NULL
		    ORDER BY attempt_index ASC, created_at ASC, event_id ASC
		    LIMIT 1
		) qe ON true
		LEFT JOIN LATERAL (
		    SELECT 1 AS exists_flag
		    FROM embedding_publish ep_new
		    JOIN LATERAL (
		        SELECT details_json
		        FROM embedding_publish_event
		        WHERE publish_id = ep_new.publish_id
		          AND event_kind = 'queued'
		          AND details_json IS NOT NULL
		        ORDER BY attempt_index ASC, created_at ASC, event_id ASC
		        LIMIT 1
		    ) qenew ON true
		    JOIN LATERAL (
		        SELECT event_kind
		        FROM embedding_publish_event
		        WHERE publish_id = ep_new.publish_id
		        ORDER BY created_at DESC, event_id DESC
		        LIMIT 1
		    ) latnew ON true
		    WHERE ep_new.node_id = n.node_id
		      AND qenew.details_json ->> 'supersedes_publish_id' = last_pub.publish_id::text
		      AND latnew.event_kind NOT IN ('superseded', 'failed')
		    LIMIT 1
		) in_flight ON true
		WHERE n.repo_id = $1::uuid
		  AND n.kind IN ('method', 'block')
	`
	rows, err := s.db.QueryContext(ctx, q, repoID)
	if err != nil {
		return nil, fmt.Errorf("snapshot: scan node targets: %w", err)
	}
	defer rows.Close()

	var out []nodeTarget
	for rows.Next() {
		var t nodeTarget
		if err := rows.Scan(&t.NodeID, &t.Kind, &t.CanonicalSignature,
			&t.PriorPublishID, &t.PriorQueuedDetailsJ, &t.InFlight); err != nil {
			return nil, fmt.Errorf("snapshot: scan node row: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("snapshot: iter node rows: %w", err)
	}
	return out, nil
}

// enqueueNodeTarget writes the new publish + queued event for
// ONE Node target inside its own transaction. Returns
// (enqueued, error) where `enqueued=false` means the dedupe
// gate caught a concurrent snapshot in flight.
//
// The dedupe gate executes inside the transaction so the
// "already in flight" check and the INSERTs are atomic
// against a concurrent snapshot call.
func (s *Service) enqueueNodeTarget(ctx context.Context, repoID, snapshotID string, t nodeTarget) (bool, error) {
	priorDetails, err := decodePriorQueuedDetails(t.PriorQueuedDetailsJ)
	if err != nil {
		return false, fmt.Errorf("snapshot: decode prior details for node %s: %w", t.NodeID, err)
	}
	if priorDetails.Content == "" {
		// The Node has a `published` publish but the
		// publisher's queued snapshot is missing — this is
		// the "legacy row" path the §9.6a content resolver
		// also rejects. We cannot reconstruct the source
		// body without a re-index; skip and surface as a
		// "skipped" count so the operator sees the
		// under-coverage.
		s.logger.Warn("snapshot.skip.no_prior_content",
			slog.String("op", "snapshot"),
			slog.String("node_id", t.NodeID),
			slog.String("prior_publish_id", t.PriorPublishID),
		)
		return false, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("snapshot: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Iter-2 fix #1: advisory transaction lock keyed on the
	// prior_publish_id serializes ALL concurrent snapshot
	// callers attempting to supersede the SAME prior. The
	// lock is released on tx commit/rollback (PG built-in
	// semantics) so concurrent enqueues for DIFFERENT
	// priors run in parallel. Under READ COMMITTED isolation
	// the dedupe-probe query below would otherwise miss a
	// concurrent INSERT committed between two callers'
	// statement snapshots; the advisory lock makes the
	// supersede operation a strict serialization point.
	//
	// We use the prior_publish_id (not node_id) as the lock
	// key because the supersede contract is publish-row-
	// scoped: two callers racing to supersede different
	// publish rows for the same node (rare but possible
	// across model bumps) should not serialize. The
	// `hashtextextended` wrapper returns a 64-bit hash that
	// fills the bigint advisory-lock keyspace.
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('snapshot:supersede:' || $1::text, 0))`,
		t.PriorPublishID,
	); err != nil {
		return false, fmt.Errorf("snapshot: acquire supersede lock: %w", err)
	}

	// Dedupe gate: refuse to enqueue if the prior publish's
	// latest event already points at another snapshot
	// replacement that has NOT terminated. We look for a
	// newer `embedding_publish` for the same Node whose
	// queued event carries a `supersedes_publish_id` equal
	// to our prior — that's our marker that another snapshot
	// is already in flight.
	const dedupeQ = `
		SELECT 1
		FROM embedding_publish ep
		JOIN LATERAL (
		    SELECT details_json
		    FROM embedding_publish_event
		    WHERE publish_id = ep.publish_id
		      AND event_kind = 'queued'
		      AND details_json IS NOT NULL
		    ORDER BY attempt_index ASC, created_at ASC, event_id ASC
		    LIMIT 1
		) qe ON true
		JOIN LATERAL (
		    SELECT event_kind
		    FROM embedding_publish_event
		    WHERE publish_id = ep.publish_id
		    ORDER BY created_at DESC, event_id DESC
		    LIMIT 1
		) latest ON true
		WHERE ep.node_id = $1::uuid
		  AND qe.details_json ->> 'supersedes_publish_id' = $2
		  AND latest.event_kind NOT IN ('superseded', 'failed')
		LIMIT 1
	`
	var dup int
	err = tx.QueryRowContext(ctx, dedupeQ, t.NodeID, t.PriorPublishID).Scan(&dup)
	switch {
	case err == nil:
		// Another snapshot is already in flight for this
		// target.
		return false, nil
	case errors.Is(err, sql.ErrNoRows):
		// No duplicate — proceed.
	default:
		return false, fmt.Errorf("snapshot: dedupe probe: %w", err)
	}

	pointID, err := embedding.NewUUIDv4()
	if err != nil {
		return false, fmt.Errorf("snapshot: mint point_id: %w", err)
	}

	const insertPublishQ = `
		INSERT INTO embedding_publish
		    (node_id, embedding_model_version, qdrant_point_id)
		VALUES ($1::uuid, $2, $3::uuid)
		RETURNING publish_id::text
	`
	var newPublishID string
	if err := tx.QueryRowContext(ctx, insertPublishQ,
		t.NodeID, s.modelVersion, pointID,
	).Scan(&newPublishID); err != nil {
		return false, fmt.Errorf("snapshot: insert embedding_publish: %w", err)
	}

	queuedDetails := buildQueuedDetails(priorDetails.Content, priorDetails.SignatureOnly,
		s.modelVersion, snapshotID, t.PriorPublishID)
	raw, err := json.Marshal(queuedDetails)
	if err != nil {
		return false, fmt.Errorf("snapshot: marshal queued details: %w", err)
	}
	const insertEventQ = `
		INSERT INTO embedding_publish_event
		    (publish_id, event_kind, attempt_index, details_json)
		VALUES ($1::uuid, 'queued'::embedding_publish_event_kind, 0, $2::jsonb)
	`
	if _, err := tx.ExecContext(ctx, insertEventQ, newPublishID, string(raw)); err != nil {
		return false, fmt.Errorf("snapshot: insert queued event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("snapshot: commit node enqueue: %w", err)
	}

	s.logger.Info("snapshot.node.enqueued",
		slog.String("op", "snapshot"),
		slog.String("snapshot_id", snapshotID),
		slog.String("repo_id", repoID),
		slog.String("node_id", t.NodeID),
		slog.String("kind", t.Kind),
		slog.String("new_publish_id", newPublishID),
		slog.String("prior_publish_id", t.PriorPublishID),
	)
	return true, nil
}

// -----------------------------------------------------------
// Concept enqueue path
// -----------------------------------------------------------

// conceptTarget mirrors nodeTarget for promoted concept rows.
// Concepts are cross-repo (G6) so the repo scope is provided
// via concept_support.repo_id rather than a column on
// concept_version itself.
type conceptTarget struct {
	ConceptVersionID    string
	CanonicalSignature  string
	PriorPublishID      string
	PriorQueuedDetailsJ string
	// InFlight mirrors [nodeTarget.InFlight] — true when a
	// newer non-terminal concept-supersede publish already
	// exists. (Iter-2 fix #2.)
	InFlight bool
}

func (s *Service) enqueueConceptTargets(ctx context.Context, repoID, snapshotID string) (int, int, error) {
	targets, err := s.scanConceptTargets(ctx, repoID)
	if err != nil {
		return 0, 0, err
	}
	count := 0
	skipped := 0
	for _, t := range targets {
		if t.InFlight {
			skipped++
			s.logger.Info("snapshot.concept.skip.in_flight",
				slog.String("op", "snapshot"),
				slog.String("snapshot_id", snapshotID),
				slog.String("repo_id", repoID),
				slog.String("concept_version_id", t.ConceptVersionID),
				slog.String("prior_publish_id", t.PriorPublishID),
			)
			continue
		}
		ok, err := s.enqueueConceptTarget(ctx, repoID, snapshotID, t)
		if err != nil {
			return count, skipped, err
		}
		if !ok {
			skipped++
			continue
		}
		count++
		s.metrics.IncPending(1)
	}
	return count, skipped, nil
}

// scanConceptTargets enumerates promoted concept_version rows
// attributable to `repoID`. Attribution is via concept_support
// (architecture §5.5.3) since concept_version has no repo_id.
// DISTINCT keeps the result one-row-per-cv even when the
// concept has multiple support rows. Mirrors the node-scan's
// last-published shape so an already-in-flight concept
// supersede is counted as skipped, not silently omitted.
func (s *Service) scanConceptTargets(ctx context.Context, repoID string) ([]conceptTarget, error) {
	const q = `
		SELECT DISTINCT
		    cv.concept_version_id::text,
		    coalesce(c.name, '')        AS canonical_signature,
		    last_pub.publish_id::text,
		    coalesce(qe.details_json::text, ''),
		    in_flight.exists_flag IS NOT NULL AS in_flight
		FROM concept_version cv
		JOIN concept c ON c.concept_id = cv.concept_id
		JOIN concept_support cs
		    ON cs.concept_version_id = cv.concept_version_id
		   AND cs.repo_id = $1::uuid
		JOIN LATERAL (
		    SELECT ep.publish_id, ep.created_at
		    FROM embedding_publish ep
		    JOIN LATERAL (
		        SELECT event_kind
		        FROM embedding_publish_event
		        WHERE publish_id = ep.publish_id
		        ORDER BY created_at DESC, event_id DESC
		        LIMIT 1
		    ) latest_inner ON true
		    WHERE ep.concept_version_id = cv.concept_version_id
		      AND latest_inner.event_kind = 'published'
		    ORDER BY ep.created_at DESC
		    LIMIT 1
		) last_pub ON true
		LEFT JOIN LATERAL (
		    SELECT details_json
		    FROM embedding_publish_event
		    WHERE publish_id = last_pub.publish_id
		      AND event_kind = 'queued'
		      AND details_json IS NOT NULL
		    ORDER BY attempt_index ASC, created_at ASC, event_id ASC
		    LIMIT 1
		) qe ON true
		LEFT JOIN LATERAL (
		    SELECT 1 AS exists_flag
		    FROM embedding_publish ep_new
		    JOIN LATERAL (
		        SELECT details_json
		        FROM embedding_publish_event
		        WHERE publish_id = ep_new.publish_id
		          AND event_kind = 'queued'
		          AND details_json IS NOT NULL
		        ORDER BY attempt_index ASC, created_at ASC, event_id ASC
		        LIMIT 1
		    ) qenew ON true
		    JOIN LATERAL (
		        SELECT event_kind
		        FROM embedding_publish_event
		        WHERE publish_id = ep_new.publish_id
		        ORDER BY created_at DESC, event_id DESC
		        LIMIT 1
		    ) latnew ON true
		    WHERE ep_new.concept_version_id = cv.concept_version_id
		      AND qenew.details_json ->> 'supersedes_publish_id' = last_pub.publish_id::text
		      AND latnew.event_kind NOT IN ('superseded', 'failed')
		    LIMIT 1
		) in_flight ON true
		WHERE cv.promoted = true
	`
	rows, err := s.db.QueryContext(ctx, q, repoID)
	if err != nil {
		return nil, fmt.Errorf("snapshot: scan concept targets: %w", err)
	}
	defer rows.Close()

	var out []conceptTarget
	for rows.Next() {
		var t conceptTarget
		if err := rows.Scan(&t.ConceptVersionID, &t.CanonicalSignature,
			&t.PriorPublishID, &t.PriorQueuedDetailsJ, &t.InFlight); err != nil {
			return nil, fmt.Errorf("snapshot: scan concept row: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("snapshot: iter concept rows: %w", err)
	}
	return out, nil
}

func (s *Service) enqueueConceptTarget(ctx context.Context, repoID, snapshotID string, t conceptTarget) (bool, error) {
	priorDetails, err := decodePriorQueuedDetails(t.PriorQueuedDetailsJ)
	if err != nil {
		return false, fmt.Errorf("snapshot: decode prior concept details for cv %s: %w", t.ConceptVersionID, err)
	}
	// Iter-3 fix #1: the concept-promoter's queued events
	// emit `name` / `description_md` / `fingerprint` —
	// they do NOT emit `content`. The previous gate here
	// (`priorDetails.Content == ""`) therefore dropped
	// every real promoted-Concept prior. The correct gate
	// for a concept prior is "has embeddable concept
	// content" — i.e. name OR description_md is non-empty
	// (matches `buildConceptContent` in the promoter,
	// which would return "(empty concept)" if both were
	// blank). See `priorQueuedDetails.hasConceptContent`.
	if !priorDetails.hasConceptContent() {
		s.logger.Warn("snapshot.skip.no_prior_content",
			slog.String("op", "snapshot"),
			slog.String("concept_version_id", t.ConceptVersionID),
			slog.String("prior_publish_id", t.PriorPublishID),
		)
		return false, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("snapshot: begin concept tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Iter-2 fix #1: per-prior advisory lock — see the
	// matching comment in `enqueueNodeTarget`.
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('snapshot:supersede:' || $1::text, 0))`,
		t.PriorPublishID,
	); err != nil {
		return false, fmt.Errorf("snapshot: acquire concept supersede lock: %w", err)
	}

	const dedupeQ = `
		SELECT 1
		FROM embedding_publish ep
		JOIN LATERAL (
		    SELECT details_json
		    FROM embedding_publish_event
		    WHERE publish_id = ep.publish_id
		      AND event_kind = 'queued'
		      AND details_json IS NOT NULL
		    ORDER BY attempt_index ASC, created_at ASC, event_id ASC
		    LIMIT 1
		) qe ON true
		JOIN LATERAL (
		    SELECT event_kind
		    FROM embedding_publish_event
		    WHERE publish_id = ep.publish_id
		    ORDER BY created_at DESC, event_id DESC
		    LIMIT 1
		) latest ON true
		WHERE ep.concept_version_id = $1::uuid
		  AND qe.details_json ->> 'supersedes_publish_id' = $2
		  AND latest.event_kind NOT IN ('superseded', 'failed')
		LIMIT 1
	`
	var dup int
	err = tx.QueryRowContext(ctx, dedupeQ, t.ConceptVersionID, t.PriorPublishID).Scan(&dup)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, sql.ErrNoRows):
		// proceed
	default:
		return false, fmt.Errorf("snapshot: concept dedupe probe: %w", err)
	}

	pointID, err := embedding.NewUUIDv4()
	if err != nil {
		return false, fmt.Errorf("snapshot: mint concept point_id: %w", err)
	}

	const insertPublishQ = `
		INSERT INTO embedding_publish
		    (concept_version_id, embedding_model_version, qdrant_point_id)
		VALUES ($1::uuid, $2, $3::uuid)
		RETURNING publish_id::text
	`
	var newPublishID string
	if err := tx.QueryRowContext(ctx, insertPublishQ,
		t.ConceptVersionID, s.modelVersion, pointID,
	).Scan(&newPublishID); err != nil {
		return false, fmt.Errorf("snapshot: insert concept embedding_publish: %w", err)
	}

	queuedDetails := buildConceptQueuedDetails(priorDetails,
		s.modelVersion, snapshotID, t.PriorPublishID)
	raw, err := json.Marshal(queuedDetails)
	if err != nil {
		return false, fmt.Errorf("snapshot: marshal concept queued details: %w", err)
	}
	const insertEventQ = `
		INSERT INTO embedding_publish_event
		    (publish_id, event_kind, attempt_index, details_json)
		VALUES ($1::uuid, 'queued'::embedding_publish_event_kind, 0, $2::jsonb)
	`
	if _, err := tx.ExecContext(ctx, insertEventQ, newPublishID, string(raw)); err != nil {
		return false, fmt.Errorf("snapshot: insert concept queued event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("snapshot: commit concept enqueue: %w", err)
	}

	s.logger.Info("snapshot.concept.enqueued",
		slog.String("op", "snapshot"),
		slog.String("snapshot_id", snapshotID),
		slog.String("repo_id", repoID),
		slog.String("concept_version_id", t.ConceptVersionID),
		slog.String("new_publish_id", newPublishID),
		slog.String("prior_publish_id", t.PriorPublishID),
	)
	return true, nil
}

// -----------------------------------------------------------
// JSONB helpers
// -----------------------------------------------------------

// priorQueuedDetails is the union of every shape a prior
// `queued` event JSONB body may take. We need a single
// lenient decoder because the snapshot service reads queued
// events written by TWO different upstreams that emit
// DIFFERENT key sets:
//
//   - Method/Block publishes (internal/embedding/publisher.go's
//     `queuedEventDetails`) emit `content` + `signature_only` +
//     `embedding_model_version`.
//   - Promoted-Concept publishes (internal/promoter/service.go's
//     `queuedEventDetails`) emit `concept_id` +
//     `concept_version_id` + `name` + `description_md` +
//     `fingerprint` + `embedding_model_version`. The promoter
//     does NOT write a `content` key — embedder input is
//     synthesized at runtime from name + description_md via
//     `buildConceptContent`.
//
// Iter-3 fix #1: the prior decoder only knew the node shape
// (`content`/`signature_only`) and therefore decoded EVERY
// promoted-Concept prior into `{Content: ""}`, which then
// tripped the `Content==""` skip in `enqueueConceptTarget`
// and dropped every real concept from the snapshot. We now
// recognize both shapes here; the per-path enqueue picks the
// gate appropriate for its kind.
//
// We duplicate the field set rather than import the
// publisher's / promoter's private types so the dependency
// stays one-way (snapshot → embedding/promoter, never the
// reverse).
type priorQueuedDetails struct {
	// Node-publish shape.
	Content       string `json:"content"`
	SignatureOnly bool   `json:"signature_only"`

	// Promoted-Concept-publish shape.
	ConceptID        string `json:"concept_id"`
	ConceptVersionID string `json:"concept_version_id"`
	Name             string `json:"name"`
	DescriptionMD    string `json:"description_md"`
	Fingerprint      string `json:"fingerprint"`

	// Shared across both shapes.
	EmbeddingModelVersion string `json:"embedding_model_version"`
}

// hasConceptContent is the concept-side analogue of the node
// `Content != ""` gate. We require at least one of `name` /
// `description_md` because the promoter's
// `buildConceptContent` template synthesizes the embedder
// input from those two fields; a row with neither set would
// produce `"(empty concept)"` for the embedder, which is
// indistinguishable from a corrupted prior. Fingerprint
// alone is NOT sufficient — it is a non-text identity hash
// that the embedder cannot meaningfully consume.
func (d priorQueuedDetails) hasConceptContent() bool {
	return strings.TrimSpace(d.Name) != "" ||
		strings.TrimSpace(d.DescriptionMD) != ""
}

// snapshotQueuedDetails is the §9.6a-compliant JSONB shape the
// snapshot service writes for Method/Block (Node) targets.
// First three fields mirror the publisher's existing
// `queuedEventDetails`; the last two are the snapshot
// discriminators the publisher's transition-to-published hook
// reads back.
type snapshotQueuedDetails struct {
	Content               string `json:"content"`
	SignatureOnly         bool   `json:"signature_only"`
	EmbeddingModelVersion string `json:"embedding_model_version"`
	SnapshotID            string `json:"snapshot_id"`
	SupersedesPublishID   string `json:"supersedes_publish_id"`
}

// snapshotConceptQueuedDetails is the §9.6a-compliant JSONB
// shape the snapshot service writes for promoted-Concept
// targets. Iter-3 fix #1: the first six fields preserve the
// promoter's `queuedEventDetails` shape verbatim so the
// concept-promoter resolver can round-trip the publish (it
// reconstructs embedder input via `buildConceptContent(name,
// description_md)`); the last two are the snapshot
// discriminators the promoter's
// `commitConceptPublishedWithSupersede` probe reads back.
//
// Deliberately separate from `snapshotQueuedDetails` (Node
// path) so the two emitted shapes stay shape-specific — a
// mixed body that carries `content:""` AND concept identity
// would invite ambiguous downstream branches.
type snapshotConceptQueuedDetails struct {
	ConceptID             string `json:"concept_id"`
	ConceptVersionID      string `json:"concept_version_id"`
	Name                  string `json:"name"`
	DescriptionMD         string `json:"description_md"`
	Fingerprint           string `json:"fingerprint"`
	EmbeddingModelVersion string `json:"embedding_model_version"`
	SnapshotID            string `json:"snapshot_id"`
	SupersedesPublishID   string `json:"supersedes_publish_id"`
}

func decodePriorQueuedDetails(raw string) (priorQueuedDetails, error) {
	if raw == "" {
		return priorQueuedDetails{}, nil
	}
	var out priorQueuedDetails
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return priorQueuedDetails{}, err
	}
	return out, nil
}

func buildQueuedDetails(content string, signatureOnly bool, modelVersion, snapshotID, priorPublishID string) snapshotQueuedDetails {
	return snapshotQueuedDetails{
		Content:               content,
		SignatureOnly:         signatureOnly,
		EmbeddingModelVersion: modelVersion,
		SnapshotID:            snapshotID,
		SupersedesPublishID:   priorPublishID,
	}
}

// buildConceptQueuedDetails is the concept-side equivalent of
// `buildQueuedDetails`. Passes through every identity field
// the promoter originally wrote, overrides
// `embedding_model_version` with the snapshot's forced model
// version (so a downstream re-embed lands on the current
// model, not the prior's), and stamps the snapshot
// discriminators.
func buildConceptQueuedDetails(prior priorQueuedDetails, modelVersion, snapshotID, priorPublishID string) snapshotConceptQueuedDetails {
	return snapshotConceptQueuedDetails{
		ConceptID:             prior.ConceptID,
		ConceptVersionID:      prior.ConceptVersionID,
		Name:                  prior.Name,
		DescriptionMD:         prior.DescriptionMD,
		Fingerprint:           prior.Fingerprint,
		EmbeddingModelVersion: modelVersion,
		SnapshotID:            snapshotID,
		SupersedesPublishID:   priorPublishID,
	}
}

// -----------------------------------------------------------
// Misc helpers
// -----------------------------------------------------------

// defaultSnapshotID mints a fresh 16-byte hex token (32 hex
// chars). Chosen over a uuid because the value is a sortable
// audit token, not a primary key — keeping the format
// compact and trivially copy-pasteable in operator dashboards.
func defaultSnapshotID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// isInvalidUUIDError matches PostgreSQL SQLSTATE 22P02 from
// the `$1::uuid` cast on a malformed input. Mirrored from
// the same helper in `internal/mgmtapi/handler.go` to keep
// the snapshot package free of any cross-package internal
// dependency on mgmtapi.
func isInvalidUUIDError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "invalid input syntax for type uuid")
}
