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

	"github.com/lib/pq" // *pq.Error gives us SQLSTATE + Constraint name

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
		// Stage 1.3 step 7: composite partial UNIQUE that gates
		// duplicate synthetic_positive emissions for the same
		// feedback Episode (the §9.8 mitigation, per the
		// operator's iteration-2 directive on 0013). The
		// partition key (`created_at`) is required by PostgreSQL
		// on UNIQUE indexes over partitioned tables; the
		// architectural intent (cross-restart single-emission)
		// is owned by the Consolidator's app-layer ledger
		// (Stage 5.4).
		{"episode_synthetic_positive_feedback_uidx", "UNIQUE INDEX episode_synthetic_positive_feedback_uidx ON"},
		{"episode_synthetic_positive_feedback_uidx", "(synthesized_from_feedback_episode_id, created_at)"},
		{"episode_synthetic_positive_feedback_uidx", "WHERE (kind = 'synthetic_positive'"},
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

// TestSyntheticPositive_compositePartialUniqueRejectsSameKey
// exercises Stage 1.3 scenario "synthetic-positive uniqueness"
// against the iteration-2 implementation: a composite partial
// UNIQUE index on `(synthesized_from_feedback_episode_id,
// created_at) WHERE kind='synthetic_positive'`. Two
// synthetic_positive rows that share BOTH columns (e.g. emitted
// in the same transaction, where `now()` is fixed) collide on
// the unique index and the second insert is rejected.
//
// The composite shape is forced on us by PostgreSQL's rule that
// every UNIQUE column on a partitioned table must include the
// partition key. The operator (Stage 1.3 iter 2) chose this
// shape over the previous sentinel-table-and-trigger workaround;
// see 0013_synthetic_positive_unique.sql for the contract notes.
//
// Note on the constraint-name assertion: PostgreSQL surfaces the
// CHILD partition's autogenerated unique-index name in the error
// (e.g. `episode_p2026_05_<col1>_<col2>_idx`), not the parent
// index name declared in 0013. We therefore (a) assert SQLSTATE
// 23505 strictly, (b) catalog-verify via `pg_inherits` that the
// reported index is either the parent itself OR a partition
// child attached to that parent, and (c) re-check the parent
// index definition. The combination proves the violation came
// from THIS unique index without coupling to the autogenerated
// child-index naming scheme.
func TestSyntheticPositive_compositePartialUniqueRejectsSameKey(t *testing.T) {
	db, schema, cleanup := openTestDB(t)
	defer cleanup()
	mustUp(t, db)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	var repoID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO repo (url, default_branch, current_head_sha)
		VALUES ('https://example.test/synth-positive', 'main', 'dddd4444')
		RETURNING repo_id
	`).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	// The shared key the unique index must reject. The
	// implementation-plan scenario phrasing is "two
	// Consolidator runs emit synthetic positives for the SAME
	// feedback episode".
	feedbackEpisodeID := "33333333-3333-3333-3333-333333333333"
	parentEpisodeID := "44444444-4444-4444-4444-444444444444"
	// synthetic_positive rows MUST carry a non-null context_id
	// per arch §5.3.1 (NULL legal only for feedback Episodes;
	// synthetic_positive copies the parent's context_id per G7).
	// Episode → RecallContextLog has no DB-level FK (partitioned
	// parent; see 0007 header), so a fabricated UUID is fine.
	contextID := "55555555-5555-5555-5555-555555555555"

	// Pin both inserts to the SAME explicit created_at. This is
	// the conflict the composite partial UNIQUE rejects:
	// same `(synthesized_from_feedback_episode_id, created_at)`
	// pair under the `kind='synthetic_positive'` predicate.
	// Inside a single transaction `now()` is constant, so two
	// inserts from the same Consolidator tick land on identical
	// timestamps even without an explicit value.
	sharedTS := time.Now().UTC().Truncate(time.Microsecond)

	// First synthetic_positive: succeeds.
	var firstEpisodeID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO episode (
			episode_group_id, repo_id, session_id, trace_id, kind,
			synthesized_from_parent_episode_id,
			synthesized_from_feedback_episode_id,
			context_id,
			action, outcome, created_at
		)
		VALUES (
			gen_random_uuid(), $1, 'sess-a', 'trace-a', 'synthetic_positive',
			$2, $3, $4, '{"op":"replay"}'::jsonb, 'success', $5
		)
		RETURNING episode_id
	`, repoID, parentEpisodeID, feedbackEpisodeID, contextID, sharedTS).Scan(&firstEpisodeID); err != nil {
		t.Fatalf("first synthetic_positive Episode insert should succeed: %v", err)
	}
	if firstEpisodeID == "" {
		t.Fatal("first insert should have returned a non-empty episode_id")
	}

	// Second synthetic_positive: same feedback_episode_id AND
	// same created_at, fresh trace + session. The composite
	// partial UNIQUE rejects it.
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
			$2, $3, $4, '{"op":"replay"}'::jsonb, 'success', $5
		)
	`, repoID, parentEpisodeID, feedbackEpisodeID, contextID, sharedTS)

	// Assertion 1: SQLSTATE 23505 (unique_violation). Strict
	// match -- a CHECK violation (23514) or any other error here
	// would mean the unique index is missing or the wrong
	// constraint fired first.
	pqErr := assertPQErrCode(t, err, "23505")

	// Assertion 2: the reported constraint is either the parent
	// index name OR a partition child index attached to it via
	// `pg_inherits`. This is the catalog-backed equivalent of an
	// exact-name match that survives partman-routed inserts.
	assertUniqueViolationFromParentOrChild(t, db, schema,
		"episode_synthetic_positive_feedback_uidx", pqErr)

	// Assertion 3: the parent partial UNIQUE has the expected
	// shape. A future PR that drops or rewrites this index would
	// regress here. Combined with assertion 2, this proves the
	// violation came from the index we declared in 0013.
	def := indexDef(t, db, schema, "episode_synthetic_positive_feedback_uidx")
	if !strings.Contains(def, "synthesized_from_feedback_episode_id, created_at") {
		t.Errorf("parent unique index columns regressed: %s", def)
	}
	if !strings.Contains(def, "kind = 'synthetic_positive'") {
		t.Errorf("parent unique index WHERE clause regressed: %s", def)
	}

	// The first row is still there; the second was rolled back.
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
}

// TestSyntheticPositive_compositePartialUniqueAllowsDifferentCreatedAt
// documents the DELIBERATE limitation of the composite partial
// UNIQUE chosen in Stage 1.3 iter 2: two synthetic_positive
// rows that share a `synthesized_from_feedback_episode_id` but
// differ in `created_at` BOTH land, even within the same monthly
// partition. The DB-level index is not a cross-time gate; the
// authoritative cross-restart single-emission contract is owned
// by the Consolidator's app-layer ledger (Stage 5.4 / §7.7).
//
// This test is the "negative" companion to the
// composite-partial-unique-rejects-same-key test. If a future
// PR tightens the index (e.g. drops the `created_at` column
// somehow), this test fails loudly so the tightening is
// intentional and not an accident.
func TestSyntheticPositive_compositePartialUniqueAllowsDifferentCreatedAt(t *testing.T) {
	db, _, cleanup := openTestDB(t)
	defer cleanup()
	mustUp(t, db)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	var repoID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO repo (url, default_branch, current_head_sha)
		VALUES ('https://example.test/synth-positive-2', 'main', 'eeee5555')
		RETURNING repo_id
	`).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	feedbackEpisodeID := "66666666-6666-6666-6666-666666666666"
	parentEpisodeID := "77777777-7777-7777-7777-777777777777"
	contextID := "88888888-8888-8888-8888-888888888888"

	baseTS := time.Now().UTC().Truncate(time.Microsecond)

	// First insert at baseTS.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO episode (
			episode_group_id, repo_id, session_id, trace_id, kind,
			synthesized_from_parent_episode_id,
			synthesized_from_feedback_episode_id,
			context_id,
			action, outcome, created_at
		)
		VALUES (
			gen_random_uuid(), $1, 'sess-a', 'trace-a', 'synthetic_positive',
			$2, $3, $4, '{"op":"replay"}'::jsonb, 'success', $5
		)
	`, repoID, parentEpisodeID, feedbackEpisodeID, contextID, baseTS); err != nil {
		t.Fatalf("first synthetic_positive insert should succeed: %v", err)
	}

	// Second insert: same feedback id, different created_at
	// (one millisecond later -- still inside the same monthly
	// partition). Per the operator's directive this must
	// SUCCEED at the DB layer; cross-time deduplication is
	// owned by Stage 5.4.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO episode (
			episode_group_id, repo_id, session_id, trace_id, kind,
			synthesized_from_parent_episode_id,
			synthesized_from_feedback_episode_id,
			context_id,
			action, outcome, created_at
		)
		VALUES (
			gen_random_uuid(), $1, 'sess-b', 'trace-b', 'synthetic_positive',
			$2, $3, $4, '{"op":"replay"}'::jsonb, 'success', $5
		)
	`, repoID, parentEpisodeID, feedbackEpisodeID, contextID, baseTS.Add(time.Millisecond)); err != nil {
		t.Fatalf("second synthetic_positive insert with different created_at should SUCCEED at DB layer (operator-directed limitation); got: %v", err)
	}

	// Both rows are present -- confirming the gate is exact-key,
	// not per-(feedback_id) anywhere across created_at values.
	var episodeCount int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM episode
		WHERE synthesized_from_feedback_episode_id = $1
	`, feedbackEpisodeID).Scan(&episodeCount); err != nil {
		t.Fatalf("episode count query: %v", err)
	}
	if episodeCount != 2 {
		t.Errorf("expected both synthetic_positive rows to land (composite UNIQUE allows different created_at); got %d", episodeCount)
	}
}

// TestEpisode_provenanceCheckConstraints exercises the
// architecture-level field-table invariants encoded by the
// CHECK constraints in 0007_episode.sql (arch §5.3.1). Each
// subcase is one row of the (kind, field) truth table; the test
// asserts SQLSTATE and `constraint name` rather than message
// substring so a Postgres locale change cannot silently weaken
// the assertion.
//
// Constraints covered:
//   - episode_synthesized_from_parent_provenance_chk
//   - episode_synthesized_from_feedback_provenance_chk
//   - episode_parent_episode_id_provenance_chk
//   - episode_context_id_required_unless_feedback_chk
func TestEpisode_provenanceCheckConstraints(t *testing.T) {
	db, _, cleanup := openTestDB(t)
	defer cleanup()
	mustUp(t, db)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	var repoID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO repo (url, default_branch, current_head_sha)
		VALUES ('https://example.test/provenance', 'main', 'aaaa9999')
		RETURNING repo_id
	`).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	// Stable UUID literals so case-level intent is readable.
	const (
		parentUUID   = "00000000-0000-0000-0000-0000000000aa"
		feedbackUUID = "00000000-0000-0000-0000-0000000000bb"
		contextUUID  = "00000000-0000-0000-0000-0000000000cc"
		parentForFB  = "00000000-0000-0000-0000-0000000000dd"
		synthParUUID = "00000000-0000-0000-0000-0000000000ee"
		synthFBUUID  = "00000000-0000-0000-0000-0000000000ff"
		synthCtxUUID = "00000000-0000-0000-0000-00000000a000"
		extraParUUID = "00000000-0000-0000-0000-00000000b000"
		extraFBUUID  = "00000000-0000-0000-0000-00000000c000"
	)

	// Each case below builds a full INSERT and asserts whether
	// it should be rejected (and by which CHECK) or accepted.
	// `args` are the same ten columns in every case so the SQL
	// stays identical, which avoids accidental SQL drift between
	// case definitions.
	//
	// `corrected_action` is in the column list because
	// `episode_corrected_action_chk` enforces
	// `outcome='human_corrected' IFF corrected_action IS NOT NULL`.
	// If we omitted it, every `human_corrected` row would fail
	// that unrelated CHECK first and the test would assert the
	// wrong constraint name (or, for rows that are SUPPOSED to
	// succeed, the test would fail outright).
	const insertSQL = `
		INSERT INTO episode (
			episode_group_id, repo_id, session_id, trace_id, kind,
			parent_episode_id,
			synthesized_from_parent_episode_id,
			synthesized_from_feedback_episode_id,
			context_id,
			action, outcome, corrected_action
		)
		VALUES (
			gen_random_uuid(), $1, $2, $3, $4::episode_kind,
			$5, $6, $7, $8,
			'{"op":"x"}'::jsonb, $9::outcome, $10::jsonb
		)
	`

	// Useful pointer helpers -- pq is happy with sql.Null* or
	// raw strings, but mixing typed nulls and strings inside
	// $args is fiddly. We pass pointer-style: nil means SQL NULL.
	str := func(s string) any { return s }
	null := func() any { return nil }

	// Convenience: every `human_corrected` row needs a non-null
	// corrected_action (or it trips the unrelated
	// `episode_corrected_action_chk`). Centralised here so a
	// future re-shape of the JSON shape only edits one place.
	correctedActionJSON := str(`{"op":"corrected"}`)

	cases := []struct {
		name            string
		kind            string
		session         string
		trace           string
		parent          any // parent_episode_id
		synthPar        any // synthesized_from_parent_episode_id
		synthFB         any // synthesized_from_feedback_episode_id
		ctx             any // context_id
		outcome         string
		correctedAction any // corrected_action; nil unless outcome=human_corrected (or testing corrected_action_chk)
		wantOK          bool
		wantSQLState    string // 23514 for CHECK violation
		wantConstr      string // exact constraint name
	}{
		// === REJECTED cases ===

		{
			name:    "synthetic_positive missing synth_parent",
			kind:    "synthetic_positive",
			session: "s1", trace: "t1",
			parent:          null(),
			synthPar:        null(), // <-- missing
			synthFB:         str(synthFBUUID),
			ctx:             str(synthCtxUUID),
			outcome:         "success",
			correctedAction: null(),
			wantSQLState:    "23514",
			wantConstr:      "episode_synthesized_from_parent_provenance_chk",
		},
		{
			name:    "synthetic_positive missing synth_feedback",
			kind:    "synthetic_positive",
			session: "s2", trace: "t2",
			parent:          null(),
			synthPar:        str(synthParUUID),
			synthFB:         null(), // <-- missing
			ctx:             str(synthCtxUUID),
			outcome:         "success",
			correctedAction: null(),
			wantSQLState:    "23514",
			wantConstr:      "episode_synthesized_from_feedback_provenance_chk",
		},
		{
			name:    "synthetic_positive missing context_id",
			kind:    "synthetic_positive",
			session: "s3", trace: "t3",
			parent:          null(),
			synthPar:        str(synthParUUID),
			synthFB:         str(synthFBUUID),
			ctx:             null(), // <-- missing
			outcome:         "success",
			correctedAction: null(),
			wantSQLState:    "23514",
			wantConstr:      "episode_context_id_required_unless_feedback_chk",
		},
		{
			name:    "agent row with synth_parent set",
			kind:    "agent",
			session: "s4", trace: "t4",
			parent:          null(),
			synthPar:        str(parentUUID), // <-- forbidden on non-synthetic_positive
			synthFB:         null(),
			ctx:             str(contextUUID),
			outcome:         "success",
			correctedAction: null(),
			wantSQLState:    "23514",
			wantConstr:      "episode_synthesized_from_parent_provenance_chk",
		},
		{
			name:    "agent row with synth_feedback set",
			kind:    "agent",
			session: "s5", trace: "t5",
			parent:          null(),
			synthPar:        null(),
			synthFB:         str(feedbackUUID), // <-- forbidden on non-synthetic_positive
			ctx:             str(contextUUID),
			outcome:         "success",
			correctedAction: null(),
			wantSQLState:    "23514",
			wantConstr:      "episode_synthesized_from_feedback_provenance_chk",
		},
		{
			name:    "agent row with parent_episode_id set",
			kind:    "agent",
			session: "s6", trace: "t6",
			parent:          str(extraParUUID), // <-- forbidden on non-feedback
			synthPar:        null(),
			synthFB:         null(),
			ctx:             str(contextUUID),
			outcome:         "success",
			correctedAction: null(),
			wantSQLState:    "23514",
			wantConstr:      "episode_parent_episode_id_provenance_chk",
		},
		{
			name:    "feedback row without parent_episode_id",
			kind:    "feedback",
			session: "s7", trace: "t7",
			parent:          null(), // <-- REQUIRED on feedback
			synthPar:        null(),
			synthFB:         null(),
			ctx:             null(),
			outcome:         "human_corrected",
			correctedAction: correctedActionJSON, // satisfies corrected_action_chk so the missing-parent CHECK is the SOLE failure
			wantSQLState:    "23514",
			wantConstr:      "episode_parent_episode_id_provenance_chk",
		},
		{
			name:    "feedback row with synth fields set",
			kind:    "feedback",
			session: "s8", trace: "t8",
			parent:          str(parentForFB),
			synthPar:        str(synthParUUID), // <-- forbidden on feedback
			synthFB:         null(),
			ctx:             null(),
			outcome:         "human_corrected",
			correctedAction: correctedActionJSON,
			wantSQLState:    "23514",
			wantConstr:      "episode_synthesized_from_parent_provenance_chk",
		},
		{
			name:    "synthetic_positive with parent_episode_id set",
			kind:    "synthetic_positive",
			session: "s9", trace: "t9",
			parent:          str(extraFBUUID), // <-- forbidden on non-feedback
			synthPar:        str(synthParUUID),
			synthFB:         str(synthFBUUID),
			ctx:             str(synthCtxUUID),
			outcome:         "success",
			correctedAction: null(),
			wantSQLState:    "23514",
			wantConstr:      "episode_parent_episode_id_provenance_chk",
		},

		// === episode_corrected_action_chk coverage. These rows
		// are well-formed on every OTHER provenance dimension, so
		// the corrected_action CHECK is the SOLE failure.
		{
			name:    "human_corrected outcome without corrected_action",
			kind:    "feedback",
			session: "s10", trace: "t10",
			parent:          str(parentForFB),
			synthPar:        null(),
			synthFB:         null(),
			ctx:             null(),
			outcome:         "human_corrected",
			correctedAction: null(), // <-- forbidden when outcome=human_corrected
			wantSQLState:    "23514",
			wantConstr:      "episode_corrected_action_chk",
		},
		{
			name:    "non-human_corrected outcome with corrected_action",
			kind:    "agent",
			session: "s11", trace: "t11",
			parent:          null(),
			synthPar:        null(),
			synthFB:         null(),
			ctx:             str(contextUUID),
			outcome:         "success",
			correctedAction: correctedActionJSON, // <-- forbidden when outcome<>human_corrected
			wantSQLState:    "23514",
			wantConstr:      "episode_corrected_action_chk",
		},

		// === SANITY: well-formed rows MUST succeed. Guards the
		// CHECKs from over-rejecting and leaving the table dead.

		{
			name:    "valid agent row",
			kind:    "agent",
			session: "ok-agent", trace: "trace-ok-agent",
			parent:          null(),
			synthPar:        null(),
			synthFB:         null(),
			ctx:             str(contextUUID),
			outcome:         "success",
			correctedAction: null(),
			wantOK:          true,
		},
		{
			name:    "valid feedback row (context_id NULL)",
			kind:    "feedback",
			session: "ok-fb-1", trace: "trace-ok-fb-1",
			parent:          str(parentForFB),
			synthPar:        null(),
			synthFB:         null(),
			ctx:             null(), // explicitly NULL -- the feedback exception
			outcome:         "human_corrected",
			correctedAction: correctedActionJSON, // outcome=human_corrected requires corrected_action
			wantOK:          true,
		},
		{
			name:    "valid feedback row (context_id non-NULL allowed)",
			kind:    "feedback",
			session: "ok-fb-2", trace: "trace-ok-fb-2",
			parent:          str(parentForFB),
			synthPar:        null(),
			synthFB:         null(),
			ctx:             str(contextUUID), // one-directional: feedback MAY carry context_id
			outcome:         "human_corrected",
			correctedAction: correctedActionJSON,
			wantOK:          true,
		},
		{
			name:    "valid synthetic_positive row",
			kind:    "synthetic_positive",
			session: "ok-syn", trace: "trace-ok-syn",
			parent:          null(),
			synthPar:        str(synthParUUID),
			synthFB:         str("00000000-0000-0000-0000-00000000d000"),
			ctx:             str(synthCtxUUID),
			outcome:         "success",
			correctedAction: null(),
			wantOK:          true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, insertSQL,
				repoID, tc.session, tc.trace, tc.kind,
				tc.parent, tc.synthPar, tc.synthFB, tc.ctx,
				tc.outcome, tc.correctedAction,
			)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("insert should succeed: %v", err)
				}
				return
			}
			assertPQViolation(t, err, tc.wantSQLState, tc.wantConstr)
		})
	}
}

// TestSyntheticPositive_uniquenessAcrossRestarts is retained as
// a thin shim that delegates to the iteration-2 implementation
// tests so callers searching for the historical scenario name
// (implementation-plan.md "synthetic-positive uniqueness") still
// find a matching test entry. The actual assertions live in
// TestSyntheticPositive_compositePartialUniqueRejectsSameKey
// (the rejection side) and
// TestSyntheticPositive_compositePartialUniqueAllowsDifferentCreatedAt
// (the limitation documentation). We keep this alias so future
// renames are caught explicitly rather than silently dropped.
func TestSyntheticPositive_uniquenessAcrossRestarts(t *testing.T) {
	t.Run("rejects_same_key", TestSyntheticPositive_compositePartialUniqueRejectsSameKey)
	t.Run("allows_different_created_at", TestSyntheticPositive_compositePartialUniqueAllowsDifferentCreatedAt)
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

// assertPQViolation asserts the supplied error is a *pq.Error
// with the expected SQLSTATE and constraint name. Asserting on
// SQLSTATE + Constraint is strictly stronger than message-
// substring matching: it survives Postgres locale changes,
// driver wrapping, and lower-cased message normalisation.
func assertPQViolation(t *testing.T, err error, wantSQLState, wantConstraint string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected SQLSTATE=%s constraint=%s; got nil error",
			wantSQLState, wantConstraint)
	}
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		t.Fatalf("expected *pq.Error, got %T: %v", err, err)
	}
	if string(pqErr.Code) != wantSQLState {
		t.Errorf("SQLSTATE = %q, want %q (msg: %v)",
			string(pqErr.Code), wantSQLState, err)
	}
	if pqErr.Constraint != wantConstraint {
		t.Errorf("Constraint = %q, want %q (msg: %v)",
			pqErr.Constraint, wantConstraint, err)
	}
}

// assertPQErrCode is the lighter-weight cousin of
// assertPQViolation: it asserts the supplied error is a
// *pq.Error with the expected SQLSTATE and returns it so the
// caller can do further inspection (Constraint, Table, Schema).
// Used for unique-violation tests on partitioned tables where
// the reported Constraint name belongs to the CHILD partition's
// autogenerated index and an exact-name assertion is brittle;
// the caller follows up with
// assertUniqueViolationFromParentOrChild for catalog-backed
// provenance.
func assertPQErrCode(t *testing.T, err error, wantSQLState string) *pq.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected SQLSTATE=%s; got nil error", wantSQLState)
	}
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		t.Fatalf("expected *pq.Error, got %T: %v", err, err)
	}
	if string(pqErr.Code) != wantSQLState {
		t.Fatalf("SQLSTATE = %q, want %q (msg: %v)",
			string(pqErr.Code), wantSQLState, err)
	}
	return pqErr
}

// assertUniqueViolationFromParentOrChild asserts that the index
// reported on `pqErr.Constraint` is either the named parent
// partial-UNIQUE index, OR a partition child index attached to
// that parent via `pg_inherits`. This is the catalog-backed
// equivalent of an exact-name match.
//
// Background: PostgreSQL's _bt_check_unique surfaces the actual
// relation (the child partition's auto-generated unique index)
// in the error, not the parent declared in the migration. The
// child names are autogenerated by ChooseIndexName -- the
// truncation and partition-name embedding make them unsuitable
// for exact assertions. `pg_inherits` is the authoritative
// catalog link from a child to its parent index, so we query it
// directly.
func assertUniqueViolationFromParentOrChild(t *testing.T, db *sql.DB, schema, parentIndex string, pqErr *pq.Error) {
	t.Helper()
	if pqErr.Constraint == "" {
		t.Fatalf("pqErr.Constraint is empty; expected %q or an attached partition child index (msg: %v)",
			parentIndex, pqErr)
	}
	if pqErr.Constraint == parentIndex {
		return // exact match against the parent index itself
	}
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()
	var attached bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_inherits inh
			JOIN pg_class child   ON child.oid   = inh.inhrelid
			JOIN pg_class parent  ON parent.oid  = inh.inhparent
			JOIN pg_namespace cns ON cns.oid     = child.relnamespace
			JOIN pg_namespace pns ON pns.oid     = parent.relnamespace
			WHERE pns.nspname = $1 AND parent.relname = $2
			  AND cns.nspname = $1 AND child.relname  = $3
			  AND parent.relkind IN ('i','I')
			  AND child.relkind  IN ('i','I')
		)
	`, schema, parentIndex, pqErr.Constraint).Scan(&attached); err != nil {
		t.Fatalf("pg_inherits lookup for (%s, %s, %s): %v",
			schema, parentIndex, pqErr.Constraint, err)
	}
	if !attached {
		t.Fatalf("unique violation came from %q which is neither %q nor an attached partition child of it (msg: %v)",
			pqErr.Constraint, parentIndex, pqErr)
	}
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
