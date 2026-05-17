package consolidator

// Integration tests for the Stage 6.1 Consolidator worker
// against a live PostgreSQL 16 + pg_partman v5 instance. Skips
// cleanly when AGENT_MEMORY_PG_URL is unset, mirroring the
// convention in migrations/test_migrate_test.go and the sibling
// graphwriter / tracelogpruner integration suites.
//
// Implementation-plan.md Stage 6.1 acceptance scenarios:
//
//   * "threshold creates new Concept"  -- TestTick_thresholdCreatesNewConcept
//   * "subsequent run only adds version" -- TestTick_subsequentRunAppendsVersion
//   * "support spans repos"            -- TestTick_supportSpansRepos
//
// Plus robustness coverage:
//
//   * Idempotent re-tick                  -- TestTick_idempotentReTick
//   * Sub-threshold is a no-op            -- TestTick_subThresholdIsNoOp
//   * Wake-after-N fires early tick       -- TestRun_wakeAfterNEpisodes
//
// Every scenario seeds its own per-test schema (CREATE SCHEMA
// + SET search_path) and runs the entire migration chain so the
// Consolidator exercises the production schema shape, including
// the partitioned episode/observation tables and pg_partman
// rolling-window setup.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/migrations"
)

const (
	intEnvPGURL      = "AGENT_MEMORY_PG_URL"
	intTestDBTimeout = 60 * time.Second
)

// consolFixture is the per-test PostgreSQL substrate. db is the
// owner connection that created the schema and ran migrations.
// We deliberately do NOT flip agent_memory_app LOGIN here -- the
// Consolidator service is migration-owner-equivalent at app
// level (it issues INSERT+SELECT+UPDATE on the same tables the
// migration owns), and the role-grant story is already covered
// by migrations/test_stage14_role_grants_test.go.
type consolFixture struct {
	db      *sql.DB
	schema  string
	cleanup func()
}

func openConsolFixture(t *testing.T) *consolFixture {
	t.Helper()
	base := os.Getenv(intEnvPGURL)
	if base == "" {
		t.Skipf("skipping: %s is unset; integration tests require a live PostgreSQL", intEnvPGURL)
	}

	owner, err := sql.Open("postgres", base)
	if err != nil {
		t.Fatalf("sql.Open owner: %v", err)
	}
	// IMPORTANT: keep at least 2 connections so the Consolidator's
	// emission phase (pins one conn for the advisory lock + scan)
	// can coexist with the finalize UPDATE (borrows a SECOND
	// conn via the pool). The iter-2 fix releases the pinned
	// conn BEFORE finalize, so MaxOpenConns=1 would ALSO work,
	// but we keep =2 here to give the harness a deadlock-detection
	// signal: if a regression reintroduces the deadlock, the
	// SET search_path pin breaks and the test fails immediately
	// instead of hanging.
	owner.SetMaxOpenConns(2)
	owner.SetMaxIdleConns(2)

	ctx, cancel := context.WithTimeout(context.Background(), intTestDBTimeout)
	defer cancel()
	if err := owner.PingContext(ctx); err != nil {
		_ = owner.Close()
		t.Skipf("skipping: cannot reach PostgreSQL at %s: %v", intEnvPGURL, err)
	}
	schema := newConsolSchemaName(t)
	if _, err := owner.ExecContext(ctx, `CREATE SCHEMA `+quoteConsolIdent(schema)); err != nil {
		_ = owner.Close()
		t.Fatalf("create schema %q: %v", schema, err)
	}
	// search_path includes partman so pg_partman helpers and
	// any LEFT JOIN to recall_context_log / episode partitions
	// resolve unqualified. Re-applied per connection so the
	// 2-conn pool sees the same search_path.
	for i := 0; i < 2; i++ {
		conn, cerr := owner.Conn(ctx)
		if cerr != nil {
			_ = owner.Close()
			t.Fatalf("pin conn for SET search_path: %v", cerr)
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf(
			`SET search_path TO %s, public, partman`, quoteConsolIdent(schema),
		)); err != nil {
			_ = conn.Close()
			_ = owner.Close()
			t.Fatalf("set search_path: %v", err)
		}
		_ = conn.Close()
	}
	if err := migrations.New(owner).Up(ctx); err != nil {
		_ = owner.Close()
		t.Fatalf("migrations.Up: %v", err)
	}

	cleanup := func() {
		ctx2, c2 := context.WithTimeout(context.Background(), intTestDBTimeout)
		defer c2()
		schemaPrefix := strings.ReplaceAll(schema, "_", "#_") + ".%"
		_, _ = owner.ExecContext(ctx2, `
			DELETE FROM partman.part_config
			WHERE parent_table LIKE $1 ESCAPE '#'
		`, schemaPrefix)
		_, _ = owner.ExecContext(ctx2, `DROP SCHEMA `+quoteConsolIdent(schema)+` CASCADE`)
		_ = owner.Close()
	}
	return &consolFixture{db: owner, schema: schema, cleanup: cleanup}
}

func newConsolSchemaName(t *testing.T) string {
	t.Helper()
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "amconsol_" + hex.EncodeToString(buf[:])
}

func quoteConsolIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// uniqueLockKey returns a per-test advisory-lock key. Using a
// shared CONSOLID literal across parallel test runs would
// serialise every test on the global lock; the per-test key
// keeps the suite parallel-safe. The constant 0x4000_0000_0000_0000
// upper bit ensures these keys cannot collide with
// ConsolidatorAdvisoryLockKey (0x434F4E534F4C4944) or the
// testpglock keys, which both live below that ceiling.
var consolLockCounter atomic.Int64

func uniqueLockKey() int64 {
	return 0x4000000000000000 | consolLockCounter.Add(1)
}

// ────────────────────────────────────────────────────────────
// Seed helpers
// ────────────────────────────────────────────────────────────

func seedRepo(ctx context.Context, t *testing.T, db *sql.DB, slug string) string {
	t.Helper()
	var repoID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO repo (url, default_branch, current_head_sha, language_hints)
		VALUES ($1, 'main', 'deadbeef', ARRAY['go']::text[])
		RETURNING repo_id::text
	`, "https://example.test/"+slug+"-"+randomHex(t, 4)).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	return repoID
}

// seedNode inserts a node row with a CALLER-SUPPLIED fingerprint.
// Two repos that share a logical Node call seedNode with the
// same fingerprint bytes; the UNIQUE INDEX is on
// (repo_id, fingerprint) so the per-repo insert succeeds while
// the fingerprint collides across repos -- which is the G6
// cross-repo invariant under test.
func seedNode(ctx context.Context, t *testing.T, db *sql.DB, repoID string, fp []byte, label string) string {
	t.Helper()
	var nodeID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO node (repo_id, kind, canonical_signature, fingerprint, from_sha)
		VALUES ($1::uuid, 'method', $2, $3::bytea, 'deadbeef')
		RETURNING node_id::text
	`, repoID, label+"-"+randomHex(t, 4), fp).Scan(&nodeID); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	return nodeID
}

// seedRecallContext inserts a recall_context_log row so an
// Episode that needs a non-NULL context_id (everything except
// kind='feedback') can reference it.
func seedRecallContext(ctx context.Context, t *testing.T, db *sql.DB, repoID string) string {
	t.Helper()
	var ctxID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO recall_context_log
		    (repo_id, verb, query_json, reranker_model_version)
		VALUES ($1::uuid, 'recall'::verb, '{}'::jsonb, 'v-test')
		RETURNING context_id::text
	`, repoID).Scan(&ctxID); err != nil {
		t.Fatalf("seed recall_context_log: %v", err)
	}
	return ctxID
}

// seedEpisode inserts a kind='agent', outcome='success'
// Episode -- which the polarity() function maps to "positive"
// -- plus one node_hit Observation pointing at nodeID. Returns
// the episode_id. Callers wanting cross-repo collision should
// pass nodes with the SAME fingerprint across repos.
func seedEpisode(
	ctx context.Context, t *testing.T, db *sql.DB,
	repoID, contextID, nodeID string,
) string {
	t.Helper()
	var epID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO episode
		    (episode_group_id, repo_id, session_id, trace_id, kind,
		     context_id, action, outcome)
		VALUES (gen_random_uuid(), $1::uuid, $2, $3, 'agent'::episode_kind,
		        $4::uuid, '{"op":"test"}'::jsonb, 'success'::outcome)
		RETURNING episode_id::text
	`, repoID, "sess-"+randomHex(t, 4), "trace-"+randomHex(t, 4), contextID).Scan(&epID); err != nil {
		t.Fatalf("seed episode: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO observation
		    (episode_id, role, node_id)
		VALUES ($1::uuid, 'node_hit'::observation_role, $2::uuid)
	`, epID, nodeID); err != nil {
		t.Fatalf("seed observation: %v", err)
	}
	return epID
}

// seedNegativeEpisode is like seedEpisode but with
// outcome='failure' -> polarity "negative". Used by the
// idempotency test.
func seedNegativeEpisode(
	ctx context.Context, t *testing.T, db *sql.DB,
	repoID, contextID, nodeID string,
) string {
	t.Helper()
	var epID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO episode
		    (episode_group_id, repo_id, session_id, trace_id, kind,
		     context_id, action, outcome)
		VALUES (gen_random_uuid(), $1::uuid, $2, $3, 'agent'::episode_kind,
		        $4::uuid, '{"op":"test"}'::jsonb, 'failure'::outcome)
		RETURNING episode_id::text
	`, repoID, "sess-"+randomHex(t, 4), "trace-"+randomHex(t, 4), contextID).Scan(&epID); err != nil {
		t.Fatalf("seed negative episode: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO observation
		    (episode_id, role, node_id)
		VALUES ($1::uuid, 'node_hit'::observation_role, $2::uuid)
	`, epID, nodeID); err != nil {
		t.Fatalf("seed observation: %v", err)
	}
	return epID
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(buf)
}

func randomFingerprint(t *testing.T) []byte {
	t.Helper()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return buf
}

// deterministicFingerprint hashes a string seed so two callers
// passing the same seed get bit-identical bytes -- this is how
// the cross-repo scenario engineers a SHARED fingerprint while
// keeping the per-repo node_id distinct.
func deterministicFingerprint(seed string) []byte {
	sum := sha256.Sum256([]byte(seed))
	return sum[:]
}

func newConsolService(t *testing.T, db *sql.DB, threshold int) *Service {
	t.Helper()
	svc, err := New(db, Config{
		Threshold:       threshold,
		RunInterval:     time.Second,
		TickTimeout:     intTestDBTimeout,
		AdvisoryLockKey: uniqueLockKey(),
	}, silentLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

// ────────────────────────────────────────────────────────────
// Assertion helpers
// ────────────────────────────────────────────────────────────

func mustCountConcepts(ctx context.Context, t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM concept`).Scan(&n); err != nil {
		t.Fatalf("count concepts: %v", err)
	}
	return n
}

func mustReadLatestConceptVersion(ctx context.Context, t *testing.T, db *sql.DB, conceptID string) (versionIndex, supportCount, negativeCount int) {
	t.Helper()
	if err := db.QueryRowContext(ctx, `
		SELECT version_index, support_count, negative_count
		  FROM concept_version
		 WHERE concept_id = $1::uuid
		 ORDER BY version_index DESC
		 LIMIT 1
	`, conceptID).Scan(&versionIndex, &supportCount, &negativeCount); err != nil {
		t.Fatalf("read latest concept_version: %v", err)
	}
	return
}

func mustCountVersions(ctx context.Context, t *testing.T, db *sql.DB, conceptID string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM concept_version WHERE concept_id = $1::uuid`, conceptID,
	).Scan(&n); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	return n
}

func mustReadSoleConceptID(ctx context.Context, t *testing.T, db *sql.DB) string {
	t.Helper()
	var id string
	if err := db.QueryRowContext(ctx,
		`SELECT concept_id::text FROM concept LIMIT 1`,
	).Scan(&id); err != nil {
		t.Fatalf("read concept_id: %v", err)
	}
	return id
}

// assertEveryNodeHitSupportHasNodeID is the per-(Episode, Node)
// emission contract from implementation-plan.md §6.1 line 895
// "Attach ConceptSupport rows per contributing Node/Episode/repo".
// Every concept_support row whose contributing Episode had at
// least one node_hit Observation MUST have a non-NULL node_id.
// Returns the count of (NULL node_id) rows so the caller can
// assert it is zero.
func mustCountNullNodeIDSupportForNodeHitEpisodes(ctx context.Context, t *testing.T, db *sql.DB, conceptID string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*)
		  FROM concept_support cs
		 WHERE cs.concept_id = $1::uuid
		   AND cs.node_id IS NULL
		   AND EXISTS (
		     SELECT 1 FROM observation o
		      WHERE o.episode_id = cs.episode_id
		        AND o.role = 'node_hit'
		   )
	`, conceptID).Scan(&n); err != nil {
		t.Fatalf("count null-node support for node-hit episodes: %v", err)
	}
	return n
}

// mustCountConsolidatorRuns returns the count of consolidator_run
// rows in a given status (or every row when status is empty).
func mustCountConsolidatorRuns(ctx context.Context, t *testing.T, db *sql.DB, status string) int {
	t.Helper()
	var (
		n   int
		err error
	)
	if status == "" {
		err = db.QueryRowContext(ctx, `SELECT count(*) FROM consolidator_run`).Scan(&n)
	} else {
		err = db.QueryRowContext(ctx,
			`SELECT count(*) FROM consolidator_run WHERE status = $1`, status,
		).Scan(&n)
	}
	if err != nil {
		t.Fatalf("count consolidator_run: %v", err)
	}
	return n
}

// ────────────────────────────────────────────────────────────
// Scenario A: threshold creates new Concept.
//
// Implementation-plan §6.1 line 893: "For each group crossing
// the threshold, append a Concept (...) and ConceptVersion".
// Scenario brief: "Given 10 positive Episodes referencing the
// same Node, when the Consolidator runs, then 1 Concept +
// ConceptVersion(support_count=10) are created."
// ────────────────────────────────────────────────────────────

func TestTick_thresholdCreatesNewConcept(t *testing.T) {
	fix := openConsolFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), intTestDBTimeout)
	defer cancel()

	repoID := seedRepo(ctx, t, fix.db, "scenA")
	contextID := seedRecallContext(ctx, t, fix.db, repoID)
	nodeID := seedNode(ctx, t, fix.db, repoID, randomFingerprint(t), "node-A")

	for i := 0; i < 10; i++ {
		_ = seedEpisode(ctx, t, fix.db, repoID, contextID, nodeID)
	}

	svc := newConsolService(t, fix.db, 10)
	res, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.LockSkipped {
		t.Fatalf("did not expect lock-skip on a fresh schema; tick=%+v", res)
	}
	if res.EpisodesScanned != 10 {
		t.Fatalf("expected 10 episodes scanned, got %d", res.EpisodesScanned)
	}
	if res.ConceptsCreated != 1 {
		t.Fatalf("expected 1 concept created, got %d", res.ConceptsCreated)
	}
	if res.VersionsAppended != 1 {
		t.Fatalf("expected 1 version appended, got %d", res.VersionsAppended)
	}
	if res.SupportsAppended != 10 {
		t.Fatalf("expected 10 support rows (one per (episode, node)), got %d", res.SupportsAppended)
	}

	if n := mustCountConcepts(ctx, t, fix.db); n != 1 {
		t.Fatalf("expected exactly 1 concept row, got %d", n)
	}
	conceptID := mustReadSoleConceptID(ctx, t, fix.db)
	idx, sup, neg := mustReadLatestConceptVersion(ctx, t, fix.db, conceptID)
	if idx != 0 || sup != 10 || neg != 0 {
		t.Fatalf("expected v=0 support=10 neg=0; got v=%d support=%d neg=%d", idx, sup, neg)
	}

	// Per-(episode, node) support emission contract (issue #7).
	if n := mustCountNullNodeIDSupportForNodeHitEpisodes(ctx, t, fix.db, conceptID); n != 0 {
		t.Fatalf("expected 0 NULL-node support rows for node-hit episodes; got %d", n)
	}

	// consolidator_run lifecycle: one row, status='done',
	// with a non-NULL high-water mark (issue #3 / lifecycle).
	if n := mustCountConsolidatorRuns(ctx, t, fix.db, "done"); n != 1 {
		t.Fatalf("expected 1 'done' consolidator_run, got %d", n)
	}
	var markIsNull bool
	if err := fix.db.QueryRowContext(ctx, `
		SELECT episode_high_water_mark IS NULL
		  FROM consolidator_run WHERE status='done' LIMIT 1
	`).Scan(&markIsNull); err != nil {
		t.Fatalf("check mark: %v", err)
	}
	if markIsNull {
		t.Fatalf("expected non-NULL high-water mark on the successful run")
	}
}

// ────────────────────────────────────────────────────────────
// Scenario B: subsequent run only adds a fresh ConceptVersion
// (NOT a new Concept). After a first 10-episode tick, seed 5
// more episodes for the same signature and tick again; expect:
//   - 0 new concepts
//   - 1 new version (v=1) with support_count=15
//   - 5 new support rows
// ────────────────────────────────────────────────────────────

func TestTick_subsequentRunAppendsVersion(t *testing.T) {
	fix := openConsolFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), intTestDBTimeout)
	defer cancel()

	repoID := seedRepo(ctx, t, fix.db, "scenB")
	contextID := seedRecallContext(ctx, t, fix.db, repoID)
	nodeID := seedNode(ctx, t, fix.db, repoID, randomFingerprint(t), "node-B")

	for i := 0; i < 10; i++ {
		_ = seedEpisode(ctx, t, fix.db, repoID, contextID, nodeID)
	}

	svc := newConsolService(t, fix.db, 10)
	if _, err := svc.Tick(ctx); err != nil {
		t.Fatalf("first Tick: %v", err)
	}
	conceptID := mustReadSoleConceptID(ctx, t, fix.db)

	// 5 more episodes on the same signature.
	for i := 0; i < 5; i++ {
		_ = seedEpisode(ctx, t, fix.db, repoID, contextID, nodeID)
	}

	res, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if res.EpisodesScanned != 5 {
		t.Fatalf("second tick must DELTA scan only the new 5 episodes; got %d (issue #2)",
			res.EpisodesScanned)
	}
	if res.ConceptsCreated != 0 {
		t.Fatalf("expected 0 new concepts on second tick, got %d", res.ConceptsCreated)
	}
	if res.VersionsAppended != 1 {
		t.Fatalf("expected 1 fresh version on second tick, got %d", res.VersionsAppended)
	}
	if res.SupportsAppended != 5 {
		t.Fatalf("expected 5 fresh support rows on second tick, got %d", res.SupportsAppended)
	}

	idx, sup, neg := mustReadLatestConceptVersion(ctx, t, fix.db, conceptID)
	if idx != 1 || sup != 15 || neg != 0 {
		t.Fatalf("expected v=1 support=15 neg=0; got v=%d support=%d neg=%d", idx, sup, neg)
	}
	if n := mustCountVersions(ctx, t, fix.db, conceptID); n != 2 {
		t.Fatalf("expected 2 concept_version rows after the second tick, got %d", n)
	}
}

// ────────────────────────────────────────────────────────────
// Scenario C: support spans repos (G6 cross-repo Concept).
//
// Implementation-plan §6.1: two repos with their own Node rows
// that share a CANONICAL fingerprint MUST collide on the same
// Concept. Iter-1's version cheated by having repo2 Observations
// reference repo1 Nodes; that path is architecturally invalid
// under G2 (every Node is repo-scoped). Iter-2 sets up two
// genuinely independent repos with matching fingerprints.
// ────────────────────────────────────────────────────────────

func TestTick_supportSpansRepos(t *testing.T) {
	fix := openConsolFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), intTestDBTimeout)
	defer cancel()

	// Shared fingerprint bytes: two repos, two distinct Node
	// rows (each with its OWN node_id UUID), same 32-byte
	// fingerprint. node_repo_fingerprint_uidx is on
	// (repo_id, fingerprint) so both inserts succeed.
	sharedFP := deterministicFingerprint("shared-canonical-method")

	repoA := seedRepo(ctx, t, fix.db, "scenC-A")
	repoB := seedRepo(ctx, t, fix.db, "scenC-B")
	ctxA := seedRecallContext(ctx, t, fix.db, repoA)
	ctxB := seedRecallContext(ctx, t, fix.db, repoB)
	nodeA := seedNode(ctx, t, fix.db, repoA, sharedFP, "shared-method")
	nodeB := seedNode(ctx, t, fix.db, repoB, sharedFP, "shared-method")
	if nodeA == nodeB {
		t.Fatalf("two repos must produce distinct node UUIDs even with shared fingerprint")
	}

	// 6 episodes in repoA, 4 in repoB -- total 10, crossing
	// the default threshold via the cross-repo collision.
	for i := 0; i < 6; i++ {
		_ = seedEpisode(ctx, t, fix.db, repoA, ctxA, nodeA)
	}
	for i := 0; i < 4; i++ {
		_ = seedEpisode(ctx, t, fix.db, repoB, ctxB, nodeB)
	}

	svc := newConsolService(t, fix.db, 10)
	res, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.EpisodesScanned != 10 || res.ConceptsCreated != 1 ||
		res.VersionsAppended != 1 || res.SupportsAppended != 10 {
		t.Fatalf("unexpected tick result: %+v", res)
	}

	if n := mustCountConcepts(ctx, t, fix.db); n != 1 {
		t.Fatalf("expected EXACTLY one Concept (G6: cross-repo collision); got %d", n)
	}
	conceptID := mustReadSoleConceptID(ctx, t, fix.db)

	// Support rows MUST carry both repo_ids.
	rows, err := fix.db.QueryContext(ctx,
		`SELECT DISTINCT repo_id::text FROM concept_support WHERE concept_id = $1::uuid`,
		conceptID)
	if err != nil {
		t.Fatalf("read distinct repo_ids: %v", err)
	}
	defer rows.Close()
	seen := make(map[string]bool)
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen[r] = true
	}
	if !seen[repoA] || !seen[repoB] {
		t.Fatalf("expected concept_support to span both repos; got %v", seen)
	}

	// Per-(episode, node) NULL-node contract still holds.
	if n := mustCountNullNodeIDSupportForNodeHitEpisodes(ctx, t, fix.db, conceptID); n != 0 {
		t.Fatalf("expected 0 NULL-node support rows for node-hit episodes; got %d", n)
	}
}

// ────────────────────────────────────────────────────────────
// Scenario D: idempotent re-tick.
// Running Tick twice on the SAME data must NOT duplicate
// versions or support rows.
// ────────────────────────────────────────────────────────────

func TestTick_idempotentReTick(t *testing.T) {
	fix := openConsolFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), intTestDBTimeout)
	defer cancel()

	repoID := seedRepo(ctx, t, fix.db, "scenD")
	contextID := seedRecallContext(ctx, t, fix.db, repoID)
	nodeID := seedNode(ctx, t, fix.db, repoID, randomFingerprint(t), "node-D")

	for i := 0; i < 10; i++ {
		_ = seedEpisode(ctx, t, fix.db, repoID, contextID, nodeID)
	}

	svc := newConsolService(t, fix.db, 10)
	if _, err := svc.Tick(ctx); err != nil {
		t.Fatalf("first Tick: %v", err)
	}
	conceptID := mustReadSoleConceptID(ctx, t, fix.db)
	v1 := mustCountVersions(ctx, t, fix.db, conceptID)
	var supBefore int
	if err := fix.db.QueryRowContext(ctx,
		`SELECT count(*) FROM concept_support WHERE concept_id = $1::uuid`,
		conceptID,
	).Scan(&supBefore); err != nil {
		t.Fatalf("count support before: %v", err)
	}

	// Second tick with NO new episodes: high-water mark is at
	// the tail, DELTA scan returns 0 rows.
	res2, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if res2.EpisodesScanned != 0 {
		t.Fatalf("re-tick must scan 0 episodes (DELTA cursor advanced); got %d", res2.EpisodesScanned)
	}
	if res2.VersionsAppended != 0 || res2.SupportsAppended != 0 || res2.ConceptsCreated != 0 {
		t.Fatalf("re-tick must be a no-op: %+v", res2)
	}
	if got := mustCountVersions(ctx, t, fix.db, conceptID); got != v1 {
		t.Fatalf("version count drift: was %d, now %d", v1, got)
	}
	var supAfter int
	if err := fix.db.QueryRowContext(ctx,
		`SELECT count(*) FROM concept_support WHERE concept_id = $1::uuid`,
		conceptID,
	).Scan(&supAfter); err != nil {
		t.Fatalf("count support after: %v", err)
	}
	if supAfter != supBefore {
		t.Fatalf("support row count drift: was %d, now %d", supBefore, supAfter)
	}
}

// ────────────────────────────────────────────────────────────
// Scenario E: sub-threshold groups don't create a Concept.
// 5 positive episodes against the default threshold 10 must
// yield 0 concepts.
// ────────────────────────────────────────────────────────────

func TestTick_subThresholdIsNoOp(t *testing.T) {
	fix := openConsolFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), intTestDBTimeout)
	defer cancel()

	repoID := seedRepo(ctx, t, fix.db, "scenE")
	contextID := seedRecallContext(ctx, t, fix.db, repoID)
	nodeID := seedNode(ctx, t, fix.db, repoID, randomFingerprint(t), "node-E")

	for i := 0; i < 5; i++ {
		_ = seedEpisode(ctx, t, fix.db, repoID, contextID, nodeID)
	}

	svc := newConsolService(t, fix.db, 10)
	res, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.EpisodesScanned != 5 {
		t.Fatalf("expected 5 scanned; got %d", res.EpisodesScanned)
	}
	if res.ConceptsCreated != 0 || res.VersionsAppended != 0 || res.SupportsAppended != 0 {
		t.Fatalf("sub-threshold must not emit anything: %+v", res)
	}
	if n := mustCountConcepts(ctx, t, fix.db); n != 0 {
		t.Fatalf("expected 0 concept rows below threshold; got %d", n)
	}
}

// ────────────────────────────────────────────────────────────
// Scenario E2 (iter-3 evaluator's #3 finding + iter-4 #1):
// support accumulates ACROSS ticks for sub-threshold first-time
// groups via the durable concept_candidate_support staging table
// (migration 0021).
//
// Plan: seed 5 positive Episodes (below threshold=10), Tick
// (no concept created, but candidate_support has 5 pending rows
// AND the high-water mark advances past them), seed 5 MORE
// positive Episodes referencing the same Node, Tick again, then
// assert EXACTLY ONE Concept with the latest ConceptVersion's
// support_count=10.
//
// Iter-4 design departure from iter-3: tick 1 ADVANCES the
// cursor past all 5 sub-threshold Episodes (vs. iter-3's
// walk-until-first-pending which left mark=NULL). Sub-threshold
// support persistence moved from in-memory (per-tick) to
// durable rows (per-signature). Tick 2 therefore scans only 5
// NEW Episodes; the cumulative count comes from
// concept_candidate_support, not from re-scanning the entire
// Episode table.
// ────────────────────────────────────────────────────────────

func TestTick_subThresholdAccumulatesAcrossTicks(t *testing.T) {
	fix := openConsolFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), intTestDBTimeout)
	defer cancel()

	repoID := seedRepo(ctx, t, fix.db, "scenE2")
	contextID := seedRecallContext(ctx, t, fix.db, repoID)
	nodeID := seedNode(ctx, t, fix.db, repoID, randomFingerprint(t), "node-E2")

	// Tick 1: 5 positive Episodes (sub-threshold).
	for i := 0; i < 5; i++ {
		_ = seedEpisode(ctx, t, fix.db, repoID, contextID, nodeID)
	}
	svc := newConsolService(t, fix.db, 10)
	res1, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick 1: %v", err)
	}
	if res1.EpisodesScanned != 5 {
		t.Fatalf("tick 1: expected 5 scanned; got %d", res1.EpisodesScanned)
	}
	if res1.ConceptsCreated != 0 || res1.VersionsAppended != 0 || res1.SupportsAppended != 0 {
		t.Fatalf("tick 1: sub-threshold must not emit anything: %+v", res1)
	}
	if n := mustCountConcepts(ctx, t, fix.db); n != 0 {
		t.Fatalf("tick 1: expected 0 concept rows below threshold; got %d", n)
	}

	// iter-4 INVARIANT: after tick 1 the high-water mark MUST
	// HAVE ADVANCED past all 5 sub-threshold Episodes. Their
	// support contributions are durably staged in
	// concept_candidate_support (migration 0021) so the cursor
	// is free to advance. Verify the mark is NON-NULL via the
	// 'done' consolidator_run row (lock_skipped rows are
	// excluded by status filter -- the iter-3 #2 finding fix).
	var mark sql.NullString
	if err := fix.db.QueryRowContext(ctx, `
		SELECT episode_high_water_mark::text
		  FROM consolidator_run
		 WHERE status='done'
		 ORDER BY finished_at DESC
		 LIMIT 1
	`).Scan(&mark); err != nil {
		t.Fatalf("read mark after tick 1: %v", err)
	}
	if !mark.Valid {
		t.Fatalf("tick 1: expected mark to ADVANCE past sub-threshold Episodes (iter-4 candidate-state strategy); got NULL")
	}

	// And the candidate-support staging table MUST contain the
	// 5 pending contributions (1 row per (episode, node) since
	// the seed creates a single-node hit per Episode).
	var pendingCandidates int
	if err := fix.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM concept_candidate_support
		 WHERE promoted_to_concept_id IS NULL
	`).Scan(&pendingCandidates); err != nil {
		t.Fatalf("count pending candidate_support: %v", err)
	}
	if pendingCandidates != 5 {
		t.Fatalf("tick 1: expected 5 pending candidate_support rows; got %d", pendingCandidates)
	}

	// Tick 2: 5 MORE positive Episodes (same signature). Because
	// the mark advanced in tick 1, the DELTA scan in tick 2 sees
	// ONLY the 5 NEW Episodes; the cumulative-count decision
	// (10 >= threshold) is taken from concept_candidate_support
	// (5 pending + 5 from this tick = 10 distinct positives).
	for i := 0; i < 5; i++ {
		_ = seedEpisode(ctx, t, fix.db, repoID, contextID, nodeID)
	}
	res2, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick 2: %v", err)
	}
	if res2.EpisodesScanned != 5 {
		t.Fatalf("tick 2: expected 5 scanned (DELTA past tick-1 mark); got %d", res2.EpisodesScanned)
	}
	if res2.ConceptsCreated != 1 {
		t.Fatalf("tick 2: expected 1 concept created (threshold crossed via candidate-state aggregate); got %d", res2.ConceptsCreated)
	}
	if res2.VersionsAppended != 1 {
		t.Fatalf("tick 2: expected 1 version appended; got %d", res2.VersionsAppended)
	}
	if res2.SupportsAppended != 10 {
		t.Fatalf("tick 2: expected 10 support rows (per-(episode, node), 10 total candidate rows promoted); got %d",
			res2.SupportsAppended)
	}

	if n := mustCountConcepts(ctx, t, fix.db); n != 1 {
		t.Fatalf("expected exactly 1 concept row after tick 2; got %d", n)
	}
	conceptID := mustReadSoleConceptID(ctx, t, fix.db)
	idx, sup, neg := mustReadLatestConceptVersion(ctx, t, fix.db, conceptID)
	if idx != 0 || sup != 10 || neg != 0 {
		t.Fatalf("after tick 2 expected v=0 support=10 neg=0; got v=%d support=%d neg=%d",
			idx, sup, neg)
	}

	// All 10 candidate rows must now be marked promoted (NULL
	// pending count = 0).
	if err := fix.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM concept_candidate_support
		 WHERE promoted_to_concept_id IS NULL
	`).Scan(&pendingCandidates); err != nil {
		t.Fatalf("recount pending after promotion: %v", err)
	}
	if pendingCandidates != 0 {
		t.Fatalf("expected 0 pending candidate_support after promotion; got %d", pendingCandidates)
	}

	// Per-(episode, node) emission contract still holds.
	if n := mustCountNullNodeIDSupportForNodeHitEpisodes(ctx, t, fix.db, conceptID); n != 0 {
		t.Fatalf("expected 0 NULL-node support rows for node-hit episodes; got %d", n)
	}

	// And tick 3 (no new Episodes) MUST be a true no-op: the
	// mark advanced past all 10 on tick 2 so the DELTA scan
	// returns 0 rows.
	res3, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick 3: %v", err)
	}
	if res3.EpisodesScanned != 0 {
		t.Fatalf("tick 3: expected 0 scanned (mark advanced past all 10); got %d",
			res3.EpisodesScanned)
	}
	if res3.ConceptsCreated != 0 || res3.VersionsAppended != 0 || res3.SupportsAppended != 0 {
		t.Fatalf("tick 3: expected pure no-op; got %+v", res3)
	}
}

// ────────────────────────────────────────────────────────────
// Scenario F: wake-after-N fires an early tick.
//
// Issue #5: implementation-plan.md §6.1 line 873 calls for "wakes
// every K minutes (§7.7) or after N new Episodes (configurable)".
// We configure RunInterval=1h (so the long-poll ticker will NOT
// fire) and WakeAfterNEpisodes=10 / WakeCheckInterval=150ms. The
// service should see 10 unconsumed episodes within the first
// few wake-check cycles, fire a Tick, and crystallise the
// Concept.
//
// NOTE on starting state: Run() always fires one immediate Tick
// at startup, BEFORE the wake-check loop engages. We seed
// episodes AFTER that initial tick so the wake-after-N path is
// the one that crosses the threshold.
// ────────────────────────────────────────────────────────────

func TestRun_wakeAfterNEpisodes(t *testing.T) {
	fix := openConsolFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), intTestDBTimeout)
	defer cancel()

	repoID := seedRepo(ctx, t, fix.db, "scenF")
	contextID := seedRecallContext(ctx, t, fix.db, repoID)
	nodeID := seedNode(ctx, t, fix.db, repoID, randomFingerprint(t), "node-F")

	// Long-poll interval is 1h so only the wake-after-N path
	// can fire a tick within the 30s test deadline.
	svc, err := New(fix.db, Config{
		Threshold:          10,
		RunInterval:        1 * time.Hour,
		TickTimeout:        intTestDBTimeout,
		WakeAfterNEpisodes: 10,
		WakeCheckInterval:  150 * time.Millisecond,
		AdvisoryLockKey:    uniqueLockKey(),
	}, silentLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runCtx, runCancel := context.WithTimeout(ctx, 20*time.Second)
	defer runCancel()

	runDone := make(chan error, 1)
	go func() { runDone <- svc.Run(runCtx) }()

	// Give the initial Tick + the first wake-check a chance to
	// complete the no-op pass over an empty cluster.
	time.Sleep(500 * time.Millisecond)

	// Seed 10 episodes (crosses threshold). The wake-after-N
	// loop should fire a tick within a handful of WakeCheckInterval
	// cycles.
	for i := 0; i < 10; i++ {
		_ = seedEpisode(ctx, t, fix.db, repoID, contextID, nodeID)
	}

	// Poll for the concept's appearance. Bail after 8 seconds
	// (well below the test-deadline budget) to give clear
	// failure output rather than the goroutine hanging.
	deadline := time.Now().Add(8 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("wake-after-N never fired a tick that crystallised the concept; "+
				"concepts=%d (expected >=1) within %s",
				mustCountConcepts(ctx, t, fix.db), time.Since(deadline.Add(-8*time.Second)))
		}
		if mustCountConcepts(ctx, t, fix.db) >= 1 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	runCancel()
	select {
	case err := <-runDone:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Run returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Run did not honour ctx cancellation in time")
	}

	// Final correctness check: the concept's latest version
	// must record support_count >= 10.
	conceptID := mustReadSoleConceptID(ctx, t, fix.db)
	_, sup, _ := mustReadLatestConceptVersion(ctx, t, fix.db, conceptID)
	if sup < 10 {
		t.Fatalf("expected support_count >= 10 after wake-tick; got %d", sup)
	}

	// Idempotency: at most one consolidator_run row should
	// have observed the >=10 threshold (the wake-fired one).
	// We tolerate the initial empty-tick + the wake-fired tick.
	if n := mustCountConsolidatorRuns(ctx, t, fix.db, "done"); n < 1 {
		t.Fatalf("expected >=1 'done' run, got %d", n)
	}

	// Negative cases for the second test scenario in this file:
	// using a negative episode in addition increases negative_count.
	// (We don't add one here; that case is exercised by the unit
	// test TestEpisodeStatePolarity and by the rerun test below.)
}

// ────────────────────────────────────────────────────────────
// Bonus: a negative-polarity episode bumps negative_count.
// This is not a Stage 6.1 acceptance scenario per se, but
// exercises the polarity → support/negative split end-to-end
// against live PG so a polarity regression surfaces.
// ────────────────────────────────────────────────────────────

func TestTick_negativeEpisodeContributesToNegativeCount(t *testing.T) {
	fix := openConsolFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), intTestDBTimeout)
	defer cancel()

	repoID := seedRepo(ctx, t, fix.db, "scenN")
	contextID := seedRecallContext(ctx, t, fix.db, repoID)
	nodeID := seedNode(ctx, t, fix.db, repoID, randomFingerprint(t), "node-N")

	// 10 positive + 3 negative against the same node (same sig).
	for i := 0; i < 10; i++ {
		_ = seedEpisode(ctx, t, fix.db, repoID, contextID, nodeID)
	}
	for i := 0; i < 3; i++ {
		_ = seedNegativeEpisode(ctx, t, fix.db, repoID, contextID, nodeID)
	}

	svc := newConsolService(t, fix.db, 10)
	res, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.EpisodesScanned != 13 {
		t.Fatalf("expected 13 scanned; got %d", res.EpisodesScanned)
	}

	conceptID := mustReadSoleConceptID(ctx, t, fix.db)
	_, sup, neg := mustReadLatestConceptVersion(ctx, t, fix.db, conceptID)
	if sup != 10 || neg != 3 {
		t.Fatalf("expected sup=10 neg=3; got sup=%d neg=%d", sup, neg)
	}
}

// ────────────────────────────────────────────────────────────
// Iter-3 evaluator finding #2 regression test: a lock-skipped
// run's stale mark MUST NOT regress the effective cursor.
//
// Plan: tick once with 10 Episodes -> creates concept, mark
// advances to ep10. Then INSERT a synthetic
// status='lock_skipped' consolidator_run row whose
// finished_at is NEWER than the 'done' run AND whose
// episode_high_water_mark is NULL (the stale-mark shape that
// would otherwise rewind the cursor). Tick again; the DELTA
// scan MUST return 0 rows because priorHighWater's
// `WHERE status='done'` filter excludes the lock_skipped row
// and keeps the cursor anchored at ep10.
//
// Without the iter-4 #2 fix, priorHighWater picked up the
// most-recent 'done'-OR-anything row (older code path), the
// lock_skipped row's NULL mark would be the new "prior",
// scanEpisodes would re-scan all 10 Episodes, and the
// idempotency dedup would still produce an extra
// concept_version row.
// ────────────────────────────────────────────────────────────

func TestPriorHighWater_excludesLockSkippedRuns(t *testing.T) {
	fix := openConsolFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), intTestDBTimeout)
	defer cancel()

	repoID := seedRepo(ctx, t, fix.db, "scenLS")
	contextID := seedRecallContext(ctx, t, fix.db, repoID)
	nodeID := seedNode(ctx, t, fix.db, repoID, randomFingerprint(t), "node-LS")

	for i := 0; i < 10; i++ {
		_ = seedEpisode(ctx, t, fix.db, repoID, contextID, nodeID)
	}

	svc := newConsolService(t, fix.db, 10)
	res1, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick 1: %v", err)
	}
	if res1.ConceptsCreated != 1 || res1.EpisodesScanned != 10 {
		t.Fatalf("tick 1 should create the concept; got %+v", res1)
	}

	// Sanity: the 'done' tick-1 row carries the post-tick mark.
	var doneMark sql.NullString
	if err := fix.db.QueryRowContext(ctx, `
		SELECT episode_high_water_mark::text
		  FROM consolidator_run
		 WHERE status='done'
		 ORDER BY finished_at DESC
		 LIMIT 1
	`).Scan(&doneMark); err != nil {
		t.Fatalf("read done mark: %v", err)
	}
	if !doneMark.Valid {
		t.Fatalf("expected 'done' mark to be non-NULL after tick 1")
	}

	// Inject the adversarial lock_skipped row: NULL mark,
	// finished_at = NOW() + 5 minutes. Under the iter-3 (buggy)
	// priorHighWater that ordered by finished_at WITHOUT a
	// status filter, this row would WIN and the next tick would
	// rewind to a NULL cursor.
	//
	// NOTE: the column list MUST stay aligned with
	// migrations/0012_run_tables.sql -- consolidator_run has
	// only {run_id, started_at, finished_at,
	// episode_high_water_mark, status}; the operational counters
	// live in Prometheus metrics, NOT in this row.
	if _, err := fix.db.ExecContext(ctx, `
		INSERT INTO consolidator_run
		    (started_at, finished_at, status, episode_high_water_mark)
		VALUES (now(), now() + interval '5 minutes', 'lock_skipped', NULL)
	`); err != nil {
		t.Fatalf("inject lock_skipped row: %v", err)
	}

	res2, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick 2: %v", err)
	}
	if res2.EpisodesScanned != 0 {
		t.Fatalf("tick 2: priorHighWater should exclude lock_skipped row; "+
			"expected 0 scanned (cursor anchored at ep10); got %d (cursor regressed)",
			res2.EpisodesScanned)
	}
	if res2.ConceptsCreated != 0 || res2.VersionsAppended != 0 || res2.SupportsAppended != 0 {
		t.Fatalf("tick 2: lock_skipped exclusion failed -- got new emissions: %+v", res2)
	}

	// The concept_version count must still be 1 (no duplicate
	// version appended via stale-mark re-scan).
	conceptID := mustReadSoleConceptID(ctx, t, fix.db)
	if v := mustCountVersions(ctx, t, fix.db, conceptID); v != 1 {
		t.Fatalf("expected exactly 1 concept_version after lock_skipped injection; got %d", v)
	}
}

// ────────────────────────────────────────────────────────────
// Iter-3 evaluator finding #1 regression test: a permanently
// sub-threshold signature MUST NOT pin the high-water cursor.
//
// Plan: seed 3 positive Episodes of sig-A (threshold=10) +
// 3 positive Episodes of sig-B (also sub-threshold, distinct
// node so the fingerprint differs). Tick 1. Assert:
//   - 6 scanned, 0 concepts created,
//   - mark advanced to the LAST of the 6 seeded Episodes,
//   - concept_candidate_support has 6 pending rows total (3
//     per signature).
// Then tick 2 with NO new Episodes -- DELTA scan MUST return 0
// rows (proves the cursor did NOT regress). Finally seed 7 MORE
// sig-A Episodes (3 + 7 = 10, crossing threshold via durable
// candidate aggregation) and tick 3. Assert:
//   - 7 scanned (only the 7 new ones; NOT 13 = re-scan storm),
//   - 1 concept created (sig-A's),
//   - sig-A version support=10 (3 pending + 7 new),
//   - sig-B remains pending (3 candidate_support rows still NULL
//     promoted_to_concept_id).
//
// Under the iter-3 walk-until-first-pending bug, sig-A (sorted
// before sig-B in some signature orderings) being pending would
// pin the cursor BEFORE sig-A's first Episode forever, forcing
// every subsequent tick to re-scan ALL 6 (or 13) episodes -- the
// "unbounded rescans" the evaluator named in iter-3 #1.
// ────────────────────────────────────────────────────────────

func TestTick_pendingSigDoesNotPinCursor(t *testing.T) {
	fix := openConsolFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), intTestDBTimeout)
	defer cancel()

	repoID := seedRepo(ctx, t, fix.db, "scenPin")
	contextID := seedRecallContext(ctx, t, fix.db, repoID)
	nodeA := seedNode(ctx, t, fix.db, repoID, randomFingerprint(t), "node-pin-A")
	nodeB := seedNode(ctx, t, fix.db, repoID, randomFingerprint(t), "node-pin-B")

	// Tick 1: 3 sig-A + 3 sig-B, both sub-threshold.
	for i := 0; i < 3; i++ {
		_ = seedEpisode(ctx, t, fix.db, repoID, contextID, nodeA)
	}
	for i := 0; i < 3; i++ {
		_ = seedEpisode(ctx, t, fix.db, repoID, contextID, nodeB)
	}

	svc := newConsolService(t, fix.db, 10)
	res1, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick 1: %v", err)
	}
	if res1.EpisodesScanned != 6 {
		t.Fatalf("tick 1: expected 6 scanned; got %d", res1.EpisodesScanned)
	}
	if res1.ConceptsCreated != 0 {
		t.Fatalf("tick 1: sub-threshold -- expected 0 concepts; got %d", res1.ConceptsCreated)
	}

	// Mark MUST have advanced (iter-4 candidate-state strategy).
	var mark1 sql.NullString
	if err := fix.db.QueryRowContext(ctx, `
		SELECT episode_high_water_mark::text
		  FROM consolidator_run
		 WHERE status='done'
		 ORDER BY finished_at DESC
		 LIMIT 1
	`).Scan(&mark1); err != nil {
		t.Fatalf("read mark after tick 1: %v", err)
	}
	if !mark1.Valid {
		t.Fatalf("tick 1: cursor MUST advance past sub-threshold Episodes (iter-3 #1 regression)")
	}

	var pendingCount int
	if err := fix.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM concept_candidate_support
		 WHERE promoted_to_concept_id IS NULL
	`).Scan(&pendingCount); err != nil {
		t.Fatalf("count pending candidates: %v", err)
	}
	if pendingCount != 6 {
		t.Fatalf("tick 1: expected 6 pending candidate_support rows (3 per sig); got %d", pendingCount)
	}

	// Tick 2: no new Episodes -- the DELTA scan MUST return 0.
	// This is the direct contradiction of the iter-3 cursor
	// pinning bug.
	res2, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick 2: %v", err)
	}
	if res2.EpisodesScanned != 0 {
		t.Fatalf("tick 2: cursor pinned -- expected 0 scanned, got %d (iter-3 #1 regression: "+
			"a pending sig is forcing re-scans)", res2.EpisodesScanned)
	}

	// Tick 3: 7 more sig-A Episodes -> sig-A crosses threshold
	// via candidate aggregation (3 pending + 7 new = 10 distinct
	// positive episodes).
	for i := 0; i < 7; i++ {
		_ = seedEpisode(ctx, t, fix.db, repoID, contextID, nodeA)
	}
	res3, err := svc.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick 3: %v", err)
	}
	if res3.EpisodesScanned != 7 {
		t.Fatalf("tick 3: expected DELTA=7 (only the new sig-A episodes); got %d (cursor regressed)",
			res3.EpisodesScanned)
	}
	if res3.ConceptsCreated != 1 {
		t.Fatalf("tick 3: expected 1 concept created (sig-A crosses threshold); got %d",
			res3.ConceptsCreated)
	}

	// Exactly 1 concept total (sig-A's). sig-B remains pending.
	if n := mustCountConcepts(ctx, t, fix.db); n != 1 {
		t.Fatalf("expected 1 total concept (sig-B still pending); got %d", n)
	}
	conceptID := mustReadSoleConceptID(ctx, t, fix.db)
	idx, sup, neg := mustReadLatestConceptVersion(ctx, t, fix.db, conceptID)
	if idx != 0 || sup != 10 || neg != 0 {
		t.Fatalf("sig-A concept: expected v=0 sup=10 neg=0; got v=%d sup=%d neg=%d",
			idx, sup, neg)
	}

	// sig-B's 3 candidate_support rows MUST still be pending.
	if err := fix.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM concept_candidate_support
		 WHERE promoted_to_concept_id IS NULL
	`).Scan(&pendingCount); err != nil {
		t.Fatalf("recount pending after sig-A promotion: %v", err)
	}
	if pendingCount != 3 {
		t.Fatalf("expected 3 pending candidate_support rows (sig-B only); got %d", pendingCount)
	}
}
