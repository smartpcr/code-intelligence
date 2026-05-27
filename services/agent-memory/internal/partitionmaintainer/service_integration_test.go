package partitionmaintainer

// Integration tests for the Stage 8.2 partition-rotation
// maintainer against a live PostgreSQL 16 + pg_partman v5
// instance. Skips cleanly when AGENT_MEMORY_PG_URL is unset,
// mirroring the convention used by every other integration
// suite in this repo (graphwriter, recallcontext, tracelogpruner).
//
// Implementation-plan.md Stage 8.2 acceptance scenarios:
//
//   * "forward partitions always present" -- after migrations
//     run pg_partman with p_premake := 3 has provisioned at
//     least the next 3 months of children on every monthly
//     partitioned parent (and the next 3 weeks on the weekly
//     trace_observation_log).
//
//   * "lag alert fires" -- when the forward partition window
//     has been allowed to drain past (now() - 1 day), the
//     partition_provision_lag gauge (whole seconds) exceeds 86400.
//
//   * "chaos: scheduler disabled for an hour" -- writes into
//     every partitioned table at NOW() still succeed without
//     touching the default partition (premake=3 covers the
//     1-hour outage trivially -- this proves the implementation
//     plan's premake-as-buffer invariant).

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/migrations"
)

const (
	envPGURL      = "AGENT_MEMORY_PG_URL"
	testDBTimeout = 60 * time.Second
)

// maintainerFixture is the per-test PostgreSQL substrate. The
// maintainer is privileged-by-design (run_maintenance issues
// CREATE TABLE and DETACH PARTITION which both require
// ownership) so we open as the migration-runner owner role --
// same shape as tracelogpruner's prunerFixture.
type maintainerFixture struct {
	db      *sql.DB
	schema  string
	cleanup func()
	// parents is the list of partman-registered parents in
	// this test's schema (set after migrations run).
	parents []string
}

func openMaintainerFixture(t *testing.T) *maintainerFixture {
	t.Helper()
	base := os.Getenv(envPGURL)
	if base == "" {
		t.Skipf("skipping: %s is unset; integration tests require a live PostgreSQL", envPGURL)
	}

	owner, err := sql.Open("postgres", base)
	if err != nil {
		t.Fatalf("sql.Open owner: %v", err)
	}
	// Single-connection pool so the per-test SET search_path
	// is sticky across every subsequent query (graphwriter /
	// recallcontext / tracelogpruner all use this pattern --
	// any change in pool size here would break the integration
	// path silently).
	owner.SetMaxOpenConns(1)
	owner.SetMaxIdleConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()
	if err := owner.PingContext(ctx); err != nil {
		_ = owner.Close()
		t.Skipf("skipping: cannot reach PostgreSQL at %s: %v", envPGURL, err)
	}
	schema := newSchemaName(t)
	if _, err := owner.ExecContext(ctx, `CREATE SCHEMA `+quoteIdent(schema)); err != nil {
		_ = owner.Close()
		t.Fatalf("create schema %q: %v", schema, err)
	}
	// search_path includes partman so the scrape's
	// `partman.show_partition_info` / `partman.run_maintenance`
	// calls resolve. Also includes public so any pgcrypto
	// helpers used by migrations resolve unqualified.
	if _, err := owner.ExecContext(ctx, fmt.Sprintf(
		`SET search_path TO %s, public, partman`, quoteIdent(schema),
	)); err != nil {
		_ = owner.Close()
		t.Fatalf("set search_path: %v", err)
	}
	if err := migrations.New(owner).Up(ctx); err != nil {
		_ = owner.Close()
		t.Fatalf("migrations.Up: %v", err)
	}

	// Discover the partman-registered parents in this schema.
	// Use the same `split_part(...)` filter shape as the
	// production scrape so the test exercises the real query.
	rows, err := owner.QueryContext(ctx, `
		SELECT parent_table
		  FROM partman.part_config
		 WHERE split_part(parent_table, '.', 1) = $1
		 ORDER BY parent_table
	`, schema)
	if err != nil {
		_ = owner.Close()
		t.Fatalf("discover parents: %v", err)
	}
	var parents []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			_ = rows.Close()
			_ = owner.Close()
			t.Fatalf("scan parent: %v", err)
		}
		parents = append(parents, p)
	}
	_ = rows.Close()
	if len(parents) == 0 {
		_ = owner.Close()
		t.Fatalf("no partman-registered parents found in schema %q after migrations", schema)
	}

	cleanup := func() {
		ctx2, c2 := context.WithTimeout(context.Background(), testDBTimeout)
		defer c2()
		// Drop partman.part_config rows pointing at tables in
		// this schema BEFORE the DROP SCHEMA so partman's
		// cluster-wide state stays clean. Same defence as
		// tracelogpruner and migrations test fixtures.
		schemaPrefix := strings.ReplaceAll(schema, "_", "#_") + ".%"
		_, _ = owner.ExecContext(ctx2, `
			DELETE FROM partman.part_config
			WHERE parent_table LIKE $1 ESCAPE '#'
		`, schemaPrefix)
		_, _ = owner.ExecContext(ctx2, `DROP SCHEMA `+quoteIdent(schema)+` CASCADE`)
		_ = owner.Close()
	}
	return &maintainerFixture{
		db:      owner,
		schema:  schema,
		cleanup: cleanup,
		parents: parents,
	}
}

func newSchemaName(t *testing.T) string {
	t.Helper()
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "ammaint_" + hex.EncodeToString(buf[:])
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// silentLogger returns a logger that drops everything except
// Warn+ to keep integration test output readable.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// newMaintainer constructs a Service scoped to this test's
// partman-registered parents. Using the explicit ParentTables
// list avoids any chance of touching another tenant's parents
// when multiple test schemas coexist on the same cluster.
func newMaintainer(t *testing.T, fix *maintainerFixture) *Service {
	t.Helper()
	svc, err := New(fix.db, Config{
		ParentTables:        fix.parents,
		MaintenanceInterval: time.Hour,
		MaintenanceTimeout:  testDBTimeout,
		LagScrapeInterval:   time.Hour,
		LagScrapeTimeout:    testDBTimeout,
	}, silentLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

// TestForwardPartitionsAlwaysPresent realises the
// implementation-plan §8.2 "forward partitions always present"
// scenario: after migrations run, pg_partman with `p_premake :=
// 3` has provisioned at least the next 3 forward children on
// every registered parent.
//
// "Forward" here means children whose lower bound is at or
// after `now()`. The default partition is excluded from the
// count -- it has no bounds and is not a forward provisioning
// signal.
//
// We tolerate a count higher than 3 because pg_partman may
// have also provisioned the CURRENT-period child during the
// migration; the contract is "at least 3 forward", not "exactly
// 3".
func TestForwardPartitionsAlwaysPresent(t *testing.T) {
	fix := openMaintainerFixture(t)
	defer fix.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	for _, parent := range fix.parents {
		n := countForwardChildren(ctx, t, fix.db, parent)
		if n < 3 {
			t.Errorf("parent %q: forward children = %d, want ≥ 3 (p_premake := 3 contract)",
				parent, n)
		}
	}
}

// countForwardChildren returns the number of non-default
// children of `parent` whose lower bound is at or after now().
// "Forward" excludes the current-period child whose lower bound
// is < now() but upper bound is in the future; we want a
// strict-forward count to honour the §8.2 "next 3 months" wording.
func countForwardChildren(
	ctx context.Context, t *testing.T, db *sql.DB, parent string,
) int {
	t.Helper()
	const q = `
		WITH children AS (
			SELECT format('%I.%I', n.nspname, c.relname) AS child_table
			  FROM pg_inherits i
			  JOIN pg_class c ON c.oid = i.inhrelid
			  JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE i.inhparent = to_regclass($1::text)
			   AND pg_get_expr(c.relpartbound, c.oid) <> 'DEFAULT'
		)
		SELECT count(*)::int
		  FROM children
		  CROSS JOIN LATERAL partman.show_partition_info(
		      child_table, NULL, $1::text
		  ) pi
		 WHERE pi.child_start_time >= now()
	`
	var n int
	if err := db.QueryRowContext(ctx, q, parent).Scan(&n); err != nil {
		t.Fatalf("countForwardChildren(%q): %v", parent, err)
	}
	return n
}

// TestScrapeLagZeroAfterMigrations realises the implicit
// healthy-baseline contract: ScrapeLag returns 0 immediately
// after migrations because premake=3 keeps every parent's
// latest_child_end_time well past now()+1 day.
func TestScrapeLagZeroAfterMigrations(t *testing.T) {
	fix := openMaintainerFixture(t)
	defer fix.cleanup()

	svc := newMaintainer(t, fix)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	res, err := svc.ScrapeLag(ctx)
	if err != nil {
		t.Fatalf("ScrapeLag: %v", err)
	}
	if res.MaxLagSeconds != 0 {
		t.Errorf("baseline MaxLagSeconds = %d, want 0 (premake=3 buffer is healthy)",
			res.MaxLagSeconds)
	}
	if got := svc.Metrics().ProvisionLagSeconds(); got != 0 {
		t.Errorf("baseline gauge = %d, want 0", got)
	}
	if got := svc.Metrics().ParentsObserved(); got != uint64(len(fix.parents)) {
		t.Errorf("ParentsObserved = %d, want %d", got, len(fix.parents))
	}
}

// TestScrapeLagDetectsStaleForwardWindow realises the §8.2
// "lag alert fires" scenario in the only way available to a
// real-time test that cannot fast-forward wall clock:
//
// Pick one parent (the weekly trace_observation_log -- weekly
// children are smallest and cheapest to manipulate), detach
// every non-default child whose end_time is in the future, and
// re-scrape. With no forward children left, the lag SQL falls
// back to the CURRENT-period child whose end_time is at most
// (start_of_week + 1 week) -- comfortably less than (now() + 1
// day) only when the current child's end_time is < now() + 1d.
//
// To make the assertion robust without depending on the
// weekday-alignment of `now()`, we go further: detach EVERY
// non-default child. With zero non-default children the lag
// query's CTE is empty, MAX is NULL, GREATEST(0, NULL) is 0...
// which is NOT what we want.
//
// Instead we create a manual "stale" child far in the past via
// partman.create_partition_time, then detach every forward
// child. The lag query then aggregates over a single child
// whose end_time is ~38 days ago, yielding lag ~ 39 days =
// 3 369 600 seconds, well past the 86 400 (1 day) alert threshold.
//
// On success:
//   - the parent's lag exceeds 86400.
//   - the aggregate MaxLagSeconds is at least that lag.
//   - the gauge reflects the same value.
func TestScrapeLagDetectsStaleForwardWindow(t *testing.T) {
	fix := openMaintainerFixture(t)
	defer fix.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	parent := fix.schema + ".trace_observation_log"
	if !contains(fix.parents, parent) {
		t.Skipf("trace_observation_log parent not present in %v", fix.parents)
	}

	// Materialise a single stale forward "anchor" so the
	// lag CTE is not empty after we detach the rest.
	stale := time.Now().Add(-45 * 24 * time.Hour).UTC()
	if _, err := fix.db.ExecContext(ctx, `
		SELECT partman.create_partition_time($1, ARRAY[$2]::timestamptz[])
	`, parent, stale); err != nil {
		t.Fatalf("create stale partition: %v", err)
	}
	// Detach every non-default child whose end_time is at
	// or beyond now() so the parent's "latest" non-default
	// child_end_time becomes the stale anchor.
	detachForwardChildren(ctx, t, fix.db, parent)

	svc := newMaintainer(t, fix)
	res, err := svc.ScrapeLag(ctx)
	if err != nil {
		t.Fatalf("ScrapeLag: %v", err)
	}

	// Find the parent's lag in the breakdown.
	var parentLag int64 = -1
	for _, pl := range res.ParentLags {
		if pl.ParentTable == parent {
			parentLag = pl.LagSeconds
			break
		}
	}
	if parentLag < 0 {
		t.Fatalf("ScrapeLag did not produce a row for %q; ParentLags=%v", parent, res.ParentLags)
	}
	const oneDay = int64(86400)
	if parentLag <= oneDay {
		t.Errorf("parent lag = %d seconds, want > %d (1 day) so the §8.2 alert fires",
			parentLag, oneDay)
	}
	if res.MaxLagSeconds < parentLag {
		t.Errorf("MaxLagSeconds = %d, want ≥ per-parent lag %d", res.MaxLagSeconds, parentLag)
	}
	if got := svc.Metrics().ProvisionLagSeconds(); got != uint64(res.MaxLagSeconds) {
		t.Errorf("gauge = %d, want %d (= MaxLagSeconds)", got, res.MaxLagSeconds)
	}
}

// detachForwardChildren detaches every non-default child of
// `parent` whose end_time is at or beyond now(). The detach
// (not drop) keeps the child relation around for partman to
// re-register on the next run_maintenance tick, mirroring how
// an operator-injected fault would surface in production.
func detachForwardChildren(
	ctx context.Context, t *testing.T, db *sql.DB, parent string,
) {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		WITH children AS (
			SELECT format('%I.%I', n.nspname, c.relname) AS child_table
			  FROM pg_inherits i
			  JOIN pg_class c ON c.oid = i.inhrelid
			  JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE i.inhparent = to_regclass($1::text)
			   AND pg_get_expr(c.relpartbound, c.oid) <> 'DEFAULT'
		)
		SELECT child_table
		  FROM children
		  CROSS JOIN LATERAL partman.show_partition_info(
		      child_table, NULL, $1::text
		  ) pi
		 WHERE pi.child_end_time >= now()
	`, parent)
	if err != nil {
		t.Fatalf("list forward children of %q: %v", parent, err)
	}
	var forward []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			_ = rows.Close()
			t.Fatalf("scan forward child: %v", err)
		}
		forward = append(forward, c)
	}
	_ = rows.Close()
	for _, child := range forward {
		// ALTER TABLE ... DETACH PARTITION requires
		// ownership; the fixture's owner connection has it
		// because the schema was created here.
		stmt := fmt.Sprintf(`ALTER TABLE %s DETACH PARTITION %s`,
			quoteQualified(parent), child)
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("DETACH %s from %s: %v", child, parent, err)
		}
	}
}

// quoteQualified turns "schema.table" into "schema"."table".
// pg_partman stores `parent_table` quoted-ready (per the 0014
// header) but pg_inherits surfaces unquoted names; for symmetry
// we quote both sides here.
func quoteQualified(s string) string {
	i := strings.LastIndex(s, ".")
	if i < 0 {
		return quoteIdent(s)
	}
	return quoteIdent(s[:i]) + "." + quoteIdent(s[i+1:])
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// TestChaosSchedulerDisabledNoWriteFails realises the §8.2
// chaos scenario: the scheduler is disabled for ~1 hour and
// writes into every partitioned parent at NOW() (and at
// NOW()+1h) still succeed, proving the implementation-plan's
// premake-as-buffer invariant.
//
// "Scheduler disabled" is exercised two ways together so the
// proof is not theoretical:
//
//  1. Postgres side: `partman.part_config.automatic_maintenance
//     = 'off'` for every parent in our test scope. This is the
//     canonical pg_partman v5 knob the bgw consults; an
//     automatic_maintenance='off' row is skipped by every
//     `partman.run_maintenance()` call that did not name the
//     parent explicitly. We then issue
//     `SELECT partman.run_maintenance(p_analyze:=false)` (no
//     parent arg -- the same call shape the bgw issues every
//     `pg_partman_bgw.interval` seconds) and assert the
//     per-parent latest child_end_time did NOT advance. This
//     proves the in-cluster scheduler genuinely treated the
//     test parents as paused.
//
//  2. Go side: we never launch `Service.Run`; `MaintenanceRunsTotal`
//     stays 0. The application's own scheduler is also "disabled".
//
// Time travel for the "1 hour" claim: instead of waiting 60
// minutes of wall clock, we issue per-parent INSERTs at both
// `now()` AND `now() + interval '1 hour'`. The premake=3
// buffer must route BOTH to non-default children. The 1-hour
// future write proves any write within an hour after the
// scheduler stopped would also land safely. A short real
// `time.Sleep` (5s, well above the bgw's per-iteration sleep)
// also runs so the bgw has a real chance to tick AND skip our
// parents during the chaos window.
//
// Cleanup restores `automatic_maintenance` to its prior value
// per-parent (capture-then-restore, not blind 'on') so the
// test does not leak state on a shared local cluster.
func TestChaosSchedulerDisabledNoWriteFails(t *testing.T) {
	fix := openMaintainerFixture(t)
	defer fix.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	svc := newMaintainer(t, fix)

	// Pre-flight: confirm the role can mutate part_config; if
	// not, fail loudly rather than silently skipping the bgw-
	// disable step.
	assertPartConfigUpdatable(ctx, t, fix.db)

	// 1. Premake buffer is healthy on every parent.
	for _, parent := range fix.parents {
		if n := countForwardChildren(ctx, t, fix.db, parent); n < 3 {
			t.Fatalf("parent %q: forward children = %d, want ≥3 before chaos window",
				parent, n)
		}
	}

	// 2. The mathematical 1-hour proof: for every parent,
	// `latest_child_end_time - now() > 1 hour`. Then
	// `min(latest_child_end_time across parents) - now()` is
	// the cluster-wide outage budget; assert it is > 1 hour.
	// A clock-skew tolerance of 1 minute is added so a slow
	// CI runner does not racily fail.
	budget := minOutageBudget(ctx, t, fix.db, fix.parents)
	if budget < time.Hour+time.Minute {
		t.Fatalf("min outage budget across parents = %s; want > 1h + 1min slack",
			budget)
	}

	// 3. REAL bgw disable: capture prior automatic_maintenance
	// values, then set 'off' for every test parent. Restore in
	// t.Cleanup with a fresh context (the test ctx may be
	// canceled).
	prior := disableAutomaticMaintenanceForTestParents(ctx, t, fix.db, fix.parents)
	t.Cleanup(func() {
		cuCtx, cuCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cuCancel()
		restoreAutomaticMaintenance(cuCtx, t, fix.db, prior)
	})

	// 4. Snapshot latest_child_end_time per parent BEFORE we
	// fire the bgw-equivalent call so we can prove the call
	// did NOT advance any forward window for our scope.
	before := snapshotLatestChildEndTimes(ctx, t, fix.db, fix.parents)

	// 5. Bgw-equivalent: SELECT partman.run_maintenance(p_analyze:=false)
	// with no parent arg. With automatic_maintenance='off' on every
	// test parent this MUST skip them (it may still process other
	// rows in part_config, but ours are paused).
	if _, err := fix.db.ExecContext(ctx, `SELECT partman.run_maintenance(p_analyze := false)`); err != nil {
		t.Fatalf("simulated bgw run_maintenance(no-arg): %v", err)
	}

	// Real 5-second pause -- comfortably above the bgw's
	// internal sleep granularity so the bgw daemon (if active)
	// can tick and observe automatic_maintenance='off'.
	time.Sleep(5 * time.Second)

	after := snapshotLatestChildEndTimes(ctx, t, fix.db, fix.parents)
	for parent, beforeEnd := range before {
		afterEnd := after[parent]
		if !afterEnd.Equal(beforeEnd) {
			t.Errorf("parent %q: latest_child_end_time advanced from %s to %s during chaos window; bgw disable did not stick",
				parent, beforeEnd, afterEnd)
		}
	}

	// 6. Writes at NOW() AND at NOW()+1h against every
	// partitioned parent succeed and land in a non-default
	// child. The +1h write proves a 1-hour-into-the-outage
	// insert is also safe.
	deps := seedChaosDeps(ctx, t, fix.db)
	for _, parent := range fix.parents {
		landed := insertAtOffsetAndReturnTableoid(ctx, t, fix.db, parent, deps, 0)
		if strings.HasSuffix(landed, "_default") {
			t.Errorf("parent %q: INSERT at NOW() landed in default partition %q; premake buffer was insufficient",
				parent, landed)
		}
		landedFuture := insertAtOffsetAndReturnTableoid(ctx, t, fix.db, parent, deps, time.Hour)
		if strings.HasSuffix(landedFuture, "_default") {
			t.Errorf("parent %q: INSERT at NOW()+1h landed in default partition %q; 1-hour outage would have failed",
				parent, landedFuture)
		}
	}

	// 7. Lag gauge stays at 0 across the chaos window (no
	// maintenance call advanced our parents and the premake
	// buffer is still healthy).
	res, err := svc.ScrapeLag(ctx)
	if err != nil {
		t.Fatalf("ScrapeLag after chaos window: %v", err)
	}
	if res.MaxLagSeconds != 0 {
		t.Errorf("post-chaos MaxLagSeconds = %d, want 0", res.MaxLagSeconds)
	}

	// 8. Confirm the Go-side scheduler never auto-ticked.
	if got := svc.Metrics().MaintenanceRunsTotal(); got != 0 {
		t.Errorf("MaintenanceRunsTotal = %d, want 0 (no Run was launched)", got)
	}
}

// assertPartConfigUpdatable fails the test loudly if the
// current role lacks UPDATE on partman.part_config. Required
// for the chaos test's automatic_maintenance toggle; without it
// the test would silently no-op the bgw-disable step.
func assertPartConfigUpdatable(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	var ok bool
	if err := db.QueryRowContext(ctx,
		`SELECT has_table_privilege(current_user, 'partman.part_config', 'UPDATE')`,
	).Scan(&ok); err != nil {
		t.Fatalf("check part_config UPDATE privilege: %v", err)
	}
	if !ok {
		var who string
		_ = db.QueryRowContext(ctx, `SELECT current_user`).Scan(&who)
		t.Fatalf("role %q lacks UPDATE on partman.part_config; chaos test cannot toggle automatic_maintenance. Grant via: GRANT UPDATE ON partman.part_config TO %s",
			who, who)
	}
}

// minOutageBudget returns the minimum across `parents` of
// (latest non-default child_end_time - now()). The value is the
// "how long the scheduler can be down before writes start
// landing in default" SLO budget. Used by the chaos test's
// 1-hour structural proof.
func minOutageBudget(
	ctx context.Context, t *testing.T, db *sql.DB, parents []string,
) time.Duration {
	t.Helper()
	var minSec int64 = math.MaxInt64
	for _, parent := range parents {
		var sec int64
		if err := db.QueryRowContext(ctx, `
			WITH children AS (
				SELECT format('%I.%I', n.nspname, c.relname) AS child_table
				  FROM pg_inherits i
				  JOIN pg_class c ON c.oid = i.inhrelid
				  JOIN pg_namespace n ON n.oid = c.relnamespace
				 WHERE i.inhparent = to_regclass($1::text)
				   AND pg_get_expr(c.relpartbound, c.oid) <> 'DEFAULT'
			)
			SELECT COALESCE(EXTRACT(EPOCH FROM (
			           max(pi.child_end_time) - now()
			       ))::bigint, 0)
			  FROM children
			  CROSS JOIN LATERAL partman.show_partition_info(
			      child_table, NULL, $1::text
			  ) pi
		`, parent).Scan(&sec); err != nil {
			t.Fatalf("compute outage budget for %q: %v", parent, err)
		}
		if sec < minSec {
			minSec = sec
		}
	}
	if minSec == math.MaxInt64 {
		minSec = 0
	}
	return time.Duration(minSec) * time.Second
}

// disableAutomaticMaintenanceForTestParents sets
// `automatic_maintenance='off'` on every parent in scope and
// returns the prior values (parent_table → prior value) so the
// caller can restore them in t.Cleanup.
//
// Why capture-then-restore rather than blind 'on': a shared
// local cluster may legitimately have a parent set to 'off' for
// some other reason; blindly flipping it back to 'on' would
// leak test state into a sibling test or a developer's manual
// experiment.
func disableAutomaticMaintenanceForTestParents(
	ctx context.Context, t *testing.T, db *sql.DB, parents []string,
) map[string]string {
	t.Helper()
	prior := make(map[string]string, len(parents))
	for _, parent := range parents {
		var was string
		if err := db.QueryRowContext(ctx, `
			SELECT automatic_maintenance::text
			  FROM partman.part_config
			 WHERE parent_table = $1::text
		`, parent).Scan(&was); err != nil {
			t.Fatalf("capture automatic_maintenance for %q: %v", parent, err)
		}
		prior[parent] = was
		if _, err := db.ExecContext(ctx, `
			UPDATE partman.part_config
			   SET automatic_maintenance = 'off'
			 WHERE parent_table = $1::text
		`, parent); err != nil {
			t.Fatalf("disable automatic_maintenance for %q: %v", parent, err)
		}
	}
	return prior
}

// restoreAutomaticMaintenance flips each parent_table's
// automatic_maintenance back to its captured prior value. Best-
// effort: a failure here is logged at Errorf but does NOT panic
// the cleanup -- the test schema is dropped CASCADE shortly
// after and the part_config rows go with it.
func restoreAutomaticMaintenance(
	ctx context.Context, t *testing.T, db *sql.DB, prior map[string]string,
) {
	t.Helper()
	for parent, was := range prior {
		if _, err := db.ExecContext(ctx, `
			UPDATE partman.part_config
			   SET automatic_maintenance = $2::text
			 WHERE parent_table = $1::text
		`, parent, was); err != nil {
			t.Errorf("restore automatic_maintenance for %q to %q: %v", parent, was, err)
		}
	}
}

// snapshotLatestChildEndTimes returns the latest non-default
// child_end_time per parent. The chaos test uses two snapshots
// (before / after the simulated bgw tick) to prove no
// re-provisioning happened for the scoped parents.
func snapshotLatestChildEndTimes(
	ctx context.Context, t *testing.T, db *sql.DB, parents []string,
) map[string]time.Time {
	t.Helper()
	out := make(map[string]time.Time, len(parents))
	for _, parent := range parents {
		var ts sql.NullTime
		if err := db.QueryRowContext(ctx, `
			WITH children AS (
				SELECT format('%I.%I', n.nspname, c.relname) AS child_table
				  FROM pg_inherits i
				  JOIN pg_class c ON c.oid = i.inhrelid
				  JOIN pg_namespace n ON n.oid = c.relnamespace
				 WHERE i.inhparent = to_regclass($1::text)
				   AND pg_get_expr(c.relpartbound, c.oid) <> 'DEFAULT'
			)
			SELECT max(pi.child_end_time)
			  FROM children
			  CROSS JOIN LATERAL partman.show_partition_info(
			      child_table, NULL, $1::text
			  ) pi
		`, parent).Scan(&ts); err != nil {
			t.Fatalf("snapshot latest child_end_time for %q: %v", parent, err)
		}
		if ts.Valid {
			out[parent] = ts.Time
		}
	}
	return out
}

// chaosDeps holds the FK-satisfying parent rows the chaos test
// needs to issue at-NOW writes against every partitioned parent.
// Seeded ONCE per test invocation so the per-parent INSERT loop
// stays focused on routing-correctness.
type chaosDeps struct {
	repoID string
	nodeID string
	edgeID string
}

// seedChaosDeps inserts the minimal repo / node / edge graph
// required for the FK-bearing INSERTs (trace_observation_log
// needs an edge; embedding_publish needs a node). All rows are
// disposed of with the test schema cleanup.
func seedChaosDeps(ctx context.Context, t *testing.T, db *sql.DB) chaosDeps {
	t.Helper()
	var d chaosDeps
	if err := db.QueryRowContext(ctx, `
		INSERT INTO repo (url, default_branch, current_head_sha, language_hints)
		VALUES ($1, 'main', 'deadbeef', ARRAY['go']::text[])
		RETURNING repo_id
	`, "https://example.test/chaos-"+randHex(t, 4)).Scan(&d.repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	makeNode := func() string {
		var id string
		if err := db.QueryRowContext(ctx, `
			INSERT INTO node (repo_id, kind, canonical_signature, fingerprint, from_sha)
			VALUES ($1, 'method', $2, $3::bytea, 'deadbeef')
			RETURNING node_id
		`, d.repoID, "pkg.Chaos#m_"+randHex(t, 4), randBytes(t, 32)).Scan(&id); err != nil {
			t.Fatalf("seed node: %v", err)
		}
		return id
	}
	d.nodeID = makeNode()
	dst := makeNode()
	if err := db.QueryRowContext(ctx, `
		INSERT INTO edge (repo_id, kind, src_node_id, dst_node_id, fingerprint, from_sha)
		VALUES ($1, 'observed_calls', $2, $3, $4::bytea, 'chaos')
		RETURNING edge_id
	`, d.repoID, d.nodeID, dst, randBytes(t, 32)).Scan(&d.edgeID); err != nil {
		t.Fatalf("seed edge: %v", err)
	}
	return d
}

// insertAtOffsetAndReturnTableoid issues a parent-specific
// INSERT whose partition-key column (`started_at` for
// trace_observation_log, `created_at` for the others) is
// `now() + offset`. Returns the tableoid::regclass::text of
// the child that received the row. The chaos test passes
// `offset = 0` and `offset = 1 * time.Hour`; the latter
// proves a 1-hour-into-an-outage write still lands in a
// non-default child given the premake buffer.
//
// The per-parent INSERT shapes are baked into a small switch
// because every partitioned parent has different non-null / FK
// columns; sharing a single shape would require either a
// per-parent metadata table (overkill) or relaxing the parents'
// constraints (out of scope).
func insertAtOffsetAndReturnTableoid(
	ctx context.Context, t *testing.T, db *sql.DB, parent string, deps chaosDeps, offset time.Duration,
) string {
	t.Helper()
	short := parent
	if i := strings.LastIndex(parent, "."); i >= 0 {
		short = parent[i+1:]
	}
	// The duration is rendered as `interval 'N seconds'`
	// (server-evaluated) so the partition-routing decision is
	// made by Postgres against its own clock, not the Go
	// caller's clock. Required for offset>0 inserts to land in
	// the right forward child.
	offsetSec := int64(offset / time.Second)

	var (
		tableoid string
		err      error
	)
	switch short {
	case "episode":
		err = db.QueryRowContext(ctx, fmt.Sprintf(`
			INSERT INTO episode
				(episode_group_id, repo_id, session_id, trace_id, kind,
				 context_id, action, outcome, created_at)
			VALUES (gen_random_uuid(), $1, 'chaos-session', 'chaos-trace',
			        'agent', gen_random_uuid(), '{}'::jsonb, 'success',
			        now() + interval '%d seconds')
			RETURNING tableoid::regclass::text
		`, offsetSec), deps.repoID).Scan(&tableoid)
	case "episode_update":
		err = db.QueryRowContext(ctx, fmt.Sprintf(`
			INSERT INTO episode_update
				(episode_id, new_outcome, actor, created_at)
			VALUES (gen_random_uuid(), 'success', 'system',
			        now() + interval '%d seconds')
			RETURNING tableoid::regclass::text
		`, offsetSec)).Scan(&tableoid)
	case "observation":
		err = db.QueryRowContext(ctx, fmt.Sprintf(`
			INSERT INTO observation
				(episode_id, role, concept_id, weight, created_at)
			VALUES (gen_random_uuid(), 'concept_hit', gen_random_uuid(), 0,
			        now() + interval '%d seconds')
			RETURNING tableoid::regclass::text
		`, offsetSec)).Scan(&tableoid)
	case "recall_context_log":
		err = db.QueryRowContext(ctx, fmt.Sprintf(`
			INSERT INTO recall_context_log
				(repo_id, verb, query_json, reranker_model_version, created_at)
			VALUES ($1, 'recall', '{}'::jsonb, 'chaos-model-v1',
			        now() + interval '%d seconds')
			RETURNING tableoid::regclass::text
		`, offsetSec), deps.repoID).Scan(&tableoid)
	case "trace_observation_log":
		err = db.QueryRowContext(ctx, fmt.Sprintf(`
			INSERT INTO trace_observation_log
				(edge_id, trace_id, span_id, started_at, duration_ms)
			VALUES ($1, 'trace-chaos', 'span-chaos',
			        now() + interval '%d seconds', 1.0)
			RETURNING tableoid::regclass::text
		`, offsetSec), deps.edgeID).Scan(&tableoid)
	case "embedding_publish":
		err = db.QueryRowContext(ctx, fmt.Sprintf(`
			INSERT INTO embedding_publish
				(node_id, embedding_model_version, qdrant_point_id, created_at)
			VALUES ($1, 'chaos-model-v1', gen_random_uuid(),
			        now() + interval '%d seconds')
			RETURNING tableoid::regclass::text
		`, offsetSec), deps.nodeID).Scan(&tableoid)
	case "embedding_publish_event":
		err = db.QueryRowContext(ctx, fmt.Sprintf(`
			INSERT INTO embedding_publish_event
				(publish_id, event_kind, attempt_index, created_at)
			VALUES (gen_random_uuid(), 'queued', 0,
			        now() + interval '%d seconds')
			RETURNING tableoid::regclass::text
		`, offsetSec)).Scan(&tableoid)
	default:
		t.Fatalf("chaos INSERT: unknown partitioned parent %q -- add a per-parent shape", short)
	}
	if err != nil {
		t.Fatalf("INSERT into %q at NOW()+%s: %v", parent, offset, err)
	}
	return tableoid
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(buf)
}

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return buf
}

// TestRunMaintenance_scopedReprovisionsAfterDetach realises
// the operational flow expected by the §8.2 chaos scenario's
// recovery phase: once the scheduler resumes (or our explicit
// RunMaintenance tick lands), the detached forward children are
// re-provisioned and the lag returns to zero.
//
// Pre-state: TestScrapeLagDetectsStaleForwardWindow detached
// every forward child of trace_observation_log. Here we
// reproduce that detached state, run RunMaintenance once, and
// assert the lag returns to 0.
func TestRunMaintenance_scopedReprovisionsAfterDetach(t *testing.T) {
	fix := openMaintainerFixture(t)
	defer fix.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	parent := fix.schema + ".trace_observation_log"
	if !contains(fix.parents, parent) {
		t.Skipf("trace_observation_log parent not present in %v", fix.parents)
	}

	// Detach every forward child to simulate a long scheduler
	// outage.
	detachForwardChildren(ctx, t, fix.db, parent)

	svc := newMaintainer(t, fix)

	// Run maintenance. partman.run_maintenance MUST re-provision
	// the premake=3 forward window for this parent.
	if _, err := svc.RunMaintenance(ctx); err != nil {
		t.Fatalf("RunMaintenance: %v", err)
	}
	// Lag must now be 0 -- the premake buffer is restored.
	res, err := svc.ScrapeLag(ctx)
	if err != nil {
		t.Fatalf("ScrapeLag: %v", err)
	}
	if res.MaxLagSeconds != 0 {
		t.Errorf("post-recovery MaxLagSeconds = %d, want 0 (run_maintenance restored premake buffer)",
			res.MaxLagSeconds)
	}
	// And the forward-children count must again be >=3.
	if n := countForwardChildren(ctx, t, fix.db, parent); n < 3 {
		t.Errorf("post-recovery forward children = %d, want ≥3", n)
	}
}
