package graphreader

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultMaxConns is the per-pool connection cap the GraphReader
// runs with when `PoolOptions.MaxConns` is left at zero. The
// number is sized to cover the tech-spec §8.3 RPS envelope:
//
//   - `agent.recall`        : 50 RPS sustained, 200 RPS burst
//   - `agent.expand`        : 20 RPS sustained
//   - `agent.summarize`     : 5  RPS sustained
//   - `mgmt.read.*`         : low double-digit RPS combined
//
// Each request issues 1-2 short SELECTs averaging ~50 ms of
// backend time under nominal load (per the indexed schema in
// migrations 0003-0005). 50 RPS × 50 ms ≈ 2.5 concurrent
// backends for `recall` steady-state; a 200 RPS burst pushes
// the working set to ~10. Adding `expand`, `summarize`, and
// the management read traffic doubles the ceiling, and we
// keep ~50% headroom so the pool never becomes the SLO
// bottleneck — for an agent-memory pod backing the production
// `agent-api` and `mgmt-api` binaries, 32 maps onto a
// PostgreSQL `max_connections = 200` cluster with room to
// spare for the writer (`agent_memory_app`) and admin
// (`agent_memory_admin`) sessions sharing the cluster.
//
// Tune via `PoolOptions.MaxConns` per deployment if the
// per-pod fanout differs (e.g. one beefier reader pod at
// 64 conns or many lightweight pods at 8 conns).
const DefaultMaxConns int32 = 32

// DefaultMaxConnLifetime caps a backend session at 30 minutes
// before pgxpool rotates it. This bounds how long a single
// connection's `search_path`, role membership, or stale
// prepared-statement cache can linger after an operator
// changes things behind our back.
const DefaultMaxConnLifetime = 30 * time.Minute

// DefaultMaxConnLifetimeJitter spreads the per-connection
// recycle deadline across a 5-minute window so a pod
// restarted at t=0 doesn't end up reconnecting every backend
// at the same wall-clock moment 30 minutes later. Without
// jitter a deployment fleet can synchronise its connection
// thrash and saturate the cluster's auth path.
const DefaultMaxConnLifetimeJitter = 5 * time.Minute

// DefaultMaxConnIdleTime trims idle backends after 5 minutes so
// the cluster `pg_stat_activity` view doesn't accumulate
// long-lived idle connections from a quiet shift.
const DefaultMaxConnIdleTime = 5 * time.Minute

// DefaultMinConns keeps two backends pre-warmed per pod so the
// first `agent.recall` after a quiet window doesn't pay the
// ~100ms TCP+TLS handshake on the hot path. Two is enough to
// absorb a single-request burst; the rest scale up on demand.
const DefaultMinConns int32 = 2

// DefaultExpectedRole is the PostgreSQL role name the
// GraphReader's `pgxpool.Pool` is expected to authenticate as
// in production. It matches migration 0017's `agent_memory_ro`
// (SELECT-only on every graph-readable table). NewPool defaults
// `PoolOptions.ExpectedRole` to this constant unless the caller
// explicitly opts out with `PoolOptions.AllowAnyRole = true`,
// so a misconfigured deployment that points the reader binary
// at a writer-role DSN (`agent_memory_app`) fails at startup
// rather than silently re-introducing the G5 violation
// migration 0017 was designed to close.
const DefaultExpectedRole = "agent_memory_ro"

// PoolOptions overrides the per-pool defaults in `NewPool`.
// Every field is optional; zero values fall back to the
// Default* constants documented above.
type PoolOptions struct {
	// MaxConns caps the pool size. Pass zero to use
	// DefaultMaxConns (sized for the §8.3 RPS envelope).
	MaxConns int32
	// MinConns is the warm-pool size. Pass zero for
	// DefaultMinConns. NewPool clamps `MinConns` to the final
	// `MaxConns` so an override like `MaxConns=1, MinConns=0`
	// resolves to `MaxConns=1, MinConns=1` rather than the
	// nonsensical `MaxConns=1, MinConns=2` you'd get if each
	// field were defaulted independently.
	MinConns int32
	// MaxConnLifetime caps how long a single backend session
	// is reused. Pass zero for DefaultMaxConnLifetime.
	MaxConnLifetime time.Duration
	// MaxConnLifetimeJitter spreads the per-connection
	// recycle deadline. Pass zero for DefaultMaxConnLifetimeJitter.
	MaxConnLifetimeJitter time.Duration
	// MaxConnIdleTime caps the idle-pool dwell time. Pass
	// zero for DefaultMaxConnIdleTime.
	MaxConnIdleTime time.Duration
	// SearchPath is an optional SQL `SET search_path` value
	// applied on every new backend connection via the pgxpool
	// `AfterConnect` hook. Production wiring usually pins
	// this to the per-environment schema (e.g. `agent_memory,
	// public, partman`); integration tests pin it to the
	// per-test schema so the reader resolves unqualified
	// table names against the same isolated namespace the
	// writer used.
	SearchPath string
	// ExpectedRole overrides the default role assertion. When
	// empty (the common case) NewPool uses DefaultExpectedRole
	// (`agent_memory_ro`). When AllowAnyRole = true the
	// assertion is skipped entirely; in that case any value
	// in ExpectedRole is ignored.
	//
	// NewPool verifies the assertion eagerly by acquiring a
	// connection before returning, so a wrong-role DSN fails
	// at construction time rather than the first read. The
	// returned error wraps the actual role pgx authenticated
	// as so operators can debug the deployment misconfiguration
	// without scraping logs.
	ExpectedRole string
	// AllowAnyRole disables the role assertion. Set this only
	// in tests / tooling that intentionally connect as a
	// non-`agent_memory_ro` role (e.g. an admin-role
	// connection that needs to seed fixture data through the
	// same pool wiring). Production callers MUST leave this
	// false so the database-layer DML defence stays in place.
	AllowAnyRole bool
	// SkipEagerAcquire short-circuits the connection acquire
	// NewPool performs as its final step. It exists for the
	// narrow set of tests that want to construct a pool
	// without touching the network at all (e.g. unit-testing
	// the option-defaulting logic itself). Production callers
	// MUST leave this false so the eager AfterConnect check
	// fires before NewPool returns. This is intentionally
	// distinct from AllowAnyRole — a caller may want to keep
	// the role assertion but still skip the network round-trip
	// during a dry-run construction.
	SkipEagerAcquire bool
}

// NewPool constructs the *pgxpool.Pool the GraphReader runs
// over. The DSN must authenticate as the read-only role
// (`agent_memory_ro` per migration 0017) in production
// deployments; integration tests may set
// `PoolOptions.AllowAnyRole = true` when they intentionally
// connect as a different role.
//
// The returned pool is owned by the caller — `defer pool.Close()`
// on shutdown.
//
// Eager verification
// ------------------
// Unless `PoolOptions.SkipEagerAcquire` is true, NewPool
// acquires one connection from the freshly-constructed pool
// before returning, releasing it back to the pool on success.
// That round-trip is what makes the `AfterConnect` hooks
// (search_path SET, ExpectedRole assertion) observable to the
// caller — without it pgxpool defers connection creation
// until the first user query, and a wrong-role DSN would
// only surface later from a deep call site that has no easy
// way to fail-fast the binary startup.
//
// Acquire failures close the pool and propagate the error so
// the caller does not have to remember to free a half-built
// pool.
func NewPool(ctx context.Context, dsn string, opts PoolOptions) (*pgxpool.Pool, error) {
	if dsn == "" {
		return nil, errors.New("graphreader: NewPool: empty dsn")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("graphreader: parse dsn: %w", err)
	}

	// Apply per-field defaults first, then clamp MinConns to
	// the effective MaxConns. Independent defaulting would
	// produce nonsense for a deployment that lowers only
	// MaxConns (e.g. MaxConns=1, MinConns=0 → MaxConns=1,
	// MinConns=2 with pgxpool then refusing to construct).
	cfg.MaxConns = opts.MaxConns
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = DefaultMaxConns
	}
	cfg.MinConns = opts.MinConns
	if cfg.MinConns <= 0 {
		cfg.MinConns = DefaultMinConns
	}
	if cfg.MinConns > cfg.MaxConns {
		cfg.MinConns = cfg.MaxConns
	}
	cfg.MaxConnLifetime = opts.MaxConnLifetime
	if cfg.MaxConnLifetime <= 0 {
		cfg.MaxConnLifetime = DefaultMaxConnLifetime
	}
	cfg.MaxConnLifetimeJitter = opts.MaxConnLifetimeJitter
	if cfg.MaxConnLifetimeJitter <= 0 {
		cfg.MaxConnLifetimeJitter = DefaultMaxConnLifetimeJitter
	}
	cfg.MaxConnIdleTime = opts.MaxConnIdleTime
	if cfg.MaxConnIdleTime <= 0 {
		cfg.MaxConnIdleTime = DefaultMaxConnIdleTime
	}

	// Resolve the effective ExpectedRole. The brief calls for
	// the production read-only role to be the default; callers
	// who want to deviate (admin-role fixture seeding,
	// connecting as `postgres` in a sandbox) MUST explicitly
	// pass AllowAnyRole so the deviation is auditable in code.
	effectiveRole := opts.ExpectedRole
	if effectiveRole == "" && !opts.AllowAnyRole {
		effectiveRole = DefaultExpectedRole
	}
	if opts.AllowAnyRole {
		effectiveRole = ""
	}

	// Compose the optional AfterConnect hooks. SearchPath runs
	// first (it's stateful — every later query depends on it),
	// ExpectedRole runs second (it's a one-shot assertion
	// against the same connection). Either may be absent; if
	// both are absent we leave AfterConnect at nil so pgxpool
	// skips the indirection entirely.
	var hooks []func(context.Context, *pgx.Conn) error
	if sp := opts.SearchPath; sp != "" {
		// The hook runs once per new backend session. We use
		// `set_config(..., false)` rather than `SET LOCAL`
		// because SET LOCAL is transaction-scoped and would
		// not survive into the actual query the caller runs.
		// `false` for is_local pins the value for the lifetime
		// of the session.
		hooks = append(hooks, func(ctx context.Context, conn *pgx.Conn) error {
			_, err := conn.Exec(ctx, "SELECT set_config('search_path', $1, false)", sp)
			if err != nil {
				return fmt.Errorf("graphreader: SET search_path: %w", err)
			}
			return nil
		})
	}
	if effectiveRole != "" {
		want := effectiveRole
		hooks = append(hooks, func(ctx context.Context, conn *pgx.Conn) error {
			var got string
			if err := conn.QueryRow(ctx, "SELECT current_user").Scan(&got); err != nil {
				return fmt.Errorf("graphreader: check current_user: %w", err)
			}
			if got != want {
				return fmt.Errorf(
					"graphreader: pool authenticated as %q, expected %q "+
						"(production wiring requires the read-only role; "+
						"see migration 0017 — set PoolOptions.AllowAnyRole = true "+
						"to bypass this check)",
					got, want,
				)
			}
			return nil
		})
	}
	if len(hooks) > 0 {
		cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			for _, h := range hooks {
				if err := h(ctx, conn); err != nil {
					return err
				}
			}
			return nil
		}
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("graphreader: create pool: %w", err)
	}

	// Eagerly trigger AfterConnect so role / search_path /
	// connectivity issues surface as a NewPool error rather
	// than a deferred failure from the first user query. The
	// SkipEagerAcquire knob exists for the narrow set of
	// unit-test paths that intentionally construct a pool
	// without touching the network.
	if !opts.SkipEagerAcquire {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("graphreader: eager acquire: %w", err)
		}
		conn.Release()
	}
	return pool, nil
}
