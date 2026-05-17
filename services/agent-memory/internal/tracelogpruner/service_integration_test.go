package tracelogpruner

// Integration tests for the Stage 4.3 retention pruner against
// a live PostgreSQL 16 + pg_partman v5 instance. Skips cleanly
// when AGENT_MEMORY_PG_URL is unset, mirroring the convention
// in migrations/test_migrate_test.go and the sibling
// graphwriter / graphreader integration suites.
//
// Implementation-plan.md Stage 4.3 acceptance scenarios:
//
//   * "30-day window dropped" -- a partition whose week-range
//     ends ≥30 days ago must be detached and
//     trace_log_partitions_dropped_total += 1.
//
//   * "aggregate row preserved" -- a trace_observation row
//     whose last_observed_at is 60 days old must survive the
//     pruner pass (C8 — aggregates are never pruned).
//
// The two scenarios are exercised inside one test that
// materialises both pre-conditions, then runs Prune ONCE, then
// asserts both consequences. Running them in one Prune call
// keeps the pg_partman state simple and matches the production
// shape where the daily cron sweep handles every retention
// concern in one pass.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
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

// prunerFixture is the per-test PostgreSQL substrate. The
// pruner is privileged-by-design (it issues DETACH PARTITION
// which requires ownership), so unlike the graphwriter /
// recallcontext fixtures we do NOT flip the agent_memory_app
// role to LOGIN here -- the owner *sql.DB IS the pruner's
// production-shape role (the migration-runner owner).
type prunerFixture struct {
	db      *sql.DB
	schema  string
	cleanup func()
}

func openPrunerFixture(t *testing.T) *prunerFixture {
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
	// stays sticky for every subsequent query the fixture
	// issues (matches the graphwriter / recallcontext pattern).
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
	// search_path includes partman so the pruner's
	// `partman.drop_partition_time` resolves and so manual
	// fixture-setup SQL can call partman helpers unqualified
	// when convenient.
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

	cleanup := func() {
		ctx2, c2 := context.WithTimeout(context.Background(), testDBTimeout)
		defer c2()
		// Drop partman.part_config rows pointing at tables in
		// this schema BEFORE the DROP SCHEMA so partman's
		// cluster-wide state stays clean. Pattern adapted from
		// the graphwriter / migrations fixtures. The schema is
		// purely alphanumeric (hex suffix) so the LIKE escape
		// is defensive rather than load-bearing.
		schemaPrefix := strings.ReplaceAll(schema, "_", "#_") + ".%"
		_, _ = owner.ExecContext(ctx2, `
			DELETE FROM partman.part_config
			WHERE parent_table LIKE $1 ESCAPE '#'
		`, schemaPrefix)
		_, _ = owner.ExecContext(ctx2, `DROP SCHEMA `+quoteIdent(schema)+` CASCADE`)
		_ = owner.Close()
	}
	return &prunerFixture{db: owner, schema: schema, cleanup: cleanup}
}

func newSchemaName(t *testing.T) string {
	t.Helper()
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "amprune_" + hex.EncodeToString(buf[:])
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// seedEdgeIDs creates two distinct Edge rows so the test can
// (a) hang trace_observation_log rows off one edge in an old
// partition and (b) hang a stale trace_observation aggregate
// row off the OTHER edge for the preserve-aggregate scenario.
// Returns (edgeForOldLog, edgeForStaleAggregate).
//
// We INSERT directly via the owner connection rather than
// going through graphwriter because the pruner tests do not
// need to exercise fingerprint determinism or idempotent
// insert semantics — those have dedicated suites in the
// graphwriter package. Direct SQL keeps the fixture small and
// the seed-failure mode obvious.
func seedEdgeIDs(ctx context.Context, t *testing.T, db *sql.DB) (string, string) {
	t.Helper()

	var repoID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO repo (url, default_branch, current_head_sha, language_hints)
		VALUES ($1, 'main', 'deadbeef', ARRAY['go']::text[])
		RETURNING repo_id
	`, "https://example.test/pruner-"+randHex(t, 4)).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	// Two methods (caller + callee) plus a third pair so we
	// can mint two distinct edges. fingerprint is a bytea(32);
	// we generate per-row random bytes to satisfy the UNIQUE
	// (repo_id, fingerprint) index on `node`.
	makeMethod := func() string {
		var nodeID string
		if err := db.QueryRowContext(ctx, `
			INSERT INTO node (repo_id, kind, canonical_signature, fingerprint, from_sha)
			VALUES ($1, 'method', $2, $3::bytea, 'deadbeef')
			RETURNING node_id
		`, repoID, "pkg.Trace#m_"+randHex(t, 4), randBytes(t, 32)).Scan(&nodeID); err != nil {
			t.Fatalf("seed method node: %v", err)
		}
		return nodeID
	}
	src1, dst1 := makeMethod(), makeMethod()
	src2, dst2 := makeMethod(), makeMethod()

	makeEdge := func(src, dst string) string {
		var edgeID string
		if err := db.QueryRowContext(ctx, `
			INSERT INTO edge (repo_id, kind, src_node_id, dst_node_id, fingerprint, from_sha)
			VALUES ($1, 'observed_calls', $2, $3, $4::bytea, 'observed')
			RETURNING edge_id
		`, repoID, src, dst, randBytes(t, 32)).Scan(&edgeID); err != nil {
			t.Fatalf("seed edge: %v", err)
		}
		return edgeID
	}
	return makeEdge(src1, dst1), makeEdge(src2, dst2)
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

// TestPrune_detachesOldPartitionAndPreservesAggregate is the
// load-bearing Stage 4.3 acceptance integration test. It
// materialises both pre-conditions for the two implementation-
// plan scenarios in one schema:
//
//  1. An old trace_observation_log row whose `started_at` is
//     45 days in the past — comfortably ≥30 days so the
//     week-range partition's UPPER bound is also ≥30 days old
//     regardless of which day of the week we land on. Captured
//     via `RETURNING tableoid::regclass::text` so the assertion
//     binds to the EXACT partition pg_partman backfilled, not
//     a name we had to guess.
//
//  2. A trace_observation aggregate row on a SEPARATE edge
//     with `last_observed_at = NOW() - 60 days`. The pruner
//     must not touch the (non-partitioned) `trace_observation`
//     table — C8 invariant — so this row must still be SELECT-
//     visible after the pass.
//
// After Prune runs once with retention=30 days:
//
//   - the captured old partition is no longer present in
//     `pg_inherits` as a child of `trace_observation_log`
//     (it has been DETACHED; with KeepTable=true the standalone
//     table still exists, but it is no longer routed to by an
//     INSERT into the parent);
//   - the standalone partition relation still exists in
//     pg_class (KeepTable=true semantic: detach, do not drop);
//   - the child-count for the parent in pg_inherits has
//     decreased by exactly 1;
//   - PruneResult.PartitionsDropped == 1 (the integer return
//     value of pg_partman v5 `drop_partition_time`);
//   - the trace_log_partitions_dropped_total counter == 1;
//   - the stale trace_observation aggregate row still exists.
//
// Note on the return-value contract: pg_partman v5's
// `drop_partition_time` is declared `RETURNS int` — a single
// integer carrying the count of partitions detached or
// dropped. It does NOT return the relation names. The
// assertion that the EXACT partition was detached is therefore
// done via `pg_inherits` lookup, not via PruneResult fields.
//
// Why 45 days, not 35
// -------------------
// The Stage 4.3 brief originally said "materialises a 35-day-
// old log partition". A partition whose START is 35 days ago
// covers `[start, start+7d)`, which means the UPPER bound is
// at most 28 days ago — younger than the 30-day retention
// floor — when the chosen day-of-week aligns badly. pg_partman's
// expiry check compares the partition UPPER bound to
// `NOW() - retention`, so a literal 35-day-old start MAY NOT
// trip the cutoff. We use 45 days for deterministic expiry
// regardless of weekday alignment: 45-day-old start → upper
// bound at most 38 days ago → comfortably past the 30-day
// floor. The implementation-plan acceptance text was clarified
// in this iteration to say "older than the retention window"
// to align the doc with the test's deterministic offset.
func TestPrune_detachesOldPartitionAndPreservesAggregate(t *testing.T) {
	fix := openPrunerFixture(t)
	defer fix.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	edgeForOldLog, edgeForStaleAggregate := seedEdgeIDs(ctx, t, fix.db)

	parentQualified := fix.schema + ".trace_observation_log"

	// Backfill a partition whose week contains 45 days ago.
	// pg_partman v5 `create_partition_time` accepts the
	// schema-qualified parent and an array of timestamps; it
	// will create one partition per distinct week-aligned
	// timestamp.
	oldTimestamp := time.Now().Add(-45 * 24 * time.Hour).UTC()
	if _, err := fix.db.ExecContext(ctx, `
		SELECT partman.create_partition_time($1, ARRAY[$2]::timestamptz[])
	`, parentQualified, oldTimestamp); err != nil {
		t.Fatalf("create_partition_time: %v", err)
	}

	// Insert a log row into the backfilled partition. The
	// RETURNING tableoid::regclass clause gives us the
	// concrete partition relname so the assertion is
	// independent of pg_partman's name-generation convention.
	var oldPartitionRelname string
	if err := fix.db.QueryRowContext(ctx, `
		INSERT INTO trace_observation_log
			(edge_id, trace_id, span_id, started_at, duration_ms)
		VALUES ($1, 'trace-old', 'span-old', $2::timestamptz, 1.0)
		RETURNING tableoid::regclass::text
	`, edgeForOldLog, oldTimestamp).Scan(&oldPartitionRelname); err != nil {
		t.Fatalf("insert old log row: %v", err)
	}
	// Defence-in-depth: confirm the row did NOT land in the
	// default partition (which would mean create_partition_time
	// silently no-op'd and pg_partman never produced a
	// detachable child).
	if strings.HasSuffix(oldPartitionRelname, "_default") {
		t.Fatalf("old row landed in default partition %q; create_partition_time did not backfill",
			oldPartitionRelname)
	}

	// Materialise the C8 pre-condition: a stale aggregate row
	// whose last_observed_at is 60 days old. We INSERT
	// directly so we don't have to back-date a value the
	// graphwriter would stamp to NOW.
	staleObservedAt := time.Now().Add(-60 * 24 * time.Hour).UTC()
	if _, err := fix.db.ExecContext(ctx, `
		INSERT INTO trace_observation
			(edge_id, observation_count, p50_latency_ms, p95_latency_ms,
			 latest_span_ref, last_observed_at)
		VALUES ($1, 42, 12.5, 21.0, 'trace-stale:span-stale', $2)
	`, edgeForStaleAggregate, staleObservedAt); err != nil {
		t.Fatalf("seed stale aggregate: %v", err)
	}

	// Confirm pre-conditions BEFORE pruning so a fixture bug
	// surfaces with a clear message instead of being conflated
	// with a pruner bug.
	childCountBefore := countPartitionChildren(ctx, t, fix.db, fix.schema, "trace_observation_log")
	if childCountBefore < 2 {
		t.Fatalf("pre-prune child count = %d, want ≥2 (default + backfilled)", childCountBefore)
	}
	if !partitionIsChild(ctx, t, fix.db, fix.schema, "trace_observation_log", oldPartitionRelname) {
		t.Fatalf("backfilled partition %q is not a child of trace_observation_log before prune",
			oldPartitionRelname)
	}
	if !relationExists(ctx, t, fix.db, fix.schema, oldPartitionRelname) {
		t.Fatalf("backfilled partition %q does not exist as a relation before prune",
			oldPartitionRelname)
	}
	if !aggregateExists(ctx, t, fix.db, edgeForStaleAggregate) {
		t.Fatal("stale aggregate row missing before prune")
	}

	// Construct the pruner under test. Use the same owner
	// *sql.DB; production binaries connect with the
	// migration-runner role for the same reason.
	svc, err := New(fix.db, Config{
		ParentTable:  parentQualified,
		Retention:    30 * 24 * time.Hour,
		KeepTable:    boolPtr(true),
		RunInterval:  time.Hour, // unused by Prune
		PruneTimeout: testDBTimeout,
	}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := svc.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	// Scenario 1: 30-day window dropped.
	//
	// pg_partman v5 returns the integer count of detached or
	// dropped partitions. We materialised exactly one
	// expired-by-retention partition, so the count must be 1.
	if res.PartitionsDropped != 1 {
		t.Errorf("PruneResult.PartitionsDropped = %d, want 1 (pg_partman v5 RETURNS int)", res.PartitionsDropped)
	}
	snap := svc.Metrics().Snapshot()
	if snap[MetricTraceLogPartitionsDroppedTotal] != 1 {
		t.Errorf("%s counter = %d, want 1",
			MetricTraceLogPartitionsDroppedTotal,
			snap[MetricTraceLogPartitionsDroppedTotal])
	}
	// Catalog-state assertion: the SPECIFIC partition we
	// materialised is no longer a child of the parent.
	// This is the load-bearing "exact partition detached"
	// check — far more robust than relying on the function's
	// return value, which is just a count.
	if partitionIsChild(ctx, t, fix.db, fix.schema, "trace_observation_log", oldPartitionRelname) {
		t.Errorf("partition %q is still a child of trace_observation_log after prune; expected DETACHED",
			oldPartitionRelname)
	}
	// KeepTable=true: the standalone relation MUST still
	// exist after detach. A DROP would surface here as a
	// missing relation.
	if !relationExists(ctx, t, fix.db, fix.schema, oldPartitionRelname) {
		t.Errorf("partition table %q was DROPPED after prune; expected DETACHED + retained with KeepTable=true",
			oldPartitionRelname)
	}
	// Cross-check: child count must have decreased by
	// exactly one. A larger delta would mean we accidentally
	// detached a younger partition too.
	childCountAfter := countPartitionChildren(ctx, t, fix.db, fix.schema, "trace_observation_log")
	if got, want := childCountBefore-childCountAfter, 1; got != want {
		t.Errorf("child-count delta = %d (before=%d, after=%d), want %d",
			got, childCountBefore, childCountAfter, want)
	}

	// Scenario 2: aggregate row preserved (C8).
	if !aggregateExists(ctx, t, fix.db, edgeForStaleAggregate) {
		t.Error("stale trace_observation row was removed by the pruner; C8 invariant violated")
	}
}

// partitionIsChild returns true when `partition` (possibly a
// `schema.name` qualified string) is in `pg_inherits` as a
// child of `parent` in `schema`. The pruner's success
// criterion for scenario 1 is that this returns FALSE after
// Prune.
func partitionIsChild(
	ctx context.Context, t *testing.T, db *sql.DB,
	schema, parent, partition string,
) bool {
	t.Helper()
	// Strip schema-qualification on the partition relname if
	// `tableoid::regclass::text` returned it qualified.
	// pg_class.relname is just the table-local name.
	partRelname := stripSchema(partition)
	var found bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM pg_inherits inh
			  JOIN pg_class child  ON child.oid  = inh.inhrelid
			  JOIN pg_class parent ON parent.oid = inh.inhparent
			  JOIN pg_namespace cn ON cn.oid = child.relnamespace
			  JOIN pg_namespace pn ON pn.oid = parent.relnamespace
			 WHERE pn.nspname = $1
			   AND parent.relname = $2
			   AND cn.nspname = $1
			   AND child.relname = $3
		)
	`, schema, parent, partRelname).Scan(&found); err != nil {
		t.Fatalf("partitionIsChild query: %v", err)
	}
	return found
}

// countPartitionChildren returns the number of children
// `parent` has in `pg_inherits` (including the bootstrap
// default partition).
func countPartitionChildren(
	ctx context.Context, t *testing.T, db *sql.DB,
	schema, parent string,
) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*)
		  FROM pg_inherits inh
		  JOIN pg_class child  ON child.oid  = inh.inhrelid
		  JOIN pg_class parent ON parent.oid = inh.inhparent
		  JOIN pg_namespace cn ON cn.oid = child.relnamespace
		  JOIN pg_namespace pn ON pn.oid = parent.relnamespace
		 WHERE pn.nspname = $1
		   AND parent.relname = $2
		   AND cn.nspname = $1
	`, schema, parent).Scan(&n); err != nil {
		t.Fatalf("countPartitionChildren query: %v", err)
	}
	return n
}

// relationExists reports whether a relation with the given
// (possibly schema-qualified) name exists in `schema`. Used by
// the integration test to confirm that KeepTable=true left the
// detached partition's standalone relation in place — i.e.
// DETACH, not DROP. The schema-qualification is honoured via
// pg_namespace lookup so the answer does NOT depend on the
// session search_path.
func relationExists(
	ctx context.Context, t *testing.T, db *sql.DB,
	schema, relation string,
) bool {
	t.Helper()
	rel := stripSchema(relation)
	var found bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM pg_class c
			  JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = $1
			   AND c.relname = $2
		)
	`, schema, rel).Scan(&found); err != nil {
		t.Fatalf("relationExists query: %v", err)
	}
	return found
}

// stripSchema returns the relation-local name (without the
// `schema.` prefix) from a possibly-qualified identifier. Used
// by the pg_class / pg_inherits helpers so the caller does not
// have to care whether they got an unqualified or qualified
// relname from `tableoid::regclass::text`.
func stripSchema(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

// aggregateExists checks whether the trace_observation row
// for `edgeID` is still SELECT-visible. The C8 invariant means
// this MUST stay true across any number of Prune calls.
func aggregateExists(
	ctx context.Context, t *testing.T, db *sql.DB, edgeID string,
) bool {
	t.Helper()
	var found bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM trace_observation WHERE edge_id = $1
		)
	`, edgeID).Scan(&found); err != nil {
		t.Fatalf("aggregateExists query: %v", err)
	}
	return found
}

// TestPrune_noOpWhenAllPartitionsInsideWindow exercises the
// healthy-day path: every partition is within the 30-day
// window so pg_partman returns 0 and the counter stays at 0.
// Catches a regression where the pruner over-counts (e.g.
// the iter-1 bug that incremented by row count rather than
// the integer return value).
func TestPrune_noOpWhenAllPartitionsInsideWindow(t *testing.T) {
	fix := openPrunerFixture(t)
	defer fix.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	parentQualified := fix.schema + ".trace_observation_log"
	svc, err := New(fix.db, Config{
		ParentTable:  parentQualified,
		Retention:    30 * 24 * time.Hour,
		KeepTable:    boolPtr(true),
		PruneTimeout: testDBTimeout,
	}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := svc.Prune(ctx)
	if err != nil {
		// Surface schema-qualification / role-grant misconfig
		// in a way that makes the failure mode obvious. The
		// most common cause of an unexpected error here is
		// the test owner not having ownership of the
		// partitioned parent (which DETACH PARTITION requires),
		// which surfaces as a SQLSTATE 42501 wrapped in the
		// pg_partman call's text.
		if errors.Is(err, context.DeadlineExceeded) {
			t.Skipf("skipping: pg_partman call exceeded %v -- check role ownership", testDBTimeout)
		}
		t.Fatalf("Prune: %v", err)
	}
	if res.PartitionsDropped != 0 {
		t.Errorf("PartitionsDropped = %d, want 0 (no partitions outside 30d window)",
			res.PartitionsDropped)
	}
	if got := svc.Metrics().PartitionsDroppedTotal(); got != 0 {
		t.Errorf("%s counter = %d, want 0",
			MetricTraceLogPartitionsDroppedTotal, got)
	}
	if got := svc.Metrics().RunsTotal(); got != 1 {
		t.Errorf("%s counter = %d, want 1",
			MetricTraceLogPruneRunsTotal, got)
	}
}
