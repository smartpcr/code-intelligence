package graphreader

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestNewPool_rejectsEmptyDSN ensures NewPool guards the most
// common call-site bug — forgetting to wire the DSN env var —
// before pgx fails opaquely on parse.
func TestNewPool_rejectsEmptyDSN(t *testing.T) {
	_, err := NewPool(context.Background(), "", PoolOptions{})
	if err == nil || !strings.Contains(err.Error(), "empty dsn") {
		t.Fatalf("empty DSN must be rejected, got %v", err)
	}
}

// TestNewPool_rejectsMalformedDSN ensures pgx parse failures
// propagate with a clear "parse dsn" prefix.
func TestNewPool_rejectsMalformedDSN(t *testing.T) {
	_, err := NewPool(context.Background(), "::not-a-dsn::", PoolOptions{})
	if err == nil {
		t.Fatal("malformed DSN must be rejected")
	}
	if !strings.Contains(err.Error(), "parse dsn") {
		t.Fatalf("error must mention parse dsn, got %v", err)
	}
}

// TestPoolDefaults_documentedConstants is a regression guard
// for the documented Default* constants. If a future patch
// silently shrinks DefaultMaxConns below 32 the integration
// suite's parallel coverage of the §8.3 burst envelope (200
// RPS) would start exhausting connections and flake — this
// test ensures the change is intentional.
func TestPoolDefaults_documentedConstants(t *testing.T) {
	if DefaultMaxConns < 32 {
		t.Errorf("DefaultMaxConns shrunk below 32 (got %d); §8.3 RPS envelope assumes ≥32",
			DefaultMaxConns)
	}
	if DefaultMinConns < 1 {
		t.Errorf("DefaultMinConns must keep at least one warm conn, got %d", DefaultMinConns)
	}
	if DefaultMaxConnLifetime < time.Minute {
		t.Errorf("DefaultMaxConnLifetime suspiciously short: %v", DefaultMaxConnLifetime)
	}
	if DefaultMaxConnLifetimeJitter <= 0 {
		t.Errorf("DefaultMaxConnLifetimeJitter must be positive to avoid synchronised reconnect spikes, got %v",
			DefaultMaxConnLifetimeJitter)
	}
	if DefaultMaxConnLifetimeJitter >= DefaultMaxConnLifetime {
		t.Errorf("jitter %v must be smaller than lifetime %v",
			DefaultMaxConnLifetimeJitter, DefaultMaxConnLifetime)
	}
}

// TestPoolDefaults_expectedRoleMatchesMigration pins the
// DefaultExpectedRole constant to the exact role name created
// in migration 0017. If a future migration renames the role
// the constant MUST change in lockstep; this test catches a
// silent drift before production startups begin failing.
func TestPoolDefaults_expectedRoleMatchesMigration(t *testing.T) {
	if DefaultExpectedRole != "agent_memory_ro" {
		t.Fatalf(
			"DefaultExpectedRole drifted from migration 0017_reader_role.sql: got %q, want %q",
			DefaultExpectedRole, "agent_memory_ro",
		)
	}
}

// TestNewPool_defaultsToReadOnlyRole verifies Fix 2: a caller
// that constructs a pool with the zero `PoolOptions{}` must
// still get the production read-only role assertion wired up.
// We use SkipEagerAcquire so the test does not need a live
// PostgreSQL — the only behaviour under test is that the
// option-defaulting code paints the AfterConnect hook with
// DefaultExpectedRole. After construction we discard the
// pool (it never opened a socket).
func TestNewPool_defaultsToReadOnlyRole(t *testing.T) {
	pool, err := NewPool(
		context.Background(),
		"postgres://example.invalid/db?sslmode=disable&connect_timeout=1",
		PoolOptions{SkipEagerAcquire: true},
	)
	if err != nil {
		t.Fatalf("NewPool with zero opts: %v", err)
	}
	defer pool.Close()
	cfg := pool.Config()
	if cfg.AfterConnect == nil {
		t.Fatal("AfterConnect must be installed when ExpectedRole defaults are applied")
	}
}

// TestNewPool_allowAnyRoleSkipsAssertion verifies the explicit
// opt-out: a caller that sets `AllowAnyRole = true` and no
// SearchPath gets no AfterConnect hook at all (so a non-
// `agent_memory_ro` role connection is accepted).
func TestNewPool_allowAnyRoleSkipsAssertion(t *testing.T) {
	pool, err := NewPool(
		context.Background(),
		"postgres://example.invalid/db?sslmode=disable&connect_timeout=1",
		PoolOptions{SkipEagerAcquire: true, AllowAnyRole: true},
	)
	if err != nil {
		t.Fatalf("NewPool with AllowAnyRole: %v", err)
	}
	defer pool.Close()
	cfg := pool.Config()
	if cfg.AfterConnect != nil {
		t.Fatal("AfterConnect must be nil when AllowAnyRole=true and no SearchPath is set")
	}
}

// TestNewPool_clampsMinConnsToMaxConns verifies Fix 3: a
// deployment that lowers only MaxConns must not end up with
// MinConns > MaxConns. Previously the defaults were applied
// independently and `MaxConns=1, MinConns=0` resolved to
// `MaxConns=1, MinConns=2`, which pgxpool rejects at first
// Acquire.
func TestNewPool_clampsMinConnsToMaxConns(t *testing.T) {
	pool, err := NewPool(
		context.Background(),
		"postgres://example.invalid/db?sslmode=disable&connect_timeout=1",
		PoolOptions{
			MaxConns:         1,
			SkipEagerAcquire: true,
			AllowAnyRole:     true,
		},
	)
	if err != nil {
		t.Fatalf("NewPool with MaxConns=1: %v", err)
	}
	defer pool.Close()
	cfg := pool.Config()
	if cfg.MaxConns != 1 {
		t.Fatalf("MaxConns: got %d, want 1", cfg.MaxConns)
	}
	if cfg.MinConns > cfg.MaxConns {
		t.Fatalf("MinConns (%d) must not exceed MaxConns (%d) after defaulting",
			cfg.MinConns, cfg.MaxConns)
	}
}

// TestNewPool_clampPreservesUserMinConnsWhenValid is the
// inverse guard for Fix 3: when the caller passes a MinConns
// that fits inside MaxConns we must honour it, not silently
// promote to DefaultMinConns. The previous "<= 0" check
// already did the right thing for explicit positive values,
// but Fix 3 added a post-default clamp so this test pins down
// the combined behaviour.
func TestNewPool_clampPreservesUserMinConnsWhenValid(t *testing.T) {
	pool, err := NewPool(
		context.Background(),
		"postgres://example.invalid/db?sslmode=disable&connect_timeout=1",
		PoolOptions{
			MaxConns:         8,
			MinConns:         3,
			SkipEagerAcquire: true,
			AllowAnyRole:     true,
		},
	)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	cfg := pool.Config()
	if cfg.MaxConns != 8 || cfg.MinConns != 3 {
		t.Fatalf("got MaxConns=%d MinConns=%d, want 8/3", cfg.MaxConns, cfg.MinConns)
	}
}

// TestNewPool_explicitExpectedRoleOverridesDefault confirms a
// caller can still pin a non-default role (e.g. a future
// `agent_memory_ro_v2` migration in flight) by passing
// ExpectedRole explicitly — the default only kicks in when
// the field is empty.
func TestNewPool_explicitExpectedRoleOverridesDefault(t *testing.T) {
	pool, err := NewPool(
		context.Background(),
		"postgres://example.invalid/db?sslmode=disable&connect_timeout=1",
		PoolOptions{
			ExpectedRole:     "agent_memory_ro_v2",
			SkipEagerAcquire: true,
		},
	)
	if err != nil {
		t.Fatalf("NewPool with explicit ExpectedRole: %v", err)
	}
	defer pool.Close()
	// We can't introspect the captured `want` from the
	// closure, but the presence of an AfterConnect hook is
	// the observable contract.
	if pool.Config().AfterConnect == nil {
		t.Fatal("AfterConnect must be installed when ExpectedRole is explicit")
	}
}
