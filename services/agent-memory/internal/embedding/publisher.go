package embedding

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Closed-set Qdrant collection names that match the bootstrap
// in `cmd/qdrant-bootstrap/main.go` (and tech-spec §8.7.5 /
// implementation-plan Stage 1.4).  Pinning them as exported
// constants means downstream wiring code (the AST dispatcher
// adapter, the integration tests, the future Concept Promoter)
// can reference `embedding.CollectionMethod` without hand-
// typing the string literal — one `grep -F` finds every
// coupling.
const (
	CollectionMethod  = "agent_memory_method"
	CollectionBlock   = "agent_memory_block"
	CollectionConcept = "agent_memory_concept"
)

// Closed-set event-kind constants that mirror the values
// declared in the `embedding_publish_event_kind` ENUM
// (migration `0015_embedding_publish.sql`).  Defining them as
// constants keeps the §9.6a state-machine literals out of
// scattered string literals so a future ENUM expansion only
// needs to touch this file plus the migration.
const (
	EventKindQueued        = "queued"
	EventKindVectorWritten = "vector_written"
	EventKindPublished     = "published"
	EventKindFailed        = "failed"
	EventKindSuperseded    = "superseded"
)

// NodeKind is the closed set of `Node.kind` values this
// publisher accepts on its method/block-side flows.  Concept
// publishes are owned by the Concept Promoter (Stage 6.2) and
// reject any other kind defensively so a wiring bug surfaces
// at submit time, not at the `embedding_publish.exactly_one_target_chk`
// CHECK violation hours later.
const (
	NodeKindMethod = "method"
	NodeKindBlock  = "block"
)

// ErrAttemptFailed wraps a transient embedder or Qdrant
// failure that the publisher has DURABLY recorded as a
// `failed` `EmbeddingPublishEvent` row.  Callers that see
// `errors.Is(err, ErrAttemptFailed)` know two things:
//
//  1. The publish attempt did NOT reach `published` — the
//     vector is not addressable by GraphReader.
//  2. The failure is in the PostgreSQL event log already, so
//     it is safe to keep processing other work (the ingest
//     job continues; a background flusher will retry per
//     §9.6a step 4).
//
// Any error that is NOT wrapped by `ErrAttemptFailed` (e.g.
// `INSERT INTO embedding_publish ...` blew up because the
// connection died, or the caller passed an unknown
// `req.Kind`) must abort the calling unit of work — there is
// no durable record from which to recover.
var ErrAttemptFailed = errors.New("embedding: publish attempt failed (recorded as 'failed')")

// Embedder is the embedding-model client the publisher
// delegates step 4 of §9.6a to.  The interface is intentionally
// minimal — the embedder owns model selection, batching, and
// the wire protocol — so the publisher stays agnostic to which
// model is in use (E5, gte-small, BGE, future swap).
//
// Implementations MUST honour the supplied `ctx` for both
// cancellation and timeout.  Returning a non-nil error is
// classified by the publisher as a recordable §9.6a failure
// (the publisher inserts `event_kind='failed'`).
type Embedder interface {
	// Embed produces a single normalised vector for `content`.
	// L2-normalisation is the embedder's responsibility because
	// Qdrant collections are configured with cosine distance
	// (tech-spec §8.1) which is equivalent to dot-product only
	// for unit vectors.
	Embed(ctx context.Context, content string) ([]float32, error)
	// ModelVersion is the stable identifier the publisher
	// records on every `EmbeddingPublish` row's
	// `embedding_model_version` column (risk §9.6).  Format
	// is the embedder's choice but should be operator-readable
	// and globally unique per training run (e.g.
	// "gte-small@2024-09-15").
	ModelVersion() string
}

// Qdrant is the vector-store client the publisher delegates
// step 4 (upsert) and step 5 (read-after-write confirm) to.
// The interface is minimal so the integration test can swap a
// fake without standing up a real Qdrant container — the
// `httptest`-backed shim in `publisher_unit_test.go` exercises
// the full §9.6a flow against an in-memory implementation.
//
// Implementations MUST NOT return from `Upsert` until the
// write is durable enough to satisfy the subsequent
// `PointExists` confirm; on a real Qdrant server this means
// passing `?wait=true` (the `HTTPQdrant` client in
// `qdrant.go` does this; alternative implementations should
// document their own consistency guarantees here).
type Qdrant interface {
	// Upsert writes one point with `pointID`, `vector`, and
	// `payload` to the named collection.  Idempotent: re-running
	// with the same `pointID` overwrites the prior body.  This
	// is what makes the publish protocol crash-safe — a retry
	// after a Qdrant 5xx never produces a duplicate point.
	Upsert(ctx context.Context, collection, pointID string, vector []float32, payload map[string]any) error
	// PointExists implements the §9.6a step-5 read-after-write
	// confirm.  Returns (true, nil) when the point is queryable;
	// (false, nil) when the GET succeeded but reported absence
	// (e.g. eventual consistency lag); a non-nil error for
	// transport / 5xx failures.
	PointExists(ctx context.Context, collection, pointID string) (bool, error)
}

// PublishRequest is the input shape callers (the Stage 3.2
// dispatcher, the future delta-mode handler) hand to
// `Publisher.Publish`.  Every field is required EXCEPT
// `CanonicalSignature`, which is allowed empty in defensive
// tests but should be populated in production so the Qdrant
// payload carries enough provenance for downstream diagnostics
// (rubber-duck #8).
type PublishRequest struct {
	// NodeID is the textual UUID of the Node row the AST
	// emitter has already inserted (and committed).  The
	// publisher uses it as the `embedding_publish.node_id`
	// foreign key; if the Node row does not exist the
	// `embedding_publish.node_id_fkey` FK violation surfaces
	// as a non-`ErrAttemptFailed` error (callers MUST treat
	// it as a wiring bug, not a transient failure).
	NodeID string
	// RepoID is the textual UUID of the parent Repo, copied
	// onto the Qdrant payload so the §6.4 recall-filter pushdown
	// (filter by `repo_id` BEFORE k-NN scan) is honoured.
	RepoID string
	// Kind discriminates Method vs Block publishes.  Closed set
	// {NodeKindMethod, NodeKindBlock}; the publisher rejects
	// any other value rather than letting it slip through to
	// the wrong Qdrant collection.
	Kind string
	// CanonicalSignature is the architecture-owned
	// `Node.canonical_signature` value.  Copied onto the Qdrant
	// payload so a debugging operator can dereference a vector
	// hit back to a source location without re-joining
	// PostgreSQL.
	CanonicalSignature string
	// Content is the source text the embedder hashes.  For a
	// Method this is `MethodDecl.BodySource`; for a Block it is
	// the byte slice `src[StartByte:EndByte+1]` (note: inclusive
	// end byte — see `block.go:64-69`).  For a bodyless Method
	// declaration the dispatcher substitutes the canonical
	// signature and sets `SignatureOnly=true` (per evaluator
	// iter-1 finding #2: every emitted Method MUST get a
	// publish row, even those without a body).
	Content string
	// SignatureOnly is true when `Content` is the canonical
	// signature in place of the body text.  Recorded on the
	// Qdrant payload so a recall reader can distinguish
	// "embedded body" hits from "embedded signature" hits
	// without re-querying PostgreSQL.  Always false for Block
	// publishes and for Method publishes whose body was
	// non-empty.
	SignatureOnly bool
}

// PublishResult exposes the durable identifiers the publisher
// minted for this attempt.  Useful for downstream logging and
// (in the integration test) for issuing the `Retry` call.
type PublishResult struct {
	// PublishID is the `embedding_publish.publish_id` value
	// the publisher inserted (or, on `Retry`, the existing
	// row's id).  Use this when correlating event log rows.
	PublishID string
	// QdrantPointID is the `embedding_publish.qdrant_point_id`
	// value the publisher minted and upserted.  The Qdrant
	// payload always carries this back so a reverse lookup
	// from a Qdrant hit to the publish row is one query.
	QdrantPointID string
	// AttemptIndex is the value the publisher stamped on the
	// most-recently-appended `embedding_publish_event` row.
	// 0 on the initial `Publish`; N+1 of the prior max on
	// every `Retry`.
	AttemptIndex int
	// LastEventKind is the terminal `event_kind` for this
	// attempt — `published` on the happy path, `failed`
	// otherwise.  Surfaced for symmetric logging; the error
	// returned by `Publish` carries the same signal via
	// `errors.Is(err, ErrAttemptFailed)`.
	LastEventKind string
}

// Publisher implements the §9.6a state machine.  Construct
// with `NewPublisher`.  A `Publisher` is safe for concurrent
// use across goroutines — the underlying `*sql.DB`,
// `Embedder`, and `Qdrant` implementations are expected to be
// concurrency-safe and the publisher holds no per-call mutable
// state.
type Publisher struct {
	db       *sql.DB
	embedder Embedder
	qdrant   Qdrant

	logger *slog.Logger
	now    func() time.Time

	// newUUID is overridable so tests can pin deterministic
	// publish_id / point_id values.  Production passes nil →
	// uses `NewUUIDv4`.
	newUUID func() (string, error)
}

// Option is the functional-options shape used to construct a
// `Publisher` without bloating the constructor's positional
// arguments.
type Option func(*Publisher)

// WithLogger overrides the publisher's structured logger.
// Defaults to `slog.Default()`.
func WithLogger(logger *slog.Logger) Option {
	return func(p *Publisher) {
		if logger != nil {
			p.logger = logger
		}
	}
}

// WithClock overrides the wall-clock the publisher uses for
// log timestamps.  Defaults to `time.Now`.  The publisher does
// NOT pass this clock into PostgreSQL — `created_at` columns
// default to `now()` server-side so the database's clock is
// authoritative for partition routing.
func WithClock(now func() time.Time) Option {
	return func(p *Publisher) {
		if now != nil {
			p.now = now
		}
	}
}

// WithUUIDFactory overrides the UUID minter.  Tests use this
// to assert that the publisher carried a specific `point_id`
// into the Qdrant upsert call.
func WithUUIDFactory(fn func() (string, error)) Option {
	return func(p *Publisher) {
		if fn != nil {
			p.newUUID = fn
		}
	}
}

// NewPublisher constructs a Publisher.  Panics on nil `db`,
// `embedder`, or `qdrant` — the publisher cannot operate
// without any of them and a silent no-op would defeat the
// §9.6a acceptance contract.
func NewPublisher(db *sql.DB, embedder Embedder, qdrant Qdrant, opts ...Option) *Publisher {
	if db == nil {
		panic("embedding: NewPublisher: nil *sql.DB")
	}
	if embedder == nil {
		panic("embedding: NewPublisher: nil Embedder")
	}
	if qdrant == nil {
		panic("embedding: NewPublisher: nil Qdrant")
	}
	p := &Publisher{
		db:       db,
		embedder: embedder,
		qdrant:   qdrant,
		logger:   slog.Default(),
		now:      time.Now,
		newUUID:  NewUUIDv4,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// ModelVersion returns the current embedder's model version
// — the value the publisher would record on a fresh
// `embedding_publish` row IF called right now.  Exposed for
// the §9.6a Flusher: when a stuck publish row's recorded
// `embedding_model_version` differs from the current model,
// the row is unretryable under its original model
// (Publisher.Retry refuses model bumps at L409-420) and the
// flusher must mark it `superseded` instead of churning on
// it forever.  The flusher reads this via
// `ContentLookup.CurrentModelVersion` and the resolver
// surfaces `ErrSupersededByModel`.
//
// Returns the embedder's `ModelVersion()` verbatim.  An
// empty return is the publisher's signal that the embedder
// is misconfigured — the Retry / Flush path treats this as
// a non-recordable retry error rather than a supersede.
func (p *Publisher) ModelVersion() string {
	return p.embedder.ModelVersion()
}

// CollectionFor returns the Qdrant collection name the
// publisher targets for the supplied node kind.  Exported so
// the AST adapter (`astadapter.go`) and tests can derive the
// collection from a node kind without duplicating the mapping.
func CollectionFor(kind string) (string, error) {
	switch kind {
	case NodeKindMethod:
		return CollectionMethod, nil
	case NodeKindBlock:
		return CollectionBlock, nil
	default:
		return "", fmt.Errorf("embedding: unsupported node kind %q "+
			"(want %q or %q)", kind, NodeKindMethod, NodeKindBlock)
	}
}

// Publish runs the §9.6a write protocol end-to-end for a
// freshly-emitted Node.  See package doc for the seven-step
// state machine.  Return contract:
//
//   - `(result, nil)` on the happy path; `result.LastEventKind
//     == EventKindPublished`.
//   - `(result, err)` where `errors.Is(err, ErrAttemptFailed)`
//     when the embedder or Qdrant tripped AND the publisher
//     successfully recorded a `failed` event in PostgreSQL.
//     `result.PublishID` is set so the caller (or a background
//     retry job) can `Retry` against the same publish row.
//   - `(zero, err)` for any non-recordable failure: missing
//     Node FK target, invalid kind, PostgreSQL outage, JSON
//     marshal failure, etc.  The caller must NOT treat this as
//     a queued/failed publish because no durable state was
//     written.
func (p *Publisher) Publish(ctx context.Context, req PublishRequest) (PublishResult, error) {
	if err := p.validateRequest(req); err != nil {
		return PublishResult{}, err
	}

	pointID, err := p.newUUID()
	if err != nil {
		return PublishResult{}, fmt.Errorf("embedding: mint point_id: %w", err)
	}

	modelVersion := strings.TrimSpace(p.embedder.ModelVersion())
	if modelVersion == "" {
		// The risk §9.6 mitigation depends on every
		// `EmbeddingPublish` row carrying the embedding model
		// version.  An empty version would silently undermine
		// the future "supersede on model bump" flow; refuse
		// loudly here so a misconfigured embedder is caught at
		// the first publish, not at the first re-embed.
		return PublishResult{}, errors.New(
			"embedding: Embedder.ModelVersion() returned empty; " +
				"risk §9.6 requires a non-empty version per publish")
	}

	publishID, err := p.insertPublishAndQueued(ctx, req, pointID, modelVersion)
	if err != nil {
		// No durable record exists; the caller MUST treat
		// this as a fatal wiring/DB failure.
		return PublishResult{}, fmt.Errorf("embedding: insert publish + queued: %w", err)
	}

	result := PublishResult{
		PublishID:     publishID,
		QdrantPointID: pointID,
		AttemptIndex:  0,
		LastEventKind: EventKindQueued,
	}

	return p.runAttempt(ctx, req, result, modelVersion)
}

// Retry appends a fresh `queued` event for an existing
// `publish_id` (re-using the publish row's `point_id` so the
// Qdrant upsert remains idempotent against the original
// point) and re-runs steps 4-6 of §9.6a.  The caller is
// responsible for re-supplying `req.Content` — but in the
// production retry path the §9.6a Flusher reconstructs
// `req` from the queued snapshot the publisher wrote into
// `embedding_publish_event.details_json` on the prior
// `Publish` / earlier `Retry` (see `marshalQueuedDetails` +
// `insertPublishAndQueued` below), via
// `PublishEventContentResolver`.  Callers that already hold
// the body in hand (e.g. an in-flight ingest retry that
// never released the source bytes) can pass it directly to
// skip the resolver round-trip.  This matches rubber-duck #1.
//
// Retry refreshes the queued snapshot on every call so a
// retry-after-retry chain picks up the LATEST body shape, not
// the first attempt's — important when an operator patched
// `Content` or `SignatureOnly` between attempts.
//
// The new event rows carry `attempt_index = max(prior) + 1` so
// operators can correlate a retry's `queued / vector_written /
// published` triple back to its triggering `failed` row.
func (p *Publisher) Retry(ctx context.Context, publishID string, req PublishRequest) (PublishResult, error) {
	if publishID == "" {
		return PublishResult{}, errors.New("embedding: Retry: empty publishID")
	}
	if err := p.validateRequest(req); err != nil {
		return PublishResult{}, err
	}

	row, err := p.lookupPublishRow(ctx, publishID)
	if err != nil {
		return PublishResult{}, fmt.Errorf("embedding: Retry lookup: %w", err)
	}
	if row.NodeID != req.NodeID {
		// Refusing the mismatch up-front catches the operator
		// error of passing the wrong publish_id back into a
		// retry; the alternative (silently retrying against
		// the wrong Node) corrupts the recall index without
		// any obvious log signal.
		return PublishResult{}, fmt.Errorf(
			"embedding: Retry: publish_id %s targets node %s, request supplied node %s",
			publishID, row.NodeID, req.NodeID)
	}

	maxAttempt, err := p.maxAttemptIndex(ctx, publishID)
	if err != nil {
		return PublishResult{}, fmt.Errorf("embedding: Retry max attempt: %w", err)
	}
	nextAttempt := maxAttempt + 1

	// Rubber-duck #3: the publish row records the model
	// version of the FIRST attempt.  If the operator has
	// rolled the embedder forward between attempts, retrying
	// under the old `publish_id` would write a model-B vector
	// into a row labelled `model-A` — that is exactly the
	// drift risk §9.6 carries.  Refuse the retry and force
	// the caller into a fresh Publish + supersede flow.
	currentModel := strings.TrimSpace(p.embedder.ModelVersion())
	if currentModel == "" {
		return PublishResult{}, errors.New(
			"embedding: Retry: Embedder.ModelVersion() returned empty")
	}
	if currentModel != row.ModelVersion {
		return PublishResult{}, fmt.Errorf(
			"embedding: Retry: model version mismatch: "+
				"publish %s recorded %q, current embedder is %q "+
				"(model bumps require a fresh Publish + supersede, not Retry)",
			publishID, row.ModelVersion, currentModel)
	}

	// Refresh the queued-event snapshot so a subsequent
	// flusher iteration (which reads back through
	// `PublishEventContentResolver`) finds the LATEST
	// `Content` / `SignatureOnly` shape, not the first
	// attempt's.  The model version is by construction equal
	// to `row.ModelVersion` here (mismatch was rejected
	// above); recording it again keeps every queued event
	// self-sufficient for resolver lookups.
	retryDetails, err := marshalQueuedDetails(req, row.ModelVersion)
	if err != nil {
		return PublishResult{}, fmt.Errorf("embedding: Retry marshal details: %w", err)
	}

	if err := p.insertEvent(ctx, publishID, EventKindQueued, nextAttempt, retryDetails); err != nil {
		return PublishResult{}, fmt.Errorf("embedding: Retry insert queued: %w", err)
	}

	result := PublishResult{
		PublishID:     publishID,
		QdrantPointID: row.PointID,
		AttemptIndex:  nextAttempt,
		LastEventKind: EventKindQueued,
	}

	return p.runAttempt(ctx, req, result, row.ModelVersion)
}

// runAttempt is the shared steps-4-through-7 path used by both
// `Publish` and `Retry`.  The publish row + queued event are
// the caller's responsibility (because the SQL shape differs
// between the two entry points); from here on the protocol is
// identical.
func (p *Publisher) runAttempt(
	ctx context.Context,
	req PublishRequest,
	result PublishResult,
	modelVersion string,
) (PublishResult, error) {
	collection, err := CollectionFor(req.Kind)
	if err != nil {
		// We already validated kind in `validateRequest`; this
		// is belt-and-suspenders.  No event recorded because
		// the validation would have rejected the call before
		// step 2.
		return result, err
	}

	// Step 4a: embedder call.
	vec, err := p.embedder.Embed(ctx, req.Content)
	if err != nil {
		// Rubber-duck #5: cancellation/deadline is the caller's
		// signal that the unit of work is being torn down;
		// recording it as `failed` would mislead operators (the
		// publish wasn't fundamentally broken, the worker just
		// shut down).  Leave the latest event at `queued` so a
		// future retry picks it up; surface the cancellation
		// error verbatim (NOT wrapped in ErrAttemptFailed) so
		// the dispatcher propagates rather than swallows.
		if cancelled := ctx.Err(); cancelled != nil {
			return result, fmt.Errorf("embedding: embedder cancelled: %w", cancelled)
		}
		recordErr := p.insertEvent(ctx, result.PublishID, EventKindFailed,
			result.AttemptIndex, failureDetails("embedder", err))
		if recordErr != nil {
			// Could not record the failure — both the
			// embedder AND the event log are unreachable.
			// Surface BOTH to the caller so triage has the
			// full causal chain; this is the rare case that
			// is NOT wrapped in `ErrAttemptFailed`.
			return result, fmt.Errorf(
				"embedding: embedder failed (%v) AND failed-event insert failed: %w",
				err, recordErr)
		}
		result.LastEventKind = EventKindFailed
		p.logAttempt("embedder_failed", req, result, slog.String("error", err.Error()))
		return result, fmt.Errorf("%w: embedder: %v", ErrAttemptFailed, err)
	}

	payload := p.buildPayload(req, result, modelVersion)

	// Step 4b: Qdrant upsert.
	if err := p.qdrant.Upsert(ctx, collection, result.QdrantPointID, vec, payload); err != nil {
		if cancelled := ctx.Err(); cancelled != nil {
			return result, fmt.Errorf("embedding: qdrant upsert cancelled: %w", cancelled)
		}
		recordErr := p.insertEvent(ctx, result.PublishID, EventKindFailed,
			result.AttemptIndex, failureDetails("qdrant_upsert", err))
		if recordErr != nil {
			return result, fmt.Errorf(
				"embedding: qdrant upsert failed (%v) AND failed-event insert failed: %w",
				err, recordErr)
		}
		result.LastEventKind = EventKindFailed
		p.logAttempt("qdrant_upsert_failed", req, result, slog.String("error", err.Error()))
		return result, fmt.Errorf("%w: qdrant upsert: %v", ErrAttemptFailed, err)
	}

	// Step 4c: vector_written event.
	if err := p.insertEvent(ctx, result.PublishID, EventKindVectorWritten,
		result.AttemptIndex, nil); err != nil {
		// PG outage after a successful Qdrant upsert.  This is
		// a non-`ErrAttemptFailed` error: the durable event
		// log diverged from Qdrant.  Caller must abort.
		return result, fmt.Errorf("embedding: insert vector_written: %w", err)
	}
	result.LastEventKind = EventKindVectorWritten

	// Step 5: read-after-write confirm.
	ok, err := p.qdrant.PointExists(ctx, collection, result.QdrantPointID)
	if err != nil || !ok {
		if cancelled := ctx.Err(); cancelled != nil {
			return result, fmt.Errorf("embedding: qdrant confirm cancelled: %w", cancelled)
		}
		details := failureDetails("qdrant_confirm", err)
		if err == nil {
			details = json.RawMessage(`{"phase":"qdrant_confirm","error":"point not found after upsert"}`)
		}
		recordErr := p.insertEvent(ctx, result.PublishID, EventKindFailed,
			result.AttemptIndex, details)
		if recordErr != nil {
			return result, fmt.Errorf(
				"embedding: qdrant confirm failed AND failed-event insert failed: %w", recordErr)
		}
		result.LastEventKind = EventKindFailed
		if err != nil {
			p.logAttempt("qdrant_confirm_failed", req, result, slog.String("error", err.Error()))
			return result, fmt.Errorf("%w: qdrant confirm: %v", ErrAttemptFailed, err)
		}
		p.logAttempt("qdrant_confirm_missing", req, result)
		return result, fmt.Errorf("%w: qdrant confirm: point %s not found in %s",
			ErrAttemptFailed, result.QdrantPointID, collection)
	}

	// Step 6: published event.
	if err := p.insertEvent(ctx, result.PublishID, EventKindPublished,
		result.AttemptIndex, nil); err != nil {
		return result, fmt.Errorf("embedding: insert published: %w", err)
	}
	result.LastEventKind = EventKindPublished
	p.logAttempt("published", req, result)
	return result, nil
}

func (p *Publisher) validateRequest(req PublishRequest) error {
	if req.NodeID == "" {
		return errors.New("embedding: PublishRequest.NodeID is required")
	}
	if req.RepoID == "" {
		return errors.New("embedding: PublishRequest.RepoID is required")
	}
	switch req.Kind {
	case NodeKindMethod, NodeKindBlock:
		// ok
	default:
		return fmt.Errorf("embedding: PublishRequest.Kind %q not supported "+
			"(want %q or %q)", req.Kind, NodeKindMethod, NodeKindBlock)
	}
	// Reject zero-information embeddings up-front.  An empty
	// `Content` would produce a uniform-zero (or
	// embedder-defined "empty input") vector that pollutes the
	// recall index with a degenerate point.  The dispatcher's
	// bodyless-method path (dispatcher.go ~L417-434) covers
	// the only legitimate "no source body" case by populating
	// `Content` with the canonical signature and setting
	// `SignatureOnly=true` — so by the time a request reaches
	// the publisher, `Content` is ALWAYS non-empty by contract.
	// The `SignatureOnly` flag is informational (recorded in
	// the payload + the queued snapshot) and does NOT exempt
	// callers from the body requirement.
	if strings.TrimSpace(req.Content) == "" {
		return errors.New("embedding: PublishRequest.Content is required " +
			"(use the canonical signature with SignatureOnly=true for bodyless methods)")
	}
	return nil
}

// insertPublishAndQueued runs the §9.6a steps 2 and 3 in a
// SINGLE PostgreSQL transaction so an orphan `embedding_publish`
// row without its accompanying `queued` event row is impossible
// (per rubber-duck #6).  The transaction holds no other state,
// commits immediately, and is bounded by `ctx`.
func (p *Publisher) insertPublishAndQueued(
	ctx context.Context,
	req PublishRequest,
	pointID, modelVersion string,
) (string, error) {
	queuedDetails, err := marshalQueuedDetails(req, modelVersion)
	if err != nil {
		return "", err
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("embedding: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const insertPublishQ = `
		INSERT INTO embedding_publish
		    (node_id, embedding_model_version, qdrant_point_id)
		VALUES ($1, $2, $3)
		RETURNING publish_id::text
	`
	var publishID string
	if err := tx.QueryRowContext(ctx, insertPublishQ,
		req.NodeID, modelVersion, pointID,
	).Scan(&publishID); err != nil {
		return "", fmt.Errorf("embedding: insert embedding_publish: %w", err)
	}

	const insertEventQ = `
		INSERT INTO embedding_publish_event
		    (publish_id, event_kind, attempt_index, details_json)
		VALUES ($1, $2::embedding_publish_event_kind, $3, $4::jsonb)
	`
	if _, err := tx.ExecContext(ctx, insertEventQ,
		publishID, EventKindQueued, 0, string(queuedDetails),
	); err != nil {
		return "", fmt.Errorf("embedding: insert queued event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("embedding: commit publish+queued: %w", err)
	}
	return publishID, nil
}

// queuedEventDetails is the JSON shape the publisher records
// in `embedding_publish_event.details_json` for every
// `queued` event (initial publish AND every Retry).  It is
// the §9.6a-compliant snapshot that lets a background
// `Flusher` re-drive an interrupted publish without holding
// the caller's `Content` in memory across the failure.
//
// All fields are LITERAL copies of the originating
// `PublishRequest` (no normalisation, no truncation) so the
// `PublishEventContentResolver` can round-trip the request
// through a publish_event row and feed it back into
// `Publisher.Retry`.  The `EmbeddingModelVersion` field is
// the model the originating publish targeted; when it
// disagrees with the resolver's lookup (i.e. an operator
// bumped the embedder mid-flight) the resolver returns
// `ErrSupersededByModel` so the row is retired rather than
// retried under the wrong model.
//
// Why details_json and not a new column on embedding_publish?
// `details_json` is already JSONB; the schema and grants
// (migrations 0015/0016/0017) tolerate it; no migration is
// required.  The recall path (Stage 4) NEVER reads
// `details_json` — it gates on `event_kind = 'published'` —
// so this widening cannot leak source bytes into a recall
// hit.  The Stage 2.2 reader role (0017) DOES have SELECT
// on `embedding_publish_event`, which means operator
// triage queries can read the snapshot too; that is an
// intentional trade-off (source content is already
// reachable through `node` + the materialiser anyway).
type queuedEventDetails struct {
	Content               string `json:"content"`
	SignatureOnly         bool   `json:"signature_only"`
	EmbeddingModelVersion string `json:"embedding_model_version"`
}

// QueuedDetailsKey is the JSONB top-level key set the resolver
// expects.  Exported so a future operator tool can compose the
// same shape without depending on the `queuedEventDetails`
// private type.
const (
	QueuedDetailsKeyContent       = "content"
	QueuedDetailsKeySignatureOnly = "signature_only"
	QueuedDetailsKeyModelVersion  = "embedding_model_version"
)

// marshalQueuedDetails returns the JSONB body the publisher
// writes for a `queued` event.  Surfaces marshal failures as
// hard errors — a malformed JSON body would corrupt the
// resolver and we'd rather refuse to publish than write a
// row the resolver cannot decode.
func marshalQueuedDetails(req PublishRequest, modelVersion string) (json.RawMessage, error) {
	body := queuedEventDetails{
		Content:               req.Content,
		SignatureOnly:         req.SignatureOnly,
		EmbeddingModelVersion: modelVersion,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("embedding: marshal queued details: %w", err)
	}
	return raw, nil
}

// insertEvent appends a single event row.  Append-only by
// design — there is no UPDATE path, no `ON CONFLICT` clause,
// no row mutation.  This is the §9.6a invariant the migration
// 0016 role grants enforce at the database layer.
func (p *Publisher) insertEvent(
	ctx context.Context,
	publishID, kind string,
	attempt int,
	details json.RawMessage,
) error {
	var detailsArg any
	if len(details) > 0 {
		detailsArg = string(details)
	}
	const q = `
		INSERT INTO embedding_publish_event
		    (publish_id, event_kind, attempt_index, details_json)
		VALUES ($1, $2::embedding_publish_event_kind, $3, $4::jsonb)
	`
	if _, err := p.db.ExecContext(ctx, q, publishID, kind, attempt, detailsArg); err != nil {
		return fmt.Errorf("embedding: insert %s event: %w", kind, err)
	}
	return nil
}

// publishRow is the small subset of `embedding_publish`
// columns the `Retry` path needs to re-run the protocol.
type publishRow struct {
	NodeID       string
	PointID      string
	ModelVersion string
}

func (p *Publisher) lookupPublishRow(ctx context.Context, publishID string) (publishRow, error) {
	const q = `
		SELECT
		    coalesce(node_id::text, ''),
		    qdrant_point_id::text,
		    embedding_model_version
		FROM embedding_publish
		WHERE publish_id = $1
	`
	var row publishRow
	if err := p.db.QueryRowContext(ctx, q, publishID).
		Scan(&row.NodeID, &row.PointID, &row.ModelVersion); err != nil {
		return publishRow{}, err
	}
	if row.NodeID == "" {
		// The publish row points at a `concept_version_id`,
		// not a `node_id`.  The Method/Block retry path
		// shouldn't be touching it; refuse loudly so a wiring
		// bug is caught here.
		return publishRow{}, fmt.Errorf(
			"embedding: publish %s has no node_id (it targets a concept_version)",
			publishID)
	}
	return row, nil
}

func (p *Publisher) maxAttemptIndex(ctx context.Context, publishID string) (int, error) {
	const q = `
		SELECT coalesce(max(attempt_index), 0)
		FROM embedding_publish_event
		WHERE publish_id = $1
	`
	var n int
	if err := p.db.QueryRowContext(ctx, q, publishID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// buildPayload assembles the Qdrant payload per rubber-duck #8:
// repo_id, kind, node_id, publish_id, canonical_signature, and
// embedding_model_version.  The bootstrap (cmd/qdrant-bootstrap)
// indexes `repo_id` and `kind` for filter pushdown; the other
// fields are reverse-lookup conveniences GraphReader and
// operators rely on when triaging a recall hit.
//
// `signature_only` carries through the Method-without-body
// signal (evaluator iter-1 finding #2): a recall reader can
// downweight or annotate hits whose embedded text is a
// signature rather than a body without re-joining PostgreSQL.
func (p *Publisher) buildPayload(req PublishRequest, result PublishResult, modelVersion string) map[string]any {
	return map[string]any{
		"repo_id":                 req.RepoID,
		"kind":                    req.Kind,
		"node_id":                 req.NodeID,
		"publish_id":              result.PublishID,
		"canonical_signature":     req.CanonicalSignature,
		"embedding_model_version": modelVersion,
		"signature_only":          req.SignatureOnly,
	}
}

func (p *Publisher) logAttempt(eventOp string, req PublishRequest, result PublishResult, extras ...slog.Attr) {
	attrs := []slog.Attr{
		slog.String("op", "embedding.publish."+eventOp),
		slog.String("publish_id", result.PublishID),
		slog.String("point_id", result.QdrantPointID),
		slog.String("node_id", req.NodeID),
		slog.String("repo_id", req.RepoID),
		slog.String("kind", req.Kind),
		slog.Int("attempt_index", result.AttemptIndex),
		slog.String("last_event_kind", result.LastEventKind),
	}
	attrs = append(attrs, extras...)
	// LogAttrs is the lowest-overhead path on slog and avoids
	// the variadic-`any` allocation churn the publisher would
	// otherwise pay for on every Method/Block publish.
	if result.LastEventKind == EventKindFailed {
		p.logger.LogAttrs(context.Background(), slog.LevelWarn, "embedding.publish.failed", attrs...)
	} else {
		p.logger.LogAttrs(context.Background(), slog.LevelInfo, "embedding.publish", attrs...)
	}
}

// failureDetails marshals a closed-set diagnostic payload for
// the `embedding_publish_event.details_json` column.  Kept in
// one place so operators see the same `{phase, error}` shape
// regardless of which §9.6a step tripped.
func failureDetails(phase string, err error) json.RawMessage {
	body := map[string]any{
		"phase": phase,
	}
	if err != nil {
		body["error"] = err.Error()
	}
	raw, mErr := json.Marshal(body)
	if mErr != nil {
		// json.Marshal of a `map[string]any` keyed by string
		// literals cannot fail for our inputs; this branch
		// guards against a future reflection bug. We hard-code
		// the phase to a sentinel rather than interpolating the
		// caller-supplied value because the signature accepts
		// arbitrary `string` — if a future caller passes a phase
		// containing `"` or `\`, raw concatenation would emit
		// malformed JSON that the `$4::jsonb` cast in
		// `insertEvent` would reject, turning a marshal-fallback
		// into a second failure that masks the original error.
		return json.RawMessage(`{"phase":"unknown","error":"json_marshal_failed"}`)
	}
	return json.RawMessage(raw)
}
