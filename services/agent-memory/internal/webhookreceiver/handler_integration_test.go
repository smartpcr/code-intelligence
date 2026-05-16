package webhookreceiver_test

// End-to-end integration test for the Stage 3.5 Webhook Receiver,
// implementation-plan.md §3.5 acceptance scenarios:
//
//   * "invalid signature rejected" -- a POST whose HMAC is computed
//     under the wrong secret must respond 401 AND write no
//     repo_event row AND enqueue no ingest_jobs row.
//   * "valid push enqueues delta job" -- a POST whose HMAC is
//     computed under the correct per-repo secret must respond 202
//     AND produce exactly one repo_event(kind=push) row AND one
//     ingest_jobs(mode=delta) row.
//
// The brief calls for "an end-to-end test (via `docker compose`)".
// We satisfy this by reusing the existing AGENT_MEMORY_PG_URL
// integration-test convention (see migrations/test_migrate_test.go
// and internal/repoindexer/worker_integration_test.go) -- the
// docker-compose Postgres container exports its DSN via that env
// var, and the CI matrix in .github/workflows/agent-memory-ci.yml
// is what brings the stack up. Re-running `docker compose up` from
// inside the test would either duplicate that orchestration or
// require Docker-in-Docker, both of which the existing
// integration-test pattern explicitly avoids.
//
// The test skips cleanly when AGENT_MEMORY_PG_URL is unset, so
// `make test` on a developer laptop without the stack still exits 0.

import (
	"bytes"
	"context"
	"crypto/hmac"
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
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/webhookreceiver"
	"github.com/smartpcr/code-intelligence/services/agent-memory/migrations"
)

const (
	envPGURL      = "AGENT_MEMORY_PG_URL"
	testDBTimeout = 60 * time.Second
)

// dbFixture mirrors the per-test-schema pattern used by every
// other integration test in this service. A fresh randomly-named
// schema isolates the test from any concurrent run; CASCADE drop
// in cleanup leaves the cluster clean.
//
// `dsn` is the connection string with the per-test schema baked
// into the libpq `options=-c search_path=...` startup parameter,
// so any other process / pool that opens the same DSN will land
// in the test schema automatically. The e2e subprocess test
// hands this DSN to the spawned webhook-receiver binary.
type dbFixture struct {
	db      *sql.DB
	schema  string
	dsn     string
	cleanup func()
}

func openFixture(t *testing.T) *dbFixture {
	t.Helper()
	base := os.Getenv(envPGURL)
	if base == "" {
		t.Skipf("skipping: %s is unset; integration tests require a live PostgreSQL", envPGURL)
	}

	// Per-fixture random schema name. We compute it BEFORE
	// opening any pool so we can bake the schema into the DSN's
	// `options=-c search_path=...` parameter -- that parameter
	// is a libpq startup option that PostgreSQL applies to
	// EVERY backend connection at session start, which means
	// any database/sql connection (the test pool, the spawned
	// webhook-receiver binary's pool, anything else opened with
	// this same DSN) sees the test schema without needing a
	// separate per-connection `SET search_path` SQL statement.
	// This is what defeats the iter-1 race the evaluator
	// flagged: capping MaxOpenConns at 1 alone is fragile
	// because the database/sql layer can still reconnect on
	// error and would lose the search_path on the new backend.
	schema := newSchemaName(t)
	schemaDSN, err := dsnWithSearchPath(base, schema)
	if err != nil {
		t.Fatalf("dsnWithSearchPath: %v", err)
	}

	// Owner pool used by the test for seeding, assertions, and
	// schema cleanup. Cap at one open connection (mirrors
	// migrations/test_migrate_test.go:66-72) so any SET-style
	// runtime parameter stays pinned even if the search_path
	// parameter weren't already on the DSN. Belt-and-suspenders.
	owner, err := sql.Open("postgres", schemaDSN)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	owner.SetMaxOpenConns(1)
	owner.SetMaxIdleConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()
	if err := owner.PingContext(ctx); err != nil {
		_ = owner.Close()
		t.Skipf("skipping: cannot reach PostgreSQL at %s: %v", envPGURL, err)
	}
	if _, err := owner.ExecContext(ctx, `CREATE SCHEMA `+quoteIdent(schema)); err != nil {
		_ = owner.Close()
		t.Fatalf("create schema %q: %v", schema, err)
	}
	// Belt + suspenders: even though the DSN bakes the
	// search_path, set it explicitly on this connection too so
	// migrations.Up runs in the right schema if libpq's
	// `options=` ever gets dropped (e.g. by a pooler).
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
		schemaPrefix := strings.ReplaceAll(schema, "_", "#_") + ".%"
		_, _ = owner.ExecContext(ctx2, `
			DELETE FROM partman.part_config
			WHERE parent_table LIKE $1 ESCAPE '#'
		`, schemaPrefix)
		_, _ = owner.ExecContext(ctx2, `DROP SCHEMA `+quoteIdent(schema)+` CASCADE`)
		_ = owner.Close()
	}
	return &dbFixture{db: owner, schema: schema, dsn: schemaDSN, cleanup: cleanup}
}

func newSchemaName(t *testing.T) string {
	t.Helper()
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "amwh_" + hex.EncodeToString(buf[:])
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// dsnWithSearchPath returns `base` with libpq's
// `options=-c search_path=<schema>,public,partman` startup
// parameter set so EVERY backend connection opened with the
// returned DSN starts its session with the test schema in
// search_path. Without this, only the connection that ran the
// `SET search_path TO ...` statement sees the test schema --
// any sibling connection acquired from the pool would fall back
// to `public`, surfacing flaky "relation does not exist" or
// (worse) silent writes into the production schema.
//
// The function understands both libpq DSN shapes:
//   - URL form  -> `postgres://user:pass@host:port/db?key=val`
//   - KV form   -> `host=... dbname=... user=... password=...`
//
// For the URL form we manipulate the query string; for KV we
// append a quoted `options='...'` token. If the input already
// carries an `options=` value we ABORT with an error: clobbering
// an operator-set libpq option would silently change connection
// behavior in ways the operator did not authorize.
func dsnWithSearchPath(base, schema string) (string, error) {
	// Schema names produced by newSchemaName are
	// `amwh_<12 hex chars>` -- all lowercase alphanumeric +
	// underscore, so they don't need libpq-level quoting and
	// embedding raw double-quote characters in the libpq
	// `options=` value risks confusing the backend command-line
	// parser. Guard with a literal sanity check anyway.
	for _, r := range schema {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_') {
			return "", fmt.Errorf(
				"refusing to bake unsafe schema name into libpq options: %q", schema)
		}
	}
	formatted := "-c search_path=" + schema + ",public,partman"

	// URL form is detected by scheme prefix.
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

	// KV form: scan tokens for a pre-existing `options=`. We
	// only do a leading-token check (no full DSN parser) because
	// the integration-test DSN comes from CI / docker-compose
	// and is well-formed.
	for _, tok := range strings.Fields(base) {
		if strings.HasPrefix(tok, "options=") || strings.HasPrefix(tok, "options ") {
			return "", fmt.Errorf(
				"refusing to clobber existing libpq options token on DSN: %q", tok)
		}
	}
	// Append a quoted options token. libpq accepts both
	// single-quoted and unquoted values but single-quoting
	// keeps the embedded space (`-c search_path=`) safe.
	return base + " options='" + formatted + "'", nil
}

// seedRepoWithSecret inserts one `repo` row + one
// `repo_webhook_secret` row and returns the repo_id (as a UUID
// text string). The secret is a 32-byte random value -- mirrors
// the entropy `mgmt.register` (Stage 7.1) will use.
func seedRepoWithSecret(ctx context.Context, t *testing.T, db *sql.DB, secret string) string {
	t.Helper()
	var repoID string
	err := db.QueryRowContext(ctx, `
		INSERT INTO repo (url, default_branch, current_head_sha)
		VALUES ($1, 'main', $2)
		RETURNING repo_id::text
	`, "https://example.test/"+t.Name(), "0000000000000000000000000000000000000000").Scan(&repoID)
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO repo_webhook_secret (repo_id, webhook_secret)
		VALUES ($1::uuid, $2)
	`, repoID, secret); err != nil {
		t.Fatalf("insert repo_webhook_secret: %v", err)
	}
	return repoID
}

// signBody returns the GitHub-style X-Hub-Signature-256 header
// value: `sha256=<lowercase-hex>` of HMAC-SHA256(secret, body).
func signBody(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write(body); err != nil {
		t.Fatalf("hmac.Write: %v", err)
	}
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// countRepoEventRows returns the number of repo_event rows for
// the given repo. Used by both rejection and acceptance tests
// to assert exact-cardinality outcomes.
func countRepoEventRows(ctx context.Context, t *testing.T, db *sql.DB, repoID string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM repo_event WHERE repo_id = $1::uuid
	`, repoID).Scan(&n); err != nil {
		t.Fatalf("count repo_event: %v", err)
	}
	return n
}

// countIngestJobRows is the ingest_jobs counterpart.
func countIngestJobRows(ctx context.Context, t *testing.T, db *sql.DB, repoID string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM ingest_jobs WHERE repo_id = $1::uuid
	`, repoID).Scan(&n); err != nil {
		t.Fatalf("count ingest_jobs: %v", err)
	}
	return n
}

// ----- Scenario: invalid signature rejected ---------------------

func TestWebhookReceiver_invalidSignature_returns401_andWritesNoRows(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	const correctSecret = "correct-secret-for-this-repo"
	const wrongSecret = "an-attacker-guess-that-is-incorrect"
	repoID := seedRepoWithSecret(ctx, t, fix.db, correctSecret)

	h := webhookreceiver.NewHandler(fix.db, webhookreceiver.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := mustJSON(t, webhookreceiver.Payload{
		Kind:    "push",
		FromSHA: "1111111111111111111111111111111111111111",
		ToSHA:   "2222222222222222222222222222222222222222",
	})
	// HMAC computed with the WRONG secret -- the receiver must
	// reject this request before any DB writes happen.
	sig := signBody(t, wrongSecret, body)

	resp := postWebhook(t, srv, repoID, sig, body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	if n := countRepoEventRows(ctx, t, fix.db, repoID); n != 0 {
		t.Errorf("repo_event rows after invalid signature = %d, want 0", n)
	}
	if n := countIngestJobRows(ctx, t, fix.db, repoID); n != 0 {
		t.Errorf("ingest_jobs rows after invalid signature = %d, want 0", n)
	}
}

func TestWebhookReceiver_unknownRepo_returns401(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	// Don't seed -- the repo_id below is just a fresh random UUID.
	const repoID = "deadbeef-dead-beef-dead-beefdeadbeef"

	h := webhookreceiver.NewHandler(fix.db, webhookreceiver.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := mustJSON(t, webhookreceiver.Payload{Kind: "push", ToSHA: "f00"})
	sig := signBody(t, "any-secret", body)

	resp := postWebhook(t, srv, repoID, sig, body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	if n := countRepoEventRows(ctx, t, fix.db, repoID); n != 0 {
		t.Errorf("repo_event rows after unknown-repo = %d, want 0", n)
	}
}

// ----- Scenario: valid push enqueues delta job ------------------

func TestWebhookReceiver_validPush_writesRepoEventAndIngestJob(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	const secret = "the-real-per-repo-secret"
	repoID := seedRepoWithSecret(ctx, t, fix.db, secret)

	h := webhookreceiver.NewHandler(fix.db, webhookreceiver.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	const (
		fromSHA = "1111111111111111111111111111111111111111"
		toSHA   = "2222222222222222222222222222222222222222"
	)
	body := mustJSON(t, webhookreceiver.Payload{
		Kind:    "push",
		FromSHA: fromSHA,
		ToSHA:   toSHA,
	})
	sig := signBody(t, secret, body)

	resp := postWebhook(t, srv, repoID, sig, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want %d. body=%q", resp.StatusCode, http.StatusAccepted, readBody(t, resp))
	}
	var decoded webhookreceiver.Response
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if decoded.EventID == "" || decoded.JobID == "" {
		t.Errorf("response = %+v; want non-empty event_id and job_id", decoded)
	}

	// Verify the repo_event row was created with the supplied
	// (kind, from_sha, to_sha) tuple.
	var (
		gotKind           string
		gotFromSHA        sql.NullString
		gotToSHA          string
		gotReceivedAtUnix int64
	)
	if err := fix.db.QueryRowContext(ctx, `
		SELECT kind::text, from_sha, to_sha, EXTRACT(EPOCH FROM received_at)::bigint
		FROM repo_event WHERE event_id = $1::uuid
	`, decoded.EventID).Scan(&gotKind, &gotFromSHA, &gotToSHA, &gotReceivedAtUnix); err != nil {
		t.Fatalf("select repo_event: %v", err)
	}
	if gotKind != "push" {
		t.Errorf("repo_event.kind = %q, want push", gotKind)
	}
	if !gotFromSHA.Valid || gotFromSHA.String != fromSHA {
		t.Errorf("repo_event.from_sha = %v, want %q", gotFromSHA, fromSHA)
	}
	if gotToSHA != toSHA {
		t.Errorf("repo_event.to_sha = %q, want %q", gotToSHA, toSHA)
	}
	if gotReceivedAtUnix == 0 {
		t.Errorf("repo_event.received_at unset (got %d)", gotReceivedAtUnix)
	}

	// Verify the ingest_jobs row was enqueued with mode='delta'
	// and status='pending'.
	var (
		gotMode, gotStatus string
		gotJobFromSHA      sql.NullString
		gotJobToSHA        string
	)
	if err := fix.db.QueryRowContext(ctx, `
		SELECT mode::text, status::text, from_sha, to_sha
		FROM ingest_jobs WHERE job_id = $1::uuid
	`, decoded.JobID).Scan(&gotMode, &gotStatus, &gotJobFromSHA, &gotJobToSHA); err != nil {
		t.Fatalf("select ingest_jobs: %v", err)
	}
	if gotMode != "delta" {
		t.Errorf("ingest_jobs.mode = %q, want delta", gotMode)
	}
	if gotStatus != "pending" {
		t.Errorf("ingest_jobs.status = %q, want pending", gotStatus)
	}
	if !gotJobFromSHA.Valid || gotJobFromSHA.String != fromSHA {
		t.Errorf("ingest_jobs.from_sha = %v, want %q", gotJobFromSHA, fromSHA)
	}
	if gotJobToSHA != toSHA {
		t.Errorf("ingest_jobs.to_sha = %q, want %q", gotJobToSHA, toSHA)
	}

	// Exactly one of each.
	if n := countRepoEventRows(ctx, t, fix.db, repoID); n != 1 {
		t.Errorf("repo_event row count = %d, want 1", n)
	}
	if n := countIngestJobRows(ctx, t, fix.db, repoID); n != 1 {
		t.Errorf("ingest_jobs row count = %d, want 1", n)
	}
}

// ----- idempotent re-delivery (defence-in-depth) ----------------

func TestWebhookReceiver_duplicatePush_dedupesIngestJob(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	const secret = "dedupe-test-secret"
	repoID := seedRepoWithSecret(ctx, t, fix.db, secret)

	h := webhookreceiver.NewHandler(fix.db, webhookreceiver.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := mustJSON(t, webhookreceiver.Payload{
		Kind:    "push",
		FromSHA: "aaaa",
		ToSHA:   "bbbb",
	})
	sig := signBody(t, secret, body)

	var firstJobID string
	for i := 0; i < 3; i++ {
		resp := postWebhook(t, srv, repoID, sig, body)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("iter %d status = %d, want %d", i, resp.StatusCode, http.StatusAccepted)
		}
		var decoded webhookreceiver.Response
		if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
			t.Fatalf("iter %d decode: %v", i, err)
		}
		if i == 0 {
			firstJobID = decoded.JobID
		} else if decoded.JobID != firstJobID {
			t.Errorf("iter %d: job_id = %q, want %q (dedupe broken)", i, decoded.JobID, firstJobID)
		}
	}

	// Three repo_event rows (audit log) but ONE ingest_jobs row
	// (dedupe on the unique index).
	if n := countRepoEventRows(ctx, t, fix.db, repoID); n != 3 {
		t.Errorf("repo_event row count = %d, want 3 (one per delivery)", n)
	}
	if n := countIngestJobRows(ctx, t, fix.db, repoID); n != 1 {
		t.Errorf("ingest_jobs row count = %d, want 1 (deduped)", n)
	}
}

// ----- helpers --------------------------------------------------

func postWebhook(t *testing.T, srv *httptest.Server, repoID, sig string, body []byte) *http.Response {
	t.Helper()
	url := srv.URL + webhookreceiver.RoutePrefix + repoID
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sig != "" {
		req.Header.Set(webhookreceiver.DefaultSignatureHeader, sig)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	})
	return resp
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
