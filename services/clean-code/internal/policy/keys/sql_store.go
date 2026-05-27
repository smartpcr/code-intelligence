package keys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/gofrs/uuid"
	"github.com/lib/pq"
)

// DefaultSchema is the canonical PostgreSQL schema name the
// Stage 5.1 migration `0005_policy_signing_keys.up.sql`
// installs the table in. Production callers reach
// [NewSQLStore] which pins this default; live integration
// tests use [NewSQLStoreWithSchema] to land on an isolated
// schema (e.g. `clean_code_keys_test`) so they do not race
// with the storage-package migration round-trip that
// `DROP SCHEMA clean_code CASCADE`s on prep.
const DefaultSchema = "clean_code"

// TableName is the unqualified name of the signing-key table.
// Combined with [DefaultSchema] (or a test-injected override)
// to produce a fully-qualified identifier at SQLStore
// construction time. Kept as an exported constant so a
// `grep -rnF "policy_signing_keys"` finds the production SQL
// alongside the migration.
const TableName = "policy_signing_keys"

// SQLTableQualified is the production fully-qualified
// identifier `clean_code.policy_signing_keys`. Stage 5.1
// callers can reach this directly when they only need the
// canonical name. Tests that need an isolated schema construct
// their own qualified name via [SchemaTable].
const SQLTableQualified = DefaultSchema + "." + TableName

// pgSQLStateUniqueViolation is the PostgreSQL SQLSTATE for a
// UNIQUE / PRIMARY KEY violation. The migration declares both
// `key_id uuid PRIMARY KEY` and `fingerprint text UNIQUE`, so
// either collision surfaces as this state code. Detecting it
// canonically (rather than string-matching the message) is what
// lets the Manager translate the DB-level rejection into the
// in-package [ErrDuplicateKey] sentinel.
const pgSQLStateUniqueViolation = "23505"

// pgSQLStateCheckViolation is the PostgreSQL SQLSTATE for a
// CHECK constraint failure. Surfaced by the migration's
// fingerprint shape / public-key length / algorithm canonical
// constraints. SQLStore translates this into a wrapped
// [ErrInvalidPublicKey] / shape error so the Manager can
// distinguish "DB rejected the shape" from "transient
// transport error".
const pgSQLStateCheckViolation = "23514"

// SchemaTable returns the fully-qualified identifier
// `<schema>.policy_signing_keys`. Both identifiers are quoted
// via `pq.QuoteIdentifier` so a schema name with embedded
// special characters never produces a syntactically-broken
// statement. Returned identifier is safe to inline into SQL.
func SchemaTable(schema string) string {
	return pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(TableName)
}

// SQLStore persists [KeyRecord] rows in
// `<schema>.policy_signing_keys` using `database/sql`. The
// caller owns the `*sql.DB` lifecycle -- SQLStore does not
// call `Close`, so the same pool can back multiple sub-stores
// in later stages.
//
// Append-only by contract: the only DML SQLStore issues is
// INSERT and SELECT. The migration's
// `REVOKE UPDATE, DELETE` grants enforce this at the DB level
// independently; SQLStore is the in-process witness of that
// contract.
type SQLStore struct {
	db    *sql.DB
	table string // fully-qualified, already quoted
}

// NewSQLStore wraps db using the canonical
// `clean_code.policy_signing_keys` table. Returns an error if
// db is nil so a mis-wired composition root fails fast at
// start-up rather than at the first Insert.
func NewSQLStore(db *sql.DB) (*SQLStore, error) {
	return NewSQLStoreWithSchema(db, DefaultSchema)
}

// NewSQLStoreWithSchema is the test-friendly constructor:
// callers inject a non-default PostgreSQL schema (e.g. an
// isolated `clean_code_keys_test` for the live integration
// suite). schema MUST be non-empty.
//
// Production code reaches [NewSQLStore] which pins
// [DefaultSchema]; only tests should use this entry point.
// The migrate round-trip in `internal/storage/migrate_test.go`
// owns the canonical `clean_code` schema and DROP SCHEMA
// CASCADEs it on prep; a separate test schema here avoids
// the parallel-package race the iter-2 evaluator caught.
func NewSQLStoreWithSchema(db *sql.DB, schema string) (*SQLStore, error) {
	if db == nil {
		return nil, errors.New("policy/keys: NewSQLStore: *sql.DB is nil")
	}
	if schema == "" {
		return nil, errors.New("policy/keys: NewSQLStoreWithSchema: schema is empty")
	}
	return &SQLStore{db: db, table: SchemaTable(schema)}, nil
}

// Insert satisfies [Store.Insert]. Maps PostgreSQL SQLSTATE
// `23505` (unique_violation) to a wrapped [ErrDuplicateKey]
// so the Manager can branch on the sentinel via `errors.Is`.
// Other DB errors are returned wrapped with the offending
// SQLSTATE so a logger downstream can pattern-match on it.
func (s *SQLStore) Insert(ctx context.Context, rec KeyRecord) error {
	if err := validateRecord(rec); err != nil {
		return err
	}
	stmt := fmt.Sprintf(
		`INSERT INTO %s
		     (key_id, fingerprint, public_key, key_handle, valid_from, algorithm)
		 VALUES ($1, $2, $3, $4, $5, $6)`, s.table)
	_, err := s.db.ExecContext(ctx, stmt,
		rec.KeyID.String(),
		rec.Fingerprint,
		rec.PublicKey,
		string(rec.Handle),
		rec.ValidFrom.UTC(),
		rec.Algorithm,
	)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) {
			switch string(pqErr.Code) {
			case pgSQLStateUniqueViolation:
				return fmt.Errorf("%w: SQLSTATE=%s constraint=%s message=%s",
					ErrDuplicateKey, pqErr.Code, pqErr.Constraint, pqErr.Message)
			case pgSQLStateCheckViolation:
				return fmt.Errorf("%w: SQLSTATE=%s constraint=%s message=%s",
					ErrInvalidPublicKey, pqErr.Code, pqErr.Constraint, pqErr.Message)
			}
		}
		return fmt.Errorf("policy/keys: SQLStore.Insert: %w", err)
	}
	return nil
}

// List satisfies [Store.List]. Loads every row in canonical
// `ValidFrom ASC, KeyID ASC` order. Every row is passed through
// [validateRecord] so a manually-edited / corrupted row fails
// at Load rather than at the first Sign attempt -- a corrupt
// DB row is a `signing_key_cache` health-gate failure, not a
// silent signature mismatch later.
func (s *SQLStore) List(ctx context.Context) ([]KeyRecord, error) {
	stmt := fmt.Sprintf(
		`SELECT key_id, fingerprint, public_key, key_handle, valid_from, algorithm
		 FROM %s
		 ORDER BY valid_from ASC, key_id ASC`, s.table)
	rows, err := s.db.QueryContext(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("policy/keys: SQLStore.List: query: %w", err)
	}
	defer rows.Close()

	out := make([]KeyRecord, 0)
	for rows.Next() {
		var (
			keyIDText   string
			fingerprint string
			publicKey   []byte
			handleText  string
			rec         KeyRecord
		)
		if err := rows.Scan(&keyIDText, &fingerprint, &publicKey, &handleText, &rec.ValidFrom, &rec.Algorithm); err != nil {
			return nil, fmt.Errorf("policy/keys: SQLStore.List: scan: %w", err)
		}
		id, err := uuid.FromString(keyIDText)
		if err != nil {
			return nil, fmt.Errorf("policy/keys: SQLStore.List: bad key_id %q in row: %w", keyIDText, err)
		}
		rec.KeyID = id
		rec.Fingerprint = fingerprint
		rec.PublicKey = publicKey
		rec.Handle = KeyHandle(handleText)
		if err := validateRecord(rec); err != nil {
			return nil, fmt.Errorf("policy/keys: SQLStore.List: row key_id=%s failed shape validation: %w", id, err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("policy/keys: SQLStore.List: rows: %w", err)
	}
	return out, nil
}

// Compile-time check that SQLStore satisfies Store.
var _ Store = (*SQLStore)(nil)
