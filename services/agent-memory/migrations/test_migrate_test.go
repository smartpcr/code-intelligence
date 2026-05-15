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
// implementation-plan.md Stage 1.2. After Up() every expected
// ENUM, table, and UNIQUE index must be present in the per-test
// schema.
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
		"repo", "commit",
		"node", "edge",
		"node_retirement", "edge_retirement",
		"trace_observation", "trace_observation_log",
		"repo_event",
		"ingest_jobs",
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
func indexDef(t *testing.T, db *sql.DB, schema, name string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()
	var def sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT pg_get_indexdef(c.oid)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind = 'i'
	`, schema, name).Scan(&def)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("indexDef(%s.%s): %v", schema, name, err)
	}
	return def.String
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
