package migrations_test

// Integration test that exercises the migrations against a live
// PostgreSQL 16 instance. The test skips cleanly when
// AGENT_MEMORY_PG_URL is unset, so `make test` on a developer
// laptop without the docker compose stack still exits 0.
//
// In CI the integration-stack job in
// .github/workflows/agent-memory-ci.yml exports
// AGENT_MEMORY_PG_URL to the docker compose Postgres container,
// at which point every scenario below runs for real.
//
// Implementation-plan.md Stage 1.2 test scenarios covered here:
//
//   * "structural schema applies cleanly"
//       -> TestUp_appliesEntireStage12_andEveryExpectedObjectExists
//   * "ingest_jobs accepts only valid mode/status"
//       -> TestIngestJobs_rejectsInvalidMode
//       -> TestIngestJobs_rejectsInvalidStatus
//   * "fingerprint CHECK rejects wrong length"
//       -> TestNode_fingerprintLengthCheck
//       -> TestEdge_fingerprintLengthCheck
//   * "round-trip migration"
//       -> TestRoundTrip_schemaIsByteIdenticalAfterDownUp

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq" // database/sql driver: "postgres"

	"github.com/smartpcr/code-intelligence/services/agent-memory/migrations"
)

// envPGURL is the connection-string env var the CI workflow
// exports for the docker compose Postgres container. When it is
// empty every integration test in this file is skipped.
const envPGURL = "AGENT_MEMORY_PG_URL"

// testDBTimeout caps any single SQL operation. Bumped to 30s so
// the (rare) cold-start partition creation in CI doesn't flake.
const testDBTimeout = 30 * time.Second

// openTestDB returns a *sql.DB rooted in a freshly created
// per-test schema, plus a cleanup hook that drops the schema and
// closes the handle. The schema isolation lets concurrent tests
// share one PostgreSQL database without colliding.
func openTestDB(t *testing.T) (*sql.DB, string, func()) {
	t.Helper()
	url := os.Getenv(envPGURL)
	if url == "" {
		t.Skipf("skipping: %s is unset; integration tests require a live PostgreSQL", envPGURL)
	}
	db, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("sql.Open(postgres): %v", err)
	}
	// `search_path` is a per-session setting. Capping the pool at
	// one connection guarantees every Exec/Query in this test
	// runs on the same backend session as the SET search_path
	// statement below; otherwise round-robin connection pickup
	// would route some statements at `public` instead of our
	// isolated test schema.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Skipf("skipping: cannot reach PostgreSQL at %s: %v", envPGURL, err)
	}
	schema := newSchemaName(t)
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA `+quoteIdent(schema)); err != nil {
		_ = db.Close()
		t.Fatalf("create test schema %q: %v", schema, err)
	}
	// Point every unqualified DDL at the test schema. `public`
	// is still on the path so pgcrypto's gen_random_uuid is
	// resolvable; `partman` is appended for pg_partman in
	// future stages but is harmless here.
	if _, err := db.ExecContext(ctx, fmt.Sprintf(
		`SET search_path TO %s, public, partman`, quoteIdent(schema),
	)); err != nil {
		_ = db.Close()
		t.Fatalf("set search_path: %v", err)
	}
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
		defer cancel()
		// pg_partman.part_config rows referencing tables in this
		// schema must be removed before the schema drops, because
		// the partman BGW will otherwise try to maintain a
		// dangling parent_table reference. Tests that never call
		// Up() leave part_config empty -- DELETE on no rows is a
		// no-op.
		//
		// The schema prefix is escaped (ESCAPE '#') because
		// `amtest_<hex>` contains a `_` which is a LIKE wildcard
		// matching any single character; without escaping, this
		// cleanup could erroneously delete part_config rows
		// belonging to a sibling test schema (or another tenant).
		schemaPrefix := strings.ReplaceAll(schema, "_", "#_") + ".%"
		_, _ = db.ExecContext(ctx, `
			DELETE FROM partman.part_config
			WHERE parent_table LIKE $1 ESCAPE '#'
		`, schemaPrefix)
		// CASCADE drops every table, type, index, and partition
		// created during the test, including the migration journal.
		_, _ = db.ExecContext(ctx, `DROP SCHEMA `+quoteIdent(schema)+` CASCADE`)
		_ = db.Close()
	}
	return db, schema, cleanup
}

// newSchemaName returns a short random identifier suitable for
// `CREATE SCHEMA`. The "amtest_" prefix keeps the namespace
// hygienic if the test panics before cleanup runs.
func newSchemaName(t *testing.T) string {
	t.Helper()
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return "amtest_" + hex.EncodeToString(buf[:])
}

// quoteIdent quotes a SQL identifier per PostgreSQL rules. Used
// only on values we synthesized ourselves; never on user input.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// TestUp_appliesEntireStage12_andEveryExpectedObjectExists is the
// "structural schema applies cleanly" scenario from
// implementation-plan.md Stage 1.2, extended by Stage 1.3 to
// cover the episodic + concept tables added by 0007 .. 0014.
// After Up() every expected ENUM, table, and UNIQUE index must
// be present in the per-test schema.
func TestUp_appliesEntireStage12_andEveryExpectedObjectExists(t *testing.T) {
	db, schema, cleanup := openTestDB(t)
	defer cleanup()
	m := migrations.New(db)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}

	wantTables := []string{
		// Stage 1.2 structural set.
		"repo", "repo_commit",
		"node", "edge",
		"node_retirement", "edge_retirement",
		"trace_observation", "trace_observation_log",
		"repo_event",
		"ingest_jobs",
		// Stage 1.3 episodic + concept set.
		"episode", "episode_update", "observation",
		"recall_context_log",
		"concept", "concept_version", "concept_support",
		"consolidator_run", "promoter_run", "reranker_model",
		"synthetic_positive_emission",
	}
	for _, tbl := range wantTables {
		if !relationExists(t, db, schema, tbl) {
			t.Errorf("expected table %q in schema %q after Up()", tbl, schema)
		}
	}

	wantEnums := map[string][]string{
		// Architecture-level enums (tech-spec §8.7.1).
		"node_kind":        {"repo", "package", "file", "class", "method", "block"},
		"edge_kind":        {"contains", "imports", "static_calls", "observed_calls", "extends", "implements", "reads", "writes", "renamed_to"},
		"episode_kind":     {"agent", "feedback", "synthetic_positive"},
		"outcome":          {"success", "failure", "refused", "degraded", "human_corrected"},
		"block_kind":       {"entry", "branch", "loop_body", "exception", "exit"},
		"concept_band":     {"low", "medium", "high"},
		"producer":         {"consolidator", "promoter"},
		"polarity":         {"positive", "negative"},
		"actor":            {"operator", "consolidator", "system"},
		"observation_role": {"node_hit", "edge_hit", "call_edge_hit", "concept_hit", "degraded_recall_context"},
		"repo_event_kind":  {"push", "merge", "register", "manual"},
		"verb":             {"recall", "expand", "summarize"},
		"degraded_reason":  {"episodic_log_unavailable", "graph_store_unavailable", "embedding_index_unavailable", "reranker_model_stale", "span_ingestor_backpressure", "consolidator_backpressure"},
		// ingest_jobs locals (defined in 0006a).
		"ingest_mode":   {"full", "delta", "manual"},
		"ingest_status": {"pending", "claimed", "running", "done", "failed"},
	}
	for name, want := range wantEnums {
		got := enumLabels(t, db, schema, name)
		if got == nil {
			t.Errorf("expected ENUM %q in schema %q after Up()", name, schema)
			continue
		}
		if !stringSlicesEqual(got, want) {
			t.Errorf("ENUM %q labels = %v, want %v (closed-set drift!)", name, got, want)
		}
	}

	// Stronger than name-only: assert the FULL pg_get_indexdef
	// for indices whose shape is contractual (G2 fingerprint
	// uniqueness, idempotent enqueue, hot-path partial). Renaming
	// is fine; quietly changing the columns / predicate is not.
	wantIdxDef := []struct {
		index, mustContain string
	}{
		{"node_repo_fingerprint_uidx", "UNIQUE INDEX node_repo_fingerprint_uidx ON"},
		{"node_repo_fingerprint_uidx", "(repo_id, fingerprint)"},
		{"edge_repo_fingerprint_uidx", "UNIQUE INDEX edge_repo_fingerprint_uidx ON"},
		{"edge_repo_fingerprint_uidx", "(repo_id, fingerprint)"},
		{"node_retirement_node_id_uidx", "UNIQUE INDEX node_retirement_node_id_uidx ON"},
		{"node_retirement_node_id_uidx", "(node_id)"},
		{"edge_retirement_edge_id_uidx", "UNIQUE INDEX edge_retirement_edge_id_uidx ON"},
		{"edge_retirement_edge_id_uidx", "(edge_id)"},
		{"ingest_jobs_dedupe_uidx", "UNIQUE INDEX ingest_jobs_dedupe_uidx ON"},
		{"ingest_jobs_dedupe_uidx", "COALESCE(from_sha,"},
		// The partial-pending index is what makes
		// SELECT ... FOR UPDATE SKIP LOCKED fast; if the WHERE
		// clause drifts the queue degrades silently.
		{"ingest_jobs_pending_idx", "WHERE (status = 'pending'"},
		// TraceObservationLog scan per tech-spec §8.7.2.
		{"trace_observation_log_edge_started_idx", "(edge_id, started_at DESC)"},
		// Stage 1.3: concept fingerprint uniqueness (G6 -- no
		// repo_id, cross-repo per arch §5.5.1).
		{"concept_fingerprint_uidx", "UNIQUE INDEX concept_fingerprint_uidx ON"},
		{"concept_fingerprint_uidx", "(fingerprint)"},
		// Stage 1.3: most-recent ConceptVersion read per tech-spec
		// §8.7.2 / arch §5.5.1.
		{"concept_version_concept_version_idx", "(concept_id, version_index DESC)"},
		// Stage 1.3: (concept_id, version_index) monotonicity
		// guard (arch §5.5.2: "Monotonic per concept_id").
		{"concept_version_concept_version_uidx", "UNIQUE INDEX concept_version_concept_version_uidx ON"},
		// Stage 1.3: Episode hot-path read for mgmt.read.episodes.
		{"episode_repo_created_idx", "(repo_id, created_at DESC)"},
		// Stage 1.3: EpisodeUpdate current_status join hot path.
		{"episode_update_episode_created_idx", "(episode_id, created_at DESC)"},
		// Stage 1.3: Observation gather-by-episode hot path.
		{"observation_episode_created_idx", "(episode_id, created_at DESC)"},
		// Stage 1.3: RecallContextLog hot-path read for
		// mgmt.read.recall_contexts.
		{"recall_context_log_repo_created_idx", "(repo_id, created_at DESC)"},
	}
	for _, w := range wantIdxDef {
		def := indexDef(t, db, schema, w.index)
		if def == "" {
			t.Errorf("expected index %q to exist", w.index)
			continue
		}
		if !strings.Contains(def, w.mustContain) {
			t.Errorf("index %q definition missing substring %q\n  full def: %s",
				w.index, w.mustContain, def)
		}
	}
}

// TestIngestJobs_rejectsInvalidMode is the
// "ingest_jobs accepts only valid mode/status" scenario, mode half.
func TestIngestJobs_rejectsInvalidMode(t *testing.T) {
	db, _, cleanup := openTestDB(t)
	defer cleanup()
	mustUp(t, db)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()
	// Seed a repo so the FK in ingest_jobs has a row to point at.
	var repoID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO repo (url, default_branch, current_head_sha)
		VALUES ('https://example.test/repo', 'main', 'deadbeef')
		RETURNING repo_id
	`).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO ingest_jobs (repo_id, mode, to_sha)
		VALUES ($1, 'rebuild', 'deadbeef')
	`, repoID)
	if err == nil {
		t.Fatal("expected ENUM violation inserting mode='rebuild'; got nil")
	}
	// ENUM violation is reported by Postgres with SQLSTATE 22P02
	// (invalid_text_representation) when the literal is cast
	// to an enum type. We assert on the human message rather
	// than the code to keep the assertion driver-agnostic.
	if !strings.Contains(err.Error(), "ingest_mode") {
		t.Errorf("error does not mention ingest_mode ENUM: %v", err)
	}
}

// TestIngestJobs_rejectsInvalidStatus is the status half of
// "ingest_jobs accepts only valid mode/status".
func TestIngestJobs_rejectsInvalidStatus(t *testing.T) {
	db, _, cleanup := openTestDB(t)
	defer cleanup()
	mustUp(t, db)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()
	var repoID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO repo (url, default_branch, current_head_sha)
		VALUES ('https://example.test/repo', 'main', 'cafef00d')
		RETURNING repo_id
	`).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO ingest_jobs (repo_id, mode, to_sha, status)
		VALUES ($1, 'full', 'cafef00d', 'unknown')
	`, repoID)
	if err == nil {
		t.Fatal("expected ENUM violation inserting status='unknown'; got nil")
	}
	if !strings.Contains(err.Error(), "ingest_status") {
		t.Errorf("error does not mention ingest_status ENUM: %v", err)
	}
}

// TestNode_fingerprintLengthCheck is the "fingerprint CHECK
// rejects wrong length" scenario for node. A 1-byte fingerprint
// must be rejected with a CHECK violation that mentions
// octet_length (per implementation-plan.md scenario wording).
func TestNode_fingerprintLengthCheck(t *testing.T) {
	db, _, cleanup := openTestDB(t)
	defer cleanup()
	mustUp(t, db)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()
	var repoID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO repo (url, default_branch, current_head_sha)
		VALUES ('https://example.test/repo', 'main', 'aaaa1111')
		RETURNING repo_id
	`).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	// 1-byte fingerprint must trip the octet_length CHECK.
	_, err := db.ExecContext(ctx, `
		INSERT INTO node (fingerprint, repo_id, kind, canonical_signature, from_sha)
		VALUES (E'\\x00', $1, 'method', 'pkg.X#y()', 'aaaa1111')
	`, repoID)
	if err == nil {
		t.Fatal("expected CHECK violation for 1-byte fingerprint; got nil")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "octet_length") && !strings.Contains(msg, "node_fingerprint_octet_length_chk") {
		t.Errorf("CHECK error does not reference octet_length or the constraint name: %v", err)
	}
	// Sanity: a valid 32-byte fingerprint *does* go in. This
	// guards against the CHECK accidentally rejecting everything.
	var nodeID string
	err = db.QueryRowContext(ctx, `
		INSERT INTO node (fingerprint, repo_id, kind, canonical_signature, from_sha)
		VALUES (decode('00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff', 'hex'),
		        $1, 'method', 'pkg.X#y()', 'aaaa1111')
		RETURNING node_id
	`, repoID).Scan(&nodeID)
	if err != nil {
		t.Fatalf("32-byte fingerprint insert should succeed: %v", err)
	}
	if nodeID == "" {
		t.Fatal("expected returned node_id to be non-empty")
	}
}

// TestEdge_fingerprintLengthCheck mirrors TestNode_fingerprintLengthCheck
// for the edge table; both rows in the structural graph carry the
// same 32-byte fingerprint invariant per G2 (tech-spec §8.7.1).
func TestEdge_fingerprintLengthCheck(t *testing.T) {
	db, _, cleanup := openTestDB(t)
	defer cleanup()
	mustUp(t, db)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()
	var repoID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO repo (url, default_branch, current_head_sha)
		VALUES ('https://example.test/repo', 'main', 'bbbb2222')
		RETURNING repo_id
	`).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	// Two valid nodes so the FK on edge can resolve.
	var srcID, dstID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO node (fingerprint, repo_id, kind, canonical_signature, from_sha)
		VALUES (decode('00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff', 'hex'),
		        $1, 'method', 'pkg.A#a()', 'bbbb2222')
		RETURNING node_id
	`, repoID).Scan(&srcID); err != nil {
		t.Fatalf("seed src node: %v", err)
	}
	if err := db.QueryRowContext(ctx, `
		INSERT INTO node (fingerprint, repo_id, kind, canonical_signature, from_sha)
		VALUES (decode('ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100', 'hex'),
		        $1, 'method', 'pkg.B#b()', 'bbbb2222')
		RETURNING node_id
	`, repoID).Scan(&dstID); err != nil {
		t.Fatalf("seed dst node: %v", err)
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO edge (fingerprint, repo_id, kind, src_node_id, dst_node_id, from_sha)
		VALUES (E'\\xAB', $1, 'static_calls', $2, $3, 'bbbb2222')
	`, repoID, srcID, dstID)
	if err == nil {
		t.Fatal("expected CHECK violation for 1-byte edge fingerprint; got nil")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "octet_length") && !strings.Contains(msg, "edge_fingerprint_octet_length_chk") {
		t.Errorf("CHECK error does not reference octet_length or the constraint name: %v", err)
	}
}

// TestRoundTrip_schemaIsByteIdenticalAfterDownUp is the
// "round-trip migration" scenario. Apply Up, capture a canonical
// fingerprint of the schema, apply Down, apply Up again, capture
// again, assert identical.
//
// We compute the canonical fingerprint from pg_catalog +
// information_schema instead of shelling to `pg_dump --schema-only`
// because (a) the latter requires the pg_dump binary on the
// runner host outside the container and (b) the catalog-driven
// query is itself byte-deterministic (sorted, normalized) which
// is what "byte-for-byte" really means here.
func TestRoundTrip_schemaIsByteIdenticalAfterDownUp(t *testing.T) {
	db, schema, cleanup := openTestDB(t)
	defer cleanup()
	m := migrations.New(db)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	if err := m.Up(ctx); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	before := canonicalSchema(t, db, schema)

	if err := m.Down(ctx); err != nil {
		t.Fatalf("Down: %v", err)
	}
	// After a complete down, the only object left should be the
	// migration journal. Spot-check by ensuring the marker
	// tables are gone.
	for _, tbl := range []string{"repo", "node", "edge", "ingest_jobs"} {
		if relationExists(t, db, schema, tbl) {
			t.Errorf("table %q still present after Down", tbl)
		}
	}

	if err := m.Up(ctx); err != nil {
		t.Fatalf("second Up: %v", err)
	}
	after := canonicalSchema(t, db, schema)

	if before != after {
		t.Errorf("schema canonical form drifted across round trip\n\nBEFORE:\n%s\n\nAFTER:\n%s",
			before, after)
	}
}

// TestDown_isIdempotent guards against the "Down twice" foot-gun
// (which can happen in CI retry loops). The second Down must
// succeed as a no-op.
func TestDown_isIdempotent(t *testing.T) {
	db, _, cleanup := openTestDB(t)
	defer cleanup()
	m := migrations.New(db)
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if err := m.Down(ctx); err != nil {
		t.Fatalf("first Down: %v", err)
	}
	if err := m.Down(ctx); err != nil {
		t.Fatalf("second Down should be a no-op, got: %v", err)
	}
	versions, err := m.AppliedVersions(ctx)
	if err != nil {
		t.Fatalf("AppliedVersions after second Down: %v", err)
	}
	if len(versions) != 0 {
		t.Errorf("journal not empty after Down: %v", versions)
	}
}

// TestObservation_checkRejectsMultiTarget exercises Stage 1.3
// scenario "Observation CHECK rejects multi-target" -- the table
// CHECK constraints on `observation` reject any row with more
// than one target column populated AND any row whose `role` does
// not match its target column.
func TestObservation_checkRejectsMultiTarget(t *testing.T) {
	db, _, cleanup := openTestDB(t)
	defer cleanup()
	mustUp(t, db)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	var repoID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO repo (url, default_branch, current_head_sha)
		VALUES ('https://example.test/observation', 'main', 'cccc3333')
		RETURNING repo_id
	`).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	var nodeID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO node (fingerprint, repo_id, kind, canonical_signature, from_sha)
		VALUES (decode('0101010101010101010101010101010101010101010101010101010101010101', 'hex'),
		        $1, 'method', 'pkg.Obs#hit()', 'cccc3333')
		RETURNING node_id
	`, repoID).Scan(&nodeID); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	// A synthetic episode_id -- Observation does NOT carry a real
	// FK to Episode (partitioned parent, see 0007 header), so we
	// can fabricate one without inserting the Episode row first.
	episodeID := "11111111-1111-1111-1111-111111111111"
	conceptIDStub := "22222222-2222-2222-2222-222222222222"

	// Case 1: BOTH node_id and concept_id set. Should trip the
	// exactly-one-target CHECK, regardless of `role`.
	_, err := db.ExecContext(ctx, `
		INSERT INTO observation (episode_id, role, node_id, concept_id)
		VALUES ($1, 'node_hit', $2, $3)
	`, episodeID, nodeID, conceptIDStub)
	if err == nil {
		t.Fatal("expected CHECK violation for multi-target observation; got nil")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "observation_exactly_one_target_chk") {
		t.Errorf("multi-target CHECK error should reference observation_exactly_one_target_chk: %v", err)
	}

	// Case 2: role/target mismatch -- role='node_hit' but target
	// is `concept_id`. exactly_one passes (count=1), but the
	// role-target pairing CHECK rejects.
	_, err = db.ExecContext(ctx, `
		INSERT INTO observation (episode_id, role, concept_id)
		VALUES ($1, 'node_hit', $2)
	`, episodeID, conceptIDStub)
	if err == nil {
		t.Fatal("expected CHECK violation for role/target mismatch; got nil")
	}
	msg = strings.ToLower(err.Error())
	if !strings.Contains(msg, "observation_role_target_chk") {
		t.Errorf("role-target CHECK error should reference observation_role_target_chk: %v", err)
	}

	// Sanity: a well-formed row (role='node_hit', node_id set,
	// nothing else) succeeds. Guards against the CHECKs over-
	// rejecting and the table looking dead.
	var obsID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO observation (episode_id, role, node_id, weight)
		VALUES ($1, 'node_hit', $2, 0.75)
		RETURNING observation_id
	`, episodeID, nodeID).Scan(&obsID); err != nil {
		t.Fatalf("well-formed observation insert should succeed: %v", err)
	}
	if obsID == "" {
		t.Fatal("expected returned observation_id to be non-empty")
	}
}

// TestSyntheticPositive_uniquenessAcrossRestarts exercises Stage
// 1.3 scenario "synthetic-positive uniqueness" -- the §9.8 risk
// mitigation. Two synthetic_positive Episode rows that share a
// `synthesized_from_feedback_episode_id` must collide on the
// `synthetic_positive_emission` sentinel PK, rolling back the
// second insert. Crucially, the second insert is forced into a
// LATER monthly partition (created_at = now() + 2 months) so the
// test exercises the cross-partition / cross-restart failure
// mode that motivated the sentinel-table substitute -- a literal
// partial UNIQUE on `episode` would have given only per-partition
// uniqueness and would NOT have rejected this case.
func TestSyntheticPositive_uniquenessAcrossRestarts(t *testing.T) {
	db, schema, cleanup := openTestDB(t)
	defer cleanup()
	mustUp(t, db)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	// Sentinel objects (substitute for the "partial UNIQUE on
	// partitioned episode" the implementation-plan literally
	// asks for) are catalogued so future maintainers trip over
	// a regression that removes the trigger or function.
	if !relationExists(t, db, schema, "synthetic_positive_emission") {
		t.Fatal("synthetic_positive_emission sentinel table missing after Up")
	}
	if !triggerExists(t, db, schema, "episode", "episode_synthetic_positive_sentinel") {
		t.Fatal("episode_synthetic_positive_sentinel trigger missing after Up")
	}
	if !functionExists(t, db, schema, "episode_synthetic_positive_sentinel") {
		t.Fatal("episode_synthetic_positive_sentinel trigger function missing after Up")
	}

	var repoID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO repo (url, default_branch, current_head_sha)
		VALUES ('https://example.test/synth-positive', 'main', 'dddd4444')
		RETURNING repo_id
	`).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	// The shared key the sentinel PK must reject. The
	// implementation-plan scenario phrasing is "two
	// Consolidator runs emit synthetic positives for the SAME
	// feedback episode".
	feedbackEpisodeID := "33333333-3333-3333-3333-333333333333"
	parentEpisodeID := "44444444-4444-4444-4444-444444444444"
	// synthetic_positive rows MUST carry a non-null context_id
	// per arch §5.3.1 (NULL legal only for `feedback` Episodes;
	// synthetic_positive copies the parent's context_id per G7).
	// Episode → RecallContextLog has no DB-level FK (partitioned
	// parent; see 0007 header), so a fabricated UUID is fine.
	contextID := "55555555-5555-5555-5555-555555555555"

	// First synthetic_positive: succeeds, drops a sentinel row.
	// created_at defaults to now() -- lands in the current
	// monthly partition.
	var firstEpisodeID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO episode (
			episode_group_id, repo_id, session_id, trace_id, kind,
			synthesized_from_parent_episode_id,
			synthesized_from_feedback_episode_id,
			context_id,
			action, outcome
		)
		VALUES (
			gen_random_uuid(), $1, 'sess-a', 'trace-a', 'synthetic_positive',
			$2, $3, $4, '{"op":"replay"}'::jsonb, 'success'
		)
		RETURNING episode_id
	`, repoID, parentEpisodeID, feedbackEpisodeID, contextID).Scan(&firstEpisodeID); err != nil {
		t.Fatalf("first synthetic_positive Episode insert should succeed: %v", err)
	}

	// The sentinel row exists with the shared key.
	var sentinelCount int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM synthetic_positive_emission
		WHERE synthesized_from_feedback_episode_id = $1
	`, feedbackEpisodeID).Scan(&sentinelCount); err != nil {
		t.Fatalf("sentinel count query: %v", err)
	}
	if sentinelCount != 1 {
		t.Fatalf("expected exactly 1 sentinel row after first insert; got %d", sentinelCount)
	}

	// Second synthetic_positive: same feedback_episode_id, fresh
	// trace + session, AND created_at = now() + 2 months so the
	// row targets a different monthly partition (provisioned by
	// pg_partman in 0014 with p_premake := 3). The trigger fires
	// on the new partition, hits the sentinel PK, and rolls back
	// the Episode insert. This is the literal §9.8 scenario
	// ("Consolidator restart in a later month").
	_, err := db.ExecContext(ctx, `
		INSERT INTO episode (
			episode_group_id, repo_id, session_id, trace_id, kind,
			synthesized_from_parent_episode_id,
			synthesized_from_feedback_episode_id,
			context_id,
			action, outcome, created_at
		)
		VALUES (
			gen_random_uuid(), $1, 'sess-b', 'trace-b', 'synthetic_positive',
			$2, $3, $4, '{"op":"replay"}'::jsonb, 'success',
			now() + interval '2 months'
		)
	`, repoID, parentEpisodeID, feedbackEpisodeID, contextID)
	if err == nil {
		t.Fatal("expected PK violation on second synthetic_positive insert; got nil")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "synthetic_positive_emission_pkey") &&
		!strings.Contains(msg, "synthesized_from_feedback_episode_id") {
		t.Errorf("duplicate sentinel error should reference synthetic_positive_emission_pkey or synthesized_from_feedback_episode_id: %v", err)
	}

	// The Episode INSERT was rolled back atomically with the
	// trigger's failed sentinel INSERT -- there should still be
	// exactly one Episode row carrying this feedback key (the
	// originally-inserted firstEpisodeID).
	var episodeCount int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM episode
		WHERE synthesized_from_feedback_episode_id = $1
	`, feedbackEpisodeID).Scan(&episodeCount); err != nil {
		t.Fatalf("episode count query: %v", err)
	}
	if episodeCount != 1 {
		t.Errorf("Episode row count should still be 1 after rejected duplicate; got %d", episodeCount)
	}
	if firstEpisodeID == "" {
		t.Error("first insert should have returned a non-empty episode_id")
	}
}

// TestPgPartman_provisionsForwardPartitions exercises Stage 1.3
// scenario "monthly partitions auto-provision" -- after 0014
// runs, each of the 5 partitioned tables is registered with
// pg_partman and carries at least `p_premake` (3) forward
// partitions beyond the user-created default partition.
func TestPgPartman_provisionsForwardPartitions(t *testing.T) {
	db, schema, cleanup := openTestDB(t)
	defer cleanup()
	mustUp(t, db)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	// 1. partman.part_config carries an entry for every
	//    partitioned parent we registered in 0014 -- AND ONLY
	//    those parents, in this test schema.
	expectedParents := []string{
		schema + ".trace_observation_log",
		schema + ".episode",
		schema + ".episode_update",
		schema + ".observation",
		schema + ".recall_context_log",
	}
	registered := map[string]bool{}
	// Escape `_` in the schema name: it's a LIKE wildcard
	// matching any single character, which would otherwise let
	// us see sibling-test schemas' rows. Same pattern as the
	// per-test cleanup DELETE.
	schemaPrefix := strings.ReplaceAll(schema, "_", "#_") + ".%"
	rows, err := db.QueryContext(ctx, `
		SELECT parent_table FROM partman.part_config
		WHERE parent_table LIKE $1 ESCAPE '#'
		ORDER BY parent_table
	`, schemaPrefix)
	if err != nil {
		t.Fatalf("part_config query: %v", err)
	}
	for rows.Next() {
		var pt string
		if err := rows.Scan(&pt); err != nil {
			rows.Close()
			t.Fatalf("scan part_config: %v", err)
		}
		registered[pt] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("part_config iter: %v", err)
	}
	rows.Close()
	for _, want := range expectedParents {
		if !registered[want] {
			t.Errorf("partman.part_config missing entry for %s", want)
		}
	}
	if got, want := len(registered), len(expectedParents); got != want {
		t.Errorf("part_config row count: got %d want %d (rows=%v)", got, want, registered)
	}

	// 2. Every registered parent has at least 4 children: 1
	//    current-period partition + 3 forward (p_premake := 3),
	//    plus the user-created default partition retained via
	//    `p_default_table := false`. Lower bound of 4 tolerates
	//    timezone/boundary edge-cases at month and week
	//    transitions; the architectural contract is the 3
	//    forward partitions.
	for _, parent := range expectedParents {
		var childCount int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*)
			FROM pg_inherits i
			JOIN pg_class c ON c.oid = i.inhparent
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname || '.' || c.relname = $1
		`, parent).Scan(&childCount); err != nil {
			t.Fatalf("child-partition count for %s: %v", parent, err)
		}
		if childCount < 4 {
			t.Errorf("pg_partman should provision >= 4 child partitions for %s (current + 3 premake + default); got %d", parent, childCount)
		}
	}

	// 3. The implementation-plan scenario is sharper than a
	//    child-count: it says "partition tables covering at
	//    least the next 3 months". Verify that the latest
	//    non-default child partition's FROM bound is at least
	//    p_premake periods in the future for each parent, which
	//    is what p_premake := 3 guarantees pg_partman provisioned.
	//
	//    The lower-bound threshold is `now() + (premake - 1)
	//    intervals`: with premake=3 we expect partitions at
	//    [current, current+1, current+2, current+3]; the latest
	//    FROM is current+3, which is strictly greater than
	//    now()+2 intervals regardless of where in the current
	//    period we are.
	now := time.Now().UTC()
	for _, parent := range expectedParents {
		var threshold time.Time
		switch parent {
		case schema + ".trace_observation_log":
			// 2 weeks ahead — the latest FROM should be ~3 weeks ahead.
			threshold = now.Add(14 * 24 * time.Hour)
		default:
			// 2 months ahead — the latest FROM should be ~3 months ahead.
			threshold = now.AddDate(0, 2, 0)
		}
		var maxFrom sql.NullTime
		if err := db.QueryRowContext(ctx, `
			SELECT max(
				(regexp_match(
					pg_get_expr(c.relpartbound, c.oid),
					'FROM \(''([^'']+)''\)'
				))[1]::timestamptz
			)
			FROM pg_inherits i
			JOIN pg_class c ON c.oid = i.inhrelid
			JOIN pg_class p ON p.oid = i.inhparent
			JOIN pg_namespace n ON n.oid = p.relnamespace
			WHERE n.nspname || '.' || p.relname = $1
			  AND pg_get_expr(c.relpartbound, c.oid) <> 'DEFAULT'
		`, parent).Scan(&maxFrom); err != nil {
			t.Fatalf("max-FROM-bound query for %s: %v", parent, err)
		}
		if !maxFrom.Valid {
			t.Errorf("no dated partitions found for %s -- pg_partman did not provision forward partitions", parent)
			continue
		}
		if !maxFrom.Time.After(threshold) {
			t.Errorf("pg_partman should provision partitions covering at least 3 forward periods for %s; latest FROM = %s, threshold (now + 2 intervals) = %s",
				parent, maxFrom.Time, threshold)
		}
	}
}

// ------------------------------------------------------------------
// Helpers
// ------------------------------------------------------------------

func mustUp(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()
	if err := migrations.New(db).Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
}

func relationExists(t *testing.T, db *sql.DB, schema, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()
	var exists bool
	err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1
			  AND c.relname = $2
			  AND c.relkind IN ('r', 'p')
		)`, schema, name).Scan(&exists)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("relationExists(%s.%s): %v", schema, name, err)
	}
	return exists
}

func enumExists(t *testing.T, db *sql.DB, schema, name string) bool {
	t.Helper()
	return enumLabels(t, db, schema, name) != nil
}

// enumLabels returns the labels of a named ENUM in enumsortorder
// or nil if the type does not exist. Used by the structural-
// schema test to detect closed-set drift, not just absence.
func enumLabels(t *testing.T, db *sql.DB, schema, name string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()
	rows, err := db.QueryContext(ctx, `
		SELECT e.enumlabel
		FROM pg_type t
		JOIN pg_namespace n ON n.oid = t.typnamespace
		JOIN pg_enum e ON e.enumtypid = t.oid
		WHERE n.nspname = $1
		  AND t.typname = $2
		  AND t.typtype = 'e'
		ORDER BY e.enumsortorder
	`, schema, name)
	if err != nil {
		t.Fatalf("enumLabels(%s.%s): %v", schema, name, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var lbl string
		if err := rows.Scan(&lbl); err != nil {
			t.Fatalf("scan enum label: %v", err)
		}
		out = append(out, lbl)
	}
	return out
}

// indexDef returns the canonical CREATE INDEX statement for a
// given index name in the target schema, or "" if not found.
// Used to assert that an index's columns / predicate / uniqueness
// haven't quietly drifted -- name-only assertions miss that.
//
// `relkind IN ('i', 'I')` covers both ordinary indices (`i`) and
// indices on partitioned parents (`I`). The Stage 1.3 episodic
// tables are partitioned, so several of the contractual indices
// live on partitioned parents and would be invisible if we
// filtered on `i` alone.
func indexDef(t *testing.T, db *sql.DB, schema, name string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()
	var def sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT pg_get_indexdef(c.oid)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('i', 'I')
	`, schema, name).Scan(&def)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("indexDef(%s.%s): %v", schema, name, err)
	}
	return def.String
}

// triggerExists reports whether a named trigger is defined on a
// given table in the target schema. Used to assert the sentinel
// substitute for the (PostgreSQL-impossible) partial UNIQUE on
// partitioned `episode`.
func triggerExists(t *testing.T, db *sql.DB, schema, table, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()
	var exists bool
	err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_trigger tr
			JOIN pg_class c     ON c.oid = tr.tgrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1
			  AND c.relname = $2
			  AND tr.tgname = $3
			  AND NOT tr.tgisinternal
		)`, schema, table, name).Scan(&exists)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("triggerExists(%s.%s.%s): %v", schema, table, name, err)
	}
	return exists
}

// functionExists reports whether a function with the given name
// is defined in the target schema. Used to catch a regression
// that drops the sentinel trigger function and breaks the
// synthetic-positive uniqueness substitute.
func functionExists(t *testing.T, db *sql.DB, schema, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()
	var exists bool
	err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_proc p
			JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = $1 AND p.proname = $2
		)`, schema, name).Scan(&exists)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("functionExists(%s.%s): %v", schema, name, err)
	}
	return exists
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func uniqueIndexExists(t *testing.T, db *sql.DB, schema, table, index string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()
	var exists bool
	err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_index i
			JOIN pg_class c ON c.oid = i.indexrelid
			JOIN pg_class t ON t.oid = i.indrelid
			JOIN pg_namespace n ON n.oid = t.relnamespace
			WHERE n.nspname = $1
			  AND t.relname = $2
			  AND c.relname = $3
			  AND i.indisunique
		)`, schema, table, index).Scan(&exists)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("uniqueIndexExists(%s.%s.%s): %v", schema, table, index, err)
	}
	return exists
}

// _ keeps the unused helpers compile-checked alongside the
// stronger indexDef / enumLabels assertions so future test
// authors have ready-made existence probes available without
// re-deriving them. Removing this _ usage if you drop the
// helpers entirely is fine.
var _ = []any{enumExists, uniqueIndexExists}

// canonicalSchema produces a deterministic text fingerprint of
// every object the migrations create in the target schema. The
// fingerprint is constructed from sorted catalog queries so
// running it twice on the same schema returns the same string
// byte-for-byte. This is what the "round-trip" scenario asserts.
func canonicalSchema(t *testing.T, db *sql.DB, schema string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*testDBTimeout)
	defer cancel()
	var out strings.Builder

	// 1) ENUM types and their members (in enumsortorder).
	out.WriteString("# ENUMS\n")
	rows, err := db.QueryContext(ctx, `
		SELECT t.typname, string_agg(e.enumlabel, ',' ORDER BY e.enumsortorder)
		FROM pg_type t
		JOIN pg_namespace n ON n.oid = t.typnamespace
		JOIN pg_enum e ON e.enumtypid = t.oid
		WHERE n.nspname = $1
		GROUP BY t.typname
		ORDER BY t.typname
	`, schema)
	if err != nil {
		t.Fatalf("canonicalSchema enums: %v", err)
	}
	for rows.Next() {
		var name, members string
		if err := rows.Scan(&name, &members); err != nil {
			rows.Close()
			t.Fatalf("scan enums: %v", err)
		}
		fmt.Fprintf(&out, "  %s(%s)\n", name, members)
	}
	rows.Close()

	// 2) Tables + columns (with type, nullable, default).
	out.WriteString("# TABLES\n")
	rows, err = db.QueryContext(ctx, `
		SELECT c.relname,
		       c.relkind,
		       a.attname,
		       format_type(a.atttypid, a.atttypmod) AS coltype,
		       a.attnotnull,
		       COALESCE(pg_get_expr(d.adbin, d.adrelid), '') AS coldef
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE n.nspname = $1
		  AND c.relkind IN ('r', 'p')
		  AND a.attnum > 0
		  AND NOT a.attisdropped
		ORDER BY c.relname, a.attnum
	`, schema)
	if err != nil {
		t.Fatalf("canonicalSchema tables: %v", err)
	}
	for rows.Next() {
		var tbl, kind, col, colType, colDef string
		var notNull bool
		if err := rows.Scan(&tbl, &kind, &col, &colType, &notNull, &colDef); err != nil {
			rows.Close()
			t.Fatalf("scan tables: %v", err)
		}
		fmt.Fprintf(&out, "  %s[%s] %s %s notnull=%t default=%q\n",
			tbl, kind, col, colType, notNull, colDef)
	}
	rows.Close()

	// 3) Constraints (PRIMARY KEY, FK, UNIQUE, CHECK).
	out.WriteString("# CONSTRAINTS\n")
	rows, err = db.QueryContext(ctx, `
		SELECT c.relname,
		       con.conname,
		       con.contype,
		       pg_get_constraintdef(con.oid, true) AS def
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		ORDER BY c.relname, con.conname
	`, schema)
	if err != nil {
		t.Fatalf("canonicalSchema constraints: %v", err)
	}
	for rows.Next() {
		var tbl, name, def string
		var ctype byte
		if err := rows.Scan(&tbl, &name, &ctype, &def); err != nil {
			rows.Close()
			t.Fatalf("scan constraints: %v", err)
		}
		fmt.Fprintf(&out, "  %s.%s [%c] %s\n", tbl, name, ctype, def)
	}
	rows.Close()

	// 4) Indices (full DDL via pg_get_indexdef so partial /
	//    expression / unique flags all round-trip).
	out.WriteString("# INDICES\n")
	rows, err = db.QueryContext(ctx, `
		SELECT t.relname, c.relname AS index_name, pg_get_indexdef(i.indexrelid)
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_class t ON t.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE n.nspname = $1
		  AND NOT EXISTS (
		      SELECT 1 FROM pg_constraint con
		      WHERE con.conindid = i.indexrelid
		  )
		ORDER BY t.relname, c.relname
	`, schema)
	if err != nil {
		t.Fatalf("canonicalSchema indices: %v", err)
	}
	for rows.Next() {
		var tbl, idx, def string
		if err := rows.Scan(&tbl, &idx, &def); err != nil {
			rows.Close()
			t.Fatalf("scan indices: %v", err)
		}
		// Strip the schema-qualifier so the canonical text is
		// stable across two schemas with the same logical
		// content (the round-trip test reuses the same schema,
		// but we keep this defensive for future cross-schema
		// diffs). PostgreSQL emits the bare schema name (not
		// quoted) when the identifier needs no quoting.
		def = strings.ReplaceAll(def, schema+".", "")
		fmt.Fprintf(&out, "  %s.%s %s\n", tbl, idx, def)
	}
	rows.Close()

	// 5) Partitioning: parent partition key + every child's
	//    partition bound expression. Without this section the
	//    round-trip test would miss drift in the partitioned
	//    trace_observation_log -- e.g. someone changing it from
	//    RANGE(started_at) to RANGE(created_at), or removing the
	//    DEFAULT partition we use as a bootstrap before pg_partman.
	out.WriteString("# PARTITION_KEYS\n")
	rows, err = db.QueryContext(ctx, `
		SELECT c.relname, pg_get_partkeydef(c.oid)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind = 'p'
		ORDER BY c.relname
	`, schema)
	if err != nil {
		t.Fatalf("canonicalSchema partition keys: %v", err)
	}
	for rows.Next() {
		var parent, def string
		if err := rows.Scan(&parent, &def); err != nil {
			rows.Close()
			t.Fatalf("scan partition keys: %v", err)
		}
		fmt.Fprintf(&out, "  %s %s\n", parent, def)
	}
	rows.Close()

	out.WriteString("# PARTITION_CHILDREN\n")
	rows, err = db.QueryContext(ctx, `
		SELECT parent.relname,
		       child.relname,
		       COALESCE(pg_get_expr(child.relpartbound, child.oid), '') AS bound
		FROM pg_inherits inh
		JOIN pg_class child ON child.oid = inh.inhrelid
		JOIN pg_class parent ON parent.oid = inh.inhparent
		JOIN pg_namespace n ON n.oid = child.relnamespace
		WHERE n.nspname = $1
		ORDER BY parent.relname, child.relname
	`, schema)
	if err != nil {
		t.Fatalf("canonicalSchema partition children: %v", err)
	}
	for rows.Next() {
		var parent, child, bound string
		if err := rows.Scan(&parent, &child, &bound); err != nil {
			rows.Close()
			t.Fatalf("scan partition children: %v", err)
		}
		fmt.Fprintf(&out, "  %s -> %s %s\n", parent, child, bound)
	}
	rows.Close()

	return out.String()
}
