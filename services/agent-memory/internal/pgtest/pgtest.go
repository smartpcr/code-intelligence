// Package pgtest provisions per-test PostgreSQL schemas for
// integration tests that exercise the agent-memory storage
// stack against a live cluster.
//
// It exists so cross-package integration tests
// (`internal/graphsink`, `internal/repoindexer`, etc.) share
// ONE schema-bootstrap path instead of each duplicating the
// open / migrate / pin-search_path / cleanup dance the
// per-package `*_integration_test.go` files have grown
// independently. The REPO-SCANNER Stage 3.8 backend-parity
// golden test (`internal/graphsink/parity_postgres_test.go`)
// is the first consumer and the workstream brief calls out
// the `pgtest` name literally.
//
// USAGE
//
//	fx := pgtest.OpenSchema(t)
//	writer := graphwriter.New(fx.DB, nil)
//	reader := graphreader.New(fx.Pool, nil)
//
// SKIP-NOT-FAIL CONTRACT. When `AGENT_MEMORY_PG_URL` is unset
// or the cluster cannot be reached, OpenSchema calls
// `t.Skipf(...)` instead of `t.Fatalf`. This matches the
// repo-wide convention every `*_integration_test.go` file
// already follows (see
// `internal/graphwriter/writer_integration_test.go` and
// `internal/graphreader/reader_integration_test.go`).
//
// CLEANUP. OpenSchema registers a `t.Cleanup` that drops the
// per-test schema CASCADE and (mirroring the writer / reader
// integration tests) deletes any `partman.part_config` rows
// pointing at tables in that schema so the cluster's
// pg_partman state stays clean between runs.
//
// ROLE POLICY. OpenSchema connects as the OWNER role from the
// DSN (no `ALTER ROLE agent_memory_app WITH LOGIN` flip). The
// parity gate is identity-shape, not role-grant. Tests that
// MUST exercise the `agent_memory_app` / `agent_memory_ro`
// LOGIN windows continue to use the existing
// `internal/graphwriter/writer_integration_test.go` /
// `internal/graphreader/reader_integration_test.go` patterns
// (which acquire the cluster-wide advisory lock in
// `internal/testpglock` before flipping the role).
package pgtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/migrations"
)

// EnvDSN is the environment variable every consumer reads to
// locate the test PostgreSQL cluster. Exported so callers can
// reference it by name in skip / documentation strings.
const EnvDSN = "AGENT_MEMORY_PG_URL"

// defaultTimeout is the per-step deadline OpenSchema applies
// to PingContext / CREATE SCHEMA / migrations.Up. 90s is
// generous enough for the full migrations bundle on a cold
// cluster but short enough that a hung integration job fails
// loudly instead of timing out at the CI level.
const defaultTimeout = 90 * time.Second

// Fixture is the per-test substrate OpenSchema returns. The
// caller hands `DB` to writers (graphwriter, graphsink/postgres
// sink) and `Pool` to readers (graphreader, graphsink/postgres
// reader); both point at the same per-test schema.
type Fixture struct {
	// DB is the owner-role *sql.DB the migrations were applied
	// through. `search_path` is pinned to the per-test schema
	// (plus `public, partman`) so unqualified DDL / DML
	// targets the per-test tables.
	DB *sql.DB

	// Pool is a `*pgxpool.Pool` against the SAME owner role +
	// schema as `DB`. It is the pool `graphreader.New` and the
	// `graphsink/postgres` Reader adapter expect. Constructed
	// with `graphreader.NewPool(..., AllowAnyRole: true)` so
	// the owner-role connection is accepted without the
	// production read-only-role assertion.
	Pool *pgxpool.Pool

	// Schema is the per-test schema name (e.g.
	// `pgtest_<hex>`). Exposed so callers can quote it in raw
	// SQL for fixture assertions.
	Schema string

	// DSN is the original DSN OpenSchema parsed from
	// `AGENT_MEMORY_PG_URL`. Exposed so callers that need to
	// open a second connection (e.g. to assert role-grant
	// behaviour) reuse the same source of truth.
	DSN string

	cleanups []func()
}

// Close runs every registered cleanup in LIFO order. It is
// idempotent: a second call is a no-op. Tests do not have to
// call this manually -- OpenSchema registers `Close` as a
// `t.Cleanup` -- but the method is exported for the rare
// path that wants to release resources before the test ends.
func (f *Fixture) Close() {
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
	f.cleanups = nil
}

// OpenSchema provisions a fresh per-test schema on the cluster
// pointed at by `AGENT_MEMORY_PG_URL`, applies every migration
// via `migrations.New(db).Up(ctx)`, returns a Fixture wrapping
// the owner *sql.DB plus a pgxpool.Pool pinned to the same
// schema, and registers a cleanup that drops the schema
// CASCADE.
//
// Skips the test (rather than failing) when:
//   - `AGENT_MEMORY_PG_URL` is unset, or
//   - the cluster cannot be reached with a 90s timeout.
//
// Fatals the test on any other provisioning failure (schema
// create, migrations apply, pool connect).
func OpenSchema(t *testing.T) *Fixture {
	t.Helper()
	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		t.Skipf("skipping: %s is unset; integration tests require a live PostgreSQL", EnvDSN)
	}

	owner, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("pgtest: sql.Open: %v", err)
	}
	owner.SetMaxOpenConns(1)
	owner.SetMaxIdleConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	if err := owner.PingContext(ctx); err != nil {
		_ = owner.Close()
		t.Skipf("skipping: cannot reach PostgreSQL at %s: %v", EnvDSN, err)
	}

	schema := newSchemaName(t)
	if _, err := owner.ExecContext(ctx, `CREATE SCHEMA `+quoteIdent(schema)); err != nil {
		_ = owner.Close()
		t.Fatalf("pgtest: CREATE SCHEMA %q: %v", schema, err)
	}

	if _, err := owner.ExecContext(ctx, fmt.Sprintf(
		`SET search_path TO %s, public, partman`, quoteIdent(schema),
	)); err != nil {
		_ = owner.Close()
		t.Fatalf("pgtest: SET search_path: %v", err)
	}

	if err := migrations.New(owner).Up(ctx); err != nil {
		_ = owner.Close()
		t.Fatalf("pgtest: migrations.Up: %v", err)
	}

	pool, err := graphreader.NewPool(ctx, dsn, graphreader.PoolOptions{
		MaxConns:     4,
		MinConns:     1,
		SearchPath:   schema + ", public, partman",
		AllowAnyRole: true,
	})
	if err != nil {
		dropSchema(owner, schema)
		_ = owner.Close()
		t.Fatalf("pgtest: graphreader.NewPool: %v", err)
	}

	fx := &Fixture{
		DB:     owner,
		Pool:   pool,
		Schema: schema,
		DSN:    dsn,
	}
	fx.cleanups = append(fx.cleanups, func() { pool.Close() })
	fx.cleanups = append(fx.cleanups, func() {
		dropSchema(owner, schema)
	})
	fx.cleanups = append(fx.cleanups, func() { _ = owner.Close() })

	t.Cleanup(fx.Close)
	return fx
}

// dropSchema removes the per-test schema and its `partman`
// rows. Best-effort: errors are swallowed because cleanup
// happens AFTER the test result is final and we'd rather leak
// a stray row than mask a real failure.
func dropSchema(owner *sql.DB, schema string) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	schemaPrefix := strings.ReplaceAll(schema, "_", "#_") + ".%"
	_, _ = owner.ExecContext(ctx, `
		DELETE FROM partman.part_config
		WHERE parent_table LIKE $1 ESCAPE '#'
	`, schemaPrefix)
	_, _ = owner.ExecContext(ctx, `DROP SCHEMA `+quoteIdent(schema)+` CASCADE`)
}

func newSchemaName(t *testing.T) string {
	t.Helper()
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("pgtest: rand: %v", err)
	}
	return "pgtest_" + hex.EncodeToString(buf[:])
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
