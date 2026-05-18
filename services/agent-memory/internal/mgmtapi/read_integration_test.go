package mgmtapi_test

// Live-PostgreSQL integration tests for the Stage 7.5
// `mgmt.read.*` verbs. implementation-plan.md §7.5 calls out
// the matrix of read endpoints (`mgmt.read.repos`,
// `mgmt.read.commits`, `mgmt.read.episodes`,
// `mgmt.read.observations`, `mgmt.read.context`,
// `mgmt.read.concepts`, `mgmt.read.concept_supports`,
// `mgmt.read.graph_node`, `mgmt.read.trace_observation`) plus
// these test scenarios:
//
//   - `current_status` reflects the latest EpisodeUpdate;
//   - `mgmt.read.context` tolerates retired Node/Edge ids and
//     surfaces a `retired_at_sha` badge;
//   - every successful response carries the §6.3 `degraded`
//     envelope (false in Stage 7.5).
//
// This file pins each endpoint against a live PostgreSQL 16 +
// pg_partman v5 schema seeded with a deliberately small but
// representative graph (one repo + one commit + one recall
// context referencing a retired Node + one EpisodeUpdate
// flipping an Episode's outcome + one TraceObservation log
// row). The handler runs through `httptest.NewServer` so the
// real auth middleware + JSON envelope path are exercised end
// to end; the asserts target the wire shape, not the SQL
// plan.
//
// Skips cleanly when AGENT_MEMORY_PG_URL is unset (same
// convention as feedback_integration_test.go).

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/mgmtapi"
	"github.com/smartpcr/code-intelligence/services/agent-memory/migrations"
)

const (
	readIntEnvPGURL     = "AGENT_MEMORY_PG_URL"
	readIntTimeout      = 60 * time.Second
	readIntBearer       = "dev-read-e2e-token"
	readIntSubject      = "read-e2e-op"
	readIntHeadSHA      = "1111111111111111111111111111111111111111"
	readIntDeltaSHA     = "2222222222222222222222222222222222222222"
	readIntPostRetireSHA = "3333333333333333333333333333333333333333"
)

// readIntFixture mirrors feedbackIntFixture (random per-test
// schema, search_path baked into libpq options, migrations
// run against the test schema, partman.part_config cleanup on
// teardown). Duplicated rather than extracted to keep each
// integration suite editable in isolation.
type readIntFixture struct {
	db      *sql.DB
	schema  string
	cleanup func()
}

func openReadIntFixture(t *testing.T) *readIntFixture {
	t.Helper()
	base := os.Getenv(readIntEnvPGURL)
	if base == "" {
		t.Skipf("skipping: %s is unset; integration tests require a live PostgreSQL", readIntEnvPGURL)
	}

	schema := newReadIntSchemaName(t)
	dsn, err := feedbackIntDSNWithSearchPath(base, schema)
	if err != nil {
		t.Fatalf("dsnWithSearchPath: %v", err)
	}

	owner, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	owner.SetMaxOpenConns(2)
	owner.SetMaxIdleConns(2)

	ctx, cancel := context.WithTimeout(context.Background(), readIntTimeout)
	defer cancel()
	if err := owner.PingContext(ctx); err != nil {
		_ = owner.Close()
		t.Skipf("skipping: cannot reach PostgreSQL at %s: %v", readIntEnvPGURL, err)
	}
	if _, err := owner.ExecContext(ctx, `CREATE SCHEMA `+quoteFeedbackIntIdent(schema)); err != nil {
		_ = owner.Close()
		t.Fatalf("create schema %q: %v", schema, err)
	}
	for i := 0; i < 2; i++ {
		conn, cerr := owner.Conn(ctx)
		if cerr != nil {
			_ = owner.Close()
			t.Fatalf("pin conn: %v", cerr)
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf(
			`SET search_path TO %s, public, partman`, quoteFeedbackIntIdent(schema),
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
		ctx2, c2 := context.WithTimeout(context.Background(), readIntTimeout)
		defer c2()
		schemaPrefix := strings.ReplaceAll(schema, "_", "#_") + ".%"
		_, _ = owner.ExecContext(ctx2, `
			DELETE FROM partman.part_config
			WHERE parent_table LIKE $1 ESCAPE '#'
		`, schemaPrefix)
		_, _ = owner.ExecContext(ctx2, `DROP SCHEMA `+quoteFeedbackIntIdent(schema)+` CASCADE`)
		_ = owner.Close()
	}
	return &readIntFixture{db: owner, schema: schema, cleanup: cleanup}
}

func newReadIntSchemaName(t *testing.T) string {
	t.Helper()
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "amrde2e_" + hex.EncodeToString(buf[:])
}

// readIntRandHex returns a hex-encoded random buffer for
// uniqueness suffixes (repo URLs, signatures).
func readIntRandHex(t *testing.T, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(buf)
}

// readIntSeed is the full row inventory the test fixture
// inserts before exercising the read endpoints. Holding the
// ids in a struct lets each assert reach into a known shape
// rather than threading 10+ scalars through every helper.
type readIntSeed struct {
	repoID         string
	contextID      string
	liveNodeID     string
	liveNeighborID string
	retiredNodeID  string
	edgeID         string
	liveEdgeID     string
	parentEpisode  string
	feedbackEp     string
	traceEdgeID    string
	conceptID      string
	versionID      string
	supportID      string
	parentCreated  time.Time
}

// seedReadIntFixture creates the full inventory the read
// tests expect. Composition (in insert order):
//
//   - 1 repo + 1 commit
//   - 1 ingest_jobs row (status=done) so mgmt.read.repos's
//     latest-ingest join surfaces a non-null status.
//   - 2 nodes (one of which is retired) + 1 edge
//   - 1 recall_context_log row referencing all three (live
//     node, retired node, edge) so mgmt.read.context can
//     surface the retired_at_sha badge.
//   - 1 agent Episode + 1 EpisodeUpdate (failure -> human_corrected)
//     so mgmt.read.episodes asserts current_status reflects
//     the update.
//   - 1 Observation under the Episode so mgmt.read.observations
//     asserts the partition-prune path.
//   - 1 concept + 1 concept_version (promoted=true) + 1
//     concept_support so mgmt.read.concepts / concept_supports
//     return rows.
//   - 1 trace_observation aggregate + 3 trace_observation_log
//     rows so mgmt.read.trace_observation pagination + tail
//     fire.
func seedReadIntFixture(t *testing.T, fx *readIntFixture) *readIntSeed {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), readIntTimeout)
	defer cancel()

	s := &readIntSeed{}

	// repo + commit + ingest job.
	repoURL := fmt.Sprintf("https://example.test/read-e2e-%s", readIntRandHex(t, 4))
	if err := fx.db.QueryRowContext(ctx, `
		INSERT INTO repo (url, default_branch, current_head_sha, language_hints)
		VALUES ($1, 'main', $2, ARRAY['go']::text[])
		RETURNING repo_id::text
	`, repoURL, readIntHeadSHA).Scan(&s.repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	if _, err := fx.db.ExecContext(ctx, `
		INSERT INTO repo_commit (repo_id, sha, parent_sha, committed_at, index_status)
		VALUES ($1::uuid, $2, NULL, now(), 'indexed')
	`, s.repoID, readIntHeadSHA); err != nil {
		t.Fatalf("seed repo_commit: %v", err)
	}
	// Two more commits so SHA-pinned graph_node tests can
	// walk repo_commit.parent_sha: delta is child of HEAD and
	// is the retired_at_sha for the retired node; post-retire
	// is child of delta and represents a SHA at which the
	// node is tombstoned.
	if _, err := fx.db.ExecContext(ctx, `
		INSERT INTO repo_commit (repo_id, sha, parent_sha, committed_at, index_status)
		VALUES ($1::uuid, $2, $3, now(), 'indexed')
	`, s.repoID, readIntDeltaSHA, readIntHeadSHA); err != nil {
		t.Fatalf("seed repo_commit delta: %v", err)
	}
	if _, err := fx.db.ExecContext(ctx, `
		INSERT INTO repo_commit (repo_id, sha, parent_sha, committed_at, index_status)
		VALUES ($1::uuid, $2, $3, now(), 'indexed')
	`, s.repoID, readIntPostRetireSHA, readIntDeltaSHA); err != nil {
		t.Fatalf("seed repo_commit post-retire: %v", err)
	}
	if _, err := fx.db.ExecContext(ctx, `
		INSERT INTO ingest_jobs (repo_id, mode, to_sha, status)
		VALUES ($1::uuid, 'full'::ingest_mode, $2, 'done'::ingest_status)
	`, s.repoID, readIntHeadSHA); err != nil {
		t.Fatalf("seed ingest_jobs: %v", err)
	}

	// Two nodes; the second gets a tombstone.
	liveFP := make([]byte, 32)
	retFP := make([]byte, 32)
	_, _ = rand.Read(liveFP)
	_, _ = rand.Read(retFP)
	if err := fx.db.QueryRowContext(ctx, `
		INSERT INTO node (repo_id, kind, canonical_signature, fingerprint, from_sha)
		VALUES ($1::uuid, 'method', $2, $3::bytea, $4)
		RETURNING node_id::text
	`, s.repoID, "live-sig-"+readIntRandHex(t, 3), liveFP, readIntHeadSHA).Scan(&s.liveNodeID); err != nil {
		t.Fatalf("seed live node: %v", err)
	}
	if err := fx.db.QueryRowContext(ctx, `
		INSERT INTO node (repo_id, kind, canonical_signature, fingerprint, from_sha)
		VALUES ($1::uuid, 'method', $2, $3::bytea, $4)
		RETURNING node_id::text
	`, s.repoID, "ret-sig-"+readIntRandHex(t, 3), retFP, readIntHeadSHA).Scan(&s.retiredNodeID); err != nil {
		t.Fatalf("seed retired node: %v", err)
	}
	if _, err := fx.db.ExecContext(ctx, `
		INSERT INTO node_retirement (node_id, retired_at_sha)
		VALUES ($1::uuid, $2)
	`, s.retiredNodeID, readIntDeltaSHA); err != nil {
		t.Fatalf("seed node_retirement: %v", err)
	}

	// Third node that stays live so the current-view
	// neighbor query (which anti-joins retired neighbors)
	// still has an edge to assert against.
	liveNeighborFP := make([]byte, 32)
	_, _ = rand.Read(liveNeighborFP)
	if err := fx.db.QueryRowContext(ctx, `
		INSERT INTO node (repo_id, kind, canonical_signature, fingerprint, from_sha)
		VALUES ($1::uuid, 'method', $2, $3::bytea, $4)
		RETURNING node_id::text
	`, s.repoID, "live2-sig-"+readIntRandHex(t, 3), liveNeighborFP, readIntHeadSHA).Scan(&s.liveNeighborID); err != nil {
		t.Fatalf("seed live neighbor node: %v", err)
	}

	// One edge from live -> retired so SHA-pinned queries at
	// older shas (when the retired node was still alive) can
	// observe it; in the current view it must be filtered.
	edgeFP := make([]byte, 32)
	_, _ = rand.Read(edgeFP)
	if err := fx.db.QueryRowContext(ctx, `
		INSERT INTO edge (repo_id, kind, src_node_id, dst_node_id, fingerprint, from_sha)
		VALUES ($1::uuid, 'static_calls'::edge_kind, $2::uuid, $3::uuid, $4::bytea, $5)
		RETURNING edge_id::text
	`, s.repoID, s.liveNodeID, s.retiredNodeID, edgeFP, readIntHeadSHA).Scan(&s.edgeID); err != nil {
		t.Fatalf("seed edge: %v", err)
	}
	// Edge from live -> live2 that stays current under the
	// anti-join (neither endpoint nor edge is retired).
	liveEdgeFP := make([]byte, 32)
	_, _ = rand.Read(liveEdgeFP)
	if err := fx.db.QueryRowContext(ctx, `
		INSERT INTO edge (repo_id, kind, src_node_id, dst_node_id, fingerprint, from_sha)
		VALUES ($1::uuid, 'static_calls'::edge_kind, $2::uuid, $3::uuid, $4::bytea, $5)
		RETURNING edge_id::text
	`, s.repoID, s.liveNodeID, s.liveNeighborID, liveEdgeFP, readIntHeadSHA).Scan(&s.liveEdgeID); err != nil {
		t.Fatalf("seed live edge: %v", err)
	}

	// Recall context referencing live, retired (so we can
	// assert retired_at_sha surfacing), and edge.
	if err := fx.db.QueryRowContext(ctx, `
		INSERT INTO recall_context_log
		    (repo_id, verb, query_json,
		     node_ids, edge_ids, concept_ids,
		     reranker_model_version, served_under_degraded)
		VALUES ($1::uuid, 'recall'::verb, $2::jsonb,
		        ARRAY[$3::uuid, $4::uuid]::uuid[],
		        ARRAY[$5::uuid]::uuid[],
		        ARRAY[]::uuid[],
		        'rerank-v1', false)
		RETURNING context_id::text
	`, s.repoID, `{"q":"test"}`,
		s.liveNodeID, s.retiredNodeID, s.edgeID,
	).Scan(&s.contextID); err != nil {
		t.Fatalf("seed recall_context_log: %v", err)
	}

	// Parent agent Episode with an EpisodeUpdate.
	if err := fx.db.QueryRowContext(ctx, `
		INSERT INTO episode
		    (episode_group_id, repo_id, session_id, trace_id, kind,
		     context_id, action, outcome)
		VALUES (gen_random_uuid(), $1::uuid, 'sess-1', 'trace-1', 'agent'::episode_kind,
		        $2::uuid, '{"op":"parent"}'::jsonb, 'failure'::outcome)
		RETURNING episode_id::text, created_at
	`, s.repoID, s.contextID).Scan(&s.parentEpisode, &s.parentCreated); err != nil {
		t.Fatalf("seed parent episode: %v", err)
	}
	if _, err := fx.db.ExecContext(ctx, `
		INSERT INTO episode_update (episode_id, new_outcome, note, actor)
		VALUES ($1::uuid, 'human_corrected'::outcome, 'op-flipped', 'operator'::actor)
	`, s.parentEpisode); err != nil {
		t.Fatalf("seed episode_update: %v", err)
	}
	if _, err := fx.db.ExecContext(ctx, `
		INSERT INTO observation
		    (episode_id, role, node_id)
		VALUES ($1::uuid, 'node_hit'::observation_role, $2::uuid)
	`, s.parentEpisode, s.liveNodeID); err != nil {
		t.Fatalf("seed observation: %v", err)
	}

	// Feedback episode (so we can exercise the feedback row
	// shape in mgmt.read.episodes).
	if err := fx.db.QueryRowContext(ctx, `
		INSERT INTO episode
		    (episode_group_id, repo_id, session_id, trace_id, kind,
		     parent_episode_id, action, outcome)
		VALUES (gen_random_uuid(), $1::uuid, 'sess-fb', 'trace-fb', 'feedback'::episode_kind,
		        $2::uuid, '{"op":"feedback"}'::jsonb, 'failure'::outcome)
		RETURNING episode_id::text
	`, s.repoID, s.parentEpisode).Scan(&s.feedbackEp); err != nil {
		t.Fatalf("seed feedback episode: %v", err)
	}

	// Concept + version + support.
	conceptFP := make([]byte, 32)
	_, _ = rand.Read(conceptFP)
	if err := fx.db.QueryRowContext(ctx, `
		INSERT INTO concept (fingerprint, name, description_md)
		VALUES ($1::bytea, $2, 'test concept')
		RETURNING concept_id::text
	`, conceptFP, "test-concept-"+readIntRandHex(t, 3)).Scan(&s.conceptID); err != nil {
		t.Fatalf("seed concept: %v", err)
	}
	if err := fx.db.QueryRowContext(ctx, `
		INSERT INTO concept_version
		    (concept_id, version_index, confidence, confidence_band,
		     support_count, negative_count, producer, producer_run_id, promoted)
		VALUES ($1::uuid, 0, 0.9, 'high'::concept_band,
		        1, 0, 'consolidator'::producer, gen_random_uuid(), true)
		RETURNING concept_version_id::text
	`, s.conceptID).Scan(&s.versionID); err != nil {
		t.Fatalf("seed concept_version: %v", err)
	}
	if err := fx.db.QueryRowContext(ctx, `
		INSERT INTO concept_support
		    (concept_id, concept_version_id, repo_id, node_id, polarity)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, 'positive'::polarity)
		RETURNING support_id::text
	`, s.conceptID, s.versionID, s.repoID, s.liveNodeID).Scan(&s.supportID); err != nil {
		t.Fatalf("seed concept_support: %v", err)
	}

	// Trace observation aggregate + log rows. Use the edge
	// we already seeded so the FK on trace_observation /
	// trace_observation_log is satisfied.
	s.traceEdgeID = s.edgeID
	if _, err := fx.db.ExecContext(ctx, `
		INSERT INTO trace_observation
		    (edge_id, observation_count, p50_latency_ms, p95_latency_ms,
		     latest_span_ref, last_observed_at)
		VALUES ($1::uuid, 3, 12.5, 99.0, 'trace-A/span-3', now())
	`, s.traceEdgeID); err != nil {
		t.Fatalf("seed trace_observation: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := fx.db.ExecContext(ctx, `
			INSERT INTO trace_observation_log
			    (edge_id, trace_id, span_id, started_at, duration_ms)
			VALUES ($1::uuid, $2, $3, now() - ($4 || ' minutes')::interval, $5)
		`, s.traceEdgeID, fmt.Sprintf("trace-%d", i), fmt.Sprintf("span-%d", i), i, float64(10+i)); err != nil {
			t.Fatalf("seed trace_observation_log[%d]: %v", i, err)
		}
	}

	return s
}

// readIntStartServer wires the real mgmtapi.Handler against
// the live fixture and exposes it via httptest. Tests issue
// real HTTP GET requests so the auth middleware + ServeMux
// + handler flow runs end to end.
func readIntStartServer(t *testing.T, fx *readIntFixture) (*httptest.Server, func()) {
	t.Helper()
	h := mgmtapi.NewHandler(fx.db,
		&mgmtapi.StaticBearerVerifier{Secret: readIntBearer, Subject: readIntSubject},
		&staticHeadResolver{sha: readIntHeadSHA},
		mgmtapi.Options{Logger: silentFeedbackIntLogger()},
	)
	mux := http.NewServeMux()
	mux.Handle("/v1/repos", h)
	mux.Handle("/v1/repos/", h)
	mux.Handle("/v1/episodes", h)
	mux.Handle("/v1/episodes/", h)
	mux.Handle("/v1/commits", h)
	mux.Handle("/v1/observations", h)
	mux.Handle("/v1/concepts", h)
	mux.Handle("/v1/concept_supports", h)
	mux.Handle("/v1/context", h)
	mux.Handle("/v1/context/", h)
	mux.Handle("/v1/graph_node", h)
	mux.Handle("/v1/graph_node/", h)
	mux.Handle("/v1/trace_observation", h)
	mux.Handle("/v1/trace_observation/", h)
	srv := httptest.NewServer(mux)
	return srv, srv.Close
}

// readIntGet issues an authenticated GET to `srv.URL + path`
// and returns (statusCode, decoded body as generic map).
func readIntGet(t *testing.T, srv *httptest.Server, path string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+readIntBearer)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) == 0 {
		return resp.StatusCode, nil
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode body: %v body=%q", err, string(body))
	}
	return resp.StatusCode, out
}

// requireDegradedFalse asserts the §6.3 envelope is present
// and flips false (Stage 7.5 always serves false). Test fails
// loudly when missing because the envelope is the contract.
func requireDegradedFalse(t *testing.T, body map[string]any) {
	t.Helper()
	v, ok := body["degraded"]
	if !ok {
		t.Fatalf("missing top-level degraded field; body=%v", body)
	}
	if v != false {
		t.Errorf("degraded=%v; want false", v)
	}
}

// ---------------------------------------------------------------
// Per-endpoint round-trip tests
// ---------------------------------------------------------------

func TestReadInt_repos_roundTripsLatestIngest(t *testing.T) {
	fx := openReadIntFixture(t)
	defer fx.cleanup()
	seed := seedReadIntFixture(t, fx)
	srv, closeSrv := readIntStartServer(t, fx)
	defer closeSrv()

	status, body := readIntGet(t, srv, "/v1/repos")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}
	requireDegradedFalse(t, body)
	repos, ok := body["repos"].([]any)
	if !ok || len(repos) != 1 {
		t.Fatalf("expected 1 repo; got %#v", body["repos"])
	}
	r := repos[0].(map[string]any)
	if r["repo_id"] != seed.repoID {
		t.Errorf("repo_id mismatch: %v != %v", r["repo_id"], seed.repoID)
	}
	if r["latest_ingest_status"] != "done" {
		t.Errorf("expected latest_ingest_status=done; got %v", r["latest_ingest_status"])
	}
}

func TestReadInt_commits_roundTrips(t *testing.T) {
	fx := openReadIntFixture(t)
	defer fx.cleanup()
	seed := seedReadIntFixture(t, fx)
	srv, closeSrv := readIntStartServer(t, fx)
	defer closeSrv()

	status, body := readIntGet(t, srv, "/v1/commits?repo_id="+seed.repoID)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}
	requireDegradedFalse(t, body)
	commits := body["commits"].([]any)
	// The seed inserts HEAD, DELTA, and post-retire commits so
	// SHA-pinned graph_node tests have an ancestor chain to
	// walk; mgmt.read.commits surfaces all three.
	if len(commits) != 3 {
		t.Fatalf("expected 3 commits; got %d: %#v", len(commits), commits)
	}
	seen := map[string]bool{}
	for _, raw := range commits {
		c := raw.(map[string]any)
		sha, _ := c["sha"].(string)
		seen[sha] = true
	}
	for _, want := range []string{readIntHeadSHA, readIntDeltaSHA, readIntPostRetireSHA} {
		if !seen[want] {
			t.Errorf("expected commits to contain %s; got %v", want, seen)
		}
	}
}

func TestReadInt_episodes_currentStatusReflectsLatestUpdate(t *testing.T) {
	fx := openReadIntFixture(t)
	defer fx.cleanup()
	seed := seedReadIntFixture(t, fx)
	srv, closeSrv := readIntStartServer(t, fx)
	defer closeSrv()

	// Use a wide `since=` so the parent Episode is in scope
	// regardless of test wall-clock drift.
	status, body := readIntGet(t, srv, "/v1/episodes?since=24h")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}
	requireDegradedFalse(t, body)
	eps := body["episodes"].([]any)
	if len(eps) < 2 {
		t.Fatalf("expected at least 2 episodes (agent+feedback); got %#v", eps)
	}
	var foundParent bool
	for _, raw := range eps {
		e := raw.(map[string]any)
		if e["episode_id"] != seed.parentEpisode {
			continue
		}
		foundParent = true
		if e["outcome"] != "failure" {
			t.Errorf("parent outcome should remain failure; got %v", e["outcome"])
		}
		if e["current_status"] != "human_corrected" {
			t.Errorf("expected current_status=human_corrected after EpisodeUpdate; got %v", e["current_status"])
		}
	}
	if !foundParent {
		t.Errorf("parent episode %s missing from results", seed.parentEpisode)
	}
}

func TestReadInt_episodes_sinceRequired(t *testing.T) {
	fx := openReadIntFixture(t)
	defer fx.cleanup()
	_ = seedReadIntFixture(t, fx)
	srv, closeSrv := readIntStartServer(t, fx)
	defer closeSrv()

	status, body := readIntGet(t, srv, "/v1/episodes")
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400; got %d body=%v", status, body)
	}
	if body["code"] != "since_required" {
		t.Errorf("expected code=since_required; got %v", body["code"])
	}
}

func TestReadInt_observations_roundTrips(t *testing.T) {
	fx := openReadIntFixture(t)
	defer fx.cleanup()
	seed := seedReadIntFixture(t, fx)
	srv, closeSrv := readIntStartServer(t, fx)
	defer closeSrv()

	status, body := readIntGet(t, srv, "/v1/observations?episode_id="+seed.parentEpisode)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}
	requireDegradedFalse(t, body)
	obs := body["observations"].([]any)
	if len(obs) != 1 {
		t.Fatalf("expected 1 observation; got %#v", obs)
	}
	o := obs[0].(map[string]any)
	if o["role"] != "node_hit" {
		t.Errorf("role mismatch: %v", o["role"])
	}
	if o["node_id"] != seed.liveNodeID {
		t.Errorf("node_id mismatch: %v", o["node_id"])
	}
}

func TestReadInt_context_surfacesRetiredAtSHA(t *testing.T) {
	fx := openReadIntFixture(t)
	defer fx.cleanup()
	seed := seedReadIntFixture(t, fx)
	srv, closeSrv := readIntStartServer(t, fx)
	defer closeSrv()

	status, body := readIntGet(t, srv, "/v1/context/"+seed.contextID)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}
	requireDegradedFalse(t, body)
	nodes := body["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes in context; got %#v", nodes)
	}

	// Locate the retired node and assert the badge.
	var sawRetiredBadge bool
	for _, raw := range nodes {
		n := raw.(map[string]any)
		if n["node_id"] != seed.retiredNodeID {
			continue
		}
		if n["retired_at_sha"] != readIntDeltaSHA {
			t.Errorf("expected retired_at_sha=%s on retired node; got %v",
				readIntDeltaSHA, n["retired_at_sha"])
		}
		sawRetiredBadge = true
	}
	if !sawRetiredBadge {
		t.Errorf("retired node %s missing from context nodes", seed.retiredNodeID)
	}

	edges := body["edges"].([]any)
	if len(edges) != 1 {
		t.Errorf("expected 1 edge; got %#v", edges)
	}
}

func TestReadInt_concepts_returnsLatestVersion(t *testing.T) {
	fx := openReadIntFixture(t)
	defer fx.cleanup()
	seed := seedReadIntFixture(t, fx)
	srv, closeSrv := readIntStartServer(t, fx)
	defer closeSrv()

	status, body := readIntGet(t, srv, "/v1/concepts?promoted=true")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}
	requireDegradedFalse(t, body)
	cs := body["concepts"].([]any)
	if len(cs) != 1 {
		t.Fatalf("expected 1 promoted concept; got %#v", cs)
	}
	c := cs[0].(map[string]any)
	if c["concept_id"] != seed.conceptID {
		t.Errorf("concept_id mismatch: %v != %v", c["concept_id"], seed.conceptID)
	}
	if c["latest_promoted"] != true {
		t.Errorf("expected latest_promoted=true; got %v", c["latest_promoted"])
	}
	if c["latest_confidence_band"] != "high" {
		t.Errorf("expected latest_confidence_band=high; got %v", c["latest_confidence_band"])
	}
}

func TestReadInt_conceptSupports_roundTrips(t *testing.T) {
	fx := openReadIntFixture(t)
	defer fx.cleanup()
	seed := seedReadIntFixture(t, fx)
	srv, closeSrv := readIntStartServer(t, fx)
	defer closeSrv()

	status, body := readIntGet(t, srv, "/v1/concept_supports?concept_id="+seed.conceptID)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}
	requireDegradedFalse(t, body)
	sups := body["supports"].([]any)
	if len(sups) != 1 {
		t.Fatalf("expected 1 support; got %#v", sups)
	}
	s := sups[0].(map[string]any)
	if s["support_id"] != seed.supportID {
		t.Errorf("support_id mismatch: %v", s["support_id"])
	}
}

func TestReadInt_graphNode_invalidShaShape_returns400(t *testing.T) {
	fx := openReadIntFixture(t)
	defer fx.cleanup()
	seed := seedReadIntFixture(t, fx)
	srv, closeSrv := readIntStartServer(t, fx)
	defer closeSrv()

	status, body := readIntGet(t, srv,
		"/v1/graph_node/"+seed.liveNodeID+"?sha=not-a-sha")
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400; got %d body=%v", status, body)
	}
	if body["code"] != "invalid_sha" {
		t.Errorf("expected code=invalid_sha; got %v", body["code"])
	}
}

func TestReadInt_graphNode_unknownSha_returns404(t *testing.T) {
	fx := openReadIntFixture(t)
	defer fx.cleanup()
	seed := seedReadIntFixture(t, fx)
	srv, closeSrv := readIntStartServer(t, fx)
	defer closeSrv()

	const unknownSha = "9999999999999999999999999999999999999999"
	status, body := readIntGet(t, srv,
		"/v1/graph_node/"+seed.liveNodeID+"?sha="+unknownSha)
	if status != http.StatusNotFound {
		t.Fatalf("expected 404; got %d body=%v", status, body)
	}
	if body["code"] != "unknown_sha" {
		t.Errorf("expected code=unknown_sha; got %v", body["code"])
	}
}

func TestReadInt_graphNode_neighborsCurrentViewAntiJoinsRetired(t *testing.T) {
	fx := openReadIntFixture(t)
	defer fx.cleanup()
	seed := seedReadIntFixture(t, fx)
	srv, closeSrv := readIntStartServer(t, fx)
	defer closeSrv()

	// Current view: edge -> retired node MUST be filtered;
	// only the edge -> live neighbor remains.
	status, body := readIntGet(t, srv, "/v1/graph_node/"+seed.liveNodeID)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}
	requireDegradedFalse(t, body)
	if body["retired_at_sha"] != nil && body["retired_at_sha"] != "" {
		t.Errorf("live node should not carry retired_at_sha; got %v", body["retired_at_sha"])
	}
	out := body["outgoing_edges"].([]any)
	if len(out) != 1 {
		t.Fatalf("expected 1 outgoing edge after anti-join; got %#v", out)
	}
	e := out[0].(map[string]any)
	if e["edge_id"] != seed.liveEdgeID {
		t.Errorf("expected liveEdgeID %s; got %v", seed.liveEdgeID, e["edge_id"])
	}
	if e["neighbor_node_id"] != seed.liveNeighborID {
		t.Errorf("expected neighbor=liveNeighbor %s; got %v",
			seed.liveNeighborID, e["neighbor_node_id"])
	}

	// Retired node directly (no ?sha=): current view still
	// returns the node + its node_retirement badge.
	status, body = readIntGet(t, srv, "/v1/graph_node/"+seed.retiredNodeID)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}
	if body["retired_at_sha"] != readIntDeltaSHA {
		t.Errorf("expected retired_at_sha=%s; got %v", readIntDeltaSHA, body["retired_at_sha"])
	}
}

func TestReadInt_graphNode_shaPinned_aliveAtRetirementBoundary(t *testing.T) {
	fx := openReadIntFixture(t)
	defer fx.cleanup()
	seed := seedReadIntFixture(t, fx)
	srv, closeSrv := readIntStartServer(t, fx)
	defer closeSrv()

	// At the retired_at_sha itself the node is still alive
	// (the retirement only takes effect at descendant commits).
	status, body := readIntGet(t, srv,
		"/v1/graph_node/"+seed.retiredNodeID+"?sha="+readIntDeltaSHA)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}
	requireDegradedFalse(t, body)
	if v := body["retired_at_sha"]; v != nil && v != "" {
		t.Errorf("expected no tombstone badge at retirement boundary; got %v", v)
	}
}

func TestReadInt_graphNode_shaPinned_tombstonedAtDescendant(t *testing.T) {
	fx := openReadIntFixture(t)
	defer fx.cleanup()
	seed := seedReadIntFixture(t, fx)
	srv, closeSrv := readIntStartServer(t, fx)
	defer closeSrv()

	// At post-retire SHA (descendant of retired_at_sha) the
	// node is tombstoned and the response surfaces the badge.
	status, body := readIntGet(t, srv,
		"/v1/graph_node/"+seed.retiredNodeID+"?sha="+readIntPostRetireSHA)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}
	if body["retired_at_sha"] != readIntDeltaSHA {
		t.Errorf("expected retired_at_sha=%s; got %v", readIntDeltaSHA, body["retired_at_sha"])
	}
}

func TestReadInt_graphNode_shaPinned_neighborsFilteredAtDescendant(t *testing.T) {
	fx := openReadIntFixture(t)
	defer fx.cleanup()
	seed := seedReadIntFixture(t, fx)
	srv, closeSrv := readIntStartServer(t, fx)
	defer closeSrv()

	// At post-retire SHA the live node still exists; its
	// outgoing edges include only the live neighbor because
	// the retired neighbor is gone by then.
	status, body := readIntGet(t, srv,
		"/v1/graph_node/"+seed.liveNodeID+"?sha="+readIntPostRetireSHA)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}
	out := body["outgoing_edges"].([]any)
	if len(out) != 1 {
		t.Fatalf("expected exactly 1 outgoing edge; got %#v", out)
	}
	e := out[0].(map[string]any)
	if e["neighbor_node_id"] != seed.liveNeighborID {
		t.Errorf("expected liveNeighbor as only neighbor at post-retire SHA; got %v",
			e["neighbor_node_id"])
	}
}

func TestReadInt_graphNode_barePath_returns404(t *testing.T) {
	fx := openReadIntFixture(t)
	defer fx.cleanup()
	_ = seedReadIntFixture(t, fx)
	srv, closeSrv := readIntStartServer(t, fx)
	defer closeSrv()

	status, body := readIntGet(t, srv, "/v1/graph_node")
	if status != http.StatusNotFound {
		t.Fatalf("expected 404; got %d body=%v", status, body)
	}
	if body["code"] != "node_id_required" {
		t.Errorf("expected code=node_id_required; got %v", body["code"])
	}
}

func TestReadInt_context_barePath_returns404(t *testing.T) {
	fx := openReadIntFixture(t)
	defer fx.cleanup()
	_ = seedReadIntFixture(t, fx)
	srv, closeSrv := readIntStartServer(t, fx)
	defer closeSrv()

	status, body := readIntGet(t, srv, "/v1/context")
	if status != http.StatusNotFound {
		t.Fatalf("expected 404; got %d body=%v", status, body)
	}
	if body["code"] != "context_id_required" {
		t.Errorf("expected code=context_id_required; got %v", body["code"])
	}
}

func TestReadInt_traceObservation_barePath_returns404(t *testing.T) {
	fx := openReadIntFixture(t)
	defer fx.cleanup()
	_ = seedReadIntFixture(t, fx)
	srv, closeSrv := readIntStartServer(t, fx)
	defer closeSrv()

	status, body := readIntGet(t, srv, "/v1/trace_observation")
	if status != http.StatusNotFound {
		t.Fatalf("expected 404; got %d body=%v", status, body)
	}
	if body["code"] != "edge_id_required" {
		t.Errorf("expected code=edge_id_required; got %v", body["code"])
	}
}

func TestReadInt_traceObservation_roundTripsTail(t *testing.T) {
	fx := openReadIntFixture(t)
	defer fx.cleanup()
	seed := seedReadIntFixture(t, fx)
	srv, closeSrv := readIntStartServer(t, fx)
	defer closeSrv()

	status, body := readIntGet(t, srv, "/v1/trace_observation/"+seed.traceEdgeID+"?limit=2")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}
	requireDegradedFalse(t, body)
	if body["observation_count"] != float64(3) {
		t.Errorf("observation_count mismatch: %v", body["observation_count"])
	}
	tail := body["tail"].([]any)
	if len(tail) != 2 {
		t.Errorf("expected 2 tail rows; got %d", len(tail))
	}
	// 3 rows total, limit=2 -> next_offset=2 emitted.
	if body["next_offset"] != float64(2) {
		t.Errorf("expected next_offset=2; got %v", body["next_offset"])
	}
}
