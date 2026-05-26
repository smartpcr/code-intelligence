package churn

// Stage 4.4 (ingest churn verb feeds materialiser,
// implementation-plan.md lines 410-425) -- production
// PG-backed [ChurnEventWriter] and [ChurnEventReader]
// implementations.
//
// The Stage 4.4 [Ingester] is identity-agnostic at its
// public surface (it depends on the small [ChurnEventWriter]
// interface), so this file is the seam through which the
// composition root in `cmd/clean-code-metric-ingestor/main.go`
// wires real Postgres I/O.
//
// # Writer-ownership contract
//
// The `clean_code.churn_event` table is APPEND-ONLY at the
// application layer: migration 0010 GRANTs INSERT,SELECT to
// the `clean_code_metric_ingestor` role and REVOKEs
// UPDATE,DELETE (architecture Sec 4.4). The verb (which runs
// AS `clean_code_metric_ingestor`) is the SOLE writer; the
// materialiser is a READ-ONLY consumer on a later pass. The
// table's role grants enforce the contract at the DB layer
// so a future regression in this writer cannot mutate
// staged rows.
//
// # Schema qualification
//
// Statements are built with `lib/pq.QuoteIdentifier` to
// produce `"<schema>"."churn_event"`. The default schema is
// `clean_code`; tests inject a non-default schema name via
// [NewPGChurnEventStoreWithSchema] so they can sandbox a
// fixture-loaded schema without colliding with the live one.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofrs/uuid"
	"github.com/lib/pq"
)

// pgChurnEventDefaultSchema is the canonical schema name the
// migration 0010 lives under. Mirrors the
// `clean_code` constant in `internal/storage` and
// `internal/metric_ingestor.pgScanRunDefaultSchema`. The three
// MUST agree; a drift would break the FK from
// `churn_event.scan_run_id` to `scan_run.scan_run_id`.
const pgChurnEventDefaultSchema = "clean_code"

// pgChurnEventTable is the unqualified `churn_event` table
// name. Schema-qualified at statement-build time via
// [pq.QuoteIdentifier].
const pgChurnEventTable = "churn_event"

// pgChurnEventParamsPerRow is the count of bind parameters
// [WriteChurnEvents] sends per row of the multi-row INSERT.
// Pinned as a named constant because PG's wire-protocol
// parameter limit (uint16, 65535) divided by this number is
// the hard upper bound on rows-per-statement -- a refactor
// that drifts the column list MUST update this constant in
// the same change.
const pgChurnEventParamsPerRow = 9

// pgChurnEventMaxRowsPerInsert caps a single INSERT
// statement's row count so the bind-parameter count
// (rows * pgChurnEventParamsPerRow) stays well under PG's
// 65535 wire-protocol limit. Hard ceiling is floor(65535/9)
// = 7281 rows; 5000 leaves ~30% headroom for a future column
// addition (8 params/row -> 8190 ceiling; 10 params/row ->
// 6553 ceiling) without re-tuning the chunk size.
//
// Payloads above this size are split into N chunks executed
// inside ONE explicit transaction so the all-or-nothing
// contract holds across the chunk boundary.
const pgChurnEventMaxRowsPerInsert = 5000

// ErrPGChurnEventStoreNilDB surfaces a nil *sql.DB at
// composition-root wiring time. Tests pin this sentinel via
// errors.Is so the wire-time misconfig is caught loudly.
var ErrPGChurnEventStoreNilDB = errors.New("churn: NewPGChurnEventStore: *sql.DB is nil")

// ErrPGChurnEventStoreEmptySchema surfaces an empty schema
// name at composition-root wiring time.
var ErrPGChurnEventStoreEmptySchema = errors.New("churn: NewPGChurnEventStoreWithSchema: schema is empty")

// PGChurnEventStore is the production-PG implementation of
// BOTH [ChurnEventWriter] (the verb's staging path) and
// [ChurnEventReader] (the materialiser's read pass). One
// store handles both because the role grants on the table
// allow the SAME role (`clean_code_metric_ingestor`) to
// INSERT and SELECT -- splitting into two types would buy
// nothing.
//
// # Concurrency
//
// The struct holds an immutable `*sql.DB` handle plus an
// immutable schema string; no mutable per-call state. Safe
// for concurrent use across goroutines / Router workers.
//
// # Transaction shape
//
// [WriteChurnEvents] runs the batch inside ONE explicit
// `BEGIN/COMMIT` transaction. Small batches resolve to a
// single multi-row INSERT inside that TX; large batches
// (above [pgChurnEventMaxRowsPerInsert] rows) are split into
// multiple INSERTs to stay under PostgreSQL's 65535
// bind-parameter wire-protocol limit -- all chunks share the
// transaction so an error on the Nth chunk rolls back the
// preceding chunks. On any row failing (CHECK violation, FK
// violation, unique-constraint violation) the entire batch
// is rolled back, so the all-or-nothing contract the
// in-memory fake enforces holds at the PG layer too.
//
// The transaction is OWNED by this writer (BeginTx in
// WriteChurnEvents); it is NOT shared with the Router's
// scan_run-open transaction. The two writes do not need to
// be co-located in one TX: the Router's scan_run claim is
// already durable (and idempotent on `payload_hash`) by the
// time the verb dispatches, so a churn_event INSERT that
// fails after a successful claim leaves only an empty
// scan_run -- which the materialiser ignores and the
// staleness sweep eventually retracts.
type PGChurnEventStore struct {
	db     *sql.DB
	schema string
}

// NewPGChurnEventStore wraps `db` using the canonical
// `clean_code` schema. Returns an error on misconfiguration
// so the wire-time failure surfaces at composition-root
// startup rather than at first request.
func NewPGChurnEventStore(db *sql.DB) (*PGChurnEventStore, error) {
	return NewPGChurnEventStoreWithSchema(db, pgChurnEventDefaultSchema)
}

// NewPGChurnEventStoreWithSchema is the test-friendly
// constructor: tests inject a non-default schema name to
// keep their SQL assertions visibly diff-able from
// production (e.g. `clean_code_ingestor_test`).
func NewPGChurnEventStoreWithSchema(db *sql.DB, schema string) (*PGChurnEventStore, error) {
	if db == nil {
		return nil, ErrPGChurnEventStoreNilDB
	}
	if strings.TrimSpace(schema) == "" {
		return nil, ErrPGChurnEventStoreEmptySchema
	}
	return &PGChurnEventStore{db: db, schema: schema}, nil
}

// qualifyTable returns `"<schema>"."churn_event"`.
func (s *PGChurnEventStore) qualifyTable() string {
	return pq.QuoteIdentifier(s.schema) + "." + pq.QuoteIdentifier(pgChurnEventTable)
}

// WriteChurnEvents implements [ChurnEventWriter]. Stages
// the batch into `<schema>.churn_event` under ONE explicit
// transaction so the all-or-nothing contract holds even when
// the batch is split into multiple INSERT statements.
//
// # Chunking (PG bind-parameter limit)
//
// PostgreSQL's wire protocol caps bind parameters per
// statement at uint16 (65535). This writer sends
// [pgChurnEventParamsPerRow] (=9) parameters per row, so a
// single INSERT can carry at most floor(65535/9)=7281 rows.
// Stage 4.4 payloads are bounded by the Router's body-size
// limit, but a future increase (or a misbehaving publisher)
// could push past the ceiling -- a runtime "extended protocol
// limited to 65535 parameters" error from lib/pq is a worse
// failure mode than transparent chunking.
//
// The writer therefore splits `events` into chunks of at
// most [pgChurnEventMaxRowsPerInsert] (=5000) rows. Each
// chunk is one parameterised multi-row INSERT; ALL chunks
// run inside the SAME explicit BEGIN/COMMIT transaction so
// a failure on chunk N rolls back chunks 1..N-1 and the call
// returns a wrapped error.
//
// # Empty input
//
// Empty `events` slice is a no-op -- no round-trip to the DB
// (no BEGIN issued).
//
// # Idempotency
//
// The migration 0010 UNIQUE constraint
// `churn_event_scan_run_row_uniq` on `(scan_run_id,
// payload_row_index)` is the row-level idempotency anchor.
// This writer issues plain `INSERT`s (NO `ON CONFLICT DO
// NOTHING`) because the (verb, payload_hash) idempotency
// anchor at the Router layer (migration 0009) is the
// authoritative dedupe; a duplicate INSERT here surfaces a
// regression (e.g. the Router admitted a second claim for
// the same hash), and a 23505 error is the loudest signal.
//
// # Canonical SQL shape (schema is interpolated at run time
// via pq.QuoteIdentifier; the production deployment runs
// with schema='clean_code', so the effective statements are):
//
//	BEGIN;
//	INSERT INTO clean_code.churn_event
//	    (churn_event_id, scan_run_id, repo_id, sha,
//	     file_path, modified_at, author,
//	     payload_row_index, created_at)
//	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9), ($10,...), ...;
//	-- (repeated for each chunk of <= 5000 rows)
//	COMMIT;
//
// Author is passed as `sql.NullString` (empty -> NULL) so the
// `author` column's nullable type round-trips correctly.
func (s *PGChurnEventStore) WriteChurnEvents(ctx context.Context, events []ChurnEvent) error {
	if len(events) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("churn: PGChurnEventStore.WriteChurnEvents: begin: %w", err)
	}
	// Defer rollback; a successful Commit makes Rollback a
	// no-op that returns sql.ErrTxDone, which we deliberately
	// ignore.
	defer func() { _ = tx.Rollback() }()

	for start := 0; start < len(events); start += pgChurnEventMaxRowsPerInsert {
		end := start + pgChurnEventMaxRowsPerInsert
		if end > len(events) {
			end = len(events)
		}
		if err := s.writeChunk(ctx, tx, events[start:end]); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("churn: PGChurnEventStore.WriteChurnEvents: commit: %w", err)
	}
	return nil
}

// writeChunk emits ONE multi-row INSERT for `events` against
// the open transaction. `events` MUST be non-empty and its
// length MUST NOT exceed [pgChurnEventMaxRowsPerInsert] --
// the caller (WriteChurnEvents) enforces both.
func (s *PGChurnEventStore) writeChunk(ctx context.Context, tx *sql.Tx, events []ChurnEvent) error {
	var (
		placeholders strings.Builder
		args         = make([]any, 0, pgChurnEventParamsPerRow*len(events))
	)
	for i, ev := range events {
		if i > 0 {
			placeholders.WriteString(", ")
		}
		base := i * pgChurnEventParamsPerRow
		fmt.Fprintf(&placeholders,
			"($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9,
		)
		// `author` -> NULL when empty so the nullable column
		// stays semantically `unknown` rather than the empty
		// string.
		var author sql.NullString
		if strings.TrimSpace(ev.Author) != "" {
			author = sql.NullString{String: ev.Author, Valid: true}
		}
		args = append(args,
			ev.ChurnEventID,
			ev.ScanRunID,
			ev.RepoID,
			ev.SHA,
			ev.FilePath,
			ev.ModifiedAt.UTC(),
			author,
			ev.PayloadRowIndex,
			ev.CreatedAt.UTC(),
		)
	}

	stmt := fmt.Sprintf(
		`INSERT INTO %s
		    (churn_event_id, scan_run_id, repo_id, sha,
		     file_path, modified_at, author,
		     payload_row_index, created_at)
		 VALUES %s`,
		s.qualifyTable(),
		placeholders.String(),
	)

	if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
		return fmt.Errorf("churn: PGChurnEventStore.WriteChurnEvents: exec: %w", err)
	}
	return nil
}

// ListChurnEventsForRepo implements [ChurnEventReader]. The
// materialiser's read pass calls this to pull staged events
// for a per-repo window scan.
//
// # Canonical SQL shape (schema interpolated at run time via
// pq.QuoteIdentifier; production schema='clean_code'):
//
//	SELECT churn_event_id, scan_run_id, repo_id, sha,
//	       file_path, modified_at, author,
//	       payload_row_index, created_at
//	FROM clean_code.churn_event
//	WHERE repo_id = $1
//	  AND ($2::timestamptz IS NULL OR modified_at >= $2)
//	ORDER BY modified_at DESC, created_at DESC, churn_event_id
//
// The `(repo_id, modified_at DESC)` index from migration
// 0010 covers the predicate + sort prefix. The materialiser
// caps its scan by `window_days` (e.g. 90 days) so the
// index range read stays bounded.
//
// `author` is read into an `sql.NullString` and mapped back
// to an empty string when NULL so the returned [ChurnEvent]
// shape mirrors the writer's input shape.
func (s *PGChurnEventStore) ListChurnEventsForRepo(ctx context.Context, repoID uuid.UUID, since time.Time) ([]ChurnEvent, error) {
	if repoID == uuid.Nil {
		return nil, errors.New("churn: PGChurnEventStore.ListChurnEventsForRepo: repo_id is the zero UUID")
	}

	var sinceArg any
	if since.IsZero() {
		sinceArg = nil
	} else {
		sinceArg = since.UTC()
	}

	stmt := fmt.Sprintf(
		`SELECT churn_event_id, scan_run_id, repo_id, sha,
		        file_path, modified_at, author,
		        payload_row_index, created_at
		 FROM %s
		 WHERE repo_id = $1
		   AND ($2::timestamptz IS NULL OR modified_at >= $2)
		 ORDER BY modified_at DESC, created_at DESC, churn_event_id`,
		s.qualifyTable(),
	)

	rows, err := s.db.QueryContext(ctx, stmt, repoID, sinceArg)
	if err != nil {
		return nil, fmt.Errorf("churn: PGChurnEventStore.ListChurnEventsForRepo: query: %w", err)
	}
	defer rows.Close()

	out := make([]ChurnEvent, 0)
	for rows.Next() {
		var (
			ev     ChurnEvent
			author sql.NullString
		)
		if err := rows.Scan(
			&ev.ChurnEventID,
			&ev.ScanRunID,
			&ev.RepoID,
			&ev.SHA,
			&ev.FilePath,
			&ev.ModifiedAt,
			&author,
			&ev.PayloadRowIndex,
			&ev.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("churn: PGChurnEventStore.ListChurnEventsForRepo: scan: %w", err)
		}
		if author.Valid {
			ev.Author = author.String
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("churn: PGChurnEventStore.ListChurnEventsForRepo: rows.Err: %w", err)
	}
	return out, nil
}

// Compile-time interface assertions: a future signature drift
// surfaces at build time, not at first call.
var (
	_ ChurnEventWriter = (*PGChurnEventStore)(nil)
	_ ChurnEventReader = (*PGChurnEventStore)(nil)
)
