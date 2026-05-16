package testpglock

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"
)

const envPGURL = "AGENT_MEMORY_PG_URL"

// TestAcquireAppRoleLogin_rejectsEmptyDSN is the pure-unit
// guard: an empty DSN must surface as an error rather than
// opening a degenerate "postgres" handle that would fail later.
func TestAcquireAppRoleLogin_rejectsEmptyDSN(t *testing.T) {
	t.Parallel()
	if _, err := AcquireAppRoleLogin(context.Background(), ""); err == nil {
		t.Error("expected error for empty DSN; got nil")
	}
}

// TestAppRoleLoginKey_isExpectedMagic pins the deterministic
// bigint value cross-package callers depend on. A future refactor
// that accidentally changes the constant would silently break
// cluster-wide serialisation — this test fails loudly instead.
func TestAppRoleLoginKey_isExpectedMagic(t *testing.T) {
	t.Parallel()
	const want int64 = 0x41474E544D454D31 // "AGNTMEM1"
	if AppRoleLoginKey != want {
		t.Errorf("AppRoleLoginKey = %#x, want %#x (ASCII \"AGNTMEM1\")",
			AppRoleLoginKey, want)
	}
}

// TestAcquireAppRoleLogin_serialisesConcurrentAcquirers proves
// the cluster-wide serialisation guarantee that the migrations
// and graphwriter packages depend on: two goroutines that race
// to acquire MUST see the second one block until the first
// releases. Skips cleanly when AGENT_MEMORY_PG_URL is unset.
func TestAcquireAppRoleLogin_serialisesConcurrentAcquirers(t *testing.T) {
	dsn := os.Getenv(envPGURL)
	if dsn == "" {
		t.Skipf("skipping: %s is unset", envPGURL)
	}
	// Defence: confirm the cluster is actually reachable before
	// asserting the lock behaviour. Otherwise the test would
	// appear to "pass" by virtue of the Acquire call failing.
	probe, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("skipping: cannot sql.Open: %v", err)
	}
	pctx, pcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer pcancel()
	if err := probe.PingContext(pctx); err != nil {
		_ = probe.Close()
		t.Skipf("skipping: cannot reach PostgreSQL at %s: %v", envPGURL, err)
	}
	_ = probe.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Goroutine A acquires the lock and holds it for ~1s.
	holdStart := make(chan struct{})
	holdRelease := make(chan struct{})
	var releaseA func()
	go func() {
		var err error
		releaseA, err = AcquireAppRoleLogin(ctx, dsn)
		if err != nil {
			t.Errorf("A: Acquire: %v", err)
			close(holdStart)
			return
		}
		close(holdStart)
		<-holdRelease
		releaseA()
	}()
	<-holdStart
	if releaseA == nil {
		t.Fatal("goroutine A failed to acquire")
	}

	// Goroutine B tries to acquire while A is holding. We
	// expect B to block until A releases. The signal is the
	// time delta — B's Acquire returning before we drop
	// `holdRelease` would prove serialisation is broken.
	bGotAt := make(chan time.Time, 1)
	go func() {
		release, err := AcquireAppRoleLogin(ctx, dsn)
		if err != nil {
			t.Errorf("B: Acquire: %v", err)
			bGotAt <- time.Time{}
			return
		}
		bGotAt <- time.Now()
		release()
	}()

	// Confirm B is genuinely waiting (does not complete within
	// the wait window).
	select {
	case got := <-bGotAt:
		t.Fatalf("B acquired before A released; got at %v (locks NOT serialised)", got)
	case <-time.After(400 * time.Millisecond):
		// Expected: B is blocked.
	}

	releasedAt := time.Now()
	close(holdRelease) // A: release

	select {
	case got := <-bGotAt:
		if got.Before(releasedAt) {
			t.Errorf("B acquired before A released: got %v < released %v", got, releasedAt)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("B never acquired after A released; locks may be stuck")
	}
}

// TestAcquireAppRoleLogin_releaseDoesNotPanic confirms release
// completes cleanly when invoked exactly once (the contract the
// `success := false; defer if !success { release() }` pattern
// depends on). The closure runs both the explicit unlock and
// the *sql.DB Close in series.
func TestAcquireAppRoleLogin_releaseDoesNotPanic(t *testing.T) {
	dsn := os.Getenv(envPGURL)
	if dsn == "" {
		t.Skipf("skipping: %s is unset", envPGURL)
	}
	probe, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("skipping: cannot sql.Open: %v", err)
	}
	pctx, pcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer pcancel()
	if err := probe.PingContext(pctx); err != nil {
		_ = probe.Close()
		t.Skipf("skipping: cannot reach PostgreSQL at %s: %v", envPGURL, err)
	}
	_ = probe.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	release, err := AcquireAppRoleLogin(ctx, dsn)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()
}
