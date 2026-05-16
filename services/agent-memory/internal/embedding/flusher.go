package embedding

// Flusher is the §9.6a background retry scanner.  Per
// tech-spec §9.6a ("Qdrant outages surface through C22 as
// `embedding_index_unavailable`; the writer queues the
// publish by leaving the latest event at `queued` or `failed`
// and a background flusher retries"), the publisher records
// transient failures durably but does NOT loop on them
// in-process — that's the wrong granularity for a per-Node
// ingest hook.  The Flusher is the operational counterpart:
// it scans the publish event log for stuck rows and re-runs
// `Publisher.Retry` for each.
//
// Design choices
// --------------
//
//   - The Flusher reads back source content from the
//     publisher's queued-event snapshot via a
//     `ContentResolver` interface.  The publisher writes a
//     JSON snapshot of `Content` / `SignatureOnly` /
//     `EmbeddingModelVersion` into
//     `embedding_publish_event.details_json` on every
//     `Publish` / `Retry` (publisher.go ~L633-694), and the
//     default production resolver
//     (`PublishEventContentResolver`, wired by
//     `cmd/repoindexer/main.go`) decodes that snapshot.
//     Alternative resolvers (blob cache, re-parse from a
//     materialised workspace) plug in via the same interface
//     for callers that want to keep `details_json` slim.
//
//   - The Flusher detects operator-current model drift
//     BEFORE calling the resolver.  Once per Flush cycle it
//     reads `Publisher.ModelVersion()` and short-circuits any
//     row whose recorded `embedding_model_version` differs,
//     writing a `superseded` event without materializing
//     content (avoids a wasted DB read and avoids the
//     `Publisher.Retry` "model version mismatch" non-recordable
//     error path that would otherwise churn the row forever).
//     The resolver's matching `lookup.CurrentModelVersion`
//     check is defence-in-depth for callers that drive
//     `Resolve` outside the Flusher.
//
//   - The Flusher does NOT take a distributed lock.  Per
//     tech-spec §8.3 a single repoindexer process owns
//     retries; if the deployment scales to multiple
//     repoindexer instances a follow-up workstream MUST add
//     an advisory lock (rubber-duck #6 from iter-0).
//
//   - The scan filter "latest event is failed OR (queued AND
//     older than threshold)" mirrors tech-spec §9.6a: a
//     `queued` row in steady-state is the normal in-flight
//     publish; only an OLD `queued` row indicates the
//     publisher crashed mid-attempt and never recorded the
//     terminal event.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
)

// ContentResolver is the callback the flusher uses to
// re-materialise the source body for a publish that needs
// retry.  Implementations OWN the strategy for retrieving
// the content: a blob store backed by the materialised
// workspace, a content-addressed cache, or an in-process
// re-parse from the Node's `body_start_byte` / `body_end_byte`
// against the file content.
//
// Returning an error MUST cause the flusher to record the
// publish row as DEFERRED (the row stays in its current
// state, no Retry attempt is logged, the per-Flush counter
// `Stats.ResolveErrors` advances).  Returning a fully-formed
// `PublishRequest` MUST mirror the original `Publish` shape
// — NodeID, RepoID, Kind, CanonicalSignature, Content, and
// (for bodyless methods) SignatureOnly.
//
// Implementations MUST honour `ctx`.
type ContentResolver interface {
	Resolve(ctx context.Context, lookup ContentLookup) (PublishRequest, error)
}

// ContentLookup is the (NodeID-plus-context) primitive the
// flusher hands to the resolver.  The flusher populates every
// field from the persisted publish row + the joined `node`
// row so a typical resolver does NOT have to issue an extra
// DB read — it gets `Kind` / `RepoID` / `CanonicalSignature`
// for free and uses `PublishID` only as the seam back to its
// own content store (e.g. `embedding_publish_event.details_json`,
// a blob cache, or a re-materialised git workspace).
//
// Model-drift contract
// --------------------
// `ModelVersion` is the model recorded on the publish row at
// initial publish time.  `CurrentModelVersion` is the model
// the publisher's embedder reports RIGHT NOW (the flusher
// reads it from `*Publisher.ModelVersion()` once per Flush
// cycle).  A resolver MUST return `ErrSupersededByModel`
// when the two disagree — the flusher records a `superseded`
// event instead of retrying with the wrong model.
//
// This wiring closes the §9.6a model-bump churn loop: without
// it, a publish row recorded under model A whose row also
// says `model A` would loop forever once the operator rolled
// the embedder forward to model B (Publisher.Retry refuses
// the call at publisher.go:409-420 with a non-recordable
// error, and the flusher's RetryErrors counter just
// increments).  With it, the first Flush after the model
// bump retires the row with a `superseded` event so the
// backlog drains instead of churning.
//
// `Kind` is `NodeKindMethod` ("method") or `NodeKindBlock`
// ("block") — copied verbatim from `node.kind`.  Resolvers
// short-circuit on this to look up Method vs Block source
// content without an FK probe.
//
// `RepoID` is the textual UUID of `node.repo_id`; carried so
// the resolver can route a multi-repo workspace cache without
// re-querying `node`.
//
// `CanonicalSignature` is `node.canonical_signature`; the
// resolver uses it when reconstructing a `PublishRequest`
// whose other fields it must populate from a content store
// (the `PublishRequest.CanonicalSignature` field on
// `Publisher.Retry` is matched against the existing publish
// row for diagnostic logging only, not as a state check).
type ContentLookup struct {
	PublishID           string
	NodeID              string
	ModelVersion        string
	CurrentModelVersion string
	Kind                string
	RepoID              string
	CanonicalSignature  string
}

// ErrSupersededByModel is the sentinel a `ContentResolver`
// returns when the requested publish's model_version has
// been rolled forward by the operator and the row should be
// retired rather than retried.  The flusher records a
// `superseded` event and skips the Retry call.  Returning
// any other error treats the lookup as a transient failure
// (the flusher advances `Stats.ResolveErrors` and leaves the
// row untouched for the next scan).
var ErrSupersededByModel = errors.New("embedding: publish superseded by newer model version")

// Stats is the per-Flush summary the flusher returns.  Useful
// for tests and for the operator metrics endpoint
// (implementation-plan.md:1380 expects a Prometheus binding
// here too).
type Stats struct {
	// Scanned is the number of rows the SQL filter
	// matched.  Lower-bound for `Retried + Skipped +
	// ResolveErrors + RetryErrors`.
	Scanned int
	// Retried is the number of rows for which `Retry`
	// returned `nil` (i.e. the row reached `published`
	// during this flush).
	Retried int
	// RetriedFailed is the number of Retry calls that
	// surfaced `ErrAttemptFailed` (the row recorded a
	// fresh `failed` event but did NOT reach `published`).
	// The row stays eligible for the next flush.
	RetriedFailed int
	// Superseded is the number of rows the resolver
	// flagged as `ErrSupersededByModel`.  The flusher
	// recorded a `superseded` event for each.
	Superseded int
	// ResolveErrors counts ContentResolver failures (other
	// than `ErrSupersededByModel`).  The publish rows are
	// left untouched.
	ResolveErrors int
	// RetryErrors counts non-`ErrAttemptFailed` Retry
	// failures (e.g. PG outage during the Retry's event
	// insert).  The publish rows are left untouched.
	RetryErrors int
}

// FlusherMetrics is the operator-visible counter set the
// flusher increments cumulatively across Flush calls.
type FlusherMetrics struct {
	// FlushesTotal is the cumulative number of Flush
	// invocations (success or failure).
	FlushesTotal atomic.Int64
	// RetriedTotal is the cumulative count of publish rows
	// that reached `published` via a flusher Retry.
	RetriedTotal atomic.Int64
	// SupersededTotal is the cumulative count of publish
	// rows the flusher marked `superseded` because the
	// resolver returned `ErrSupersededByModel`.
	SupersededTotal atomic.Int64
}

// Flusher scans the §9.6a publish event log for stuck rows
// and retries them.  Safe for concurrent Flush invocations
// (the underlying SQL holds short transactions), but the
// embedded `Publisher` and `ContentResolver` MUST be
// concurrency-safe themselves.
type Flusher struct {
	db        *sql.DB
	publisher *Publisher
	resolver  ContentResolver

	queuedAgeThreshold time.Duration
	failedAgeThreshold time.Duration
	scanLimit          int
	logger             *slog.Logger
	now                func() time.Time
	metrics            *FlusherMetrics
}

// FlusherOption configures a `Flusher` at construction time.
type FlusherOption func(*Flusher)

// WithQueuedAgeThreshold pins how long a `queued`-latest row
// must sit before the flusher considers it stuck.  Defaults
// to 5 minutes — long enough that a healthy publish that
// hasn't reached `vector_written` yet isn't retried out from
// under the in-flight attempt, short enough that an
// abandoned attempt doesn't stall recall indefinitely.
//
// An explicit `0` is a valid value and means "no age gate —
// every queued row qualifies immediately."  This is the
// shape integration tests use to drive deterministic
// scans without sleeping the goroutine.  Negative values
// are nonsensical (they would advertise rows in the future
// as stuck, racing fresh in-flight attempts) and the option
// silently ignores them so a caller bug cannot stomp the
// default.
func WithQueuedAgeThreshold(d time.Duration) FlusherOption {
	return func(f *Flusher) {
		if d < 0 {
			return
		}
		f.queuedAgeThreshold = d
	}
}

// WithFailedAgeThreshold pins how long a `failed`-latest row
// must sit before retry.  Defaults to 30 seconds — short
// because a `failed` row is definitionally not in-flight, so
// the retry can fire as soon as the embedder/Qdrant come
// back up.
//
// An explicit `0` is a valid value and means "no age gate —
// every failed row qualifies immediately."  Tests pass `0`
// to drive a deterministic scan without sleeping; production
// always passes a positive duration so a transient outage
// followed by an immediate flush doesn't spin on the same
// failing rows.  Negative values are silently ignored (see
// `WithQueuedAgeThreshold` for the rationale).
func WithFailedAgeThreshold(d time.Duration) FlusherOption {
	return func(f *Flusher) {
		if d < 0 {
			return
		}
		f.failedAgeThreshold = d
	}
}

// WithScanLimit caps the number of rows the flusher pulls
// per Flush.  Defaults to 100 — small enough that a long
// backlog doesn't monopolise the Flush call, large enough
// that steady-state retries finish in one Flush.
func WithScanLimit(n int) FlusherOption {
	return func(f *Flusher) {
		if n > 0 {
			f.scanLimit = n
		}
	}
}

// WithFlusherLogger overrides the flusher's structured
// logger.  Defaults to `slog.Default()`.
func WithFlusherLogger(logger *slog.Logger) FlusherOption {
	return func(f *Flusher) {
		if logger != nil {
			f.logger = logger
		}
	}
}

// WithFlusherClock overrides the flusher's clock.  Tests
// pin this to drive deterministic age thresholds.
func WithFlusherClock(now func() time.Time) FlusherOption {
	return func(f *Flusher) {
		if now != nil {
			f.now = now
		}
	}
}

// WithFlusherMetrics injects an externally-owned metric set
// so the Prometheus binding can register one counter set per
// flusher instance.  Pass nil (or omit) to opt into a
// flusher-owned default.
func WithFlusherMetrics(m *FlusherMetrics) FlusherOption {
	return func(f *Flusher) {
		if m != nil {
			f.metrics = m
		}
	}
}

// NewFlusher constructs a `Flusher`.  Panics on nil `db`,
// `publisher`, or `resolver` — every one is structurally
// required and a silent no-op would leave §9.6a backlog rows
// stuck forever.
func NewFlusher(db *sql.DB, publisher *Publisher, resolver ContentResolver, opts ...FlusherOption) *Flusher {
	if db == nil {
		panic("embedding: NewFlusher: nil *sql.DB")
	}
	if publisher == nil {
		panic("embedding: NewFlusher: nil *Publisher")
	}
	if resolver == nil {
		panic("embedding: NewFlusher: nil ContentResolver")
	}
	f := &Flusher{
		db:                 db,
		publisher:          publisher,
		resolver:           resolver,
		queuedAgeThreshold: 5 * time.Minute,
		failedAgeThreshold: 30 * time.Second,
		scanLimit:          100,
		logger:             slog.Default(),
		now:                time.Now,
		metrics:            &FlusherMetrics{},
	}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// Metrics exposes the flusher's metric set for the
// Prometheus binding and for tests that want to assert on
// counter movement.
func (f *Flusher) Metrics() *FlusherMetrics {
	return f.metrics
}

// stuckPublishRow is the small projection of `embedding_publish`
// columns the Flush path needs to drive a Retry.  Joined with
// `node` so the resolver gets `kind`, `repo_id`, and
// `canonical_signature` without a second query.
//
// `MaxAttemptIndex` carries `max(embedding_publish_event.attempt_index)`
// for the publish, computed once during the scan.  Used by the
// supersede branches to stamp the terminal `superseded` event at
// the latest attempt's index rather than a hardcoded `0` — a
// `superseded` event is the terminal status of the existing
// latest attempt under the obsolete model, NOT a new attempt
// (no embed/upsert work happens), so re-using the latest
// attempt_index is the semantically correct audit-log shape.
// Sourced from a `max()` aggregate (not "latest event's
// attempt_index") so a future out-of-order insert path cannot
// regress the supersede attempt_index below the true high
// water mark.
type stuckPublishRow struct {
	PublishID          string
	NodeID             string
	ModelVersion       string
	LatestKind         string
	MaxAttemptIndex    int
	Kind               string
	RepoID             string
	CanonicalSignature string
}

// Flush runs ONE scan-and-retry cycle.  Suitable as the body
// of a `for range time.Tick(...)` loop in production OR for
// a single explicit call in tests.  Errors returned from
// Flush itself are limited to the SQL scan failing; per-row
// failures (resolver error, retry error) are recorded on
// `Stats` rather than aborted, so a single broken Node
// cannot stall the whole backlog.
func (f *Flusher) Flush(ctx context.Context) (Stats, error) {
	f.metrics.FlushesTotal.Add(1)

	rows, err := f.findStuckPublishRows(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("embedding: Flusher.Flush scan: %w", err)
	}
	stats := Stats{Scanned: len(rows)}
	// Read the operator-current model version ONCE per Flush
	// cycle so the supersede pre-check below cannot race a
	// mid-cycle embedder rotation (the second-half of the
	// scan would otherwise see model B while the first-half
	// saw model A and emit inconsistent supersede decisions
	// across the same scan).
	currentModel := f.publisher.ModelVersion()
	for _, row := range rows {
		// Respect context cancellation between rows so a
		// shutdown signal cuts the flush short without
		// dropping the in-flight Retry.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stats, ctxErr
		}

		// Flusher-side supersede pre-check.  When the row's
		// recorded model differs from the operator-current
		// model, the publish row is unretryable under its
		// original model (Publisher.Retry refuses model
		// bumps at publisher.go:409-420 with a
		// non-recordable error) — without this gate the
		// row would churn forever, incrementing
		// `RetryErrors` on every cycle.  Recording
		// `superseded` here also avoids materializing
		// source content (the resolver's DB read) for a
		// row we cannot retry anyway.  The empty-string
		// defence skips the check when EITHER side is
		// blank (a misconfigured embedder OR a legacy row
		// without a recorded model) so we surface the
		// underlying misconfiguration via the Retry path
		// instead of silently superseding.
		//
		// Supersede stamps `row.MaxAttemptIndex` (NOT 0,
		// NOT MaxAttemptIndex+1).  `superseded` is the
		// terminal status of the existing latest attempt
		// under the obsolete model — no embed/upsert
		// happens — so re-using the latest attempt_index
		// is the semantically correct audit-log shape and
		// preserves the migration's monotonic-non-decreasing
		// invariant on `attempt_index` per publish.
		if row.ModelVersion != "" && currentModel != "" && row.ModelVersion != currentModel {
			if err := f.publisher.insertEvent(ctx,
				row.PublishID, EventKindSuperseded, row.MaxAttemptIndex, nil,
			); err != nil {
				stats.RetryErrors++
				f.logger.LogAttrs(ctx, slog.LevelWarn,
					"embedding.flusher.superseded_record_failed",
					slog.String("publish_id", row.PublishID),
					slog.String("error", err.Error()),
				)
				continue
			}
			stats.Superseded++
			f.metrics.SupersededTotal.Add(1)
			f.logger.LogAttrs(ctx, slog.LevelInfo,
				"embedding.flusher.superseded_by_model",
				slog.String("publish_id", row.PublishID),
				slog.String("node_id", row.NodeID),
				slog.String("row_model", row.ModelVersion),
				slog.String("current_model", currentModel),
				slog.Int("attempt_index", row.MaxAttemptIndex),
			)
			continue
		}

		lookup := ContentLookup{
			PublishID:           row.PublishID,
			NodeID:              row.NodeID,
			ModelVersion:        row.ModelVersion,
			CurrentModelVersion: currentModel,
			Kind:                row.Kind,
			RepoID:              row.RepoID,
			CanonicalSignature:  row.CanonicalSignature,
		}
		req, rerr := f.resolver.Resolve(ctx, lookup)
		if rerr != nil {
			if errors.Is(rerr, ErrSupersededByModel) {
				// Resolver-driven supersede branch.  Same
				// semantic as the pre-check above: stamp
				// `row.MaxAttemptIndex` so the supersede
				// event lives on the latest-attempt's
				// audit row, preserving the migration's
				// monotonic-non-decreasing attempt_index
				// invariant per publish.
				if err := f.publisher.insertEvent(ctx,
					row.PublishID, EventKindSuperseded, row.MaxAttemptIndex, nil,
				); err != nil {
					stats.RetryErrors++
					f.logger.LogAttrs(ctx, slog.LevelWarn,
						"embedding.flusher.superseded_record_failed",
						slog.String("publish_id", row.PublishID),
						slog.String("error", err.Error()),
					)
					continue
				}
				stats.Superseded++
				f.metrics.SupersededTotal.Add(1)
				continue
			}
			stats.ResolveErrors++
			f.logger.LogAttrs(ctx, slog.LevelWarn,
				"embedding.flusher.resolve_failed",
				slog.String("publish_id", row.PublishID),
				slog.String("node_id", row.NodeID),
				slog.String("error", rerr.Error()),
			)
			continue
		}

		// Defensive: validate the resolver returned the
		// node_id the lookup specified.  A mismatch is a
		// wiring bug (resolver routed to the wrong content)
		// and would corrupt the recall index if we
		// proceeded with the Retry.
		if req.NodeID != row.NodeID {
			stats.ResolveErrors++
			f.logger.LogAttrs(ctx, slog.LevelError,
				"embedding.flusher.resolver_returned_wrong_node",
				slog.String("publish_id", row.PublishID),
				slog.String("requested_node_id", row.NodeID),
				slog.String("returned_node_id", req.NodeID),
			)
			continue
		}

		_, retryErr := f.publisher.Retry(ctx, row.PublishID, req)
		if retryErr == nil {
			stats.Retried++
			f.metrics.RetriedTotal.Add(1)
			continue
		}
		if errors.Is(retryErr, ErrAttemptFailed) {
			stats.RetriedFailed++
			f.logger.LogAttrs(ctx, slog.LevelInfo,
				"embedding.flusher.retry_failed_recorded",
				slog.String("publish_id", row.PublishID),
				slog.String("node_id", row.NodeID),
				slog.String("error", retryErr.Error()),
			)
			continue
		}
		stats.RetryErrors++
		// Non-recordable retry failure (DB outage, model
		// mismatch, etc.); leave the row untouched and
		// surface the error in the log so triage can pick
		// it up.
		f.logger.LogAttrs(ctx, slog.LevelWarn,
			"embedding.flusher.retry_error",
			slog.String("publish_id", row.PublishID),
			slog.String("node_id", row.NodeID),
			slog.String("error", retryErr.Error()),
		)
	}
	return stats, nil
}

// Run drives `Flush` on a `time.Ticker` cadence until `ctx`
// is cancelled.  Returns the cancellation error verbatim so
// the supervising goroutine can distinguish shutdown from a
// real failure.  Per-cycle errors are logged, NOT returned —
// a transient scan failure must not crash the long-running
// retry loop.
func (f *Flusher) Run(ctx context.Context, every time.Duration) error {
	if every <= 0 {
		return errors.New("embedding: Flusher.Run: every must be > 0")
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			stats, err := f.Flush(ctx)
			if err != nil {
				f.logger.LogAttrs(ctx, slog.LevelWarn,
					"embedding.flusher.flush_failed",
					slog.String("error", err.Error()),
				)
				continue
			}
			if stats.Scanned > 0 {
				f.logger.LogAttrs(ctx, slog.LevelInfo,
					"embedding.flusher.flush_done",
					slog.Int("scanned", stats.Scanned),
					slog.Int("retried", stats.Retried),
					slog.Int("retried_failed", stats.RetriedFailed),
					slog.Int("superseded", stats.Superseded),
					slog.Int("resolve_errors", stats.ResolveErrors),
					slog.Int("retry_errors", stats.RetryErrors),
				)
			}
		}
	}
}

// findStuckPublishRows runs the §9.6a-aware scan.  Returns
// the rows whose latest `embedding_publish_event` is either
// `failed` (older than `failedAgeThreshold`) OR `queued`
// (older than `queuedAgeThreshold`).
//
// Query shape: LATERAL skip-scan, NOT a CTE-with-DISTINCT-ON.
// The earlier shape wrapped every event row in a
// `WITH latest AS (SELECT DISTINCT ON (publish_id) ...)`
// barrier plus a sibling `MAX(attempt_index) GROUP BY publish_id`
// CTE.  PostgreSQL cannot push the outer `event_kind` /
// `latest_at` filter through either barrier, so every flush
// cycle (default 30s) paid a full-table sort + HashAggregate
// over `embedding_publish_event` even with an empty backlog,
// holding a shared lock that competed with the publisher's
// append path.  Pruning recent events *inside* the CTE would
// be a correctness regression: a `published` or
// `vector_written` event written within the last
// `max(failedAgeThreshold, queuedAgeThreshold)` would drop
// out of the scan, the `DISTINCT ON` would then promote the
// older `queued` / `failed` event to "latest", and the
// flusher would falsely re-drive an already-terminal row.
// The LATERAL shape preserves "latest event per publish_id"
// semantics by probing the partitioned
// `embedding_publish_event_publish_created_idx` index from
// migration 0015 with `LIMIT 1` per publish — O(P · log E)
// instead of O(E · log E), and an empty backlog stays empty
// because the outer WHERE filters drop the probe result
// before any extra work is done.
//
// `max_attempt` is still a `MAX(attempt_index)` aggregate
// (NOT "the latest event's attempt_index") so an
// out-of-order insert (clock skew, manually backfilled
// legacy row, future batched-insert path) cannot regress
// the supersede attempt_index below the true high water
// mark.  See rubber-duck #b on supersede-attempt-index.
// The aggregate runs only for publish rows that survived
// the latest-event filter — typically O(events-per-publish)
// per stuck row, a handful of index entries.
//
// Joins `node` (non-partitioned, indexed on `node_id`) to
// surface `kind`, `repo_id`, and `canonical_signature` into
// the `ContentLookup` the resolver receives.  Without the
// JOIN the resolver would have to re-issue the same lookup
// per scanned row, which would hammer `node` in proportion
// to backlog depth.  The INNER JOIN on `node` also enforces
// `p.node_id IS NOT NULL` structurally (concept-targeted
// publishes are routed through their own flusher), letting
// the planner pick the `embedding_publish_node_id_idx`
// partial index for the driving scan.
//
// Bounded by `scanLimit` so a long backlog drains across
// multiple Flush calls rather than monopolising any one.
func (f *Flusher) findStuckPublishRows(ctx context.Context) ([]stuckPublishRow, error) {
	const q = `
		SELECT
		    p.publish_id::text,
		    coalesce(p.node_id::text, ''),
		    p.embedding_model_version,
		    l.event_kind::text,
		    coalesce(ma.max_attempt, 0),
		    n.kind::text,
		    n.repo_id::text,
		    n.canonical_signature
		FROM embedding_publish p
		JOIN node n ON n.node_id = p.node_id
		JOIN LATERAL (
		    SELECT e.event_kind, e.created_at AS latest_at
		    FROM embedding_publish_event e
		    WHERE e.publish_id = p.publish_id
		    ORDER BY e.created_at DESC, e.event_id DESC
		    LIMIT 1
		) l ON true
		JOIN LATERAL (
		    SELECT MAX(e.attempt_index) AS max_attempt
		    FROM embedding_publish_event e
		    WHERE e.publish_id = p.publish_id
		) ma ON true
		WHERE p.node_id IS NOT NULL
		  AND (
		      (l.event_kind = 'failed' AND l.latest_at < $1)
		   OR (l.event_kind = 'queued' AND l.latest_at < $2)
		  )
		ORDER BY l.latest_at ASC
		LIMIT $3
	`
	now := f.now()
	failedBefore := now.Add(-f.failedAgeThreshold)
	queuedBefore := now.Add(-f.queuedAgeThreshold)

	rows, err := f.db.QueryContext(ctx, q, failedBefore, queuedBefore, f.scanLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []stuckPublishRow
	for rows.Next() {
		var row stuckPublishRow
		if err := rows.Scan(
			&row.PublishID,
			&row.NodeID,
			&row.ModelVersion,
			&row.LatestKind,
			&row.MaxAttemptIndex,
			&row.Kind,
			&row.RepoID,
			&row.CanonicalSignature,
		); err != nil {
			return nil, err
		}
		if strings.TrimSpace(row.NodeID) == "" {
			// Concept publishes (concept_version_id !=
			// NULL) are not the Method/Block flusher's
			// surface; defensive skip if the SQL ever
			// drifts.
			continue
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
