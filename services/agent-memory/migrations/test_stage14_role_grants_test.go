package migrations_test

// Stage 1.4 integration tests covering the role-grant scenarios
// from implementation-plan.md:
//
//   * "Add an integration test that connects as agent_memory_app
//      and asserts UPDATE on Node fails with permission denied"
//       -> TestRoleGrants_appRoleCannotUpdateNode
//   * "Add an integration test that connects as agent_memory_app
//      and asserts UPDATE on TraceObservation succeeds"
//       -> TestRoleGrants_appRoleCanUpdateTraceObservation
//
// Both tests open a SECOND *sql.DB connected as the
// `agent_memory_app` role (not via SET ROLE -- the
// implementation-plan and acceptance scenarios both use the
// phrase "logged in as agent_memory_app", so the test must
// exercise the actual authentication path).
//
// How LOGIN is enabled
// --------------------
// 0016_roles_grants.sql creates `agent_memory_app` as NOLOGIN by
// design: production deploys set the password via a separate
// `ALTER ROLE ... WITH LOGIN PASSWORD '...'` so the credential
// never lives in source. The integration-test fixture mirrors
// that pattern -- after Up() the test does the LOGIN+password
// ALTER, opens a fresh connection as the app role, runs the
// scenario, then reverts the role to NOLOGIN.
//
// Concurrency: the cluster-wide `agent_memory_app` role is
// shared. Within one `go test ./migrations` invocation the tests
// run sequentially (Go's default), so the LOGIN flip is
// contained. Stage 2.1 introduced a second test package
// (`internal/graphwriter`) that also flips this role; both call
// sites now acquire `testpglock.AcquireAppRoleLogin` (a
// session-level pg_advisory_lock keyed on the same constant)
// BEFORE the LOGIN ALTER and release it AFTER the NOLOGIN
// revert, so cross-package races during `go test ./...` are
// serialised at the cluster level rather than left to chance.
//
// As with the other tests in this package, the file is skipped
// cleanly when AGENT_MEMORY_PG_URL is unset (developer laptop
// without the docker compose stack).

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/lib/pq" // *pq.Error gives us SQLSTATE strings

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/testpglock"
)

// pgErrCodeInsufficientPrivilege is the SQLSTATE the per-test
// assertions below match against. PostgreSQL returns
// "42501 -- permission denied for table <name>" when a role
// without UPDATE on a relation attempts UPDATE; the SQLSTATE is
// the load-bearing signal (the message text is locale-sensitive).
const pgErrCodeInsufficientPrivilege = "42501"

// openAppRoleDB enables LOGIN on the cluster-wide
// `agent_memory_app` role with a per-test random password,
// then opens a SECOND *sql.DB connected as that role with
// search_path pinned to the per-test schema. The returned
// cleanup hook closes the new handle AND reverts the role to
// NOLOGIN (the safe production default).
//
// ownerDB is the existing migration-owner connection. It must
// have CREATEROLE or superuser to issue ALTER ROLE.
//
// The connection-string surgery is done with net/url so the
// caller does not have to know the docker-compose username,
// host, or sslmode -- only the env-var URL.
func openAppRoleDB(
	t *testing.T, ownerDB *sql.DB, schema string,
) (*sql.DB, func()) {
	t.Helper()

	base := os.Getenv("AGENT_MEMORY_PG_URL")
	if base == "" {
		// openTestDB already skipped, but defend against
		// future callers that reuse this helper.
		t.Skip("AGENT_MEMORY_PG_URL not set; cannot connect as app role")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %s: %v", base, err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		t.Fatalf("AGENT_MEMORY_PG_URL must be a postgres:// URL "+
			"(got scheme %q); keyword=value DSNs are not supported "+
			"by this helper", u.Scheme)
	}

	// Per-test random password keeps two test invocations from
	// stomping each other's credentials, even though the
	// cluster-wide role is shared.
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	password := "amapp_" + hex.EncodeToString(buf[:])

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	// Cross-package serialisation: take the shared
	// pg_advisory_lock before we flip the role to LOGIN. The
	// lock owns its own *sql.DB on a session-pinned
	// connection, so it does NOT borrow from ownerDB's pool
	// (capped at 1 connection here) and cannot deadlock the
	// subsequent ALTER ROLE statements.
	releaseLock, err := testpglock.AcquireAppRoleLogin(ctx, base)
	if err != nil {
		t.Fatalf("acquire app-role login lock: %v", err)
	}
	// On the failure path, runtime.Goexit (triggered by
	// t.Fatalf) unwinds deferred functions before terminating
	// the goroutine, so the lock is released even though the
	// caller never sees the returned cleanup closure.
	// On the success path, success = true silences this guard
	// and the returned cleanup func owns the release.
	success := false
	defer func() {
		if !success {
			releaseLock()
		}
	}()

	// pq.QuoteLiteral handles the single-quote escaping for the
	// password literal. ALTER ROLE in PostgreSQL does not
	// support parameter binding for password values, so the
	// literal must be inlined safely.
	if _, err := ownerDB.ExecContext(ctx,
		`ALTER ROLE agent_memory_app WITH LOGIN PASSWORD `+
			pq.QuoteLiteral(password),
	); err != nil {
		t.Fatalf("enable LOGIN on agent_memory_app: %v", err)
	}

	// Build the new connection URL: same host/db/sslmode as the
	// owner, but user=agent_memory_app + the random password.
	u2 := *u
	u2.User = url.UserPassword("agent_memory_app", password)
	appDB, err := sql.Open("postgres", u2.String())
	if err != nil {
		// Still revert the role before failing.
		_, _ = ownerDB.ExecContext(context.Background(),
			`ALTER ROLE agent_memory_app WITH NOLOGIN`)
		t.Fatalf("sql.Open(app-role): %v", err)
	}
	// Pin to one connection so the post-Open `SET search_path`
	// lands on the same backend session every subsequent
	// statement uses (mirrors openTestDB's reasoning).
	appDB.SetMaxOpenConns(1)
	appDB.SetMaxIdleConns(1)

	if err := appDB.PingContext(ctx); err != nil {
		_ = appDB.Close()
		_, _ = ownerDB.ExecContext(context.Background(),
			`ALTER ROLE agent_memory_app WITH NOLOGIN`)
		t.Fatalf("ping app-role db (does pg_hba.conf permit "+
			"agent_memory_app to LOGIN?): %v", err)
	}

	// The app role connects with `public` as the default
	// search_path; the test data lives in the per-test schema,
	// so we must SET search_path on this fresh session too.
	// USAGE on the schema is granted by 0016 -- without it,
	// even the SET would fail (search_path SET itself doesn't
	// validate permissions, but subsequent table lookups would).
	if _, err := appDB.ExecContext(ctx,
		`SET search_path TO `+quoteIdent(schema)+`, public`,
	); err != nil {
		_ = appDB.Close()
		_, _ = ownerDB.ExecContext(context.Background(),
			`ALTER ROLE agent_memory_app WITH NOLOGIN`)
		t.Fatalf("SET search_path on app-role session: %v", err)
	}

	success = true
	cleanup := func() {
		_ = appDB.Close()
		// Best-effort revert. Even if this fails (e.g. owner
		// already torn down), the next test's openAppRoleDB
		// will overwrite the password and re-flip LOGIN.
		ctx2, c2 := context.WithTimeout(
			context.Background(), testDBTimeout)
		defer c2()
		_, _ = ownerDB.ExecContext(ctx2,
			`ALTER ROLE agent_memory_app WITH NOLOGIN`)
		// Release the cross-package advisory lock AFTER the
		// NOLOGIN revert so the next acquirer never observes
		// this test's LOGIN state.
		releaseLock()
	}
	return appDB, cleanup
}

// TestRoleGrants_appRoleCannotUpdateNode is the
// "app role cannot UPDATE Node" scenario from
// implementation-plan.md Stage 1.4. The role 0016 created has
// INSERT + SELECT on `node` but no UPDATE; the assertion is on
// SQLSTATE 42501 (insufficient_privilege).
//
// Connection model: the seed inserts run on the migration-owner
// connection (which has all privileges), then the UPDATE runs
// on a SEPARATE *sql.DB connection authenticated as
// `agent_memory_app` -- exercising the full LOGIN path the
// implementation-plan calls out. This is stronger than the
// SET ROLE pattern because it also exercises pg_hba.conf and
// the role's password handling.
func TestRoleGrants_appRoleCannotUpdateNode(t *testing.T) {
	ownerDB, schema, cleanup := openTestDB(t)
	defer cleanup()
	mustUp(t, ownerDB)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	// Seed a repo + node as the migration owner. The node row
	// is the UPDATE target the app role will be denied on.
	var repoID string
	if err := ownerDB.QueryRowContext(ctx, `
		INSERT INTO repo (url, default_branch, current_head_sha)
		VALUES ('https://example.test/role-grants-node', 'main', 'aa00aa00')
		RETURNING repo_id
	`).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	var nodeID string
	if err := ownerDB.QueryRowContext(ctx, `
		INSERT INTO node (fingerprint, repo_id, kind, canonical_signature, from_sha)
		VALUES (decode('0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a', 'hex'),
		        $1, 'method', 'pkg.RoleTest#hit()', 'aa00aa00')
		RETURNING node_id
	`, repoID).Scan(&nodeID); err != nil {
		t.Fatalf("seed node: %v", err)
	}

	// Open a SECOND connection as agent_memory_app. The
	// returned cleanup reverts the role to NOLOGIN.
	appDB, appCleanup := openAppRoleDB(t, ownerDB, schema)
	defer appCleanup()

	// Sanity: confirm the app role identity on the new
	// connection. This tests that the LOGIN actually happened
	// (vs accidentally re-using the owner connection).
	var who string
	if err := appDB.QueryRowContext(ctx, `SELECT current_user`).Scan(&who); err != nil {
		t.Fatalf("current_user on app-role connection: %v", err)
	}
	if who != "agent_memory_app" {
		t.Fatalf("current_user on app-role connection = %q, want %q",
			who, "agent_memory_app")
	}

	// The forbidden statement. We expect SQLSTATE 42501.
	_, err := appDB.ExecContext(ctx, `
		UPDATE node SET attrs_json = '{}'::jsonb WHERE node_id = $1
	`, nodeID)
	if err == nil {
		t.Fatal("expected permission denied (SQLSTATE 42501); got nil")
	}
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		t.Fatalf("expected *pq.Error, got %T: %v", err, err)
	}
	if string(pqErr.Code) != pgErrCodeInsufficientPrivilege {
		t.Errorf("SQLSTATE = %q, want %q (msg: %v)",
			string(pqErr.Code), pgErrCodeInsufficientPrivilege, err)
	}
	// Defence in depth: the message text *should* mention
	// "permission denied" -- this is a soft assertion
	// (locale-sensitive) so we only Logf, not Errorf.
	if !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
		t.Logf("note: error text does not contain 'permission denied' "+
			"(locale-sensitive; SQLSTATE is the load-bearing assertion): %v", err)
	}
}

// TestRoleGrants_appRoleCanUpdateTraceObservation is the
// "app role can UPDATE TraceObservation" scenario. The §8.7.4
// classification keeps `trace_observation` UPDATE-grantable
// (mutable counter row; provenance is the append-only
// `trace_observation_log`), so 0016 grants UPDATE on it. The
// canonical mutation the writer issues is an
// observation_count increment; the test asserts the UPDATE
// succeeds AND affects exactly one row.
//
// Same connection model as
// TestRoleGrants_appRoleCannotUpdateNode -- seeds run on the
// owner connection, the UPDATE runs on a fresh agent_memory_app
// connection.
func TestRoleGrants_appRoleCanUpdateTraceObservation(t *testing.T) {
	ownerDB, schema, cleanup := openTestDB(t)
	defer cleanup()
	mustUp(t, ownerDB)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	// Seed a repo + src/dst nodes + observed_calls edge +
	// trace_observation row as the migration owner.
	var repoID string
	if err := ownerDB.QueryRowContext(ctx, `
		INSERT INTO repo (url, default_branch, current_head_sha)
		VALUES ('https://example.test/role-grants-trace', 'main', 'bb11bb11')
		RETURNING repo_id
	`).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	var srcID, dstID string
	if err := ownerDB.QueryRowContext(ctx, `
		INSERT INTO node (fingerprint, repo_id, kind, canonical_signature, from_sha)
		VALUES (decode('1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b', 'hex'),
		        $1, 'method', 'pkg.RoleSrc#a()', 'bb11bb11')
		RETURNING node_id
	`, repoID).Scan(&srcID); err != nil {
		t.Fatalf("seed src node: %v", err)
	}
	if err := ownerDB.QueryRowContext(ctx, `
		INSERT INTO node (fingerprint, repo_id, kind, canonical_signature, from_sha)
		VALUES (decode('2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c', 'hex'),
		        $1, 'method', 'pkg.RoleDst#b()', 'bb11bb11')
		RETURNING node_id
	`, repoID).Scan(&dstID); err != nil {
		t.Fatalf("seed dst node: %v", err)
	}
	var edgeID string
	if err := ownerDB.QueryRowContext(ctx, `
		INSERT INTO edge (fingerprint, repo_id, kind, src_node_id, dst_node_id, from_sha)
		VALUES (decode('3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d', 'hex'),
		        $1, 'observed_calls', $2, $3, 'bb11bb11')
		RETURNING edge_id
	`, repoID, srcID, dstID).Scan(&edgeID); err != nil {
		t.Fatalf("seed edge: %v", err)
	}
	if _, err := ownerDB.ExecContext(ctx, `
		INSERT INTO trace_observation (edge_id, observation_count, p50_latency_ms, p95_latency_ms)
		VALUES ($1, 0, 0, 0)
	`, edgeID); err != nil {
		t.Fatalf("seed trace_observation: %v", err)
	}

	// Open a fresh connection as agent_memory_app.
	appDB, appCleanup := openAppRoleDB(t, ownerDB, schema)
	defer appCleanup()

	// Sanity: real LOGIN, not a SET ROLE.
	var who string
	if err := appDB.QueryRowContext(ctx, `SELECT current_user`).Scan(&who); err != nil {
		t.Fatalf("current_user on app-role connection: %v", err)
	}
	if who != "agent_memory_app" {
		t.Fatalf("current_user on app-role connection = %q, want %q",
			who, "agent_memory_app")
	}

	// The canonical Span Ingestor mutation: bump the counter.
	res, err := appDB.ExecContext(ctx, `
		UPDATE trace_observation
		SET observation_count = observation_count + 1
		WHERE edge_id = $1
	`, edgeID)
	if err != nil {
		t.Fatalf("UPDATE trace_observation should succeed under agent_memory_app: %v", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("RowsAffected: %v", err)
	}
	if rows != 1 {
		t.Errorf("UPDATE affected %d rows, want 1", rows)
	}

	// Defence in depth: read the new counter back via SELECT
	// (still under agent_memory_app -- SELECT is granted on
	// every table the app role can INSERT or UPDATE).
	var count int64
	if err := appDB.QueryRowContext(ctx, `
		SELECT observation_count FROM trace_observation WHERE edge_id = $1
	`, edgeID).Scan(&count); err != nil {
		t.Fatalf("SELECT trace_observation under app role: %v", err)
	}
	if count != 1 {
		t.Errorf("observation_count after UPDATE = %d, want 1", count)
	}
}

// TestRoleGrants_rolesExist is a thin smoke test: after the
// migration set runs, the two roles must be present in
// pg_roles. The test does not assert on per-grant ACL state
// (that's covered by the UPDATE-allow / UPDATE-deny tests
// above); it guards against the migration silently no-op'ing
// the CREATE ROLE block when both roles happen to pre-exist
// from a prior run, AND it documents the role-naming contract
// at the assertion layer so a future rename surfaces here.
func TestRoleGrants_rolesExist(t *testing.T) {
	db, _, cleanup := openTestDB(t)
	defer cleanup()
	mustUp(t, db)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	for _, role := range []string{"agent_memory_app", "agent_memory_admin"} {
		var exists bool
		if err := db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`,
			role,
		).Scan(&exists); err != nil {
			t.Fatalf("pg_roles lookup for %s: %v", role, err)
		}
		if !exists {
			t.Errorf("role %s missing from pg_roles after Up()", role)
		}
	}
}
