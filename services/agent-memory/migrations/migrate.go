package migrations

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
)

//go:embed *.sql
var sqlFS embed.FS

// JournalTable is the name of the schema-migrations journal.
// Migrations land in this table in apply order; rows are removed
// on `Down`. The table itself is created lazily on first `Up`
// and dropped only when the consumer explicitly calls `Reset`.
const JournalTable = "_schema_migrations"

// Migration is a single parsed `.sql` file ready to apply.
type Migration struct {
	// Version is the numeric prefix of the filename (e.g. "0001",
	// "0006a"). Lexicographic sort on Version is what defines
	// apply order; the leading zero pad makes "0006a" sort after
	// "0006" naturally without needing custom comparison.
	Version string
	// Name is the human-readable suffix (e.g. "enums",
	// "repo_commit") with no extension.
	Name string
	// Filename is the embedded file path (e.g. "0001_enums.sql").
	Filename string
	// Up is the SQL between `-- migrate:up` and `-- migrate:down`.
	Up string
	// Down is the SQL after `-- migrate:down`, or empty when the
	// file declares no down block.
	Down string
}

// All returns every embedded migration parsed and sorted in
// lexicographic Version order. The returned slice is a fresh
// copy so callers may safely mutate it.
func All() ([]Migration, error) {
	entries, err := fs.ReadDir(sqlFS, ".")
	if err != nil {
		return nil, fmt.Errorf("migrations: read embedded fs: %w", err)
	}
	out := make([]Migration, 0, len(entries))
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".sql") {
			continue
		}
		body, err := fs.ReadFile(sqlFS, ent.Name())
		if err != nil {
			return nil, fmt.Errorf("migrations: read %s: %w", ent.Name(), err)
		}
		m, err := parse(ent.Name(), string(body))
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// versionRe matches the leading numeric (plus optional letter
// suffix) of a migration filename. "0001_enums.sql" -> "0001",
// "0006a_ingest_jobs.sql" -> "0006a". The letter suffix lets us
// slip an extra migration between two already-deployed ones
// without re-numbering everything downstream.
var versionRe = regexp.MustCompile(`^(\d+[a-z]?)_(.+)\.sql$`)

func parse(filename, body string) (Migration, error) {
	matches := versionRe.FindStringSubmatch(filename)
	if matches == nil {
		return Migration{}, fmt.Errorf(
			"migrations: filename %q does not match NNNN[a]_name.sql",
			filename,
		)
	}
	up, down := splitUpDown(body)
	if strings.TrimSpace(up) == "" {
		return Migration{}, fmt.Errorf(
			"migrations: %s has no `-- migrate:up` body",
			filename,
		)
	}
	return Migration{
		Version:  matches[1],
		Name:     matches[2],
		Filename: filename,
		Up:       up,
		Down:     down,
	}, nil
}

// splitUpDown carves a single .sql file into its up and down
// halves on the `-- migrate:up` / `-- migrate:down` sentinels.
// The sentinels are matched at the start of a line and ignore
// trailing whitespace. Content before any sentinel is dropped
// (typically a file-level comment header).
func splitUpDown(body string) (up, down string) {
	const (
		upMarker   = "-- migrate:up"
		downMarker = "-- migrate:down"
	)
	lines := strings.Split(body, "\n")
	var (
		section string // "up", "down", or "" (preamble)
		upBuf   strings.Builder
		downBuf strings.Builder
	)
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t\r")
		switch {
		case trimmed == upMarker:
			section = "up"
			continue
		case trimmed == downMarker:
			section = "down"
			continue
		}
		switch section {
		case "up":
			upBuf.WriteString(line)
			upBuf.WriteString("\n")
		case "down":
			downBuf.WriteString(line)
			downBuf.WriteString("\n")
		}
	}
	return upBuf.String(), downBuf.String()
}

// Migrator owns the lifecycle of the schema journal and the
// migration apply / revert paths. It is stateless beyond the
// embedded SQL files and the *sql.DB handle the caller passes in,
// so multiple Migrators can coexist (e.g. one per test database).
type Migrator struct {
	DB *sql.DB
}

// New constructs a Migrator that writes through the given handle.
// The handle is not closed by the Migrator -- the caller owns its
// lifecycle.
func New(db *sql.DB) *Migrator { return &Migrator{DB: db} }

// Up applies every embedded migration whose version has not yet
// been recorded in the journal, in ascending Version order, each
// inside its own transaction. A failure mid-pass leaves the
// journal consistent: the failing migration is rolled back and
// the apply loop stops with an error.
func (m *Migrator) Up(ctx context.Context) error {
	if err := m.ensureJournal(ctx); err != nil {
		return err
	}
	applied, err := m.appliedVersions(ctx)
	if err != nil {
		return err
	}
	all, err := All()
	if err != nil {
		return err
	}
	for _, mg := range all {
		if applied[mg.Version] {
			continue
		}
		if err := m.applyOne(ctx, mg); err != nil {
			return fmt.Errorf("migrations: apply %s: %w", mg.Filename, err)
		}
	}
	return nil
}

// Down reverts every applied migration in reverse order until the
// journal is empty. The journal table itself is left in place so
// a subsequent Up can re-apply cleanly.
func (m *Migrator) Down(ctx context.Context) error {
	if err := m.ensureJournal(ctx); err != nil {
		return err
	}
	applied, err := m.appliedVersionsOrdered(ctx)
	if err != nil {
		return err
	}
	byVersion := map[string]Migration{}
	all, err := All()
	if err != nil {
		return err
	}
	for _, mg := range all {
		byVersion[mg.Version] = mg
	}
	// Revert newest-applied first.
	for i := len(applied) - 1; i >= 0; i-- {
		v := applied[i]
		mg, ok := byVersion[v]
		if !ok {
			return fmt.Errorf("migrations: journal references unknown version %q", v)
		}
		if err := m.revertOne(ctx, mg); err != nil {
			return fmt.Errorf("migrations: revert %s: %w", mg.Filename, err)
		}
	}
	return nil
}

// Reset drops the journal table. Tests use this between
// round-trip iterations; production code should never call it.
func (m *Migrator) Reset(ctx context.Context) error {
	_, err := m.DB.ExecContext(ctx, "DROP TABLE IF EXISTS "+JournalTable)
	return err
}

// AppliedVersions returns the versions currently recorded in the
// journal, in apply order.
func (m *Migrator) AppliedVersions(ctx context.Context) ([]string, error) {
	if err := m.ensureJournal(ctx); err != nil {
		return nil, err
	}
	return m.appliedVersionsOrdered(ctx)
}

func (m *Migrator) ensureJournal(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS ` + JournalTable + ` (
    version    text        PRIMARY KEY,
    name       text        NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now()
)`
	_, err := m.DB.ExecContext(ctx, ddl)
	return err
}

func (m *Migrator) appliedVersions(ctx context.Context) (map[string]bool, error) {
	versions, err := m.appliedVersionsOrdered(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(versions))
	for _, v := range versions {
		out[v] = true
	}
	return out, nil
}

func (m *Migrator) appliedVersionsOrdered(ctx context.Context) ([]string, error) {
	rows, err := m.DB.QueryContext(ctx,
		"SELECT version FROM "+JournalTable+" ORDER BY applied_at ASC, version ASC",
	)
	if err != nil {
		return nil, fmt.Errorf("migrations: select journal: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (m *Migrator) applyOne(ctx context.Context, mg Migration) error {
	tx, err := m.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// Deferred rollback is a safety net: if Commit() succeeds it
	// returns sql.ErrTxDone (which we discard); if a panic or an
	// early return slips out between here and the commit, the
	// transaction is still released instead of leaking.
	defer tx.Rollback() //nolint:errcheck // best-effort cleanup; commit path makes this a no-op
	// Strip the in-file BEGIN/COMMIT (which we keep in the .sql
	// for readability + manual psql replay) so the SQL body
	// runs inside the Migrator-managed transaction.
	body := stripTopLevelTxn(mg.Up)
	if _, err := tx.ExecContext(ctx, body); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO "+JournalTable+" (version, name) VALUES ($1, $2)",
		mg.Version, mg.Name,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *Migrator) revertOne(ctx context.Context, mg Migration) error {
	if strings.TrimSpace(mg.Down) == "" {
		return fmt.Errorf("migration %s has no down block", mg.Filename)
	}
	tx, err := m.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// See applyOne for the rationale on the deferred rollback.
	defer tx.Rollback() //nolint:errcheck // best-effort cleanup; commit path makes this a no-op
	body := stripTopLevelTxn(mg.Down)
	if _, err := tx.ExecContext(ctx, body); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM "+JournalTable+" WHERE version = $1", mg.Version,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// stripTopLevelTxn removes solitary `BEGIN;` / `COMMIT;` /
// `ROLLBACK;` statements from a migration body so the outer
// Migrator-managed transaction is the only one in flight. We
// keep them in the .sql files because they let an operator run
// the file manually with `psql -f`; here we elide them to avoid
// "BEGIN inside transaction" errors.
//
// The match is intentionally strict: only lines that contain a
// single keyword followed by an optional semicolon (case-insensitive,
// whitespace tolerated) are stripped. Lines like
// `BEGIN; CREATE TABLE ...` are NOT stripped -- such a layout
// is not used by our migration files.
var txnLineRe = regexp.MustCompile(`(?im)^\s*(?:BEGIN|COMMIT|ROLLBACK)\s*;?\s*$`)

func stripTopLevelTxn(body string) string {
	return txnLineRe.ReplaceAllString(body, "")
}

// ErrNoMigrations indicates `All()` returned zero rows. It is
// surfaced by the round-trip test so a malformed embed.FS gets
// caught loudly instead of silently passing.
var ErrNoMigrations = errors.New("migrations: no embedded .sql files found")
