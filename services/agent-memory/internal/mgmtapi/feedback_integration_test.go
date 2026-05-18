package mgmtapi_test

// End-to-end integration test for the Stage 7.3 `mgmt.feedback`
// verb. implementation-plan.md §7.3:
//
//	"Add an end-to-end test that asserts the full §7.3 wire
//	 flow lands the expected three Episodes
//	 (`agent` / `feedback` / `synthetic_positive`)."
//
// This test wires the REAL HTTP handler against a live
// PostgreSQL 16 + pg_partman v5 schema, calls POST
// /v1/episodes/{parent_id}/feedback through httptest, then
// runs ONE Consolidator Tick and asserts the three Episodes
// exist with the correct kind / outcome / provenance shape.
//
// The test is the cross-package counterpart to the in-package
// behavioural unit tests in feedback_unit_test.go (which prove
// the handler's wire / validation / DB-row-shape behaviour
// against go-sqlmock) and the consolidator integration test
// `TestTick_correctionYieldsSyntheticPositive` (which proves
// the Consolidator's promotion rule against rows seeded via a
// hand-written SQL stand-in). Together they pin the full
// architecture.md §4.4 / §7.3 / §7.7 step-4 chain end to end:
//
//	operator POST -> mgmtapi handler -> feedback Episode + EpisodeUpdate
//	                                     |
//	                                     v
//	                              Consolidator Tick -> synthetic_positive
//
// Skips cleanly when AGENT_MEMORY_PG_URL is unset, mirroring
// the convention in every other *_integration_test.go in this
// service (migrations/test_migrate_test.go,
// internal/webhookreceiver/handler_integration_test.go,
// internal/consolidator/service_integration_test.go).

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/consolidator"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/mgmtapi"
	"github.com/smartpcr/code-intelligence/services/agent-memory/migrations"
)

const (
	feedbackIntEnvPGURL   = "AGENT_MEMORY_PG_URL"
	feedbackIntTimeout    = 60 * time.Second
	feedbackIntBearer     = "dev-feedback-e2e-token"
	feedbackIntSubject    = "feedback-e2e-op"
	feedbackIntHeadSHA    = "0000000000000000000000000000000000000000"
	feedbackIntLockKeyBit = int64(0x4000_0000_0000_0000)
)

// feedbackIntFixture is the per-test PostgreSQL substrate.
// Mirrors the consolidator / webhookreceiver fixture pattern:
//   - random per-test schema (avoids cross-test interference
//     when `go test -p N` runs the suite in parallel).
//   - search_path baked into the DSN's libpq `options=` so
//     EVERY backend connection acquired from the pool lands
//     in the test schema, even after a reconnect.
//   - migrations run by the same pool, so the schema carries
//     the production shape (partitioned tables, pg_partman
//     setup, every enum, every check constraint).
type feedbackIntFixture struct {
	db      *sql.DB
	schema  string
	cleanup func()
}

func openFeedbackIntFixture(t *testing.T) *feedbackIntFixture {
	t.Helper()
	base := os.Getenv(feedbackIntEnvPGURL)
	if base == "" {
		t.Skipf("skipping: %s is unset; integration tests require a live PostgreSQL", feedbackIntEnvPGURL)
	}

	schema := newFeedbackIntSchemaName(t)
	schemaDSN, err := feedbackIntDSNWithSearchPath(base, schema)
	if err != nil {
		t.Fatalf("dsnWithSearchPath: %v", err)
	}

	owner, err := sql.Open("postgres", schemaDSN)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// The Consolidator's emission phase pins one conn for the
	// advisory lock + candidate scan while a second conn runs
	// the finalize UPDATE; capping the pool below 2 would
	// deadlock the Tick that follows the handler's POST.
	// MaxOpenConns=2 matches consolidator/service_integration_test.go.
	owner.SetMaxOpenConns(2)
	owner.SetMaxIdleConns(2)

	ctx, cancel := context.WithTimeout(context.Background(), feedbackIntTimeout)
	defer cancel()
	if err := owner.PingContext(ctx); err != nil {
		_ = owner.Close()
		t.Skipf("skipping: cannot reach PostgreSQL at %s: %v", feedbackIntEnvPGURL, err)
	}
	if _, err := owner.ExecContext(ctx, `CREATE SCHEMA `+quoteFeedbackIntIdent(schema)); err != nil {
		_ = owner.Close()
		t.Fatalf("create schema %q: %v", schema, err)
	}
	// Belt + suspenders: re-apply search_path on the owner
	// pool's two starter connections so migrations.Up runs in
	// the right schema even if libpq's `options=` were
	// stripped by an exotic pooler.
	for i := 0; i < 2; i++ {
		conn, cerr := owner.Conn(ctx)
		if cerr != nil {
			_ = owner.Close()
			t.Fatalf("pin conn for SET search_path: %v", cerr)
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
		ctx2, c2 := context.WithTimeout(context.Background(), feedbackIntTimeout)
		defer c2()
		schemaPrefix := strings.ReplaceAll(schema, "_", "#_") + ".%"
		_, _ = owner.ExecContext(ctx2, `
			DELETE FROM partman.part_config
			WHERE parent_table LIKE $1 ESCAPE '#'
		`, schemaPrefix)
		_, _ = owner.ExecContext(ctx2, `DROP SCHEMA `+quoteFeedbackIntIdent(schema)+` CASCADE`)
		_ = owner.Close()
	}
	return &feedbackIntFixture{db: owner, schema: schema, cleanup: cleanup}
}

func newFeedbackIntSchemaName(t *testing.T) string {
	t.Helper()
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "amfbe2e_" + hex.EncodeToString(buf[:])
}

func quoteFeedbackIntIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// feedbackIntDSNWithSearchPath bakes the per-test schema into
// libpq's `options=-c search_path=...` startup parameter so
// every backend connection acquired with the returned DSN
// lands in the test schema at session start. Without this, a
// reconnect during the test (driver retry, transient network
// blip) would land on a connection whose search_path is the
// default `public` -- silently writing into / reading from the
// wrong schema.
//
// Mirrors webhookreceiver/handler_integration_test.go's
// `dsnWithSearchPath` (kept local so the two integration
// suites can be edited independently).
func feedbackIntDSNWithSearchPath(base, schema string) (string, error) {
	for _, r := range schema {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_') {
			return "", fmt.Errorf(
				"refusing to bake unsafe schema name into libpq options: %q", schema)
		}
	}
	formatted := "-c search_path=" + schema + ",public,partman"

	if strings.HasPrefix(base, "postgres://") || strings.HasPrefix(base, "postgresql://") {
		u, err := url.Parse(base)
		if err != nil {
			return "", fmt.Errorf("parse URL DSN: %w", err)
		}
		q := u.Query()
		if existing := q.Get("options"); existing != "" {
			return "", fmt.Errorf(
				"refusing to clobber existing libpq options=%q on DSN", existing)
		}
		q.Set("options", formatted)
		u.RawQuery = q.Encode()
		return u.String(), nil
	}

	for _, tok := range strings.Fields(base) {
		if strings.HasPrefix(tok, "options=") || strings.HasPrefix(tok, "options ") {
			return "", fmt.Errorf(
				"refusing to clobber existing libpq options token on DSN: %q", tok)
		}
	}
	return base + " options='" + formatted + "'", nil
}

// staticHeadResolver is the no-op resolver this test uses --
// mgmt.feedback never invokes it (the verb operates on an
// existing parent_id, not a new repo), but mgmtapi.NewHandler
// panics on a nil resolver so a constructor stand-in is
// required.
type staticHeadResolver struct{ sha string }

func (r *staticHeadResolver) Resolve(_ context.Context, _, _ string) (string, error) {
	return r.sha, nil
}

func silentFeedbackIntLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// ────────────────────────────────────────────────────────────
// Seed helpers (duplicated from consolidator/service_integration_test
// to keep this test self-contained; the duplication is
// intentional -- internal package tests cannot share helpers
// across packages without exposing them on the public surface).
// ────────────────────────────────────────────────────────────

func seedFeedbackIntRepo(ctx context.Context, t *testing.T, db *sql.DB) string {
	t.Helper()
	var repoID string
	url := fmt.Sprintf("https://example.test/feedback-e2e-%s", randFeedbackIntHex(t, 4))
	if err := db.QueryRowContext(ctx, `
		INSERT INTO repo (url, default_branch, current_head_sha, language_hints)
		VALUES ($1, 'main', $2, ARRAY['go']::text[])
		RETURNING repo_id::text
	`, url, feedbackIntHeadSHA).Scan(&repoID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	return repoID
}

func seedFeedbackIntRecallContext(ctx context.Context, t *testing.T, db *sql.DB, repoID string) string {
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

func seedFeedbackIntNode(ctx context.Context, t *testing.T, db *sql.DB, repoID string) string {
	t.Helper()
	fp := make([]byte, 32)
	if _, err := rand.Read(fp); err != nil {
		t.Fatalf("rand: %v", err)
	}
	var nodeID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO node (repo_id, kind, canonical_signature, fingerprint, from_sha)
		VALUES ($1::uuid, 'method', $2, $3::bytea, $4)
		RETURNING node_id::text
	`, repoID, "sig-feedback-e2e-"+randFeedbackIntHex(t, 4), fp, feedbackIntHeadSHA).Scan(&nodeID); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	return nodeID
}

// seedFeedbackIntParentEpisode inserts a kind='agent',
// outcome='success' Episode plus one node_hit Observation
// pointing at nodeID. Returns the parent episode_id. The
// outcome is 'success' because the spec only promotes
// synthetic positives off PARENTS the operator overrides --
// the parent's polarity (positive / negative) is irrelevant
// to the §7.3 wire flow under test.
func seedFeedbackIntParentEpisode(
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
		        $4::uuid, '{"op":"parent"}'::jsonb, 'success'::outcome)
		RETURNING episode_id::text
	`, repoID, "sess-parent-"+randFeedbackIntHex(t, 4),
		"trace-parent-"+randFeedbackIntHex(t, 4), contextID).Scan(&epID); err != nil {
		t.Fatalf("seed parent episode: %v", err)
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

func randFeedbackIntHex(t *testing.T, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(buf)
}

// ────────────────────────────────────────────────────────────
// Assertion helpers
// ────────────────────────────────────────────────────────────

// countEpisodesOfKind returns the number of `episode` rows
// for the given (kind, parentEpisodeID). Used to assert the
// exact-cardinality §7.3 outcome:
//
//	1 agent  + 1 feedback + 1 synthetic_positive == 3 episodes.
//
// `parentRelationship` selects which provenance column to
// match the parent on:
//
//	"self"            -> the parent itself (episode_id = parentID)
//	"parent_episode"  -> direct parent_episode_id  = parentID (feedback)
//	"synth_parent"    -> synthesized_from_parent_episode_id = parentID
func countEpisodesOfKind(
	ctx context.Context, t *testing.T, db *sql.DB,
	kind, parentEpisodeID, parentRelationship string,
) int {
	t.Helper()
	var query string
	switch parentRelationship {
	case "self":
		query = `SELECT count(*) FROM episode
		           WHERE kind = $1::episode_kind AND episode_id = $2::uuid`
	case "parent_episode":
		query = `SELECT count(*) FROM episode
		           WHERE kind = $1::episode_kind AND parent_episode_id = $2::uuid`
	case "synth_parent":
		query = `SELECT count(*) FROM episode
		           WHERE kind = $1::episode_kind
		             AND synthesized_from_parent_episode_id = $2::uuid`
	default:
		t.Fatalf("unknown parentRelationship %q", parentRelationship)
	}
	var n int
	if err := db.QueryRowContext(ctx, query, kind, parentEpisodeID).Scan(&n); err != nil {
		t.Fatalf("count %s episodes: %v", kind, err)
	}
	return n
}

func countEpisodeUpdates(
	ctx context.Context, t *testing.T, db *sql.DB,
	episodeID, newOutcome, actor string,
) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM episode_update
		 WHERE episode_id = $1::uuid
		   AND new_outcome = $2::outcome
		   AND actor = $3::actor
	`, episodeID, newOutcome, actor).Scan(&n); err != nil {
		t.Fatalf("count episode_update: %v", err)
	}
	return n
}

// ────────────────────────────────────────────────────────────
// The end-to-end test
// ────────────────────────────────────────────────────────────

// TestE2E_FeedbackVerbProducesAllThreeEpisodes exercises the
// FULL §7.3 wire flow:
//
//  1. Seed a kind='agent' parent Episode with one observation.
//  2. POST /v1/episodes/{parent_id}/feedback through the
//     real mgmtapi handler (auth + decode + validate + DB).
//  3. Assert the handler wrote the two §4.4 step-3 rows
//     (feedback Episode + EpisodeUpdate) inside a single tx.
//  4. Run ONE Consolidator Tick (Stage 6.3).
//  5. Assert the synthetic_positive Episode now exists with
//     the correct provenance pointers and that the total
//     Episode count for this parent's lineage is exactly 3
//     (agent + feedback + synthetic_positive).
//
// Skips when AGENT_MEMORY_PG_URL is unset.
func TestE2E_FeedbackVerbProducesAllThreeEpisodes(t *testing.T) {
	fix := openFeedbackIntFixture(t)
	defer fix.cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), feedbackIntTimeout)
	defer cancel()

	// Stage 0: seed the parent agent Episode + its context +
	// one observation. The Consolidator's Stage 6.3 query
	// requires the parent to have kind='agent' (matches the
	// handler's parent-kind gate too) and the synthetic
	// positive's observation-mirror copies the parent's
	// observations -- so seeding one observation lets us
	// assert the mirror copies it across.
	repoID := seedFeedbackIntRepo(ctx, t, fix.db)
	contextID := seedFeedbackIntRecallContext(ctx, t, fix.db, repoID)
	nodeID := seedFeedbackIntNode(ctx, t, fix.db, repoID)
	parentEpisodeID := seedFeedbackIntParentEpisode(ctx, t, fix.db, repoID, contextID, nodeID)

	// Stage 1: build the real HTTP handler against the same
	// per-test schema. The auth verifier accepts a fixed
	// dev token; the head resolver is a no-op stand-in
	// because mgmt.feedback never consults it (it only runs
	// for register / ingest).
	handler := mgmtapi.NewHandler(
		fix.db,
		&mgmtapi.StaticBearerVerifier{Secret: feedbackIntBearer, Subject: feedbackIntSubject},
		&staticHeadResolver{sha: feedbackIntHeadSHA},
		mgmtapi.Options{Logger: silentFeedbackIntLogger()},
	)

	// Stage 2: POST /v1/episodes/{parent_id}/feedback.
	body := mgmtapi.FeedbackRequest{
		Outcome:         "human_corrected",
		CorrectedAction: json.RawMessage(`{"op":"corrected","why":"e2e-§7.3"}`),
		Note:            "stage-7.3 end-to-end test",
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatalf("encode body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost,
		"/v1/episodes/"+parentEpisodeID+"/feedback", &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+feedbackIntBearer)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("handler status = %d, want %d. body=%q",
			w.Code, http.StatusCreated, w.Body.String())
	}
	var resp mgmtapi.FeedbackResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%q)", err, w.Body.String())
	}
	if resp.FeedbackEpisodeID == "" {
		t.Fatalf("response.feedback_episode_id is empty: %q", w.Body.String())
	}

	// Stage 3: post-handler assertions. The handler must
	// have written exactly:
	//   * 1 kind='feedback' Episode whose parent_episode_id
	//     matches the seeded parent.
	//   * 1 episode_update(new_outcome=human_corrected,
	//     actor=operator) on the parent.
	// And it must NOT have touched the parent's columns
	// (G3 -- the parent row is immutable).
	if n := countEpisodesOfKind(ctx, t, fix.db, "feedback", parentEpisodeID, "parent_episode"); n != 1 {
		t.Fatalf("expected 1 feedback Episode for parent, got %d", n)
	}
	if n := countEpisodeUpdates(ctx, t, fix.db, parentEpisodeID, "human_corrected", "operator"); n != 1 {
		t.Fatalf("expected 1 episode_update(human_corrected, operator), got %d", n)
	}
	// Parent's own outcome is unchanged (G3).
	var parentOutcome string
	if err := fix.db.QueryRowContext(ctx, `
		SELECT outcome::text FROM episode WHERE episode_id = $1::uuid
	`, parentEpisodeID).Scan(&parentOutcome); err != nil {
		t.Fatalf("read parent outcome: %v", err)
	}
	if parentOutcome != "success" {
		t.Fatalf("parent Episode mutated: outcome=%q, want unchanged 'success' (G3 violation)", parentOutcome)
	}
	// Before Tick: ZERO synthetic_positives -- the
	// Consolidator has not run yet, so the handler MUST NOT
	// be doing the promotion inline.
	if n := countEpisodesOfKind(ctx, t, fix.db, "synthetic_positive", parentEpisodeID, "synth_parent"); n != 0 {
		t.Fatalf("expected 0 synthetic_positive BEFORE Tick (Stage 6.3 owns promotion), got %d", n)
	}

	// Stage 4: run ONE Consolidator Tick. Threshold=1 so the
	// concept-promotion gate cannot mask a synthetic-positive
	// regression; AdvisoryLockKey uses the per-test high-bit
	// space so concurrent integration runs in the same
	// cluster do not serialise on a shared global lock.
	consolSvc, err := consolidator.New(fix.db, consolidator.Config{
		Threshold:       1,
		RunInterval:     time.Second,
		TickTimeout:     feedbackIntTimeout,
		AdvisoryLockKey: feedbackIntLockKeyBit | int64(time.Now().UnixNano()),
	}, silentFeedbackIntLogger())
	if err != nil {
		t.Fatalf("consolidator.New: %v", err)
	}
	tickRes, err := consolSvc.Tick(ctx)
	if err != nil {
		t.Fatalf("consolidator.Tick: %v", err)
	}
	if tickRes.LockSkipped {
		t.Fatalf("unexpected lock-skip on a fresh schema; tick=%+v", tickRes)
	}
	if tickRes.SyntheticPositivesCreated != 1 {
		t.Fatalf("expected 1 synthetic_positive created in Tick, got %d (tick=%+v)",
			tickRes.SyntheticPositivesCreated, tickRes)
	}

	// Stage 5: post-Tick assertions -- the §7.3 acceptance
	// statement spelled out as three counts.
	if n := countEpisodesOfKind(ctx, t, fix.db, "agent", parentEpisodeID, "self"); n != 1 {
		t.Fatalf("agent Episode count for parent = %d, want 1", n)
	}
	if n := countEpisodesOfKind(ctx, t, fix.db, "feedback", parentEpisodeID, "parent_episode"); n != 1 {
		t.Fatalf("feedback Episode count for parent = %d, want 1", n)
	}
	if n := countEpisodesOfKind(ctx, t, fix.db, "synthetic_positive", parentEpisodeID, "synth_parent"); n != 1 {
		t.Fatalf("synthetic_positive Episode count for parent = %d, want 1", n)
	}

	// And the synthetic_positive's provenance pointers
	// reference the handler-written feedback Episode -- not
	// some other Episode the Consolidator might pick up. This
	// is the wire-level proof that the handler's RETURNING
	// episode_id flows all the way through to the synthetic
	// positive's `synthesized_from_feedback_episode_id`.
	var synthFromFeedback string
	if err := fix.db.QueryRowContext(ctx, `
		SELECT synthesized_from_feedback_episode_id::text
		  FROM episode
		 WHERE kind = 'synthetic_positive'::episode_kind
		   AND synthesized_from_parent_episode_id = $1::uuid
	`, parentEpisodeID).Scan(&synthFromFeedback); err != nil {
		t.Fatalf("read synth provenance: %v", err)
	}
	if synthFromFeedback != resp.FeedbackEpisodeID {
		t.Fatalf("synth.synthesized_from_feedback_episode_id = %q, want handler-returned %q",
			synthFromFeedback, resp.FeedbackEpisodeID)
	}
}
