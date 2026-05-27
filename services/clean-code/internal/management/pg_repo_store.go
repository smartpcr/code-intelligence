package management

// Stage 6.2 -- PostgreSQL-backed [RepoStore] used by the
// `clean-code-metric-ingestor` composition root to honour
// the atomicity contract of `mgmt.register_repo` and
// `mgmt.set_mode` in production.
//
// # Atomicity invariant (architecture Sec 5.1.4 + Sec 6.3)
//
// Both verbs MUST land their catalog mutation
// (INSERT/UPDATE on `clean_code.repo`) AND their matching
// `repo_event` audit row in ONE database transaction. The
// in-memory implementation in [InMemoryRepoStore] mirrors
// the contract via a single mutex; this PG store binds
// both statements to a `*sql.Tx` and commits or rolls back
// the pair together.
//
// # Idempotency on repo_url -- advisory lock + SELECT-FOR-UPDATE
//
// The schema (migration 0006_repo_url.up.sql) declares
// `repo_url` as a write-once NULLable column WITHOUT a
// unique constraint. Two concurrent `mgmt.register_repo`
// calls for the same URL would therefore race the
// SELECT-then-INSERT check and produce two rows.
//
// The PG store serialises registrations per-URL via a
// `pg_advisory_xact_lock(hashtext(repo_url))` at the very
// start of the transaction. The lock is xact-scoped so it
// releases automatically on COMMIT / ROLLBACK; it is per
// hash-of-URL so concurrent registrations of DIFFERENT
// URLs are not serialised.
//
// # Role boundary (migrations/0004_roles.up.sql:311-313)
//
// The Management role holds:
//
//	GRANT INSERT (repo_id, display_name, mode, default_branch),
//	      UPDATE (display_name, mode, default_branch)
//	  ON clean_code.repo
//	      TO clean_code_management;
//	GRANT INSERT (repo_url), UPDATE (repo_url)
//	  ON clean_code.repo
//	      TO clean_code_management;        -- 0006_repo_url.up.sql:140-141
//	GRANT INSERT, SELECT ON clean_code.repo_event
//	      TO clean_code_management;        -- 0001:313
//
// The PG store opens the supplied `*sql.DB` (which the
// composition root passes from the mgmt-role DSN) and
// issues only those column-level operations. Production
// audit grep on the SQL strings confirms each INSERT names
// exactly the granted columns.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gofrs/uuid"
	"github.com/lib/pq"
)

// Sentinel errors emitted by [NewPGRepoStore] at
// composition-root wiring time.
var (
	// ErrPGRepoStoreNilDB surfaces a nil *sql.DB at
	// wiring time.
	ErrPGRepoStoreNilDB = errors.New("management: NewPGRepoStore: *sql.DB is nil")
	// ErrPGRepoStoreEmptySchema surfaces an empty schema
	// name at wiring time.
	ErrPGRepoStoreEmptySchema = errors.New("management: NewPGRepoStoreWithSchema: schema is empty")
)

const (
	pgRepoStoreDefaultSchema = "clean_code"
	pgRepoStoreRepoTable     = "repo"
	pgRepoStoreEventTable    = "repo_event"
)

// PGRepoStore is the production PostgreSQL-backed
// [RepoStore] used by the `clean-code-metric-ingestor`
// binary's mgmt routes.
//
// Both `RegisterRepo` and `SetRepoMode` open a single
// `*sql.Tx` and bind both the catalog mutation and the
// `repo_event` append to that transaction; a failure on
// either rolls back the pair, preserving the architecture
// Sec 5.1.4 append-only audit invariant.
type PGRepoStore struct {
	db            *sql.DB
	schema        string
	repoQName     string // pre-quoted `"<schema>"."repo"`
	eventQName    string // pre-quoted `"<schema>"."repo_event"`
	eventEnumName string // pre-quoted `"<schema>"."repo_event_kind"`
}

// NewPGRepoStore wraps `db` using the canonical
// `clean_code` schema. The supplied handle MUST carry the
// `clean_code_management` role's credentials -- the
// per-column INSERT/UPDATE grants on `clean_code.repo`
// (migrations/0004_roles.up.sql:311-312 +
// 0006_repo_url.up.sql:140-141) are the only writes the
// store issues against the catalog.
func NewPGRepoStore(db *sql.DB) (*PGRepoStore, error) {
	return NewPGRepoStoreWithSchema(db, pgRepoStoreDefaultSchema)
}

// NewPGRepoStoreWithSchema is the test-friendly
// schema-isolated constructor. `schema` is interpolated as
// a pre-quoted identifier; callers MUST pass a trusted,
// validated value.
func NewPGRepoStoreWithSchema(db *sql.DB, schema string) (*PGRepoStore, error) {
	if db == nil {
		return nil, ErrPGRepoStoreNilDB
	}
	if strings.TrimSpace(schema) == "" {
		return nil, ErrPGRepoStoreEmptySchema
	}
	qschema := pq.QuoteIdentifier(schema)
	return &PGRepoStore{
		db:            db,
		schema:        schema,
		repoQName:     qschema + "." + pq.QuoteIdentifier(pgRepoStoreRepoTable),
		eventQName:    qschema + "." + pq.QuoteIdentifier(pgRepoStoreEventTable),
		eventEnumName: qschema + "." + pq.QuoteIdentifier("repo_event_kind"),
	}, nil
}

// RegisterRepo implements [RepoStore].
//
// Transaction shape:
//
//  1. `BEGIN`
//  2. `SELECT pg_advisory_xact_lock(hashtext($1::text))` --
//     serialises ONLY concurrent registrations of the same
//     repo_url (different URLs hash to different keys so
//     they do not block each other).
//  3. `SELECT repo_id, mode FROM <schema>.repo WHERE
//     repo_url = $1 LIMIT 1` -- under the lock above, this
//     read is race-free for the URL we care about.
//  4. If a row exists: COMMIT (releasing the advisory lock),
//     return [RegisterRepoResult]{Created: false, ...}.
//  5. Else: `INSERT INTO <schema>.repo (display_name, mode,
//     default_branch, repo_url) VALUES (...) RETURNING
//     repo_id` -- the DB DEFAULT mints `repo_id` and
//     `created_at`; the column list matches the Management
//     role's column-level INSERT grant.
//  6. `INSERT INTO <schema>.repo_event (repo_id, kind,
//     payload_json) VALUES (..., 'registered', ...)` --
//     same transaction so the audit row commits with the
//     catalog row.
//  7. `COMMIT`.
//
// On any failure the deferred ROLLBACK runs and BOTH writes
// disappear (or rather never become visible) -- this is the
// atomicity invariant the rubber-duck pass for Stage 6.2
// pinned.
func (s *PGRepoStore) RegisterRepo(ctx context.Context, req RegisterRepoRowRequest) (RegisterRepoResult, error) {
	if err := ctx.Err(); err != nil {
		return RegisterRepoResult{}, err
	}
	url := strings.TrimSpace(req.RepoURL)
	if url == "" {
		return RegisterRepoResult{}, ErrRepoStoreEmptyURL
	}
	branch := strings.TrimSpace(req.DefaultBranch)
	if branch == "" {
		return RegisterRepoResult{}, ErrRepoStoreEmptyDefaultBranch
	}
	mode := strings.TrimSpace(req.Mode)
	if mode == "" {
		// DB default per `0001_catalog_lifecycle.up.sql:154`.
		mode = RepoModeEmbedded
	}
	if !IsAllowedRepoMode(mode) {
		return RegisterRepoResult{}, fmt.Errorf("%w: got %q", ErrRepoStoreInvalidMode, mode)
	}
	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		displayName = deriveDisplayNameFromURL(url)
	}
	actor := strings.TrimSpace(req.Actor)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RegisterRepoResult{}, fmt.Errorf("management: PGRepoStore.RegisterRepo BEGIN: %w", err)
	}
	// Rollback is a no-op after COMMIT -- the defer is the
	// safety net for every early return below.
	defer func() { _ = tx.Rollback() }()

	// Per-URL advisory lock. `hashtext` is a stable 32-bit
	// hash; collisions across different URLs are harmless
	// because the SELECT below still re-checks
	// `repo_url = $1` exactly.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1::text))`, url); err != nil {
		return RegisterRepoResult{}, fmt.Errorf("management: PGRepoStore.RegisterRepo advisory_xact_lock(%q): %w", url, err)
	}

	// Idempotency lookup -- now safe under the advisory
	// lock.
	var (
		existingID   uuid.UUID
		existingMode string
	)
	lookupSQL := fmt.Sprintf(`SELECT repo_id, mode FROM %s WHERE repo_url = $1 LIMIT 1`, s.repoQName)
	err = tx.QueryRowContext(ctx, lookupSQL, url).Scan(&existingID, &existingMode)
	switch {
	case err == nil:
		// Existing row -- idempotent path. Commit (releases
		// the advisory lock) and return the existing id.
		if err := tx.Commit(); err != nil {
			return RegisterRepoResult{}, fmt.Errorf("management: PGRepoStore.RegisterRepo COMMIT (idempotent path, repo_id=%s): %w", existingID, err)
		}
		return RegisterRepoResult{
			RepoID:  existingID,
			Created: false,
			Mode:    existingMode,
		}, nil
	case errors.Is(err, sql.ErrNoRows):
		// fall through to fresh-insert path
	default:
		return RegisterRepoResult{}, fmt.Errorf("management: PGRepoStore.RegisterRepo SELECT lookup (url=%q): %w", url, err)
	}

	// Fresh insert. Column list matches the Management role's
	// per-column INSERT grant in 0004 line 311 + 0006 line
	// 140. `repo_id` and `created_at` use the DB defaults.
	var repoID uuid.UUID
	insertRepoSQL := fmt.Sprintf(
		`INSERT INTO %s (display_name, mode, default_branch, repo_url)
		     VALUES ($1, $2, $3, $4)
		  RETURNING repo_id`,
		s.repoQName,
	)
	if err := tx.QueryRowContext(ctx, insertRepoSQL, displayName, mode, branch, url).Scan(&repoID); err != nil {
		return RegisterRepoResult{}, fmt.Errorf("management: PGRepoStore.RegisterRepo INSERT repo (url=%q): %w", url, err)
	}

	// Audit row -- canonical payload shape mirrors the
	// in-memory store + runbook contract.
	payload := map[string]any{
		"repo_url":       url,
		"default_branch": branch,
		"mode":           mode,
		"display_name":   displayName,
	}
	if actor != "" {
		payload["actor"] = actorPrefix + actor
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return RegisterRepoResult{}, fmt.Errorf("management: PGRepoStore.RegisterRepo marshal registered payload (repo_id=%s): %w", repoID, err)
	}
	insertEventSQL := fmt.Sprintf(
		`INSERT INTO %s (repo_id, kind, payload_json)
		     VALUES ($1, $2::%s, $3::jsonb)`,
		s.eventQName,
		s.eventEnumName,
	)
	if _, err := tx.ExecContext(ctx, insertEventSQL, repoID, RepoEventKindRegistered, string(payloadBytes)); err != nil {
		return RegisterRepoResult{}, fmt.Errorf("management: PGRepoStore.RegisterRepo INSERT repo_event(registered) (repo_id=%s): %w", repoID, err)
	}

	if err := tx.Commit(); err != nil {
		return RegisterRepoResult{}, fmt.Errorf("management: PGRepoStore.RegisterRepo COMMIT (fresh path, repo_id=%s): %w", repoID, err)
	}
	return RegisterRepoResult{
		RepoID:  repoID,
		Created: true,
		Mode:    mode,
	}, nil
}

// SetRepoMode implements [RepoStore].
//
// Transaction shape:
//
//  1. `BEGIN`
//  2. `SELECT mode FROM <schema>.repo WHERE repo_id = $1
//     FOR UPDATE` -- locks the row so a concurrent
//     `set_mode` against the same repo serialises.
//  3. If [sql.ErrNoRows], return [ErrRepoStoreUnknownRepo].
//  4. If existing mode == new mode, COMMIT and return
//     [SetRepoModeResult]{Changed: false} -- canonical
//     no-op, no UPDATE, no audit row.
//  5. Else: `UPDATE <schema>.repo SET mode = $2 WHERE
//     repo_id = $1` + `INSERT INTO <schema>.repo_event ...`
//     in the same transaction.
//  6. `COMMIT`.
func (s *PGRepoStore) SetRepoMode(ctx context.Context, req SetRepoModeRequest) (SetRepoModeResult, error) {
	if err := ctx.Err(); err != nil {
		return SetRepoModeResult{}, err
	}
	if req.RepoID == uuid.Nil {
		return SetRepoModeResult{}, ErrRepoStoreZeroRepoID
	}
	mode := strings.TrimSpace(req.Mode)
	if !IsAllowedRepoMode(mode) {
		return SetRepoModeResult{}, fmt.Errorf("%w: got %q", ErrRepoStoreInvalidMode, mode)
	}
	actor := strings.TrimSpace(req.Actor)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SetRepoModeResult{}, fmt.Errorf("management: PGRepoStore.SetRepoMode BEGIN: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var previous string
	lookupSQL := fmt.Sprintf(`SELECT mode FROM %s WHERE repo_id = $1 FOR UPDATE`, s.repoQName)
	err = tx.QueryRowContext(ctx, lookupSQL, req.RepoID).Scan(&previous)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return SetRepoModeResult{}, fmt.Errorf("%w: repo_id=%s", ErrRepoStoreUnknownRepo, req.RepoID)
	case err != nil:
		return SetRepoModeResult{}, fmt.Errorf("management: PGRepoStore.SetRepoMode SELECT mode (repo_id=%s): %w", req.RepoID, err)
	}

	if previous == mode {
		// Canonical no-op: row already at target mode.
		// COMMIT so the SELECT-FOR-UPDATE lock releases;
		// no UPDATE, no event.
		if err := tx.Commit(); err != nil {
			return SetRepoModeResult{}, fmt.Errorf("management: PGRepoStore.SetRepoMode COMMIT (no-op path, repo_id=%s): %w", req.RepoID, err)
		}
		return SetRepoModeResult{
			RepoID:       req.RepoID,
			Mode:         previous,
			PreviousMode: previous,
			Changed:      false,
		}, nil
	}

	// Real transition: UPDATE + audit row, both in the txn.
	updateSQL := fmt.Sprintf(`UPDATE %s SET mode = $2 WHERE repo_id = $1`, s.repoQName)
	res, err := tx.ExecContext(ctx, updateSQL, req.RepoID, mode)
	if err != nil {
		return SetRepoModeResult{}, fmt.Errorf("management: PGRepoStore.SetRepoMode UPDATE (repo_id=%s): %w", req.RepoID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return SetRepoModeResult{}, fmt.Errorf("management: PGRepoStore.SetRepoMode RowsAffected (repo_id=%s): %w", req.RepoID, err)
	}
	if affected != 1 {
		// The SELECT-FOR-UPDATE above already confirmed the
		// row exists; affected != 1 would indicate a
		// concurrent DELETE (which the schema does not
		// permit) or a wiring bug.
		return SetRepoModeResult{}, fmt.Errorf("management: PGRepoStore.SetRepoMode UPDATE affected=%d (want 1) for repo_id=%s", affected, req.RepoID)
	}

	payload := map[string]any{
		"mode":          mode,
		"previous_mode": previous,
	}
	if actor != "" {
		payload["actor"] = actorPrefix + actor
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return SetRepoModeResult{}, fmt.Errorf("management: PGRepoStore.SetRepoMode marshal mode_changed payload (repo_id=%s): %w", req.RepoID, err)
	}
	insertEventSQL := fmt.Sprintf(
		`INSERT INTO %s (repo_id, kind, payload_json)
		     VALUES ($1, $2::%s, $3::jsonb)`,
		s.eventQName,
		s.eventEnumName,
	)
	if _, err := tx.ExecContext(ctx, insertEventSQL, req.RepoID, RepoEventKindModeChanged, string(payloadBytes)); err != nil {
		return SetRepoModeResult{}, fmt.Errorf("management: PGRepoStore.SetRepoMode INSERT repo_event(mode_changed) (repo_id=%s): %w", req.RepoID, err)
	}

	if err := tx.Commit(); err != nil {
		return SetRepoModeResult{}, fmt.Errorf("management: PGRepoStore.SetRepoMode COMMIT (repo_id=%s): %w", req.RepoID, err)
	}
	return SetRepoModeResult{
		RepoID:       req.RepoID,
		Mode:         mode,
		PreviousMode: previous,
		Changed:      true,
	}, nil
}

// Compile-time interface guard.
var _ RepoStore = (*PGRepoStore)(nil)
