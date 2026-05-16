// Package testpglock provides cross-package serialisation for
// integration tests that mutate cluster-wide PostgreSQL state.
//
// Why this exists
// ---------------
// Multiple test packages in the agent-memory module each open
// their own fixture and (separately) flip the shared
// `agent_memory_app` role between NOLOGIN → LOGIN PASSWORD '<x>'
// → NOLOGIN to exercise the role-grant policy from tech-spec
// §8.7.4. Migration 0016 creates the role as NOLOGIN by design
// — production sets the password out-of-band — so the
// integration tests temporarily flip LOGIN, connect, run their
// assertions, and revert.
//
// That pattern is safe within ONE `go test ./somepkg` run (Go
// runs tests sequentially by default). But when `go test ./...`
// runs multiple packages concurrently against the same database
// (typically when AGENT_MEMORY_PG_URL is set against a shared
// docker-compose cluster), the role flip becomes a cluster-level
// race: package A's `ALTER ROLE agent_memory_app WITH LOGIN
// PASSWORD 'a'` can land between package B's
// `LOGIN PASSWORD 'b'` and B's connection attempt with 'b',
// producing a spurious authentication failure.
//
// Iter 2 of the GraphWriter library workstream introduced a
// second package (`internal/graphwriter`) that mutates the role,
// breaking the "migrations is the only package that touches it"
// assumption the existing fixture relied on.
//
// What this package does
// ----------------------
// `AcquireAppRoleLogin` opens its OWN dedicated *sql.DB to the
// supplied DSN, calls `SELECT pg_advisory_lock($AppRoleLoginKey)`,
// and returns a release closure. The lock is session-scoped:
// PostgreSQL holds it on the dedicated backend until the
// connection drops. Both call sites — `migrations` and
// `internal/graphwriter` — acquire this lock BEFORE the first
// `ALTER ROLE ... WITH LOGIN` and release it AFTER the
// corresponding `ALTER ROLE ... WITH NOLOGIN`, so the LOGIN
// window is mutually exclusive across every test binary.
//
// Design notes
// ------------
//
//   - The helper opens its OWN *sql.DB rather than borrowing a
//     *sql.Conn from the caller's pool. The caller's pool is
//     typically `SetMaxOpenConns(1)` to preserve session-local
//     `search_path` across the fixture's statements; stealing
//     that lone connection would deadlock the very `ALTER ROLE`
//     the lock is meant to serialise.
//
//   - Session-level (`pg_advisory_lock`) was picked over
//     transaction-level (`pg_advisory_xact_lock`) because the
//     LOGIN window straddles multiple top-level statements with
//     no enclosing transaction. PostgreSQL auto-releases
//     session-level advisory locks when the backend connection
//     closes — so a test process exit or panic without explicit
//     release will not leak the lock indefinitely. Within a
//     still-running test binary, explicit release is required to
//     unblock sibling tests promptly.
//
//   - The lock key is a fixed bigint that BOTH call sites import
//     from this package, so collision risk reduces to "another
//     unrelated process in the same cluster happened to pick the
//     same bigint" — vanishingly unlikely for a value derived
//     from ASCII "AGNTMEM1".
package testpglock

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/lib/pq" // register the "postgres" driver
)

// AppRoleLoginKey is the deterministic bigint
// pg_advisory_lock key both `migrations` and
// `internal/graphwriter` use to serialise `agent_memory_app`
// LOGIN/NOLOGIN flips. The numeric value is the big-endian
// ASCII encoding of "AGNTMEM1" — a `grep -F "AGNTMEM1"` finds
// every reference, and `printf '%x\n' "$(printf 'AGNTMEM1' | od
// -An -tx1 | tr -d ' ')"` reproduces it from the literal bytes.
//
// 0x41 0x47 0x4E 0x54 0x4D 0x45 0x4D 0x31 = "AGNTMEM1"
const AppRoleLoginKey int64 = 0x41474E544D454D31

// unlockTimeout bounds the best-effort `pg_advisory_unlock` call
// in the release closure so a misbehaving cluster doesn't pin
// the test goroutine. The lock itself drops automatically when
// the dedicated connection closes; the unlock RPC is only for
// pg_locks-query readability during debugging.
const unlockTimeout = 5 * time.Second

// AcquireAppRoleLogin opens a dedicated *sql.DB to the supplied
// PostgreSQL DSN, takes the cluster-wide
// `pg_advisory_lock(AppRoleLoginKey)` on a single pinned
// connection, and returns a release closure. Callers MUST hold
// the lock for the FULL LOGIN window (from the first `ALTER ROLE
// ... WITH LOGIN` to the final `ALTER ROLE ... WITH NOLOGIN`)
// and call release AFTER the NOLOGIN revert lands.
//
// Recommended call-site pattern:
//
//	release, err := testpglock.AcquireAppRoleLogin(ctx, dsn)
//	if err != nil { t.Fatalf("acquire role lock: %v", err) }
//	success := false
//	defer func() { if !success { release() } }()
//
//	// ALTER ROLE ... WITH LOGIN ...
//	// open appDB ...
//	// SET search_path ...
//
//	success = true
//	return appDB, func() {
//	    _ = appDB.Close()
//	    _, _ = ownerDB.Exec("ALTER ROLE agent_memory_app WITH NOLOGIN")
//	    release()
//	}
//
// The deferred `if !success { release() }` is load-bearing: a
// `t.Fatalf` after Acquire but before the cleanup hook is
// installed would otherwise leak the lock until process exit,
// stalling sibling packages.
func AcquireAppRoleLogin(ctx context.Context, dsn string) (release func(), err error) {
	if dsn == "" {
		return nil, errors.New("testpglock: empty dsn")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("testpglock: sql.Open: %w", err)
	}
	// Pin to one connection so the advisory lock stays on the
	// same backend session for its entire lifetime. The whole
	// purpose of this DB handle is to hold the lock, so a
	// single connection is sufficient.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("testpglock: ping: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		"SELECT pg_advisory_lock($1)", AppRoleLoginKey,
	); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf(
			"testpglock: pg_advisory_lock(%d): %w", AppRoleLoginKey, err,
		)
	}
	return func() {
		ctx2, cancel := context.WithTimeout(context.Background(), unlockTimeout)
		defer cancel()
		// Best-effort explicit unlock. Even if this fails or
		// times out, closing the connection drops the
		// session-level lock automatically.
		_, _ = db.ExecContext(ctx2,
			"SELECT pg_advisory_unlock($1)", AppRoleLoginKey,
		)
		_ = db.Close()
	}, nil
}
