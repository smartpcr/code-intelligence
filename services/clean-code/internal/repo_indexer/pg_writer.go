package repo_indexer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/gofrs/uuid"
	"github.com/lib/pq"
)

// commitTable is the unqualified `clean_code.commit` table
// name the PG-backed writer targets. Qualified at
// statement-build time via [pq.QuoteIdentifier] so a hostile
// schema injection produces a syntactically-broken statement
// rather than a successful unintended write.
const commitTable = "commit"

// repoEventTable is the unqualified `clean_code.repo_event`
// table name the writer targets.
const repoEventTable = "repo_event"

// repoEventKindRegistered is the canonical past-tense
// repo_event kind literal (architecture Sec 5.1.4 line 883).
// Pinned as a constant here so a future stage that renames
// the enum value MUST touch this file -- making the
// "past-tense canon" invariant grep-friendly.
const repoEventKindRegistered = "registered"

// repoIndexerLockNamespace isolates the writer's advisory
// locks from any other component sharing the same PostgreSQL
// instance. Two-int4 `pg_advisory_xact_lock(int4, int4)`
// variant. ASCII spells `CCRI` (Clean-Code Repo-Indexer).
const repoIndexerLockNamespace int32 = 0x43435249

// PGCatalogWriter is the production PostgreSQL-backed
// implementation of [CatalogWriter]. It executes both the
// `clean_code.commit` INSERT and the
// `clean_code.repo_event(kind='registered')` INSERT inside
// a SINGLE TRANSACTION, satisfying the [CatalogWriter]
// atomicity contract (rubber-duck iter-1 #1) and architecture
// G1 (the Repo Indexer is the SOLE writer of new commit
// rows AND it never names `scan_status` on INSERT so the
// DB DEFAULT 'pending' supplies the value).
//
// # Per-repo advisory lock
//
// Two concurrent first-SHA webhook deliveries for the SAME
// repo would otherwise both observe "no registered event
// exists" in their respective transaction snapshots and
// each INSERT one -- duplicating the registered event. The
// writer acquires `pg_advisory_xact_lock(NAMESPACE, hash32(repo_id))`
// at transaction start so the SECOND delivery blocks until
// the first commits; the second's `SELECT 1 FROM repo_event
// WHERE kind='registered'` then sees the first's INSERT and
// returns `EventInserted=false`. The lock namespace
// ([repoIndexerLockNamespace]) is distinct from the
// `scope_binding` writer's namespace so the two writers
// never contend on the same key.
//
// Architecture anchors:
//   - Sec 1.5 G1 (Repo Indexer = sole writer of commit rows)
//   - Sec 1.5.1 row 1 (Repo Indexer never UPDATEs scan_status)
//   - Sec 3.3 (Repo Indexer responsibilities)
//   - Sec 5.1.2 line 864 (commit.scan_status DEFAULT 'pending')
//   - Sec 5.1.4 lines 877-884 (RepoEvent.kind canonical
//     past-tense set)
type PGCatalogWriter struct {
	db     *sql.DB
	schema string
}

// NewPGCatalogWriter wraps `db` using the canonical
// `clean_code` schema. Production callers reach this
// constructor; test code MAY use [NewPGCatalogWriterWithSchema]
// to land on an isolated schema. Returns a non-nil error
// when `db` is nil so the misconfiguration surfaces at
// composition-root wire-up rather than at first webhook.
func NewPGCatalogWriter(db *sql.DB) (*PGCatalogWriter, error) {
	return NewPGCatalogWriterWithSchema(db, defaultSchema)
}

// defaultSchema mirrors `internal/storage.SchemaName` but is
// pinned as a constant here so the [repo_indexer] package
// does not import the storage package (the dependency would
// be one-way only -- storage today does not import
// [repo_indexer] -- but the [repo_indexer] package's contract
// is self-contained and is easier to reason about without
// the cross-package coupling).
const defaultSchema = "clean_code"

// NewPGCatalogWriterWithSchema is the test-friendly
// constructor: callers inject a non-default PostgreSQL
// schema (e.g. `clean_code_indexer_test`). Both inputs
// are validated; nil db OR empty schema is a programmer
// bug surfaced at construction time, not at first call.
func NewPGCatalogWriterWithSchema(db *sql.DB, schema string) (*PGCatalogWriter, error) {
	if db == nil {
		return nil, errors.New("repo_indexer: NewPGCatalogWriter: *sql.DB is nil")
	}
	if strings.TrimSpace(schema) == "" {
		return nil, errors.New("repo_indexer: NewPGCatalogWriterWithSchema: schema is empty")
	}
	return &PGCatalogWriter{db: db, schema: schema}, nil
}

// qualifyCommit returns the schema-qualified, properly
// quoted `<schema>.commit` identifier for use in raw SQL.
// The table name is a constant in this file (not user
// input) so the quote is defense-in-depth.
func (w *PGCatalogWriter) qualifyCommit() string {
	return pq.QuoteIdentifier(w.schema) + "." + pq.QuoteIdentifier(commitTable)
}

// qualifyRepoEvent returns the schema-qualified, properly
// quoted `<schema>.repo_event` identifier.
func (w *PGCatalogWriter) qualifyRepoEvent() string {
	return pq.QuoteIdentifier(w.schema) + "." + pq.QuoteIdentifier(repoEventTable)
}

// repoLockKey hashes `repoID` to a deterministic 32-bit
// signed value the writer passes as the second argument of
// `pg_advisory_xact_lock(int4, int4)`. Using FNV-32 (not a
// cryptographic hash) is intentional -- the lock key only
// needs distinctness across repos, NOT collision resistance;
// a hostile collision still serialises on the lock, it just
// has to wait for the unrelated repo's transaction to commit.
func repoLockKey(repoID uuid.UUID) int32 {
	h := fnv.New32a()
	_, _ = h.Write(repoID.Bytes())
	// Convert uint32 -> int32 via a bit reinterpretation so
	// the full 32-bit space is reachable as a Postgres int4.
	return int32(h.Sum32()) //nolint:gosec // overflow-on-purpose: int4 wraparound is intentional
}

// EnsureCommitAndRegisteredEvent implements [CatalogWriter]
// against a real PostgreSQL handle.
//
// # SQL shape (executed inside a single transaction)
//
//	BEGIN;
//	SELECT pg_advisory_xact_lock(NAMESPACE, hash32(repo_id));
//	INSERT INTO <schema>.commit
//	    (repo_id, sha, parent_sha, committed_at)
//	    VALUES ($1, $2, NULLIF($3, ''), $4)
//	    ON CONFLICT (repo_id, sha) DO NOTHING
//	    RETURNING 1;          -- commitInserted iff one row scans
//	-- inspect (repo_id, kind='registered') existence
//	SELECT 1 FROM <schema>.repo_event
//	    WHERE repo_id = $1 AND kind = 'registered' LIMIT 1;
//	-- if NOT present:
//	INSERT INTO <schema>.repo_event (repo_id, kind)
//	    VALUES ($1, 'registered');  -- eventInserted = true
//	COMMIT;
//
// The commit INSERT INTENTIONALLY OMITS `scan_status` from
// the column list -- the migration 0001 line 229 DEFAULT
// `'pending'` supplies the initial value. This preserves
// architecture Sec 1.5.1 row 1: "Repo Indexer never writes
// scan_status; only the Metric Ingestor does".
//
// The `repo_event` INSERT names ONLY `(repo_id, kind)` and
// relies on the `payload_json` DEFAULT (`'{}'::jsonb`) and
// the `created_at` DEFAULT (`now()`) supplied by migration
// 0001 lines 315-318.
func (w *PGCatalogWriter) EnsureCommitAndRegisteredEvent(ctx context.Context, req CommitEnsureRequest) (CommitEnsureResult, error) {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return CommitEnsureResult{}, fmt.Errorf("repo_indexer: BeginTx: %w", err)
	}
	defer func() {
		// Safe even after a successful Commit; documented
		// no-op per database/sql/sql.go (Tx.Rollback returns
		// ErrTxDone which we intentionally drop).
		_ = tx.Rollback()
	}()

	// Acquire the per-repo advisory lock so concurrent
	// first-SHA deliveries for the SAME repo serialise.
	// The lock auto-releases on COMMIT / ROLLBACK -- no
	// explicit `pg_advisory_unlock` is needed.
	if _, lockErr := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock($1, $2)`,
		repoIndexerLockNamespace, repoLockKey(req.RepoID),
	); lockErr != nil {
		return CommitEnsureResult{}, fmt.Errorf("repo_indexer: pg_advisory_xact_lock: %w", lockErr)
	}

	// Insert the commit row. The `NULLIF($3, '')` cast
	// translates the application-layer empty-string
	// convention to the DB NULL the schema expects for the
	// first commit of a repo (migration 0001 line 221:
	// `parent_sha text` -- nullable).
	commitSQL := fmt.Sprintf(
		`INSERT INTO %s (repo_id, sha, parent_sha, committed_at)
		 VALUES ($1, $2, NULLIF($3, ''), $4)
		 ON CONFLICT (repo_id, sha) DO NOTHING
		 RETURNING 1`,
		w.qualifyCommit(),
	)
	var commitProbe int
	commitErr := tx.QueryRowContext(ctx, commitSQL,
		req.RepoID, req.SHA, req.ParentSHA, req.CommittedAt,
	).Scan(&commitProbe)
	var commitInserted bool
	switch {
	case commitErr == nil:
		commitInserted = true
	case errors.Is(commitErr, sql.ErrNoRows):
		commitInserted = false
	default:
		return CommitEnsureResult{}, fmt.Errorf("repo_indexer: INSERT commit: %w", commitErr)
	}

	// Check for an existing registered event under the lock.
	// LIMIT 1 + a `SELECT 1` projection so the probe is the
	// cheapest possible existence test (no row content
	// fetched, single-row read against the
	// `repo_event_repo_created_idx` covering index).
	eventSQL := fmt.Sprintf(
		`SELECT 1 FROM %s WHERE repo_id = $1 AND kind = $2 LIMIT 1`,
		w.qualifyRepoEvent(),
	)
	var eventProbe int
	eventErr := tx.QueryRowContext(ctx, eventSQL, req.RepoID, repoEventKindRegistered).Scan(&eventProbe)
	var eventInserted bool
	switch {
	case eventErr == nil:
		eventInserted = false
	case errors.Is(eventErr, sql.ErrNoRows):
		// No registered event yet -- INSERT it. The
		// per-repo advisory lock serialises this branch
		// against concurrent first-SHA deliveries so we
		// cannot race past another transaction's INSERT.
		insertEventSQL := fmt.Sprintf(
			`INSERT INTO %s (repo_id, kind) VALUES ($1, $2)`,
			w.qualifyRepoEvent(),
		)
		if _, insErr := tx.ExecContext(ctx, insertEventSQL,
			req.RepoID, repoEventKindRegistered,
		); insErr != nil {
			return CommitEnsureResult{}, fmt.Errorf("repo_indexer: INSERT repo_event(kind='registered'): %w", insErr)
		}
		eventInserted = true
	default:
		return CommitEnsureResult{}, fmt.Errorf("repo_indexer: SELECT repo_event(kind='registered'): %w", eventErr)
	}

	if commitErr := tx.Commit(); commitErr != nil {
		return CommitEnsureResult{}, fmt.Errorf("repo_indexer: Commit: %w", commitErr)
	}

	return CommitEnsureResult{
		CommitInserted: commitInserted,
		EventInserted:  eventInserted,
	}, nil
}

// Compile-time assertion that [PGCatalogWriter] satisfies the
// [CatalogWriter] interface. If a future refactor breaks the
// signature, the build fails here BEFORE any test runs.
var _ CatalogWriter = (*PGCatalogWriter)(nil)
