package management

// Stage 3.4 -- production PostgreSQL implementation of
// [RepoEventAppender].
//
// The Management role holds:
//
//	GRANT INSERT, SELECT ON clean_code.repo_event TO clean_code_management;
//
// (migrations/0004_roles.up.sql:313). The PG appender
// here ONLY emits INSERT against `clean_code.repo_event`
// and SELECTs are issued only by the reader-side
// (management.Reader) so the schema-level role
// enforcement is honoured automatically.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofrs/uuid"
	"github.com/lib/pq"
)

// Sentinel errors emitted by [NewPGRepoEventAppender] at
// composition-root wiring time.
var (
	// ErrPGRepoEventAppenderNilDB surfaces a nil *sql.DB
	// at wiring time.
	ErrPGRepoEventAppenderNilDB = errors.New("management: NewPGRepoEventAppender: *sql.DB is nil")
	// ErrPGRepoEventAppenderEmptySchema surfaces an empty
	// schema name at wiring time.
	ErrPGRepoEventAppenderEmptySchema = errors.New("management: NewPGRepoEventAppenderWithSchema: schema is empty")
)

const (
	pgRepoEventDefaultSchema = "clean_code"
	pgRepoEventTable         = "repo_event"
)

// PGRepoEventAppender is the production
// PostgreSQL-backed [RepoEventAppender] used by
// [MgmtWriter] to persist `repo_event(kind=<verb>)` rows.
//
// # Insert shape (matches migrations/0001_catalog_lifecycle.up.sql:298-319)
//
//	INSERT INTO <schema>.repo_event
//	  (repo_id, kind, payload_json)
//	  VALUES ($1, $2::clean_code.repo_event_kind, $3::jsonb);
//
// The `kind` argument is bound as text and cast to the
// canonical enum so an unknown kind surfaces as a
// constraint violation at the DB boundary rather than as
// a silent enum lookup miss. The `event_id` and
// `created_at` columns default at the DB layer
// (`gen_random_uuid()` and `now()`), keeping clock
// authority on the database (architecture Sec 5.1.4
// "server-generated").
type PGRepoEventAppender struct {
	db        *sql.DB
	schema    string
	enumQName string // pre-quoted "<schema>"."repo_event_kind"
}

// NewPGRepoEventAppender wraps `db` using the canonical
// `clean_code` schema.
func NewPGRepoEventAppender(db *sql.DB) (*PGRepoEventAppender, error) {
	return NewPGRepoEventAppenderWithSchema(db, pgRepoEventDefaultSchema)
}

// NewPGRepoEventAppenderWithSchema is the test-friendly
// schema-isolated constructor.
func NewPGRepoEventAppenderWithSchema(db *sql.DB, schema string) (*PGRepoEventAppender, error) {
	if db == nil {
		return nil, ErrPGRepoEventAppenderNilDB
	}
	if strings.TrimSpace(schema) == "" {
		return nil, ErrPGRepoEventAppenderEmptySchema
	}
	return &PGRepoEventAppender{
		db:        db,
		schema:    schema,
		enumQName: pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier("repo_event_kind"),
	}, nil
}

// qualifyRepoEvent returns `"<schema>"."repo_event"`.
func (a *PGRepoEventAppender) qualifyRepoEvent() string {
	return pq.QuoteIdentifier(a.schema) + "." + pq.QuoteIdentifier(pgRepoEventTable)
}

// AppendRepoEvent implements [RepoEventAppender].
// INSERTs a single row into `clean_code.repo_event`.
//
// Invariants honoured at this layer (the SQL boundary):
//   - `repoID` must be non-zero (enforced here so the
//     FK violation surfaces with a clear error rather
//     than a generic FK miss).
//   - `kind` must be non-empty (the DB enum cast catches
//     unknown values; this layer just rejects the
//     pathological empty case).
//   - `payload` MUST marshal to a JSON object (the
//     column is `jsonb NOT NULL`; nil payload is bound
//     as the canonical empty object `{}`).
//   - The clock is the DATABASE clock; we do not
//     transmit `created_at` from the writer.
func (a *PGRepoEventAppender) AppendRepoEvent(ctx context.Context, repoID uuid.UUID, kind string, payload map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if repoID == uuid.Nil {
		return errors.New("management: PGRepoEventAppender.AppendRepoEvent: repoID is the zero UUID")
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return errors.New("management: PGRepoEventAppender.AppendRepoEvent: kind is empty")
	}
	if payload == nil {
		payload = map[string]any{}
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("management: PGRepoEventAppender marshal payload (kind=%s, repo_id=%s): %w", kind, repoID, err)
	}
	stmt := fmt.Sprintf(
		`INSERT INTO %s
		     (repo_id, kind, payload_json)
		  VALUES ($1, $2::%s, $3::jsonb)`,
		a.qualifyRepoEvent(),
		a.enumQName,
	)
	res, err := a.db.ExecContext(ctx, stmt, repoID, kind, string(payloadBytes))
	if err != nil {
		return fmt.Errorf("management: PGRepoEventAppender INSERT repo_event (kind=%s, repo_id=%s): %w", kind, repoID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("management: PGRepoEventAppender RowsAffected (kind=%s, repo_id=%s): %w", kind, repoID, err)
	}
	if affected != 1 {
		return fmt.Errorf("management: PGRepoEventAppender unexpected rowsAffected=%d (want 1) for kind=%s repo_id=%s", affected, kind, repoID)
	}
	return nil
}

// Touch silences unused-import warnings when the layer
// becomes time-aware in a later iter. (No-op today.)
var _ = time.Now

// Compile-time interface guard.
var _ RepoEventAppender = (*PGRepoEventAppender)(nil)
