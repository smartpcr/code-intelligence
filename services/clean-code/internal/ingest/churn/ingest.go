package churn

// Stage 4.4 (ingest churn verb feeds materialiser,
// implementation-plan.md lines 410-425).
//
// ingest.go holds the public staging adapter the
// `ingest.churn` verb dispatches to. The package contract:
//
//   - The verb writes ZERO `metric_sample` rows directly.
//     [Ingester] depends on [ChurnEventWriter] only; it has
//     NO `metric_sample` writer dependency in its type
//     signature (the lack of that field is the structural
//     proof of the contract).
//   - The verb writes rows into the `clean_code.churn_event`
//     staging table (migration 0010).
//   - The `modification_count_in_window` materialiser is the
//     SOLE writer of that metric_kind (architecture Sec
//     4.4, tech-spec Sec 4.1.1 lines 287-291). The
//     materialiser reads `churn_event` on a later pass and
//     emits `metric_sample` drafts under its own writer
//     identity. The Stage 4.4 verb never touches the
//     materialiser directly.
//
// # Production wiring (Stage 4.4 iter 2)
//
// The webhook `ingest.churn` verb dispatches directly to
// this package's [Ingester]; there is no [metric_ingestor]
// dependency in the verb's call stack. The composition root
// in `cmd/clean-code-metric-ingestor/main.go` constructs:
//
//	PGChurnEventStore -> churn.NewIngester -> webhook.NewChurnVerbHandler
//
// so the entire request path stages rows into
// `clean_code.churn_event` and writes ZERO `metric_sample`
// rows. The `modification_count_in_window` materialiser
// reads `churn_event` on a later pass and is the sole writer
// of that metric_kind. The legacy `metric_ingestor.ChurnSweep`
// code still compiles (other callers depend on it) but is no
// longer in the webhook verb path.
//
// The verb-package-boundary scenario `churn-writes-no-metric-sample`
// is satisfied at the [Ingester] boundary in the unit test
// `handler_test.go` -- the test wires the [Ingester] with
// ONLY a [ChurnEventWriter] (no [MetricSampleWriter] in
// scope) and asserts that after Ingest() the in-memory
// `metric_sample` store remains empty by construction (the
// store doesn't exist in this package). The same property is
// re-asserted at the HTTP verb level in
// `internal/ingest/webhook/churn_verb_test.go`, which now
// constructs an [InMemoryChurnEventStore] (not a metric
// sample writer) and checks staged event count.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/gofrs/uuid"
)

// ChurnEventWriter is the staging-table writer the [Ingester]
// depends on. The production implementation
// ([PGChurnEventStore] in `pg_churn_event_store.go`) is a
// PG-backed adapter that issues a chunked, transactionally
// wrapped `INSERT INTO clean_code.churn_event` using the
// canonical column set from
// `internal/db/schema/clean_code/churn_event.sql`. The
// unit-test implementation ([InMemoryChurnEventStore]) keeps
// a slice in memory.
//
// # Atomicity contract
//
// WriteChurnEvents MUST be all-or-nothing per call: either
// every event in `events` is durable, or none is. The
// production adapter satisfies this by opening its OWN
// `BEGIN/COMMIT` transaction inside the call, executing N
// chunked multi-row INSERTs against that TX, and rolling
// back on the first chunk error (see
// [PGChurnEventStore.WriteChurnEvents] for the chunking
// rationale and bind-parameter limit). The TX is NOT shared
// with the Router's scan_run-open TX; the two writes are
// sequenced (scan_run claim first, then churn_event INSERT)
// rather than co-located. The in-memory implementation
// satisfies the contract by appending under a single mutex.
//
// # Idempotency contract
//
// The Router enforces (verb, payload_hash) one-shot at
// scan_run-open time via the partial unique index from
// migration 0009 -- that is the AUTHORITATIVE dedupe anchor
// for replayed deliveries. The CHURN_EVENT-LEVEL unique
// constraint `churn_event_scan_run_row_uniq` on
// (scan_run_id, payload_row_index) from migration 0010 is
// defence-in-depth: it catches a regression where the verb
// re-fires INSIDE one open scan_run claim. The production
// writer issues plain `INSERT` (NOT `ON CONFLICT DO NOTHING`)
// because at this layer the Router has already deduped
// replayed payloads, so a row-level duplicate signals a
// regression and 23505 is the loudest failure mode.
type ChurnEventWriter interface {
	WriteChurnEvents(ctx context.Context, events []ChurnEvent) error
}

// ChurnEventReader is the read side of the staging table the
// `modification_count_in_window` materialiser pulls from on
// its next pass. Exposed as a small interface so the
// materialiser's PG-backed reader and the
// [InMemoryChurnEventStore] test fake share the same shape.
//
// Returns events with `repo_id = repoID` and `modified_at
// >= since`, ordered by `(modified_at DESC, created_at DESC,
// churn_event_id)` so the materialiser's per-scope dedupe
// observes the newest commit first (the dedupe keeps the
// representative SHA for the window's max-recency).
type ChurnEventReader interface {
	ListChurnEventsForRepo(ctx context.Context, repoID uuid.UUID, since time.Time) ([]ChurnEvent, error)
}

// InMemoryChurnEventStore is the test fake that implements
// BOTH [ChurnEventWriter] and [ChurnEventReader]. Production
// code MUST NOT use this -- the writer has no durable
// persistence and rebuilds on every process restart.
//
// The fake is safe for concurrent use.
type InMemoryChurnEventStore struct {
	mu     sync.Mutex
	events []ChurnEvent
}

// NewInMemoryChurnEventStore returns a fresh, empty store.
func NewInMemoryChurnEventStore() *InMemoryChurnEventStore {
	return &InMemoryChurnEventStore{events: nil}
}

// WriteChurnEvents implements [ChurnEventWriter]. Appends
// the slice contents under a single mutex; an empty slice
// is a no-op (the [Ingester] never calls WriteChurnEvents
// with len 0 because [Payload.Validate] rejects empty Rows,
// but the writer interface tolerates it for symmetry).
func (s *InMemoryChurnEventStore) WriteChurnEvents(_ context.Context, events []ChurnEvent) error {
	if len(events) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Defence-in-depth: enforce the unique-row invariant the
	// migration's UNIQUE (scan_run_id, payload_row_index)
	// constraint pins. A regression in the [Ingester] would
	// otherwise silently stage duplicates in the fake.
	for _, ev := range events {
		for _, existing := range s.events {
			if existing.ScanRunID == ev.ScanRunID && existing.PayloadRowIndex == ev.PayloadRowIndex {
				return fmt.Errorf("churn: InMemoryChurnEventStore: duplicate (scan_run_id=%s, payload_row_index=%d) -- regression vs the migration 0010 unique constraint", ev.ScanRunID, ev.PayloadRowIndex)
			}
		}
	}
	s.events = append(s.events, events...)
	return nil
}

// ListChurnEventsForRepo implements [ChurnEventReader]. The
// returned slice is a freshly-allocated copy (callers MAY
// mutate it without affecting the store), sorted by
// (modified_at DESC, created_at DESC, churn_event_id).
func (s *InMemoryChurnEventStore) ListChurnEventsForRepo(_ context.Context, repoID uuid.UUID, since time.Time) ([]ChurnEvent, error) {
	if repoID == uuid.Nil {
		return nil, errors.New("churn: ListChurnEventsForRepo: repo_id is the zero UUID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ChurnEvent, 0, len(s.events))
	for _, ev := range s.events {
		if ev.RepoID != repoID {
			continue
		}
		if !since.IsZero() && ev.ModifiedAt.Before(since) {
			continue
		}
		out = append(out, ev)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].ModifiedAt.Equal(out[j].ModifiedAt) {
			return out[i].ModifiedAt.After(out[j].ModifiedAt)
		}
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ChurnEventID.String() < out[j].ChurnEventID.String()
	})
	return out, nil
}

// Len returns the total number of staged events (across all
// repos / scan_runs). Test helper.
func (s *InMemoryChurnEventStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// IngestResult is returned by [Ingester.Ingest] on success.
// Carries the scan_run_id the events were written under
// (echoed from the input handle so callers don't need to
// re-thread it) and the number of staged events. Future
// fields (e.g. a slice of minted ChurnEventIDs for audit)
// can be added without breaking the existing call sites.
type IngestResult struct {
	// ScanRunID is the parent scan_run all events were
	// written under (== handle.ScanRunID).
	ScanRunID uuid.UUID
	// RepoID is the parent repo (== handle.RepoID ==
	// payload.RepoID).
	RepoID uuid.UUID
	// EventsWritten is len(payload.Rows) iff every row was
	// staged. An error result returns a zero IngestResult,
	// so a non-zero EventsWritten always corresponds to a
	// successful all-or-nothing write.
	EventsWritten int
	// StagedAt is the clock reading the [Ingester] stamped
	// on every [ChurnEvent.CreatedAt]. Returned so callers
	// (e.g. the future webhook handler) can correlate audit
	// logs without re-reading the events.
	StagedAt time.Time
}

// ErrRepoIDMismatch is returned when the [Payload.RepoID]
// does not equal [ScanRunHandle.RepoID]. The Router opens
// the scan_run with the repo_id resolved from the signing-
// key registry (architecture Sec 3.12); a payload that
// disagrees is either a publisher bug or a misrouted
// delivery and MUST NOT be staged (staging would attribute
// the rows to the wrong repo's modification_count_in_window
// rollup).
var ErrRepoIDMismatch = errors.New("churn: payload.repo_id != scan_run.repo_id")

// ErrChurnEventWriteFailed wraps the underlying writer error
// so the HTTP handler stage can `errors.Is` it without
// importing the writer's concrete error type.
var ErrChurnEventWriteFailed = errors.New("churn: writing churn_event rows failed")

// Ingester is the package's entry point for the
// `ingest.churn` verb. It is deliberately small (one method,
// three dependencies):
//
//   - eventWriter -- the staging-table writer.
//   - now -- the clock reading used for [ChurnEvent.CreatedAt]
//     and for the returned [IngestResult.StagedAt].
//   - newUUID -- the per-row [ChurnEvent.ChurnEventID]
//     minter.
//
// Notably absent (by design): no `MetricSampleWriter`, no
// `Materialiser`, no `ScopeResolver`. The Stage 4.4 verb
// stages rows only -- the materialiser does its own scope
// resolution and metric_sample emission on a later pass.
//
// The lack of those fields is the STRUCTURAL PROOF of the
// "writes ZERO metric_sample" contract (e2e-scenarios.md
// lines 658-664).
type Ingester struct {
	eventWriter ChurnEventWriter
	now         func() time.Time
	newUUID     func() (uuid.UUID, error)
}

// NewIngester returns an [Ingester] bound to `writer`.
// PANICS on nil writer -- a verb that cannot stage events
// has no purpose, so the misconfig is a wiring bug that
// should fail loudly at composition-root time.
//
// The clock defaults to [time.Now] and the UUID minter
// defaults to [uuid.NewV4] (the gofrs/uuid v4 package's
// canonical random UUID generator -- matches the existing
// repo convention in `internal/evaluator/production_gate.go`).
// Tests use [NewIngesterWithClocks] to inject deterministic
// fixtures.
func NewIngester(writer ChurnEventWriter) *Ingester {
	if writer == nil {
		panic("churn: NewIngester received nil ChurnEventWriter")
	}
	return &Ingester{
		eventWriter: writer,
		now:         time.Now,
		newUUID:     uuid.NewV4,
	}
}

// NewIngesterWithClocks returns an [Ingester] with both the
// clock and the UUID minter swapped for test fakes. PANICS
// on any nil argument.
func NewIngesterWithClocks(writer ChurnEventWriter, now func() time.Time, newUUID func() (uuid.UUID, error)) *Ingester {
	if writer == nil {
		panic("churn: NewIngesterWithClocks received nil ChurnEventWriter")
	}
	if now == nil {
		panic("churn: NewIngesterWithClocks received nil now()")
	}
	if newUUID == nil {
		panic("churn: NewIngesterWithClocks received nil newUUID()")
	}
	return &Ingester{
		eventWriter: writer,
		now:         now,
		newUUID:     newUUID,
	}
}

// Ingest stages the payload's rows into the
// `clean_code.churn_event` table under the pre-opened
// scan_run handle. It is the SOLE public method on
// [Ingester]; the unit tests under `ingest_test.go`
// exercise its full surface.
//
// Order of operations (all-or-nothing -- a failure at any
// step short-circuits before the writer call):
//
//  1. [ValidateScanRunHandle] -- reject non-canonical
//     scan_run shapes (verb, kind, sha_binding, to_sha).
//  2. [Payload.Validate] (via [BuildChurnEvents]) -- reject
//     malformed payloads (zero RepoID, zero rows, malformed
//     SHA, empty file_path, zero ModifiedAt).
//  3. RepoID cross-check -- reject when
//     payload.RepoID != handle.RepoID.
//  4. [BuildChurnEvents] -- mint per-row IDs and stamp
//     CreatedAt.
//  5. [ChurnEventWriter.WriteChurnEvents] -- batched
//     all-or-nothing write.
//
// On success, returns an [IngestResult] with the staged
// count. On failure, returns a zero IngestResult and an
// error.
func (i *Ingester) Ingest(ctx context.Context, handle ScanRunHandle, payload *Payload) (IngestResult, error) {
	if ctx == nil {
		return IngestResult{}, errors.New("churn: Ingest received nil context")
	}
	if err := ValidateScanRunHandle(handle); err != nil {
		return IngestResult{}, err
	}
	if payload == nil {
		return IngestResult{}, errors.New("churn: Ingest received nil Payload")
	}
	// Stamp `now` BEFORE BuildChurnEvents so the
	// IngestResult.StagedAt and every ChurnEvent.CreatedAt
	// share the same clock reading.
	stagedAt := i.now()
	if stagedAt.IsZero() {
		return IngestResult{}, ErrZeroNow
	}
	if err := payload.Validate(); err != nil {
		return IngestResult{}, err
	}
	if payload.RepoID != handle.RepoID {
		return IngestResult{}, fmt.Errorf("%w (payload.repo_id=%s, scan_run.repo_id=%s)", ErrRepoIDMismatch, payload.RepoID, handle.RepoID)
	}
	events, err := BuildChurnEvents(handle.ScanRunID, payload, stagedAt, i.newUUID)
	if err != nil {
		return IngestResult{}, err
	}
	if err := i.eventWriter.WriteChurnEvents(ctx, events); err != nil {
		return IngestResult{}, fmt.Errorf("%w: %w", ErrChurnEventWriteFailed, err)
	}
	return IngestResult{
		ScanRunID:     handle.ScanRunID,
		RepoID:        handle.RepoID,
		EventsWritten: len(events),
		StagedAt:      stagedAt,
	}, nil
}
