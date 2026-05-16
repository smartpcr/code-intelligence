package embedding

// Integration tests for the Stage 3.3 EmbeddingIndex writer
// (§9.6a write protocol).  The tests require a live
// PostgreSQL 16 cluster (AGENT_MEMORY_PG_URL set) and skip
// cleanly when the env var is unset, mirroring the convention
// in `internal/repoindexer/worker_integration_test.go` and
// `internal/graphwriter/writer_integration_test.go`.
//
// Coverage map (implementation-plan.md §3.3 scenarios)
// ----------------------------------------------------
//
//   * "publish state log is complete" ->
//       TestPublisher_publish_recordsCompleteStateLog
//   * "failed publish retries" ->
//       TestPublisher_retry_appendsNewAttempt
//   * "vector excluded until published" ->
//       TestPublisher_recall_excludesUntilPublished
//
// Each test stands up a fresh schema with the full migration
// chain, opens an app-role connection (the role-grant policy
// is the §9.6a append-only enforcer; tests MUST hit it through
// `agent_memory_app`, not the schema owner), seeds the minimum
// graph rows the `embedding_publish.node_id_fkey` requires,
// and exercises the publisher against an in-memory mock
// `Qdrant` whose behaviour is fully controllable per scenario.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/testpglock"
	"github.com/smartpcr/code-intelligence/services/agent-memory/migrations"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

const (
	envPGURL      = "AGENT_MEMORY_PG_URL"
	testDBTimeout = 60 * time.Second
)

// pgFixture is the local re-implementation of the dbFixture
// pattern from internal/repoindexer/worker_integration_test.go.
// Test helpers cannot cross package boundaries in Go, so the
// shape is duplicated here intentionally — keeping the helper
// adjacent to its tests also makes the integration scope
// easier to audit.
type pgFixture struct {
	owner   *sql.DB
	app     *sql.DB
	schema  string
	cleanup func()
}

func openPGFixture(t *testing.T) *pgFixture {
	t.Helper()
	base := os.Getenv(envPGURL)
	if base == "" {
		t.Skipf("skipping: %s is unset; integration tests require a live PostgreSQL", envPGURL)
	}

	owner, err := sql.Open("postgres", base)
	if err != nil {
		t.Fatalf("sql.Open owner: %v", err)
	}
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

	app, revertRole := openAppRoleDB(t, owner, base, schema)

	cleanup := func() {
		_ = app.Close()
		revertRole()
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
	return &pgFixture{owner: owner, app: app, schema: schema, cleanup: cleanup}
}

func openAppRoleDB(t *testing.T, owner *sql.DB, baseURL, schema string) (*sql.DB, func()) {
	t.Helper()
	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse PG URL: %v", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		t.Fatalf("AGENT_MEMORY_PG_URL must be postgres:// (got %q)", u.Scheme)
	}
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	password := "ampub_" + hex.EncodeToString(buf[:])

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	releaseLock, err := testpglock.AcquireAppRoleLogin(ctx, baseURL)
	if err != nil {
		t.Fatalf("acquire app-role login lock: %v", err)
	}
	success := false
	defer func() {
		if !success {
			releaseLock()
		}
	}()

	if _, err := owner.ExecContext(ctx,
		`ALTER ROLE agent_memory_app WITH LOGIN PASSWORD `+pq.QuoteLiteral(password),
	); err != nil {
		t.Fatalf("ALTER ROLE LOGIN: %v", err)
	}
	revert := func() {
		ctx2, c2 := context.WithTimeout(context.Background(), testDBTimeout)
		defer c2()
		_, _ = owner.ExecContext(ctx2, `ALTER ROLE agent_memory_app WITH NOLOGIN`)
	}

	u2 := *u
	u2.User = url.UserPassword("agent_memory_app", password)
	q := u2.Query()
	q.Set("options", "-c search_path="+schema+",public,partman")
	u2.RawQuery = q.Encode()
	app, err := sql.Open("postgres", u2.String())
	if err != nil {
		revert()
		t.Fatalf("sql.Open app: %v", err)
	}
	app.SetMaxOpenConns(4)
	app.SetMaxIdleConns(2)
	if err := app.PingContext(ctx); err != nil {
		_ = app.Close()
		revert()
		t.Fatalf("ping app DB: %v", err)
	}

	success = true
	return app, func() {
		_ = app.Close()
		revert()
		releaseLock()
	}
}

func newSchemaName(t *testing.T) string {
	t.Helper()
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "ampub_" + hex.EncodeToString(buf[:])
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// seedMethodNode inserts the minimum graph rows the publisher
// needs to satisfy the `embedding_publish.node_id_fkey` FK:
// one Repo row (via EnsureRepo), one `repo`-kind Node that
// serves as the hierarchy root, and one `method`-kind Node
// hanging off it.  Returns the repo_id (as the canonical text
// UUID the publisher receives on `req.RepoID`) and the node_id
// (also text UUID) the publish targets.
func seedMethodNode(t *testing.T, ctx context.Context, app *sql.DB) (repoID string, nodeID string) {
	t.Helper()
	w := graphwriter.New(app, slog.Default())
	repoURL := "https://example.invalid/" + newSchemaName(t)
	const sha = "0123456789abcdef0123456789abcdef01234567"
	rec, err := w.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            repoURL,
		DefaultBranch:  "main",
		CurrentHeadSHA: sha,
		LanguageHints:  []string{"go"},
	})
	if err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	// The graph hierarchy is `repo Node -> method Node`; the
	// repo *row* (rec.RepoID) is the FK target for downstream
	// rows but is NOT itself a Node — InsertNode demands a
	// parent Node, so we mint a `repo`-kind Node first.
	repoNode, err := w.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             rec.ID,
		Kind:               "repo",
		CanonicalSignature: repoURL + "::repo",
		FromSHA:            sha,
		AttrsJSON:          json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("InsertNode repo: %v", err)
	}
	method, err := w.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             rec.ID,
		Kind:               "method",
		CanonicalSignature: repoURL + "::method::" + newSchemaName(t),
		ParentNodeID:       repoNode.NodeID,
		FromSHA:            sha,
		AttrsJSON:          json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("InsertNode method: %v", err)
	}
	return rec.ID.String(), method.NodeID
}

// repoFromString reuses fingerprint.MustParseRepoID for tests
// that need to round-trip a textual repo id back through the
// graphwriter API.  Kept here so the seed helper signature
// stays string-typed.
func repoFromString(t *testing.T, s string) fingerprint.RepoID {
	t.Helper()
	r, err := fingerprint.ParseRepoID(s)
	if err != nil {
		t.Fatalf("ParseRepoID(%q): %v", s, err)
	}
	return r
}

// mockQdrant is an in-memory Qdrant implementation whose
// per-call behaviour is fully controllable via the
// `UpsertFn` / `ExistsFn` hooks.  Tracks every call in
// `Upserts` / `Exists` so tests can assert call counts.
type mockQdrant struct {
	mu       atomic.Int64
	Upserts  atomic.Int32
	Exists   atomic.Int32
	UpsertFn func(ctx context.Context, collection, pointID string, vector []float32, payload map[string]any) error
	ExistsFn func(ctx context.Context, collection, pointID string) (bool, error)
}

func (m *mockQdrant) Upsert(ctx context.Context, coll, pid string, vec []float32, payload map[string]any) error {
	m.Upserts.Add(1)
	_ = m.mu.Add(1) // touch atomic so go's race detector covers the field
	if m.UpsertFn != nil {
		return m.UpsertFn(ctx, coll, pid, vec, payload)
	}
	return nil
}

func (m *mockQdrant) PointExists(ctx context.Context, coll, pid string) (bool, error) {
	m.Exists.Add(1)
	if m.ExistsFn != nil {
		return m.ExistsFn(ctx, coll, pid)
	}
	return true, nil
}

// fixedEmbedder returns the same `[1,0,0,0]` vector for every
// `Embed` call and a stable model version.  Sufficient for the
// publisher's purposes — it never inspects the vector body.
type fixedEmbedder struct {
	Version string
	Err     error
}

func (e fixedEmbedder) Embed(ctx context.Context, content string) ([]float32, error) {
	if e.Err != nil {
		return nil, e.Err
	}
	return []float32{1, 0, 0, 0}, nil
}

func (e fixedEmbedder) ModelVersion() string {
	if e.Version == "" {
		return "test-embedder@v1"
	}
	return e.Version
}

// readEventLog returns the `(event_kind, attempt_index)` rows
// the `embedding_publish_event` table holds for `publishID`,
// ordered by `created_at` ASC (i.e. write order — the §9.6a
// "audit trail" view).
func readEventLog(t *testing.T, ctx context.Context, app *sql.DB, publishID string) []eventRow {
	t.Helper()
	rows, err := app.QueryContext(ctx, `
		SELECT event_kind::text, attempt_index
		FROM embedding_publish_event
		WHERE publish_id = $1
		ORDER BY created_at ASC, event_id ASC
	`, publishID)
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	defer rows.Close()
	var out []eventRow
	for rows.Next() {
		var r eventRow
		if err := rows.Scan(&r.Kind, &r.Attempt); err != nil {
			t.Fatalf("scan event row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

type eventRow struct {
	Kind    string
	Attempt int
}

// readLatestEvent returns the latest `event_kind` for the
// supplied publish_id, mirroring the §9.6a recall path's
// (publish_id, created_at DESC) index probe.
func readLatestEvent(t *testing.T, ctx context.Context, app *sql.DB, publishID string) string {
	t.Helper()
	var kind string
	err := app.QueryRowContext(ctx, `
		SELECT event_kind::text
		FROM embedding_publish_event
		WHERE publish_id = $1
		ORDER BY created_at DESC, event_id DESC
		LIMIT 1
	`, publishID).Scan(&kind)
	if err != nil {
		t.Fatalf("read latest event: %v", err)
	}
	return kind
}

// -----------------------------------------------------------------------------
// Scenario 1: "publish state log is complete"
// -----------------------------------------------------------------------------

// TestPublisher_publish_recordsCompleteStateLog covers the
// happy path in §9.6a: a successful publish appends exactly
// `[queued, vector_written, published]` to the event log, with
// `attempt_index = 0` on every event.  Also asserts the row in
// `embedding_publish` carries the embedder's `ModelVersion()`
// — risk §9.6 ("re-embed on model bump") depends on that
// column being populated per row.
func TestPublisher_publish_recordsCompleteStateLog(t *testing.T) {
	fix := openPGFixture(t)
	defer fix.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoID, nodeID := seedMethodNode(t, ctx, fix.app)

	q := &mockQdrant{}
	p := NewPublisher(fix.app, fixedEmbedder{Version: "stub@2025-01-01"}, q)

	res, err := p.Publish(ctx, PublishRequest{
		NodeID:             nodeID,
		RepoID:             repoID,
		Kind:               NodeKindMethod,
		CanonicalSignature: "test::sig::Publish",
		Content:            "func F() {}",
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.LastEventKind != EventKindPublished {
		t.Fatalf("LastEventKind = %q; want %q", res.LastEventKind, EventKindPublished)
	}
	if res.PublishID == "" || res.QdrantPointID == "" {
		t.Fatalf("missing identifiers in result: %+v", res)
	}

	gotEvents := readEventLog(t, ctx, fix.app, res.PublishID)
	wantKinds := []string{EventKindQueued, EventKindVectorWritten, EventKindPublished}
	if len(gotEvents) != len(wantKinds) {
		t.Fatalf("event count = %d; want %d (events=%+v)",
			len(gotEvents), len(wantKinds), gotEvents)
	}
	for i, want := range wantKinds {
		if gotEvents[i].Kind != want {
			t.Fatalf("event[%d].Kind = %q; want %q (events=%+v)",
				i, gotEvents[i].Kind, want, gotEvents)
		}
		if gotEvents[i].Attempt != 0 {
			t.Fatalf("event[%d].Attempt = %d; want 0", i, gotEvents[i].Attempt)
		}
	}

	if q.Upserts.Load() != 1 {
		t.Fatalf("Qdrant Upsert call count = %d; want 1", q.Upserts.Load())
	}
	if q.Exists.Load() != 1 {
		t.Fatalf("Qdrant PointExists call count = %d; want 1 (read-after-write confirm)", q.Exists.Load())
	}

	var gotModel string
	if err := fix.app.QueryRowContext(ctx, `
		SELECT embedding_model_version FROM embedding_publish WHERE publish_id = $1
	`, res.PublishID).Scan(&gotModel); err != nil {
		t.Fatalf("read embedding_model_version: %v", err)
	}
	if gotModel != "stub@2025-01-01" {
		t.Fatalf("embedding_model_version = %q; want stub@2025-01-01", gotModel)
	}
}

// -----------------------------------------------------------------------------
// Scenario 2: "failed publish retries"
// -----------------------------------------------------------------------------

// TestPublisher_retry_appendsNewAttempt covers the retry path:
// a transient Qdrant outage on the first call records
// `[queued, failed]` with attempt_index=0; the subsequent
// `Retry` call appends `[queued, vector_written, published]`
// at attempt_index=1.  Final log is the 5-row sequence the
// §9.6a "audit trail" view promises operators.
func TestPublisher_retry_appendsNewAttempt(t *testing.T) {
	fix := openPGFixture(t)
	defer fix.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoID, nodeID := seedMethodNode(t, ctx, fix.app)

	var calls atomic.Int32
	q := &mockQdrant{
		UpsertFn: func(ctx context.Context, _, _ string, _ []float32, _ map[string]any) error {
			if calls.Add(1) == 1 {
				return errors.New("503 Service Unavailable")
			}
			return nil
		},
	}
	p := NewPublisher(fix.app, fixedEmbedder{}, q)

	req := PublishRequest{
		NodeID:             nodeID,
		RepoID:             repoID,
		Kind:               NodeKindMethod,
		CanonicalSignature: "test::sig::Retry",
		Content:            "func F() {}",
	}
	res, err := p.Publish(ctx, req)
	if err == nil {
		t.Fatalf("Publish: expected error; got nil")
	}
	if !errors.Is(err, ErrAttemptFailed) {
		t.Fatalf("Publish err = %v; want ErrAttemptFailed", err)
	}
	if res.PublishID == "" {
		t.Fatalf("missing publish_id on recorded failure")
	}
	if res.LastEventKind != EventKindFailed {
		t.Fatalf("LastEventKind after fail = %q; want failed", res.LastEventKind)
	}

	got := readEventLog(t, ctx, fix.app, res.PublishID)
	want := []eventRow{
		{EventKindQueued, 0},
		{EventKindFailed, 0},
	}
	if !equalEvents(got, want) {
		t.Fatalf("after Publish, events = %+v; want %+v", got, want)
	}

	// Per rubber-duck #7: the `failed` row's details_json
	// MUST carry the closed-set {phase, error} shape operators
	// triage against.  A regression that drops `phase` or the
	// error message would silently hide the cause of a publish
	// failure in the audit trail.
	var details sql.NullString
	if err := fix.app.QueryRowContext(ctx, `
		SELECT details_json::text
		FROM embedding_publish_event
		WHERE publish_id = $1 AND event_kind = 'failed'
		ORDER BY created_at DESC LIMIT 1
	`, res.PublishID).Scan(&details); err != nil {
		t.Fatalf("read failed details_json: %v", err)
	}
	if !details.Valid {
		t.Fatalf("failed event details_json is NULL; want {phase, error} JSON")
	}
	if !strings.Contains(details.String, `"phase"`) ||
		!strings.Contains(details.String, `"qdrant_upsert"`) {
		t.Fatalf("details_json = %s; want phase=qdrant_upsert", details.String)
	}
	if !strings.Contains(details.String, `"error"`) ||
		!strings.Contains(details.String, "503 Service Unavailable") {
		t.Fatalf("details_json = %s; want carried embedder error text", details.String)
	}

	res2, err := p.Retry(ctx, res.PublishID, req)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if res2.LastEventKind != EventKindPublished {
		t.Fatalf("Retry LastEventKind = %q; want published", res2.LastEventKind)
	}
	if res2.AttemptIndex != 1 {
		t.Fatalf("Retry AttemptIndex = %d; want 1", res2.AttemptIndex)
	}
	if res2.QdrantPointID != res.QdrantPointID {
		t.Fatalf("Retry reused new point_id %q (was %q); retry MUST reuse the original",
			res2.QdrantPointID, res.QdrantPointID)
	}

	got2 := readEventLog(t, ctx, fix.app, res.PublishID)
	want2 := []eventRow{
		{EventKindQueued, 0},
		{EventKindFailed, 0},
		{EventKindQueued, 1},
		{EventKindVectorWritten, 1},
		{EventKindPublished, 1},
	}
	if !equalEvents(got2, want2) {
		t.Fatalf("after Retry, events = %+v; want %+v", got2, want2)
	}
}

// -----------------------------------------------------------------------------
// Scenario 3: "vector excluded until published"
// -----------------------------------------------------------------------------

// TestPublisher_recall_excludesUntilPublished proves the
// §9.6a read protocol's exclusion invariant: a vector whose
// latest event is NOT `published` MUST NOT be served by the
// recall path.
//
// We exercise the invariant DIRECTLY against the
// `(publish_id, created_at DESC)` index that GraphReader uses,
// without simulating the full Qdrant failure path (which would
// record `failed`, which is also a non-`published` state but
// reaches a different §9.6a phase).  This matches rubber-duck
// finding #3 — the scenario should test "latest event != published
// → excluded", not "Qdrant tripped → failed event recorded".
func TestPublisher_recall_excludesUntilPublished(t *testing.T) {
	fix := openPGFixture(t)
	defer fix.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoID, nodeID := seedMethodNode(t, ctx, fix.app)

	// Insert a publish row directly + a queued event ONLY.
	// Reproduces the "publisher started step 2-3 but never
	// reached step 6" state the recall path must defend
	// against.  Using raw SQL (not the Publisher) is the
	// point — we want to lock the exclusion logic against the
	// state shape, not the path that produced it.
	pointID, err := NewUUIDv4()
	if err != nil {
		t.Fatalf("NewUUIDv4: %v", err)
	}
	var publishID string
	if err := fix.app.QueryRowContext(ctx, `
		INSERT INTO embedding_publish
		    (node_id, embedding_model_version, qdrant_point_id)
		VALUES ($1, $2, $3)
		RETURNING publish_id::text
	`, nodeID, "stub@v1", pointID).Scan(&publishID); err != nil {
		t.Fatalf("insert embedding_publish: %v", err)
	}
	if _, err := fix.app.ExecContext(ctx, `
		INSERT INTO embedding_publish_event (publish_id, event_kind, attempt_index)
		VALUES ($1, 'queued'::embedding_publish_event_kind, 0)
	`, publishID); err != nil {
		t.Fatalf("insert queued event: %v", err)
	}

	// Repeat the §9.6a recall-path probe: latest event for
	// the publish_id MUST be `published` for the vector to
	// be served.
	latest := readLatestEvent(t, ctx, fix.app, publishID)
	if latest == EventKindPublished {
		t.Fatalf("latest event = %q; expected NOT published "+
			"(the recall path would incorrectly serve this vector)", latest)
	}
	if latest != EventKindQueued {
		t.Fatalf("latest event = %q; expected queued (only event we wrote)", latest)
	}

	// Exercise the §9.6a read-side primitive directly so this
	// scenario locks the GraphReader contract, not just the
	// raw event-kind probe.  RecallFilter MUST drop the
	// queued point AND increment the
	// `recall_filter_unpublished_total` counter
	// (implementation-plan.md:505).  Without this assertion
	// the recall-path coverage is only a SQL-shape smoke
	// test — the operator-visible counter that triggers an
	// alert when the publisher backlog grows would never be
	// exercised.
	filter := NewRecallFilter(fix.app, nil)
	beforeCounter := filter.Metrics().Snapshot().UnpublishedFiltered
	served, err := filter.FilterPublishedPoints(ctx, []string{pointID})
	if err != nil {
		t.Fatalf("RecallFilter.FilterPublishedPoints: %v", err)
	}
	if len(served) != 0 {
		t.Fatalf("filter returned %d ids; want 0 (queued vectors "+
			"must be excluded): %v", len(served), served)
	}
	afterCounter := filter.Metrics().Snapshot().UnpublishedFiltered
	if afterCounter-beforeCounter != 1 {
		t.Fatalf("UnpublishedFiltered delta = %d; want 1 "+
			"(one queued vector should bump the counter)",
			afterCounter-beforeCounter)
	}

	// Touch the seed helper return so the linter doesn't
	// flag repoID as unused; the publish here intentionally
	// bypasses the publisher and only needs node_id.
	_ = repoID
}

// equalEvents compares two event sequences for value equality.
// Defined separately so the failure message in the calling
// `t.Fatalf` prints the full sequences side-by-side.
func equalEvents(got, want []eventRow) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// -----------------------------------------------------------------------------
// Cross-cutting: model-version drift protection (rubber-duck #3)
// -----------------------------------------------------------------------------

// TestPublisher_retry_rejectsModelVersionMismatch locks the
// §9.6 risk-mitigation invariant: the publish row's
// `embedding_model_version` is the source of truth for a
// vector's provenance.  If the embedder is hot-swapped to a
// new model between the failing Publish and the Retry, the
// retry MUST refuse — otherwise the operator ends up with a
// Qdrant point carrying a model-A label but a model-B vector,
// and the future "supersede on model bump" flow cannot
// correctly detect drift.
func TestPublisher_retry_rejectsModelVersionMismatch(t *testing.T) {
	fix := openPGFixture(t)
	defer fix.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoID, nodeID := seedMethodNode(t, ctx, fix.app)

	var calls atomic.Int32
	q := &mockQdrant{
		UpsertFn: func(ctx context.Context, _, _ string, _ []float32, _ map[string]any) error {
			if calls.Add(1) == 1 {
				return errors.New("503 transient")
			}
			return nil
		},
	}
	p := NewPublisher(fix.app, fixedEmbedder{Version: "model-A"}, q)

	req := PublishRequest{
		NodeID: nodeID, RepoID: repoID, Kind: NodeKindMethod,
		CanonicalSignature: "test::sig::ModelDrift", Content: "func F() {}",
	}
	res, err := p.Publish(ctx, req)
	if !errors.Is(err, ErrAttemptFailed) {
		t.Fatalf("Publish: expected ErrAttemptFailed; got %v", err)
	}

	// Swap to a publisher with a DIFFERENT embedder model
	// version (operator rolled the model forward between
	// attempts).  Retry MUST refuse.
	p2 := NewPublisher(fix.app, fixedEmbedder{Version: "model-B"}, q)
	_, err = p2.Retry(ctx, res.PublishID, req)
	if err == nil {
		t.Fatalf("Retry under new model version: expected error; got nil")
	}
	if !strings.Contains(err.Error(), "model version mismatch") {
		t.Fatalf("Retry error = %v; want substring 'model version mismatch'", err)
	}

	// Confirm the refusal is BEFORE any side effect: no new
	// `queued` event was appended on top of the existing
	// `[queued, failed]` log.
	got := readEventLog(t, ctx, fix.app, res.PublishID)
	want := []eventRow{
		{EventKindQueued, 0},
		{EventKindFailed, 0},
	}
	if !equalEvents(got, want) {
		t.Fatalf("Retry refusal must not write events; got %+v; want %+v", got, want)
	}
}

// -----------------------------------------------------------------------------
// Cross-cutting: ctx cancellation does NOT record failed (rubber-duck #5)
// -----------------------------------------------------------------------------

// TestPublisher_publish_cancellationDoesNotRecordFailed proves
// caller cancellation surfaces as a non-`ErrAttemptFailed`
// error and DOES NOT append a `failed` event.  The latest
// event must remain at `queued` so a background flusher can
// pick up the retry without misclassifying the cancellation
// as a Qdrant/embedder service failure.
func TestPublisher_publish_cancellationDoesNotRecordFailed(t *testing.T) {
	fix := openPGFixture(t)
	defer fix.cleanup()

	ctx := context.Background()
	repoID, nodeID := seedMethodNode(t, ctx, fix.app)

	cancelCtx, cancel := context.WithCancel(ctx)
	q := &mockQdrant{
		UpsertFn: func(c context.Context, _, _ string, _ []float32, _ map[string]any) error {
			cancel() // simulate operator shutdown mid-upsert
			return c.Err()
		},
	}
	p := NewPublisher(fix.app, fixedEmbedder{}, q)
	res, err := p.Publish(cancelCtx, PublishRequest{
		NodeID: nodeID, RepoID: repoID, Kind: NodeKindMethod,
		CanonicalSignature: "test::sig::Cancel", Content: "func F() {}",
	})
	if err == nil {
		t.Fatalf("Publish: expected cancellation error; got nil")
	}
	if errors.Is(err, ErrAttemptFailed) {
		t.Fatalf("cancellation must NOT be classified as ErrAttemptFailed: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish err = %v; want errors.Is(..., context.Canceled)", err)
	}

	// Use a fresh context for the verification query since
	// the original ctx is cancelled.
	verifyCtx, vCancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer vCancel()
	latest := readLatestEvent(t, verifyCtx, fix.app, res.PublishID)
	if latest != EventKindQueued {
		t.Fatalf("latest event after cancellation = %q; want queued "+
			"(cancellation must leave the row at queued, NOT failed)", latest)
	}
}
