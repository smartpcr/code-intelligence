package mgmtapi

// Integration tests for the Stage 7.5 operator read endpoints
// against a live PostgreSQL 16 instance. Skips cleanly when
// AGENT_MEMORY_PG_URL is unset, mirroring the convention in
// migrations/test_migrate_test.go and
// internal/graphwriter/writer_integration_test.go.
//
// Implementation-plan.md Stage 7.5 acceptance scenarios
// covered here (sqlmock can't reach the CTE / array-hydration
// semantics this depth — the rubber-duck pass explicitly
// pinned this surface to the integration pack):
//
//   - "episodes since-filter required" -- exercised in
//     read_unit_test.go (no DB needed for the short-circuit)
//     and again here against the live planner to confirm the
//     pre-check fires before any partition scan.
//   - "current_status reflects latest update" -- seeded with
//     TWO `episode_update` rows (older + newer per the
//     rubber-duck recommendation) so the test PROVES the
//     `DISTINCT ON (episode_id) ORDER BY created_at DESC`
//     join picks the newer row rather than any-update.
//   - "context read tolerates retired ids" -- seeded node /
//     edge get a `node_retirement` / `edge_retirement` row
//     and the GET response is asserted to surface the
//     `retired_at_sha` badge while still succeeding (risk
//     §9.13).
//
// Every Stage 7.5 endpoint is exercised at least once. The
// fixture is sized to cover the §6.2.3 wire shape end-to-end
// in a single fast run; we don't try to enumerate every
// query-parameter branch (the read_unit_test.go matrix
// already covers those without paying the DB roundtrip).

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/migrations"
)

const (
	// envReadIntegrationPGURL re-uses the same env knob the
	// other integration test packages key off so a single
	// `AGENT_MEMORY_PG_URL=... go test ./...` opt-in lights
	// every PG-backed test together.
	envReadIntegrationPGURL = "AGENT_MEMORY_PG_URL"
	readIntegrationTimeout  = 30 * time.Second
)

// readFixture is the per-test PostgreSQL substrate for the
// Stage 7.5 read pack. The handler runs against the owner
// connection — read verbs are SELECT-only and the role-grant
// surface is already covered by
// migrations/test_stage14_role_grants_test.go, so flipping
// agent_memory_app LOGIN here would buy no extra coverage
// and would needlessly serialise the test with sibling
// packages via testpglock (per rubber-duck blocking issue
// #1: connection pinning matters; keeping the surface
// narrow makes that easier).
type readFixture struct {
	db      *sql.DB
	schema  string
	handler *Handler
	cleanup func()
}

// openReadFixture provisions a per-test schema, applies every
// migration, and returns a Handler that points at it. The
// returned *sql.DB is pinned to a single connection so
// `SET search_path` survives subsequent queries — both seed
// inserts and handler reads share that one connection. This
// is the pattern the rubber-duck pass blocked on (issue #1).
func openReadFixture(t *testing.T) *readFixture {
	t.Helper()
	base := os.Getenv(envReadIntegrationPGURL)
	if base == "" {
		t.Skipf("skipping: %s is unset; integration tests require a live PostgreSQL",
			envReadIntegrationPGURL)
	}

	db, err := sql.Open("postgres", base)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// Pin to one connection so search_path is durable and
	// the migrations + handler queries land in the
	// per-test schema. Same constraint as graphwriter and
	// migrations test fixtures.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), readIntegrationTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Skipf("skipping: cannot reach PostgreSQL at %s: %v",
			envReadIntegrationPGURL, err)
	}
	schema := newReadSchemaName(t)
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA `+quoteReadIdent(schema)); err != nil {
		_ = db.Close()
		t.Fatalf("create schema %q: %v", schema, err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(
		`SET search_path TO %s, public, partman`, quoteReadIdent(schema),
	)); err != nil {
		_ = db.Close()
		t.Fatalf("set search_path: %v", err)
	}

	// Apply every migration so the role grants land too —
	// the handler queries `repo_health`, partitioned
	// Episode / Observation / RecallContextLog, and the
	// CTE join against `episode_update`; all of those need
	// the migration trail through 0019 + 0020 + 0021.
	if err := migrations.New(db).Up(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("migrations.Up: %v", err)
	}

	cleanup := func() {
		ctx2, c2 := context.WithTimeout(context.Background(), readIntegrationTimeout)
		defer c2()
		// pg_partman cleanup: delete part_config rows for
		// THIS schema before the schema drop, per the
		// rubber-duck blocking issue #2 (and matching the
		// pattern in graphwriter/migrations).
		schemaPrefix := strings.ReplaceAll(schema, "_", "#_") + ".%"
		_, _ = db.ExecContext(ctx2, `
			DELETE FROM partman.part_config
			WHERE parent_table LIKE $1 ESCAPE '#'
		`, schemaPrefix)
		_, _ = db.ExecContext(ctx2, `DROP SCHEMA `+quoteReadIdent(schema)+` CASCADE`)
		_ = db.Close()
	}

	silentLog := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHandler(db,
		&StaticBearerVerifier{Secret: testToken, Subject: "test-op"},
		fakeResolver(testHeadSHA, nil),
		Options{Logger: silentLog, SecretGen: fixedSecretGen()},
	)
	return &readFixture{db: db, schema: schema, handler: handler, cleanup: cleanup}
}

func newReadSchemaName(t *testing.T) string {
	t.Helper()
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "amread_" + hex.EncodeToString(buf[:])
}

func quoteReadIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// -----------------------------------------------------------
// seeded fixture shape — every read endpoint we exercise reads
// some subset of these IDs.
// -----------------------------------------------------------

type seedIDs struct {
	repo1ID, repo2ID string
	commit1SHA       string
	commit2SHA       string
	commit1At        time.Time
	commit2At        time.Time
	node1ID, node2ID string
	edge1ID, edge2ID string
	conceptID        string
	conceptVersionID string
	supportID        string
	contextID        string
	episodeID        string
	episodeCreatedAt time.Time
	observationID    string
}

// seedReadFixture inserts the full graph the read tests need.
// Each row is timestamped explicitly so ordering assertions
// (commits DESC, episode_update latest-wins, trace_log_tail)
// are deterministic per the rubber-duck suggestion #3.
func seedReadFixture(t *testing.T, fix *readFixture) seedIDs {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), readIntegrationTimeout)
	defer cancel()

	ids := seedIDs{}

	mustExec := func(q string, args ...any) {
		if _, err := fix.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed exec failed: %v\nsql=%s", err, q)
		}
	}
	mustScan := func(q string, dst any, args ...any) {
		if err := fix.db.QueryRowContext(ctx, q, args...).Scan(dst); err != nil {
			t.Fatalf("seed scan failed: %v\nsql=%s", err, q)
		}
	}

	// Two repos. r1 is the populated one + degraded;
	// r2 is the healthy bystander. URLs are randomised
	// per test so concurrent test runs in the same cluster
	// can't collide on the `repo.url` UNIQUE.
	uniq1 := randomHex(t, 4)
	mustScan(`INSERT INTO repo (url, default_branch, current_head_sha, language_hints)
		VALUES ($1, 'main', $2, ARRAY['go']) RETURNING repo_id::text`,
		&ids.repo1ID, "https://git.example/acme/svc-"+uniq1, testHeadSHA)
	uniq2 := randomHex(t, 4)
	mustScan(`INSERT INTO repo (url, default_branch, current_head_sha, language_hints)
		VALUES ($1, 'main', $2, ARRAY[]::text[]) RETURNING repo_id::text`,
		&ids.repo2ID, "https://git.example/acme/lib-"+uniq2, testHeadSHA)

	// repo_health for r1 — degraded, with a valid enum reason.
	mustExec(`INSERT INTO repo_health (repo_id, degraded, degraded_reason, source)
		VALUES ($1::uuid, true, 'span_ingestor_backpressure', 'span-ingestor')`,
		ids.repo1ID)

	// Two commits on r1 with explicit ordering: c1 < c2 so
	// "DESC" returns c2 first and the SHA-visibility test
	// can pin a window "before c2".
	ids.commit1SHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ids.commit2SHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	ids.commit1At = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ids.commit2At = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	mustExec(`INSERT INTO repo_commit (repo_id, sha, parent_sha, committed_at, index_status)
		VALUES ($1::uuid, $2, NULL, $3, 'indexed')`,
		ids.repo1ID, ids.commit1SHA, ids.commit1At)
	mustExec(`INSERT INTO repo_commit (repo_id, sha, parent_sha, committed_at, index_status)
		VALUES ($1::uuid, $2, $3, $4, 'indexed')`,
		ids.repo1ID, ids.commit2SHA, ids.commit1SHA, ids.commit2At)

	// Two nodes: n1 (retired at c2), n2 (current).
	// Fingerprints are 32-byte hashes per the octet_length
	// CHECK (rubber-duck non-blocking #4).
	mustScan(`INSERT INTO node (fingerprint, repo_id, kind, canonical_signature, from_sha, attrs_json)
		VALUES ($1, $2::uuid, 'method', 'pkg.Foo#bar()', $3, '{"line":10}'::jsonb)
		RETURNING node_id::text`,
		&ids.node1ID,
		fingerprintFor("n1-"+ids.repo1ID), ids.repo1ID, ids.commit1SHA)
	mustScan(`INSERT INTO node (fingerprint, repo_id, kind, canonical_signature, from_sha)
		VALUES ($1, $2::uuid, 'class', 'pkg.Foo', $3)
		RETURNING node_id::text`,
		&ids.node2ID,
		fingerprintFor("n2-"+ids.repo1ID), ids.repo1ID, ids.commit1SHA)

	// node_retirement: n1 retires AT c2 (so a query at c1
	// should NOT see the retirement; a query at c2 SHOULD).
	mustExec(`INSERT INTO node_retirement (node_id, retired_at_sha, retired_at)
		VALUES ($1::uuid, $2, $3)`,
		ids.node1ID, ids.commit2SHA, ids.commit2At)

	// Two edges between n1 → n2: e1 retired, e2 current.
	mustScan(`INSERT INTO edge (fingerprint, repo_id, kind, src_node_id, dst_node_id, from_sha)
		VALUES ($1, $2::uuid, 'contains', $3::uuid, $4::uuid, $5)
		RETURNING edge_id::text`,
		&ids.edge1ID,
		fingerprintFor("e1-"+ids.repo1ID), ids.repo1ID,
		ids.node1ID, ids.node2ID, ids.commit1SHA)
	mustScan(`INSERT INTO edge (fingerprint, repo_id, kind, src_node_id, dst_node_id, from_sha)
		VALUES ($1, $2::uuid, 'observed_calls', $3::uuid, $4::uuid, $5)
		RETURNING edge_id::text`,
		&ids.edge2ID,
		fingerprintFor("e2-"+ids.repo1ID), ids.repo1ID,
		ids.node1ID, ids.node2ID, ids.commit1SHA)
	mustExec(`INSERT INTO edge_retirement (edge_id, retired_at_sha, retired_at)
		VALUES ($1::uuid, $2, $3)`,
		ids.edge1ID, ids.commit2SHA, ids.commit2At)

	// Concept + its latest version (promoted) + one support row.
	mustScan(`INSERT INTO concept (fingerprint, name, description_md)
		VALUES ($1, 'pattern.X', 'a recurring pattern')
		RETURNING concept_id::text`,
		&ids.conceptID, fingerprintFor("concept-"+ids.repo1ID))
	mustScan(`INSERT INTO concept_version
			(concept_id, version_index, confidence, confidence_band,
			 support_count, negative_count, producer, producer_run_id, promoted)
		VALUES ($1::uuid, 1, 0.95, 'high', 5, 0, 'promoter',
			gen_random_uuid(), true)
		RETURNING concept_version_id::text`,
		&ids.conceptVersionID, ids.conceptID)
	mustScan(`INSERT INTO concept_support
			(concept_id, concept_version_id, repo_id, node_id, episode_id, polarity)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, NULL, 'positive')
		RETURNING support_id::text`,
		&ids.supportID, ids.conceptID, ids.conceptVersionID,
		ids.repo1ID, ids.node1ID)

	// RecallContextLog with all three array slots populated.
	mustScan(`INSERT INTO recall_context_log
			(repo_id, verb, query_json, node_ids, edge_ids, concept_ids,
			 reranker_model_version, served_under_degraded, created_at)
		VALUES ($1::uuid, 'recall', '{"q":"test"}'::jsonb,
			ARRAY[$2::uuid, $3::uuid], ARRAY[$4::uuid, $5::uuid], ARRAY[$6::uuid],
			'reranker-v1', false, now())
		RETURNING context_id::text`,
		&ids.contextID, ids.repo1ID,
		ids.node1ID, ids.node2ID, ids.edge1ID, ids.edge2ID, ids.conceptID)

	// Episode + EpisodeUpdate (two updates per rubber-duck
	// blocking #3 — the latest is human_corrected so
	// current_status MUST surface human_corrected, not the
	// older 'degraded' value).
	ids.episodeCreatedAt = time.Now().UTC().Add(-2 * time.Hour)
	mustScan(`INSERT INTO episode
			(episode_group_id, repo_id, session_id, trace_id, kind,
			 outcome, context_id, action, created_at)
		VALUES (gen_random_uuid(), $1::uuid, 's-1', 't-1', 'agent',
			'failure', $2::uuid, '{"call":"x"}'::jsonb, $3)
		RETURNING episode_id::text`,
		&ids.episodeID, ids.repo1ID, ids.contextID, ids.episodeCreatedAt)
	// older update: degraded
	mustExec(`INSERT INTO episode_update (episode_id, new_outcome, actor, created_at)
		VALUES ($1::uuid, 'degraded', 'system', $2)`,
		ids.episodeID, ids.episodeCreatedAt.Add(time.Minute))
	// newer update: human_corrected — this is the row the
	// `DISTINCT ON (episode_id) ORDER BY created_at DESC`
	// MUST pick.
	mustExec(`INSERT INTO episode_update (episode_id, new_outcome, actor, created_at)
		VALUES ($1::uuid, 'human_corrected', 'operator', $2)`,
		ids.episodeID, ids.episodeCreatedAt.Add(2*time.Minute))

	// Observation: role=node_hit + node_id (single-target
	// invariant; rubber-duck non-blocking #5).
	mustScan(`INSERT INTO observation (episode_id, role, node_id, weight)
		VALUES ($1::uuid, 'node_hit', $2::uuid, 0.7)
		RETURNING observation_id::text`,
		&ids.observationID, ids.episodeID, ids.node1ID)

	// TraceObservation aggregate + two log rows (so the
	// `before=` cursor test has something to filter, per
	// rubber-duck suggestion #2).
	mustExec(`INSERT INTO trace_observation
			(edge_id, observation_count, p50_latency_ms, p95_latency_ms,
			 latest_span_ref, last_observed_at)
		VALUES ($1::uuid, 2, 12.3, 45.6, 'span-ref-latest', $2)`,
		ids.edge2ID, time.Now().UTC())
	mustExec(`INSERT INTO trace_observation_log
			(edge_id, trace_id, span_id, started_at, duration_ms)
		VALUES ($1::uuid, 'tr-old', 'sp-old', $2, 10.0)`,
		ids.edge2ID, time.Now().UTC().Add(-2*time.Hour))
	mustExec(`INSERT INTO trace_observation_log
			(edge_id, trace_id, span_id, started_at, duration_ms)
		VALUES ($1::uuid, 'tr-new', 'sp-new', $2, 20.0)`,
		ids.edge2ID, time.Now().UTC().Add(-30*time.Minute))

	return ids
}

// fingerprintFor returns a deterministic 32-byte hash used as
// a node / edge / concept fingerprint. The CHECK constraint
// on every fingerprint column requires exactly 32 bytes.
func fingerprintFor(seed string) []byte {
	h := sha256.Sum256([]byte(seed))
	return h[:]
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// doGet sends an authenticated GET to the fixture handler and
// returns (status, body bytes).
func doGet(t *testing.T, fix *readFixture, path string) (int, []byte) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set(AuthorizationHeader, "Bearer "+testToken)
	w := httptest.NewRecorder()
	fix.handler.ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

// -----------------------------------------------------------
// Tests
// -----------------------------------------------------------

func TestIntegration_ReadRepos_aggregatesPerRepoDegraded(t *testing.T) {
	fix := openReadFixture(t)
	defer fix.cleanup()
	ids := seedReadFixture(t, fix)

	status, body := doGet(t, fix, RouteReadRepos)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", status, body)
	}
	var resp struct {
		Repos          []RepoRow `json:"repos"`
		Degraded       bool      `json:"degraded"`
		DegradedReason string    `json:"degraded_reason"`
	}
	mustDecode(t, body, &resp)
	if !resp.Degraded {
		t.Errorf("top-level degraded=false; want true (one repo is degraded). body=%s", body)
	}
	if resp.DegradedReason != "span_ingestor_backpressure" {
		t.Errorf("degraded_reason = %q, want span_ingestor_backpressure", resp.DegradedReason)
	}
	gotR1 := false
	gotR2 := false
	for _, r := range resp.Repos {
		switch r.RepoID {
		case ids.repo1ID:
			gotR1 = true
			if !r.Degraded {
				t.Errorf("repo1 should be degraded; got %+v", r)
			}
		case ids.repo2ID:
			gotR2 = true
			if r.Degraded {
				t.Errorf("repo2 should be healthy; got %+v", r)
			}
		}
	}
	if !gotR1 || !gotR2 {
		t.Errorf("both repos must be returned (r1=%v, r2=%v); body=%s",
			gotR1, gotR2, body)
	}
}

func TestIntegration_ReadCommits_descOrderAndDegraded(t *testing.T) {
	fix := openReadFixture(t)
	defer fix.cleanup()
	ids := seedReadFixture(t, fix)

	status, body := doGet(t, fix, RouteReadCommits+"?repo_id="+ids.repo1ID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", status, body)
	}
	var resp struct {
		RepoID         string      `json:"repo_id"`
		Commits        []CommitRow `json:"commits"`
		Degraded       bool        `json:"degraded"`
		DegradedReason string      `json:"degraded_reason"`
	}
	mustDecode(t, body, &resp)
	if resp.RepoID != ids.repo1ID {
		t.Errorf("repo_id = %q, want %q", resp.RepoID, ids.repo1ID)
	}
	if len(resp.Commits) != 2 {
		t.Fatalf("commits len = %d, want 2. body=%s", len(resp.Commits), body)
	}
	if resp.Commits[0].SHA != ids.commit2SHA {
		t.Errorf("commits[0].sha = %q, want %q (DESC by committed_at)",
			resp.Commits[0].SHA, ids.commit2SHA)
	}
	if resp.Commits[1].SHA != ids.commit1SHA {
		t.Errorf("commits[1].sha = %q, want %q", resp.Commits[1].SHA, ids.commit1SHA)
	}
	if !resp.Degraded {
		t.Errorf("expected degraded=true for r1; got body=%s", body)
	}
}

// Stage 7.5 scenario 1 — live planner. Pairs with the
// no-DB unit test of the same name.
func TestIntegration_ReadEpisodes_missingSince_returns400(t *testing.T) {
	fix := openReadFixture(t)
	defer fix.cleanup()
	_ = seedReadFixture(t, fix)

	status, body := doGet(t, fix, RouteReadEpisodes)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%s", status, body)
	}
	var env ErrorEnvelope
	mustDecode(t, body, &env)
	if env.Code != "since_required" {
		t.Errorf("code = %q, want since_required", env.Code)
	}
}

// Stage 7.5 scenario 2 — TWO episode_update rows; the
// handler MUST surface the LATER one as current_status.
func TestIntegration_ReadEpisodes_currentStatusReflectsLatestUpdate(t *testing.T) {
	fix := openReadFixture(t)
	defer fix.cleanup()
	ids := seedReadFixture(t, fix)

	status, body := doGet(t, fix,
		RouteReadEpisodes+"?since=24h&repo_id="+ids.repo1ID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", status, body)
	}
	var resp struct {
		Episodes       []EpisodeRow `json:"episodes"`
		Degraded       bool         `json:"degraded"`
		DegradedReason string       `json:"degraded_reason"`
	}
	mustDecode(t, body, &resp)
	var ep *EpisodeRow
	for i := range resp.Episodes {
		if resp.Episodes[i].EpisodeID == ids.episodeID {
			ep = &resp.Episodes[i]
			break
		}
	}
	if ep == nil {
		t.Fatalf("seeded episode missing from response: %s", body)
	}
	if ep.Outcome != "failure" {
		t.Errorf("outcome = %q, want failure (the ORIGINAL column)", ep.Outcome)
	}
	if ep.CurrentStatus != "human_corrected" {
		t.Errorf("current_status = %q, want human_corrected (the LATEST update)",
			ep.CurrentStatus)
	}
}

// `outcome_in=human_corrected` should NOT filter out our
// seeded episode despite its column-level outcome being
// `failure` — the filter applies to `e.outcome::text`, which
// is the original column. This is a useful guard that the
// CTE join didn't accidentally swap the filter onto
// current_status.
func TestIntegration_ReadEpisodes_outcomeInFiltersOriginalColumn(t *testing.T) {
	fix := openReadFixture(t)
	defer fix.cleanup()
	ids := seedReadFixture(t, fix)

	status, body := doGet(t, fix,
		RouteReadEpisodes+"?since=24h&repo_id="+ids.repo1ID+"&outcome_in=failure")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", status, body)
	}
	var resp struct {
		Episodes []EpisodeRow `json:"episodes"`
	}
	mustDecode(t, body, &resp)
	if len(resp.Episodes) != 1 {
		t.Fatalf("episodes len = %d, want 1. body=%s", len(resp.Episodes), body)
	}
	if resp.Episodes[0].EpisodeID != ids.episodeID {
		t.Errorf("got wrong episode: %s", resp.Episodes[0].EpisodeID)
	}

	// Negative: filtering on success returns nothing.
	status2, body2 := doGet(t, fix,
		RouteReadEpisodes+"?since=24h&repo_id="+ids.repo1ID+"&outcome_in=success")
	if status2 != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", status2, body2)
	}
	var resp2 struct {
		Episodes []EpisodeRow `json:"episodes"`
	}
	mustDecode(t, body2, &resp2)
	if len(resp2.Episodes) != 0 {
		t.Errorf("expected 0 success-outcome episodes; got %d", len(resp2.Episodes))
	}
}

func TestIntegration_ReadObservations_byEpisodeID(t *testing.T) {
	fix := openReadFixture(t)
	defer fix.cleanup()
	ids := seedReadFixture(t, fix)

	status, body := doGet(t, fix,
		RouteReadObservations+"?episode_id="+ids.episodeID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", status, body)
	}
	var resp struct {
		EpisodeID    string           `json:"episode_id"`
		Observations []ObservationRow `json:"observations"`
		Degraded     bool             `json:"degraded"`
	}
	mustDecode(t, body, &resp)
	if resp.EpisodeID != ids.episodeID {
		t.Errorf("episode_id = %q, want %q", resp.EpisodeID, ids.episodeID)
	}
	if len(resp.Observations) != 1 {
		t.Fatalf("observations len = %d, want 1. body=%s", len(resp.Observations), body)
	}
	o := resp.Observations[0]
	if o.ObservationID != ids.observationID {
		t.Errorf("observation_id = %q, want %q", o.ObservationID, ids.observationID)
	}
	if o.Role != "node_hit" || o.NodeID != ids.node1ID {
		t.Errorf("role/node mismatch: %+v", o)
	}
	if !resp.Degraded {
		// degraded resolves via parent Episode's repo (r1, which is degraded).
		t.Errorf("expected degraded=true via parent Episode; body=%s", body)
	}
}

// Stage 7.5 scenario 3 — context read tolerates retired ids.
func TestIntegration_ReadContext_tolerateRetiredNodeAndEdge(t *testing.T) {
	fix := openReadFixture(t)
	defer fix.cleanup()
	ids := seedReadFixture(t, fix)

	status, body := doGet(t, fix, RouteReadContextPrefix+ids.contextID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (must succeed despite retirement). body=%s",
			status, body)
	}
	var resp struct {
		ContextID  string               `json:"context_id"`
		NodeIDs    []string             `json:"node_ids"`
		EdgeIDs    []string             `json:"edge_ids"`
		ConceptIDs []string             `json:"concept_ids"`
		Nodes      []ContextNodeCard    `json:"nodes"`
		Edges      []ContextEdgeCard    `json:"edges"`
		Concepts   []ContextConceptCard `json:"concepts"`
		Degraded   bool                 `json:"degraded"`
	}
	mustDecode(t, body, &resp)
	if resp.ContextID != ids.contextID {
		t.Errorf("context_id = %q, want %q", resp.ContextID, ids.contextID)
	}
	if len(resp.Nodes) != 2 || resp.Nodes[0].NodeID != ids.node1ID {
		t.Fatalf("nodes shape unexpected (ord preserved?): %+v body=%s",
			resp.Nodes, body)
	}
	// node1 is retired AT c2 — expect retired_at_sha=c2.
	if resp.Nodes[0].RetiredAtSHA != ids.commit2SHA {
		t.Errorf("nodes[0].retired_at_sha = %q, want %q (retired ID surfaces badge)",
			resp.Nodes[0].RetiredAtSHA, ids.commit2SHA)
	}
	if !resp.Nodes[0].Resolved {
		t.Errorf("nodes[0].resolved=false; want true even for retired id")
	}
	if resp.Nodes[1].RetiredAtSHA != "" {
		t.Errorf("nodes[1] (n2) should not be retired; got %q",
			resp.Nodes[1].RetiredAtSHA)
	}
	if len(resp.Edges) != 2 || resp.Edges[0].EdgeID != ids.edge1ID {
		t.Fatalf("edges shape unexpected (ord preserved?): %+v body=%s",
			resp.Edges, body)
	}
	if resp.Edges[0].RetiredAtSHA != ids.commit2SHA {
		t.Errorf("edges[0].retired_at_sha = %q, want %q",
			resp.Edges[0].RetiredAtSHA, ids.commit2SHA)
	}
	if resp.Edges[1].RetiredAtSHA != "" {
		t.Errorf("edges[1] (e2) should not be retired; got %q",
			resp.Edges[1].RetiredAtSHA)
	}
	if len(resp.Concepts) != 1 || resp.Concepts[0].ConceptID != ids.conceptID {
		t.Errorf("concepts mismatch: %+v", resp.Concepts)
	}
	if !resp.Concepts[0].Resolved {
		t.Errorf("concept.resolved = false; want true")
	}
}

// Implementation-plan §369-371 scenario "degraded snapshot
// flag": an Append with served_under_degraded=true MUST cause
// `mgmt.read.context` to return `degraded=true` to its caller
// even when repo_health says the repo is otherwise healthy.
// We seed a SECOND RecallContextLog on the HEALTHY repo
// (repo2 — no repo_health row at all) with
// served_under_degraded=true so the only thing flipping the
// envelope is the row's own flag.
func TestIntegration_ReadContext_servedUnderDegraded_setsEnvelope(t *testing.T) {
	fix := openReadFixture(t)
	defer fix.cleanup()
	ids := seedReadFixture(t, fix)

	ctx, cancel := context.WithTimeout(context.Background(), readIntegrationTimeout)
	defer cancel()

	var degradedCtxID string
	if err := fix.db.QueryRowContext(ctx, `
		INSERT INTO recall_context_log
			(repo_id, verb, query_json, node_ids, edge_ids, concept_ids,
			 reranker_model_version, served_under_degraded, created_at)
		VALUES ($1::uuid, 'recall', '{"q":"snapshot"}'::jsonb,
			ARRAY[]::uuid[], ARRAY[]::uuid[], ARRAY[]::uuid[],
			'reranker-v1', true, now())
		RETURNING context_id::text
	`, ids.repo2ID).Scan(&degradedCtxID); err != nil {
		t.Fatalf("seed served_under_degraded context: %v", err)
	}

	status, body := doGet(t, fix, RouteReadContextPrefix+degradedCtxID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", status, body)
	}
	var resp struct {
		ContextID           string `json:"context_id"`
		RepoID              string `json:"repo_id"`
		ServedUnderDegraded bool   `json:"served_under_degraded"`
		Degraded            bool   `json:"degraded"`
	}
	mustDecode(t, body, &resp)
	if resp.ContextID != degradedCtxID {
		t.Errorf("context_id = %q, want %q", resp.ContextID, degradedCtxID)
	}
	if resp.RepoID != ids.repo2ID {
		t.Errorf("repo_id = %q, want %q (healthy repo)", resp.RepoID, ids.repo2ID)
	}
	if !resp.ServedUnderDegraded {
		t.Errorf("row's served_under_degraded = false; want true")
	}
	if !resp.Degraded {
		t.Errorf("envelope degraded = false; want true (row's served_under_degraded must trip it even on a healthy repo). body=%s",
			body)
	}
}

func TestIntegration_ReadConcepts_promotedFilter(t *testing.T) {
	fix := openReadFixture(t)
	defer fix.cleanup()
	ids := seedReadFixture(t, fix)

	status, body := doGet(t, fix, RouteReadConcepts+"?promoted=true")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", status, body)
	}
	var resp struct {
		Concepts []ConceptRow `json:"concepts"`
		Degraded bool         `json:"degraded"`
	}
	mustDecode(t, body, &resp)
	var found *ConceptRow
	for i := range resp.Concepts {
		if resp.Concepts[i].ConceptID == ids.conceptID {
			found = &resp.Concepts[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("seeded concept missing: body=%s", body)
	}
	if found.VersionIndex != 1 || !found.Promoted || found.Confidence < 0.9 {
		t.Errorf("concept version fields off: %+v", *found)
	}
	if found.ConfidenceBand != "high" {
		t.Errorf("confidence_band = %q, want high", found.ConfidenceBand)
	}
}

func TestIntegration_ReadConceptSupports_byConceptID(t *testing.T) {
	fix := openReadFixture(t)
	defer fix.cleanup()
	ids := seedReadFixture(t, fix)

	status, body := doGet(t, fix,
		RouteReadConceptSupports+"?concept_id="+ids.conceptID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", status, body)
	}
	var resp struct {
		ConceptID string              `json:"concept_id"`
		Supports  []ConceptSupportRow `json:"supports"`
	}
	mustDecode(t, body, &resp)
	if resp.ConceptID != ids.conceptID {
		t.Errorf("concept_id = %q, want %q", resp.ConceptID, ids.conceptID)
	}
	if len(resp.Supports) != 1 {
		t.Fatalf("supports len = %d, want 1. body=%s", len(resp.Supports), body)
	}
	s := resp.Supports[0]
	if s.SupportID != ids.supportID || s.NodeID != ids.node1ID || s.Polarity != "positive" {
		t.Errorf("support fields wrong: %+v", s)
	}
}

func TestIntegration_ReadGraphNode_defaultView_retiredNode_returns404(t *testing.T) {
	fix := openReadFixture(t)
	defer fix.cleanup()
	ids := seedReadFixture(t, fix)

	// Default ("current") view: per architecture §6.2.3 the
	// `mgmt.read.graph_node` verb returns the node card at
	// the requested sha, default = current. A node carrying
	// a `node_retirement` tombstone is NOT part of the
	// current view; the handler must 404 with `node_retired`.
	status, body := doGet(t, fix, RouteReadGraphNodePrefix+ids.node1ID)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (retired node hidden in default view). body=%s",
			status, body)
	}
	var env ErrorEnvelope
	mustDecode(t, body, &env)
	if env.Code != "node_retired" {
		t.Errorf("code = %q, want node_retired (distinguishes from never-existed)", env.Code)
	}
}

func TestIntegration_ReadGraphNode_defaultView_aliveNode_excludesRetiredEdges(t *testing.T) {
	fix := openReadFixture(t)
	defer fix.cleanup()
	ids := seedReadFixture(t, fix)

	// n2 is alive; e1 (n1→n2) is retired, e2 (n1→n2) is
	// not. The default current view returns n2 200 with
	// only the non-retired neighbor e2 — e1's tombstone
	// is anti-joined out of the neighbor list.
	status, body := doGet(t, fix, RouteReadGraphNodePrefix+ids.node2ID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", status, body)
	}
	var resp struct {
		NodeID       string              `json:"node_id"`
		RetiredAtSHA string              `json:"retired_at_sha"`
		Neighbors    []GraphNodeNeighbor `json:"neighbors"`
	}
	mustDecode(t, body, &resp)
	if resp.NodeID != ids.node2ID {
		t.Errorf("node_id = %q, want %q", resp.NodeID, ids.node2ID)
	}
	if resp.RetiredAtSHA != "" {
		t.Errorf("alive node had retired_at_sha = %q", resp.RetiredAtSHA)
	}
	if len(resp.Neighbors) != 1 {
		t.Fatalf("neighbors len = %d, want 1 (only e2, e1 retired-excluded). body=%s",
			len(resp.Neighbors), body)
	}
	if resp.Neighbors[0].EdgeID != ids.edge2ID {
		t.Errorf("neighbor[0] = %q, want %q (e2)", resp.Neighbors[0].EdgeID, ids.edge2ID)
	}
	if resp.Neighbors[0].RetiredAtSHA != "" {
		t.Errorf("e2 carried badge in default view: %+v", resp.Neighbors[0])
	}
	if resp.Neighbors[0].Direction != "in" {
		// n2 is the dst of both edges; the neighbor card
		// for n2 reports direction='in'.
		t.Errorf("direction = %q, want in", resp.Neighbors[0].Direction)
	}
}

func TestIntegration_ReadGraphNode_includeRetired_surfacesNodeAndRetiredEdges(t *testing.T) {
	fix := openReadFixture(t)
	defer fix.cleanup()
	ids := seedReadFixture(t, fix)

	// Operator escape hatch: ?include_retired=true returns
	// the retired node (with its tombstone badge) AND the
	// retired neighbor edge (with its badge).
	status, body := doGet(t, fix,
		RouteReadGraphNodePrefix+ids.node1ID+"?include_retired=true")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", status, body)
	}
	var resp struct {
		NodeID       string              `json:"node_id"`
		Kind         string              `json:"kind"`
		RetiredAtSHA string              `json:"retired_at_sha"`
		Neighbors    []GraphNodeNeighbor `json:"neighbors"`
	}
	mustDecode(t, body, &resp)
	if resp.NodeID != ids.node1ID {
		t.Errorf("node_id mismatch: %q vs %q", resp.NodeID, ids.node1ID)
	}
	if resp.Kind != "method" {
		t.Errorf("kind = %q, want method", resp.Kind)
	}
	if resp.RetiredAtSHA != ids.commit2SHA {
		t.Errorf("retired_at_sha = %q, want %q",
			resp.RetiredAtSHA, ids.commit2SHA)
	}
	if len(resp.Neighbors) != 2 {
		t.Fatalf("neighbors len = %d, want 2 (e1+e2 both included). body=%s",
			len(resp.Neighbors), body)
	}
	seen := map[string]GraphNodeNeighbor{}
	for _, n := range resp.Neighbors {
		seen[n.EdgeID] = n
		if n.Direction != "out" {
			t.Errorf("neighbor %s direction = %q, want out", n.EdgeID, n.Direction)
		}
		if n.OtherNodeID != ids.node2ID {
			t.Errorf("neighbor %s other = %q, want %q",
				n.EdgeID, n.OtherNodeID, ids.node2ID)
		}
	}
	if seen[ids.edge1ID].RetiredAtSHA != ids.commit2SHA {
		t.Errorf("e1 neighbor should carry retired badge: %+v", seen[ids.edge1ID])
	}
	if seen[ids.edge2ID].RetiredAtSHA != "" {
		t.Errorf("e2 neighbor should NOT carry retired badge: %+v", seen[ids.edge2ID])
	}
}

// At sha=c1 (the earlier SHA) the node is visible but the
// retirement happened LATER (c2); the badge MUST be cleared
// so the point-in-time view shows "alive at c1". Neighbors
// must also be SHA-aware: e1 is retired at c2 so it's
// ALIVE at c1 (no badge); e2 was never retired.
func TestIntegration_ReadGraphNode_shaPointInTime_clearsBadgeBeforeRetirement(t *testing.T) {
	fix := openReadFixture(t)
	defer fix.cleanup()
	ids := seedReadFixture(t, fix)

	status, body := doGet(t, fix,
		RouteReadGraphNodePrefix+ids.node1ID+"?sha="+ids.commit1SHA)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", status, body)
	}
	var resp struct {
		NodeID        string              `json:"node_id"`
		RetiredAtSHA  string              `json:"retired_at_sha"`
		ResolvedAtSHA string              `json:"resolved_at_sha"`
		Neighbors     []GraphNodeNeighbor `json:"neighbors"`
	}
	mustDecode(t, body, &resp)
	if resp.ResolvedAtSHA != ids.commit1SHA {
		t.Errorf("resolved_at_sha = %q, want %q", resp.ResolvedAtSHA, ids.commit1SHA)
	}
	if resp.RetiredAtSHA != "" {
		t.Errorf("retired_at_sha = %q; want empty (retirement at c2 > c1)",
			resp.RetiredAtSHA)
	}
	if len(resp.Neighbors) != 2 {
		t.Fatalf("neighbors len = %d, want 2 (both alive at c1). body=%s",
			len(resp.Neighbors), body)
	}
	for _, n := range resp.Neighbors {
		if n.RetiredAtSHA != "" {
			t.Errorf("neighbor %s carried future-retirement badge %q at sha=c1",
				n.EdgeID, n.RetiredAtSHA)
		}
	}
}

// At sha=c2 (the retirement SHA itself) the badge surfaces
// on the node AND on the retired neighbor edge; the
// non-retired edge remains badge-free.
func TestIntegration_ReadGraphNode_shaPointInTime_keepsBadgeAtRetirement(t *testing.T) {
	fix := openReadFixture(t)
	defer fix.cleanup()
	ids := seedReadFixture(t, fix)

	status, body := doGet(t, fix,
		RouteReadGraphNodePrefix+ids.node1ID+"?sha="+ids.commit2SHA)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", status, body)
	}
	var resp struct {
		RetiredAtSHA  string              `json:"retired_at_sha"`
		ResolvedAtSHA string              `json:"resolved_at_sha"`
		Neighbors     []GraphNodeNeighbor `json:"neighbors"`
	}
	mustDecode(t, body, &resp)
	if resp.RetiredAtSHA != ids.commit2SHA {
		t.Errorf("retired_at_sha = %q, want %q", resp.RetiredAtSHA, ids.commit2SHA)
	}
	if resp.ResolvedAtSHA != ids.commit2SHA {
		t.Errorf("resolved_at_sha = %q, want %q", resp.ResolvedAtSHA, ids.commit2SHA)
	}
	// At c2 both edges are visible (their from_sha is c1)
	// but e1 carries the badge (retired at c2) and e2 does
	// not (never retired).
	if len(resp.Neighbors) != 2 {
		t.Fatalf("neighbors len = %d, want 2. body=%s",
			len(resp.Neighbors), body)
	}
	seen := map[string]GraphNodeNeighbor{}
	for _, n := range resp.Neighbors {
		seen[n.EdgeID] = n
	}
	if seen[ids.edge1ID].RetiredAtSHA != ids.commit2SHA {
		t.Errorf("e1 should carry badge at c2: %+v", seen[ids.edge1ID])
	}
	if seen[ids.edge2ID].RetiredAtSHA != "" {
		t.Errorf("e2 should NOT carry badge at c2: %+v", seen[ids.edge2ID])
	}
}

// Unknown SHA → 400.
func TestIntegration_ReadGraphNode_unknownSHA_returns400(t *testing.T) {
	fix := openReadFixture(t)
	defer fix.cleanup()
	ids := seedReadFixture(t, fix)

	bogus := strings.Repeat("c", 40)
	status, body := doGet(t, fix,
		RouteReadGraphNodePrefix+ids.node1ID+"?sha="+bogus)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%s", status, body)
	}
	var env ErrorEnvelope
	mustDecode(t, body, &env)
	if env.Code != "unknown_sha" {
		t.Errorf("code = %q, want unknown_sha", env.Code)
	}
}

func TestIntegration_ReadGraphNode_unknownNodeID_returns404(t *testing.T) {
	fix := openReadFixture(t)
	defer fix.cleanup()
	_ = seedReadFixture(t, fix)

	missing := "99999999-1111-2222-3333-444444444444"
	status, body := doGet(t, fix, RouteReadGraphNodePrefix+missing)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. body=%s", status, body)
	}
}

func TestIntegration_ReadTraceObservation_aggregateAndLogTail(t *testing.T) {
	fix := openReadFixture(t)
	defer fix.cleanup()
	ids := seedReadFixture(t, fix)

	status, body := doGet(t, fix, RouteReadTraceObsPrefix+ids.edge2ID)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", status, body)
	}
	var resp struct {
		EdgeID           string           `json:"edge_id"`
		ObservationCount int64            `json:"observation_count"`
		P50LatencyMs     float64          `json:"p50_latency_ms"`
		P95LatencyMs     float64          `json:"p95_latency_ms"`
		LatestSpanRef    string           `json:"latest_span_ref"`
		LastObservedAt   string           `json:"last_observed_at"`
		LogTail          []TraceObsLogRow `json:"log_tail"`
		Degraded         bool             `json:"degraded"`
	}
	mustDecode(t, body, &resp)
	if resp.EdgeID != ids.edge2ID {
		t.Errorf("edge_id mismatch: %q vs %q", resp.EdgeID, ids.edge2ID)
	}
	if resp.ObservationCount != 2 {
		t.Errorf("observation_count = %d, want 2", resp.ObservationCount)
	}
	if resp.LatestSpanRef != "span-ref-latest" {
		t.Errorf("latest_span_ref = %q", resp.LatestSpanRef)
	}
	if len(resp.LogTail) != 2 {
		t.Fatalf("log_tail len = %d, want 2. body=%s", len(resp.LogTail), body)
	}
	// Most-recent first.
	if resp.LogTail[0].TraceID != "tr-new" {
		t.Errorf("log_tail[0].trace_id = %q, want tr-new",
			resp.LogTail[0].TraceID)
	}
	if !resp.Degraded {
		t.Errorf("expected degraded=true via edge.repo_id; body=%s", body)
	}
}

// `before=` cursor returns only the older log row.
func TestIntegration_ReadTraceObservation_beforeCursorTrimsLogTail(t *testing.T) {
	fix := openReadFixture(t)
	defer fix.cleanup()
	ids := seedReadFixture(t, fix)

	// Cursor halfway between the two seeded log rows
	// (between -2h and -30m).
	before := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	status, body := doGet(t, fix,
		RouteReadTraceObsPrefix+ids.edge2ID+"?before="+before)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", status, body)
	}
	var resp struct {
		LogTail []TraceObsLogRow `json:"log_tail"`
	}
	mustDecode(t, body, &resp)
	if len(resp.LogTail) != 1 {
		t.Fatalf("log_tail len = %d, want 1 (cursor trims newer row). body=%s",
			len(resp.LogTail), body)
	}
	if resp.LogTail[0].TraceID != "tr-old" {
		t.Errorf("log_tail[0].trace_id = %q, want tr-old (cursor side)",
			resp.LogTail[0].TraceID)
	}
}

// Sanity: an unknown context_id → 404, NOT a 500. This was
// pre-validated by reUUID but the DB lookup still needs to
// distinguish "no row" from "DB error".
func TestIntegration_ReadContext_unknownID_returns404(t *testing.T) {
	fix := openReadFixture(t)
	defer fix.cleanup()
	_ = seedReadFixture(t, fix)

	missing := "11111111-2222-3333-4444-aaaaaaaaaaaa"
	status, body := doGet(t, fix, RouteReadContextPrefix+missing)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. body=%s", status, body)
	}
	var env ErrorEnvelope
	mustDecode(t, body, &env)
	if env.Code != "context_not_found" {
		t.Errorf("code = %q, want context_not_found", env.Code)
	}
}

// Silence the unused-import linter for pq when we don't
// actively reference it but want the side-effect "register
// postgres driver" import. The fixture relies on the lib/pq
// driver being registered for `sql.Open("postgres", ...)`.
var _ = pq.Array

// Silence the bytes import while keeping it available for any
// future test that wants to assert raw body bytes.
var _ = bytes.NewBuffer

// mustDecode mirrors the helper from handler_unit_test.go but
// returns nothing (the existing helper already exists in this
// package). We re-export the name for grep-ability across the
// integration pack. See handler_unit_test.go:mustDecode.
//
// (deliberately empty — placeholder anchor for the comment;
// the real helper is declared in handler_unit_test.go.)
var _ = json.Marshal // keep `encoding/json` referenced
