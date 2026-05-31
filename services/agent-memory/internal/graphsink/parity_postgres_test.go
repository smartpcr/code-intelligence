//go:build cgo && integration

// parity_postgres_test.go is the integration-tier arm of the
// `internal/graphsink` parity gate. It drives the AST
// dispatcher against the SAME fixture the memory + sqlite arms
// in `parity_test.go` use, but targets a live PostgreSQL
// cluster through the `graphsink/postgres` adapter, and
// asserts the captured `(repo_id, fingerprint, kind,
// canonical_signature)` Node tuples and `(kind,
// src_fingerprint, dst_fingerprint, fingerprint)` Edge tuples
// agree with the memory backend.
//
// BUILD TAGS
//
//   - `//go:build cgo && integration`. The `integration` tag
//     gates the file out of the default `go test ./...` run
//     (the unit run skips it) and switches it on in CI's
//     integration job. The `cgo` tag is inherited from
//     `parity_test.go` so the shared helpers in that file
//     compile in the same build.
//
// PROVISIONING
//
// The test reads `AGENT_MEMORY_PG_URL` for the cluster DSN
// (skipping cleanly when unset, the convention every
// `*_integration_test.go` in this repo follows), creates a
// per-test schema, pins `search_path` to it, applies every
// migration via `migrations.New(db).Up(ctx)`, and drops the
// schema on cleanup. It uses the OWNER connection
// (CREATEROLE / superuser) rather than flipping
// `agent_memory_app` to LOGIN; the parity assertion does not
// need to exercise the role-grant boundary -- that is the
// graphwriter integration test's job (`writer_integration_test.go`).
//
// SHARED HELPERS
//
// The fixture (`parityFixture`), the recording wrapper
// (`recordingSink`), the scan driver (`runScan`), the sort
// comparator (`sortParityRows`), and the assertion helpers
// (`assertNodesEqual`, `assertEdgesEqual`) all live in
// `parity_test.go` (build tag `cgo`). Because this file's
// constraint is a strict superset (`cgo && integration`), both
// files compile together when the integration build is
// requested and the shared helpers are in scope.
package graphsink_test

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

	_ "github.com/lib/pq"

	postgresadapter "github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/postgres"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/migrations"
)

const (
	parityEnvPGURL    = "AGENT_MEMORY_PG_URL"
	parityDBTimeoutDur = 60 * time.Second
)

// scanPostgres opens a per-test schema on the cluster pointed
// at by `AGENT_MEMORY_PG_URL`, applies every migration, wraps
// the resulting `*sql.DB` in a `graphwriter.Writer` +
// `graphsink/postgres.Sink`, and drives `runScan` against it.
//
// Returns the captured tuples and `t.Skip`s when the DSN is
// unset / unreachable, matching the convention in
// `internal/graphwriter/writer_integration_test.go` and
// `migrations/test_migrate_test.go`.
func scanPostgres(t *testing.T) (nodes []parityNodeRow, edges []parityEdgeRow) {
	t.Helper()
	dsn := os.Getenv(parityEnvPGURL)
	if dsn == "" {
		t.Skipf("skipping: %s is unset; the parity Postgres arm requires a live PostgreSQL", parityEnvPGURL)
	}

	owner, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	owner.SetMaxOpenConns(1)
	owner.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = owner.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), parityDBTimeoutDur)
	defer cancel()
	if err := owner.PingContext(ctx); err != nil {
		t.Skipf("skipping: cannot reach PostgreSQL at %s: %v", parityEnvPGURL, err)
	}

	schema := paritySchemaName(t)
	if _, err := owner.ExecContext(ctx, `CREATE SCHEMA `+parityQuoteIdent(schema)); err != nil {
		t.Fatalf("create schema %q: %v", schema, err)
	}
	t.Cleanup(func() {
		ctx2, c2 := context.WithTimeout(context.Background(), parityDBTimeoutDur)
		defer c2()
		schemaPrefix := strings.ReplaceAll(schema, "_", "#_") + ".%"
		_, _ = owner.ExecContext(ctx2,
			`DELETE FROM partman.part_config WHERE parent_table LIKE $1 ESCAPE '#'`,
			schemaPrefix)
		_, _ = owner.ExecContext(ctx2, `DROP SCHEMA `+parityQuoteIdent(schema)+` CASCADE`)
	})

	if _, err := owner.ExecContext(ctx, fmt.Sprintf(
		`SET search_path TO %s, public, partman`, parityQuoteIdent(schema),
	)); err != nil {
		t.Fatalf("set search_path: %v", err)
	}

	if err := migrations.New(owner).Up(ctx); err != nil {
		t.Fatalf("migrations.Up: %v", err)
	}

	writer := graphwriter.New(owner, nil)
	sink := postgresadapter.NewSink(writer)
	t.Cleanup(func() {
		_ = sink.Close()
	})

	rec := newRecordingSink(sink)
	return runScan(t, rec)
}

func paritySchemaName(t *testing.T) string {
	t.Helper()
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "amparity_" + hex.EncodeToString(buf[:])
}

func parityQuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// TestBackendParity_Postgres asserts the Postgres adapter
// (over a live cluster) emits Node + Edge tuples identical to
// the memory backend over the same fixture. This is the third
// arm of the parity gate; the memory + sqlite arms run from
// `parity_test.go` and do not require external services.
func TestBackendParity_Postgres(t *testing.T) {
	memNodes, memEdges := scanMemory(t)
	pgNodes, pgEdges := scanPostgres(t)

	if len(pgNodes) == 0 {
		t.Fatalf("postgres backend captured 0 node rows; expected the parity fixture to emit at least one Node")
	}
	if len(pgEdges) == 0 {
		t.Fatalf("postgres backend captured 0 edge rows; expected the parity fixture to emit at least one Edge")
	}

	assertNodesEqual(t, "memory", "postgres", memNodes, pgNodes)
	assertEdgesEqual(t, "memory", "postgres", memEdges, pgEdges)
}
