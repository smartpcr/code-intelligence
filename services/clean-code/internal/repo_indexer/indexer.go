package repo_indexer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/uuid"
)

// Sentinel errors for the Indexer pipeline. All wrap a
// stable identity so callers (HTTP webhook, CLI rescan
// command) can `errors.Is` to map to structured responses
// without parsing text.
var (
	// ErrZeroRepoID is returned when a [CommitEnsureRequest]
	// carries the zero `repo_id` UUID. Legitimate
	// `clean_code.repo` rows reference UUIDs minted via
	// `gen_random_uuid()` which never returns zero, so a
	// zero at the Indexer layer is always an uninitialised
	// caller value.
	ErrZeroRepoID = errors.New("repo_indexer: CommitEnsureRequest.RepoID is the zero UUID")
	// ErrEmptySHA is returned when [CommitEnsureRequest.SHA]
	// is empty (whitespace-only or zero-length).
	ErrEmptySHA = errors.New("repo_indexer: CommitEnsureRequest.SHA is empty")
	// ErrInvalidSHA is returned when [CommitEnsureRequest.SHA]
	// is non-empty but does not match the canonical
	// 40-character hex commit-SHA shape. Mirrors the same
	// guard used by `internal/ingest/churn` so the Indexer
	// and the churn webhook agree on the SHA-shape contract.
	ErrInvalidSHA = errors.New("repo_indexer: CommitEnsureRequest.SHA is not a 40-character hex commit SHA")
	// ErrInvalidParentSHA is returned when
	// [CommitEnsureRequest.ParentSHA] is non-empty AND does
	// not match the canonical 40-char hex shape. An empty
	// ParentSHA is PERMITTED -- the first commit of a repo
	// has no parent (architecture Sec 5.1.2 line 862:
	// "Nullable for the first commit of a repo").
	ErrInvalidParentSHA = errors.New("repo_indexer: CommitEnsureRequest.ParentSHA is not a 40-character hex commit SHA")
	// ErrZeroCommittedAt is returned when
	// [CommitEnsureRequest.CommittedAt] is the zero time.
	// The `clean_code.commit.committed_at` column is
	// `NOT NULL`; the Indexer refuses zero-valued
	// timestamps so the DB-side constraint cannot fire on
	// a writer bug.
	ErrZeroCommittedAt = errors.New("repo_indexer: CommitEnsureRequest.CommittedAt is the zero time")
	// ErrCatalogWriterFailure wraps any non-validation
	// error returned by
	// [CatalogWriter.EnsureCommitAndRegisteredEvent]. The
	// HTTP webhook stage maps this to 500 + a structured
	// code.
	ErrCatalogWriterFailure = errors.New("repo_indexer: CatalogWriter.EnsureCommitAndRegisteredEvent failed")
)

// shaRegex is the strict canonical pattern for a commit SHA:
// exactly 40 hexadecimal characters, no leading/trailing
// whitespace, case-insensitive (Git emits lowercase but
// upstream consumers MAY upper-case). Mirrors the pattern
// pinned in `internal/ingest/churn` so the two ingest
// surfaces share one canonical SHA shape.
var shaRegex = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// CommitEnsureRequest is the per-SHA payload the
// [CatalogWriter] needs to (a) insert one `commit` row
// and (b) atomically ensure exactly one
// `repo_event(kind='registered')` row exists for the
// parent repo.
//
// Designed as a single value type (NOT separate args) so
// future fields (e.g. `Author` for `knowledge_index`,
// `signed_by` for verified-commit metadata) can be added
// without breaking the writer signature.
type CommitEnsureRequest struct {
	// RepoID is the `clean_code.repo.repo_id` the commit
	// belongs to. MUST be a non-zero UUID; the writer
	// asserts via FK against `clean_code.repo`.
	RepoID uuid.UUID
	// SHA is the 40-char hex commit SHA. The Indexer
	// rejects any other shape pre-DB so the
	// `clean_code.commit.sha` text column never receives a
	// malformed value.
	SHA string
	// ParentSHA is the parent commit's SHA, or empty for
	// the first commit of a repo (architecture Sec 5.1.2
	// line 862). When non-empty MUST also be 40-char hex.
	// The writer persists the empty string as SQL NULL.
	ParentSHA string
	// CommittedAt is the author/committer timestamp from
	// git (architecture Sec 5.1.2 line 863). MUST be
	// non-zero; the column is `NOT NULL` in the schema.
	CommittedAt time.Time
	// Ref is the optional Git ref the webhook delivered
	// (e.g. `refs/heads/main`). Stage 3.1 does NOT act on
	// this field; it is preserved on the request so a
	// later stage (`repo.default_branch_head` maintenance)
	// can consume it without changing the wire contract.
	// Architecture Sec 5.1.1 lines 869-870 names the Repo
	// Indexer as the writer of `default_branch_head`; that
	// work lands in a follow-up stage.
	Ref string
}

// Validate returns nil iff every contract on the request
// holds. Wrapped sentinels so the HTTP webhook stage can
// map each error to a canonical 400 code.
//
// Checks (in declaration order, cheapest first):
//
//  1. [CommitEnsureRequest.RepoID] is not the zero UUID
//     ([ErrZeroRepoID]).
//  2. [CommitEnsureRequest.SHA] is non-empty
//     ([ErrEmptySHA]).
//  3. [CommitEnsureRequest.SHA] is 40-char hex
//     ([ErrInvalidSHA]).
//  4. [CommitEnsureRequest.ParentSHA], if non-empty, is
//     40-char hex ([ErrInvalidParentSHA]).
//  5. [CommitEnsureRequest.CommittedAt] is non-zero
//     ([ErrZeroCommittedAt]).
func (r CommitEnsureRequest) Validate() error {
	if r.RepoID == uuid.Nil {
		return ErrZeroRepoID
	}
	if strings.TrimSpace(r.SHA) == "" {
		return ErrEmptySHA
	}
	if !shaRegex.MatchString(r.SHA) {
		return fmt.Errorf("%w (got %q)", ErrInvalidSHA, r.SHA)
	}
	if r.ParentSHA != "" && !shaRegex.MatchString(r.ParentSHA) {
		return fmt.Errorf("%w (got %q)", ErrInvalidParentSHA, r.ParentSHA)
	}
	if r.CommittedAt.IsZero() {
		return ErrZeroCommittedAt
	}
	return nil
}

// CommitEnsureResult records what the [CatalogWriter] did
// for one [CommitEnsureRequest]. The pair `(CommitInserted,
// EventInserted)` lets the caller log a structured outcome
// and lets tests assert the idempotent / first-time
// distinctions without scraping logs.
//
// The four legal combinations:
//
//   - (true, true)   -- a fresh repo's first commit:
//     commit row INSERTed AND registered event APPENDed.
//   - (true, false)  -- a returning repo's new commit:
//     commit row INSERTed; registered event already
//     existed.
//   - (false, true)  -- pathological: never returned by
//     the production writer (a duplicate commit means the
//     repo's first commit already landed, which means the
//     registered event already exists). The in-memory
//     fake reflects this constraint -- duplicate commit
//     implies EventInserted=false.
//   - (false, false) -- a duplicate webhook delivery:
//     nothing changed, return 200 OK.
type CommitEnsureResult struct {
	// CommitInserted is true iff a new `commit` row landed.
	// False on duplicate (repo_id, sha) PK conflict.
	CommitInserted bool
	// EventInserted is true iff a new
	// `repo_event(kind='registered')` row landed for the
	// parent repo. False if a registered event already
	// existed.
	EventInserted bool
}

// CatalogWriter is the SINGLE persistence seam the Repo
// Indexer writes through. The production implementation
// (PG-backed; lands in a later stage) MUST satisfy:
//
//  1. Insert the `commit` row naming ONLY
//     `(repo_id, sha, parent_sha, committed_at)` -- OMIT
//     `scan_status` so the schema-level DEFAULT 'pending'
//     supplies the value (architecture Sec 3.3 /
//     Sec 5.1.2). The application layer NEVER names
//     `scan_status` on INSERT, preserving the Sec 1.5.1
//     row 1 invariant that the Metric Ingestor is the only
//     application writer of that column.
//  2. On a `(repo_id, sha)` PK conflict, return
//     `CommitInserted=false` and DO NOT error -- duplicate
//     webhook delivery is canonical (Stage 3.1 test
//     scenario "duplicate SHA event is a no-op"). The
//     idiomatic SQL is
//     `INSERT ... ON CONFLICT (repo_id, sha) DO NOTHING
//     RETURNING 1` -- pgx returns `ErrNoRows` on conflict
//     which the writer maps to `CommitInserted=false`.
//  3. ATOMICALLY (same transaction as step 1) ensure
//     exactly one `repo_event(kind='registered')` row
//     exists for `RepoID`. If a row already exists,
//     return `EventInserted=false`; otherwise INSERT and
//     return `EventInserted=true`. The atomic guarantee
//     protects against the partial-write race where a
//     commit lands but the registered event is lost
//     (rubber-duck iter-1 #1).
//  4. Concurrency: under concurrent webhook delivery for
//     the SAME repo, the writer MUST not produce two
//     `registered` events. The recommended shape is a
//     transaction-scoped advisory lock keyed by `RepoID`
//     OR a unique partial index
//     `WHERE kind='registered'` on `repo_event`. The
//     interface contract is "exactly one registered event
//     per repo"; the implementation chooses the
//     enforcement mechanism.
//
// The Stage 3.1 deliverable is the interface + an
// in-memory fake; the PG-backed implementation lands with
// the Stage 3.2 Metric Ingestor wiring stage.
type CatalogWriter interface {
	// EnsureCommitAndRegisteredEvent performs the
	// transactional insert-or-noop described above.
	// Returns a [CommitEnsureResult] reporting what
	// happened, or an error wrapped by
	// [ErrCatalogWriterFailure] (the Indexer adds the wrap
	// at its call site so implementations only need to
	// return the raw cause).
	EnsureCommitAndRegisteredEvent(ctx context.Context, req CommitEnsureRequest) (CommitEnsureResult, error)
}

// InMemoryCatalogWriter is a [CatalogWriter] implemented
// against in-process maps. Used by:
//
//  1. The unit tests in this package and in
//     `internal/repo_indexer/handler_test.go`.
//  2. The early skeletal `cmd/clean-coded` composition
//     root before the PG-backed writer lands (a developer
//     can spin up the webhook end-to-end against this
//     fake without provisioning a database).
//
// Concurrent calls are serialised internally so the
// writer is safe to share across parallel webhook
// deliveries. The implementation mirrors the production
// PG-backed writer's atomic guarantee under contention
// because every call takes the same `mu` -- two
// concurrent first-commit deliveries for the same repo
// will linearise and the second will observe the
// registered event already exists.
type InMemoryCatalogWriter struct {
	mu sync.Mutex
	// commits is keyed by (repo_id, sha). The value
	// carries the persisted columns PLUS the canonical
	// scan_status the DB DEFAULT would supply -- always
	// [ScanStatusPending] on INSERT. The Indexer NEVER
	// writes the scan_status column; the fake simulates
	// the DB-side default so the test scenario
	// `new-sha-inserts-pending` can assert the canonical
	// initial value.
	commits map[commitKey]CommitRecord
	// registeredRepos is the set of repos that already have
	// a `repo_event(kind='registered')` row. Membership
	// determines [CommitEnsureResult.EventInserted] on the
	// next call.
	registeredRepos map[uuid.UUID]bool
	// events is the append-only log of every
	// `repo_event` row the writer has materialised.
	// Inspected by tests to assert the exactly-one
	// registered-event invariant.
	events []RepoEventRecord
	// failNext is the test escape hatch: when non-nil the
	// next EnsureCommitAndRegisteredEvent returns this
	// error (and consumes the value, so a second call
	// runs normally).
	failNext error
}

type commitKey struct {
	repoID uuid.UUID
	sha    string
}

// CommitRecord is the in-memory shape of one persisted
// `clean_code.commit` row. Mirrors the schema columns
// MINUS the `committed_at`-vs-`created_at` distinction
// (this fake stores neither because the tests assert on
// the inputs, not on a fake-generated `created_at`).
//
// `ScanStatus` is exposed so tests can pin the
// "DB DEFAULT supplies pending" semantic without
// reaching for SQL fixtures.
type CommitRecord struct {
	RepoID      uuid.UUID
	SHA         string
	ParentSHA   string
	CommittedAt time.Time
	// ScanStatus mirrors the schema DEFAULT. ALWAYS
	// [ScanStatusPending] for an [InMemoryCatalogWriter]
	// row because the Indexer NEVER names this column on
	// INSERT and the fake supplies the DB-default value.
	ScanStatus ScanStatus
}

// RepoEventRecord is the in-memory shape of one persisted
// `clean_code.repo_event` row. The [InMemoryCatalogWriter]
// only materialises `kind='registered'` rows today; future
// stages MAY extend.
type RepoEventRecord struct {
	RepoID  uuid.UUID
	Kind    string
	Payload map[string]any
}

// NewInMemoryCatalogWriter returns a fresh
// [InMemoryCatalogWriter] with empty state.
func NewInMemoryCatalogWriter() *InMemoryCatalogWriter {
	return &InMemoryCatalogWriter{
		commits:         make(map[commitKey]CommitRecord),
		registeredRepos: make(map[uuid.UUID]bool),
	}
}

// EnsureCommitAndRegisteredEvent implements
// [CatalogWriter]. The implementation is intentionally
// straight-line so the test fake's behaviour mirrors the
// PG implementation's documented contract (atomic insert
// + registered-event ensure).
//
// PostgreSQL-shape mapping (for the writer that lands
// later):
//
//	BEGIN;
//	INSERT INTO clean_code.commit
//	    (repo_id, sha, parent_sha, committed_at)
//	    VALUES ($1, $2, NULLIF($3, ''), $4)
//	    ON CONFLICT (repo_id, sha) DO NOTHING
//	    RETURNING 1;            -- xRow.Scan -> CommitInserted
//	-- if CommitInserted=false, COMMIT and return early.
//	INSERT INTO clean_code.repo_event
//	    (repo_id, kind)
//	    VALUES ($1, 'registered')
//	    ON CONFLICT DO NOTHING  -- relies on the unique
//	    RETURNING 1;            -- partial index on
//	-- xRow.Scan -> EventInserted             -- (repo_id) WHERE kind='registered'
//	COMMIT;
func (w *InMemoryCatalogWriter) EnsureCommitAndRegisteredEvent(_ context.Context, req CommitEnsureRequest) (CommitEnsureResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.failNext != nil {
		err := w.failNext
		w.failNext = nil
		return CommitEnsureResult{}, err
	}

	key := commitKey{repoID: req.RepoID, sha: req.SHA}
	var res CommitEnsureResult
	if _, exists := w.commits[key]; !exists {
		w.commits[key] = CommitRecord{
			RepoID:      req.RepoID,
			SHA:         req.SHA,
			ParentSHA:   req.ParentSHA,
			CommittedAt: req.CommittedAt,
			// Mirror the schema-level DEFAULT 'pending'
			// (migration 0001_catalog_lifecycle.up.sql:229).
			// The Indexer NEVER names this column on
			// INSERT; the fake stamps the canonical initial
			// value here so tests can pin the
			// `new-sha-inserts-pending` invariant.
			ScanStatus: ScanStatusPending,
		}
		res.CommitInserted = true
	}

	if !w.registeredRepos[req.RepoID] {
		w.registeredRepos[req.RepoID] = true
		w.events = append(w.events, RepoEventRecord{
			RepoID: req.RepoID,
			// Canonical past-tense kind per architecture
			// Sec 5.1.4 lines 877-884 -- NOT `register`.
			// Pinned as a string literal HERE (not
			// imported from a constant) so a grep for
			// `"registered"` returns the SAME hit on the
			// canonical wire literal that the DB enum
			// label and the architecture text use.
			Kind: "registered",
		})
		res.EventInserted = true
	}

	return res, nil
}

// Commits returns a snapshot of every `commit` row
// persisted so far. The slice is a fresh copy so concurrent
// mutation by tests is safe; ordering is non-deterministic
// (map iteration order) and tests SHOULD sort by SHA before
// asserting.
func (w *InMemoryCatalogWriter) Commits() []CommitRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]CommitRecord, 0, len(w.commits))
	for _, c := range w.commits {
		out = append(out, c)
	}
	return out
}

// Events returns a snapshot of every `repo_event` row
// persisted so far, in append order.
func (w *InMemoryCatalogWriter) Events() []RepoEventRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]RepoEventRecord, len(w.events))
	copy(out, w.events)
	return out
}

// FailNext arms the writer to return `err` from the next
// [InMemoryCatalogWriter.EnsureCommitAndRegisteredEvent]
// call (and only the next). Tests use this to exercise
// the writer-failure propagation path without a stub type.
func (w *InMemoryCatalogWriter) FailNext(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failNext = err
}

// Indexer is the production Repo Indexer service. Construct
// via [NewIndexer]; one instance handles every webhook
// delivery and CLI rescan trigger the service receives.
//
// # Stateless past dependencies
//
// The Indexer carries NO per-request state; every call
// threads through the [CatalogWriter]. This matches the
// "intentionally thin" characterisation in architecture
// Sec 3.3: "a few lines of glue between a git event source
// and the Catalog table".
type Indexer struct {
	writer CatalogWriter
	logger *slog.Logger
}

// NewIndexer returns an [Indexer] wired with `writer`.
// PANICS when `writer` is nil -- a writer-less Indexer
// cannot service any request and the composition-root
// misconfig should fail loudly at startup, not silently
// drop webhook deliveries. `logger` MAY be nil
// (request-level logging is silently disabled in that
// case).
func NewIndexer(writer CatalogWriter, logger *slog.Logger) *Indexer {
	if writer == nil {
		panic("repo_indexer: NewIndexer received nil CatalogWriter")
	}
	return &Indexer{
		writer: writer,
		logger: logger,
	}
}

// OnNewSHA is the single entrypoint both the HTTP webhook
// handler and the CLI rescan trigger call. The flow is:
//
//  1. [CommitEnsureRequest.Validate] -- structural guards
//     (non-zero RepoID, 40-char hex SHA, non-zero
//     CommittedAt, etc.). Each failure wraps the relevant
//     sentinel so the HTTP layer can map it to a structured
//     400 code.
//  2. [CatalogWriter.EnsureCommitAndRegisteredEvent] --
//     transactional commit + registered-event ensure.
//     Errors wrap [ErrCatalogWriterFailure] so the HTTP
//     layer maps them to 500.
//  3. Optional structured INFO log line so operators can
//     observe duplicate-webhook patterns.
//
// The Indexer NEVER:
//
//   - UPDATEs `commit.scan_status` (the Metric Ingestor's
//     sole responsibility per architecture Sec 1.5.1
//     row 1).
//   - INSERTs into any sub-store other than Catalog /
//     Lifecycle (architecture G1 ACL row for "Worker --
//     Repo Indexer").
//   - Reads or writes `metric_sample`, `evaluation_run`,
//     or any Audit/Measurement table.
func (i *Indexer) OnNewSHA(ctx context.Context, req CommitEnsureRequest) (CommitEnsureResult, error) {
	if err := req.Validate(); err != nil {
		return CommitEnsureResult{}, err
	}

	res, err := i.writer.EnsureCommitAndRegisteredEvent(ctx, req)
	if err != nil {
		// Multi-`%w` wrap (Go 1.20+) so callers can BOTH
		// `errors.Is(err, ErrCatalogWriterFailure)` to
		// classify the failure class AND
		// `errors.As(err, &pq.Error{})` (or
		// `errors.Is(err, context.Canceled)`, etc.) to
		// reach the writer's underlying cause. A prior
		// `%w: %v` formatting silently severed the inner
		// chain, hiding driver-level sentinels from the
		// HTTP layer.
		return CommitEnsureResult{}, fmt.Errorf("%w: %w", ErrCatalogWriterFailure, err)
	}

	if i.logger != nil {
		i.logger.Info("repo_indexer: OnNewSHA",
			"repo_id", req.RepoID,
			"sha", req.SHA,
			"parent_sha", req.ParentSHA,
			"ref", req.Ref,
			"commit_inserted", res.CommitInserted,
			"event_inserted", res.EventInserted,
		)
	}

	return res, nil
}
